//! End-to-end storage benchmark for InfluxDB's Parquet path and experimental numeric codecs.

use anyhow::{Context, Result, bail};
use arrow::{
    array::{Array, ArrayRef, Float64Array, TimestampNanosecondArray},
    datatypes::{DataType, Field, Schema},
    record_batch::RecordBatch,
};
use bytes::Bytes;
use datafusion::execution::memory_pool::GreedyMemoryPool;
use influxdb3_write::{
    experimental_codecs::ExperimentalCodec, persister::TrackedMemoryArrowWriter,
};
use parquet::{
    arrow::{ArrowWriter, arrow_reader::ParquetRecordBatchReaderBuilder},
    basic::Compression,
    file::properties::WriterProperties,
};
use std::{
    env,
    fs::{self, File},
    io::{BufReader, Read, Write},
    path::{Path, PathBuf},
    sync::Arc,
    time::Instant,
};

const INPUT_MAGIC: &[u8; 8] = b"SCDBIN01";
const CUSTOM_MAGIC: &[u8; 8] = b"I3SCBIN1";

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<String> = env::args().collect();
    if !(4..=5).contains(&args.len()) {
        bail!("usage: system_codec_bench <input.scdbin> <output-dir> <dataset> [iterations]");
    }
    let dataset = read_dataset(Path::new(&args[1]))?;
    let output_dir = PathBuf::from(&args[2]);
    fs::create_dir_all(&output_dir)?;
    let iterations = if args.len() == 5 { args[4].parse()? } else { 3 };
    let batch = dataset.record_batch()?;
    println!("system,dataset,codec,iteration,rows,columns,write_ms,read_ms,file_bytes,hash_ok");

    for codec in [
        Codec::Bos,
        Codec::Subcolumn,
        Codec::ParquetZstd,
        Codec::ParquetSnappy,
        Codec::ParquetUncompressed,
    ] {
        for iteration in -1..iterations {
            let path = output_dir.join(format!(
                "{}_{}_{}.bin",
                sanitize(&args[3]),
                codec.name().to_ascii_lowercase(),
                iteration
            ));
            let result = run_once(&path, &batch, codec)?;
            fs::remove_file(&path)?;
            if iteration >= 0 {
                println!(
                    "influxdb,{},{},{},{},{},{:.6},{:.6},{},{}",
                    args[3],
                    codec.name(),
                    iteration,
                    dataset.rows,
                    dataset.columns.len(),
                    result.write_nanos as f64 / 1_000_000.0,
                    result.read_nanos as f64 / 1_000_000.0,
                    result.file_bytes,
                    result.hash_ok
                );
            }
        }
    }
    Ok(())
}

#[derive(Clone, Copy)]
enum Codec {
    Bos,
    Subcolumn,
    ParquetZstd,
    ParquetSnappy,
    ParquetUncompressed,
}

impl Codec {
    fn name(self) -> &'static str {
        match self {
            Self::Bos => "BOS",
            Self::Subcolumn => "SUBCOLUMN",
            Self::ParquetZstd => "PARQUET_ZSTD",
            Self::ParquetSnappy => "PARQUET_SNAPPY",
            Self::ParquetUncompressed => "PARQUET_UNCOMPRESSED",
        }
    }

    fn experimental(self) -> Option<ExperimentalCodec> {
        match self {
            Self::Bos => Some(ExperimentalCodec::Bos),
            Self::Subcolumn => Some(ExperimentalCodec::Subcolumn),
            _ => None,
        }
    }
}

struct BenchResult {
    write_nanos: u128,
    read_nanos: u128,
    file_bytes: u64,
    hash_ok: bool,
}

fn run_once(path: &Path, batch: &RecordBatch, codec: Codec) -> Result<BenchResult> {
    let write_start = Instant::now();
    let payload = if let Some(experimental) = codec.experimental() {
        encode_custom_batch(batch, experimental)?
    } else {
        encode_parquet(batch, codec)?
    };
    let mut file = File::create(path)?;
    file.write_all(&payload)?;
    file.sync_all()?;
    drop(file);
    let write_nanos = write_start.elapsed().as_nanos();
    let file_bytes = fs::metadata(path)?.len();

    let read_start = Instant::now();
    let bytes = fs::read(path)?;
    let decoded = if let Some(experimental) = codec.experimental() {
        decode_custom_batch(&bytes, experimental)?
    } else {
        decode_parquet(bytes)?
    };
    let hash_ok = record_batches_bit_equal(batch, &decoded);
    let read_nanos = read_start.elapsed().as_nanos();
    Ok(BenchResult {
        write_nanos,
        read_nanos,
        file_bytes,
        hash_ok,
    })
}

