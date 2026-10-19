package sequencer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/sequencer"
)

type nha040NilContext struct{ context.Context }

func TestNHA040TriggerSnapshotAndReplayPreflight(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	seq := fixture.Sequencer()
	before, err := seq.CaptureTriggerView(8 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if before.EstimatedBytes() < 512 || len(before.States) != 0 {
		t.Fatal("empty trigger capture charge or state is invalid")
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha040-trigger-snapshot")
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := fake.NewDriver(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := seq.CaptureTriggerView(512); !errors.Is(err, sequencer.ErrTriggerViewBudget) {
		t.Fatalf("capture budget did not refuse before copying: %v", err)
	}
	after, err := seq.CaptureTriggerView(8 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.States) != 1 || after.EstimatedBytes() <= before.EstimatedBytes() {
		t.Fatalf("state was not charged in focused snapshot: states=%d bytes=%d", len(after.States), after.EstimatedBytes())
	}
	if _, _, _, _, err := seq.SubscribeTriggerViewAfter(before.Cursor, 256, 1); !errors.Is(err, sequencer.ErrTriggerViewBudget) {
		t.Fatalf("replay budget did not refuse before subscription: %v", err)
	}
	sub, replay, end, charge, err := seq.SubscribeTriggerViewAfter(before.Cursor, 256, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if len(replay) != 3 || charge <= 0 || end != after.Cursor {
		t.Fatalf("replay boundary: entries=%d charge=%d end=%v", len(replay), charge, end)
	}
	if replay[0].Kind() != sequencer.KindState || replay[1].Kind() != sequencer.KindEvent || replay[2].Kind() != sequencer.KindHealth {
		t.Fatal("replay order changed")
	}
	binding, err := driver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	device, entity, err := binding.IDs()
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
	if err := seq.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	registered, err := seq.CaptureTriggerView(8 << 20)
	if err != nil || len(registered.States) != 1 || len(registered.Registry) != 1 {
		t.Fatalf("registered projection missing: %+v %v", registered, err)
	}
	if err := seq.PublishRegistry(ctx, nil); err != nil {
		t.Fatal(err)
	}
	removedSub, removedReplay, _, _, err := seq.SubscribeTriggerViewAfter(registered.Cursor, 256, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer removedSub.Close()
	if len(removedReplay) != 1 || removedReplay[0].Kind() != sequencer.KindRegistry {
		t.Fatalf("pure removal did not retain one registry cursor: %+v", removedReplay)
	}
	removed, ok := removedReplay[0].RegistryRemoved()
	if !ok || len(removed) != 1 || removed[0] != entity {
		t.Fatalf("pure removal was not recorded: %+v %v", removed, ok)
	}
	removed[0] = domain.EntityID{}
	secondRead, _ := removedReplay[0].RegistryRemoved()
	if secondRead[0] != entity {
		t.Fatal("registry removal getter exposed journal backing")
	}
	orphaned, err := seq.CaptureTriggerView(8 << 20)
	if err != nil || len(orphaned.States) != 0 || len(orphaned.Registry) != 0 {
		t.Fatalf("orphan state leaked into trigger capture: %+v %v", orphaned, err)
	}
}

func TestNHA040NativeEdgeSharesJournalWithoutEntityOrDeviceEvent(t *testing.T) {
	ctx := context.Background()
	_, fixture, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	seq := fixture.Sequencer()
	instance, _ := domain.ParseIntegrationInstanceID("nha040-native")
	sourceSequence := uint64(7)
	stamp, err := domain.NewSourceStamp("session-1", &sourceSequence)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("pressed")
	edge, err := integrations.NewNativeMQTTEdge("buttons/press", payload, false, sourceSequence)
	if err != nil {
		t.Fatal(err)
	}
	before, err := seq.CaptureTriggerView(8 << 20)
	if err != nil {
		t.Fatal(err)
	}
	deviceEvents, _, err := seq.SubscribeEdge()
	if err != nil {
		t.Fatal(err)
	}
	defer deviceEvents.Close()
	if err := seq.AcceptNativeMQTTEdge(ctx, instance, stamp, edge); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'x'
	sub, replay, _, charged, err := seq.SubscribeTriggerViewAfter(before.Cursor, 256, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if len(replay) != 1 || replay[0].Kind() != sequencer.KindNativeMQTTEdge || replay[0].EstimatedBytes() < len(payload) || charged < replay[0].EstimatedBytes() {
		t.Fatalf("native journal admission or charge: %+v %d", replay, charged)
	}
	if _, _, _, _, err := seq.SubscribeTriggerViewAfter(before.Cursor, 256, replay[0].EstimatedBytes()-1); !errors.Is(err, sequencer.ErrTriggerViewBudget) {
		t.Fatalf("native replay copied without full preflight: %v", err)
	}
	got, ok := replay[0].NativeMQTTEdge()
	if !ok {
		t.Fatal("native payload accessor failed")
	}
	_, data, seqNo, err := got.Components()
	if err != nil || string(data) != "pressed" || seqNo != sourceSequence {
		t.Fatalf("native payload changed: %q %d %v", data, seqNo, err)
	}
	data[0] = 'x'
	again, _ := replay[0].NativeMQTTEdge()
	_, data, _, _ = again.Components()
	if string(data) != "pressed" {
		t.Fatal("native payload getter exposed journal backing")
	}
	if edgeStatus, err := deviceEvents.Status(); err != nil || edgeStatus.Queued != 0 {
		t.Fatalf("native packet entered device event mailbox: %+v %v", edgeStatus, err)
	}
	if instanceGot, sourceGot, _, origin := replay[0].Origin(); !origin || instanceGot != instance || sourceGot != stamp {
		t.Fatal("native edge lost its source session stamp")
	}
}

func TestNHA040MixedRegistryRemovalBudgetRejectsBeforeMutation(t *testing.T) {
	ctx := context.Background()
	seq, err := sequencer.New(sequencer.RealClock{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha040-registry-preflight")
	device, _ := domain.ParseDeviceID("device-a")
	entity, _ := domain.ParseEntityID("Host:entity-a")
	key, _ := integrations.NewSourceKey("device-a", "relay")
	alias, _ := domain.ParseHAAlias("switch.fixture")
	old, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := seq.PublishRegistry(ctx, []integrations.RegistryRecord{old}); err != nil {
		t.Fatal(err)
	}
	before, err := seq.Capture()
	if err != nil {
		t.Fatal(err)
	}
	largeEntity, err := domain.ParseEntityID(strings.Repeat("x", sequencer.MaxJournalBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	largeDevice, _ := domain.ParseDeviceID("device-b")
	largeKey, _ := integrations.NewSourceKey("device-b", "relay")
	large, err := integrations.NewRegistryRecord(largeDevice, largeEntity, instance, largeKey, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := seq.PublishRegistry(ctx, []integrations.RegistryRecord{large}); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("mixed changed/removal entry exceeded journal budget: %v", err)
	}
	after, err := seq.Capture()
	if err != nil || after.Cursor != before.Cursor || len(after.Registry) != 1 {
		t.Fatalf("rejected mixed entry advanced cursor or replaced registry: %+v %v", after.Cursor, err)
	}
	if _, ok := after.Registry[entity]; !ok {
		t.Fatal("rejected mixed entry removed prior identity")
	}
	entries, _, err := seq.ReadAfter(before.Cursor, 1)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected mixed entry leaked into journal: %d %v", len(entries), err)
	}
}

func TestNHA040NativeEdgeRejectsTypedNilContextBeforeAdmission(t *testing.T) {
	seq, err := sequencer.New(sequencer.RealClock{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha040-typed-nil")
	sequence := uint64(1)
	stamp, err := domain.NewSourceStamp("nha040-session", &sequence)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := integrations.NewNativeMQTTEdge("buttons/press", nil, false, sequence)
	if err != nil {
		t.Fatal(err)
	}
	before, err := seq.Capture()
	if err != nil {
		t.Fatal(err)
	}
	var nilCtx *nha040NilContext
	if err := seq.AcceptNativeMQTTEdge(nilCtx, instance, stamp, edge); !errors.Is(err, sequencer.ErrInvalidInput) {
		t.Fatalf("typed-nil context crossed native admission: %v", err)
	}
	after, err := seq.Capture()
	if err != nil || after.Cursor != before.Cursor || len(after.States) != len(before.States) {
		t.Fatalf("typed-nil context changed sequencer: %+v %v", after.Cursor, err)
	}
}
