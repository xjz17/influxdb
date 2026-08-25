# Experimental BOS and Sub-column storage codecs

This branch adds lossless `BOS` and `Subcolumn` codecs to `influxdb3_write` at the nullable Arrow numeric-array boundary. The supported types are `Int32`, `Int64`, `UInt64`, `Float32`, `Float64`, and nanosecond timestamps. Validity bits and all IEEE floating-point bit patterns are preserved.

The algorithms intentionally do not claim new native Parquet encoding ids. Parquet has a fixed encoding enum, so writing an unregistered id would create files that normal InfluxDB and Arrow readers cannot open. The experimental storage path therefore wraps encoded Arrow arrays in a versioned container; the production comparison remains the normal InfluxDB Parquet writer.

`REGER` is not included. It requires timestamp plus multi-column joint modeling, which does not fit either the per-array codec boundary or Parquet's per-column pages without a new table-level format and query-reader changes.

## Verification and benchmark

The module tests cover random and extreme `i64` values, truncated input, nullable arrays, NaNs, infinities, and negative zero.

The `system_codec_bench` example reads the shared `SCDBIN01` input and measures encode plus durable file write (`sync_all`) and file read plus decode. It compares BOS and Sub-column with the production ZSTD Parquet writer plus Snappy and uncompressed Parquet. Every output is bit-checked before `hash_ok=true` is emitted.
