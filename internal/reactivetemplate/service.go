package reactivetemplate

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
)

type groupReplacement struct {
	groups []bundle.Group
	result chan error
}

type planReplacement struct {
	plan   Plan
	result chan planReplacementResult
}

type planReplacementResult struct {
	applied bool
	err     error
}

// Service owns one lossless View subscription and one render loop. Its output
// queue never invokes a consumer while holding a sequencer or service lock.
type Service struct {
	controls                        chan templateControl
	waitHandles                     map[ProgramID]*WaitHandle
	permanentPrograms, controlSlots int
	sequence                        *sequencer.Sequencer
	plan                            Plan
	clock                           Clock
	ctx                             context.Context
	cancel                          context.CancelFunc
	updates                         chan Update
	replacements                    chan groupReplacement
	planReplacements                chan planReplacement
	reservedPlan                    *PlanReservation
	done                            chan struct{}
	mu                              sync.Mutex
	closed                          bool
	err                             error // historical last diagnostic; never cleared by recovery
	fatalErr                        error // first non-continuity error; bounded and never healed by recapture
	continuityErr                   error // current stream discontinuity, until a complete successful rebase
	groupGeneration                 atomic.Uint64
	publicationGeneration           atomic.Uint64
}

func Start(ctx context.Context, sequence *sequencer.Sequencer, plan Plan, clock Clock) (*Service, error) {
	if nilReactiveValue(ctx) || sequence == nil || !plan.Active() || plan.zone == nil || nilClock(clock) {
		return nil, ErrInvalidPlan
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	owned, err := NewPlan(plan.definitions, plan.groups, plan.zone)
	if err != nil {
		return nil, err
	}
	owned.waitsEnabled = plan.waitsEnabled
	plan = owned
	runCtx, cancel := context.WithCancel(ctx)
	s := &Service{sequence: sequence, plan: plan, clock: clock, ctx: runCtx, cancel: cancel,
		controls: make(chan templateControl, 128), waitHandles: make(map[ProgramID]*WaitHandle), updates: make(chan Update, MaxPrograms), replacements: make(chan groupReplacement), planReplacements: make(chan planReplacement), done: make(chan struct{})}
	s.permanentPrograms = len(plan.definitions)
	s.groupGeneration.Store(1)
	e := &engine{service: s, outputs: make(map[ProgramID]Update), readers: make(map[ProgramID][]template.Dependency)}
	changes, err := e.rebase()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := s.emit(changes); err != nil {
		_ = e.sub.Close()
		cancel()
		return nil, err
	}
	go s.run(e)
	return s, nil
}

func (s *Service) Updates() <-chan Update {
	if s == nil {
		return nil
	}
	return s.updates
}
func (s *Service) Done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.done
}
func (s *Service) GroupGeneration() uint64 {
	if s == nil {
		return 0
	}
	return s.groupGeneration.Load()
}
func (s *Service) Error() error {
	if s == nil {
		return ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
func (s *Service) setError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.err = err
	if s.fatalErr == nil {
		s.fatalErr = err
	}
	s.mu.Unlock()
}

// UnrecoveredError reports defects still active at shutdown. Error retains the
// historical last diagnostic, and sequencer loss counters/DiagnosticLoss are
// unchanged. A repaired bounded replay gap is not a failed shutdown; evaluation,
// emission, forwarding and actual cancellation failures are never healed here.
func (s *Service) UnrecoveredError() error {
	if s == nil {
		return ErrInvalidPlan
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.fatalErr, s.continuityErr)
}
func (s *Service) setContinuityError(err error) {
	if err == nil {
		return
	}
	if !errors.Is(err, sequencer.ErrViewOverflow) && !errors.Is(err, sequencer.ErrCursorExpired) && !errors.Is(err, sequencer.ErrCursorInvalidated) && !errors.Is(err, sequencer.ErrProjectionInvalidated) {
		s.setError(err)
		return
	}
	s.mu.Lock()
	s.err = err
	s.continuityErr = err
	s.mu.Unlock()
}

// Called ONLY after engine.rebase has committed the complete capture/suffix/
// subscription handoff. Partial/failed rebase and failed publication cannot
// settle the old discontinuity. Publication generations and all existing
// continuity guards remain in rebase/emit rather than a separate retry path.
func (s *Service) publishRecovered(updates []Update) {
	if err := s.emit(updates); err != nil {
		s.setError(err)
		return
	}
	s.mu.Lock()
	if !s.closed && s.ctx.Err() == nil {
		s.continuityErr = nil
	}
	s.mu.Unlock()
}
func (s *Service) emit(updates []Update) error {
	// Ephemeral readers consume their own result; the permanent output queue
	// remains reserved for its one established publication consumer.
	permanent := make([]Update, 0, len(updates))
	newReadBytes := 0
	for _, update := range updates {
		update.publication = s.publicationGeneration.Load()
		if len(update.reads) > MaxReadsPerUpdate {
			return ErrWorkBudget
		}
		if !s.routeWaitUpdate(update) {
			newReadBytes += len(update.reads) * int(unsafe.Sizeof(template.Read{}))
			permanent = append(permanent, update)
		}
	}
	if newReadBytes > MaxQueuedReadBytes || len(permanent) > MaxPrograms {
		return ErrWorkBudget
	}
	if len(permanent) > cap(s.updates)-len(s.updates) {
		s.setError(ErrOutputFull)
		s.CloseAdmissions()
		return ErrOutputFull
	}
	for _, update := range permanent {
		select {
		case s.updates <- update:
		default:
			s.setError(ErrOutputFull)
			s.CloseAdmissions()
			return ErrOutputFull
		}
	}
	return nil
}

func (s *Service) CloseAdmissions() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
	}
	s.mu.Unlock()
}
func (s *Service) Close(ctx context.Context) error {
	if s == nil || nilReactiveValue(ctx) {
		return ErrInvalidPlan
	}
	s.CloseAdmissions()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReplaceGroups is a serialized definition generation, not an implicit YAML
// watcher. Callers supply admitted bundle groups, copied before the handoff.
func (s *Service) ReplaceGroups(ctx context.Context, groups []bundle.Group) error {
	if s == nil || nilReactiveValue(ctx) || len(groups) > 128 {
		return ErrInvalidPlan
	}
	copyGroups := make([]bundle.Group, len(groups))
	for i, group := range groups {
		if !group.Valid() {
			return ErrInvalidPlan
		}
		copyGroups[i] = group
	}
	s.mu.Lock()
	closed := s.closed
	reserved := s.reservedPlan != nil
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if reserved {
		return ErrWorkBudget
	}
	req := groupReplacement{groups: copyGroups, result: make(chan error, 1)}
	select {
	case s.replacements <- req:
	case <-s.ctx.Done():
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.result:
		return err
	case <-s.ctx.Done():
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

type engine struct {
	nextWait       uint64
	pendingControl *templateControl
	service        *Service
	snapshot       sequencer.Snapshot
	sub            *sequencer.Subscription
	outputs        map[ProgramID]Update
	readers        map[ProgramID][]template.Dependency
	watchAliases   map[domain.HAAlias]struct{}
	activeMinute   bool
	dirty          bool
	rebasing       bool
	replayBudget   *int
	lastMinute     int64
	minute         Timer
	retry          Timer
	retryDelay     time.Duration
}

func (e *engine) rebase() ([]Update, error) {
	return e.rebaseWithContinuity(false)
}

func (e *engine) rebaseWithContinuity(requireContinuity bool) ([]Update, error) {
	if requireContinuity && e.sub == nil {
		return nil, sequencer.ErrProjectionInvalidated
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := e.service.ctx.Err(); err != nil {
			return nil, err
		}
		captured, err := e.service.sequence.CaptureTriggerView(sequencer.MaxJournalBytes)
		if err != nil {
			return nil, err
		}
		// The internal engine keeps the captured maps and cursor without copying
		// capabilities or health, and remains live while external views are cut.
		snapshot := sequencer.Snapshot{Cursor: captured.Cursor, States: captured.States, Registry: captured.Registry,
			RegistryReady: captured.RegistryReady(), CapturedTick: captured.CapturedTick}
		if !snapshot.RegistryReady {
			return nil, ErrInvalidProjection
		}
		staged := *e
		staged.rebasing = true
		remaining := MaxWorkPerWake
		staged.replayBudget = &remaining
		staged.snapshot = snapshot
		staged.dirty = false
		staged.outputs = cloneOutputs(e.outputs)
		staged.readers = cloneReaders(e.readers)
		changes, err := staged.recompute()
		if err != nil {
			return nil, err
		}
		// The sequencer rejects an oversized suffix before allocating its copy.
		newSub, replay, boundary, _, err := e.service.sequence.SubscribeTriggerViewAfter(snapshot.Cursor, 256, sequencer.MaxViewQueueBytes)
		if err != nil {
			if errors.Is(err, sequencer.ErrCursorExpired) {
				return nil, err
			}
			lastErr = err
			continue
		}
		restart := false
		merged := newUpdateAccumulator()
		merged.add(changes)
		replayBytes := 0
		for _, entry := range replay {
			if err := e.service.ctx.Err(); err != nil {
				_ = newSub.Close()
				return nil, err
			}
			charge := entry.EstimatedBytes()
			if charge <= 0 || charge > sequencer.MaxViewQueueBytes-replayBytes {
				_ = newSub.Close()
				return nil, ErrWorkBudget
			}
			replayBytes += charge
			if entry.Kind() == sequencer.KindRegistry {
				restart = true
				break
			}
			more, err := staged.apply(entry)
			if err != nil {
				_ = newSub.Close()
				return nil, err
			}
			merged.add(more)
		}
		if restart {
			_ = newSub.Close()
			lastErr = sequencer.ErrProjectionInvalidated
			continue
		}
		staged.snapshot.Cursor = boundary
		if e.sub != nil {
			if requireContinuity {
				// Check continuity and detach under the admission mutex: loss
				// during capture/render/replay cannot disappear at this handoff.
				if err := e.sub.CloseForViewHandoff(newSub); err != nil {
					_ = newSub.Close()
					return nil, err
				}
			} else {
				_ = e.sub.Close()
			}
		}
		e.snapshot, e.sub, e.outputs, e.readers, e.watchAliases, e.activeMinute, e.dirty = staged.snapshot, newSub, staged.outputs, staged.readers, staged.watchAliases, staged.activeMinute, staged.dirty
		return merged.values(), nil
	}
	if lastErr == nil {
		lastErr = sequencer.ErrCursorInvalidated
	}
	return nil, lastErr
}

func cloneOutputs(source map[ProgramID]Update) map[ProgramID]Update {
	copyMap := make(map[ProgramID]Update, len(source))
	for id, value := range source {
		value.reads = append([]template.Read(nil), value.reads...)
		copyMap[id] = value
	}
	return copyMap
}
func cloneReaders(source map[ProgramID][]template.Dependency) map[ProgramID][]template.Dependency {
	copyMap := make(map[ProgramID][]template.Dependency, len(source))
	for id, deps := range source {
		copyMap[id] = append([]template.Dependency(nil), deps...)
	}
	return copyMap
}

type updateAccumulator struct {
	positions map[ProgramID]int
	items     []Update
}

func newUpdateAccumulator() updateAccumulator {
	return updateAccumulator{positions: make(map[ProgramID]int, MaxPrograms), items: make([]Update, 0, MaxPrograms)}
}
func (a *updateAccumulator) add(changes []Update) {
	for _, change := range changes {
		if i, found := a.positions[change.id]; found {
			a.items[i] = change
			continue
		}
		if len(a.items) >= MaxPrograms {
			continue
		} // Plan admission makes this unreachable.
		a.positions[change.id] = len(a.items)
		a.items = append(a.items, change)
	}
}
func (a updateAccumulator) values() []Update { return a.items }

func (e *engine) apply(entry sequencer.Entry) ([]Update, error) {
	if e.replayBudget != nil {
		if *e.replayBudget < 1 {
			return nil, ErrWorkBudget
		}
		*e.replayBudget -= 1
	}
	e.snapshot.Cursor = entry.Cursor()
	switch entry.Kind() {
	case sequencer.KindState:
	case sequencer.KindEvent, sequencer.KindHealth, sequencer.KindCapabilities, sequencer.KindNativeMQTTEdge:
		return e.flushDirty()
	default:
		return nil, ErrInvalidProjection
	}
	id, state, _, ok := entry.State()
	if !ok {
		return nil, ErrInvalidProjection
	}
	e.snapshot.States[id] = state
	interested, err := e.interested(id)
	if err != nil {
		return nil, err
	}
	if interested {
		e.dirty = true
	}
	return e.flushDirty()
}

// A source entry can arrive during rendering. Keep its recompute intent until
// the subscription has drained, even if the last queued entry is unrelated.
func (e *engine) flushDirty() ([]Update, error) {
	if !e.dirty {
		return nil, nil
	}
	changes, err := e.recompute()
	if err == nil {
		e.dirty = false
	}
	return changes, err
}
func (e *engine) interested(entity domain.EntityID) (bool, error) {
	record, ok := e.snapshot.Registry[entity]
	if !ok || record.Retired() {
		return false, nil
	}
	_, _, aliases, err := record.Aliases()
	if err != nil {
		return false, ErrInvalidProjection
	}
	if e.replayBudget != nil {
		if *e.replayBudget < len(aliases) {
			return false, ErrWorkBudget
		}
		*e.replayBudget -= len(aliases)
	}
	for _, alias := range aliases {
		if _, watched := e.watchAliases[alias]; watched {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) run(e *engine) {
	defer close(s.done)
	defer close(s.updates)
	defer func() {
		// Only this engine mutates the set; complete all observers before the
		// service's Done acknowledgement, without another task or subscription.
		s.CloseAdmissions()
		for _, handle := range s.waitHandles {
			s.finishWait(handle, ErrClosed)
		}
		if e.pendingControl != nil {
			request := e.pendingControl
			if request.release != nil {
				request.release()
			}
			request.result <- templateControlResult{err: ErrClosed}
		}
		for {
			select {
			case request := <-s.controls:
				if request.release != nil {
					request.release()
				}
				request.result <- templateControlResult{err: ErrClosed}
			case request := <-s.planReplacements:
				request.result <- planReplacementResult{err: ErrClosed}
			default:
				return
			}
		}
	}()
	defer func() {
		if e.minute != nil {
			e.minute.Stop()
		}
		if e.retry != nil {
			e.retry.Stop()
		}
		if e.sub != nil {
			_ = e.sub.Close()
		}
	}()
	for {
		if err := s.ctx.Err(); err != nil {
			return
		}
		e.armMinute()
		var delivery sequencer.Delivery
		var wake <-chan struct{}
		var err error
		if e.sub != nil {
			delivery, wake, err = e.sub.Poll()
		}
		if err != nil || (delivery.Kind() == sequencer.DeliveryLoss || (delivery.Kind() == sequencer.DeliveryEntry && !delivery.ReplayValid())) {
			s.setContinuityError(err)
			if e.sub != nil {
				_ = e.sub.Close()
				e.sub = nil
			}
			if emitErr := s.emit(e.degrade(DiagnosticLoss)); emitErr != nil {
				s.setError(emitErr)
			}
			if changes, rebaseErr := e.rebase(); rebaseErr == nil {
				e.retryDelay = 0
				s.publishRecovered(changes)
				continue
			} else {
				s.setContinuityError(rebaseErr)
				e.armRetry()
			}
		}
		if delivery.Kind() == sequencer.DeliveryEntry && err == nil {
			entry, ok := delivery.Entry()
			if !ok {
				s.setError(ErrInvalidProjection)
				continue
			}
			if entry.Kind() == sequencer.KindRegistry {
				changes, rebaseErr := e.rebase()
				if rebaseErr != nil {
					s.setContinuityError(rebaseErr)
					if e.sub != nil {
						_ = e.sub.Close()
						e.sub = nil
					}
					_ = s.emit(e.degrade(DiagnosticLoss))
					e.armRetry()
				} else {
					s.publishRecovered(changes)
				}
				continue
			}
			changes, applyErr := e.apply(entry)
			if errors.Is(applyErr, errSourcePending) {
				continue
			}
			if applyErr != nil {
				s.setError(applyErr)
				_ = s.emit(e.degrade(DiagnosticEvaluation))
			} else if emitErr := s.emit(changes); emitErr != nil {
				s.setError(emitErr)
			}
			continue
		}
		if e.sub != nil && e.dirty {
			changes, renderErr := e.flushDirty()
			if errors.Is(renderErr, errSourcePending) {
				continue
			}
			if renderErr != nil {
				s.setError(renderErr)
				_ = s.emit(e.degrade(DiagnosticEvaluation))
			} else if emitErr := s.emit(changes); emitErr != nil {
				s.setError(emitErr)
			}
			continue
		}
		if e.pendingControl != nil {
			if e.handleControl(e.pendingControl) {
				e.pendingControl = nil
			}
			continue
		}
		var minute <-chan time.Time
		if e.minute != nil {
			minute = e.minute.C()
		}
		var retry <-chan time.Time
		if e.retry != nil {
			retry = e.retry.C()
		}
		select {
		case <-s.ctx.Done():
			return
		case <-wake:
		case <-minute:
			e.minute.Stop()
			e.minute = nil
			instant := s.clock.Now().UTC()
			current := instant.Unix() / 60
			if current > e.lastMinute {
				e.lastMinute = current
				e.dirty = true
				changes, renderErr := e.recomputeAt(instant)
				if renderErr == nil {
					e.dirty = false
				}
				if errors.Is(renderErr, errSourcePending) {
					continue
				}
				if renderErr != nil {
					s.setError(renderErr)
					_ = s.emit(e.degrade(DiagnosticEvaluation))
				} else if emitErr := s.emit(changes); emitErr != nil {
					s.setError(emitErr)
				}
			}
		case <-retry:
			e.retry.Stop()
			e.retry = nil
			changes, rebaseErr := e.rebase()
			if rebaseErr != nil {
				s.setContinuityError(rebaseErr)
				e.armRetry()
			} else {
				e.retryDelay = 0
				s.publishRecovered(changes)
			}
		case request := <-s.controls:
			if !e.handleControl(&request) {
				e.pendingControl = &request
			}
		case req := <-s.replacements:
			if s.groupGeneration.Load() == ^uint64(0) {
				req.result <- ErrInvalidPlan
				continue
			}
			s.mu.Lock()
			old := e.service.plan.groups
			e.service.plan.groups = req.groups
			s.mu.Unlock()
			changes, replaceErr := e.recompute()
			if replaceErr != nil {
				s.mu.Lock()
				e.service.plan.groups = old
				s.mu.Unlock()
				s.setError(replaceErr)
			} else {
				s.groupGeneration.Add(1)
				if emitErr := s.emit(changes); emitErr != nil {
					replaceErr = emitErr
				}
			}
			req.result <- replaceErr
		case req := <-s.planReplacements:
			if s.publicationGeneration.Load() == ^uint64(0) {
				req.result <- planReplacementResult{err: ErrInvalidPlan}
				continue
			}
			old := e.service.plan
			oldPermanent := s.permanentPrograms
			transient := append([]Definition(nil), old.definitions[oldPermanent:]...)
			definitions := make([]Definition, 0, len(req.plan.definitions)+len(transient))
			definitions = append(definitions, req.plan.definitions...)
			definitions = append(definitions, transient...)
			updated, err := NewPlan(definitions, req.plan.groups, req.plan.zone)
			if err != nil {
				req.result <- planReplacementResult{err: err}
				continue
			}
			updated.waitsEnabled = old.waitsEnabled || req.plan.waitsEnabled
			priorOutputs, priorReaders := cloneOutputs(e.outputs), cloneReaders(e.readers)
			priorAliases := make(map[domain.HAAlias]struct{}, len(e.watchAliases))
			for alias := range e.watchAliases {
				priorAliases[alias] = struct{}{}
			}
			priorMinute, priorDirty := e.activeMinute, e.dirty
			s.mu.Lock()
			s.plan, s.permanentPrograms = updated, len(req.plan.definitions)
			s.mu.Unlock()
			keep := make(map[ProgramID]bool, len(definitions))
			for _, definition := range definitions {
				keep[definition.id] = true
			}
			for id := range e.outputs {
				if !keep[id] {
					delete(e.outputs, id)
					delete(e.readers, id)
				}
			}
			changes, replaceErr := e.recompute()
			if errors.Is(replaceErr, errSourcePending) {
				// Host retirement may enqueue a source after this request was
				// dequeued. Join it through the existing capture/suffix handoff;
				// this scheduling signal is not a failed configuration image.
				changes, replaceErr = e.rebaseWithContinuity(true)
			}
			applied := replaceErr == nil
			if replaceErr != nil {
				s.mu.Lock()
				s.plan, s.permanentPrograms = old, oldPermanent
				s.mu.Unlock()
				e.outputs, e.readers, e.watchAliases, e.activeMinute, e.dirty = priorOutputs, priorReaders, priorAliases, priorMinute, priorDirty
			} else {
				// A queued A permanent value must never be delivered as B after
				// replacing its definition. Re-emit the current B snapshot for
				// every permanent output; transient wait handles stay owned.
				for {
					select {
					case <-s.updates:
					default:
						goto drainedPlanUpdates
					}
				}
			drainedPlanUpdates:
				s.publicationGeneration.Add(1)
				publish := newUpdateAccumulator()
				publish.add(changes)
				for _, definition := range req.plan.definitions {
					if update, present := e.outputs[definition.id]; present {
						publish.add([]Update{update})
					}
				}
				if emitErr := s.emit(publish.values()); emitErr != nil {
					replaceErr = emitErr
				}
			}
			req.result <- planReplacementResult{applied: applied, err: replaceErr}
		}
	}
}

func (e *engine) armRetry() {
	if e.retry != nil {
		return
	}
	if e.retryDelay == 0 {
		e.retryDelay = 250 * time.Millisecond
	} else {
		e.retryDelay *= 2
		if e.retryDelay > time.Second {
			e.retryDelay = time.Second
		}
	}
	e.retry = e.service.clock.NewTimer(e.retryDelay)
}

func (e *engine) armMinute() {
	if !e.activeMinute || e.sub == nil {
		if e.minute != nil {
			e.minute.Stop()
			e.minute = nil
		}
		return
	}
	if e.minute != nil {
		return
	}
	now := e.service.clock.Now().UTC()
	next := now.Truncate(time.Minute).Add(time.Minute)
	if next.Unix()/60 <= e.lastMinute {
		next = time.Unix((e.lastMinute+1)*60, 0).UTC()
	}
	delay := next.Sub(now)
	if delay < time.Millisecond {
		delay = time.Millisecond
	}
	e.minute = e.service.clock.NewTimer(delay)
}

func (e *engine) degrade(reason Diagnostic) []Update {
	e.dirty = false
	changes := make([]Update, 0, len(e.outputs))
	for _, definition := range e.service.plan.definitions {
		old, exists := e.outputs[definition.id]
		if !exists {
			continue
		}
		quality := staleQuality(old.quality)
		degraded := Update{id: definition.id, result: old.result, valid: old.valid, quality: quality, reads: append([]template.Read(nil), old.reads...), diagnostic: reason, cursor: e.snapshot.Cursor}
		if !degraded.Equal(old) {
			e.outputs[definition.id] = degraded
			changes = append(changes, degraded)
		}
	}
	return changes
}

func staleQuality(previous domain.Quality) domain.Quality {
	availability, _, _, err := previous.Components()
	if err != nil {
		availability = domain.AvailabilityUnknown
	}
	quality, _ := domain.NewQuality(availability, domain.FreshnessStale, domain.ProvenanceEstimated)
	return quality
}

type candidate struct {
	result     template.Result
	valid      bool
	quality    domain.Quality
	reads      []template.Read
	deps       []template.Dependency
	active     []template.Dependency
	diagnostic Diagnostic
}

func equalCandidate(left, right candidate) bool {
	if left.valid != right.valid || left.quality != right.quality || left.diagnostic != right.diagnostic || (left.valid && !left.result.Equal(right.result)) || len(left.deps) != len(right.deps) || len(left.reads) != len(right.reads) {
		return false
	}
	for i := range left.reads {
		if left.reads[i] != right.reads[i] {
			return false
		}
	}
	for i := range left.deps {
		if left.deps[i] != right.deps[i] {
			return false
		}
	}
	return true
}

func unionDependencies(parts ...[]template.Dependency) ([]template.Dependency, error) {
	seen := make(map[template.Dependency]struct{})
	for _, part := range parts {
		for _, dep := range part {
			seen[dep] = struct{}{}
			if len(seen) > 128 {
				return nil, template.ErrBudget
			}
		}
	}
	out := make([]template.Dependency, 0, len(seen))
	for dep := range seen {
		out = append(out, dep)
	}
	sort.Slice(out, func(i, j int) bool { return dependencyKey(out[i]) < dependencyKey(out[j]) })
	return out, nil
}
func dependencyKey(dep template.Dependency) string {
	alias, _ := dep.Alias()
	name, _ := alias.Value()
	return string(rune(dep.Kind())) + name + "\x00" + dep.Attribute()
}

func qualityFromReads(reads []template.Read) domain.Quality {
	availability, freshness := domain.AvailabilityAvailable, domain.FreshnessFresh
	for _, read := range reads {
		if read.Dependency().Kind() == template.DependencyMembership && read.Present() {
			continue
		}
		quality, ok := read.Quality()
		if !ok {
			if availability == domain.AvailabilityAvailable {
				availability = domain.AvailabilityUnknown
			}
			freshness = domain.FreshnessStale
			continue
		}
		a, f, _, err := quality.Components()
		if err != nil {
			availability, freshness = domain.AvailabilityUnknown, domain.FreshnessStale
			continue
		}
		if a == domain.AvailabilityUnavailable {
			availability = a
		} else if a == domain.AvailabilityUnknown && availability == domain.AvailabilityAvailable {
			availability = a
		}
		if f == domain.FreshnessStale {
			freshness = f
		}
	}
	q, _ := domain.NewQuality(availability, freshness, domain.ProvenanceEstimated)
	return q
}
