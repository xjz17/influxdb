package tsm1

import (
	"fmt"
	"math"
)

func floatArrayEncodeAllSubcolumn(src []float64, b []byte) ([]byte, error) {
	if uint64(len(src)) > math.MaxUint32 {
		return nil, fmt.Errorf("Sub-column float block contains too many values")
	}

	out := b[:0]
	out = append(out, byte(floatCompressedSubcolumn<<4))
	out = appendFloatExperimentalU32(out, uint32(len(src)))
	out = appendFloatExperimentalU32(out, floatExperimentalBlockSize)
	residuals := make([]uint64, floatExperimentalBlockSize)
	digits := make([]byte, floatExperimentalBlockSize)
	for offset := 0; offset < len(src); offset += floatExperimentalBlockSize {
		blockLength := min(floatExperimentalBlockSize, len(src)-offset)
		minimum := int64(math.Float64bits(src[offset]))
		for i := 1; i < blockLength; i++ {
			value := int64(math.Float64bits(src[offset+i]))
			if value < minimum {
				minimum = value
			}
		}
		blockResiduals := residuals[:blockLength]
		var maximum uint64
		for i := 0; i < blockLength; i++ {
			blockResiduals[i] = math.Float64bits(src[offset+i]) - uint64(minimum)
			if blockResiduals[i] > maximum {
				maximum = blockResiduals[i]
			}
		}
		groups := max(1, (int(floatExperimentalBitWidth(maximum))+3)/4)
		out = appendFloatExperimentalU32(out, uint32(blockLength))
		out = appendFloatExperimentalI64(out, minimum)
		out = append(out, byte(groups))
		blockDigits := digits[:blockLength]
		for group := 0; group < groups; group++ {
			shift := group * 4
			for i, value := range blockResiduals {
				blockDigits[i] = byte(value>>shift) & 0xf
			}
			mode, payload := encodeFloatSubcolumnGroup(blockDigits)
			out = append(out, mode)
			out = appendFloatExperimentalU32(out, uint32(len(payload)))
			out = append(out, payload...)
		}
	}
	return out, nil
}

func encodeFloatSubcolumnGroup(values []byte) (byte, []byte) {
	packed := packFloatSubcolumnNibbles(values)
	rle := encodeFloatSubcolumnRLE(values)
	dictionary := encodeFloatSubcolumnDictionary(values)
	if len(rle) < len(packed) && len(rle) <= len(dictionary) {
		return 1, rle
	}
	if len(dictionary) < len(packed) {
		return 2, dictionary
	}
	return 0, packed
}

func packFloatSubcolumnNibbles(values []byte) []byte {
	out := make([]byte, (len(values)+1)/2)
	for i, value := range values {
		out[i/2] |= (value & 0xf) << ((i & 1) * 4)
	}
	return out
}

func encodeFloatSubcolumnRLE(values []byte) []byte {
	out := make([]byte, 0, len(values)/2)
	for start := 0; start < len(values); {
		end := start + 1
		for end < len(values) && values[end] == values[start] {
			end++
		}
		out = append(out, values[start])
		out = appendFloatExperimentalU32(out, uint32(end-start))
		start = end
	}
	return out
}

func encodeFloatSubcolumnDictionary(values []byte) []byte {
	dictionary := make([]byte, 0, 16)
	indexes := make([]uint64, len(values))
	for i, value := range values {
		index := -1
		for j, entry := range dictionary {
			if entry == value {
				index = j
				break
			}
		}
		if index < 0 {
			dictionary = append(dictionary, value)
			index = len(dictionary) - 1
		}
		indexes[i] = uint64(index)
	}
	width := floatExperimentalBitWidth(uint64(len(dictionary) - 1))
	packed := packFloatSubcolumnBitsLSB(indexes, width)
	out := make([]byte, 0, len(dictionary)+len(packed)+2)
	out = append(out, byte(len(dictionary)))
	out = append(out, dictionary...)
	out = append(out, width)
	out = append(out, packed...)
	return out
}

func packFloatSubcolumnBitsLSB(values []uint64, width uint8) []byte {
	out := make([]byte, floatExperimentalPackedByteLen(len(values), width))
	bitPosition := 0
	for _, value := range values {
		for bit := uint8(0); bit < width; bit++ {
			if value&(uint64(1)<<bit) != 0 {
				out[bitPosition/8] |= 1 << (bitPosition & 7)
			}
			bitPosition++
		}
	}
	return out
}

