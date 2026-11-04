package server

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	native "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"math"
	"testing"
)

func TestObservationNumericProjectionPreservesFullWidthAndSaturation(t *testing.T) {
	p := &observationProjection{}
	for _, n := range []uint64{0, math.MaxInt64, uint64(math.MaxInt64) + 1, math.MaxUint64} {
		position := p.projectAdmissionPosition(&native.AdmissionPosition{Sequence: n})
		admitted, err := apitypes.ParseDecimalUint64(position.Sequence)
		if err != nil || admitted.Value() != n {
			t.Fatal("position truncated", n, err)
		}
		counter := p.projectCounterMeasurement(&native.CounterMeasurement{Result: &native.CounterMeasurement_Exact{Exact: n}})
		if n <= math.MaxInt64 {
			exact, ok := counter.Result.(*api.ObservationCounterMeasurement_Exact)
			if !ok || exact.Exact != int64(n) {
				t.Fatal("exact counter changed", n)
			}
		} else {
			saturated, ok := counter.Result.(*api.ObservationCounterMeasurement_Saturated)
			if !ok || saturated.Saturated.LowerBound != math.MaxInt64 {
				t.Fatal("overflow represented as exact", n)
			}
		}
	}
	lost := p.projectKnownLost(&native.KnownLost{Result: &native.KnownLost_LowerBound{LowerBound: math.MaxUint64}})
	if lost.GetLowerBound() != math.MaxInt64 || p.err != nil {
		t.Fatal("lower bound projection", p.err)
	}
}
