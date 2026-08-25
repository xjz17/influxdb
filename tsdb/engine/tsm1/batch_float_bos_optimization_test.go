package tsm1

import (
	"fmt"
	"math"
	"sort"
	"testing"
)

func TestFloatArrayBOSOptimizedMatchesReference(t *testing.T) {
	values := make([]float64, 4097)
	state := uint64(0x123456789abcdef0)
	for i := range values {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		values[i] = math.Float64frombits(state)
	}
	optimized, err := floatArrayEncodeAllBOS(values, nil)
	if err != nil {
		t.Fatal(err)
	}
	reference := referenceFloatArrayEncodeAllBOS(values, nil)
	if string(optimized) != string(reference) {
		t.Fatalf("optimized BOS payload differs from the reference: got %d bytes, want %d", len(optimized), len(reference))
	}
	decoded, err := referenceFloatArrayDecodeAllBOS(optimized, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range values {
		if math.Float64bits(decoded[i]) != math.Float64bits(values[i]) {
			t.Fatalf("reference decoder mismatch at %d", i)
		}
	}
}

func TestFloatExperimentalMSBBitsMatchReference(t *testing.T) {
	state := uint64(0x123456789abcdef0)
	for width := uint8(0); width <= 64; width++ {
		values := make([]uint64, 67)
		for i := range values {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			values[i] = state
			if width < 64 {
				values[i] &= (uint64(1) << width) - 1
			}
		}
		want := referencePackBitsMSB(values, width)
		got := make([]byte, len(want))
		position := 0
		for _, value := range values {
			floatExperimentalWriteBitsMSB(got, &position, value, width)
		}
		if string(got) != string(want) {
			t.Fatalf("width %d packed bytes differ", width)
		}
		position = 0
		for i, value := range values {
			decoded, err := floatExperimentalReadBitsMSB(got, &position, width)
			if err != nil {
				t.Fatal(err)
			}
			if decoded != value {
				t.Fatalf("width %d value %d: got %x, want %x", width, i, decoded, value)
			}
		}
	}
}

func TestChooseFloatBOSPlanMatchesReference(t *testing.T) {
	state := uint64(0x123456789abcdef0)
	for length := 1; length <= floatExperimentalBlockSize-1; length++ {
		values := make([]int64, length)
		for i := range values {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			if length%3 == 0 {
				values[i] = int64(state % 17)
			} else {
				values[i] = int64(state)
			}
		}
		want := referenceChooseFloatBOSPlan(values)
		working := append([]int64(nil), values...)
		if got := chooseFloatBOSPlan(working); got != want {
			t.Fatalf("length %d plan: got %+v, want %+v", length, got, want)
		}
	}
	for name, values := range map[string][]int64{
		"all_equal": make([]int64, floatExperimentalBlockSize-1),
		"ascending": func() []int64 {
			values := make([]int64, floatExperimentalBlockSize-1)
			for i := range values {
				values[i] = int64(i) - 255
			}
			return values
		}(),
		"descending": func() []int64 {
			values := make([]int64, floatExperimentalBlockSize-1)
			for i := range values {
				values[i] = 255 - int64(i)
			}
			return values
		}(),
		"extremes": {math.MinInt64, math.MaxInt64, 0, math.MinInt64, math.MaxInt64},
	} {
		t.Run(name, func(t *testing.T) {
			want := referenceChooseFloatBOSPlan(values)
			working := append([]int64(nil), values...)
			if got := chooseFloatBOSPlan(working); got != want {
				t.Fatalf("plan: got %+v, want %+v", got, want)
			}
		})
	}
}

func BenchmarkFloatArrayBOSOptimization(b *testing.B) {
	values := make([]float64, 128*1024)
	for i := range values {
		values[i] = 20 + math.Sin(float64(i)*0.013)*7 + float64(i%97)*0.001
	}
	encoded, err := floatArrayEncodeAllBOS(values, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("encode_reference", func(b *testing.B) {
		b.ReportAllocs()
		var output []byte
		for range b.N {
			output = referenceFloatArrayEncodeAllBOS(values, output)
		}
		b.SetBytes(int64(len(output)))
	})
	b.Run("encode_optimized", func(b *testing.B) {
		b.ReportAllocs()
		var output []byte
		for range b.N {
			output, err = floatArrayEncodeAllBOS(values, output)
			if err != nil {
				b.Fatal(err)
			}
		}
		b.SetBytes(int64(len(output)))
	})
	b.Run("decode_reference", func(b *testing.B) {
		b.ReportAllocs()
		var output []float64
		for range b.N {
			output, err = referenceFloatArrayDecodeAllBOS(encoded, output)
			if err != nil {
				b.Fatal(err)
			}
		}
		b.SetBytes(int64(len(encoded)))
	})
	b.Run("decode_optimized", func(b *testing.B) {
		b.ReportAllocs()
		var output []float64
		for range b.N {
			output, err = floatArrayDecodeAllBOS(encoded, output)
			if err != nil {
				b.Fatal(err)
			}
		}
		b.SetBytes(int64(len(encoded)))
	})
}

func referenceFloatArrayEncodeAllBOS(src []float64, b []byte) []byte {
	out := b[:0]
	out = append(out, byte(floatCompressedBOS<<4))
	out = appendFloatExperimentalU32(out, uint32(len(src)))
	out = appendFloatExperimentalU32(out, floatExperimentalBlockSize)
	for offset := 0; offset < len(src); offset += floatExperimentalBlockSize {
		length := min(floatExperimentalBlockSize, len(src)-offset)
		out = appendFloatExperimentalU32(out, uint32(length))
		first := int64(math.Float64bits(src[offset]))
		out = appendFloatExperimentalI64(out, first)
		if length == 1 {
			out = appendFloatExperimentalI64(out, 0)
			out = append(out, 0)
			out = appendFloatExperimentalU32(out, 0)
			out = appendFloatExperimentalU32(out, 0)
			out = appendFloatExperimentalU32(out, 0)
			continue
		}
		deltas := make([]int64, length-1)
		previous := uint64(first)
		for i := 1; i < length; i++ {
			current := math.Float64bits(src[offset+i])
			deltas[i-1] = int64(current - previous)
			previous = current
		}
		plan := referenceChooseFloatBOSPlan(deltas)
		bitmap := make([]byte, (len(deltas)+7)/8)
		inliers := make([]uint64, 0, len(deltas)-plan.outlierCount)
		outliers := make([]int64, 0, plan.outlierCount)
		for i, delta := range deltas {
			if delta < plan.lower || delta > plan.upper {
				bitmap[i/8] |= 1 << (i & 7)
				outliers = append(outliers, delta)
			} else {
				inliers = append(inliers, uint64(delta)-uint64(plan.lower))
			}
		}
		packed := referencePackBitsMSB(inliers, plan.width)
		out = appendFloatExperimentalI64(out, plan.lower)
		out = append(out, plan.width)
		out = appendFloatExperimentalU32(out, uint32(len(bitmap)))
		out = appendFloatExperimentalU32(out, uint32(len(packed)))
		out = appendFloatExperimentalU32(out, uint32(len(outliers)))
		out = append(out, bitmap...)
		out = append(out, packed...)
		for _, value := range outliers {
			out = appendFloatExperimentalI64(out, value)
		}
	}
	return out
}

func referenceChooseFloatBOSPlan(deltas []int64) floatBOSPlan {
	sorted := append([]int64(nil), deltas...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
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
		outliers := 0
		for _, value := range deltas {
			if value < lower || value > upper {
				outliers++
			}
		}
		cost := uint64(floatExperimentalPackedByteLen(len(deltas)-outliers, width)) + uint64(outliers)*8
		candidate := floatBOSPlan{lower: lower, upper: upper, width: width, outlierCount: outliers, cost: cost}
		if !haveBest || candidate.cost < best.cost || candidate.cost == best.cost && candidate.outlierCount < best.outlierCount {
			best = candidate
			haveBest = true
		}
	}
	return best
}

func referencePackBitsMSB(values []uint64, width uint8) []byte {
	out := make([]byte, floatExperimentalPackedByteLen(len(values), width))
	bitPosition := 0
	for _, value := range values {
		for bit := int(width) - 1; bit >= 0; bit-- {
			if value&(uint64(1)<<bit) != 0 {
				out[bitPosition/8] |= 1 << (7 - (bitPosition & 7))
			}
			bitPosition++
		}
	}
	return out
}

func referenceFloatArrayDecodeAllBOS(b []byte, dst []float64) ([]float64, error) {
	reader := floatExperimentalReader{data: b}
	header, err := reader.readU8()
	if err != nil || header>>4 != floatCompressedBOS {
		return nil, err
	}
	total32, err := reader.readU32()
	if err != nil {
		return nil, err
	}
	blockSize32, err := reader.readU32()
	if err != nil {
		return nil, err
	}
	total := int(total32)
	if cap(dst) < total {
		dst = make([]float64, 0, total)
	} else {
		dst = dst[:0]
	}
	for len(dst) < total {
		length32, readErr := reader.readU32()
		if readErr != nil || length32 == 0 || length32 > blockSize32 {
			return nil, readErr
		}
		length := int(length32)
		first, _ := reader.readI64()
		lower, _ := reader.readI64()
		width, _ := reader.readU8()
		bitmapLength, _ := reader.readU32()
		packedLength, _ := reader.readU32()
		outlierCount, _ := reader.readU32()
		bitmapView, readErr := reader.readBytes(int(bitmapLength))
		if readErr != nil {
			return nil, readErr
		}
		bitmap := append([]byte(nil), bitmapView...)
		packedView, readErr := reader.readBytes(int(packedLength))
		if readErr != nil {
			return nil, readErr
		}
		packed := append([]byte(nil), packedView...)
		inlierCount := length - 1 - int(outlierCount)
		inliers, readErr := referenceUnpackBitsMSB(packed, inlierCount, width)
		if readErr != nil {
			return nil, readErr
		}
		outliers := make([]int64, int(outlierCount))
		for i := range outliers {
			outliers[i], readErr = reader.readI64()
			if readErr != nil {
				return nil, readErr
			}
		}
		previous := uint64(first)
		dst = append(dst, math.Float64frombits(previous))
		inlierPosition := 0
		outlierPosition := 0
		for i := 0; i < length-1; i++ {
			var delta int64
			if bitmap[i/8]&(1<<(i&7)) != 0 {
				delta = outliers[outlierPosition]
				outlierPosition++
			} else {
				delta = int64(uint64(lower) + inliers[inlierPosition])
				inlierPosition++
			}
			previous += uint64(delta)
			dst = append(dst, math.Float64frombits(previous))
		}
	}
	return dst, nil
}

func referenceUnpackBitsMSB(payload []byte, count int, width uint8) ([]uint64, error) {
	values := make([]uint64, count)
	bitPosition := 0
	for i := range values {
		for range width {
			if bitPosition/8 >= len(payload) {
				return nil, fmt.Errorf("truncated reference payload")
			}
			values[i] = values[i]<<1 | uint64((payload[bitPosition/8]>>(7-(bitPosition&7)))&1)
			bitPosition++
		}
	}
	return values, nil
}
