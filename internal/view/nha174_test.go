package view_test

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/view"
)

func TestNHA174ClimateSurvivesCurrentPageAndWatchCodec(t *testing.T) {
	f := newWatchFixture(t, 1)
	_, key, err := f.bindings[0].Source()
	if err != nil {
		t.Fatal(err)
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic climate view",
		Constraints: &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{ObservedTemperatureCelsius: &registry.ClimateTemperatureObservation{}, ObservedSetpointCelsius: &registry.ClimateTemperatureObservation{}, ObservedModeOptions: []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_HEAT}}}}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := f.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("climate declaration: %v", err)
	}
	current, target := 0.0, 21.0
	mode := registry.ClimateMode_CLIMATE_MODE_HEAT
	publish := func() {
		t.Helper()
		climate, err := contracts.AdmitWireClimateValue(&state.ClimateValue{CurrentTemperatureCelsius: &current, Setpoint: &state.ClimateValue_SetpointCelsius{SetpointCelsius: target}, Mode: &mode})
		if err != nil {
			t.Fatal(err)
		}
		value, err := domain.NewClimateStateValue(climate)
		if err != nil {
			t.Fatal(err)
		}
		quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		if err != nil {
			t.Fatal(err)
		}
		at := f.clock.Now()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.session.Publish(context.Background(), bindings[0], observation); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	filter, err := view.Only(f.ids)
	if err != nil {
		t.Fatal(err)
	}
	page := f.page(t, filter)
	if len(page.States) != 1 || len(page.Entities) != 1 || page.Entities[0].State.GetClimate() == nil {
		t.Fatal("page split or lost climate identity")
	}
	climate := page.States[0].GetClimate()
	if climate == nil || climate.CurrentTemperatureCelsius == nil || *climate.CurrentTemperatureCelsius != 0 || climate.Mode == nil || *climate.Mode != mode || climate.GetSetpointCelsius() != 21 || climate.Activity != nil || climate.FanActive != nil {
		t.Fatalf("page lost presences: %v", climate)
	}
	watch := openWatch(t, f, page.Cursor, f.ids...)
	current = 20.5
	f.clock.Advance(time.Second, time.Second)
	publish()
	frame := nextFrame(t, watch)
	delta := frame.GetDelta()
	if delta == nil || len(delta.States) != 1 || delta.States[0].Entity.GetEntityId() != page.States[0].Entity.GetEntityId() {
		t.Fatal("watch lost climate identity")
	}
	if climate := delta.States[0].GetClimate(); climate == nil || climate.CurrentTemperatureCelsius == nil || *climate.CurrentTemperatureCelsius != 20.5 || climate.GetSetpointCelsius() != 21 || climate.Mode == nil || *climate.Mode != mode || climate.Activity != nil {
		t.Fatal("watch lost coherent climate")
	}
}
