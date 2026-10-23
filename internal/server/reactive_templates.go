package server

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/derivedpower"
	"reflect"
	"sync"

	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrReactiveTemplateComposition = errors.New("server: invalid reactive template composition")

// ReactiveTemplateOutput is a typed, Run-owned port. Implementations must
// stop promptly when ctx is cancelled. They may publish a final calculated
// state through the same Host, never an action or parallel projection.
type ReactiveTemplateOutput interface {
	PublishReactiveTemplate(context.Context, *integrations.Manager, reactivetemplate.Update) error
}

type ReactiveTemplateComposition struct {
	plan   reactivetemplate.Plan
	output ReactiveTemplateOutput
	clock  reactivetemplate.Clock
}

type derivedReactiveOutput struct {
	previous ReactiveTemplateOutput
	derived  ReactiveTemplateOutput
	priorIDs []reactivetemplate.ProgramID
	newIDs   []reactivetemplate.ProgramID
}

func (o derivedReactiveOutput) PublishReactiveTemplate(ctx context.Context, host *integrations.Manager, update reactivetemplate.Update) error {
	for _, id := range o.newIDs {
		if update.ID() == id {
			return o.derived.PublishReactiveTemplate(ctx, host, update)
		}
	}
	for _, id := range o.priorIDs {
		if update.ID() == id {
			return o.previous.PublishReactiveTemplate(ctx, host, update)
		}
	}
	return ErrReactiveTemplateComposition
}

// One service retains the caller's plan, groups, clock, and wait admissions.
// A colliding permanent ProgramID is rejected by Plan.WithDefinitions.
func composeDerivedReactiveTemplates(previous *ReactiveTemplateComposition, additions reactivetemplate.Plan, output ReactiveTemplateOutput) (*ReactiveTemplateComposition, error) {
	if nilReactiveInterface(output) || !additions.Active() {
		return nil, ErrReactiveTemplateComposition
	}
	if previous == nil {
		return NewReactiveTemplateComposition(additions, output, reactivetemplate.RealClock{})
	}
	plan, err := previous.plan.WithPlan(additions)
	if err != nil {
		return nil, errors.Join(ErrReactiveTemplateComposition, err)
	}
	mux := derivedReactiveOutput{previous: previous.output, derived: output, priorIDs: previous.plan.ProgramIDs(), newIDs: additions.ProgramIDs()}
	return NewReactiveTemplateComposition(plan, mux, previous.clock)
}

func NewReactiveTemplateComposition(plan reactivetemplate.Plan, output ReactiveTemplateOutput, clock reactivetemplate.Clock) (*ReactiveTemplateComposition, error) {
	if !plan.Active() || nilReactiveInterface(output) || nilReactiveInterface(clock) {
		return nil, ErrReactiveTemplateComposition
	}
	return &ReactiveTemplateComposition{plan: plan, output: output, clock: clock}, nil
}

// Enable transient script waits in the one declared 044 engine. A copied
// descriptor retains permanent outputs and the caller's explicit clock; no
// second template service or fallback global engine is constructed.
func scriptReactiveTemplateComposition(composition *ReactiveTemplateComposition, runs *AutomationRunsComposition) (*ReactiveTemplateComposition, error) {
	if runs == nil || runs.scripts == nil {
		return composition, nil
	}
	if composition == nil {
		return nil, ErrInvalidTriggerComposition
	}
	plan, err := composition.plan.WithScriptWaits(runs.scripts.input.Zone)
	if err != nil {
		return nil, errors.Join(ErrInvalidTriggerComposition, err)
	}
	return &ReactiveTemplateComposition{plan: plan, output: composition.output, clock: composition.clock}, nil
}

func nilReactiveInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

type reactiveTemplateRuntime struct {
	service    *reactivetemplate.Service
	cancel     context.CancelFunc
	done       chan struct{}
	mu         sync.Mutex
	err        error
	forwarding chan *templateForwardingPause
	paused     *templateForwardingPause
	output     ReactiveTemplateOutput
	derived    *derivedpower.Publisher
}

type templateForwardingPause struct {
	before uint64
	ack    chan struct{}
	resume chan struct{}
}

