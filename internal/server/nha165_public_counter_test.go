package server

import (
	"math"
	"testing"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
)

func TestNHA165PublicCounterRejectsUnknownAndUnqualifiedValues(t *testing.T) {
	valid := &api.NumericMeasurementCapability{
		Unit: "{request}", Semantic: api.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING,
		Aggregation: api.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT,
		Observation: &api.NumericObservation{Value: proto.Float64(851), Quality: &api.ObservationQuality{
			Availability: api.Availability_AVAILABILITY_AVAILABLE, Freshness: api.Freshness_FRESHNESS_CURRENT,
			Provenance: api.Provenance_PROVENANCE_OBSERVED,
		}},
	}
	entry := func(measurement *api.NumericMeasurementCapability) *api.EntityViewEntry {
		return &api.EntityViewEntry{Capabilities: []*api.Capability{{Trait: &api.Capability_NumericMeasurement{NumericMeasurement: measurement}}}}
	}
	if sequence, err := nha165PublicSequence(entry(valid)); err != nil || sequence != nha165FirstSequence {
		t.Fatal("qualified synthetic counter was lost", sequence, err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*api.NumericMeasurementCapability)
	}{
		{"absent", func(v *api.NumericMeasurementCapability) { v.Observation.Value = nil }},
		{"zero", func(v *api.NumericMeasurementCapability) { v.Observation.Value = proto.Float64(0) }},
		{"fraction", func(v *api.NumericMeasurementCapability) { v.Observation.Value = proto.Float64(851.5) }},
		{"infinite", func(v *api.NumericMeasurementCapability) { v.Observation.Value = proto.Float64(math.Inf(1)) }},
		{"unknown", func(v *api.NumericMeasurementCapability) { v.Observation.Value = proto.Float64(math.NaN()) }},
		{"unit", func(v *api.NumericMeasurementCapability) { v.Unit = "s" }},
		{"semantic", func(v *api.NumericMeasurementCapability) { v.Semantic = api.MeasurementSemantic(999) }},
		{"stale", func(v *api.NumericMeasurementCapability) {
			v.Observation.Quality.Freshness = api.Freshness_FRESHNESS_STALE
		}},
		{"unavailable", func(v *api.NumericMeasurementCapability) {
			v.Observation.Quality.Availability = api.Availability_AVAILABILITY_UNAVAILABLE
		}},
		{"estimated", func(v *api.NumericMeasurementCapability) {
			v.Observation.Quality.Provenance = api.Provenance_PROVENANCE_ESTIMATED
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := proto.Clone(valid).(*api.NumericMeasurementCapability)
			test.mutate(value)
			if _, err := nha165PublicSequence(entry(value)); err == nil {
				t.Fatal("unqualified counter admitted")
			}
		})
	}
	duplicate := entry(valid)
	duplicate.Capabilities = append(duplicate.Capabilities, proto.Clone(duplicate.Capabilities[0]).(*api.Capability))
	if _, err := nha165PublicSequence(duplicate); err == nil {
		t.Fatal("ambiguous counter slot admitted")
	}
}
