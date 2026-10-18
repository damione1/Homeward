package integrations_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha023Rig struct {
	host    *integrations.Manager
	fixture *fake.Fixture
	entity  domain.EntityID
	epoch   string
	request *actions.CommandRequest
	driver  *nha022Driver
	service *integrations.VolatileCommandService
	gen     *nha022Generation
}

type nha023ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *nha023ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *nha023ManualClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type nha023ObservedContext struct {
	context.Context
	reached chan struct{}
	once    sync.Once
}

func (c *nha023ObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.reached) })
	return c.Context.Done()
}

func nha023SetupBare(t *testing.T, driver *nha022Driver) nha023Rig {
	t.Helper()
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "nha023-relay")
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	record := nha022Record(t, ids[0], instance, key, "switch.nha023_old", "switch.nha023", false)
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	fakeDriver, err := fake.NewActionDriver(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := fakeDriver.Start(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	gen := &nha022Generation{revision: 7}
	start, err := fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := start.Components()
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, ids[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	request.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_HaAlias{HaAlias: "switch.nha023_old"}}
	return nha023Rig{host, fixture, ids[0].Entity, epoch, request, driver, nil, gen}
}

func nha023Setup(t *testing.T, driver *nha022Driver, limits integrations.VolatileCommandLimits, clocks ...integrations.CommandClock) nha023Rig {
	t.Helper()
	rig := nha023SetupBare(t, driver)
	var err error
	if len(clocks) == 0 {
		rig.service, err = integrations.NewVolatileCommandService(rig.host, rig.gen, limits)
	} else {
		rig.service, err = integrations.NewVolatileCommandServiceWithClock(rig.host, rig.gen, clocks[0], limits)
	}
	if err != nil {
		t.Fatal(err)
	}
	return rig
}

func TestNHA023ReplayConflictAndCanonicalIntent(t *testing.T) {
	rig := nha023Setup(t, &nha022Driver{}, integrations.DefaultVolatileCommandLimits())
	status, err := rig.service.Status()
	if err != nil || status.MaxEntries != 16384 || status.MaxRetainedBytes != 64<<20 || status.MaxIntentBytes != 64<<10 {
		t.Fatalf("default ledger budget = %+v %v", status, err)
	}
	actor := nha022Actor(t)
	before := proto.Clone(rig.request).(*actions.CommandRequest)
	first, err := rig.service.Dispatch(context.Background(), actor, rig.request)
	if err != nil || first == nil || first.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.count() != 1 {
		t.Fatalf("first dispatch: %v %v, calls=%d", first, err, rig.driver.count())
	}
	if !proto.Equal(rig.request, before) {
		t.Fatal("service rewrote the caller's intention")
	}
	first.Status = actions.ActionStatus_ACTION_STATUS_FAILED
	rig.fixture.InvalidateRegistry() // replay cannot re-resolve an alias or reroute
	replayed, err := rig.service.Dispatch(context.Background(), actor, before)
	if err != nil || replayed.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || rig.driver.count() != 1 {
		t.Fatalf("replay after invalidation: %v %v, calls=%d", replayed, err, rig.driver.count())
	}
	replayed.Status = actions.ActionStatus_ACTION_STATUS_FAILED
	again, err := rig.service.Dispatch(context.Background(), actor, before)
	if err != nil || again.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("result was not copied out: %v %v", again, err)
	}
	variants := map[string]func(*actions.CommandRequest){
		"target alias versus ID": func(c *actions.CommandRequest) {
			id, _ := rig.entity.Value()
			c.Target = &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: id}}
		},
		"argument": func(c *actions.CommandRequest) {
			c.Arguments = &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: false}}}
		},
		"optional zero presence": func(c *actions.CommandRequest) { zero := uint64(0); c.ExpectedRevision = &zero },
		"config revision":        func(c *actions.CommandRequest) { c.ConfigRevision++ },
		"expiry":                 func(c *actions.CommandRequest) { c.ExpiresAt.Seconds++ },
		"endpoint":               func(c *actions.CommandRequest) { c.Endpoint = "another" },
	}
	for name, change := range variants {
		t.Run(name, func(t *testing.T) {
			changed := proto.Clone(before).(*actions.CommandRequest)
			change(changed)
			got, err := rig.service.Dispatch(context.Background(), actor, changed)
			if got != nil || !errors.Is(err, integrations.ErrCommandIDConflict) || rig.driver.count() != 1 {
				t.Fatalf("different intent = %v %v, calls=%d", got, err, rig.driver.count())
			}
		})
	}
	other, _ := domain.ParseActorID("different-trusted-actor")
	if got, err := rig.service.Dispatch(context.Background(), other, before); got != nil || !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatalf("actor changed = %v %v", got, err)
	}
}

