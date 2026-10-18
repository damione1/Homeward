package integrations_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/scenes"
	"github.com/damione1/Homeward/scope"
)

type nha046CaptureBarrier struct {
	*integrations.VolatileCommandService
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (b *nha046CaptureBarrier) CaptureLight(ctx context.Context, alias domain.HAAlias) (integrations.LightCapture, error) {
	b.calls.Add(1)
	capture, err := b.VolatileCommandService.CaptureLight(ctx, alias)
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return capture, err
	case <-ctx.Done():
		return integrations.LightCapture{}, ctx.Err()
	}
}

type nha046DispatchBarrier struct {
	*integrations.VolatileCommandService
	entered  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

type nha046RecordedBarrier struct {
	*integrations.VolatileCommandService
	entered  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (b *nha046RecordedBarrier) RecordedPreparedLight(ctx context.Context, prepared integrations.PreparedLight) (*actions.CommandResponse, bool, error) {
	close(b.entered)
	<-ctx.Done()
	close(b.canceled)
	<-b.release
	return b.VolatileCommandService.RecordedPreparedLight(ctx, prepared)
}

type nha046PanicPort struct {
	*integrations.VolatileCommandService
}

func (nha046PanicPort) DispatchPrepared(context.Context, integrations.PreparedLight) (*actions.CommandResponse, error) {
	panic("synthetic dispatch panic")
}

func (b *nha046DispatchBarrier) DispatchPrepared(ctx context.Context, prepared integrations.PreparedLight) (*actions.CommandResponse, error) {
	close(b.entered)
	<-ctx.Done()
	close(b.canceled)
	<-b.release
	return b.VolatileCommandService.DispatchPrepared(ctx, prepared)
}

func nha046TemporaryService(t *testing.T, command scenes.CommandPort, rig nha179Rig, clock *nha179Clock) (*scenes.Service, scenes.SceneRef) {
	t.Helper()
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	service, err := scenes.NewService(catalogue, rig.fixture.Sequencer(), rig.gen, command, clock)
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("light.nha179")
	id, _ := domain.ParseHAAlias("scene.nha046_lifecycle")
	ref, omitted, err := service.CreateTemporary(context.Background(), id, nil, []domain.HAAlias{alias}, time.Hour, nil)
	if err != nil || len(omitted) != 0 {
		t.Fatalf("scene create: %v %v", err, omitted)
	}
	return service, ref
}

func TestNHA046PreparationSlotConcurrentCallsCaptureOnce(t *testing.T) {
	now := time.Now()
	clock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	level := 25.0
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	barrier := &nha046CaptureBarrier{VolatileCommandService: rig.service, entered: make(chan struct{}, 1), release: make(chan struct{})}
	var once sync.Once
	service, ref := nha046TemporaryService(t, barrier, rig, clock)
	defer func() { once.Do(func() { close(barrier.release) }); service.Close() }()
	invocation, _ := domain.ParseCommandID("nha046-concurrent-slot")
	slot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), invocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	type preparedResult struct {
		prepared scenes.PreparedScene
		err      error
	}
	first := make(chan preparedResult, 1)
	second := make(chan preparedResult, 1)
	go func() {
		prepared, err := slot.PrepareOnce(context.Background())
		first <- preparedResult{prepared, err}
	}()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first capture did not reach barrier")
	}
	changed := 75.0
	rig.publish(t, true, &changed, domain.FreshnessFresh, domain.ProvenanceObserved)
	go func() {
		prepared, err := slot.PrepareOnce(context.Background())
		second <- preparedResult{prepared, err}
	}()
	once.Do(func() { close(barrier.release) })
	var a, b preparedResult
	select {
	case a = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("first preparation stalled")
	}
	select {
	case b = <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second preparation stalled")
	}
	if a.err != nil || b.err != nil || a.prepared != b.prepared || barrier.calls.Load() != 1 {
		t.Fatalf("slot recaptured changed state: first=%v second=%v captures=%d", a.err, b.err, barrier.calls.Load())
	}
}

