package sequencer_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

func TestNHA029ConfigurationCutKeepsBoundedInternalCapture(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	sequence := fixture.Sequencer()
	detached, err := sequence.CaptureTriggerView(sequencer.MaxJournalBytes)
	if err != nil || detached.RegistryReady() {
		t.Fatalf("detached capture invented a ready registry: %v", err)
	}
	publisher := nha019Start(t, host, "nha029-internal-capture")
	device, entity, err := publisher.binding.IDs()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha029-internal-capture")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := domain.ParseHAAlias("switch.fixture")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := sequence.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	before, err := sequence.Capture()
	if err != nil || !before.RegistryReady || len(before.States) != 1 || len(before.Registry) != 1 {
		t.Fatalf("real registry/state setup was not ready: %v", err)
	}
	cut, err := sequence.BeginConfigurationPublication()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = cut.Release()
		}
	}()
	if _, err := sequence.Capture(); !errors.Is(err, sequencer.ErrRegistryUnavailable) {
		t.Fatalf("external capture crossed configuration cut: %v", err)
	}
	if _, err := sequence.CaptureBounded(sequencer.MaxJournalBytes); !errors.Is(err, sequencer.ErrRegistryUnavailable) {
		t.Fatalf("bounded external capture crossed configuration cut: %v", err)
	}
	if _, _, _, err := sequence.SubscribeViewAfter(before.Cursor, 256); !errors.Is(err, sequencer.ErrRegistryUnavailable) {
		t.Fatalf("external replay subscription crossed configuration cut: %v", err)
	}
	refused, err := sequence.CaptureTriggerView(512)
	if !errors.Is(err, sequencer.ErrTriggerViewBudget) || refused.States != nil || refused.Registry != nil {
		t.Fatalf("internal capture bypassed its pre-copy budget: %v", err)
	}
	internal, err := sequence.CaptureTriggerView(sequencer.MaxJournalBytes)
	if err != nil || !internal.RegistryReady() || internal.Cursor != before.Cursor ||
		len(internal.States) != len(before.States) || len(internal.Registry) != len(before.Registry) ||
		!internal.Registry[entity].Equal(record) {
		t.Fatalf("bounded internal capture lost the exact ready projection: %v", err)
	}
	if state, present := internal.States[entity]; !present || !reflect.DeepEqual(state, before.States[entity]) {
		t.Fatal("internal capture changed the current observed state")
	}
	sub, replay, boundary, _, err := sequence.SubscribeTriggerViewAfter(internal.Cursor, 256, sequencer.MaxViewQueueBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if len(replay) != 0 || boundary != internal.Cursor {
		t.Fatal("internal capture/replay boundary changed without a source event")
	}
	if err := cut.Release(); err != nil {
		t.Fatal(err)
	}
	released = true
	if _, err := sequence.Capture(); err != nil {
		t.Fatalf("coherent cut release did not reopen capture: %v", err)
	}
	sequence.InvalidateRegistry()
	if _, err := sequence.CaptureTriggerView(sequencer.MaxJournalBytes); !errors.Is(err, sequencer.ErrRegistryUnavailable) {
		t.Fatalf("internal capture accepted an unavailable registry: %v", err)
	}
}
