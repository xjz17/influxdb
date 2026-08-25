package tsm1

import (
	"fmt"
	"math"
	"testing"
)

func TestFloatArraySubcolumnOptimizedMatchesReference(t *testing.T) {
	randomBits := make([]float64, 4097)
	state := uint64(0x123456789abcdef0)
	for i := range randomBits {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		randomBits[i] = math.Float64frombits(state)
	}
	structured := make([]float64, 4097)
	for i := range structured {
		structured[i] = 20 + math.Sin(float64(i)*0.013)*7 + float64(i%97)*0.001
	}
	for name, values := range map[string][]float64{
		"empty":       {},
		"single":      {42},
		"repeated":    make([]float64, 1025),
		"structured":  structured,
		"random_bits": randomBits,
		"special":     {-0.0, 0.0, math.NaN(), math.Float64frombits(0x7ff8000000000001), math.Inf(-1), math.Inf(1)},
	} {
		t.Run(name, func(t *testing.T) {
			optimized, err := floatArrayEncodeAllSubcolumn(values, nil)
			if err != nil {
				t.Fatal(err)
			}
			reference := referenceFloatArrayEncodeAllSubcolumn(values, nil)
			if string(optimized) != string(reference) {
				t.Fatalf("optimized Sub-column payload differs from the reference: got %d bytes, want %d", len(optimized), len(reference))
			}
			decoded, err := referenceFloatArrayDecodeAllSubcolumn(optimized, nil)
			if err != nil {
				t.Fatal(err)
			}
			optimizedDecoded, err := floatArrayDecodeAllSubcolumn(reference, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i := range values {
				want := math.Float64bits(values[i])
				if math.Float64bits(decoded[i]) != want || math.Float64bits(optimizedDecoded[i]) != want {
					t.Fatalf("decoder mismatch at %d", i)
				}
			}
		})
	}
}

func TestFloatSubcolumnDictionaryWidthsMatchReference(t *testing.T) {
	for width := uint8(0); width <= 4; width++ {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			distinct := 1 << width
			values := make([]byte, 512)
			for i := range values {
				values[i] = byte(i % distinct)
			}

			mode, payload := referenceEncodeFloatSubcolumnGroup(values)
			actual := appendFloatSubcolumnGroup(nil, values)
			want := append([]byte{mode}, 0, 0, 0, 0)
			want = appendFloatExperimentalU32(want[:1], uint32(len(payload)))
			want = append(want, payload...)
			if string(actual) != string(want) {
				t.Fatalf("group payload differs from reference for width %d", width)
			}

			// Width four is a valid decoder input even though its dictionary
			// overhead means the encoder normally chooses packed nibbles.
			dictionaryPayload := referenceEncodeFloatSubcolumnDictionary(values)
			if got := dictionaryPayload[1+distinct]; got != width {
				t.Fatalf("reference dictionary width: got %d, want %d", got, width)
			}
			referenceDigits, err := referenceDecodeFloatSubcolumnGroup(2, dictionaryPayload, len(values))
			if err != nil {
				t.Fatal(err)
			}
			residuals := make([]uint64, len(values))
			const shift = 12
			if err = decodeFloatSubcolumnGroupInto(2, dictionaryPayload, residuals, shift); err != nil {
				t.Fatal(err)
			}
			for i, digit := range referenceDigits {
				if got := byte(residuals[i] >> shift); got != digit {
					t.Fatalf("dictionary digit mismatch at %d: got %d, want %d", i, got, digit)
				}
			}
		})
	}
}

func TestFloatArraySubcolumnRejectsOversizedHeader(t *testing.T) {
	payload := []byte{byte(floatCompressedSubcolumn << 4)}
	payload = appendFloatExperimentalU32(payload, floatSubcolumnMaxValues+1)
	payload = appendFloatExperimentalU32(payload, floatExperimentalBlockSize)
	if _, err := floatArrayDecodeAllSubcolumn(payload, nil); err == nil {
		t.Fatal("Sub-column decoder accepted an oversized value count")
	}

	payload = payload[:1]
	payload = appendFloatExperimentalU32(payload, 0)
	payload = appendFloatExperimentalU32(payload, floatExperimentalBlockSize+1)
	if _, err := floatArrayDecodeAllSubcolumn(payload, nil); err == nil {
		t.Fatal("Sub-column decoder accepted an invalid block size")
	}
}

