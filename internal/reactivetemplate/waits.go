package reactivetemplate

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
)

type templateControlKind uint8

const (
	templateControlInvalid templateControlKind = iota
	templateObserve
	templateRemove
	templateEvaluate
)

type templateControl struct {
	kind      templateControlKind
	ctx       context.Context
	program   template.Program
	variables []template.Binding
	handle    *WaitHandle
	result    chan templateControlResult
	clock     MonotonicClock
	release   func()
}
type MonotonicClock interface{ MonoNow() time.Duration }
type templateControlResult struct {
	handle  *WaitHandle
	tracked template.TrackedResult
	err     error
}

// WaitHandle is a definition/reader in the one existing render engine. Ready
// denotes a safe true result or an explicit failure; Done denotes its actual
// removal, including the last minute listener when no other reader needs it.
type WaitHandle struct {
	service            *Service
	id                 ProgramID
	ready, done        chan struct{}
	completed, removed bool
	err                error
	clock              MonotonicClock
	completedAt        time.Duration
	release            func()
}

func (h *WaitHandle) Ready() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.ready
}
func (h *WaitHandle) Done() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.done
}
func (h *WaitHandle) Outcome() error {
	if h == nil || h.service == nil {
		return ErrInvalidPlan
	}
	h.service.mu.Lock()
	defer h.service.mu.Unlock()
	if !h.completed {
		return ErrInvalidPlan
	}
	return h.err
}
func (h *WaitHandle) CompletedAt() (time.Duration, bool) {
	if h == nil || h.service == nil {
		return 0, false
	}
	h.service.mu.Lock()
	defer h.service.mu.Unlock()
	return h.completedAt, h.completed && h.err == nil && !nilReactiveValue(h.clock)
}
func (h *WaitHandle) Close(ctx context.Context) error {
	if h == nil || h.service == nil || nilReactiveValue(ctx) {
		return ErrInvalidPlan
	}
	select {
	case <-h.done:
		return nil
	default:
	}
	_, err := h.service.control(templateControl{kind: templateRemove, ctx: ctx, handle: h, result: make(chan templateControlResult, 1)})
	if err != nil && err != ErrClosed {
		return err
	}
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) ObserveTemplate(ctx context.Context, program template.Program, variables []template.Binding) (*WaitHandle, error) {
	if s == nil || nilReactiveValue(ctx) || !program.Valid() {
		return nil, ErrInvalidPlan
	}
	result, err := s.control(templateControl{kind: templateObserve, ctx: ctx, program: program, variables: variables, result: make(chan templateControlResult, 1)})
	if err != nil {
		return nil, err
	}
	return result.handle, nil
}
func (s *Service) ObserveTemplateOnClock(ctx context.Context, program template.Program, variables []template.Binding, clock MonotonicClock) (*WaitHandle, error) {
	if s == nil || nilReactiveValue(ctx) || !program.Valid() || nilReactiveValue(clock) {
		return nil, ErrInvalidPlan
	}
	result, err := s.control(templateControl{kind: templateObserve, ctx: ctx, program: program, variables: variables, clock: clock, result: make(chan templateControlResult, 1)})
	return result.handle, err
}

