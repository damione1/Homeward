package contracts

import (
	"bytes"
	"errors"
	"math"
	"os"
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

func nha174Pointer[T any](value T) *T { return &value }

func nha174Description() *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: ClimateSchema, Major: 1, Endpoint: "climate", DisplayName: "Synthetic climate",
		Constraints: &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{
			ObservedTemperatureCelsius: &registry.ClimateTemperatureObservation{},
			ObservedSetpointCelsius:    &registry.ClimateTemperatureObservation{},
			ObservedSetpointRangeCelsius: &registry.ClimateSetpointRangeConstraints{
				LowCelsius:          &registry.NumberBounds{Minimum: 5, Maximum: 30, Step: nha174Pointer(0.5)},
				HighCelsius:         &registry.NumberBounds{Minimum: 5, Maximum: 30, Step: nha174Pointer(0.5)},
				MinimumDeltaCelsius: nha174Pointer(1.0), MaximumDeltaCelsius: nha174Pointer(10.0)},
			ObservedModeOptions: []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_OFF, registry.ClimateMode_CLIMATE_MODE_HEAT, registry.ClimateMode_CLIMATE_MODE_AUTO},
			ActivityOptions:     []registry.ClimateActivity{registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE, registry.ClimateActivity_CLIMATE_ACTIVITY_HEATING},
			ObservesFanActivity: true,
		}}}
}

func nha174Catalog(t *testing.T, description *registry.CapabilityDescription) (*CapabilityCatalog, domain.EntityID) {
	t.Helper()
	catalog, err := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	entity := testNHA014Entity(t)
	if err := catalog.RegisterDriverCapability(entity, description); err != nil {
		t.Fatal(err)
	}
	return catalog, entity
}

func nha174AdmitValue(t *testing.T, value *state.ClimateValue) domain.ClimateValue {
	t.Helper()
	admitted, err := AdmitWireClimateValue(value)
	if err != nil {
		t.Fatal(err)
	}
	return admitted
}

func TestNHA174ClimateWireRejectsMalformedPresenceAndUnknownEnums(t *testing.T) {
	for name, value := range map[string]*state.ClimateValue{
		"nil":                   nil,
		"empty":                 {},
		"NaN":                   {CurrentTemperatureCelsius: nha174Pointer(math.NaN())},
		"infinity":              {CurrentTemperatureCelsius: nha174Pointer(math.Inf(1))},
		"below-absolute-zero":   {CurrentTemperatureCelsius: nha174Pointer(-274.0)},
		"zero-mode-present":     {Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_UNSPECIFIED)},
		"future-mode":           {Mode: nha174Pointer(registry.ClimateMode(99))},
		"zero-activity-present": {Activity: nha174Pointer(registry.ClimateActivity_CLIMATE_ACTIVITY_UNSPECIFIED)},
		"future-activity":       {Activity: nha174Pointer(registry.ClimateActivity(99))},
		"typed-nil-single":      {Setpoint: (*state.ClimateValue_SetpointCelsius)(nil)},
		"typed-nil-range":       {Setpoint: (*state.ClimateValue_SetpointRangeCelsius)(nil)},
		"nil-range":             {Setpoint: &state.ClimateValue_SetpointRangeCelsius{}},
		"missing-low":           {Setpoint: &state.ClimateValue_SetpointRangeCelsius{SetpointRangeCelsius: &state.ClimateSetpointRange{HighCelsius: nha174Pointer(20.0)}}},
		"missing-high":          {Setpoint: &state.ClimateValue_SetpointRangeCelsius{SetpointRangeCelsius: &state.ClimateSetpointRange{LowCelsius: nha174Pointer(20.0)}}},
		"inverted-range":        {Setpoint: &state.ClimateValue_SetpointRangeCelsius{SetpointRangeCelsius: &state.ClimateSetpointRange{LowCelsius: nha174Pointer(23.0), HighCelsius: nha174Pointer(18.0)}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := AdmitWireClimateValue(value); !errors.Is(err, ErrInvalidWireState) {
				t.Fatalf("malformed climate admitted: %v", err)
			}
		})
	}
	unknown := &state.ClimateValue{CurrentTemperatureCelsius: nha174Pointer(0.0)}
	unknown.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x07})
	encoded, err := proto.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(state.ClimateValue)
	if err := proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	if _, err := AdmitWireClimateValue(decoded); !errors.Is(err, ErrInvalidWireState) {
		t.Fatal("unknown field became operational")
	}
	if roundtrip, err := proto.Marshal(decoded); err != nil || !bytes.Equal(roundtrip, encoded) {
		t.Fatal("unknown field lost wire compatibility")
	}
	values := registry.ClimateMode_CLIMATE_MODE_UNSPECIFIED.Descriptor().Values()
	for index := 1; index < values.Len(); index++ {
		wire := registry.ClimateMode(values.Get(index).Number())
		mode, err := AdmitWireClimateMode(wire)
		if err != nil {
			t.Fatalf("unhandled registry mode: %v", wire)
		}
		if encoded, err := WireClimateMode(mode); err != nil || encoded != wire {
			t.Fatal("mode conversion differs")
		}
	}
	activities := registry.ClimateActivity_CLIMATE_ACTIVITY_UNSPECIFIED.Descriptor().Values()
	for index := 1; index < activities.Len(); index++ {
		wire := registry.ClimateActivity(activities.Get(index).Number())
		activity, err := AdmitWireClimateActivity(wire)
		if err != nil {
			t.Fatalf("unhandled registry activity: %v", wire)
		}
		if encoded, err := WireClimateActivity(activity); err != nil || encoded != wire {
			t.Fatal("activity conversion differs")
		}
	}
	if _, err := WireClimateMode(domain.ClimateMode(255)); err == nil {
		t.Fatal("future domain mode encoded")
	}
	if _, err := WireClimateActivity(domain.ClimateActivity(255)); err == nil {
		t.Fatal("future domain activity encoded")
	}
}