func TestNHA023FailedAdmissionIsReplayedWithoutRoute(t *testing.T) {
	rig := nha023Setup(t, &nha022Driver{}, integrations.DefaultVolatileCommandLimits())
	bad := proto.Clone(rig.request).(*actions.CommandRequest)
	bad.Arguments = nil
	actor := nha022Actor(t)
	first, err := rig.service.Dispatch(context.Background(), actor, bad)
	if err != nil || first == nil || first.Error == nil || first.Error.FieldPath != "arguments" || rig.driver.count() != 0 {
		t.Fatalf("first rejection = %v %v, calls=%d", first, err, rig.driver.count())
	}
	rig.fixture.InvalidateRegistry()
	replay, err := rig.service.Dispatch(context.Background(), actor, bad)
	if err != nil || !proto.Equal(first, replay) || rig.driver.count() != 0 {
		t.Fatalf("rejected replay = %v %v, calls=%d", replay, err, rig.driver.count())
	}
}

func TestNHA023DriverCanReadLedgerStatus(t *testing.T) {
	driver := &nha022Driver{}
	var service *integrations.VolatileCommandService
	driver.onDispatch = func() error {
		status, err := service.Status()
		if err != nil || status.Entries != 1 {
			return errors.New("ledger status unavailable during dispatch")
		}
		return nil
	}
	rig := nha023Setup(t, driver, integrations.DefaultVolatileCommandLimits())
	service = rig.service
	got, err := service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || got == nil || got.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("pilot status read = %v %v", got, err)
	}
}

func TestNHA023EpochRestartRequiresNewIntention(t *testing.T) {
	first := nha023Setup(t, &nha022Driver{}, integrations.DefaultVolatileCommandLimits())
	actor := nha022Actor(t)
	if got, err := first.service.Dispatch(context.Background(), actor, first.request); err != nil || got.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("first epoch: %v %v", got, err)
	}
	restarted := nha023Setup(t, &nha022Driver{}, integrations.DefaultVolatileCommandLimits())
	if restarted.epoch == first.epoch {
		t.Fatal("two real sequencers unexpectedly reused an epoch")
	}
	old := proto.Clone(first.request).(*actions.CommandRequest)
	old.Target = proto.Clone(restarted.request.Target).(*wireregistry.EntityRef)
	got, err := restarted.service.Dispatch(context.Background(), actor, old)
	if err != nil || got == nil || got.Error == nil || got.Error.Code != actions.ErrorCode_ERROR_CODE_STALE_EPOCH || restarted.driver.count() != 0 {
		t.Fatalf("old epoch retry: %v %v, calls=%d", got, err, restarted.driver.count())
	}
	if old.AdmissionEpoch != first.epoch || old.CommandId != first.request.CommandId {
		t.Fatal("retry was silently renamed or refreshed")
	}
	newIntent := proto.Clone(restarted.request).(*actions.CommandRequest)
	newIntent.CommandId = "conscious-intention-after-reconciliation"
	got, err = restarted.service.Dispatch(context.Background(), actor, newIntent)
	if err != nil || got == nil || got.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || restarted.driver.count() != 1 {
		t.Fatalf("explicit new intention: %v %v, calls=%d", got, err, restarted.driver.count())
	}
}

