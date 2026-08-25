package tsm1

import (
	"encoding/binary"
	"fmt"
	"math"
)

// TSM normally limits a value block to 10,000 points. Keep a much larger
// explicit wire limit so a corrupt header cannot request a multi-gigabyte
// allocation before the payload is validated.
const floatSubcolumnMaxValues = 1 << 20

func floatArrayEncodeAllSubcolumn(src []float64, b []byte) ([]byte, error) {
	if len(src) > floatSubcolumnMaxValues {
		return nil, fmt.Errorf("Sub-column float block contains too many values")
	}

	out := b[:0]
	out = append(out, byte(floatCompressedSubcolumn<<4))
	out = appendFloatExperimentalU32(out, uint32(len(src)))
	out = appendFloatExperimentalU32(out, floatExperimentalBlockSize)
	residuals := make([]uint64, floatExperimentalBlockSize)
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
		for group := 0; group < groups; group++ {
			out = appendFloatSubcolumnGroup(out, blockResiduals, group*4)
		}
	}
	return out, nil
}

func appendFloatSubcolumnGroup(out []byte, residuals []uint64, shift int) []byte {
	packedLength := (len(residuals) + 1) / 2
	indexes := [16]int8{-1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1}
	var dictionary [16]byte
	dictionaryLength := 0
	runs := 0
	previous := byte(0)
	rleCandidate := true
	dictionaryCandidate := true
	for i, residual := range residuals {
		value := byte(residual>>shift) & 0xf
		if rleCandidate && (i == 0 || value != previous) {
			runs++
			previous = value
			if runs*5 >= packedLength {
				rleCandidate = false
			}
		}
		if dictionaryCandidate && indexes[value] < 0 {
			indexes[value] = int8(dictionaryLength)
			dictionary[dictionaryLength] = value
			dictionaryLength++
			width := floatExperimentalBitWidth(uint64(dictionaryLength - 1))
			minimumLength := 1 + dictionaryLength + 1 + floatExperimentalPackedByteLen(len(residuals), width)
			if minimumLength >= packedLength {
				dictionaryCandidate = false
			}
		}
		if !rleCandidate && !dictionaryCandidate {
			break
		}
	}
	rleLength := packedLength + 1
	if rleCandidate {
		rleLength = runs * 5
	}
	width := uint8(0)
	dictionaryLengthBytes := packedLength + 1
	if dictionaryCandidate {
		width = floatExperimentalBitWidth(uint64(dictionaryLength - 1))
		dictionaryLengthBytes = 1 + dictionaryLength + 1 + floatExperimentalPackedByteLen(len(residuals), width)
	}

	mode := byte(0)
	payloadLength := packedLength
	if rleLength < packedLength && rleLength <= dictionaryLengthBytes {
		mode = 1
		payloadLength = rleLength
	} else if dictionaryLengthBytes < packedLength {
		mode = 2
		payloadLength = dictionaryLengthBytes
	}
	out = append(out, mode)
	out = appendFloatExperimentalU32(out, uint32(payloadLength))

	switch mode {
	case 0:
		payloadStart := len(out)
		out = append(out, make([]byte, packedLength)...)
		i := 0
		for ; i+8 <= len(residuals); i += 8 {
			word := uint32(byte(residuals[i]>>shift)&0xf) |
				uint32(byte(residuals[i+1]>>shift)&0xf)<<4 |
				uint32(byte(residuals[i+2]>>shift)&0xf)<<8 |
				uint32(byte(residuals[i+3]>>shift)&0xf)<<12 |
				uint32(byte(residuals[i+4]>>shift)&0xf)<<16 |
				uint32(byte(residuals[i+5]>>shift)&0xf)<<20 |
				uint32(byte(residuals[i+6]>>shift)&0xf)<<24 |
				uint32(byte(residuals[i+7]>>shift)&0xf)<<28
			binary.LittleEndian.PutUint32(out[payloadStart+i/2:], word)
		}
		for ; i+1 < len(residuals); i += 2 {
			low := byte(residuals[i]>>shift) & 0xf
			high := byte(residuals[i+1]>>shift) & 0xf
			out[payloadStart+i/2] = low | high<<4
		}
		if i < len(residuals) {
			out[payloadStart+i/2] = byte(residuals[i]>>shift) & 0xf
		}
	case 1:
		for start := 0; start < len(residuals); {
			value := byte(residuals[start]>>shift) & 0xf
			end := start + 1
			for end < len(residuals) && byte(residuals[end]>>shift)&0xf == value {
				end++
			}
			out = append(out, value)
			out = appendFloatExperimentalU32(out, uint32(end-start))
			start = end
		}
	case 2:
		out = append(out, byte(dictionaryLength))
		out = append(out, dictionary[:dictionaryLength]...)
		out = append(out, width)
		packedLength := floatExperimentalPackedByteLen(len(residuals), width)
		payloadStart := len(out)
		out = append(out, make([]byte, packedLength)...)
		if width == 0 {
			break
		}
		payloadPosition := payloadStart
		start := 0
		if width == 3 {
			for ; start+8 <= len(residuals); start += 8 {
				word := uint32(byte(indexes[byte(residuals[start]>>shift)&0xf])) |
					uint32(byte(indexes[byte(residuals[start+1]>>shift)&0xf]))<<3 |
					uint32(byte(indexes[byte(residuals[start+2]>>shift)&0xf]))<<6 |
					uint32(byte(indexes[byte(residuals[start+3]>>shift)&0xf]))<<9 |
					uint32(byte(indexes[byte(residuals[start+4]>>shift)&0xf]))<<12 |
					uint32(byte(indexes[byte(residuals[start+5]>>shift)&0xf]))<<15 |
					uint32(byte(indexes[byte(residuals[start+6]>>shift)&0xf]))<<18 |
					uint32(byte(indexes[byte(residuals[start+7]>>shift)&0xf]))<<21
				out[payloadPosition] = byte(word)
				out[payloadPosition+1] = byte(word >> 8)
				out[payloadPosition+2] = byte(word >> 16)
				payloadPosition += 3
			}
		}
		for ; start < len(residuals); start += 8 {
			count := min(8, len(residuals)-start)
			var word uint32
			for offset := 0; offset < count; offset++ {
				value := byte(residuals[start+offset]>>shift) & 0xf
				word |= uint32(byte(indexes[value])) << (offset * int(width))
			}
			bytes := (count*int(width) + 7) / 8
			for offset := 0; offset < bytes; offset++ {
				out[payloadPosition+offset] = byte(word >> (offset * 8))
			}
			payloadPosition += bytes
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
	if total32 > floatSubcolumnMaxValues {
		return nil, fmt.Errorf("Sub-column float block contains too many values")
	}
	blockSize := int(blockSize32)
	if blockSize != floatExperimentalBlockSize {
		return nil, fmt.Errorf("Sub-column float block has an invalid block size")
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
		var constantResidual uint64
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
			if mode == 2 && len(payload) == 3 && payload[0] == 1 && payload[1] <= 0xf && payload[2] == 0 {
				constantResidual |= uint64(payload[1]) << (group * 4)
				continue
			}
			if groupErr = decodeFloatSubcolumnGroupInto(mode, payload, blockResiduals, group*4); groupErr != nil {
				return nil, groupErr
			}
		}
		for _, residual := range blockResiduals {
			value := uint64(minimum) + (residual | constantResidual)
			dst = append(dst, math.Float64frombits(value))
		}
	}
	if reader.position != len(reader.data) {
		return nil, fmt.Errorf("Sub-column float block has trailing bytes")
	}
	return dst, nil
}

func decodeFloatSubcolumnGroupInto(mode byte, payload []byte, residuals []uint64, shift int) error {
	switch mode {
	case 0:
		if len(payload) != (len(residuals)+1)/2 {
			return fmt.Errorf("Sub-column packed digit length is invalid")
		}
		position := 0
		payloadPosition := 0
		for ; position+8 <= len(residuals); position += 8 {
			word := binary.LittleEndian.Uint32(payload[payloadPosition:])
			residuals[position] |= uint64(word&0xf) << shift
			residuals[position+1] |= uint64(word>>4&0xf) << shift
			residuals[position+2] |= uint64(word>>8&0xf) << shift
			residuals[position+3] |= uint64(word>>12&0xf) << shift
			residuals[position+4] |= uint64(word>>16&0xf) << shift
			residuals[position+5] |= uint64(word>>20&0xf) << shift
			residuals[position+6] |= uint64(word>>24&0xf) << shift
			residuals[position+7] |= uint64(word>>28) << shift
			payloadPosition += 4
		}
		for _, value := range payload[payloadPosition:] {
			residuals[position] |= uint64(value&0xf) << shift
			position++
			if position < len(residuals) {
				residuals[position] |= uint64(value>>4) << shift
				position++
			}
		}
		return nil
	case 1:
		return decodeFloatSubcolumnRLEInto(payload, residuals, shift)
	case 2:
		return decodeFloatSubcolumnDictionaryInto(payload, residuals, shift)
	default:
		return fmt.Errorf("Sub-column group mode is invalid")
	}
}

func decodeFloatSubcolumnRLEInto(payload []byte, residuals []uint64, shift int) error {
	reader := floatExperimentalReader{data: payload}
	position := 0
	for position < len(residuals) {
		value, err := reader.readU8()
		if err != nil || value > 0xf {
			return fmt.Errorf("Sub-column RLE digit is invalid")
		}
		run32, err := reader.readU32()
		run := int(run32)
		if err != nil || run == 0 || run > len(residuals)-position {
			return fmt.Errorf("Sub-column RLE run is invalid")
		}
		for end := position + run; position < end; position++ {
			residuals[position] |= uint64(value) << shift
		}
	}
	if reader.position != len(payload) {
		return fmt.Errorf("Sub-column RLE group has trailing bytes")
	}
	return nil
}

func decodeFloatSubcolumnDictionaryInto(payload []byte, residuals []uint64, shift int) error {
	reader := floatExperimentalReader{data: payload}
	dictionaryLengthByte, err := reader.readU8()
	dictionaryLength := int(dictionaryLengthByte)
	if err != nil || dictionaryLength < 1 || dictionaryLength > 16 {
		return fmt.Errorf("Sub-column dictionary length is invalid")
	}
	dictionary, err := reader.readBytes(dictionaryLength)
	if err != nil {
		return err
	}
	for _, value := range dictionary {
		if value > 0xf {
			return fmt.Errorf("Sub-column dictionary digit is invalid")
		}
	}
	width, err := reader.readU8()
	if err != nil || width > 4 || len(payload)-reader.position != floatExperimentalPackedByteLen(len(residuals), width) {
		return fmt.Errorf("Sub-column dictionary width is invalid")
	}
	packed, err := reader.readBytes(len(payload) - reader.position)
	if err != nil {
		return err
	}
	if width == 0 {
		value := uint64(dictionary[0]) << shift
		for i := range residuals {
			residuals[i] |= value
		}
		return nil
	}
	mask := uint32((1 << width) - 1)
	payloadPosition := 0
	start := 0
	if width == 3 {
		dictionarySize := uint32(len(dictionary))
		for ; start+8 <= len(residuals); start += 8 {
			word := uint32(packed[payloadPosition]) |
				uint32(packed[payloadPosition+1])<<8 |
				uint32(packed[payloadPosition+2])<<16
			index0 := word & 7
			index1 := word >> 3 & 7
			index2 := word >> 6 & 7
			index3 := word >> 9 & 7
			index4 := word >> 12 & 7
			index5 := word >> 15 & 7
			index6 := word >> 18 & 7
			index7 := word >> 21
			if index0 >= dictionarySize || index1 >= dictionarySize || index2 >= dictionarySize || index3 >= dictionarySize ||
				index4 >= dictionarySize || index5 >= dictionarySize || index6 >= dictionarySize || index7 >= dictionarySize {
				return fmt.Errorf("Sub-column dictionary index is invalid")
			}
			residuals[start] |= uint64(dictionary[index0]) << shift
			residuals[start+1] |= uint64(dictionary[index1]) << shift
			residuals[start+2] |= uint64(dictionary[index2]) << shift
			residuals[start+3] |= uint64(dictionary[index3]) << shift
			residuals[start+4] |= uint64(dictionary[index4]) << shift
			residuals[start+5] |= uint64(dictionary[index5]) << shift
			residuals[start+6] |= uint64(dictionary[index6]) << shift
			residuals[start+7] |= uint64(dictionary[index7]) << shift
			payloadPosition += 3
		}
	}
	for ; start < len(residuals); start += 8 {
		count := min(8, len(residuals)-start)
		bytes := (count*int(width) + 7) / 8
		var word uint32
		for offset := 0; offset < bytes; offset++ {
			word |= uint32(packed[payloadPosition+offset]) << (offset * 8)
		}
		for offset := 0; offset < count; offset++ {
			index := int(word & mask)
			if index >= len(dictionary) {
				return fmt.Errorf("Sub-column dictionary index is invalid")
			}
			residuals[start+offset] |= uint64(dictionary[index]) << shift
			word >>= width
		}
		payloadPosition += bytes
	}
	return nil
}