func floatArrayDecodeAllSubcolumn(b []byte, dst []float64) ([]float64, error) {
	reader := floatExperimentalReader{data: b}
	header, err := reader.readU8()
	if err != nil || header>>4 != floatCompressedSubcolumn || header&0xf != 0 {
		return nil, fmt.Errorf("Sub-column float block has an invalid header")
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
		return nil, fmt.Errorf("Sub-column float block has a zero block size")
	}
	total := int(total32)
	if cap(dst) < total {
		dst = make([]float64, 0, total)
	} else {
		dst = dst[:0]
	}
	residuals := make([]uint64, blockSize)
	for len(dst) < total {
		length32, readErr := reader.readU32()
		if readErr != nil {
			return nil, readErr
		}
		length := int(length32)
		minimum, readErr := reader.readI64()
		if readErr != nil {
			return nil, readErr
		}
		groupsByte, readErr := reader.readU8()
		groups := int(groupsByte)
		if readErr != nil || length == 0 || length > blockSize || length > total-len(dst) || groups < 1 || groups > 16 {
			return nil, fmt.Errorf("Sub-column float block has an invalid block header")
		}
		blockResiduals := residuals[:length]
		clear(blockResiduals)
		for group := 0; group < groups; group++ {
			mode, groupErr := reader.readU8()
			if groupErr != nil {
				return nil, groupErr
			}
			payloadLength32, groupErr := reader.readU32()
			if groupErr != nil {
				return nil, groupErr
			}
			payload, groupErr := reader.readBytes(int(payloadLength32))
			if groupErr != nil {
				return nil, groupErr
			}
			digits, groupErr := decodeFloatSubcolumnGroup(mode, payload, length)
			if groupErr != nil {
				return nil, groupErr
			}
			shift := group * 4
			for i, digit := range digits {
				blockResiduals[i] |= uint64(digit) << shift
			}
		}
		for _, residual := range blockResiduals {
			value := uint64(minimum) + residual
			dst = append(dst, math.Float64frombits(value))
		}
	}
	if reader.position != len(reader.data) {
		return nil, fmt.Errorf("Sub-column float block has trailing bytes")
	}
	return dst, nil
}

func decodeFloatSubcolumnGroup(mode byte, payload []byte, length int) ([]byte, error) {
	switch mode {
	case 0:
		if len(payload) != (length+1)/2 {
			return nil, fmt.Errorf("Sub-column packed digit length is invalid")
		}
		values := make([]byte, length)
		for i := range values {
			values[i] = (payload[i/2] >> ((i & 1) * 4)) & 0xf
		}
		return values, nil
	case 1:
		return decodeFloatSubcolumnRLE(payload, length)
	case 2:
		return decodeFloatSubcolumnDictionary(payload, length)
	default:
		return nil, fmt.Errorf("Sub-column group mode is invalid")
	}
}

func decodeFloatSubcolumnRLE(payload []byte, length int) ([]byte, error) {
	reader := floatExperimentalReader{data: payload}
	values := make([]byte, 0, length)
	for len(values) < length {
		value, err := reader.readU8()
		if err != nil || value > 0xf {
			return nil, fmt.Errorf("Sub-column RLE digit is invalid")
		}
		run32, err := reader.readU32()
		run := int(run32)
		if err != nil || run == 0 || run > length-len(values) {
			return nil, fmt.Errorf("Sub-column RLE run is invalid")
		}
		for range run {
			values = append(values, value)
		}
	}
	if reader.position != len(payload) {
		return nil, fmt.Errorf("Sub-column RLE group has trailing bytes")
	}
	return values, nil
}

func decodeFloatSubcolumnDictionary(payload []byte, length int) ([]byte, error) {
	reader := floatExperimentalReader{data: payload}
	dictionaryLengthByte, err := reader.readU8()
	dictionaryLength := int(dictionaryLengthByte)
	if err != nil || dictionaryLength < 1 || dictionaryLength > 16 {
		return nil, fmt.Errorf("Sub-column dictionary length is invalid")
	}
	dictionary, err := reader.readBytes(dictionaryLength)
	if err != nil {
		return nil, err
	}
	for _, value := range dictionary {
		if value > 0xf {
			return nil, fmt.Errorf("Sub-column dictionary digit is invalid")
		}
	}
	width, err := reader.readU8()
	if err != nil || width > 4 || len(payload)-reader.position != floatExperimentalPackedByteLen(length, width) {
		return nil, fmt.Errorf("Sub-column dictionary width is invalid")
	}
	packed, err := reader.readBytes(len(payload) - reader.position)
	if err != nil {
		return nil, err
	}
	indexes, err := unpackFloatSubcolumnBitsLSB(packed, length, width)
	if err != nil {
		return nil, err
	}
	values := make([]byte, length)
	for i, index := range indexes {
		if index >= uint64(len(dictionary)) {
			return nil, fmt.Errorf("Sub-column dictionary index is invalid")
		}
		values[i] = dictionary[index]
	}
	return values, nil
}

func unpackFloatSubcolumnBitsLSB(payload []byte, count int, width uint8) ([]uint64, error) {
	if len(payload) != floatExperimentalPackedByteLen(count, width) {
		return nil, fmt.Errorf("Sub-column bit-packed length is invalid")
	}
	values := make([]uint64, count)
	bitPosition := 0
	for i := range values {
		for bit := uint8(0); bit < width; bit++ {
			if payload[bitPosition/8]&(1<<(bitPosition&7)) != 0 {
				values[i] |= uint64(1) << bit
			}
			bitPosition++
		}
	}
	return values, nil
}