// EvaluateCondition runs synchronously over the same projection as waits and
// permanent reactive definitions. No ReadView snapshot escapes to concurrent
// callers. The admitted tracked result is small and immutable.
func (s *Service) EvaluateCondition(ctx context.Context, program template.Program, variables []template.Binding) (template.TrackedResult, error) {
	if s == nil || nilReactiveValue(ctx) || !program.Valid() {
		return template.TrackedResult{}, ErrInvalidPlan
	}
	result, err := s.control(templateControl{kind: templateEvaluate, ctx: ctx, program: program, variables: variables, result: make(chan templateControlResult, 1)})
	return result.tracked, err
}
func (s *Service) control(request templateControl) (templateControlResult, error) {
	if s == nil || nilReactiveValue(request.ctx) {
		return templateControlResult{}, ErrInvalidPlan
	}
	if err := request.ctx.Err(); err != nil {
		return templateControlResult{}, err
	}
	if request.kind != templateRemove {
		if !request.program.Valid() || len(request.variables) > 16 {
			return templateControlResult{}, ErrInvalidPlan
		}
		// One slot covers bounded validation copies and the retained request.
		// Successful Observe transfers it to the definition until real Done.
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil {
			s.mu.Unlock()
			return templateControlResult{}, ErrClosed
		}
		if s.permanentPrograms+s.controlSlots >= MaxPrograms {
			s.mu.Unlock()
			return templateControlResult{}, ErrWorkBudget
		}
		s.controlSlots++
		s.mu.Unlock()
		var once sync.Once
		request.release = func() { once.Do(func() { s.mu.Lock(); s.controlSlots--; s.mu.Unlock() }) }
		if _, err := template.NewInputs(nil, request.variables, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); err != nil {
			request.release()
			return templateControlResult{}, err
		}
		owned := make([]template.Binding, len(request.variables))
		for index, binding := range request.variables {
			owned[index] = template.Binding{Name: binding.Name, Value: binding.Value.Copy()}
		}
		request.variables = owned
	}
	s.mu.Lock()
	if s.closed || s.ctx.Err() != nil {
		s.mu.Unlock()
		if request.release != nil {
			request.release()
		}
		return templateControlResult{}, ErrClosed
	}
	select {
	case s.controls <- request:
	default:
		s.mu.Unlock()
		if request.release != nil {
			request.release()
		}
		return templateControlResult{}, ErrWorkBudget
	}
	s.mu.Unlock()
	// Once admitted, await the actual acknowledgement even after cancellation.
	// The render operation uses that same context, and cleanup cannot orphan a
	// just-created definition by losing a race with the response channel.
	select {
	case result := <-request.result:
		if err := request.ctx.Err(); err != nil {
			if result.handle != nil {
				_ = result.handle.Close(context.Background())
			}
			return templateControlResult{}, err
		}
		return result, result.err
	case <-s.done:
		return templateControlResult{}, ErrClosed
	}
}

func (s *Service) routeWaitUpdate(update Update) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	handle, present := s.waitHandles[update.id]
	if !present {
		return false
	}
	if handle.completed {
		return true
	}
	switch update.diagnostic {
	case DiagnosticEvaluation:
		return true // keep the real reader/dependencies until recovery
	case DiagnosticHealthy:
		result, usable := update.CurrentResult()
		if !usable || !strings.EqualFold(result.Text(), "true") {
			return true
		}
	case DiagnosticWorkBudget:
		handle.err = ErrWorkBudget
	case DiagnosticLoss, DiagnosticOscillation, DiagnosticUnspecified:
		handle.err = ErrInvalidProjection
	default:
		handle.err = ErrInvalidProjection
	}
	handle.completed = true
	if handle.err == nil && !nilReactiveValue(handle.clock) {
		handle.completedAt = handle.clock.MonoNow()
	}
	close(handle.ready)
	return true
}
func (s *Service) finishWait(handle *WaitHandle, err error) {
	s.mu.Lock()
	if handle.removed {
		s.mu.Unlock()
		return
	}
	handle.removed = true
	if !handle.completed {
		handle.completed, handle.err = true, err
		close(handle.ready)
	}
	delete(s.waitHandles, handle.id)
	release := handle.release
	handle.release = nil
	s.mu.Unlock()
	if release != nil {
		release()
	}
	close(handle.done)
}

