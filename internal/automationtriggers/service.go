package automationtriggers

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrServiceClosed = errors.New("automation triggers: service closed")

const (
	MaxFiredQueue      = 128
	MaxOutputBytes     = 1 << 20
	MaxEngineCharge    = 8 << 20
	runtimeFixedCharge = 128 << 10
	maxReplayEntries   = 256
)

type ResultKind uint8

const (
	ResultInvalid ResultKind = iota
	ResultFired
	ResultMissed
	ResultDegraded
)

type DegradedReason uint8

const (
	DegradedReasonInvalid DegradedReason = iota
	DegradedGap
	DegradedCapacity
	DegradedSource
)

// Result is an observation for the future run orchestrator. Reading a fired
// result grants no command, scene, or script capability.
type Result struct {
	kind                   ResultKind
	reason                 DegradedReason
	automation, triggerKey string
	at                     time.Time
	cursor                 sequencer.Cursor
	entity                 domain.EntityID
	payload                []byte
}

func (r Result) Kind() ResultKind         { return r.kind }
func (r Result) Reason() DegradedReason   { return r.reason }
func (r Result) Automation() string       { return r.automation }
func (r Result) TriggerKey() string       { return r.triggerKey }
func (r Result) At() time.Time            { return r.at }
func (r Result) Cursor() sequencer.Cursor { return r.cursor }
func (r Result) Entity() domain.EntityID  { return r.entity }
func (r Result) Payload() []byte          { return append([]byte(nil), r.payload...) }

type pendingFor struct {
	deadline         time.Duration
	generation       uint64
	oldText, newText string
}

type mqttHighWater struct {
	session string
	seq     uint64
}

type calendarKey struct {
	source                 CalendarSourceID
	uid, instance, trigger string
	edge                   uint8
}

type Service struct {
	mu                                                   sync.Mutex
	plan                                                 Plan
	seq                                                  *sequencer.Sequencer
	clock                                                Clock
	ctx                                                  context.Context
	cancel                                               context.CancelFunc
	shutdownCtx                                          context.Context
	requestShutdown                                      context.CancelFunc
	stopShutdownBridge                                   func() bool
	sunDone                                              chan struct{}
	startDone                                            chan struct{}
	cancelSun                                            context.CancelFunc
	starting, started, closed, admissionsOpen, suspended bool
	planning                                             bool
	suspendReason                                        DegradedReason
	generation                                           uint64
	sub                                                  *sequencer.Subscription
	cursor                                               sequencer.Cursor
	states                                               map[domain.EntityID]domain.ObservedState
	registry                                             map[domain.EntityID]integrations.RegistryRecord
	stateCharges                                         map[domain.EntityID]int
	recordCharges                                        map[domain.EntityID]int
	projectionBytes, replayBytes                         int
	armed                                                map[int]bool
	numericUnits                                         map[int]domain.Unit
	pending                                              map[int]pendingFor
	civil                                                map[int]string
	civilUTC                                             map[int]time.Time
	mqttWater                                            map[int]mqttHighWater
	windows                                              map[CalendarSourceID]CalendarWindow
	calendarDegraded                                     map[CalendarSourceID]bool
	seen                                                 map[calendarKey]struct{}
	seenBytes                                            int
	scheduled                                            map[int]time.Time
	scheduledMono                                        map[int]time.Duration
	lastWall                                             time.Time
	results                                              []Result
	firedQueued, resultBytes                             int
	wake, resultWake, recoveryWake                       chan struct{}
	readerDone, timerDone                                chan struct{}
	joinedDone                                           chan struct{}
	joinOnce, asyncCloseOnce                             sync.Once
	err                                                  error
}

