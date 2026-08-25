package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/influxdata/influxdb/v2/tsdb"
	"github.com/influxdata/influxdb/v2/tsdb/engine/tsm1"
)

const (
	inputMagic = "SCDBIN01"
	blockRows  = 1000
)

type codec byte

const (
	codecBOS codec = iota + 1
	codecSubcolumn
	codecGorilla
	codecDeltaSimple8bBitcast
)

func (c codec) name() string {
	switch c {
	case codecBOS:
		return "BOS"
	case codecSubcolumn:
		return "SUBCOLUMN"
	case codecGorilla:
		return "GORILLA"
	case codecDeltaSimple8bBitcast:
		return "DELTA_SIMPLE8B_BITCAST"
	default:
		return "UNKNOWN"
	}
}

func (c codec) floatEncoding() (tsm1.FloatArrayEncoding, bool) {
	switch c {
	case codecBOS:
		return tsm1.FloatArrayEncodingBOS, true
	case codecSubcolumn:
		return tsm1.FloatArrayEncodingSubcolumn, true
	case codecGorilla:
		return tsm1.FloatArrayEncodingGorilla, true
	default:
		return 0, false
	}
}

type dataset struct {
	rows    int
	columns [][]float64
}

type result struct {
	writeTime time.Duration
	readTime  time.Duration
	fileBytes int64
	hashOK    bool
}

func main() {
	if len(os.Args) < 4 || len(os.Args) > 6 {
		fatalf("usage: tsm-codec-bench <input.scdbin> <output-dir> <dataset> [iterations] [warmups]")
	}
	iterations := parsePositiveArg(4, 3)
	warmups := parsePositiveArg(5, 1)
	data, err := readDataset(os.Args[1])
	if err != nil {
		fatalf("read input: %v", err)
	}
	if data.rows == 0 || len(data.columns) == 0 {
		fatalf("input must contain at least one row and numeric column")
	}
	if err := os.MkdirAll(os.Args[2], 0o755); err != nil {
		fatalf("create output directory: %v", err)
	}

	fmt.Println("system,dataset,codec,iteration,rows,columns,write_ms,read_ms,file_bytes,hash_ok")
	codecs := []codec{codecBOS, codecSubcolumn, codecGorilla, codecDeltaSimple8bBitcast}
	for _, selected := range codecs {
		for iteration := -warmups; iteration < iterations; iteration++ {
			runtime.GC()
			path := filepath.Join(os.Args[2], fmt.Sprintf("%s_%s_%d.tsm", sanitize(os.Args[3]), strings.ToLower(selected.name()), iteration))
			measurement, runErr := runOnce(path, data, selected)
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				fatalf("remove benchmark file: %v", removeErr)
			}
			if runErr != nil {
				fatalf("%s iteration %d: %v", selected.name(), iteration, runErr)
			}
			if iteration >= 0 {
				fmt.Printf("influxdb,%s,%s,%d,%d,%d,%.6f,%.6f,%d,%t\n",
					os.Args[3], selected.name(), iteration, data.rows, len(data.columns),
					float64(measurement.writeTime.Nanoseconds())/1e6,
					float64(measurement.readTime.Nanoseconds())/1e6,
					measurement.fileBytes, measurement.hashOK)
			}
		}
	}
}

func runOnce(path string, data dataset, selected codec) (result, error) {
	writeStart := time.Now()
	file, err := os.Create(path)
	if err != nil {
		return result{}, err
	}
	writer, err := tsm1.NewTSMWriter(file)
	if err != nil {
		_ = file.Close()
		return result{}, err
	}
	if err = writeEncodedTSM(writer, data, selected); err == nil {
		err = writer.WriteIndex()
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return result{}, err
	}
	writeTime := time.Since(writeStart)
	info, err := os.Stat(path)
	if err != nil {
		return result{}, err
	}

	readStart := time.Now()
	file, err = os.Open(path)
	if err != nil {
		return result{}, err
	}
	reader, err := tsm1.NewTSMReader(file)
	if err != nil {
		_ = file.Close()
		return result{}, err
	}
	hashOK, err := decodeAndValidate(reader, data, selected)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return result{}, err
	}
	return result{writeTime: writeTime, readTime: time.Since(readStart), fileBytes: info.Size(), hashOK: hashOK}, nil
}