func startReactiveTemplates(ctx context.Context, identity integrations.IdentityAuthority, host *integrations.Manager, sequence *sequencer.Sequencer, composition *ReactiveTemplateComposition) (*reactiveTemplateRuntime, error) {
	if composition == nil {
		return nil, nil
	}
	if nilReactiveInterface(ctx) || nilReactiveInterface(identity) || host == nil || sequence == nil || !composition.plan.Active() || nilReactiveInterface(composition.output) || nilReactiveInterface(composition.clock) {
		return nil, ErrReactiveTemplateComposition
	}
	snapshot, err := sequence.Capture()
	if err != nil {
		return nil, err
	}
	if !snapshot.RegistryReady {
		projection, ok := identity.(interface {
			AttachProjection(context.Context, integrations.ProjectionSink) error
		})
		if !ok {
			return nil, ErrReactiveTemplateComposition
		}
		if err := projection.AttachProjection(ctx, sequence); err != nil {
			return nil, err
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	service, err := reactivetemplate.Start(runCtx, sequence, composition.plan, composition.clock)
	if err != nil {
		cancel()
		return nil, err
	}
	runtime := &reactiveTemplateRuntime{service: service, cancel: cancel, done: make(chan struct{}), forwarding: make(chan *templateForwardingPause), output: composition.output, derived: derivedOutputPublisher(composition.output)}
	go func() {
		defer close(runtime.done)
		failed := false
		var pause *templateForwardingPause
		for {
			var update reactivetemplate.Update
			var ok bool
			select {
			case request := <-runtime.forwarding:
				pause = request
				close(request.ack) // all previously dequeued A forwarding joined
				continue
			case update, ok = <-service.Updates():
				if !ok {
					goto outputClosed
				}
			}
			if pause != nil {
				select {
				case <-pause.resume:
					pause = nil
				default:
				}
				if pause != nil {
					if update.Publication() <= pause.before {
						continue
					}
					// Hold at most this one B value. Its remaining image is in
					// the existing bounded service queue; no second output queue.
					select {
					case <-pause.resume:
						pause = nil
					case <-runCtx.Done():
						continue
					}
				}
			}
			if failed {
				continue
			}
			if update.Publication() != service.Publication() {
				continue
			}
			runtime.mu.Lock()
			output := runtime.output
			runtime.mu.Unlock()
			if err := output.PublishReactiveTemplate(runCtx, host, update); err != nil {
				runtime.mu.Lock()
				runtime.err = err
				runtime.mu.Unlock()
				failed = true
				service.CloseAdmissions()
			}
		}
	outputClosed:
		// Retain historical diagnostics in Service.Error, but fail drain only
		// for an unresolved continuity defect or a non-recoverable error.
		if err := service.UnrecoveredError(); err != nil {
			runtime.mu.Lock()
			runtime.err = errors.Join(runtime.err, err)
			runtime.mu.Unlock()
		}
		<-service.Done()
	}()
	return runtime, nil
}

type inactiveReactiveOutput struct{}

func (inactiveReactiveOutput) PublishReactiveTemplate(context.Context, *integrations.Manager, reactivetemplate.Update) error {
	return ErrReactiveTemplateComposition
}

func derivedOutputPublisher(output ReactiveTemplateOutput) *derivedpower.Publisher {
	switch value := output.(type) {
	case derivedPowerOutput:
		return value.publisher
	case derivedReactiveOutput:
		return derivedOutputPublisher(value.derived)
	default:
		return nil
	}
}

func (r *reactiveTemplateRuntime) replaceDerivedOutput(publisher *derivedpower.Publisher, ids []reactivetemplate.ProgramID, baseIDs []reactivetemplate.ProgramID) error {
	if r == nil {
		return ErrReactiveTemplateComposition
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused == nil {
		return ErrReactiveTemplateComposition
	}
	base := r.output
	switch value := base.(type) {
	case derivedPowerOutput:
		base = inactiveReactiveOutput{}
	case derivedReactiveOutput:
		base = value.previous
	}
	if publisher == nil {
		r.output = base
	} else {
		r.output = derivedReactiveOutput{previous: base, derived: derivedPowerOutput{publisher: publisher}, priorIDs: append([]reactivetemplate.ProgramID(nil), baseIDs...), newIDs: append([]reactivetemplate.ProgramID(nil), ids...)}
	}
	r.derived = publisher
	return nil
}

// PauseForActivation joins an A callback already removed from Updates. While
// paused the one pump discards A and retains B in its existing queue.
func (r *reactiveTemplateRuntime) PauseForActivation(ctx context.Context) error {
	if r == nil || r.service == nil || r.done == nil || r.forwarding == nil || r.cancel == nil || nilReactiveInterface(ctx) {
		return ErrReactiveTemplateComposition
	}
	r.mu.Lock()
	if r.paused != nil {
		r.mu.Unlock()
		return ErrReactiveTemplateComposition
	}
	p := &templateForwardingPause{before: r.service.Publication(), ack: make(chan struct{}), resume: make(chan struct{})}
	r.paused = p
	r.mu.Unlock()
	select {
	case r.forwarding <- p:
		// Once handed to the pump, acknowledge actual drain even if ctx
		// expires. No untracked callback survives a successful cut.
		select {
		case <-p.ack:
			return nil
		case <-r.done:
			return ErrReactiveTemplateComposition
		}
	case <-ctx.Done():
		r.mu.Lock()
		if r.paused == p {
			r.paused = nil
		}
		r.mu.Unlock()
		return ctx.Err()
	case <-r.done:
		return ErrReactiveTemplateComposition
	}
}

func (r *reactiveTemplateRuntime) ResumeAfterActivation() error {
	if r == nil {
		return ErrReactiveTemplateComposition
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused == nil {
		return ErrReactiveTemplateComposition
	}
	close(r.paused.resume)
	r.paused = nil
	return nil
}

func (r *reactiveTemplateRuntime) CloseAdmissions() {
	if r != nil {
		r.cancel()
		r.service.CloseAdmissions()
	}
}
func (r *reactiveTemplateRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if nilReactiveInterface(ctx) {
		return ErrReactiveTemplateComposition
	}
	r.CloseAdmissions()
	select {
	case <-r.done:
		return r.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *reactiveTemplateRuntime) Done() <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}
func (r *reactiveTemplateRuntime) Error() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}
