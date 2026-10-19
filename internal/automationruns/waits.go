package automationruns

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/domain"
)

type WaitExit uint8

const (
	WaitExitInvalid WaitExit = iota
	WaitCompleted
	WaitContinuedAfterTimeout
	WaitStoppedByTimeout
	WaitCancelled
	WaitSourceFailed
)

// Variables and terminal cause are independent: completed=true/remaining=0
// can accompany WaitStoppedByTimeout after both notifications were processed.
type waitObservation struct {
	kind         ir.ActionKind
	completed    bool
	remaining    time.Duration
	hasRemaining bool
	trigger      automationtriggers.WaitTrigger
	hasTrigger   bool
	exit         WaitExit
}

func (w waitObservation) value() (template.Value, error) {
	if w.kind != ir.ActionWait && w.kind != ir.ActionWaitTemplate {
		return template.Value{}, ErrInvalidPlan
	}
	remaining := template.None()
	if w.hasRemaining {
		var err error
		remaining, err = template.Float(w.remaining.Seconds())
		if err != nil {
			return template.Value{}, err
		}
	}
	fields := []template.Binding{{Name: "completed", Value: template.Bool(w.completed)}, {Name: "remaining", Value: remaining}}
	if w.kind == ir.ActionWait {
		trigger := template.None()
		if w.hasTrigger {
			var err error
			trigger, err = triggerMetadata(w.trigger.ID(), w.trigger.Index(), w.trigger.Kind())
			if err != nil {
				return template.Value{}, err
			}
		}
		fields = append(fields, template.Binding{Name: "trigger", Value: trigger})
	}
	return template.Object(fields)
}
func (w waitObservation) publish(environment *Environment) error {
	value, err := w.value()
	if err != nil {
		return err
	}
	return environment.Set("wait", value)
}
func triggerPlatform(kind ir.TriggerKind) (string, error) {
	switch kind {
	case ir.TriggerTime:
		return "time", nil
	case ir.TriggerSunset:
		return "sun", nil
	case ir.TriggerEvent:
		return "event", nil
	case ir.TriggerMQTT:
		return "mqtt", nil
	case ir.TriggerDeviceMQTT:
		return "device", nil
	case ir.TriggerState:
		return "state", nil
	case ir.TriggerNumericState:
		return "numeric_state", nil
	case ir.TriggerTimePattern:
		return "time_pattern", nil
	case ir.TriggerCalendar:
		return "calendar", nil
	case ir.TriggerInvalid:
		return "", ErrInvalidPlan
	}
	return "", ErrInvalidPlan
}
func triggerMetadata(id string, index int, kind ir.TriggerKind) (template.Value, error) {
	if index < 0 || index >= automationtriggers.MaxTriggers {
		return template.Value{}, ErrInvalidPlan
	}
	if id == "" {
		id = strconv.Itoa(index)
	}
	platform, err := triggerPlatform(kind)
	if err != nil {
		return template.Value{}, err
	}
	idValue, err := template.String(id)
	if err != nil {
		return template.Value{}, err
	}
	idxValue, err := template.String(strconv.Itoa(index))
	if err != nil {
		return template.Value{}, err
	}
	platformValue, err := template.String(platform)
	if err != nil {
		return template.Value{}, err
	}
	return template.Object([]template.Binding{{Name: "id", Value: idValue}, {Name: "idx", Value: idxValue}, {Name: "platform", Value: platformValue}})
}

type triggerWaitResource interface {
	Ready() <-chan struct{}
	Close() <-chan struct{}
	Outcome() (automationtriggers.Result, error)
	Trigger() (automationtriggers.WaitTrigger, bool)
	CompletedAt() (time.Duration, bool)
}
type templateWaitResource interface {
	Ready() <-chan struct{}
	Done() <-chan struct{}
	Close(context.Context) error
	Outcome() error
	CompletedAt() (time.Duration, bool)
}
type waitResources struct {
	once     sync.Once
	trigger  triggerWaitResource
	template templateWaitResource
	timer    automationtriggers.Timer
	err      error
}