func TestNHA174ClimateObservationCapabilitiesDoNotAuthorizeActions(t *testing.T) {
	description := nha174Description()
	catalog, entity := nha174Catalog(t, description)
	if err := catalog.AdmitReadOnlyDriverCapability(description); err != nil {
		t.Fatalf("read-only observations refused: %v", err)
	}
	observation := nha174AdmitValue(t, &state.ClimateValue{CurrentTemperatureCelsius: nha174Pointer(0.0), Setpoint: &state.ClimateValue_SetpointCelsius{SetpointCelsius: 0}, Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_HEAT), FanActive: nha174Pointer(false)})
	if err := catalog.AdmitDomainClimate(entity, "climate", observation); err != nil {
		t.Fatalf("unknown device bounds must not mean unsupported: %v", err)
	}
	for _, command := range []*actions.CommandRequest{
		testNHA014Command(ClimateSchema, "climate", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_HEAT)}}}),
		testNHA014Command(ClimateSchema, "climate", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: nha174Pointer(21.0)}}}),
	} {
		if err := catalog.AdmitCapabilityCommand(command, "epoch-a", 1); err == nil {
			t.Fatal("observation declaration enabled a climate action")
		}
	}
	description.GetClimate().ObservedTemperatureCelsius = nil
	description.GetClimate().ObservedModeOptions[0] = registry.ClimateMode(99)
	if err := catalog.AdmitDomainClimate(entity, "climate", observation); err != nil {
		t.Fatal("caller changed admitted catalogue")
	}
	if err := catalog.AdmitDomainClimate(entity, "other", observation); !errors.Is(err, ErrUnsupportedClimateObservation) {
		t.Fatalf("wrong endpoint accepted: %v", err)
	}
	other, err := domain.ParseEntityID("Host:unknown-climate")
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.AdmitDomainClimate(other, "climate", observation); !errors.Is(err, ErrUnsupportedClimateObservation) {
		t.Fatalf("wrong identity accepted: %v", err)
	}
	commandOnly := nha174Description()
	commandOnly.GetClimate().ObservedTemperatureCelsius = nil
	commandOnly.GetClimate().SetpointCelsius = &registry.NumberBounds{Minimum: 5, Maximum: 30}
	commandOnly.GetClimate().ModeOptions = []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_HEAT}
	commandOnly.GetClimate().ObservedModeOptions = nil
	commandCatalog, commandEntity := nha174Catalog(t, commandOnly)
	if err := commandCatalog.AdmitDomainClimate(commandEntity, "climate", observation); !errors.Is(err, ErrUnsupportedClimateObservation) {
		t.Fatalf("command authorization implied observation: %v", err)
	}
}