func BenchmarkFloatArraySubcolumnOptimization(b *testing.B) {
	values := make([]float64, 128*1024)
	for i := range values {
		values[i] = 20 + math.Sin(float64(i)*0.013)*7 + float64(i%97)*0.001
	}
	encoded := referenceFloatArrayEncodeAllSubcolumn(values, nil)
	b.Run("encode_reference", func(b *testing.B) {
		b.ReportAllocs()
		var output []byte
		for range b.N {
			output = referenceFloatArrayEncodeAllSubcolumn(values, output)
		}
		b.SetBytes(int64(len(output)))
	})
	b.Run("encode_optimized", func(b *testing.B) {
		b.ReportAllocs()
		var output []byte
		var err error
		for range b.N {
			output, err = floatArrayEncodeAllSubcolumn(values, output)
			if err != nil {
				b.Fatal(err)
			}
		}
		b.SetBytes(int64(len(output)))
	})
	b.Run("decode_reference", func(b *testing.B) {
		b.ReportAllocs()
		var output []float64
		var err error
		for range b.N {
			output, err = referenceFloatArrayDecodeAllSubcolumn(encoded, output)
			if err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		assertFloatBitsEqual(b, output, values)
		b.SetBytes(int64(len(encoded)))
	})
	b.Run("decode_optimized", func(b *testing.B) {
		b.ReportAllocs()
		var output []float64
		var err error
		for range b.N {
			output, err = floatArrayDecodeAllSubcolumn(encoded, output)
			if err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		assertFloatBitsEqual(b, output, values)
		b.SetBytes(int64(len(encoded)))
	})
}

func assertFloatBitsEqual(tb testing.TB, got, want []float64) {
	tb.Helper()
	if len(got) != len(want) {
		tb.Fatalf("decoded length: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Float64bits(got[i]) != math.Float64bits(want[i]) {
			tb.Fatalf("decoded bit mismatch at %d", i)
		}
	}
}

func referenceFloatArrayEncodeAllSubcolumn(src []float64, b []byte) []byte {
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
			maximum = max(maximum, blockResiduals[i])
		}
		groups := max(1, (int(floatExperimentalBitWidth(maximum))+3)/4)
		out = appendFloatExperimentalU32(out, uint32(blockLength))
		out = appendFloatExperimentalI64(out, minimum)
		out = append(out, byte(groups))
		blockDigits := digits[:blockLength]
		for group := range groups {
			shift := group * 4
			for i, value := range blockResiduals {
				blockDigits[i] = byte(value>>shift) & 0xf
			}
			mode, payload := referenceEncodeFloatSubcolumnGroup(blockDigits)
			out = append(out, mode)
			out = appendFloatExperimentalU32(out, uint32(len(payload)))
			out = append(out, payload...)
		}
	}
	return out
}

func referenceEncodeFloatSubcolumnGroup(values []byte) (byte, []byte) {
	packed := referencePackFloatSubcolumnNibbles(values)
	rle := referenceEncodeFloatSubcolumnRLE(values)
	dictionary := referenceEncodeFloatSubcolumnDictionary(values)
	if len(rle) < len(packed) && len(rle) <= len(dictionary) {
		return 1, rle
	}
	if len(dictionary) < len(packed) {
		return 2, dictionary
	}
	return 0, packed
}

func referencePackFloatSubcolumnNibbles(values []byte) []byte {
	out := make([]byte, (len(values)+1)/2)
	for i, value := range values {
		out[i/2] |= (value & 0xf) << ((i & 1) * 4)
	}
	return out
}

func referenceEncodeFloatSubcolumnRLE(values []byte) []byte {
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

func referenceEncodeFloatSubcolumnDictionary(values []byte) []byte {
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
	packed := referencePackFloatSubcolumnBitsLSB(indexes, width)
	out := make([]byte, 0, len(dictionary)+len(packed)+2)
	out = append(out, byte(len(dictionary)))
	out = append(out, dictionary...)
	out = append(out, width)
	out = append(out, packed...)
	return out
}

func referencePackFloatSubcolumnBitsLSB(values []uint64, width uint8) []byte {
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

func referenceFloatArrayDecodeAllSubcolumn(b []byte, dst []float64) ([]float64, error) {
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
	if err != nil || blockSize32 == 0 {
		return nil, fmt.Errorf("Sub-column float block has an invalid block size")
	}
	total := int(total32)
	if cap(dst) < total {
		dst = make([]float64, 0, total)
	} else {
		dst = dst[:0]
	}
	residuals := make([]uint64, int(blockSize32))
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
		if readErr != nil || length == 0 || length > int(blockSize32) || length > total-len(dst) || groups < 1 || groups > 16 {
			return nil, fmt.Errorf("Sub-column float block has an invalid block header")
		}
		blockResiduals := residuals[:length]
		clear(blockResiduals)
		for group := range groups {
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
			digits, groupErr := referenceDecodeFloatSubcolumnGroup(mode, payload, length)
			if groupErr != nil {
				return nil, groupErr
			}
			shift := group * 4
			for i, digit := range digits {
				blockResiduals[i] |= uint64(digit) << shift
			}
		}
		for _, residual := range blockResiduals {
			dst = append(dst, math.Float64frombits(uint64(minimum)+residual))
		}
	}
	if reader.position != len(reader.data) {
		return nil, fmt.Errorf("Sub-column float block has trailing bytes")
	}
	return dst, nil
}

func referenceDecodeFloatSubcolumnGroup(mode byte, payload []byte, length int) ([]byte, error) {
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
	case 2:
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
		width, err := reader.readU8()
		if err != nil || width > 4 || len(payload)-reader.position != floatExperimentalPackedByteLen(length, width) {
			return nil, fmt.Errorf("Sub-column dictionary width is invalid")
		}
		packed, err := reader.readBytes(len(payload) - reader.position)
		if err != nil {
			return nil, err
		}
		indexes, err := referenceUnpackFloatSubcolumnBitsLSB(packed, length, width)
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
	default:
		return nil, fmt.Errorf("Sub-column group mode is invalid")
	}
}

func referenceUnpackFloatSubcolumnBitsLSB(payload []byte, count int, width uint8) ([]uint64, error) {
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
