package contracts

import (
	"testing"
	"time"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func TestNHA077FanWireAdmissionPreservesOffAndRejectsAbsence(t *testing.T) {
	if _, err := domain.NewFanStateValue(domain.FanValue{}); err == nil {
		t.Fatal("Go zero fan speed admitted as a present state")
	}
	catalog, err := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	entity := testNHA014Entity(t)
	description := &registry.CapabilityDescription{SchemaId: FanSchema, Major: 1,
		Endpoint: "speed", DisplayName: "Synthetic fan",
		Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{
			ObservesPower: true, Speed: &registry.ControlChoices{Choices: []*registry.ControlChoice{{Key: "Off"}, {Key: "High"}}}}}}
	if err := catalog.RegisterDriverCapability(entity, description); err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable,
		domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	off, err := domain.NewFanStateValue(nha212FixtureFan(t, "Off"))
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := testNHA017Wire(t, nil, quality, &off,
		time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), 1)
	if wire.GetFan() == nil || wire.GetFan().Speed == nil || *wire.GetFan().Speed != "Off" || wire.GetFan().On == nil || *wire.GetFan().On {
		t.Fatal("present Off fan state disappeared on wire")
	}
	profile := &StateMeasurementAdmission{Catalog: catalog, Endpoint: "speed"}
	if _, err := AdmitWireState(wire, nil, profile); err != nil {
		t.Fatalf("present Off fan state rejected: %v", err)
	}
	absent, unknown := "", "uncatalogued"
	for _, speed := range []*string{&absent, &unknown} {
		candidate := proto.CloneOf(wire)
		candidate.GetFan().Speed = speed
		if _, err := AdmitWireState(candidate, nil, profile); err == nil {
			t.Fatal("invalid source key admitted")
		}
	}
	missing := proto.Clone(wire).(*state.ObservedState)
	missing.Value = nil
	missing.Quality.Availability = state.Availability_AVAILABILITY_UNKNOWN
	missing.Quality.Freshness = state.Freshness_FRESHNESS_STALE
	if admitted, err := AdmitWireState(missing, nil, profile); err != nil {
		t.Fatalf("absent state should remain representable: %v", err)
	} else if _, present := admitted.CurrentValue(); present {
		t.Fatal("absent initial fan state became Off")
	}
}
