package tsm1

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"
	"strings"
)

const (
	floatCompressedBOS         = 2
	floatCompressedSubcolumn   = 3
	floatExperimentalBlockSize = 512
)

// FloatArrayEncoding identifies a value encoding stored in the high nibble of
// an InfluxDB TSM float value block.
type FloatArrayEncoding byte

const (
	FloatArrayEncodingGorilla   FloatArrayEncoding = floatCompressedGorilla
	FloatArrayEncodingBOS       FloatArrayEncoding = floatCompressedBOS
	FloatArrayEncodingSubcolumn FloatArrayEncoding = floatCompressedSubcolumn
)

// ParseFloatArrayEncoding parses the storage configuration spelling for a
// TSM float value codec.
func ParseFloatArrayEncoding(name string) (FloatArrayEncoding, error) {
	switch strings.ToLower(name) {
	case "gorilla":
		return FloatArrayEncodingGorilla, nil
	case "bos":
		return FloatArrayEncodingBOS, nil
	case "subcolumn":
		return FloatArrayEncodingSubcolumn, nil
	default:
		return 0, fmt.Errorf("unknown TSM float encoding %q", name)
	}
}

// FloatArrayEncodeAllWithEncoding encodes a TSM float value block with the
// selected value encoding. FloatArrayEncodeAll continues to select Gorilla so
// existing callers and files retain their original format.
func FloatArrayEncodeAllWithEncoding(src []float64, b []byte, encoding FloatArrayEncoding) ([]byte, error) {
	switch encoding {
	case FloatArrayEncodingGorilla:
		return floatArrayEncodeAllGorilla(src, b)
	case FloatArrayEncodingBOS:
		return floatArrayEncodeAllBOS(src, b)
	case FloatArrayEncodingSubcolumn:
		return floatArrayEncodeAllSubcolumn(src, b)
	default:
		return nil, fmt.Errorf("FloatArrayEncodeAllWithEncoding: unknown encoding %d", encoding)
	}
}

type floatBOSPlan struct {
	lower        int64
	upper        int64
	width        uint8
	outlierCount int
	cost         uint64
}

func floatArrayEncodeAllBOS(src []float64, b []byte) ([]byte, error) {
	if uint64(len(src)) > math.MaxUint32 {
		return nil, fmt.Errorf("BOS float block contains too many values")
	}

	out := b[:0]
	out = append(out, byte(floatCompressedBOS<<4))
	out = appendFloatExperimentalU32(out, uint32(len(src)))
	out = appendFloatExperimentalU32(out, floatExperimentalBlockSize)
	deltas := make([]int64, floatExperimentalBlockSize-1)
	sorted := make([]int64, floatExperimentalBlockSize-1)

	for offset := 0; offset < len(src); offset += floatExperimentalBlockSize {
		blockLength := min(floatExperimentalBlockSize, len(src)-offset)
		out = appendFloatExperimentalU32(out, uint32(blockLength))
		first := int64(math.Float64bits(src[offset]))
		out = appendFloatExperimentalI64(out, first)
		if blockLength == 1 {
			out = appendFloatExperimentalI64(out, 0)
			out = append(out, 0)
			out = appendFloatExperimentalU32(out, 0)
			out = appendFloatExperimentalU32(out, 0)
			out = appendFloatExperimentalU32(out, 0)
			continue
		}

		blockDeltas := deltas[:blockLength-1]
		previous := uint64(first)
		for i := 1; i < blockLength; i++ {
			current := math.Float64bits(src[offset+i])
			blockDeltas[i-1] = int64(current - previous)
			previous = current
		}
		blockSorted := sorted[:len(blockDeltas)]
		copy(blockSorted, blockDeltas)
		slices.Sort(blockSorted)
		plan := chooseFloatBOSPlan(blockSorted)
		bitmapLength := (len(blockDeltas) + 7) >> 3
		inlierCount := len(blockDeltas) - plan.outlierCount
		packedLength := floatExperimentalPackedByteLen(inlierCount, plan.width)

		out = appendFloatExperimentalI64(out, plan.lower)
		out = append(out, plan.width)
		out = appendFloatExperimentalU32(out, uint32(bitmapLength))
		out = appendFloatExperimentalU32(out, uint32(packedLength))
		out = appendFloatExperimentalU32(out, uint32(plan.outlierCount))
		payloadStart := len(out)
		out = append(out, make([]byte, bitmapLength+packedLength)...)
		bitmap := out[payloadStart : payloadStart+bitmapLength]
		packed := out[payloadStart+bitmapLength:]
		bitPosition := 0
		for i, delta := range blockDeltas {
			if delta < plan.lower || delta > plan.upper {
				bitmap[i>>3] |= 1 << (i & 7)
				continue
			}
			floatExperimentalWriteBitsMSB(packed, &bitPosition, uint64(delta)-uint64(plan.lower), plan.width)
		}
		for _, delta := range blockDeltas {
			if delta < plan.lower || delta > plan.upper {
				out = appendFloatExperimentalI64(out, delta)
			}
		}
	}
	return out, nil
}