fn encode_custom_batch(batch: &RecordBatch, codec: ExperimentalCodec) -> Result<Vec<u8>> {
    let mut out = Vec::new();
    out.extend_from_slice(CUSTOM_MAGIC);
    out.extend_from_slice(&(batch.num_columns() as u32).to_be_bytes());
    out.extend_from_slice(&(batch.num_rows() as u64).to_be_bytes());
    for array in batch.columns() {
        let payload = codec.encode_arrow_array(array.as_ref())?;
        out.extend_from_slice(&(payload.len() as u64).to_be_bytes());
        out.extend_from_slice(&payload);
    }
    Ok(out)
}

fn decode_custom_batch(payload: &[u8], codec: ExperimentalCodec) -> Result<RecordBatch> {
    let mut cursor = std::io::Cursor::new(payload);
    let mut magic = [0_u8; 8];
    cursor.read_exact(&mut magic)?;
    if &magic != CUSTOM_MAGIC {
        bail!("invalid InfluxDB experimental batch magic");
    }
    let columns = read_u32(&mut cursor)? as usize;
    let rows = read_u64(&mut cursor)? as usize;
    let mut arrays = Vec::with_capacity(columns);
    for _ in 0..columns {
        let len = read_u64(&mut cursor)? as usize;
        let mut body = vec![0_u8; len];
        cursor.read_exact(&mut body)?;
        arrays.push(codec.decode_arrow_array(&body)?);
    }
    if cursor.position() as usize != payload.len() {
        bail!("trailing InfluxDB experimental batch bytes");
    }
    let fields: Vec<Field> = arrays
        .iter()
        .enumerate()
        .map(|(index, array)| {
            let name = if index == 0 {
                "time".to_string()
            } else {
                format!("c{}", index - 1)
            };
            Field::new(name, array.data_type().clone(), true)
        })
        .collect();
    let batch = RecordBatch::try_new(Arc::new(Schema::new(fields)), arrays)?;
    if batch.num_rows() != rows {
        bail!("InfluxDB experimental batch row mismatch");
    }
    Ok(batch)
}

fn encode_parquet(batch: &RecordBatch, codec: Codec) -> Result<Vec<u8>> {
    let mut bytes = Vec::new();
    if matches!(codec, Codec::ParquetZstd) {
        let pool = Arc::new(GreedyMemoryPool::new(usize::MAX));
        let mut writer =
            TrackedMemoryArrowWriter::try_new(&mut bytes, Arc::clone(&batch.schema()), pool)?;
        writer.write(batch.clone())?;
        writer.close()?;
    } else {
        let compression = match codec {
            Codec::ParquetSnappy => Compression::SNAPPY,
            Codec::ParquetUncompressed => Compression::UNCOMPRESSED,
            _ => unreachable!(),
        };
        let properties = WriterProperties::builder()
            .set_compression(compression)
            .build();
        let mut writer = ArrowWriter::try_new(&mut bytes, batch.schema(), Some(properties))?;
        writer.write(batch)?;
        writer.close()?;
    }
    Ok(bytes)
}

fn decode_parquet(bytes: Vec<u8>) -> Result<RecordBatch> {
    let reader = ParquetRecordBatchReaderBuilder::try_new(Bytes::from(bytes))?
        .with_batch_size(65_536)
        .build()?;
    let batches: Vec<RecordBatch> = reader.collect::<std::result::Result<_, _>>()?;
    if batches.is_empty() {
        bail!("Parquet reader returned no record batches");
    }
    let column_count = batches[0].num_columns();
    let total_rows = batches.iter().map(RecordBatch::num_rows).sum();
    let mut time = Vec::with_capacity(total_rows);
    let mut values: Vec<Vec<f64>> = (1..column_count)
        .map(|_| Vec::with_capacity(total_rows))
        .collect();
    for batch in batches {
        let time_array = batch
            .column(0)
            .as_any()
            .downcast_ref::<TimestampNanosecondArray>()
            .context("Parquet time column type")?;
        time.extend((0..time_array.len()).map(|row| time_array.value(row)));
        for column in 1..column_count {
            let array = batch
                .column(column)
                .as_any()
                .downcast_ref::<Float64Array>()
                .context("Parquet value column type")?;
            values[column - 1].extend((0..array.len()).map(|row| array.value(row)));
        }
    }
    let mut fields = vec![Field::new(
        "time",
        DataType::Timestamp(arrow::datatypes::TimeUnit::Nanosecond, None),
        false,
    )];
    let mut arrays: Vec<ArrayRef> = vec![Arc::new(TimestampNanosecondArray::from(time))];
    for (index, column) in values.into_iter().enumerate() {
        fields.push(Field::new(format!("c{index}"), DataType::Float64, false));
        arrays.push(Arc::new(Float64Array::from(column)));
    }
    Ok(RecordBatch::try_new(Arc::new(Schema::new(fields)), arrays)?)
}

