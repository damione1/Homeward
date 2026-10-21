package observability

import "math"

// NativeOrdinal observes admitted metadata without cloning a row or any
// protobuf while the producer merges its two bounded native cuts.
func (r TraceRow) NativeOrdinal() (uint64, bool) {
	if r.wire == nil {
		return 0, false
	}
	value := r.wire.GetOrdinal().GetNative()
	return value, value > 0 && value < math.MaxUint64
}
