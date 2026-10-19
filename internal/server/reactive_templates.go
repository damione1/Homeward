package server

import (
	"context"
	"errors"
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
	service *reactivetemplate.Service
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	err     error
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
	runtime := &reactiveTemplateRuntime{service: service, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(runtime.done)
		failed := false
		for update := range service.Updates() {
			if failed {
				continue
			}
			if err := composition.output.PublishReactiveTemplate(runCtx, host, update); err != nil {
				runtime.mu.Lock()
				runtime.err = err
				runtime.mu.Unlock()
				failed = true
				service.CloseAdmissions()
			}
		}
		if err := service.Error(); err != nil {
			runtime.mu.Lock()
			runtime.err = errors.Join(runtime.err, err)
			runtime.mu.Unlock()
		}
		<-service.Done()
	}()
	return runtime, nil
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