func TestNHA023ConcurrentRoutersJoinAndFollowerCancellation(t *testing.T) {
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{})}
	var entered atomic.Int32
	driver.onDispatch = func() error { entered.Add(1); return nil }
	rig := nha023Setup(t, driver, integrations.DefaultVolatileCommandLimits())
	otherRouter, err := integrations.NewCommandRouter(rig.host, rig.gen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := integrations.NewVolatileCommandService(rig.host, rig.gen, integrations.DefaultVolatileCommandLimits()); !errors.Is(err, integrations.ErrVolatileServiceAttached) {
		t.Fatalf("second ledger attached: %v", err)
	}
	actor := nha022Actor(t)
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	first := make(chan outcome, 1)
	go func() {
		response, err := rig.service.Dispatch(context.Background(), actor, rig.request)
		first <- outcome{response, err}
	}()
	<-driver.entered
	var wg sync.WaitGroup
	results := make(chan outcome, 16)
	joined := make([]<-chan struct{}, 16)
	for i := 0; i < 16; i++ {
		observed := &nha023ObservedContext{Context: context.Background(), reached: make(chan struct{})}
		joined[i] = observed.reached
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := otherRouter.Dispatch(observed, actor, rig.request)
			results <- outcome{response, err}
		}()
	}
	for _, reached := range joined {
		<-reached
	}
	changed := proto.Clone(rig.request).(*actions.CommandRequest)
	changed.Endpoint = "changed"
	if got, err := otherRouter.Dispatch(context.Background(), actor, changed); got != nil || !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatalf("conflict while pending = %v %v", got, err)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	observed := &nha023ObservedContext{Context: cancelCtx, reached: make(chan struct{})}
	cancelled := make(chan outcome, 1)
	go func() {
		response, err := otherRouter.Dispatch(observed, actor, rig.request)
		cancelled <- outcome{response, err}
	}()
	<-observed.reached // follower reached the pending select
	cancel()
	if result := <-cancelled; result.response != nil || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled follower = %v %v", result.response, result.err)
	}
	close(driver.release)
	wg.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("follower result = %v %v", result.response, result.err)
		}
	}
	result := <-first
	if result.err != nil || result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.count() != 1 || entered.Load() != 1 {
		t.Fatalf("leader result=%v %v, entries=%d, calls=%d", result.response, result.err, entered.Load(), driver.count())
	}
}

func TestNHA023IndeterminateAndBoundedRetention(t *testing.T) {
	transport := errors.New("uncertain transport write")
	var entries atomic.Int32
	driver := &nha022Driver{onDispatch: func() error { entries.Add(1); return transport }}
	limits, err := integrations.NewVolatileCommandLimits(1, 4096, 9000)
	if err != nil {
		t.Fatal(err)
	}
	clock := &nha023ManualClock{now: time.Now()}
	rig := nha023Setup(t, driver, limits, clock)
	actor := nha022Actor(t)
	first, err := rig.service.Dispatch(context.Background(), actor, rig.request)
	if first == nil || first.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) {
		t.Fatalf("transport result = %v %v", first, err)
	}
	retry, err := rig.service.Dispatch(context.Background(), actor, rig.request)
	if retry == nil || !proto.Equal(first, retry) || !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) {
		t.Fatalf("uncertain retry = %v %v", retry, err)
	}
	if driver.count() != 0 { // onDispatch aborts before recording, but entered once
		t.Fatal("unexpected fake driver record")
	}
	if entries.Load() != 1 {
		t.Fatalf("transport was retried: %d", entries.Load())
	}
	status, err := rig.service.Status()
	if err != nil || status.Entries != 1 || status.RetainedBytes <= 0 || status.MaxEntries != 1 || status.MaxRetainedBytes != 9000 {
		t.Fatalf("ledger status = %+v %v", status, err)
	}
	second := proto.Clone(rig.request).(*actions.CommandRequest)
	second.CommandId = "new-command"
	if got, err := rig.service.Dispatch(context.Background(), actor, second); got != nil || !errors.Is(err, integrations.ErrVolatileLedgerFull) {
		t.Fatalf("quota did not refuse new intention: %v %v", got, err)
	}
	clock.Advance(2 * time.Hour)
	expired, err := rig.service.Dispatch(context.Background(), actor, rig.request)
	if err != nil || expired == nil || expired.Error == nil || expired.Error.FieldPath != "expires_at" || entries.Load() != 1 {
		t.Fatalf("retry past expiry = %v %v, entries=%d", expired, err, entries.Load())
	}
	changed := proto.Clone(rig.request).(*actions.CommandRequest)
	changed.Endpoint = "changed"
	if got, err := rig.service.Dispatch(context.Background(), actor, changed); got != nil || !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatalf("quota weakened conflict: %v %v", got, err)
	}
}

