package storage

import (
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"math"
	"strings"
	"testing"
)

func TestNHA203HistoricalNumericDescriptorPreservesPersistedInterpretation(t *testing.T) {
	valid := recordermaintenance.NumericInput{Unit: strings.Repeat("x", 300), CanonicalUnit: "W", Scale: 1, Precision: -2, Dimension: domain.DimensionPower, Semantic: domain.SemanticMeasurement, Aggregation: domain.AggregationCurrent}
	if !historicalNumericDescriptorValid(valid) {
		t.Fatal("historical long symbol or negative precision narrowed")
	}
	for _, mutate := range []func(*recordermaintenance.NumericInput){
		func(v *recordermaintenance.NumericInput) { v.Unit = strings.Repeat("x", historyquery.MaxPointBytes+1) },
		func(v *recordermaintenance.NumericInput) { v.Unit = "a b" },
		func(v *recordermaintenance.NumericInput) { v.Unit = string([]byte{255}) },
		func(v *recordermaintenance.NumericInput) { v.Scale = math.NaN() },
		func(v *recordermaintenance.NumericInput) { v.Offset = math.Inf(1) },
		func(v *recordermaintenance.NumericInput) { v.Scale = 0 },
		func(v *recordermaintenance.NumericInput) { v.Precision = -309 },
		func(v *recordermaintenance.NumericInput) { v.Dimension = domain.Dimension(255) },
		func(v *recordermaintenance.NumericInput) { v.Semantic = domain.SemanticClass(255) },
		func(v *recordermaintenance.NumericInput) { v.Aggregation = domain.AggregationKind(255) },
	} {
		changed := valid
		mutate(&changed)
		if historicalNumericDescriptorValid(changed) {
			t.Fatalf("invalid persisted interpretation accepted: %+v", changed)
		}
	}
}