func TestNHA046CloseCancelsAndJoinsAdmittedApply(t *testing.T) {
	now := time.Now()
	clock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	level := 25.0
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	barrier := &nha046DispatchBarrier{VolatileCommandService: rig.service, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	service, ref := nha046TemporaryService(t, barrier, rig, clock)
	defer func() { once.Do(func() { close(barrier.release) }); service.Close() }()
	invocation, _ := domain.ParseCommandID("nha046-close-admitted")
	slot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), invocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() { _, err := service.ApplyPrepared(context.Background(), prepared); applied <- err }()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not reach barrier")
	}
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	select {
	case <-barrier.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel admitted apply")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before admitted apply joined")
	default:
	}
	once.Do(func() { close(barrier.release) })
	select {
	case err := <-applied:
		if err != nil && !errors.Is(err, scenes.ErrSceneClosed) {
			t.Fatalf("apply: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not finish")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join")
	}
	if rig.driver.Count() != 0 {
		t.Fatal("driver called after scene closure")
	}
	if _, err := service.ApplyPrepared(context.Background(), prepared); !errors.Is(err, scenes.ErrSceneClosed) {
		t.Fatalf("closed scene admitted a new apply: %v", err)
	}
}

func TestNHA046TemporaryPartialResultKeepsMemberRefusal(t *testing.T) {
	now := time.Now()
	clock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	observed := 25.0
	rig.publish(t, true, &observed, domain.FreshnessFresh, domain.ProvenanceObserved)
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	service, err := scenes.NewService(catalogue, rig.fixture.Sequencer(), rig.gen, rig.service, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	lightAlias, _ := domain.ParseHAAlias("light.nha179")
	switchAlias, _ := domain.ParseHAAlias("switch.missing")
	level := 37.5
	value, err := domain.NewLightValue(true, &level, nil)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := scenes.NewExplicitLight(lightAlias, value)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHAAlias("scene.nha046_partial")
	ref, omitted, err := service.CreateTemporary(context.Background(), id, []scenes.ExplicitMember{explicit}, []domain.HAAlias{switchAlias}, time.Hour, nil)
	if err != nil || len(omitted) != 1 || omitted[0].Code() != scenes.OmissionAlias {
		t.Fatalf("partial capture: %v %v", err, omitted)
	}
	invocation, _ := domain.ParseCommandID("nha046-partial-occurrence")
	slot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), invocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	members := prepared.Members()
	if len(members) != 2 || members[0].Outcome() != scenes.OutcomePrepared || members[1].Outcome() != scenes.OutcomeRefused || members[1].Field() != "capture" {
		t.Fatalf("member diagnostic lost: %+v", members)
	}
	result, err := service.ApplyPrepared(context.Background(), prepared)
	if err != nil || result.Aggregate() != scenes.AggregatePartial || rig.driver.Count() != 1 {
		t.Fatalf("partial scene result: %v %+v sends=%d", err, result, rig.driver.Count())
	}
}

func TestNHA046CloseJoinsLedgerOnlyLookupAfterSceneForget(t *testing.T) {
	now := time.Now()
	clock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	level := 25.0
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	barrier := &nha046RecordedBarrier{VolatileCommandService: rig.service, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	service, ref := nha046TemporaryService(t, barrier, rig, clock)
	defer func() { once.Do(func() { close(barrier.release) }); service.Close() }()
	invocation, _ := domain.ParseCommandID("nha046-close-lookup")
	slot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), invocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteTemporary(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	applied := make(chan struct{})
	go func() { _, _ = service.ApplyPrepared(context.Background(), prepared); close(applied) }()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ledger-only lookup did not reach barrier")
	}
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	select {
	case <-barrier.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel ledger-only lookup")
	}
	select {
	case <-closed:
		t.Fatal("Close returned while ledger lookup was blocked")
	default:
	}
	once.Do(func() { close(barrier.release) })
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("ledger-only lookup did not finish")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join lookup")
	}
	if rig.driver.Count() != 0 {
		t.Fatal("ledger-only lookup dispatched")
	}
}

func TestNHA046PanicReleasesSceneApplyLease(t *testing.T) {
	now := time.Now()
	clock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	level := 25.0
	rig.publish(t, true, &level, domain.FreshnessFresh, domain.ProvenanceObserved)
	service, ref := nha046TemporaryService(t, nha046PanicPort{rig.service}, rig, clock)
	invocation, _ := domain.ParseCommandID("nha046-panic-lease")
	slot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), invocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("synthetic panic was swallowed")
			}
		}()
		_, _ = service.ApplyPrepared(context.Background(), prepared)
	}()
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("panic leaked active Apply lease")
	}
	if rig.driver.Count() != 0 {
		t.Fatal("panic fixture reached the driver")
	}
}

