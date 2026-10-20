package server

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrInvalidTriggerComposition = errors.New("server: invalid automation trigger composition")

// AutomationTriggerResults is the single read-only consumer of the trigger
// engine's bounded result queue. The callback in RuntimePlan must install its
// consumer and return before Supervisor.Start; NextResult must be read outside
// that callback. It confers no action or mutable service capability.
type AutomationTriggerResults interface {
	NextResult(context.Context) (automationtriggers.Result, error)
	Done() <-chan struct{}
	Error() error
}

// Keep the concrete service out of the callback's dynamic interface value.
// RuntimePlan is trusted Go composition, but this port does not expose Close,
// admissions, calendar replacement, or the service's mutable internals.
type triggerResultReader struct{ service *automationtriggers.Service }

func (r triggerResultReader) NextResult(ctx context.Context) (automationtriggers.Result, error) {
	return r.service.NextResult(ctx)
}
func (r triggerResultReader) Done() <-chan struct{} { return r.service.Done() }
func (r triggerResultReader) Error() error          { return r.service.Error() }

var _ AutomationTriggerResults = triggerResultReader{}

type AutomationRunsInput struct {
	Plan     automationruns.Plan
	Preparer automationruns.Preparer
	Executor automationruns.Executor
}

// AutomationRunsComposition explicitly opts the trusted Run assembly into
// consuming trigger results. Default startup never executes an automation.
type AutomationRunsComposition struct {
	mu       sync.Mutex
	plan     automationruns.Plan
	preparer automationruns.Preparer
	executor automationruns.Executor
	runtime  *automationruns.Controller
	scripts  *scriptRunsComposition
}

func NewAutomationRunsComposition(input AutomationRunsInput) (*AutomationRunsComposition, error) {
	if !input.Plan.Valid() || nilReactiveInterface(input.Preparer) || nilReactiveInterface(input.Executor) {
		return nil, ErrInvalidTriggerComposition
	}
	return &AutomationRunsComposition{plan: input.Plan, preparer: input.Preparer, executor: input.Executor}, nil
}

func validateAutomationRunAssembly(triggers *automationtriggers.Plan, runs *AutomationRunsComposition, callback func(context.Context, AutomationTriggerResults) error) error {
	if runs == nil {
		return nil
	}
	if !runs.plan.Valid() || callback != nil {
		return ErrInvalidTriggerComposition
	}
	if triggers == nil {
		if runs.scripts == nil || len(runs.plan.AutomationIDs()) != 0 {
			return ErrInvalidTriggerComposition
		}
	} else if !triggers.Valid() || !slices.Equal(triggers.AutomationIDs(), runs.plan.AutomationIDs()) {
		return ErrInvalidTriggerComposition
	}
	return nil
}

// Traces exposes a bounded, read-only observation of admissions and refusals.
func (c *AutomationRunsComposition) Traces() ([]automationruns.Trace, uint64) {
	if c == nil {
		return nil, 0
	}
	c.mu.Lock()
	runtime := c.runtime
	c.mu.Unlock()
	return runtime.Traces()
}

func (c *AutomationRunsComposition) Done() <-chan struct{} {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runtime.Done()
}

func prepareAutomationRuns(ctx context.Context, triggers *automationtriggers.Service, composition *AutomationRunsComposition, services scriptRunServices) (*automationruns.Controller, error) {
	if composition == nil {
		return nil, nil
	}
	if composition.scripts != nil {
		services.triggers = triggers
		return prepareScriptRuns(ctx, composition, services)
	}
	if triggers == nil {
		return nil, ErrInvalidTriggerComposition
	}
	composition.mu.Lock()
	defer composition.mu.Unlock()
	if composition.runtime != nil {
		return nil, ErrInvalidTriggerComposition
	}
	runs, err := automationruns.NewController(composition.plan, triggerResultReader{triggers}, composition.preparer, composition.executor)
	if err != nil {
		return nil, err
	}
	if services.history != nil {
		if err := runs.AttachHistoryObservationSink(services.history); err != nil {
			return nil, err
		}
	}
	composition.runtime = runs
	return runs, nil
}