func (r *waitResources) close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		if !nilPort(r.timer) {
			r.timer.Stop()
		}
		if !nilPort(r.trigger) {
			if done := r.trigger.Close(); done != nil {
				<-done
			} else {
				r.err = ErrInvalidPlan
			}
		}
		if !nilPort(r.template) {
			r.err = errors.Join(r.err, r.template.Close(context.Background()))
			if done := r.template.Done(); done != nil {
				<-done
			} else {
				r.err = errors.Join(r.err, ErrInvalidPlan)
			}
		}
	})
	return r.err
}

type delayOutcome uint8

const (
	delayInvalid delayOutcome = iota
	delayElapsed
	delayStopped
)

func (i *Interpreter) delay(ctx context.Context, duration time.Duration, suspend func()) (outcome delayOutcome, err error) {
	if nilPort(ctx) || duration < 0 {
		return delayInvalid, ErrInvalidPlan
	}
	if ctx.Err() != nil {
		return delayStopped, nil
	}
	if duration == 0 {
		return delayElapsed, nil
	}
	resources := waitResources{timer: i.input.Clock.NewTimer(duration)}
	defer func() { err = errors.Join(err, resources.close()) }()
	if nilPort(resources.timer) || resources.timer.C() == nil {
		return delayInvalid, ErrInvalidPlan
	}
	if suspend != nil {
		suspend()
	}
	select {
	case <-ctx.Done():
		return delayStopped, nil
	case <-resources.timer.C():
		if ctx.Err() != nil {
			return delayStopped, nil
		}
		return delayElapsed, nil
	}
}
func usableTemplate(tracked template.TrackedResult) (bool, error) {
	for _, read := range tracked.Reads() {
		quality, present := read.Quality()
		if !present || !read.Present() {
			return false, ErrInvalidPlan
		}
		availability, freshness, provenance, err := quality.Components()
		if err != nil || availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh ||
			(provenance != domain.ProvenanceObserved && provenance != domain.ProvenanceEstimated) {
			return false, ErrInvalidPlan
		}
	}
	result, present := tracked.Result()
	if !present {
		return false, ErrInvalidPlan
	}
	return strings.EqualFold(result.Text(), "true"), nil
}

