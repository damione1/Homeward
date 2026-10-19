package view_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/damione1/Homeward/contracts"
	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
	"strings"
	"testing"
	"time"
)

func TestNHA105ViewPagesReplayAndWire(t *testing.T) {
	f := newWatchFixture(t, 1)
	_, key, _ := f.bindings[0].Source()
	description := &registrywire.CapabilityDescription{SchemaId: contracts.SolarEventSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic solar view", Constraints: &registrywire.CapabilityDescription_SolarEvent{SolarEvent: &solarwire.SolarEventConstraints{Event: solarwire.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}}
	candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := f.session.Discover(context.Background(), []integrations.Candidate{candidate})
	if err != nil {
		t.Fatal(err)
	}
	publish := func(occurrence domain.SolarOccurrence) {
		t.Helper()
		event, _ := domain.NewSolarEventValue(domain.SolarDawn, occurrence)
		value, _ := domain.NewSolarEventStateValue(event)
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		at := f.clock.Now()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.session.Publish(context.Background(), bindings[0], observation); err != nil {
			t.Fatal(err)
		}
	}
	present, _ := domain.SolarOccurrenceAt(f.clock.Now().Add(time.Hour))
	publish(present)
	page := f.page(t, view.All())
	if page.States[0].GetSolarEvent() == nil || page.States[0].GetSolarEvent().Occurrence.GetAt() == nil {
		t.Fatal("solar page lost present event")
	}
	missing, _ := domain.SolarOccurrenceMissing(domain.SolarNoEventWithinSearchHorizon)
	f.clock.Advance(time.Second, time.Second)
	publish(missing)
	watch := openWatch(t, f, page.Cursor)
	defer watch.Close()
	frame := nextFrame(t, watch)
	if frame.GetDelta() == nil || len(frame.GetDelta().States) != 1 || frame.GetDelta().States[0].GetSolarEvent().Occurrence.GetAbsent() != solarwire.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON {
		t.Fatal("replayed solar absence became a timestamp")
	}
	if page.States[0].GetSolarEvent().Occurrence.GetAt() == nil {
		t.Fatal("old solar page mutated")
	}
	f.clock.Advance(time.Second, time.Second)
	publish(present)
	live := nextFrame(t, watch)
	if live.GetDelta() == nil || live.GetDelta().States[0].GetSolarEvent().Occurrence.GetAt() == nil {
		t.Fatal("solar replay/live seam lost presence")
	}
}

type nha105ViewIdentity struct{}

func (nha105ViewIdentity) Resolve(ctx context.Context, instance domain.IntegrationInstanceID, keys []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if _, err := instance.Value(); err != nil {
		return nil, err
	}
	identities := make([]integrations.ResolvedIdentity, len(keys))
	for i, key := range keys {
		device, entity, err := key.Components()
		if err != nil {
			return nil, err
		}
		identities[i].Device, err = domain.ParseDeviceID("nha105-" + device)
		if err != nil {
			return nil, err
		}
		identities[i].Entity, err = domain.ParseEntityID("nha105-" + device + "-" + entity)
		if err != nil {
			return nil, err
		}
	}
	return identities, nil
}

func TestNHA105ViewPreparationIsBounded(t *testing.T) {
	f := newWatchFixture(t, 2)
	f.publish(t, 0, false)
	f.publish(t, 1, false)
	previous, err := f.service.OpenView(context.Background(), f.actor, view.All(), 1)
	if err != nil || previous.NextPageToken == "" {
		t.Fatal("old capture missing")
	}
	measureConstructor := func() float64 {
		return testing.AllocsPerRun(5, func() {
			service, err := view.NewService(f.seq, f.auth, f.auth, f.clock)
			if err != nil || service == nil {
				panic("view availability failed")
			}
		})
	}
	small := measureConstructor()
	largeText := strings.Repeat("s", 16384)
	description := &registrywire.CapabilityDescription{SchemaId: contracts.HelperSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic large select", Constraints: &registrywire.CapabilityDescription_Helper{Helper: &helperwire.HelperConstraints{Kind: helperwire.HelperKind_HELPER_KIND_SELECT, Operations: []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET}, SelectOptions: []string{largeText}}}}
	candidates := make([]integrations.Candidate, 200)
	keys := make([]integrations.SourceKey, len(candidates))
	for i := range candidates {
		key, _ := integrations.NewSourceKey(fmt.Sprintf("nha105-large-%d", i), "value")
		keys[i] = key
		candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{description})
		if err != nil {
			t.Fatal(err)
		}
		candidates[i] = candidate
	}
	snapshot, err := f.seq.CaptureBounded(view.MaxSnapshotBytes)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]integrations.RegistryRecord, 0, len(snapshot.Registry)+len(candidates))
	for _, record := range snapshot.Registry {
		records = append(records, record)
	}
	identity := nha105ViewIdentity{}
	instance, _ := domain.ParseIntegrationInstanceID("nha105-large-view")
	identities, err := identity.Resolve(context.Background(), instance, keys)
	if err != nil {
		t.Fatal(err)
	}
	for i, resolved := range identities {
		alias, _ := domain.ParseHAAlias(fmt.Sprintf("sensor.nha105_large_%d", i))
		record, err := integrations.NewRegistryRecord(resolved.Device, resolved.Entity, instance, keys[i], alias, alias, []domain.HAAlias{alias}, false)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := f.seq.PublishRegistry(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(identity, &projectedSink{sequence: f.seq}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(context.Background(), candidates)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.seq.CaptureBounded(view.MaxSnapshotBytes); err != nil {
		t.Fatal("descriptions alone unexpectedly exceeded test preparation budget")
	}
	helper, err := domain.NewHelperSelect(largeText)
	if err != nil {
		t.Fatal(err)
	}
	value, _ := domain.NewHelperStateValue(helper, 1)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	for _, binding := range bindings {
		at := f.clock.Now()
		observation, err := integrations.NewStateObservation("value", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(context.Background(), binding, observation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.seq.CaptureBounded(view.MaxSnapshotBytes); !errors.Is(err, sequencer.ErrSnapshotBudget) {
		t.Fatal("global image omitted retained helper backing")
	}
	if large := measureConstructor(); large > small+2 {
		t.Fatal("NewService copied the global projection")
	}
	filter, _ := view.Only(f.ids[:1])
	if _, err := f.service.OpenView(context.Background(), f.actor, filter, 1); !errors.Is(err, view.ErrResourceExhausted) {
		t.Fatal("narrow filter bypassed global preparation budget")
	}
	if retained, err := f.service.ReadPage(context.Background(), f.actor, previous.SnapshotToken, previous.NextPageToken); err != nil || len(retained.Items) != 1 {
		t.Fatal("refused preparation destroyed predecessor capture")
	}
}
