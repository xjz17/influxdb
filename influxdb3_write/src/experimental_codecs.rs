//! Lossless experimental BOS and Sub-column codecs for InfluxDB numeric batches.
//!
//! These codecs intentionally live above Parquet's fixed encoding enum. Callers can encode numeric
//! arrays before object-store persistence and decode them back into the original Arrow values.

use arrow::{
    array::{
        Array, ArrayRef, Float32Array, Float32Builder, Float64Array, Float64Builder, Int32Array,
        Int32Builder, Int64Array, Int64Builder, TimestampNanosecondArray,
        TimestampNanosecondBuilder, UInt64Array, UInt64Builder,
    },
    datatypes::DataType,
};
use std::sync::Arc;
use thiserror::Error;

const BLOCK_SIZE: usize = 512;
const BOS_MAGIC: &[u8; 4] = b"BOS1";
const SUBCOLUMN_MAGIC: &[u8; 4] = b"SUB1";
const ARRAY_MAGIC: &[u8; 4] = b"I3AR";

#[derive(Debug, Clone, Copy, Eq, PartialEq)]
pub enum ExperimentalCodec {
    Bos,
    Subcolumn,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum CodecError {
    #[error("truncated experimental codec payload")]
    Truncated,
    #[error("invalid experimental codec payload: {0}")]
    Invalid(&'static str),
    #[error("unsupported Arrow data type for experimental codec: {0}")]
    UnsupportedType(String),
}

pub type Result<T, E = CodecError> = std::result::Result<T, E>;

impl ExperimentalCodec {
    pub fn encode_i64(self, values: &[i64]) -> Vec<u8> {
        match self {
            Self::Bos => encode_bos(values),
            Self::Subcolumn => encode_subcolumn(values),
        }
    }

    pub fn decode_i64(self, payload: &[u8]) -> Result<Vec<i64>> {
        match self {
            Self::Bos => decode_bos(payload),
            Self::Subcolumn => decode_subcolumn(payload),
        }
    }

    /// Encodes a nullable numeric Arrow array while preserving every IEEE bit pattern.
    pub fn encode_arrow_array(self, array: &dyn Array) -> Result<Vec<u8>> {
        let (type_tag, values) = arrow_values_to_i64(array)?;
        let mut validity = vec![0_u8; array.len().div_ceil(8)];
        for index in 0..array.len() {
            if array.is_valid(index) {
                validity[index / 8] |= 1 << (index % 8);
            }
        }
        let body = self.encode_i64(&values);
        let mut out = Vec::with_capacity(body.len() + validity.len() + 24);
        out.extend_from_slice(ARRAY_MAGIC);
        out.push(1);
        out.push(type_tag);
        put_u64(&mut out, array.len() as u64);
        put_u32(&mut out, validity.len() as u32);
        put_u64(&mut out, body.len() as u64);
        out.extend_from_slice(&validity);
        out.extend_from_slice(&body);
        Ok(out)
    }

    /// Decodes an Arrow array written by [`Self::encode_arrow_array`].
    pub fn decode_arrow_array(self, payload: &[u8]) -> Result<ArrayRef> {
        let mut reader = Reader::new(payload);
        reader.require_magic(ARRAY_MAGIC)?;
        if reader.read_u8()? != 1 {
            return Err(CodecError::Invalid("Arrow codec version"));
        }
        let type_tag = reader.read_u8()?;
        let len = reader.read_len()?;
        let validity_len = reader.read_u32()? as usize;
        let body_len = reader.read_len()?;
        if validity_len != len.div_ceil(8) {
            return Err(CodecError::Invalid("Arrow validity length"));
        }
        let validity = reader.read_bytes(validity_len)?;
        let values = self.decode_i64(reader.read_bytes(body_len)?)?;
        reader.require_end()?;
        if values.len() != len {
            return Err(CodecError::Invalid("Arrow value count"));
        }
        i64_to_arrow_values(type_tag, &values, validity)
    }
}

fn arrow_values_to_i64(array: &dyn Array) -> Result<(u8, Vec<i64>)> {
    macro_rules! collect_array {
        ($array_type:ty, $tag:expr, $conversion:expr) => {{
            let array = array
                .as_any()
                .downcast_ref::<$array_type>()
                .ok_or(CodecError::Invalid("Arrow downcast"))?;
            let values = (0..array.len())
                .map(|index| {
                    if array.is_null(index) {
                        0
                    } else {
                        ($conversion)(array.value(index))
                    }
                })
                .collect();
            Ok(($tag, values))
        }};
    }

    match array.data_type() {
        DataType::Int64 => collect_array!(Int64Array, 1, |value| value),
        DataType::UInt64 => collect_array!(UInt64Array, 2, |value| value as i64),
        DataType::Float64 => collect_array!(Float64Array, 3, |value: f64| value.to_bits() as i64),
        DataType::Timestamp(arrow::datatypes::TimeUnit::Nanosecond, None) => {
            collect_array!(TimestampNanosecondArray, 4, |value| value)
        }
        DataType::Int32 => collect_array!(Int32Array, 5, |value| i64::from(value)),
        DataType::Float32 => {
            collect_array!(Float32Array, 6, |value: f32| i64::from(value.to_bits()))
        }
        data_type => Err(CodecError::UnsupportedType(data_type.to_string())),
    }
}

fn i64_to_arrow_values(type_tag: u8, values: &[i64], validity: &[u8]) -> Result<ArrayRef> {
    macro_rules! build_array {
        ($builder_type:ty, $conversion:expr) => {{
            let mut builder = <$builder_type>::with_capacity(values.len());
            for (index, &value) in values.iter().enumerate() {
                if validity[index / 8] & (1 << (index % 8)) == 0 {
                    builder.append_null();
                } else {
                    builder.append_value(($conversion)(value));
                }
            }
            Ok(Arc::new(builder.finish()) as ArrayRef)
        }};
    }

    match type_tag {
        1 => build_array!(Int64Builder, |value| value),
        2 => build_array!(UInt64Builder, |value| value as u64),
        3 => build_array!(Float64Builder, |value: i64| f64::from_bits(value as u64)),
        4 => build_array!(TimestampNanosecondBuilder, |value| value),
        5 => build_array!(Int32Builder, |value| value as i32),
        6 => build_array!(Float32Builder, |value: i64| f32::from_bits(value as u32)),
        _ => Err(CodecError::Invalid("Arrow type tag")),
    }
}

fn encode_bos(values: &[i64]) -> Vec<u8> {
    let mut out = Vec::with_capacity(values.len() * 4);
    out.extend_from_slice(BOS_MAGIC);
    put_u64(&mut out, values.len() as u64);
    put_u32(&mut out, BLOCK_SIZE as u32);
    for block in values.chunks(BLOCK_SIZE) {
        put_u32(&mut out, block.len() as u32);
        put_i64(&mut out, block[0]);
        if block.len() == 1 {
            out.push(0);
            put_u32(&mut out, 0);
            put_u32(&mut out, 0);
            put_u32(&mut out, 0);
            put_i64(&mut out, 0);
            continue;
        }

        let deltas: Vec<i64> = block
            .windows(2)
            .map(|pair| pair[1].wrapping_sub(pair[0]))
            .collect();
        let plan = choose_bos_plan(&deltas);
        let mut bitmap = vec![0_u8; deltas.len().div_ceil(8)];
        let mut inliers = Vec::with_capacity(deltas.len());
        let mut outliers = Vec::new();
        for (index, &delta) in deltas.iter().enumerate() {
            if delta < plan.low || delta > plan.high {
                bitmap[index / 8] |= 1 << (index % 8);
                outliers.push(delta);
            } else {
                inliers.push((delta as u64).wrapping_sub(plan.low as u64));
            }
        }
        let packed = pack_bits(&inliers, plan.width);
        out.push(plan.width);
        put_u32(&mut out, bitmap.len() as u32);
        put_u32(&mut out, packed.len() as u32);
        put_u32(&mut out, outliers.len() as u32);
        put_i64(&mut out, plan.low);
        out.extend_from_slice(&bitmap);
        out.extend_from_slice(&packed);
        for outlier in outliers {
            put_i64(&mut out, outlier);
        }
    }
    out
}

fn decode_bos(payload: &[u8]) -> Result<Vec<i64>> {
    let mut reader = Reader::new(payload);
    reader.require_magic(BOS_MAGIC)?;
    let total = reader.read_len()?;
    let block_size = reader.read_u32()? as usize;
    if block_size == 0 {
        return Err(CodecError::Invalid("zero BOS block size"));
    }
    let mut values = Vec::with_capacity(total);
    while values.len() < total {
        let len = reader.read_u32()? as usize;
        if len == 0 || len > block_size || len > total - values.len() {
            return Err(CodecError::Invalid("BOS block length"));
        }
        let first = reader.read_i64()?;
        let width = reader.read_u8()?;
        if width > 64 {
            return Err(CodecError::Invalid("BOS bit width"));
        }
        let bitmap_len = reader.read_u32()? as usize;
        let packed_len = reader.read_u32()? as usize;
        let outlier_count = reader.read_u32()? as usize;
        let base = reader.read_i64()?;
        let delta_count = len - 1;
        if bitmap_len != delta_count.div_ceil(8) {
            return Err(CodecError::Invalid("BOS bitmap length"));
        }
        let bitmap = reader.read_bytes(bitmap_len)?;
        let actual_outliers = (0..delta_count)
            .filter(|index| bitmap[*index / 8] & (1 << (*index % 8)) != 0)
            .count();
        if actual_outliers != outlier_count {
            return Err(CodecError::Invalid("BOS outlier count"));
        }
        let inlier_count = delta_count - outlier_count;
        if packed_len != packed_byte_len(inlier_count, width) {
            return Err(CodecError::Invalid("BOS packed length"));
        }
        let packed = reader.read_bytes(packed_len)?;
        let inliers = unpack_bits(packed, inlier_count, width)?;
        let mut outliers = Vec::with_capacity(outlier_count);
        for _ in 0..outlier_count {
            outliers.push(reader.read_i64()?);
        }

        values.push(first);
        let mut previous = first;
        let mut inlier_index = 0;
        let mut outlier_index = 0;
        for index in 0..delta_count {
            let delta = if bitmap[index / 8] & (1 << (index % 8)) != 0 {
                let value = outliers[outlier_index];
                outlier_index += 1;
                value
            } else {
                let value = (base as u64).wrapping_add(inliers[inlier_index]) as i64;
                inlier_index += 1;
                value
            };
            previous = previous.wrapping_add(delta);
            values.push(previous);
        }
    }
    reader.require_end()?;
    Ok(values)
}

#[derive(Debug)]
struct BosPlan {
    low: i64,
    high: i64,
    width: u8,
    bytes: usize,
}

fn choose_bos_plan(deltas: &[i64]) -> BosPlan {
    let mut sorted = deltas.to_vec();
    sorted.sort_unstable();
    let trims = [
        0,
        deltas.len() / 100,
        deltas.len() / 50,
        deltas.len() / 20,
        deltas.len() / 10,
    ];
    let mut best = None;
    for trim in trims {
        if trim * 2 >= sorted.len() {
            continue;
        }
        let low = sorted[trim];
        let high = sorted[sorted.len() - trim - 1];
        let width = bit_width((high as u64).wrapping_sub(low as u64));
        let inliers = deltas
            .iter()
            .filter(|&&value| value >= low && value <= high)
            .count();
        let outliers = deltas.len() - inliers;
        let bytes = packed_byte_len(inliers, width) + outliers * i64::BITS as usize / 8;
        if best
            .as_ref()
            .is_none_or(|candidate: &BosPlan| bytes < candidate.bytes)
        {
            best = Some(BosPlan {
                low,
                high,
                width,
                bytes,
            });
        }
    }
    best.expect("non-empty BOS delta block has a plan")
}

fn encode_subcolumn(values: &[i64]) -> Vec<u8> {
    let mut out = Vec::with_capacity(values.len() * 4);
    out.extend_from_slice(SUBCOLUMN_MAGIC);
    put_u64(&mut out, values.len() as u64);
    put_u32(&mut out, BLOCK_SIZE as u32);
    for block in values.chunks(BLOCK_SIZE) {
        put_u32(&mut out, block.len() as u32);
        let base = *block.iter().min().expect("non-empty block");
        put_i64(&mut out, base);
        let residuals: Vec<u64> = block
            .iter()
            .map(|&value| (value as u64).wrapping_sub(base as u64))
            .collect();
        let max = residuals.iter().copied().max().unwrap_or(0);
        let groups = usize::from(bit_width(max)).div_ceil(4).max(1);
        out.push(groups as u8);
        for group in 0..groups {
            let digits: Vec<u8> = residuals
                .iter()
                .map(|value| ((value >> (group * 4)) & 0xf) as u8)
                .collect();
            let (mode, payload) = encode_digit_group(&digits);
            out.push(mode);
            put_u32(&mut out, payload.len() as u32);
            out.extend_from_slice(&payload);
        }
    }
    out
}

fn decode_subcolumn(payload: &[u8]) -> Result<Vec<i64>> {
    let mut reader = Reader::new(payload);
    reader.require_magic(SUBCOLUMN_MAGIC)?;
    let total = reader.read_len()?;
    let block_size = reader.read_u32()? as usize;
    if block_size == 0 {
        return Err(CodecError::Invalid("zero Sub-column block size"));
    }
    let mut values = Vec::with_capacity(total);
    while values.len() < total {
        let len = reader.read_u32()? as usize;
        if len == 0 || len > block_size || len > total - values.len() {
            return Err(CodecError::Invalid("Sub-column block length"));
        }
        let base = reader.read_i64()?;
        let groups = reader.read_u8()? as usize;
        if !(1..=16).contains(&groups) {
            return Err(CodecError::Invalid("Sub-column group count"));
        }
        let mut residuals = vec![0_u64; len];
        for group in 0..groups {
            let mode = reader.read_u8()?;
            let payload_len = reader.read_u32()? as usize;
            let digits = decode_digit_group(mode, reader.read_bytes(payload_len)?, len)?;
            for (index, digit) in digits.into_iter().enumerate() {
                residuals[index] |= u64::from(digit) << (group * 4);
            }
        }
        values.extend(
            residuals
                .into_iter()
                .map(|residual| (base as u64).wrapping_add(residual) as i64),
        );
    }
    reader.require_end()?;
    Ok(values)
}

fn encode_digit_group(digits: &[u8]) -> (u8, Vec<u8>) {
    let packed = pack_nibbles(digits);
    let rle = encode_nibble_rle(digits);
    let dictionary = encode_nibble_dictionary(digits);
    if rle.len() < packed.len() && rle.len() <= dictionary.len() {
        (1, rle)
    } else if dictionary.len() < packed.len() {
        (2, dictionary)
    } else {
        (0, packed)
    }
}

fn decode_digit_group(mode: u8, payload: &[u8], len: usize) -> Result<Vec<u8>> {
    match mode {
        0 => unpack_nibbles(payload, len),
        1 => decode_nibble_rle(payload, len),
        2 => decode_nibble_dictionary(payload, len),
        _ => Err(CodecError::Invalid("Sub-column group mode")),
    }
}

fn pack_nibbles(values: &[u8]) -> Vec<u8> {
    let mut out = vec![0_u8; values.len().div_ceil(2)];
    for (index, value) in values.iter().enumerate() {
        out[index / 2] |= (value & 0xf) << ((index % 2) * 4);
    }
    out
}

fn unpack_nibbles(payload: &[u8], len: usize) -> Result<Vec<u8>> {
    if payload.len() != len.div_ceil(2) {
        return Err(CodecError::Invalid("Sub-column packed digit length"));
    }
    Ok((0..len)
        .map(|index| (payload[index / 2] >> ((index % 2) * 4)) & 0xf)
        .collect())
}

fn encode_nibble_rle(values: &[u8]) -> Vec<u8> {
    let mut out = Vec::new();
    let mut index = 0;
    while index < values.len() {
        let value = values[index];
        let mut end = index + 1;
        while end < values.len() && values[end] == value {
            end += 1;
        }
        out.push(value);
        put_u32(&mut out, (end - index) as u32);
        index = end;
    }
    out
}

fn decode_nibble_rle(payload: &[u8], len: usize) -> Result<Vec<u8>> {
    let mut reader = Reader::new(payload);
    let mut values = Vec::with_capacity(len);
    while values.len() < len {
        let value = reader.read_u8()?;
        if value > 0xf {
            return Err(CodecError::Invalid("Sub-column RLE digit"));
        }
        let run = reader.read_u32()? as usize;
        if run == 0 || run > len - values.len() {
            return Err(CodecError::Invalid("Sub-column RLE run"));
        }
        values.extend(std::iter::repeat_n(value, run));
    }
    reader.require_end()?;
    Ok(values)
}

fn encode_nibble_dictionary(values: &[u8]) -> Vec<u8> {
    let mut dictionary = Vec::new();
    let mut indexes = Vec::with_capacity(values.len());
    for &value in values {
        let index = dictionary
            .iter()
            .position(|&entry| entry == value)
            .unwrap_or_else(|| {
                dictionary.push(value);
                dictionary.len() - 1
            });
        indexes.push(index as u64);
    }
    let width = bit_width((dictionary.len() - 1) as u64);
    let packed = pack_bits(&indexes, width);
    let mut out = Vec::with_capacity(dictionary.len() + packed.len() + 2);
    out.push(dictionary.len() as u8);
    out.extend_from_slice(&dictionary);
    out.push(width);
    out.extend_from_slice(&packed);
    out
}

fn decode_nibble_dictionary(payload: &[u8], len: usize) -> Result<Vec<u8>> {
    let mut reader = Reader::new(payload);
    let dictionary_len = reader.read_u8()? as usize;
    if !(1..=16).contains(&dictionary_len) {
        return Err(CodecError::Invalid("Sub-column dictionary length"));
    }
    let dictionary = reader.read_bytes(dictionary_len)?;
    if dictionary.iter().any(|&value| value > 0xf) {
        return Err(CodecError::Invalid("Sub-column dictionary digit"));
    }
    let width = reader.read_u8()?;
    if width > 4 || reader.remaining() != packed_byte_len(len, width) {
        return Err(CodecError::Invalid("Sub-column dictionary index width"));
    }
    let indexes = unpack_bits(reader.read_bytes(reader.remaining())?, len, width)?;
    indexes
        .into_iter()
        .map(|index| {
            dictionary
                .get(index as usize)
                .copied()
                .ok_or(CodecError::Invalid("Sub-column dictionary index"))
        })
        .collect()
}

fn bit_width(value: u64) -> u8 {
    if value == 0 {
        0
    } else {
        (u64::BITS - value.leading_zeros()) as u8
    }
}

fn packed_byte_len(count: usize, width: u8) -> usize {
    count.saturating_mul(width as usize).div_ceil(8)
}

fn pack_bits(values: &[u64], width: u8) -> Vec<u8> {
    let mut out = vec![0_u8; packed_byte_len(values.len(), width)];
    let mut bit_position = 0;
    for &value in values {
        for bit in 0..width {
            if value & (1_u64 << bit) != 0 {
                out[bit_position / 8] |= 1 << (bit_position % 8);
            }
            bit_position += 1;
        }
    }
    out
}

fn unpack_bits(payload: &[u8], count: usize, width: u8) -> Result<Vec<u64>> {
    if payload.len() != packed_byte_len(count, width) {
        return Err(CodecError::Invalid("packed bit length"));
    }
    let mut values = Vec::with_capacity(count);
    let mut bit_position = 0;
    for _ in 0..count {
        let mut value = 0_u64;
        for bit in 0..width {
            if payload[bit_position / 8] & (1 << (bit_position % 8)) != 0 {
                value |= 1_u64 << bit;
            }
            bit_position += 1;
        }
        values.push(value);
    }
    Ok(values)
}

fn put_u32(out: &mut Vec<u8>, value: u32) {
    out.extend_from_slice(&value.to_be_bytes());
}

fn put_u64(out: &mut Vec<u8>, value: u64) {
    out.extend_from_slice(&value.to_be_bytes());
}

fn put_i64(out: &mut Vec<u8>, value: i64) {
    out.extend_from_slice(&value.to_be_bytes());
}

struct Reader<'a> {
    data: &'a [u8],
    position: usize,
}

impl<'a> Reader<'a> {
    fn new(data: &'a [u8]) -> Self {
        Self { data, position: 0 }
    }