func chooseFloatBOSPlan(sorted []int64) floatBOSPlan {
	trims := [...]int{0, len(sorted) / 100, len(sorted) / 50, len(sorted) / 20, len(sorted) / 10}
	var best floatBOSPlan
	haveBest := false
	previousTrim := -1
	for _, trim := range trims {
		if trim == previousTrim || trim*2 >= len(sorted) {
			continue
		}
		previousTrim = trim
		lower := sorted[trim]
		upper := sorted[len(sorted)-trim-1]
		width := floatExperimentalBitWidth(uint64(upper) - uint64(lower))
		below := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= lower })
		atMostUpper := sort.Search(len(sorted), func(i int) bool { return sorted[i] > upper })
		outliers := below + len(sorted) - atMostUpper
		inliers := len(sorted) - outliers
		cost := uint64(floatExperimentalPackedByteLen(inliers, width)) + uint64(outliers)*8
		candidate := floatBOSPlan{
			lower: lower, upper: upper, width: width, outlierCount: outliers, cost: cost,
		}
		if !haveBest || candidate.cost < best.cost ||
			(candidate.cost == best.cost && candidate.outlierCount < best.outlierCount) {
			best = candidate
			haveBest = true
		}
	}
	return best
}

func floatArrayDecodeAllBOS(b []byte, dst []float64) ([]float64, error) {
	reader := floatExperimentalReader{data: b}
	header, err := reader.readU8()
	if err != nil || header>>4 != floatCompressedBOS || header&0xf != 0 {
		return nil, fmt.Errorf("BOS float block has an invalid header")
	}
	total32, err := reader.readU32()
	if err != nil {
		return nil, err
	}
	blockSize32, err := reader.readU32()
	if err != nil {
		return nil, err
	}
	blockSize := int(blockSize32)
	if blockSize == 0 {
		return nil, fmt.Errorf("BOS float block has a zero block size")
	}
	total := int(total32)
	if cap(dst) < total {
		dst = make([]float64, 0, total)
	} else {
		dst = dst[:0]
	}

	for len(dst) < total {
		length32, readErr := reader.readU32()
		if readErr != nil {
			return nil, readErr
		}
		length := int(length32)
		if length == 0 || length > blockSize || length > total-len(dst) {
			return nil, fmt.Errorf("BOS float block has an invalid block length")
		}
		first, readErr := reader.readI64()
		if readErr != nil {
			return nil, readErr
		}
		lower, readErr := reader.readI64()
		if readErr != nil {
			return nil, readErr
		}
		width, readErr := reader.readU8()
		if readErr != nil || width > 64 {
			return nil, fmt.Errorf("BOS float block has an invalid bit width")
		}
		bitmapLength32, readErr := reader.readU32()
		if readErr != nil {
			return nil, readErr
		}
		packedLength32, readErr := reader.readU32()
		if readErr != nil {
			return nil, readErr
		}
		outlierCount32, readErr := reader.readU32()
		if readErr != nil {
			return nil, readErr
		}
		deltaCount := length - 1
		bitmapLength := int(bitmapLength32)
		packedLength := int(packedLength32)
		outlierCount := int(outlierCount32)
		if bitmapLength != (deltaCount+7)>>3 || outlierCount > deltaCount {
			return nil, fmt.Errorf("BOS float block has an invalid payload header")
		}
		bitmap, readErr := reader.readBytes(bitmapLength)
		if readErr != nil {
			return nil, readErr
		}
		actualOutliers := 0
		for _, value := range bitmap {
			actualOutliers += bits.OnesCount8(value)
		}
		if actualOutliers != outlierCount {
			return nil, fmt.Errorf("BOS float block has an invalid outlier bitmap")
		}
		inlierCount := deltaCount - outlierCount
		if packedLength != floatExperimentalPackedByteLen(inlierCount, width) {
			return nil, fmt.Errorf("BOS float block has an invalid packed length")
		}
		packed, readErr := reader.readBytes(packedLength)
		if readErr != nil {
			return nil, readErr
		}

		previous := uint64(first)
		dst = append(dst, math.Float64frombits(previous))
		bitPosition := 0
		for i := 0; i < deltaCount; i++ {
			var delta int64
			if bitmap[i>>3]&(1<<(i&7)) != 0 {
				delta, readErr = reader.readI64()
				if readErr != nil {
					return nil, readErr
				}
			} else {
				value, bitErr := floatExperimentalReadBitsMSB(packed, &bitPosition, width)
				if bitErr != nil {
					return nil, bitErr
				}
				delta = int64(uint64(lower) + value)
			}
			previous += uint64(delta)
			dst = append(dst, math.Float64frombits(previous))
		}
	}
	if reader.position != len(reader.data) {
		return nil, fmt.Errorf("BOS float block has trailing bytes")
	}
	return dst, nil
}

