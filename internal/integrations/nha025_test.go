package integrations_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
)

type nha025Driver struct {
	mu    sync.Mutex
	calls int
	call  func(context.Context, integrations.AdmittedCommand) error
}

func (d *nha025Driver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	if d.call != nil {
		return d.call(ctx, command)
	}
	return nil
}
func (d *nha025Driver) Count() int { d.mu.Lock(); defer d.mu.Unlock(); return d.calls }

type nha025Rig struct {
	host    *integrations.Manager
	fixture *fake.Fixture
	session *integrations.Session
	binding integrations.Binding
	request *actions.CommandRequest
	service *integrations.VolatileCommandService
	driver  *nha025Driver
}

func nha025Setup(t *testing.T, driver *nha025Driver, commandClock integrations.CommandClock) nha025Rig {
	t.Helper()
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance := nha016Instance(t, "nha025-relay")
	key, err := integrations.NewSourceKey("fixture-controller", "relay")
	if err != nil {
		t.Fatal(err)
	}
	identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{nha022Record(t, identities[0], instance, key, "switch.nha025_old", "switch.nha025", false)}); err != nil {
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
	binding, err := fakeDriver.Binding()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.AttachCommandDriver(session, driver); err != nil {
		t.Fatal(err)
	}
	start, err := fixture.Sequencer().Start()
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := start.Components()
	if err != nil {
		t.Fatal(err)
	}
	request := nha022Request(t, identities[0].Entity, epoch, contracts.SwitchSchema, "relay", nha022Switch())
	var service *integrations.VolatileCommandService
	if commandClock == nil {
		service, err = integrations.NewVolatileCommandService(host, &nha022Generation{revision: 7}, integrations.DefaultVolatileCommandLimits())
	} else {
		service, err = integrations.NewVolatileCommandServiceWithClock(host, &nha022Generation{revision: 7}, commandClock, integrations.DefaultVolatileCommandLimits())
	}
	if err != nil {
		t.Fatal(err)
	}
	return nha025Rig{host, fixture, session, binding, request, service, driver}
}
func nha025ID(t *testing.T, request *actions.CommandRequest) domain.CommandID {
	t.Helper()
	id, err := domain.ParseCommandID(request.CommandId)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func nha025Status(t *testing.T, host *integrations.Manager, id domain.CommandID, want actions.ActionStatus) integrations.ActionSnapshot {
	t.Helper()
	snapshot, err := host.ActionStatus(id)
	if err != nil || snapshot.Status != want {
		t.Fatalf("progress = %+v, %v; want %v", snapshot, err, want)
	}
	return snapshot
}
func nha025Publish(t *testing.T, rig nha025Rig, on bool, causality *domain.Causality, quality domain.Quality) error {
	t.Helper()
	value := domain.NewSwitchValue(on)
	at := time.Now()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if causality != nil {
		observation, err = observation.WithCausality(*causality)
		if err != nil {
			t.Fatal(err)
		}
	}
	return rig.session.Publish(context.Background(), rig.binding, observation)
}

func TestNHA025AckRequiresCorrelatedFreshCompatibleState(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	id := nha025ID(t, rig.request)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	stale, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceObserved)
	driver.call = func(ctx context.Context, command integrations.AdmittedCommand) error {
		if err := command.Acknowledge(); err != nil {
			return err
		}
		causality, err := command.Causality()
		if err != nil {
			return err
		}
		if err := nha025Publish(t, rig, false, &causality, quality); err != nil {
			return err
		}
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		if err := nha025Publish(t, rig, true, nil, quality); err != nil {
			return err
		}
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		foreign, _ := domain.ParseCorrelationID("other-command-token")
		wrongCause, _ := domain.NewCausality(nil, &foreign)
		if err := nha025Publish(t, rig, true, &wrongCause, quality); err != nil {
			return err
		}
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		value := domain.NewSwitchValue(true)
		at := time.Now()
		wrongEndpoint, err := integrations.NewStateObservation("other-endpoint", quality, &value, &at, at, nil)
		if err != nil {
			return err
		}
		wrongEndpoint, err = wrongEndpoint.WithCausality(causality)
		if err != nil {
			return err
		}
		if err := rig.session.Publish(ctx, rig.binding, wrongEndpoint); !errors.Is(err, integrations.ErrInvalidInput) {
			return errors.New("foreign endpoint admitted")
		}
		if err := nha025Publish(t, rig, true, &causality, stale); err != nil {
			return err
		}
		nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
		return nha025Publish(t, rig, true, &causality, quality)
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("dispatch=%v %v", response, err)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	replayed, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || !proto.Equal(response, replayed) || rig.driver.Count() != 1 {
		t.Fatalf("ledger mutated/replayed: %v %v", replayed, err)
	}
}

func TestNHA025OtherCommandProofCannotConfirmFirst(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	firstID := nha025ID(t, rig.request)
	second := proto.Clone(rig.request).(*actions.CommandRequest)
	second.CommandId = "command-2"
	secondID := nha025ID(t, second)
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		id, _, _, err := command.Identity()
		if err != nil {
			return err
		}
		if err := command.Acknowledge(); err != nil {
			return err
		}
		if id != secondID {
			return nil
		}
		causality, err := command.Causality()
		if err != nil {
			return err
		}
		return nha025Publish(t, rig, true, &causality, quality)
	}
	if _, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); err != nil {
		t.Fatal(err)
	}
	nha025Status(t, rig.host, firstID, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	if _, err := rig.service.Dispatch(context.Background(), nha022Actor(t), second); err != nil {
		t.Fatal(err)
	}
	nha025Status(t, rig.host, firstID, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	nha025Status(t, rig.host, secondID, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
}

func TestNHA025PartialStructuredStateNeedsQualifiedProtocolProof(t *testing.T) {
	on := true
	brightness := 50.0
	speed := "medium"
	cases := []struct {
		name, schema string
		description  *wireregistry.CapabilityDescription
		arguments    *actions.ActionArgs
	}{
		{"light brightness", contracts.LightSchema, &wireregistry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "relay", DisplayName: "Fake light", Constraints: &wireregistry.CapabilityDescription_Light{Light: &wireregistry.LightConstraints{SetOn: true, BrightnessPercent: &wireregistry.NumberBounds{Minimum: 0, Maximum: 100}}}}, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on, BrightnessPercent: &brightness}}}},
		{"fan speed", contracts.FanSchema, &wireregistry.CapabilityDescription{SchemaId: contracts.FanSchema, Major: 1, Endpoint: "relay", DisplayName: "Fake fan", Constraints: &wireregistry.CapabilityDescription_Fan{Fan: &wireregistry.FanConstraints{SetOn: true, SpeedOptions: []string{"medium"}}}}, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{On: &on, Speed: &speed}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			driver := &nha025Driver{}
			rig := nha025Setup(t, driver, nil)
			_, key, err := rig.binding.Source()
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{tc.description})
			if err != nil {
				t.Fatal(err)
			}
			bindings, err := rig.session.Discover(context.Background(), []integrations.Candidate{candidate})
			if err != nil || len(bindings) != 1 {
				t.Fatalf("replace capability: %v", err)
			}
			rig.binding = bindings[0]
			rig.request.CapabilitySchemaId = tc.schema
			rig.request.Arguments = tc.arguments
			id := nha025ID(t, rig.request)
			quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
			driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
				if err := command.Acknowledge(); err != nil {
					return err
				}
				cause, err := command.Causality()
				if err != nil {
					return err
				}
				if err := nha025Publish(t, rig, true, &cause, quality); err != nil {
					return err
				}
				nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
				return command.ConfirmProtocol(&nha025Proof{qualified: true})
			}
			response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
			if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatalf("dispatch=%v %v", response, err)
			}
			nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
		})
	}
}

