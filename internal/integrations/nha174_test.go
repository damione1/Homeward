package integrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

func nha174Pointer[T any](value T) *T { return &value }

func nha174ClimateRig(t *testing.T, driver *nha025Driver) (nha025Rig, integrations.Candidate) {
	t.Helper()
	rig := nha025Setup(t, driver, nil)
	_, key, err := rig.binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic thermostat",
		Constraints: &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{
			SetpointCelsius:            &registry.NumberBounds{Minimum: 5, Maximum: 30, Step: nha174Pointer(0.5)},
			ModeOptions:                []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_OFF, registry.ClimateMode_CLIMATE_MODE_HEAT},
			ObservedTemperatureCelsius: &registry.ClimateTemperatureObservation{Bounds: &registry.NumberBounds{Minimum: -20, Maximum: 50}},
			ObservedSetpointCelsius:    &registry.ClimateTemperatureObservation{Bounds: &registry.NumberBounds{Minimum: 5, Maximum: 30, Step: nha174Pointer(0.5)}},
			ObservedModeOptions:        []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_OFF, registry.ClimateMode_CLIMATE_MODE_HEAT},
			ActivityOptions:            []registry.ClimateActivity{registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE, registry.ClimateActivity_CLIMATE_ACTIVITY_HEATING},
		}}}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("climate discovery: %v", err)
	}
	rig.binding = bindings[0]
	return rig, candidate
}

func nha174Publish(t *testing.T, rig nha025Rig, wire *state.ClimateValue, cause *domain.Causality) error {
	t.Helper()
	climate, err := contracts.AdmitWireClimateValue(wire)
	if err != nil {
		return err
	}
	value, err := domain.NewClimateStateValue(climate)
	if err != nil {
		return err
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return err
	}
	at := time.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		return err
	}
	if cause != nil {
		observation, err = observation.WithCausality(*cause)
		if err != nil {
			return err
		}
	}
	return rig.session.Publish(context.Background(), rig.binding, observation)
}

func nha174FullClimate() *state.ClimateValue {
	return &state.ClimateValue{CurrentTemperatureCelsius: nha174Pointer(0.0), Setpoint: &state.ClimateValue_SetpointCelsius{SetpointCelsius: 21}, Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_HEAT), Activity: nha174Pointer(registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE)}
}

func TestNHA174TrueHostPublishesOneCoherentClimateAndKeepsEffectsClosed(t *testing.T) {
	driver := &nha025Driver{}
	rig, candidate := nha174ClimateRig(t, driver)
	before, err := rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	if err := nha174Publish(t, rig, nha174FullClimate(), nil); err != nil {
		t.Fatal(err)
	}
	after, err := rig.fixture.Sequencer().Capture()
	if err != nil {
		t.Fatal(err)
	}
	_, beforeSequence, _ := before.Cursor.Components()
	_, afterSequence, _ := after.Cursor.Components()
	_, entity, err := rig.binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.States) != 1 || afterSequence != beforeSequence+1 {
		t.Fatal("climate fields were published separately")
	}
	observed, present := after.States[entity]
	if !present {
		t.Fatal("original entity identity missing")
	}
	value, present := observed.CurrentValue()
	if !present {
		t.Fatal("current climate missing")
	}
	climate, ok := value.Climate()
	if !ok {
		t.Fatal("wrong state variant")
	}
	parts, err := climate.Components()
	if err != nil || parts.CurrentTemperature == nil || parts.Setpoint == nil || parts.Mode == nil || *parts.Mode != domain.ClimateModeHeat || parts.Activity == nil || *parts.Activity != domain.ClimateActivityIdle || parts.FanActive != nil {
		t.Fatalf("partial climate: %+v %v", parts, err)
	}
	if number, err := parts.CurrentTemperature.Value(); err != nil || number != 0 {
		t.Fatal("zero reading lost")
	}
	if setpoint, ok := parts.Setpoint.Single(); !ok {
		t.Fatal("single setpoint missing")
	} else if number, err := setpoint.Value(); err != nil || number != 21 {
		t.Fatal("setpoint lost")
	}
	for _, availability := range []domain.Availability{domain.AvailabilityUnknown, domain.AvailabilityUnavailable} {
		quality, err := domain.NewQuality(availability, domain.FreshnessStale, domain.ProvenanceObserved)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := integrations.NewStateObservation("relay", quality, nil, nil, time.Now(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := rig.session.Publish(context.Background(), rig.binding, observation); err != nil {
			t.Fatal(err)
		}
		missing, present := rig.fixture.State(entity)
		if !present {
			t.Fatal("quality-only state missing")
		}
		if _, current := missing.CurrentValue(); current {
			t.Fatal("unknown/unavailable became zero climate")
		}
		if display, present := missing.DisplayValue(); !present || display != value {
			t.Fatal("last-known climate not retained")
		}
	}
	instance, _, err := rig.binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	reconnected, err := rig.host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := reconnected.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("reconnect: %v", err)
	}
	rig.session, rig.binding = reconnected, bindings[0]
	if err := nha174Publish(t, rig, nha174FullClimate(), nil); err != nil {
		t.Fatal(err)
	}
	if driver.Count() != 0 {
		t.Fatal("state publication/discovery/reconnect dispatched an action")
	}
}