func TestNHA023PilotPanicClosesPendingWithoutRedispatch(t *testing.T) {
	var entries atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	driver := &nha022Driver{onDispatch: func() error { entries.Add(1); close(entered); <-release; panic("simulated pilot fault") }}
	rig := nha023Setup(t, driver, integrations.DefaultVolatileCommandLimits())
	actor := nha022Actor(t)
	recovered := make(chan any, 1)
	go func() {
		defer func() {
			recovered <- recover()
		}()
		_, _ = rig.service.Dispatch(context.Background(), actor, rig.request)
	}()
	<-entered
	followerRouter, err := integrations.NewCommandRouter(rig.host, rig.gen)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	follower := make(chan outcome, 1)
	observed := &nha023ObservedContext{Context: context.Background(), reached: make(chan struct{})}
	go func() {
		response, err := followerRouter.Dispatch(observed, actor, rig.request)
		follower <- outcome{response, err}
	}()
	<-observed.reached
	close(release)
	if got := <-recovered; got == nil {
		t.Fatal("pilot panic was swallowed")
	}
	joined := <-follower
	if joined.response == nil || joined.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || !errors.Is(joined.err, integrations.ErrCommandOutcomeIndeterminate) {
		t.Fatalf("pending follower after panic = %v %v", joined.response, joined.err)
	}
	got, err := rig.service.Dispatch(context.Background(), actor, rig.request)
	if got == nil || got.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || entries.Load() != 1 {
		t.Fatalf("retry after pilot panic = %v %v, entries=%d", got, err, entries.Load())
	}
}

func TestNHA023LeaderCancellationAfterDriverEntryIsRecorded(t *testing.T) {
	var entries atomic.Int32
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{}), onDispatch: func() error { entries.Add(1); return nil }}
	rig := nha023Setup(t, driver, integrations.DefaultVolatileCommandLimits())
	actor := nha022Actor(t)
	leaderCtx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	leader := make(chan outcome, 1)
	go func() {
		response, err := rig.service.Dispatch(leaderCtx, actor, rig.request)
		leader <- outcome{response, err}
	}()
	<-driver.entered
	observed := &nha023ObservedContext{Context: context.Background(), reached: make(chan struct{})}
	follower := make(chan outcome, 1)
	go func() {
		response, err := rig.service.Dispatch(observed, actor, rig.request)
		follower <- outcome{response, err}
	}()
	<-observed.reached
	cancel()
	for _, ch := range []chan outcome{leader, follower} {
		result := <-ch
		if result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || !errors.Is(result.err, integrations.ErrCommandOutcomeIndeterminate) {
			t.Fatalf("cancel after pilot entry = %v %v", result.response, result.err)
		}
	}
	retry, err := rig.service.Dispatch(context.Background(), actor, rig.request)
	if retry == nil || retry.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || !errors.Is(err, integrations.ErrCommandOutcomeIndeterminate) || entries.Load() != 1 {
		t.Fatalf("cancelled leader retry = %v %v, entries=%d", retry, err, entries.Load())
	}
}

