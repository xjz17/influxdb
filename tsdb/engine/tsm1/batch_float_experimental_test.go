package tsm1_test

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"

	"github.com/influxdata/influxdb/v2/tsdb"
	"github.com/influxdata/influxdb/v2/tsdb/engine/tsm1"
)

func TestFloatArrayExperimentalRoundTrip(t *testing.T) {
	randomBits := make([]float64, 1025)
	state := uint64(0x123456789abcdef0)
	for i := range randomBits {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		randomBits[i] = math.Float64frombits(state)
	}
	cases := [][]float64{
		{},
		{42},
		{17, 17, 17, 17, 17},
		{-0.0, 0.0, 1.25, math.NaN(), math.Float64frombits(0x7ff8000000000001), math.Inf(-1)},
		randomBits,
	}
	for _, encoding := range []tsm1.FloatArrayEncoding{
		tsm1.FloatArrayEncodingBOS,
		tsm1.FloatArrayEncodingSubcolumn,
	} {
		for _, values := range cases {
			payload, err := tsm1.FloatArrayEncodeAllWithEncoding(values, nil, encoding)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := tsm1.FloatArrayDecodeAll(payload, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(decoded) != len(values) {
				t.Fatalf("length mismatch: got %d, want %d", len(decoded), len(values))
			}
			for i := range values {
				if math.Float64bits(decoded[i]) != math.Float64bits(values[i]) {
					t.Fatalf("bit mismatch at %d: got %016x, want %016x", i, math.Float64bits(decoded[i]), math.Float64bits(values[i]))
				}
			}
		}
	}
}

func TestFloatArrayExperimentalRejectsTruncation(t *testing.T) {
	values := make([]float64, 1000)
	for i := range values {
		values[i] = float64(i * i)
	}
	for _, encoding := range []tsm1.FloatArrayEncoding{
		tsm1.FloatArrayEncodingBOS,
		tsm1.FloatArrayEncodingSubcolumn,
	} {
		payload, err := tsm1.FloatArrayEncodeAllWithEncoding(values, nil, encoding)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tsm1.FloatArrayDecodeAll(payload[:len(payload)-1], nil); err == nil {
			t.Fatalf("encoding %d accepted a truncated payload", encoding)
		}
	}
}

func TestFloatArrayExperimentalPhysicalBlockRoundTrip(t *testing.T) {
	values := []float64{-0.0, 0.0, 1.25, math.NaN(), math.Float64frombits(0x7ff8000000000001), math.Inf(-1)}
	for _, encoding := range []tsm1.FloatArrayEncoding{
		tsm1.FloatArrayEncodingBOS,
		tsm1.FloatArrayEncodingSubcolumn,
	} {
		timestamps := make([]int64, len(values))
		for i := range timestamps {
			timestamps[i] = int64(i)
		}
		array := &tsdb.FloatArray{Timestamps: timestamps, Values: append([]float64(nil), values...)}
		block, err := tsm1.EncodeFloatArrayBlockWithEncoding(array, nil, encoding)
		if err != nil {
			t.Fatal(err)
		}
		var batch tsdb.FloatArray
		if err = tsm1.DecodeFloatArrayBlock(block, &batch); err != nil {
			t.Fatal(err)
		}
		var legacy []tsm1.FloatValue
		legacy, err = tsm1.DecodeFloatBlock(block, &legacy)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Values) != len(values) || len(legacy) != len(values) {
			t.Fatalf("decoded length mismatch")
		}
		for i := range values {
			want := math.Float64bits(values[i])
			if math.Float64bits(batch.Values[i]) != want || math.Float64bits(legacy[i].RawValue()) != want {
				t.Fatalf("block value mismatch at %d", i)
			}
			if batch.Timestamps[i] != int64(i) || legacy[i].UnixNano() != int64(i) {
				t.Fatalf("block timestamp mismatch at %d", i)
			}
		}
	}
}

func TestFloatArrayBOSWireFormat(t *testing.T) {
	values := make([]float64, 1025)
	state := uint64(0x123456789abcdef0)
	for i := range values {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		values[i] = math.Float64frombits(state)
	}
	payload, err := tsm1.FloatArrayEncodeAllWithEncoding(values, nil, tsm1.FloatArrayEncodingBOS)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if len(payload) != 8412 {
		t.Fatalf("BOS payload length changed: got %d, want 8412", len(payload))
	}
	if got, want := hex.EncodeToString(digest[:]), "7db5ffc1cc860ca1298ee828e870a5cb228a304baa20b6f67d5e2d4913ab538b"; got != want {
		t.Fatalf("BOS payload hash changed: got %s, want %s", got, want)
	}
}