fn record_batches_bit_equal(expected: &RecordBatch, actual: &RecordBatch) -> bool {
    if expected.num_rows() != actual.num_rows() || expected.num_columns() != actual.num_columns() {
        return false;
    }
    for column in 0..expected.num_columns() {
        let expected_array = expected.column(column);
        let actual_array = actual.column(column);
        if expected_array.data_type() != actual_array.data_type() {
            return false;
        }
        match expected_array.data_type() {
            DataType::Timestamp(arrow::datatypes::TimeUnit::Nanosecond, None) => {
                let left = expected_array
                    .as_any()
                    .downcast_ref::<TimestampNanosecondArray>()
                    .expect("timestamp array");
                let right = actual_array
                    .as_any()
                    .downcast_ref::<TimestampNanosecondArray>()
                    .expect("timestamp array");
                for row in 0..left.len() {
                    if left.value(row) != right.value(row) {
                        return false;
                    }
                }
            }
            DataType::Float64 => {
                let left = expected_array
                    .as_any()
                    .downcast_ref::<Float64Array>()
                    .expect("float array");
                let right = actual_array
                    .as_any()
                    .downcast_ref::<Float64Array>()
                    .expect("float array");
                for row in 0..left.len() {
                    if left.value(row).to_bits() != right.value(row).to_bits() {
                        return false;
                    }
                }
            }
            _ => return false,
        }
    }
    true
}

struct Dataset {
    rows: usize,
    columns: Vec<Vec<f64>>,
}

impl Dataset {
    fn record_batch(&self) -> Result<RecordBatch> {
        let mut fields = vec![Field::new(
            "time",
            DataType::Timestamp(arrow::datatypes::TimeUnit::Nanosecond, None),
            false,
        )];
        let mut arrays: Vec<ArrayRef> = vec![Arc::new(TimestampNanosecondArray::from_iter_values(
            (0..self.rows).map(|value| value as i64),
        ))];
        for (index, column) in self.columns.iter().enumerate() {
            fields.push(Field::new(format!("c{index}"), DataType::Float64, false));
            arrays.push(Arc::new(Float64Array::from(column.clone())));
        }
        Ok(RecordBatch::try_new(Arc::new(Schema::new(fields)), arrays)?)
    }
}

fn read_dataset(path: &Path) -> Result<Dataset> {
    let mut input = BufReader::new(File::open(path)?);
    let mut magic = [0_u8; 8];
    input.read_exact(&mut magic)?;
    if &magic != INPUT_MAGIC {
        bail!("invalid SCDBIN input magic");
    }
    let columns = read_u32(&mut input)? as usize;
    let rows = usize::try_from(read_u64(&mut input)?).context("row count does not fit usize")?;
    if columns == 0 {
        bail!("SCDBIN has no numeric columns");
    }
    for _ in 0..columns {
        let mut len = [0_u8; 2];
        input.read_exact(&mut len)?;
        let mut name = vec![0_u8; u16::from_be_bytes(len) as usize];
        input.read_exact(&mut name)?;
    }
    let mut values = vec![vec![0_f64; rows]; columns];
    for column in &mut values {
        for value in column {
            *value = f64::from_bits(read_u64(&mut input)?);
        }
    }
    let mut trailing = [0_u8; 1];
    if input.read(&mut trailing)? != 0 {
        bail!("trailing SCDBIN bytes");
    }
    Ok(Dataset {
        rows,
        columns: values,
    })
}

fn read_u32(reader: &mut impl Read) -> Result<u32> {
    let mut bytes = [0_u8; 4];
    reader.read_exact(&mut bytes)?;
    Ok(u32::from_be_bytes(bytes))
}

fn read_u64(reader: &mut impl Read) -> Result<u64> {
    let mut bytes = [0_u8; 8];
    reader.read_exact(&mut bytes)?;
    Ok(u64::from_be_bytes(bytes))
}

fn sanitize(value: &str) -> String {
    value
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || "_.-".contains(character) {
                character
            } else {
                '_'
            }
        })
        .collect()
}