func TestNHA023FollowerDeadlineWhenPilotIgnoresContext(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	driver := &nha022Driver{onDispatch: func() error { close(entered); <-release; return nil }}
	rig := nha023Setup(t, driver, integrations.DefaultVolatileCommandLimits())
	rig.request.ExpiresAt = timestamppb.New(time.Now().Add(time.Second))
	actor := nha022Actor(t)
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	leader := make(chan outcome, 1)
	go func() {
		response, err := rig.service.Dispatch(context.Background(), actor, rig.request)
		leader <- outcome{response, err}
	}()
	<-entered
	observed := &nha023ObservedContext{Context: context.Background(), reached: make(chan struct{})}
	follower := make(chan outcome, 1)
	go func() {
		response, err := rig.service.Dispatch(observed, actor, rig.request)
		follower <- outcome{response, err}
	}()
	<-observed.reached
	select {
	case result := <-follower:
		if result.err != nil || result.response == nil || result.response.Error == nil || result.response.Error.FieldPath != "expires_at" {
			t.Fatalf("follower deadline = %v %v", result.response, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower waited past its expiry for an uncooperative pilot")
	}
	close(release)
	result := <-leader
	if result.err != nil || result.response == nil || result.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.count() != 1 {
		t.Fatalf("pilot completion = %v %v, calls=%d", result.response, result.err, driver.count())
	}
}

func TestNHA023AttachAfterRawDispatchIsRefused(t *testing.T) {
	driver := &nha022Driver{entered: make(chan struct{}), release: make(chan struct{})}
	rig := nha023SetupBare(t, driver)
	router, err := integrations.NewCommandRouter(rig.host, rig.gen)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	actor := nha022Actor(t)
	go func() { _, err := router.Dispatch(context.Background(), actor, rig.request); done <- err }()
	<-driver.entered
	if _, err := integrations.NewVolatileCommandService(rig.host, rig.gen, integrations.DefaultVolatileCommandLimits()); !errors.Is(err, integrations.ErrVolatileServiceAttached) {
		t.Fatalf("attached ledger while a raw pilot ran: %v", err)
	}
	close(driver.release)
	if err := <-done; err != nil || driver.count() != 1 {
		t.Fatalf("raw dispatch = %v, calls=%d", err, driver.count())
	}
	if _, err := integrations.NewVolatileCommandService(rig.host, rig.gen, integrations.DefaultVolatileCommandLimits()); !errors.Is(err, integrations.ErrVolatileServiceAttached) {
		t.Fatalf("attached ledger after raw dispatch: %v", err)
	}
}

func TestNHA023IntentAndByteQuotas(t *testing.T) {
	limits, err := integrations.NewVolatileCommandLimits(10, 4096, 5632)
	if err != nil {
		t.Fatal(err)
	}
	rig := nha023Setup(t, &nha022Driver{}, limits)
	actor := nha022Actor(t)
	tooLarge := proto.Clone(rig.request).(*actions.CommandRequest)
	tooLarge.CapabilitySchemaId = strings.Repeat("x", 128<<10)
	if got, err := rig.service.Dispatch(context.Background(), actor, tooLarge); got != nil || !errors.Is(err, integrations.ErrCommandIntentTooLarge) {
		t.Fatalf("oversized intention = %v %v", got, err)
	}
	if got, err := rig.service.Dispatch(context.Background(), actor, rig.request); err != nil || got.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("first intention = %v %v", got, err)
	}
	if got, err := rig.service.Dispatch(context.Background(), actor, tooLarge); got != nil || !errors.Is(err, integrations.ErrCommandIDConflict) {
		t.Fatalf("oversized reuse of admitted ID = %v %v", got, err)
	}
	full := false
	for i := 2; i <= 10; i++ {
		second := proto.Clone(rig.request).(*actions.CommandRequest)
		second.CommandId = "byte-quota-command-" + string(rune('a'+i))
		got, err := rig.service.Dispatch(context.Background(), actor, second)
		if errors.Is(err, integrations.ErrVolatileLedgerFull) {
			if got != nil {
				t.Fatalf("full ledger returned response: %v", got)
			}
			full = true
			break
		}
		if err != nil || got == nil || got.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("byte quota iteration %d = %v %v", i, got, err)
		}
	}
	status, err := rig.service.Status()
	if err != nil || !full || status.Entries < 1 || status.Entries >= status.MaxEntries || status.MaxEntries != 10 || rig.driver.count() != status.Entries {
		t.Fatalf("byte quota status = %+v %v, calls=%d", status, err, rig.driver.count())
	}
}