    fn require_magic(&mut self, expected: &[u8]) -> Result<()> {
        if self.read_bytes(expected.len())? == expected {
            Ok(())
        } else {
            Err(CodecError::Invalid("codec magic"))
        }
    }

    fn read_u8(&mut self) -> Result<u8> {
        Ok(self.read_bytes(1)?[0])
    }

    fn read_u32(&mut self) -> Result<u32> {
        Ok(u32::from_be_bytes(
            self.read_bytes(4)?.try_into().expect("checked length"),
        ))
    }

    fn read_u64(&mut self) -> Result<u64> {
        Ok(u64::from_be_bytes(
            self.read_bytes(8)?.try_into().expect("checked length"),
        ))
    }

    fn read_i64(&mut self) -> Result<i64> {
        Ok(i64::from_be_bytes(
            self.read_bytes(8)?.try_into().expect("checked length"),
        ))
    }

    fn read_len(&mut self) -> Result<usize> {
        usize::try_from(self.read_u64()?).map_err(|_| CodecError::Invalid("row count"))
    }

    fn read_bytes(&mut self, len: usize) -> Result<&'a [u8]> {
        let end = self
            .position
            .checked_add(len)
            .ok_or(CodecError::Truncated)?;
        let bytes = self
            .data
            .get(self.position..end)
            .ok_or(CodecError::Truncated)?;
        self.position = end;
        Ok(bytes)
    }