func writeEncodedTSM(writer tsm1.TSMWriter, data dataset, selected codec) error {
	for columnIndex, column := range data.columns {
		key := benchmarkKey(columnIndex)
		for offset := 0; offset < data.rows; offset += blockRows {
			length := min(blockRows, data.rows-offset)
			block, err := encodeBlock(column[offset:offset+length], offset, selected)
			if err != nil {
				return err
			}
			if err = writer.WriteBlock(key, int64(offset), int64(offset+length-1), block); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeBlock(values []float64, offset int, selected codec) ([]byte, error) {
	timestamps := make([]int64, len(values))
	for i := range timestamps {
		timestamps[i] = int64(offset + i)
	}
	if encoding, ok := selected.floatEncoding(); ok {
		valueCopy := append([]float64(nil), values...)
		array := &tsdb.FloatArray{Timestamps: timestamps, Values: valueCopy}
		return tsm1.EncodeFloatArrayBlockWithEncoding(array, nil, encoding)
	}
	if selected == codecDeltaSimple8bBitcast {
		bitValues := make([]int64, len(values))
		for i, value := range values {
			bitValues[i] = int64(math.Float64bits(value))
		}
		array := &tsdb.IntegerArray{Timestamps: timestamps, Values: bitValues}
		return tsm1.EncodeIntegerArrayBlock(array, nil)
	}
	return nil, fmt.Errorf("unknown codec %d", selected)
}

func decodeAndValidate(reader *tsm1.TSMReader, expected dataset, selected codec) (bool, error) {
	rowsByColumn := make([]int, len(expected.columns))
	columnIndex := 0
	iterator := reader.BlockIterator()
	for iterator.Next() {
		key, minTime, maxTime, blockType, _, block, err := iterator.Read()
		if err != nil {
			return false, err
		}
		for columnIndex < len(expected.columns) && !bytesEqual(key, benchmarkKey(columnIndex)) {
			columnIndex++
		}
		if columnIndex >= len(expected.columns) || !bytesEqual(key, benchmarkKey(columnIndex)) {
			return false, fmt.Errorf("unexpected TSM key %q", key)
		}
		row := rowsByColumn[columnIndex]
		expectedColumn := expected.columns[columnIndex]
		if minTime != int64(row) {
			return false, nil
		}
		if selected == codecDeltaSimple8bBitcast {
			if blockType != tsm1.BlockInteger {
				return false, nil
			}
			var decoded tsdb.IntegerArray
			if err = tsm1.DecodeIntegerArrayBlock(block, &decoded); err != nil {
				return false, err
			}
			for i, value := range decoded.Values {
				if row+i >= len(expectedColumn) || decoded.Timestamps[i] != int64(row+i) || uint64(value) != math.Float64bits(expectedColumn[row+i]) {
					return false, nil
				}
			}
			row += len(decoded.Values)
		} else {
			if blockType != tsm1.BlockFloat64 {
				return false, nil
			}
			var decoded tsdb.FloatArray
			if err = tsm1.DecodeFloatArrayBlock(block, &decoded); err != nil {
				return false, err
			}
			for i, value := range decoded.Values {
				if row+i >= len(expectedColumn) || decoded.Timestamps[i] != int64(row+i) || math.Float64bits(value) != math.Float64bits(expectedColumn[row+i]) {
					return false, nil
				}
			}
			row += len(decoded.Values)
		}
		if maxTime != int64(row-1) {
			return false, nil
		}
		rowsByColumn[columnIndex] = row
	}
	if err := iterator.Err(); err != nil {
		return false, err
	}
	for index, rows := range rowsByColumn {
		if rows != expected.rows {
			return false, fmt.Errorf("decoded row count mismatch for column %d", index)
		}
	}
	return true, nil
}

func benchmarkKey(column int) []byte {
	return tsm1.SeriesFieldKeyBytes(fmt.Sprintf("benchmark,column=%08d", column), "value")
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func readDataset(path string) (dataset, error) {
	file, err := os.Open(path)
	if err != nil {
		return dataset{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	magic := make([]byte, len(inputMagic))
	if _, err = io.ReadFull(reader, magic); err != nil || string(magic) != inputMagic {
		return dataset{}, fmt.Errorf("invalid SCDBIN input magic")
	}
	var columns uint32
	var rows uint64
	if err = binary.Read(reader, binary.BigEndian, &columns); err != nil {
		return dataset{}, err
	}
	if err = binary.Read(reader, binary.BigEndian, &rows); err != nil {
		return dataset{}, err
	}
	if rows > uint64(^uint(0)>>1) {
		return dataset{}, fmt.Errorf("row count does not fit int")
	}
	for range columns {
		var nameLength uint16
		if err = binary.Read(reader, binary.BigEndian, &nameLength); err != nil {
			return dataset{}, err
		}
		if _, err = io.CopyN(io.Discard, reader, int64(nameLength)); err != nil {
			return dataset{}, err
		}
	}
	data := dataset{rows: int(rows), columns: make([][]float64, int(columns))}
	var raw [8]byte
	for column := range data.columns {
		data.columns[column] = make([]float64, data.rows)
		for row := range data.columns[column] {
			if _, err = io.ReadFull(reader, raw[:]); err != nil {
				return dataset{}, err
			}
			data.columns[column][row] = math.Float64frombits(binary.BigEndian.Uint64(raw[:]))
		}
	}
	if _, err = reader.ReadByte(); err == nil {
		return dataset{}, fmt.Errorf("SCDBIN input has trailing bytes")
	} else if !errors.Is(err, io.EOF) {
		return dataset{}, err
	}
	return data, nil
}

func parsePositiveArg(index, fallback int) int {
	if len(os.Args) <= index {
		return fallback
	}
	value, err := strconv.Atoi(os.Args[index])
	if err != nil || value <= 0 {
		fatalf("argument %d must be a positive integer", index+1)
	}
	return value
}

func sanitize(value string) string {
	return strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("_.-", character) {
			return character
		}
		return '_'
	}, value)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
