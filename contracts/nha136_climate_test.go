package contracts

import (
	"math"
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func nha136Ptr[T any](v T) *T { return &v }
func nha136Capability() *registry.CapabilityDescription {
	limits := &registry.ClimateSetpointRangeConstraints{LowCelsius: &registry.NumberBounds{Minimum: 7, Maximum: 35, Step: nha136Ptr(0.5)}, HighCelsius: &registry.NumberBounds{Minimum: 7, Maximum: 35, Step: nha136Ptr(0.5)}}
	return &registry.CapabilityDescription{SchemaId: ClimateSchema, Major: 1, Endpoint: "climate", DisplayName: "Synthetic ECB601", Constraints: &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{SetpointRangeCelsius: limits, FanTargetOptions: []registry.ClimateFanTarget{registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON, registry.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO}, ObservedHumidityPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100}, ObservedFanTargetOptions: []registry.ClimateFanTarget{registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON, registry.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO}}}}
}
func TestNHA136ClimateAdditionsPreservePresenceAndAdmission(t *testing.T) {
	absent := &state.ClimateValue{HumidityPercent: nha136Ptr(0.0), FanTarget: nha136Ptr(registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON)}
	value, err := AdmitWireClimateValue(absent)
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := EncodeClimateValue(value)
	if err != nil || !proto.Equal(absent, roundtrip) {
		t.Fatal("presence lost", err)
	}
	for _, number := range []float64{-1, 101, math.Inf(1), math.NaN()} {
		if _, err := AdmitWireClimateValue(&state.ClimateValue{HumidityPercent: &number}); err == nil {
			t.Fatal("invalid humidity admitted")
		}
	}
	for _, target := range []registry.ClimateFanTarget{registry.ClimateFanTarget_CLIMATE_FAN_TARGET_UNSPECIFIED, registry.ClimateFanTarget(99)} {
		if _, err := AdmitWireClimateValue(&state.ClimateValue{FanTarget: &target}); err == nil {
			t.Fatal("unknown fan target admitted")
		}
	}
	var zero domain.ClimateFanTarget
	if _, err := zero.Automatic(); err == nil {
		t.Fatal("zero fan target admitted")
	}
	var humidity domain.RelativeHumidity
	if _, err := humidity.Value(); err == nil {
		t.Fatal("zero humidity wrapper admitted")
	}
	catalog, entity := nha174Catalog(t, nha136Capability())
	if err := catalog.AdmitDomainClimate(entity, "climate", value); err != nil {
		t.Fatal(err)
	}
	description := nha136Capability()
	if err := catalog.AdmitReadOnlyDriverCapability(description); err == nil {
		t.Fatal("new command paths became readonly")
	}
	description.GetClimate().FanTargetOptions = nil
	description.GetClimate().SetpointRangeCelsius = nil
	if err := catalog.AdmitReadOnlyDriverCapability(description); err != nil {
		t.Fatal(err)
	}
}
func TestNHA136ThresholdAndFanCommandsAreClosed(t *testing.T) {
	description := nha136Capability()
	catalog, entity := nha174Catalog(t, description)
	id, _ := entity.Value()
	for _, test := range []struct {
		name  string
		arg   *actions.SetClimate
		valid bool
	}{
		{"range", &actions.SetClimate{HeatThresholdCelsius: nha136Ptr(20.5), CoolThresholdCelsius: nha136Ptr(24.5)}, true},
		{"zero-on", &actions.SetClimate{FanTarget: nha136Ptr(registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON)}, true},
		{"one-threshold", &actions.SetClimate{HeatThresholdCelsius: nha136Ptr(20.5)}, false},
		{"reverse", &actions.SetClimate{HeatThresholdCelsius: nha136Ptr(25.0), CoolThresholdCelsius: nha136Ptr(20.0)}, false},
		{"step", &actions.SetClimate{HeatThresholdCelsius: nha136Ptr(20.25), CoolThresholdCelsius: nha136Ptr(24.5)}, false},
		{"mixed", &actions.SetClimate{SetpointCelsius: nha136Ptr(22.0), HeatThresholdCelsius: nha136Ptr(20.5), CoolThresholdCelsius: nha136Ptr(24.5)}, false},
		{"future-fan", &actions.SetClimate{FanTarget: nha136Ptr(registry.ClimateFanTarget(99))}, false},
		{"empty", &actions.SetClimate{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := testNHA014Command(ClimateSchema, "climate", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: test.arg}})
			request.Target = &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: id}}
			err := catalog.AdmitCapabilityCommand(request, request.AdmissionEpoch, request.ConfigRevision)
			if (err == nil) != test.valid {
				t.Fatalf("admission=%v wantvalid=%v", err, test.valid)
			}
		})
	}
}