func floatExperimentalBitWidth(value uint64) uint8 {
	return uint8(bits.Len64(value))
}

func floatExperimentalPackedByteLen(count int, width uint8) int {
	return (count*int(width) + 7) >> 3
}

func floatExperimentalWriteBitsMSB(data []byte, bitPosition *int, value uint64, width uint8) {
	remaining := int(width)
	for remaining > 0 {
		bitOffset := *bitPosition & 7
		take := min(8-bitOffset, remaining)
		shift := remaining - take
		mask := uint64((1 << take) - 1)
		chunk := byte((value >> shift) & mask)
		data[*bitPosition>>3] |= chunk << (8 - bitOffset - take)
		*bitPosition += take
		remaining -= take
		if bitOffset == 0 && remaining >= 8 {
			wholeBytes := remaining >> 3
			for range wholeBytes {
				shift = remaining - 8
				data[*bitPosition>>3] = byte(value >> shift)
				*bitPosition += 8
				remaining -= 8
			}
		}
	}
}

func floatExperimentalReadBitsMSB(data []byte, bitPosition *int, width uint8) (uint64, error) {
	var value uint64
	remaining := int(width)
	for remaining > 0 {
		if *bitPosition>>3 >= len(data) {
			return 0, fmt.Errorf("truncated float bit-packed payload")
		}
		bitOffset := *bitPosition & 7
		take := min(8-bitOffset, remaining)
		shift := 8 - bitOffset - take
		mask := byte((1 << take) - 1)
		value = (value << take) | uint64((data[*bitPosition>>3]>>shift)&mask)
		*bitPosition += take
		remaining -= take
	}
	return value, nil
}

func appendFloatExperimentalU32(out []byte, value uint32) []byte {
	return binary.BigEndian.AppendUint32(out, value)
}

func appendFloatExperimentalI64(out []byte, value int64) []byte {
	return binary.BigEndian.AppendUint64(out, uint64(value))
}

type floatExperimentalReader struct {
	data     []byte
	position int
}

func (r *floatExperimentalReader) readU8() (uint8, error) {
	bytes, err := r.readBytes(1)
	if err != nil {
		return 0, err
	}
	return bytes[0], nil
}

func (r *floatExperimentalReader) readU32() (uint32, error) {
	bytes, err := r.readBytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(bytes), nil
}

func (r *floatExperimentalReader) readI64() (int64, error) {
	bytes, err := r.readBytes(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(bytes)), nil
}

func (r *floatExperimentalReader) readBytes(length int) ([]byte, error) {
	if length < 0 || r.position > len(r.data)-length {
		return nil, fmt.Errorf("truncated experimental float payload")
	}
	bytes := r.data[r.position : r.position+length]
	r.position += length
	return bytes, nil
}
