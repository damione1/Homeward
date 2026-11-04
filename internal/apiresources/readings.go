package apiresources

import (
	"math"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"google.golang.org/protobuf/proto"
)

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func numericCapability(unit string, role api.MeasurementRole, limits *registry.NumberBounds, raw *state.ObservedState, number *float64, decimal *string) (*api.NumericMeasurementCapability, error) {
	b, err := bounds(limits)
	if err != nil || unit == "" || number != nil && !finite(*number) {
		return nil, ErrProjection
	}
	result := &api.NumericMeasurementCapability{Unit: unit, Role: role, Bounds: b,
		Semantic: api.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: api.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		result.Observation = &api.NumericObservation{Value: number, DecimalValue: decimal, Quality: q, ObservedTime: proto.CloneOf(raw.ObservedAt)}
	}
	return result, nil
}

func projectMeasurement(c *registry.CapabilityDescription_Measurement, raw *state.ObservedState) ([]Capability, error) {
	if c == nil || c.Measurement == nil {
		return nil, ErrProjection
	}
	definition := c.Measurement
	var value *float64
	var decimal *string
	if measured := raw.GetMeasurement(); measured != nil {
		if measured.Unit != definition.Unit {
			return nil, ErrProjection
		}
		value, decimal = &measured.Value, measured.DecimalValue
	}
	measurement, err := numericCapability(definition.Unit, api.MeasurementRole_MEASUREMENT_ROLE_GENERIC, definition.Value, raw, value, decimal)
	if err != nil {
		return nil, err
	}
	measurement.Semantic, err = mapped(definition.Semantic, measurementSemantics)
	if err != nil {
		return nil, err
	}
	measurement.Aggregation, err = mapped(definition.Aggregation, measurementAggregations)
	if err != nil {
		return nil, err
	}
	if definition.Precision > math.MaxInt32 {
		return nil, ErrProjection
	}
	measurement.Precision = int32(definition.Precision)
	return []Capability{{trait: Measurement, value: &api.Capability{Trait: &api.Capability_NumericMeasurement{NumericMeasurement: measurement}}}}, nil
}

func projectBinary(c *registry.CapabilityDescription_Binary, raw *state.ObservedState) ([]Capability, error) {
	if c == nil || c.Binary == nil {
		return nil, ErrProjection
	}
	result := &api.BinaryObservationCapability{}
	if c.Binary.DeviceClass != nil {
		class := *c.Binary.DeviceClass
		result.DeviceClass = &class
	}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		observation := &api.BinaryObservation{Quality: q, ObservedTime: proto.CloneOf(raw.ObservedAt)}
		if value, ok := raw.Value.(*state.ObservedState_SwitchOn); ok {
			b := value.SwitchOn
			observation.Value = &b
		}
		result.Observation = observation
	}
	return []Capability{{trait: Binary, value: &api.Capability{Trait: &api.Capability_BinaryObservation{BinaryObservation: result}}}}, nil
}