func TestNHA174ClimateRangeAppliesStepsAndDeltasToObservedValues(t *testing.T) {
	catalog, entity := nha174Catalog(t, nha174Description())
	for _, test := range []struct {
		low, high float64
		accepted  bool
	}{
		{18, 23, true}, {18, 19, true}, {18, 28, true},
		{18.25, 23, false}, {18, 23.25, false}, {18, 18.5, false}, {18, 28.5, false}, {4.5, 20, false}, {20, 30.5, false},
	} {
		value := nha174AdmitValue(t, &state.ClimateValue{Setpoint: &state.ClimateValue_SetpointRangeCelsius{SetpointRangeCelsius: &state.ClimateSetpointRange{LowCelsius: &test.low, HighCelsius: &test.high}}})
		err := catalog.AdmitDomainClimate(entity, "climate", value)
		if test.accepted && err != nil {
			t.Fatalf("valid step/delta range %v..%v: %v", test.low, test.high, err)
		}
		if !test.accepted && !errors.Is(err, ErrClimateObservationOutsideConstraints) {
			t.Fatalf("invalid step/delta range %v..%v: %v", test.low, test.high, err)
		}
	}
	for name, change := range map[string]func(*registry.ClimateConstraints){
		"negative-delta": func(c *registry.ClimateConstraints) {
			c.ObservedSetpointRangeCelsius.MinimumDeltaCelsius = nha174Pointer(-1.0)
		},
		"nonfinite-delta": func(c *registry.ClimateConstraints) {
			c.ObservedSetpointRangeCelsius.MaximumDeltaCelsius = nha174Pointer(math.Inf(1))
		},
		"inverted-deltas": func(c *registry.ClimateConstraints) {
			c.ObservedSetpointRangeCelsius.MinimumDeltaCelsius = nha174Pointer(11.0)
		},
		"disjoint-intervals": func(c *registry.ClimateConstraints) {
			c.ObservedSetpointRangeCelsius.LowCelsius.Minimum = 30
			c.ObservedSetpointRangeCelsius.HighCelsius.Maximum = 20
		},
		"infinite-step": func(c *registry.ClimateConstraints) {
			c.ObservedSetpointRangeCelsius.LowCelsius.Step = nha174Pointer(math.Inf(1))
		},
		"unphysical-bound": func(c *registry.ClimateConstraints) {
			c.ObservedTemperatureCelsius.Bounds = &registry.NumberBounds{Minimum: -274, Maximum: 40}
		},
		"future-mode":    func(c *registry.ClimateConstraints) { c.ObservedModeOptions = []registry.ClimateMode{99} },
		"duplicate-mode": func(c *registry.ClimateConstraints) { c.ObservedModeOptions = []registry.ClimateMode{1, 1} },
	} {
		t.Run(name, func(t *testing.T) {
			description := nha174Description()
			change(description.GetClimate())
			if err := catalog.AdmitDriverCapability(description); !errors.Is(err, ErrInvalidCapability) {
				t.Fatalf("invalid declaration admitted: %v", err)
			}
		})
	}
	unknownBounds := nha174Description()
	unknownBounds.GetClimate().ObservedSetpointRangeCelsius = &registry.ClimateSetpointRangeConstraints{}
	if err := catalog.AdmitDriverCapability(unknownBounds); err != nil {
		t.Fatal("unknown range limits required invented bounds")
	}
	// This declaration has continuously compatible intervals but its discrete
	// steps cannot achieve delta 0.5. Admission, not a speculative grid solver,
	// refuses each actual pair that violates either the steps or the delta.
	discrete := nha174Description()
	discrete.GetClimate().ObservedSetpointRangeCelsius = &registry.ClimateSetpointRangeConstraints{LowCelsius: &registry.NumberBounds{Minimum: 18, Maximum: 20, Step: nha174Pointer(1.0)}, HighCelsius: &registry.NumberBounds{Minimum: 18, Maximum: 20, Step: nha174Pointer(1.0)}, MinimumDeltaCelsius: nha174Pointer(0.5), MaximumDeltaCelsius: nha174Pointer(0.5)}
	discreteCatalog, discreteEntity := nha174Catalog(t, discrete)
	for _, pair := range [][2]float64{{18, 18.5}, {18, 19}} {
		value := nha174AdmitValue(t, &state.ClimateValue{Setpoint: &state.ClimateValue_SetpointRangeCelsius{SetpointRangeCelsius: &state.ClimateSetpointRange{LowCelsius: &pair[0], HighCelsius: &pair[1]}}})
		if err := discreteCatalog.AdmitDomainClimate(discreteEntity, "climate", value); !errors.Is(err, ErrClimateObservationOutsideConstraints) {
			t.Fatal("incompatible actual step/delta pair admitted")
		}
	}
}