func NewService(plan Plan, seq *sequencer.Sequencer, clock Clock) (*Service, error) {
	if !plan.Valid() || seq == nil || nilInterface(clock) || clock.Now().IsZero() {
		return nil, ErrInvalidPlan
	}
	shutdownCtx, requestShutdown := context.WithCancel(context.Background())
	return &Service{plan: plan, seq: seq, clock: clock, generation: 1,
		shutdownCtx: shutdownCtx, requestShutdown: requestShutdown,
		states: make(map[domain.EntityID]domain.ObservedState), registry: make(map[domain.EntityID]integrations.RegistryRecord),
		stateCharges: make(map[domain.EntityID]int), recordCharges: make(map[domain.EntityID]int),
		armed: make(map[int]bool), numericUnits: make(map[int]domain.Unit), pending: make(map[int]pendingFor), civil: make(map[int]string), civilUTC: make(map[int]time.Time),
		mqttWater: make(map[int]mqttHighWater), windows: make(map[CalendarSourceID]CalendarWindow),
		seen: make(map[calendarKey]struct{}), calendarDegraded: make(map[CalendarSourceID]bool), scheduled: make(map[int]time.Time),
		scheduledMono: make(map[int]time.Duration),
		wake:          make(chan struct{}, 1), resultWake: make(chan struct{}, 1), recoveryWake: make(chan struct{}, 1),
		readerDone: make(chan struct{}), timerDone: make(chan struct{}), joinedDone: make(chan struct{}), admissionsOpen: true}, nil
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || nilInterface(ctx) {
		return ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.starting || s.started || s.closed {
		s.mu.Unlock()
		return ErrInvalidPlan
	}
	s.starting = true
	s.startDone = make(chan struct{})
	defer func() {
		s.starting = false
		close(s.startDone)
		s.mu.Unlock()
	}()
	snapshotLimit := MaxEngineCharge - sequencer.MaxViewQueueBytes - runtimeFixedCharge - s.plan.charge - 256
	if snapshotLimit < 512 {
		return ErrCapacity
	}
	snapshot, err := s.seq.CaptureTriggerView(snapshotLimit)
	if err != nil {
		return err
	}
	replayLimit := snapshotLimit - snapshot.EstimatedBytes()
	if replayLimit <= 0 {
		return ErrCapacity
	}
	sub, replay, _, replayBytes, err := s.seq.SubscribeTriggerViewAfter(snapshot.Cursor, maxReplayEntries, replayLimit)
	if err != nil {
		return err
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.stopShutdownBridge = context.AfterFunc(s.shutdownCtx, s.cancel)
	s.sub = sub
	s.installSnapshotLocked(snapshot)
	s.replayBytes = replayBytes
	for _, entry := range replay {
		s.processEntryLocked(entry)
	}
	s.replayBytes = 0
	s.scheduleAllLocked()
	if s.closed {
		return ErrServiceClosed
	}
	if err := s.ctx.Err(); err != nil {
		s.sub.Close()
		s.cancel()
		if s.stopShutdownBridge != nil {
			s.stopShutdownBridge()
		}
		s.closed, s.admissionsOpen = true, false
		close(s.readerDone)
		close(s.timerDone)
		return err
	}
	s.started = true
	go s.readLoop()
	go s.timerLoop()
	return nil
}

func (s *Service) installSnapshotLocked(snapshot sequencer.TriggerSnapshot) {
	s.states = snapshot.States
	s.registry = snapshot.Registry
	s.stateCharges = snapshot.StateCharges
	s.recordCharges = snapshot.RecordCharges
	s.projectionBytes = snapshot.EstimatedBytes()
	s.cursor = snapshot.Cursor
	clear(s.pending)
	clear(s.armed)
	clear(s.numericUnits)
	for i, r := range s.plan.rules {
		if r.trigger.Kind() == ir.TriggerNumericState && r.entity != (domain.EntityID{}) {
			if state, ok := s.states[r.entity]; ok {
				if unit, ok := numericMeasurementUnit(r.trigger, state); ok {
					s.numericUnits[i] = unit
				}
				_, matching, valid := numericMatch(r.trigger, state)
				if valid {
					s.armed[i] = !matching
				}
			}
		}
	}
	if now := s.clock.Now().UTC(); now.After(s.lastWall) {
		s.lastWall = now
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) readLoop() {
	defer close(s.readerDone)
	for {
		s.mu.Lock()
		sub, ctx := s.sub, s.ctx
		s.mu.Unlock()
		if sub == nil || ctx.Err() != nil {
			return
		}
		delivery, err := sub.Next(ctx)
		if err != nil || !delivery.ReplayValid() {
			if ctx.Err() != nil {
				return
			}
			for {
				s.mu.Lock()
				capacityWait := s.suspended && s.suspendReason == DegradedCapacity &&
					(s.firedQueued > MaxFiredQueue/2 || s.resultBytes > MaxOutputBytes/2)
				s.mu.Unlock()
				if !capacityWait {
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-s.recoveryWake:
				}
			}
			if err := s.resynchronize(sub); err != nil {
				s.mu.Lock()
				s.err = err
				s.suspendLocked(DegradedSource)
				s.mu.Unlock()
				return
			}
			continue
		}
		entry, ok := delivery.Entry()
		if !ok {
			continue
		}
		s.mu.Lock()
		if !s.closed && s.admissionsOpen && !s.suspended {
			s.processEntryLocked(entry)
		}
		s.mu.Unlock()
	}
}

func (s *Service) resynchronize(old *sequencer.Subscription) error {
	old.Close()
	s.mu.Lock()
	if s.cancelSun != nil {
		s.cancelSun()
	}
	s.suspendLocked(DegradedGap)
	s.states = nil
	s.registry = nil
	s.stateCharges = nil
	s.recordCharges = nil
	s.projectionBytes = 0
	s.mu.Unlock()
	s.mu.Lock()
	snapshotLimit := MaxEngineCharge - sequencer.MaxViewQueueBytes - runtimeFixedCharge - s.plan.charge - s.seenBytes - s.resultBytes - s.occurrenceCountLocked()*calendarOccurrenceCharge - 256
	s.mu.Unlock()
	if snapshotLimit < 512 {
		return ErrCapacity
	}
	snapshot, err := s.seq.CaptureTriggerView(snapshotLimit)
	if err != nil {
		return err
	}
	replayLimit := snapshotLimit - snapshot.EstimatedBytes()
	if replayLimit <= 0 {
		return ErrCapacity
	}
	sub, replay, _, replayBytes, err := s.seq.SubscribeTriggerViewAfter(snapshot.Cursor, maxReplayEntries, replayLimit)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		sub.Close()
		return ErrServiceClosed
	}
	s.sub = sub
	s.generation++
	s.suspended = false
	s.suspendReason = DegradedReasonInvalid
	for source := range s.windows {
		s.calendarDegraded[source] = true
	}
	s.installSnapshotLocked(snapshot)
	s.replayBytes = replayBytes
	if s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes) > MaxEngineCharge-256 {
		s.suspendLocked(DegradedCapacity)
		s.mu.Unlock()
		return ErrCapacity
	}
	for _, entry := range replay {
		s.processEntryLocked(entry)
	}
	s.replayBytes = 0
	s.scheduleAllLocked()
	s.mu.Unlock()
	return nil
}

func (s *Service) CloseAdmissions() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.admissionsOpen = false
	if s.cancelSun != nil {
		s.cancelSun()
	}
	clear(s.pending)
	clear(s.scheduled)
	clear(s.scheduledMono)
	s.mu.Unlock()
	s.signalWake()
}

