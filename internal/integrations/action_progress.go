package integrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"sync"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
)

var (
	ErrActionUnknown         = errors.New("action has no live progression in this host epoch")
	ErrActionProgressFull    = errors.New("action progression is full")
	ErrActionSubscriberSlow  = errors.New("action subscriber fell behind")
	ErrActionSubscribersFull = errors.New("action subscriber limit reached")
	ErrActionTerminal        = errors.New("action progression is terminal")
)

// ActionPoint and ActionReceipt carry sequencer admission metadata. Their
// constructors validate shape, not authority: only the trusted Sink's receipt
// returned through Session.Publish reaches the private tracker.
type ActionPoint struct {
	sequence, revision uint64
	tick               time.Duration
	valid              bool
}

func NewActionPoint(sequence, revision uint64, tick time.Duration) (ActionPoint, error) {
	if tick < 0 {
		return ActionPoint{}, ErrInvalidInput
	}
	return ActionPoint{sequence, revision, tick, true}, nil
}

type ActionReceipt struct {
	point     ActionPoint
	state     domain.ObservedState
	instance  domain.IntegrationInstanceID
	source    domain.SourceStamp
	causality domain.Causality
	valid     bool
}

func NewActionReceipt(point ActionPoint, state domain.ObservedState, instance domain.IntegrationInstanceID, source domain.SourceStamp, causality domain.Causality) (ActionReceipt, error) {
	if !point.valid {
		return ActionReceipt{}, ErrInvalidInput
	}
	if _, _, _, _, revision, err := state.Components(); err != nil || revision != point.revision {
		return ActionReceipt{}, ErrInvalidInput
	}
	if _, err := instance.Value(); err != nil {
		return ActionReceipt{}, ErrInvalidInput
	}
	if _, _, err := source.Components(); err != nil {
		return ActionReceipt{}, ErrInvalidInput
	}
	if _, _, err := causality.Components(); err != nil {
		return ActionReceipt{}, ErrInvalidInput
	}
	return ActionReceipt{point, state, instance, source, causality, true}, nil
}

// ActionPointSource is read under Host's route fence, after sequencer route admission.
// ActionReceiptSink returns the receipt of that exact atomic state admission.
type ActionPointSource interface {
	CommandActionPoint(domain.EntityID) (ActionPoint, error)
}
type ActionReceiptSink interface {
	AcceptStateWithActionReceipt(context.Context, domain.EntityID, AcceptedState) (ActionReceipt, error)
}

// ActionClock is process-local and monotonic. Timers are stopped by Next.
type ActionClock interface {
	MonoNow() time.Duration
	NewTimer(time.Duration) ActionTimer
}
type ActionTimer interface {
	C() <-chan time.Time
	Stop() bool
}
type realActionClock struct{}

var actionClockOrigin = time.Now()

func (realActionClock) MonoNow() time.Duration { return time.Since(actionClockOrigin) }
func (realActionClock) NewTimer(d time.Duration) ActionTimer {
	return &realActionTimer{time.NewTimer(d)}
}

type realActionTimer struct{ timer *time.Timer }

func (t *realActionTimer) C() <-chan time.Time { return t.timer.C }
func (t *realActionTimer) Stop() bool          { return t.timer.Stop() }

// ActionSnapshot is an immutable live progression, separate from the cached
// CommandResponse that records only the dispatch attempt.
type ActionSnapshot struct {
	Command        domain.CommandID
	Status         actions.ActionStatus
	Sequence       uint64
	ConfigRevision uint64
}

type actionProofKind uint8

const (
	proofUnavailable actionProofKind = iota
	proofSwitch
	proofLight
	proofFan
)

type actionPredicate struct {
	kind          actionProofKind
	on            bool
	hasOn         bool
	brightness    float64
	hasBrightness bool
	colorTemp     uint32
	hasColorTemp  bool
	fanSpeed      domain.FanSpeed
	hasFanSpeed   bool
}