type nha025Proof struct{ qualified bool }

func (p *nha025Proof) QualifyProtocolConfirmation(integrations.AdmittedCommand) error {
	if !p.qualified {
		return errors.New("protocol response only accepted the envelope")
	}
	return nil
}
func TestNHA025QualifiedProtocolAndOldSessionRejected(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	id := nha025ID(t, rig.request)
	var saved integrations.AdmittedCommand
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		saved = command
		var typedNil *nha025Proof
		if err := command.ConfirmProtocol(typedNil); !errors.Is(err, integrations.ErrInvalidInput) {
			return errors.New("typed nil protocol qualifier accepted")
		}
		if err := command.ConfirmProtocol(&nha025Proof{}); err == nil {
			return errors.New("unqualified response confirmed")
		}
		return command.Acknowledge()
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("dispatch=%v %v", response, err)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	instance, _, err := rig.binding.Source()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.host.OpenSession(instance); err != nil {
		t.Fatal(err)
	}
	if err := saved.ConfirmProtocol(&nha025Proof{qualified: true}); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("old session proof: %v", err)
	}
	if err := saved.Acknowledge(); !errors.Is(err, integrations.ErrInactiveSession) {
		t.Fatalf("old session ack: %v", err)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
}
func TestNHA025QualifiedProtocolConfirmsWithoutState(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		return command.ConfirmProtocol(&nha025Proof{qualified: true})
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("dispatch=%v %v", response, err)
	}
	nha025Status(t, rig.host, nha025ID(t, rig.request), actions.ActionStatus_ACTION_STATUS_CONFIRMED)
}