func (s *Service) Close() error {
	if s == nil {
		return ErrInvalidPlan
	}
	defer s.joinOnce.Do(func() { close(s.joinedDone) })
	// Request cancellation before taking mu so a cooperative source callback
	// can return even when a concurrent planner is between lock transitions.
	if s.requestShutdown != nil {
		s.requestShutdown()
	}
	s.mu.Lock()
	if s.closed {
		startDone, started := s.startDone, s.started
		s.mu.Unlock()
		if startDone != nil {
			<-startDone
		}
		if started {
			<-s.readerDone
			<-s.timerDone
		}
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return err
	}
	s.closed, s.admissionsOpen = true, false
	if s.cancelSun != nil {
		s.cancelSun()
	}
	clear(s.pending)
	clear(s.scheduled)
	clear(s.scheduledMono)
	if s.cancel != nil {
		s.cancel()
	}
	if s.stopShutdownBridge != nil {
		s.stopShutdownBridge()
	}
	if s.sub != nil {
		s.sub.Close()
	}
	started := s.started
	startDone := s.startDone
	if !started {
		close(s.readerDone)
		close(s.timerDone)
	}
	s.mu.Unlock()
	s.signalWake()
	select {
	case s.resultWake <- struct{}{}:
	default:
	}
	if startDone != nil {
		<-startDone
	}
	if started {
		<-s.readerDone
		<-s.timerDone
	}
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	return err
}

