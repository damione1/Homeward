package contracts

import (
	"testing"
	"time"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func TestNHA069LightWireRequiresPresenceAndAdvertisedBounds(t *testing.T) {
	if _, err := domain.NewLightStateValue(domain.LightValue{}); err == nil {
		t.Fatal("zero LightValue became an observed off state")
	}
	if _, ok := (domain.StateValue{}).Light(); ok {
		t.Fatal("zero StateValue became a light")
	}
	catalog, err := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	entity := testNHA014Entity(t)
	for _, description := range []*registry.CapabilityDescription{
		{SchemaId: LightSchema, Major: 1, DisplayName: "Synthetic lamp", Endpoint: "mqtt", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100}, ColorTempMireds: &registry.NumberBounds{Minimum: 250, Maximum: 454}}}},
		{SchemaId: LightSchema, Major: 1, DisplayName: "Synthetic on-only lamp", Endpoint: "mqtt-on-only", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{}}},
	} {
		if err := catalog.RegisterDriverCapability(entity, description); err != nil {
			t.Fatal(err)
		}
	}
	on := false
	brightness := 0.0
	ct := uint32(300)
	light, err := domain.NewLightValue(on, &brightness, &ct)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewLightStateValue(light)
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	wire, _ := testNHA017Wire(t, nil, quality, &value, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), 1)
	admission := &StateCapabilityAdmission{Catalog: catalog, Endpoint: "mqtt"}
	decoded, err := AdmitWireState(wire, nil, admission)
	if err != nil {
		t.Fatal(err)
	}
	current, present := decoded.CurrentValue()
	if !present {
		t.Fatal("explicit off light value disappeared")
	}
	got, ok := current.Light()
	if !ok {
		t.Fatal("roundtrip value is not a light")
	}
	gotOn, gotBrightness, gotCT, err := got.Components()
	if err != nil || gotOn || gotBrightness == nil || *gotBrightness != 0 || gotCT == nil || *gotCT != 300 {
		t.Fatalf("off/zero/CT roundtrip: %v %v %v %v", gotOn, gotBrightness, gotCT, err)
	}
	for name, mutate := range map[string]func(*state.ObservedState){
		"missing on": func(w *state.ObservedState) { w.GetLight().On = nil },
		"CT below":   func(w *state.ObservedState) { n := uint32(249); w.GetLight().ColorTempMireds = &n },
		"CT above":   func(w *state.ObservedState) { n := uint32(455); w.GetLight().ColorTempMireds = &n },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := proto.Clone(wire).(*state.ObservedState)
			mutate(candidate)
			if _, err := AdmitWireState(candidate, nil, admission); err == nil {
				t.Fatal("out-of-profile light admitted")
			}
		})
	}
	if _, err := AdmitWireState(wire, nil, &StateCapabilityAdmission{Catalog: catalog, Endpoint: "mqtt-on-only"}); err == nil {
		t.Fatal("unadvertised brightness and CT admitted")
	}
	if _, err := AdmitWireState(wire, nil, nil); err == nil {
		t.Fatal("light admitted without descriptor")
	}
}