func (i *Interpreter) wait(ctx context.Context, node executableNode, run Run) (observation waitObservation, err error) {
	observation = waitObservation{kind: node.kind, remaining: node.timeout, hasRemaining: node.hasTimeout}
	resources := waitResources{}
	defer func() {
		cleanupErr := resources.close()
		cancelled := ctx.Err() != nil && errors.Is(err, ctx.Err()) && cleanupErr == nil
		err = errors.Join(err, cleanupErr)
		if err != nil {
			observation.exit = WaitSourceFailed
			if cancelled {
				observation.exit = WaitCancelled
			}
		}
	}()
	if err = observation.publish(run.env); err != nil {
		return
	}
	if node.kind == ir.ActionWaitTemplate {
		bindings, release, captureErr := run.env.snapshot()
		if captureErr != nil {
			err = captureErr
			return
		}
		tracked, evaluateErr := i.input.Conditions.EvaluateCondition(ctx, node.waitProgram, bindings)
		release()
		// An initial render error remains false; the observer retains reads.
		// Budget/loss errors remain explicit failures, never old display data.
		if evaluateErr != nil && !errors.Is(evaluateErr, template.ErrEvaluation) {
			err = evaluateErr
			return
		}
		if evaluateErr == nil {
			matched, qualityErr := usableTemplate(tracked)
			if qualityErr != nil {
				err = executionCause(ExecutionSourceUnavailable, node.source, qualityErr)
				return
			}
			if matched {
				observation.completed, observation.exit = true, WaitCompleted
				err = observation.publish(run.env)
				return
			}
		}
	}
	if node.hasTimeout && node.timeout == 0 {
		observation.exit = WaitContinuedAfterTimeout
		if !node.continueOnTimeout {
			observation.exit = WaitStoppedByTimeout
		}
		err = observation.publish(run.env)
		return
	}
	deadline := time.Duration(0)
	var timeout <-chan time.Time
	if node.hasTimeout {
		now := i.input.Clock.MonoNow()
		if node.timeout < 0 || now > time.Duration(1<<63-1)-node.timeout {
			err = ErrInvalidPlan
			return
		}
		deadline = now + node.timeout
		resources.timer = i.input.Clock.NewTimer(node.timeout)
		if nilPort(resources.timer) || resources.timer.C() == nil {
			err = ErrInvalidPlan
			return
		}
		timeout = resources.timer.C()
	}
	var ready <-chan struct{}
	switch node.kind {
	case ir.ActionWait:
		resources.trigger, err = i.input.TriggerWaits.ObserveWait(ctx, node.source)
		if err == nil {
			ready = resources.trigger.Ready()
		}
	case ir.ActionWaitTemplate:
		bindings, release, captureErr := run.env.snapshot()
		if captureErr != nil {
			err = captureErr
			return
		}
		resources.template, err = i.input.TemplateWaits.ObserveTemplateOnClock(ctx, node.waitProgram, bindings, i.input.Clock)
		release()
		if err == nil {
			ready = resources.template.Ready()
		}
	default:
		err = ErrInvalidPlan
	}
	if err != nil {
		return
	}
	if ready == nil {
		err = ErrInvalidPlan
		return
	}
	i.traceNode(run, node, NodeWaiting, nil)
	if run.suspend != nil {
		run.suspend()
	}
	timedOut, sourceReady := false, false
	for {
		// Take all admitted notifications at the actual waiter resumption.
		// The select wakeup does not choose the node's terminal cause.
		if ready != nil {
			select {
			case <-ready:
				sourceReady = true
				ready = nil
			default:
			}
		}
		if timeout != nil {
			select {
			case <-timeout:
				timedOut = true
				timeout = nil
			default:
			}
		}
		// If timeout became ready during that first capture, sample the
		// one-shot source once more. This is the linearization point with a
		// known-completed timeout, rather than an inconsistent two-channel
		// snapshot spanning a newly completed source notification.
		if timedOut && ready != nil {
			select {
			case <-ready:
				sourceReady = true
				ready = nil
			default:
			}
		}
		if sourceReady && !observation.completed {
			var at time.Duration
			var hasAt bool
			if resources.trigger != nil {
				_, err = resources.trigger.Outcome()
				if err == nil {
					observation.trigger, observation.hasTrigger = resources.trigger.Trigger()
					at, hasAt = resources.trigger.CompletedAt()
					if !observation.hasTrigger {
						err = ErrInvalidPlan
					}
				}
			} else {
				err = resources.template.Outcome()
				at, hasAt = resources.template.CompletedAt()
			}
			if err != nil {
				return
			}
			observation.completed = true
			if node.hasTimeout {
				if !hasAt {
					err = ErrInvalidPlan
					return
				}
				observation.remaining = max(time.Duration(0), deadline-at)
			}
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			err = errors.Join(err, observation.publish(run.env))
			return
		}
		if timedOut {
			observation.remaining = 0
			observation.exit = WaitContinuedAfterTimeout
			if !node.continueOnTimeout {
				observation.exit = WaitStoppedByTimeout
			}
			err = observation.publish(run.env)
			return
		}
		if observation.completed {
			observation.exit = WaitCompleted
			err = observation.publish(run.env)
			return
		}
		select {
		case <-ctx.Done():
		case <-ready:
			sourceReady = true
			ready = nil
		case <-timeout:
			timedOut = true
			timeout = nil
		}
	}
}
