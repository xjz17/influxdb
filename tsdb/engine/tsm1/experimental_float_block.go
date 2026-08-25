package tsm1

import "github.com/influxdata/influxdb/v2/tsdb"

// EncodeFloatArrayBlockWithEncoding writes an InfluxDB TSM float block using
// the requested value encoding and the native adaptive timestamp encoding.
func EncodeFloatArrayBlockWithEncoding(a *tsdb.FloatArray, b []byte, encoding FloatArrayEncoding) ([]byte, error) {
	if a.Len() == 0 {
		return nil, nil
	}

	vb, err := FloatArrayEncodeAllWithEncoding(a.Values, nil, encoding)
	if err != nil {
		return nil, err
	}
	tb, err := TimeArrayEncodeAll(a.Timestamps, nil)
	if err != nil {
		return nil, err
	}
	return packBlock(b, BlockFloat64, tb, vb), nil
}
