package tsm1

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
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
		out = append(out, make([]byte, bitmapLength+packedLength+plan.outlierCount*8)...)
		bitmap := out[payloadStart : payloadStart+bitmapLength]
		packed := out[payloadStart+bitmapLength : payloadStart+bitmapLength+packedLength]
		bitPosition := 0
		outlierPosition := payloadStart + bitmapLength + packedLength
		for i, delta := range blockDeltas {
			if delta < plan.lower || delta > plan.upper {
				bitmap[i>>3] |= 1 << (i & 7)
				binary.BigEndian.PutUint64(out[outlierPosition:], uint64(delta))
				outlierPosition += 8
				continue
			}
			floatExperimentalWriteBitsMSB(packed, &bitPosition, uint64(delta)-uint64(plan.lower), plan.width)
		}
	}
	return out, nil
}

type floatBOSOrderStat struct {
	rank  int
	value int64
	below int
	above int
}

func chooseFloatBOSPlan(values []int64) floatBOSPlan {
	trimCandidates := [...]int{0, len(values) / 100, len(values) / 50, len(values) / 20, len(values) / 10}
	var trims [len(trimCandidates)]int
	trimCount := 0
	previousTrim := -1
	for _, trim := range trimCandidates {
		if trim == previousTrim || trim*2 >= len(values) {
			continue
		}
		previousTrim = trim
		trims[trimCount] = trim
		trimCount++
	}

	var ranks [len(trimCandidates) * 2]int
	rankCount := 0
	for _, trim := range trims[:trimCount] {
		for _, rank := range [...]int{trim, len(values) - trim - 1} {
			position := sort.Search(rankCount, func(i int) bool { return ranks[i] >= rank })
			if position < rankCount && ranks[position] == rank {
				continue
			}
			copy(ranks[position+1:rankCount+1], ranks[position:rankCount])
			ranks[position] = rank
			rankCount++
		}
	}
	var statsStorage [len(ranks)]floatBOSOrderStat
	stats := statsStorage[:rankCount]
	for i, rank := range ranks[:rankCount] {
		stats[i].rank = rank
	}
	selectFloatBOSOrderStats(values, 0, len(values), stats)

	var best floatBOSPlan
	haveBest := false
	for _, trim := range trims[:trimCount] {
		lowerRank := trim
		upperRank := len(values) - trim - 1
		lowerPosition := sort.Search(len(stats), func(i int) bool { return stats[i].rank >= lowerRank })
		upperPosition := sort.Search(len(stats), func(i int) bool { return stats[i].rank >= upperRank })
		lower := stats[lowerPosition].value
		upper := stats[upperPosition].value
		width := floatExperimentalBitWidth(uint64(upper) - uint64(lower))
		outliers := stats[lowerPosition].below + stats[upperPosition].above
		inliers := len(values) - outliers
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

func selectFloatBOSOrderStats(values []int64, base, total int, stats []floatBOSOrderStat) {
	if len(stats) == 0 {
		return
	}
	pivot := floatBOSMedianOfThree(values[0], values[len(values)/2], values[len(values)-1])
	less, position, greater := 0, 0, len(values)
	for position < greater {
		switch {
		case values[position] < pivot:
			values[less], values[position] = values[position], values[less]
			less++
			position++
		case values[position] > pivot:
			greater--
			values[position], values[greater] = values[greater], values[position]
		default:
			position++
		}
	}
	firstEqual := base + less
	afterEqual := base + greater
	leftEnd := sort.Search(len(stats), func(i int) bool { return stats[i].rank >= firstEqual })
	rightStart := sort.Search(len(stats), func(i int) bool { return stats[i].rank >= afterEqual })
	for i := leftEnd; i < rightStart; i++ {
		stats[i].value = pivot
		stats[i].below = firstEqual
		stats[i].above = total - afterEqual
	}
	selectFloatBOSOrderStats(values[:less], base, total, stats[:leftEnd])
	selectFloatBOSOrderStats(values[greater:], afterEqual, total, stats[rightStart:])
}

func floatBOSMedianOfThree(first, middle, last int64) int64 {
	if first > middle {
		first, middle = middle, first
	}
	if middle > last {
		middle, last = last, middle
	}
	if first > middle {
		middle = first
	}
	return middle
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
		deltaPosition := 0
		for _, flags := range bitmap {
			count := min(8, deltaCount-deltaPosition)
			for range count {
				var delta int64
				if flags&1 != 0 {
					delta, readErr = reader.readI64()
					if readErr != nil {
						return nil, readErr
					}
				} else {
					value := floatExperimentalReadBitsMSBUnchecked(packed, &bitPosition, width)
					delta = int64(uint64(lower) + value)
				}
				previous += uint64(delta)
				dst = append(dst, math.Float64frombits(previous))
				flags >>= 1
				deltaPosition++
			}
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
	if remaining == 0 {
		return
	}
	bitOffset := *bitPosition & 7
	if bitOffset != 0 {
		take := min(8-bitOffset, remaining)
		shift := remaining - take
		mask := uint64((1 << take) - 1)
		data[*bitPosition>>3] |= byte((value>>shift)&mask) << (8 - bitOffset - take)
		*bitPosition += take
		remaining -= take
	}
	for remaining >= 8 {
		remaining -= 8
		data[*bitPosition>>3] = byte(value >> remaining)
		*bitPosition += 8
	}
	if remaining > 0 {
		mask := uint64((1 << remaining) - 1)
		data[*bitPosition>>3] |= byte(value&mask) << (8 - remaining)
		*bitPosition += remaining
	}
}

func floatExperimentalReadBitsMSB(data []byte, bitPosition *int, width uint8) (uint64, error) {
	remaining := int(width)
	if remaining == 0 {
		return 0, nil
	}
	if *bitPosition < 0 || *bitPosition > len(data)*8-remaining {
		return 0, fmt.Errorf("truncated float bit-packed payload")
	}
	return floatExperimentalReadBitsMSBUnchecked(data, bitPosition, width), nil
}

func floatExperimentalReadBitsMSBUnchecked(data []byte, bitPosition *int, width uint8) uint64 {
	remaining := int(width)
	if remaining == 0 {
		return 0
	}
	var value uint64
	bitOffset := *bitPosition & 7
	if bitOffset != 0 {
		take := min(8-bitOffset, remaining)
		shift := 8 - bitOffset - take
		mask := byte((1 << take) - 1)
		value = uint64((data[*bitPosition>>3] >> shift) & mask)
		*bitPosition += take
		remaining -= take
	}
	for remaining >= 8 {
		value = value<<8 | uint64(data[*bitPosition>>3])
		*bitPosition += 8
		remaining -= 8
	}
	if remaining > 0 {
		value = value<<remaining | uint64(data[*bitPosition>>3]>>(8-remaining))
		*bitPosition += remaining
	}
	return value
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