func predicateFor(request *actions.CommandRequest) actionPredicate {
	if request == nil || request.Arguments == nil {
		return actionPredicate{}
	}
	switch arg := request.Arguments.Arguments.(type) {
	case *actions.ActionArgs_SetSwitch:
		if request.CapabilitySchemaId == contracts.SwitchSchema && arg != nil && arg.SetSwitch != nil {
			return actionPredicate{kind: proofSwitch, on: arg.SetSwitch.On}
		}
	case *actions.ActionArgs_SetLight:
		if request.CapabilitySchemaId == contracts.LightSchema && arg != nil && arg.SetLight != nil {
			light := arg.SetLight
			if light.On != nil && light.BrightnessPercent == nil && light.ColorTempMireds == nil {
				return actionPredicate{kind: proofSwitch, on: *light.On}
			}
			if light.On != nil || light.BrightnessPercent != nil || light.ColorTempMireds != nil {
				predicate := actionPredicate{kind: proofLight}
				if light.On != nil {
					predicate.on, predicate.hasOn = *light.On, true
				}
				if light.BrightnessPercent != nil {
					predicate.brightness, predicate.hasBrightness = *light.BrightnessPercent, true
				}
				if light.ColorTempMireds != nil {
					predicate.colorTemp, predicate.hasColorTemp = *light.ColorTempMireds, true
				}
				return predicate
			}
		}
	case *actions.ActionArgs_SetFan:
		if request.CapabilitySchemaId == contracts.FanSchema && arg != nil && arg.SetFan != nil {
			predicate := actionPredicate{kind: proofFan}
			if arg.SetFan.On != nil {
				predicate.on, predicate.hasOn = *arg.SetFan.On, true
			}
			if arg.SetFan.Speed != nil {
				var err error
				predicate.fanSpeed, err = domain.ParseFanSpeed(*arg.SetFan.Speed)
				if err != nil {
					return actionPredicate{}
				}
				predicate.hasFanSpeed = true
			}
			return predicate
		}
	case *actions.ActionArgs_SetClimate, *actions.ActionArgs_Extension:
		return actionPredicate{}
	default:
		return actionPredicate{}
	}
	return actionPredicate{}
}

// A protocol qualifier is supplied by a trusted driver and must examine its
// actual response semantics. A capability description or nil Dispatch result
// is never a protocol confirmation.
type ProtocolConfirmation interface{ QualifyProtocolConfirmation(AdmittedCommand) error }

type actionEntry struct {
	snapshot         ActionSnapshot
	entity           domain.EntityID
	instance         domain.IntegrationInstanceID
	session          string
	endpoint, schema string
	correlation      domain.CorrelationID
	point            ActionPoint
	deadline         time.Duration
	entered          bool
	cancelDispatch   context.CancelFunc
	predicate        actionPredicate
	subs             map[*ActionSubscription]struct{}
}
type actionTracker struct {
	mu            sync.Mutex
	clock         ActionClock
	entries       map[domain.CommandID]*actionEntry
	byCorrelation map[domain.CorrelationID]*actionEntry
	retained      int
	subCount      int
}

func newActionTracker(clock ActionClock) *actionTracker {
	return &actionTracker{clock: clock, entries: make(map[domain.CommandID]*actionEntry), byCorrelation: make(map[domain.CorrelationID]*actionEntry)}
}

// SetActionClock is a trusted test/composition seam, usable before any lease.
func (h *Manager) SetActionClock(clock ActionClock) error {
	if h == nil || h.actions == nil || nilInterface(clock) || clock.MonoNow() < 0 {
		return ErrInvalidInput
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.actions.mu.Lock()
	defer h.actions.mu.Unlock()
	if len(h.actions.entries) != 0 {
		return ErrInvalidInput
	}
	h.actions.clock = clock
	return nil
}

func newActionCorrelation() (domain.CorrelationID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return domain.CorrelationID{}, err
	}
	return domain.ParseCorrelationID("cmd-" + hex.EncodeToString(raw[:]))
}