    fn remaining(&self) -> usize {
        self.data.len() - self.position
    }

    fn require_end(&self) -> Result<()> {
        if self.position == self.data.len() {
            Ok(())
        } else {
            Err(CodecError::Invalid("trailing bytes"))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_trips_edge_cases() {
        let mut random = 0x1234_5678_9abc_def0_u64;
        let mut random_values = Vec::with_capacity(10_000);
        for _ in 0..10_000 {
            random ^= random << 13;
            random ^= random >> 7;
            random ^= random << 17;
            random_values.push(random as i64);
        }
        let cases = [
            Vec::new(),
            vec![42],
            vec![7; 1025],
            vec![i64::MIN, i64::MAX, 0, -1, 1, i64::MIN],
            (0_i64..4097).map(|value| value * value).collect(),
            random_values,
        ];
        for codec in [ExperimentalCodec::Bos, ExperimentalCodec::Subcolumn] {
            for values in &cases {
                let encoded = codec.encode_i64(values);
                assert_eq!(*values, codec.decode_i64(&encoded).unwrap());
            }
        }
    }

    #[test]
    fn rejects_truncated_payloads() {
        for codec in [ExperimentalCodec::Bos, ExperimentalCodec::Subcolumn] {
            let mut encoded = codec.encode_i64(&(0_i64..1000).collect::<Vec<_>>());
            encoded.pop();
            assert!(codec.decode_i64(&encoded).is_err());
        }
    }

    #[test]
    fn arrow_float_round_trip_is_bit_exact() {
        let mut builder = Float64Builder::with_capacity(7);
        builder.append_value(-0.0);
        builder.append_value(0.0);
        builder.append_null();
        builder.append_value(1.25);
        builder.append_value(f64::NAN);
        builder.append_value(f64::from_bits(0x7ff8_0000_0000_0001));
        builder.append_value(f64::NEG_INFINITY);
        let input = builder.finish();

        for codec in [ExperimentalCodec::Bos, ExperimentalCodec::Subcolumn] {
            let payload = codec.encode_arrow_array(&input).unwrap();
            let decoded = codec.decode_arrow_array(&payload).unwrap();
            let output = decoded.as_any().downcast_ref::<Float64Array>().unwrap();
            assert_eq!(input.nulls(), output.nulls());
            for index in 0..input.len() {
                if input.is_valid(index) {
                    assert_eq!(input.value(index).to_bits(), output.value(index).to_bits());
                }
            }
        }
    }
}