type nha025Clock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*nha025Timer
	armed  chan struct{}
}
type nha025Timer struct {
	at      time.Duration
	ch      chan time.Time
	stopped bool
	clock   *nha025Clock
}

func (c *nha025Clock) MonoNow() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *nha025Clock) NewTimer(d time.Duration) integrations.ActionTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &nha025Timer{at: c.now + d, ch: make(chan time.Time, 1), clock: c}
	c.timers = append(c.timers, t)
	if c.armed != nil {
		select {
		case c.armed <- struct{}{}:
		default:
		}
	}
	if d <= 0 {
		t.ch <- time.Now()
	}
	return t
}
func (t *nha025Timer) C() <-chan time.Time { return t.ch }
func (t *nha025Timer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}
func (c *nha025Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += d
	for _, timer := range c.timers {
		if !timer.stopped && c.now >= timer.at {
			select {
			case timer.ch <- time.Now():
			default:
			}
			timer.stopped = true
		}
	}
}
func (c *nha025Clock) ActiveTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := 0
	for _, timer := range c.timers {
		if !timer.stopped {
			active++
		}
	}
	return active
}

func nha025AwaitTimer(t *testing.T, clock *nha025Clock) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for clock.ActiveTimers() == 0 {
		select {
		case <-clock.armed:
		case <-deadline.C:
			t.Fatal("pending Next did not arm an active timer")
		}
	}
}