func (s *Service) Error() error {
	if s == nil {
		return ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Service) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.joinedDone
}

// ReaderDone reports a reader failure before the rest of the service has
// joined. Run watches this signal; Done is reserved for full shutdown.
func (s *Service) ReaderDone() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.readerDone
}

// BeginClose requests one asynchronous join. A source callback can delay the
// join after Run's stop deadline, so Done remains open until it really exits.
func (s *Service) BeginClose() <-chan struct{} {
	if s == nil {
		return nil
	}
	s.asyncCloseOnce.Do(func() { go func() { _ = s.Close() }() })
	return s.Done()
}

func (s *Service) NextResult(ctx context.Context) (Result, error) {
	if s == nil || nilInterface(ctx) {
		return Result{}, ErrInvalidPlan
	}
	for {
		s.mu.Lock()
		if len(s.results) != 0 {
			result := s.results[0]
			s.results[0] = Result{}
			s.results = s.results[1:]
			s.resultBytes -= resultCharge(result)
			if result.kind == ResultFired {
				s.firedQueued--
			}
			if s.suspended && s.suspendReason == DegradedCapacity &&
				s.firedQueued <= MaxFiredQueue/2 && s.resultBytes <= MaxOutputBytes/2 {
				select {
				case s.recoveryWake <- struct{}{}:
				default:
				}
			}
			s.mu.Unlock()
			return result, nil
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return Result{}, ErrServiceClosed
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-s.resultWake:
		}
	}
}

func resultCharge(result Result) int {
	return 128 + len(result.automation) + len(result.triggerKey) + len(result.payload)
}

func (s *Service) enqueueLocked(result Result) {
	charge := resultCharge(result)
	if s.estimatedChargeLocked(s.occurrenceCountLocked(), s.seenBytes)+charge > MaxEngineCharge-256 {
		s.suspendLocked(DegradedCapacity)
		return
	}
	if result.kind == ResultFired {
		if len(result.payload) > 64<<10 || s.firedQueued >= MaxFiredQueue || charge > MaxOutputBytes-256-s.resultBytes {
			s.suspendLocked(DegradedCapacity)
			return
		}
		result.payload = append([]byte(nil), result.payload...)
		s.firedQueued++
	} else if charge > MaxOutputBytes-256-s.resultBytes {
		s.suspendLocked(DegradedCapacity)
		return
	}
	s.results = append(s.results, result)
	s.resultBytes += charge
	select {
	case s.resultWake <- struct{}{}:
	default:
	}
}

func (s *Service) suspendLocked(reason DegradedReason) {
	if s.suspended {
		return
	}
	s.suspended = true
	s.suspendReason = reason
	clear(s.pending)
	clear(s.scheduled)
	clear(s.scheduledMono)
	if s.sub != nil {
		s.sub.Close()
	}
	result := Result{kind: ResultDegraded, reason: reason, at: s.clock.Now().UTC()}
	s.results = append(s.results, result)
	s.resultBytes += resultCharge(result)
	select {
	case s.resultWake <- struct{}{}:
	default:
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) signalWake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