func (t *actionTracker) accept(id domain.CommandID, entity domain.EntityID, binding Binding, request *actions.CommandRequest, point ActionPoint, correlation domain.CorrelationID, remaining time.Duration) (*actionEntry, error) {
	if t == nil || request == nil || !point.valid || remaining <= 0 || request.Endpoint == "" || request.CapabilitySchemaId == "" {
		return nil, ErrInvalidInput
	}
	if _, err := id.Value(); err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := entity.Value(); err != nil {
		return nil, ErrInvalidInput
	}
	if _, boundEntity, err := binding.IDs(); err != nil || boundEntity != entity {
		return nil, ErrInvalidInput
	}
	if _, err := correlation.Value(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing := t.entries[id]; existing != nil {
		return nil, ErrCommandIDConflict
	}
	if t.byCorrelation[correlation] != nil {
		return nil, ErrInvalidInput
	}
	idText, _ := id.Value()
	entityText, _ := entity.Value()
	instanceText, _ := binding.instance.Value()
	correlationText, _ := correlation.Value()
	// Conservative allowance for both maps, one empty subscriber map and
	// allocator overhead; no request payload or protobuf is retained.
	cost := int(unsafe.Sizeof(actionEntry{})) + 1024 + len(idText) + len(entityText) + len(instanceText) + len(correlationText) + len(binding.session) + len(request.Endpoint) + len(request.CapabilitySchemaId)
	if len(t.entries) >= maxVolatileEntries || cost > 128<<20-t.retained {
		return nil, ErrActionProgressFull
	}
	entry := &actionEntry{snapshot: ActionSnapshot{Command: id, Status: actions.ActionStatus_ACTION_STATUS_ACCEPTED, Sequence: 1, ConfigRevision: request.ConfigRevision}, entity: entity, instance: binding.instance, session: binding.session, endpoint: request.Endpoint, schema: request.CapabilitySchemaId, correlation: correlation, point: point, deadline: t.clock.MonoNow() + remaining, predicate: predicateFor(request), subs: make(map[*ActionSubscription]struct{})}
	t.entries[id] = entry
	t.byCorrelation[correlation] = entry
	t.retained += cost
	return entry, nil
}

func terminalAction(status actions.ActionStatus) bool {
	switch status {
	case actions.ActionStatus_ACTION_STATUS_CONFIRMED, actions.ActionStatus_ACTION_STATUS_FAILED, actions.ActionStatus_ACTION_STATUS_CANCELLED, actions.ActionStatus_ACTION_STATUS_INDETERMINATE:
		return true
	case actions.ActionStatus_ACTION_STATUS_ACCEPTED, actions.ActionStatus_ACTION_STATUS_DISPATCHED, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED:
		return false
	case actions.ActionStatus_ACTION_STATUS_UNSPECIFIED:
		return false
	default:
		return false
	}
}

func (t *actionTracker) advance(e *actionEntry, status actions.ActionStatus) {
	if terminalAction(e.snapshot.Status) || e.snapshot.Status == status {
		return
	}
	e.snapshot.Status = status
	e.snapshot.Sequence++
	for sub := range e.subs {
		select {
		case sub.updates <- e.snapshot:
		default:
			sub.err = ErrActionSubscriberSlow
			close(sub.closed)
			delete(e.subs, sub)
			t.subCount--
		}
	}
}

func (t *actionTracker) expire(e *actionEntry) {
	if !terminalAction(e.snapshot.Status) && t.clock.MonoNow() >= e.deadline {
		if e.entered {
			t.advance(e, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
		} else {
			t.advance(e, actions.ActionStatus_ACTION_STATUS_CANCELLED)
		}
	}
}
func (t *actionTracker) begin(ctx context.Context, cancel context.CancelFunc, e *actionEntry) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expire(e)
	if ctx.Err() != nil || cancel == nil {
		t.advance(e, actions.ActionStatus_ACTION_STATUS_CANCELLED)
		return false
	}
	if e.snapshot.Status != actions.ActionStatus_ACTION_STATUS_ACCEPTED {
		return false
	}
	e.entered = true // the dispatch-call commitment barrier
	e.cancelDispatch = cancel
	t.advance(e, actions.ActionStatus_ACTION_STATUS_DISPATCHED)
	return true
}
func (t *actionTracker) abort(e *actionEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !e.entered {
		t.advance(e, actions.ActionStatus_ACTION_STATUS_CANCELLED)
	}
}
func (t *actionTracker) uncertain(e *actionEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.advance(e, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
}

func (t *actionTracker) finish(e *actionEntry) {
	t.mu.Lock()
	cancel := e.cancelDispatch
	e.cancelDispatch = nil // release parent context and values this epoch
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (t *actionTracker) acknowledge(e *actionEntry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expire(e)
	if !e.entered {
		return ErrInvalidInput
	}
	if terminalAction(e.snapshot.Status) && e.snapshot.Status != actions.ActionStatus_ACTION_STATUS_CONFIRMED {
		return ErrActionTerminal
	}
	if e.snapshot.Status == actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.advance(e, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED)
	}
	return nil
}
func (t *actionTracker) protocol(e *actionEntry) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expire(e)
	if !e.entered {
		return ErrInvalidInput
	}
	if terminalAction(e.snapshot.Status) {
		if e.snapshot.Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED {
			return nil
		}
		return ErrActionTerminal
	}
	t.advance(e, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	return nil
}

func (t *actionTracker) observe(binding Binding, endpoint, schema string, receipt ActionReceipt) {
	if !receipt.valid || endpoint == "" || schema == "" {
		return
	}
	_, correlation, err := receipt.causality.Components()
	if err != nil || correlation == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.byCorrelation[*correlation]
	if e == nil || !e.entered || terminalAction(e.snapshot.Status) || e.entity != binding.entity || e.instance != binding.instance || e.session != binding.session || e.endpoint != endpoint || e.schema != schema || e.point.sequence >= receipt.point.sequence || e.point.revision >= receipt.point.revision || e.point.tick > receipt.point.tick || receipt.instance != binding.instance {
		return
	}
	stateEntity, _, _, _, _, stateErr := receipt.state.Components()
	if stateErr != nil || stateEntity != binding.entity {
		return
	}
	session, _, err := receipt.source.Components()
	if err != nil || session != binding.session {
		return
	}
	value, ok := receipt.state.CriticalValue()
	if !ok {
		return
	}
	switch e.predicate.kind {
	case proofSwitch:
		on, isSwitch := value.Switch()
		if !isSwitch && e.schema == contracts.LightSchema {
			if light, isLight := value.Light(); isLight {
				on, _, _, err = light.Components()
				isSwitch = err == nil
			}
		}
		if isSwitch && on == e.predicate.on {
			t.expire(e)
			t.advance(e, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
		}
	case proofLight:
		light, isLight := value.Light()
		if !isLight {
			return
		}
		on, brightness, colorTemp, err := light.Components()
		if err != nil || (e.predicate.hasOn && on != e.predicate.on) ||
			(e.predicate.hasBrightness && (brightness == nil || math.Abs(*brightness-e.predicate.brightness) > 1e-6)) ||
			(e.predicate.hasColorTemp && (colorTemp == nil || *colorTemp != e.predicate.colorTemp)) {
			return
		}
		t.expire(e)
		t.advance(e, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	case proofFan:
		speed, isFan := value.Fan()
		if !isFan || (e.predicate.hasFanSpeed && speed != e.predicate.fanSpeed) ||
			(e.predicate.hasOn && (speed != domain.FanSpeedOff) != e.predicate.on) {
			return
		}
		t.expire(e)
		t.advance(e, actions.ActionStatus_ACTION_STATUS_CONFIRMED)
	case proofUnavailable:
	default:
	}
}

func (h *Manager) ActionStatus(id domain.CommandID) (ActionSnapshot, error) {
	if h == nil || h.actions == nil {
		return ActionSnapshot{}, ErrInvalidInput
	}
	if _, err := id.Value(); err != nil {
		return ActionSnapshot{}, err
	}
	t := h.actions
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[id]
	if e == nil {
		return ActionSnapshot{}, ErrActionUnknown
	}
	t.expire(e)
	return e.snapshot, nil
}

// CancelAction signals the in-flight driver to stop future steps. The driver
// must honor its Dispatch context and must not detach unsupervised work. A
// command past the entry barrier is indeterminate; no effect is rolled back.
func (h *Manager) CancelAction(id domain.CommandID) (ActionSnapshot, error) {
	if h == nil || h.actions == nil {
		return ActionSnapshot{}, ErrInvalidInput
	}
	if _, err := id.Value(); err != nil {
		return ActionSnapshot{}, err
	}
	t := h.actions
	t.mu.Lock()
	e := t.entries[id]
	if e == nil {
		t.mu.Unlock()
		return ActionSnapshot{}, ErrActionUnknown
	}
	t.expire(e)
	if !terminalAction(e.snapshot.Status) {
		if e.entered {
			t.advance(e, actions.ActionStatus_ACTION_STATUS_INDETERMINATE)
		} else {
			t.advance(e, actions.ActionStatus_ACTION_STATUS_CANCELLED)
		}
	}
	snapshot := e.snapshot
	cancel := e.cancelDispatch
	t.mu.Unlock()
	// context cancellation may wake or call driver code; never do it under a
	// Host, catalogue, sequencer or tracker lock.
	if cancel != nil {
		cancel()
	}
	return snapshot, nil
}

type ActionSubscription struct {
	nextMu        sync.Mutex
	tracker       *actionTracker
	entry         *actionEntry
	updates       chan ActionSnapshot
	closed        chan struct{}
	err           error  // guarded by tracker.mu
	lastDelivered uint64 // guarded by nextMu
}

func (h *Manager) SubscribeAction(id domain.CommandID) (*ActionSubscription, error) {
	if h == nil || h.actions == nil {
		return nil, ErrInvalidInput
	}
	if _, err := id.Value(); err != nil {
		return nil, err
	}
	t := h.actions
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[id]
	if e == nil {
		return nil, ErrActionUnknown
	}
	if t.subCount >= 1024 || len(e.subs) >= 16 {
		return nil, ErrActionSubscribersFull
	}
	t.expire(e)
	s := &ActionSubscription{tracker: t, entry: e, updates: make(chan ActionSnapshot, 4), closed: make(chan struct{})}
	e.subs[s] = struct{}{}
	t.subCount++
	s.updates <- e.snapshot
	return s, nil
}
func (s *ActionSubscription) Next(ctx context.Context) (ActionSnapshot, error) {
	if s == nil || s.tracker == nil || ctx == nil {
		return ActionSnapshot{}, ErrInvalidInput
	}
	s.nextMu.Lock()
	defer s.nextMu.Unlock()
	t := s.tracker
	for {
		t.mu.Lock()
		err := s.err
		terminal := terminalAction(s.entry.snapshot.Status)
		remaining := s.entry.deadline - t.clock.MonoNow()
		t.mu.Unlock()
		if err != nil {
			return ActionSnapshot{}, err
		}
		if terminal && len(s.updates) == 0 {
			return ActionSnapshot{}, io.EOF
		}
		var timeout <-chan time.Time
		var timer ActionTimer
		if !terminal {
			timer = t.clock.NewTimer(max(remaining, 0))
			if nilInterface(timer) {
				return ActionSnapshot{}, ErrInvalidInput
			}
			timeout = timer.C()
		}
		var snapshot ActionSnapshot
		var delivered bool
		select {
		case snapshot = <-s.updates:
			delivered = true
		case <-s.closed:
		case <-timeout:
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ActionSnapshot{}, ctx.Err()
		}
		if timer != nil {
			timer.Stop()
		}
		t.mu.Lock()
		err = s.err
		if !delivered {
			t.expire(s.entry)
		}
		t.mu.Unlock()
		if err != nil {
			return ActionSnapshot{}, err
		}
		if delivered && snapshot.Sequence > s.lastDelivered {
			s.lastDelivered = snapshot.Sequence
			return snapshot, nil
		}
	}
}
func (s *ActionSubscription) Close() {
	if s == nil || s.tracker == nil {
		return
	}
	t := s.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := s.entry.subs[s]; ok {
		delete(s.entry.subs, s)
		t.subCount--
		s.err = context.Canceled
		close(s.closed)
	}
}

// A correlated structured light observation can prove exactly the requested
// fields. Other actions still require a complete, qualified proof.