func nha046PublishSwitch(t *testing.T, rig nha179Rig, on bool) domain.HAAlias {
	t.Helper()
	ctx := context.Background()
	instance := nha016Instance(t, "nha179-synthetic")
	key, err := integrations.NewSourceKey("nha046-switch-device", "relay")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := rig.fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	alias, _ := domain.ParseHAAlias("switch.nha046")
	record, err := integrations.NewRegistryRecord(ids[0].Device, ids[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic relay",
		Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := rig.session.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatalf("switch discovery: %v", err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value := domain.NewSwitchValue(on)
	at := time.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.session.Publish(ctx, bindings[0], observation); err != nil {
		t.Fatal(err)
	}
	return alias
}

func TestNHA046SwitchAlreadySatisfiedProvesCurrentRevisionAndLatches(t *testing.T) {
	now := time.Now()
	clock := &nha179Clock{now: now}
	sceneClock := &nha179Clock{now: now}
	rig := nha179SetupWithClock(t, false, clock)
	alias := nha046PublishSwitch(t, rig, true)
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := scenes.CompileSelected(selection, scope.LogicMVPSourcesV1())
	if err != nil {
		t.Fatal(err)
	}
	service, err := scenes.NewService(catalogue, rig.fixture.Sequencer(), rig.gen, rig.service, sceneClock)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	explicit, err := scenes.NewExplicitSwitch(alias, true)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := domain.ParseHAAlias("scene.nha046_switch_proof")
	ref, _, err := service.CreateTemporary(context.Background(), id, []scenes.ExplicitMember{explicit}, nil, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	invocation, _ := domain.ParseCommandID("nha046-switch-already")
	slot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), invocation, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := slot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ApplyPrepared(context.Background(), prepared)
	if err != nil || result.Aggregate() != scenes.AggregateComplete || rig.driver.Count() != 0 {
		t.Fatalf("switch no-op: %v %+v", err, result)
	}
	members := result.Members()
	revision, proven := members[0].ProofRevision()
	if members[0].Outcome() != scenes.OutcomeAlreadySatisfied || !proven || revision == 0 || members[0].Response() != nil {
		t.Fatalf("no-op proof is not distinct from physical confirmation: %+v", members[0])
	}
	secondID, _ := domain.ParseCommandID("nha046-switch-changed-before-apply")
	secondSlot, err := scenes.NewPreparationSlot(service, ref, nha022Actor(t), secondID, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	stale, err := secondSlot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Equal at preparation is insufficient: a new observed revision must fail.
	nha046PublishSwitch(t, rig, false)
	changed, err := service.ApplyPrepared(context.Background(), stale)
	if err != nil || changed.Aggregate() != scenes.AggregateNone || changed.Members()[0].Outcome() == scenes.OutcomeAlreadySatisfied || rig.driver.Count() != 0 {
		t.Fatalf("stale equality admitted: %v %+v", err, changed)
	}
	if rev, proven := changed.Members()[0].ProofRevision(); proven || rev != 0 {
		t.Fatal("stale equality claimed proof")
	}
	if err := service.DeleteTemporary(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.ApplyPrepared(context.Background(), prepared)
	if err != nil || replayed.Aggregate() != scenes.AggregateComplete || rig.driver.Count() != 0 {
		t.Fatalf("latched no-op replayed after delete: %v %+v", err, replayed)
	}
	if repeat, ok := replayed.Members()[0].ProofRevision(); !ok || repeat != revision {
		t.Fatal("no-op proof revision changed on retry")
	}
	// The scene clock may lag the command router clock. Equality must not
	// announce a new no-op after the prepared wire's own deadline.
	off, err := scenes.NewExplicitSwitch(alias, false)
	if err != nil {
		t.Fatal(err)
	}
	expiringRef, _, err := service.CreateTemporary(context.Background(), id, []scenes.ExplicitMember{off}, nil, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	expiringID, _ := domain.ParseCommandID("nha046-switch-expiring-proof")
	expiringSlot, err := scenes.NewPreparationSlot(service, expiringRef, nha022Actor(t), expiringID, nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	expiring, err := expiringSlot.PrepareOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock.Set(now.Add(2 * time.Minute))
	expired, err := service.ApplyPrepared(context.Background(), expiring)
	if err != nil || expired.Aggregate() != scenes.AggregateNone || rig.driver.Count() != 0 {
		t.Fatalf("router deadline did not fence no-op: %v %+v", err, expired)
	}
	if len(expired.Members()) != 1 || expired.Members()[0].Code() != scenes.RefusalCommand ||
		expired.Members()[0].Field() != "expires_at" || expired.Members()[0].Outcome() == scenes.OutcomeAlreadySatisfied {
		t.Fatalf("router deadline was not identified: %+v", expired.Members())
	}
}