func TestNHA174ClimateSnapshotsAndCrossLanguageFixtures(t *testing.T) {
	catalog, _ := nha174Catalog(t, nha174Description())
	admission := &StateCapabilityAdmission{Catalog: catalog, Endpoint: "climate"}
	for _, name := range []string{"climate-range", "climate-off"} {
		data, err := os.ReadFile("climate_golden/" + name + ".bin")
		if err != nil {
			t.Fatal(err)
		}
		text, err := os.ReadFile("climate_golden/" + name + ".textproto")
		if err != nil {
			t.Fatal(err)
		}
		wire := new(state.ObservedState)
		if err := proto.Unmarshal(data, wire); err != nil {
			t.Fatal(err)
		}
		fromText := new(state.ObservedState)
		if err := prototext.Unmarshal(text, fromText); err != nil || !proto.Equal(wire, fromText) {
			t.Fatalf("fixture source differs: %v", err)
		}
		observed, err := AdmitWireState(wire, nil, admission)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := EncodeObservedState(observed)
		if err != nil || !proto.Equal(encoded, wire) {
			t.Fatalf("snapshot presence or clocks changed: %v", err)
		}
		if _, err := AdmitWireState(wire, nil, nil); err == nil {
			t.Fatal("climate snapshot bypassed catalogue context")
		}
		typedNil := proto.Clone(wire).(*state.ObservedState)
		typedNil.Value = (*state.ObservedState_Climate)(nil)
		if _, err := AdmitWireState(typedNil, nil, admission); err == nil {
			t.Fatal("typed nil snapshot admitted")
		}
		for _, availability := range []state.Availability{state.Availability_AVAILABILITY_UNKNOWN, state.Availability_AVAILABILITY_UNAVAILABLE} {
			missing := proto.Clone(wire).(*state.ObservedState)
			missing.Quality.Availability, missing.Value = availability, nil
			missing.Revision++
			restored, err := AdmitWireState(missing, &observed, admission)
			if err != nil {
				t.Fatal(err)
			}
			if _, present := restored.CurrentValue(); present {
				t.Fatal("unavailable snapshot invented current value")
			}
			if _, present := restored.DisplayValue(); !present {
				t.Fatal("last-known snapshot lost")
			}
			if encoded, err := EncodeObservedState(restored); err != nil || encoded.Value != nil {
				t.Fatal("last-known leaked into current wire value")
			}
			withoutPrevious, err := AdmitWireState(missing, nil, admission)
			if err != nil {
				t.Fatal(err)
			}
			if _, present := withoutPrevious.DisplayValue(); present {
				t.Fatal("wire invented last-known display without previous snapshot")
			}
		}
	}
}