func TestNHA025AckTimeoutWakesWatchAndKeepsLedger(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	clock := &nha025Clock{armed: make(chan struct{}, 16)}
	if err := rig.host.SetActionClock(clock); err != nil {
		t.Fatal(err)
	}
	var saved integrations.AdmittedCommand
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		saved = command
		return command.Acknowledge()
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("dispatch=%v %v", response, err)
	}
	id := nha025ID(t, rig.request)
	sub, err := rig.host.SubscribeAction(id)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	initial, err := sub.Next(context.Background())
	if err != nil || initial.Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		t.Fatalf("initial=%+v %v", initial, err)
	}
	result := make(chan integrations.ActionSnapshot, 1)
	errCh := make(chan error, 1)
	go func() { got, err := sub.Next(context.Background()); result <- got; errCh <- err }()
	nha025AwaitTimer(t, clock)
	clock.Advance(2 * time.Minute)
	select {
	case got := <-result:
		if err := <-errCh; err != nil || got.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			t.Fatalf("timeout=%+v %v", got, err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle watch did not wake")
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
	causality, err := saved.Causality()
	if err != nil {
		t.Fatal(err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err := nha025Publish(t, rig, true, &causality, quality); err != nil {
		t.Fatal(err)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
	if _, err := sub.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal watch: %v", err)
	}
	replayed, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || !proto.Equal(response, replayed) || driver.Count() != 1 {
		t.Fatalf("dispatch ledger changed: %v %v", replayed, err)
	}
}

func TestNHA025CancelAfterPilotEntryDoesNotRevokeEffect(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	id := nha025ID(t, rig.request)
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		if err := command.Acknowledge(); err != nil {
			return err
		}
		snapshot, err := rig.host.CancelAction(id)
		if err != nil || snapshot.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			return errors.New("post-entry cancellation implied reversal")
		}
		if err := command.ConfirmProtocol(&nha025Proof{qualified: true}); !errors.Is(err, integrations.ErrActionTerminal) {
			return errors.New("cancelled command accepted late protocol proof")
		}
		return nil
	}
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.Count() != 1 {
		t.Fatalf("dispatch=%v %v", response, err)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
}

type nha025CancelClock struct {
	mu        sync.Mutex
	now       time.Time
	host      *integrations.Manager
	id        domain.CommandID
	cancelled bool
}

func (c *nha025CancelClock) Now() time.Time {
	c.mu.Lock()
	host, id, cancelled := c.host, c.id, c.cancelled
	now := c.now
	c.mu.Unlock()
	if host != nil && !cancelled {
		status, err := host.ActionStatus(id)
		if err == nil && status.Status == actions.ActionStatus_ACTION_STATUS_ACCEPTED {
			if _, err := host.CancelAction(id); err == nil {
				c.mu.Lock()
				c.cancelled = true
				c.mu.Unlock()
			}
		}
	}
	return now
}

func TestNHA025CancelBeforeDriverEntryStopsDispatch(t *testing.T) {
	driver := &nha025Driver{}
	clock := &nha025CancelClock{now: time.Now()}
	rig := nha025Setup(t, driver, clock)
	id := nha025ID(t, rig.request)
	clock.mu.Lock()
	clock.host, clock.id = rig.host, id
	clock.mu.Unlock()
	response, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
	if err != nil || response.Status != actions.ActionStatus_ACTION_STATUS_CANCELLED || driver.Count() != 0 {
		t.Fatalf("cancelled dispatch=%v %v, calls=%d", response, err, driver.Count())
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_CANCELLED)
}

func TestNHA025CancelSignalsDriverBeforeFutureStep(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	id := nha025ID(t, rig.request)
	firstStep := make(chan struct{})
	var mu sync.Mutex
	secondStep := 0
	driver.call = func(ctx context.Context, command integrations.AdmittedCommand) error {
		if err := command.Acknowledge(); err != nil {
			return err
		}
		close(firstStep) // RAM-only first step; no device output
		<-ctx.Done()
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() == nil {
			secondStep++
		}
		return ctx.Err()
	}
	done := make(chan struct{})
	var response *actions.CommandResponse
	var dispatchErr error
	go func() {
		response, dispatchErr = rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request)
		close(done)
	}()
	select {
	case <-firstStep:
	case <-time.After(time.Second):
		t.Fatal("fake driver did not reach first step")
	}
	cancelled, err := rig.host.CancelAction(id)
	if err != nil || cancelled.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatalf("post-entry cancellation=%+v %v", cancelled, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("driver context was not cancelled")
	}
	mu.Lock()
	steps := secondStep
	mu.Unlock()
	if steps != 0 || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE || dispatchErr == nil {
		t.Fatalf("future step=%d, dispatch=%v %v", steps, response, dispatchErr)
	}
	nha025Status(t, rig.host, id, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
}

func TestNHA025WatchOrderedWhenTimeoutRacesQueuedUpdates(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	clock := &nha025Clock{armed: make(chan struct{}, 16)}
	if err := rig.host.SetActionClock(clock); err != nil {
		t.Fatal(err)
	}
	id := nha025ID(t, rig.request)
	var sub *integrations.ActionSubscription
	driver.call = func(_ context.Context, command integrations.AdmittedCommand) error {
		var err error
		sub, err = rig.host.SubscribeAction(id)
		if err != nil {
			return err
		}
		return command.Acknowledge()
	}
	if _, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	clock.Advance(2 * time.Minute)
	want := []actions.ActionStatus{actions.ActionStatus_ACTION_STATUS_DISPATCHED, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED, actions.ActionStatus_ACTION_STATUS_INDETERMINATE}
	var last uint64
	for _, status := range want {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		got, err := sub.Next(ctx)
		cancel()
		if err != nil || got.Status != status || got.Sequence <= last {
			t.Fatalf("watch=%+v %v, want %v after %d", got, err, status, last)
		}
		last = got.Sequence
	}
	if _, err := sub.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("duplicate terminal update: %v", err)
	}
}

func TestNHA025WatchCloseReleasesPendingNext(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	clock := &nha025Clock{armed: make(chan struct{}, 16)}
	if err := rig.host.SetActionClock(clock); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); err != nil {
		t.Fatal(err)
	}
	sub, err := rig.host.SubscribeAction(nha025ID(t, rig.request))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := sub.Next(context.Background()); done <- err }()
	nha025AwaitTimer(t, clock)
	sub.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release Next")
	}
}

func TestNHA025SubscriberLimitRejectsWithoutEviction(t *testing.T) {
	driver := &nha025Driver{}
	rig := nha025Setup(t, driver, nil)
	if _, err := rig.service.Dispatch(context.Background(), nha022Actor(t), rig.request); err != nil {
		t.Fatal(err)
	}
	id := nha025ID(t, rig.request)
	var subscriptions []*integrations.ActionSubscription
	for i := 0; i < 16; i++ {
		sub, err := rig.host.SubscribeAction(id)
		if err != nil {
			t.Fatal(err)
		}
		subscriptions = append(subscriptions, sub)
	}
	if _, err := rig.host.SubscribeAction(id); !errors.Is(err, integrations.ErrActionSubscribersFull) {
		t.Fatalf("seventeenth subscription: %v", err)
	}
	for _, sub := range subscriptions {
		sub.Close()
	}
	if sub, err := rig.host.SubscribeAction(id); err != nil {
		t.Fatalf("closed slot not released: %v", err)
	} else {
		sub.Close()
	}
}