func TestNHA174TrueHostRejectsInvalidClimateWithoutPartialPublication(t *testing.T) {
	rig, _ := nha174ClimateRig(t, &nha025Driver{})
	if err := nha174Publish(t, rig, nha174FullClimate(), nil); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		wire     *state.ClimateValue
		expected error
	}{
		"outside-temperature":    {&state.ClimateValue{CurrentTemperatureCelsius: nha174Pointer(51.0)}, contracts.ErrClimateObservationOutsideConstraints},
		"outside-setpoint":       {&state.ClimateValue{Setpoint: &state.ClimateValue_SetpointCelsius{SetpointCelsius: 31}}, contracts.ErrClimateObservationOutsideConstraints},
		"outside-step":           {&state.ClimateValue{Setpoint: &state.ClimateValue_SetpointCelsius{SetpointCelsius: 21.25}}, contracts.ErrClimateObservationOutsideConstraints},
		"unadvertised-mode":      {&state.ClimateValue{Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_COOL)}, contracts.ErrClimateObservationOutsideConstraints},
		"unadvertised-activity":  {&state.ClimateValue{Activity: nha174Pointer(registry.ClimateActivity_CLIMATE_ACTIVITY_COOLING)}, contracts.ErrClimateObservationOutsideConstraints},
		"unadvertised-fan-false": {&state.ClimateValue{FanActive: nha174Pointer(false)}, contracts.ErrUnsupportedClimateObservation},
		"unadvertised-range":     {&state.ClimateValue{Setpoint: &state.ClimateValue_SetpointRangeCelsius{SetpointRangeCelsius: &state.ClimateSetpointRange{LowCelsius: nha174Pointer(18.0), HighCelsius: nha174Pointer(23.0)}}}, contracts.ErrUnsupportedClimateObservation},
	} {
		t.Run(name, func(t *testing.T) {
			before, err := rig.fixture.Sequencer().Capture()
			if err != nil {
				t.Fatal(err)
			}
			if err := nha174Publish(t, rig, test.wire, nil); !errors.Is(err, test.expected) {
				t.Fatalf("expected %v got %v", test.expected, err)
			}
			after, err := rig.fixture.Sequencer().Capture()
			if err != nil {
				t.Fatal(err)
			}
			if before.Cursor != after.Cursor {
				t.Fatal("rejected climate advanced sequencer")
			}
			for id, observed := range before.States {
				if after.States[id] != observed {
					t.Fatal("rejected field partially changed state")
				}
			}
		})
	}
	unit, err := domain.NewUnitCatalog().Parse("Cel")
	if err != nil {
		t.Fatal(err)
	}
	measurement, err := domain.NewMeasurement(21, unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := integrations.NewStateObservation("relay", quality, &value, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.session.Publish(context.Background(), rig.binding, observation); !errors.Is(err, integrations.ErrInvalidInput) {
		t.Fatalf("Measurement bypassed climate contract: %v", err)
	}
}

func TestNHA174ClimateObservationNeverConfirmsAnAction(t *testing.T) {
	driver := &nha025Driver{}
	rig, _ := nha174ClimateRig(t, driver)
	rig.request.CapabilitySchemaId = contracts.ClimateSchema
	rig.request.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: nha174Pointer(21.0), Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_HEAT)}}}
	id := nha025ID(t, rig.request)
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		if err := command.Acknowledge(); err != nil {
			return err
		}
		cause, err := command.Causality()
		if err != nil {
			return err
		}
		for _, wire := range []*state.ClimateValue{
			{CurrentTemperatureCelsius: nha174Pointer(21.0)},
			{Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_HEAT)},
			nha174FullClimate(),
		} {
			if err := nha174Publish(t, rig, wire, &cause); err != nil {
				return err
			}
			nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		}
		return nil
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("simulated climate dispatch: %v %v", response, err)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	if driver.Count() != 1 {
		t.Fatal("observation retried or duplicated the simulated action")
	}
}