// prepareAutomationTriggers binds an admitted plan to Run's one sequencer.
// The factory is trusted to use NewDiscoveryPilotFactoryWithTriggerEdges for
// the matching MQTT PilotSpec; an external worker cannot call this Go ingress.
func prepareAutomationTriggers(ctx context.Context, plan *automationtriggers.Plan, sequence *sequencer.Sequencer, host *integrations.Manager, pilots []integrations.PilotSpec, clock automationtriggers.Clock) (*automationtriggers.Service, error) {
	if plan == nil {
		return nil, nil
	}
	if !plan.Valid() || sequence == nil || host == nil || nilReactiveInterface(clock) {
		return nil, ErrInvalidTriggerComposition
	}
	if filters := plan.MQTTFilters(); len(filters) != 0 {
		instance, present := plan.MQTTInstance()
		if !present {
			return nil, ErrInvalidTriggerComposition
		}
		matching := 0
		for _, pilot := range pilots {
			id, err := pilot.Instance()
			if err != nil {
				return nil, err
			}
			if id == instance {
				matching++
			}
		}
		if matching != 1 {
			return nil, ErrInvalidTriggerComposition
		}
		if err := host.EnableNativeMQTTEdges(instance); err != nil {
			return nil, err
		}
	}
	service, err := automationtriggers.NewService(*plan, sequence, clock)
	if err != nil {
		return nil, err
	}
	if err := service.Start(ctx); err != nil {
		_ = service.Close()
		return nil, err
	}
	return service, nil
}

// The proof carries the actual MQTT factory, not a parallel assertion about
// an arbitrary PilotSpec. Install it once and reject a competing instance.
// Legacy 040 plans without transient waits retain their existing assembly.
func installAutomationTriggerMQTTPilot(plan *RuntimePlan) error {
	if plan == nil {
		return ErrInvalidTriggerComposition
	}
	if plan.AutomationTriggers == nil {
		return nil
	}
	proof, present := plan.AutomationTriggers.MQTTIngress()
	if !present {
		return nil
	}
	instance, err := proof.Instance()
	if err != nil || !proof.Covers(instance, plan.AutomationTriggers.MQTTFilters()) {
		return ErrInvalidTriggerComposition
	}
	for _, pilot := range plan.Pilots {
		id, err := pilot.Instance()
		if err != nil || id == instance {
			return ErrInvalidTriggerComposition
		}
	}
	spec, err := proof.PilotSpec()
	if err != nil {
		return err
	}
	plan.Pilots = append(plan.Pilots, spec)
	return nil
}

func closeTriggerPreparedRuntime(host *integrations.Manager, service *automationtriggers.Service, runs *automationruns.Controller, composition *AutomationRunsComposition, closeOther func(context.Context, <-chan struct{}) error) error {
	stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	service.CloseAdmissions()
	if runs != nil {
		runs.BeginClose()
	}
	sceneDone := beginCloseScriptScenes(composition)
	if host != nil {
		_ = host.CloseAdmissions()
	}
	triggerErr := drainAutomationTriggers(stopCtx, service)
	runErr := drainAutomationRuns(stopCtx, runs)
	sceneErr := drainScriptScenes(stopCtx, composition)
	return errors.Join(triggerErr, runErr, sceneErr, closeOther(stopCtx, joinedAutomationDone(service.Done(), runs.Done(), sceneDone)))
}

func joinedAutomationDone(owners ...<-chan struct{}) <-chan struct{} {
	drains := make([]<-chan struct{}, 0, len(owners))
	for _, owner := range owners {
		if owner != nil {
			drains = append(drains, owner)
		}
	}
	if len(drains) == 0 {
		return nil
	}
	if len(drains) == 1 {
		return drains[0]
	}
	done := make(chan struct{})
	go func() {
		for _, owner := range drains {
			<-owner
		}
		close(done)
	}()
	return done
}

func drainAutomationRuns(ctx context.Context, runs *automationruns.Controller) error {
	if runs == nil {
		return nil
	}
	select {
	case <-runs.BeginClose():
		return runs.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func drainAutomationTriggers(ctx context.Context, service *automationtriggers.Service) error {
	if service == nil {
		return nil
	}
	done := service.BeginClose()
	select {
	case <-done:
		return service.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}