func (e *engine) handleControl(request *templateControl) (handled bool) {
	// A concurrently admitted suffix must be drained by this same engine.
	// Keep one bounded request pending; callers never retry or read snapshots.
	if request.kind != templateRemove && request.ctx.Err() == nil && errors.Is(e.checkControl(), errSourcePending) {
		return false
	}
	handled = true
	reply := templateControlResult{}
	defer func() {
		if !handled {
			return
		}
		if request.kind == templateObserve && request.handle != nil && reply.handle == nil {
			e.removeWaitDefinition(request.handle)
			e.service.finishWait(request.handle, reply.err)
		}
		if reply.handle == nil && request.release != nil {
			request.release()
		}
		request.result <- reply
	}()
	if request.kind != templateRemove {
		if err := request.ctx.Err(); err != nil {
			reply.err = err
			return
		}
		if e.sub == nil {
			reply.err = ErrInvalidProjection
			return
		}
		if err := e.checkControl(); err != nil {
			reply.err = err
			return
		}
	}
	switch request.kind {
	case templateObserve:
		if !e.service.plan.waitsEnabled || request.handle == nil && len(e.service.plan.definitions) >= MaxPrograms {
			reply.err = ErrWorkBudget
			return
		}
		handle := request.handle
		if handle == nil {
			var id ProgramID
			for {
				if e.nextWait == ^uint64(0) {
					reply.err = ErrWorkBudget
					return
				}
				e.nextWait++
				id = ProgramID{value: "script-wait/" + strconv.FormatUint(e.nextWait, 10)}
				duplicate := false
				for _, definition := range e.service.plan.definitions {
					if definition.id == id {
						duplicate = true
						break
					}
				}
				if !duplicate {
					break
				}
			}
			definition, err := NewDefinition(id, request.program, request.variables, nil)
			if err != nil {
				reply.err = err
				return
			}
			handle = &WaitHandle{service: e.service, id: id, ready: make(chan struct{}), done: make(chan struct{}), clock: request.clock, release: request.release}
			request.handle = handle
			e.service.mu.Lock()
			e.service.waitHandles[id] = handle
			e.service.mu.Unlock()
			e.service.plan.definitions = append(e.service.plan.definitions, definition)
		}
		changes, err := e.recompute()
		if errors.Is(err, errSourcePending) {
			handled = false
			return
		}
		if err != nil {
			reply.err = err
			return
		}
		if err := e.service.emit(changes); err != nil {
			reply.err = err
			return
		}
		reply.handle = handle
	case templateRemove:
		handle := request.handle
		if handle == nil || handle.service != e.service {
			reply.err = ErrInvalidPlan
			return
		}
		e.removeWaitDefinition(handle)
		e.service.finishWait(handle, context.Canceled)
	case templateEvaluate:
		view, err := e.currentReadView()
		if err != nil {
			reply.err = err
			return
		}
		inputs, err := template.NewInputsFromReadView(view, request.variables, e.service.clock.Now().UTC(), e.service.plan.zone)
		if err != nil {
			reply.err = err
			return
		}
		remaining := MaxWorkPerWake - len(e.snapshot.Registry) - len(e.snapshot.States) - len(e.service.plan.groups)
		limits := template.DefaultLimits()
		if remaining < limits.Steps() {
			limits, err = limits.WithSteps(remaining)
			if err != nil {
				reply.err = ErrWorkBudget
				return
			}
		}
		reply.tracked, reply.err = template.EvaluateTracked(request.ctx, request.program, inputs, limits)
		if reply.err == nil {
			reply.err = e.checkControl()
		}
		if errors.Is(reply.err, errSourcePending) {
			handled = false
			return
		}
	case templateControlInvalid:
		reply.err = ErrInvalidPlan
	default:
		reply.err = ErrInvalidPlan
	}
	return
}
func (e *engine) removeWaitDefinition(handle *WaitHandle) {
	for index, definition := range e.service.plan.definitions {
		if definition.id != handle.id {
			continue
		}
		copy(e.service.plan.definitions[index:], e.service.plan.definitions[index+1:])
		e.service.plan.definitions[len(e.service.plan.definitions)-1] = Definition{}
		e.service.plan.definitions = e.service.plan.definitions[:len(e.service.plan.definitions)-1]
		break
	}
	delete(e.outputs, handle.id)
	delete(e.readers, handle.id)
	// Rebuild interest immediately, so Done includes the actual last minute
	// timer stop even if a newer journal entry postpones the next fixed point.
	e.watchAliases = make(map[domain.HAAlias]struct{})
	e.activeMinute = false
	for _, dependencies := range e.readers {
		for _, dependency := range dependencies {
			if alias, present := dependency.Alias(); present {
				e.watchAliases[alias] = struct{}{}
			}
			if dependency.Kind() == template.DependencyMinute {
				e.activeMinute = true
			}
		}
	}
	e.armMinute()
}
func (e *engine) currentReadView() (template.ReadView, error) {
	view, err := ReadViewFromSnapshot(e.snapshot, e.service.plan.groups)
	if err != nil {
		return template.ReadView{}, err
	}
	for _, definition := range e.service.plan.definitions {
		if !definition.hasTarget {
			continue
		}
		update, present := e.outputs[definition.id]
		if !present {
			continue
		}
		result, present := update.CurrentResult()
		if !present {
			continue
		}
		record, present := e.snapshot.Registry[definition.target]
		if !present {
			return template.ReadView{}, ErrInvalidProjection
		}
		_, _, aliases, err := record.Aliases()
		if err != nil {
			return template.ReadView{}, err
		}
		view, err = view.WithCandidate(aliases, result.Text(), update.quality)
		if err != nil {
			return template.ReadView{}, err
		}
	}
	return view, nil
}

func (s *Service) SupportsScriptWaits(zone string) bool {
	return s != nil && s.plan.waitsEnabled && s.plan.zone != nil && s.plan.zone.String() == zone
}
