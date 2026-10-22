package server

import (
	"context"
	"github.com/damione1/Homeward/contracts"
	"sort"
	"sync"

	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
)

type activationPolicies struct{ coordinator *sessionCoordinator }
type activationPolicyReservation struct {
	guards []*sessionGuard
	ticket *mfaPolicyTicket
	once   sync.Once
}

func (p activationPolicies) ReserveReconciliationPolicies(ctx context.Context, accounts []contracts.AccountID) (configactivation.PolicyReservation, error) {
	if p.coordinator == nil || nilReactiveInterface(ctx) || len(accounts) == 0 || len(accounts) > sessionCoordinatorAccountLimit {
		return nil, storage.ErrSessionInvalid
	}
	owned := append([]contracts.AccountID(nil), accounts...)
	sort.Slice(owned, func(i, j int) bool { a, _ := owned[i].Value(); b, _ := owned[j].Value(); return a < b })
	ticket, err := p.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, err
	}
	r := &activationPolicyReservation{ticket: ticket}
	for n, account := range owned {
		if n != 0 && account == owned[n-1] {
			r.Release()
			return nil, storage.ErrSessionInvalid
		}
		guard, err := p.coordinator.enter(ctx, account)
		if err != nil {
			r.Release()
			return nil, err
		}
		r.guards = append(r.guards, guard)
		guard.mutation, err = p.coordinator.authority.BeginMutation(account)
		if err == nil {
			var snapshot storage.AuthorizationPolicySnapshot
			snapshot, err = p.coordinator.store.AuthorizationPolicy(ctx, account)
			if err == nil {
				source, sourceErr := snapshot.Source()
				err = sourceErr
				if err == nil {
					err = guard.mutation.ReservePolicy(source)
				}
			}
		}
		if err != nil {
			r.Release()
			return nil, err
		}
	}
	return r, nil
}

func (p activationPolicies) ReserveActivationPolicies(ctx context.Context, changes []configactivation.PolicyChange) (configactivation.PolicyReservation, error) {
	if p.coordinator == nil || nilReactiveInterface(ctx) || len(changes) == 0 || len(changes) > sessionCoordinatorAccountLimit {
		return nil, storage.ErrSessionInvalid
	}
	owned := append([]configactivation.PolicyChange(nil), changes...)
	sort.Slice(owned, func(i, j int) bool {
		a, _ := owned[i].Account().Value()
		b, _ := owned[j].Account().Value()
		return a < b
	})
	ticket, err := p.coordinator.enterMFAPolicy(ctx)
	if err != nil {
		return nil, err
	}
	r := &activationPolicyReservation{ticket: ticket}
	for index, change := range owned {
		if index != 0 && owned[index-1].Account() == change.Account() {
			r.Release()
			return nil, storage.ErrSessionInvalid
		}
		guard, err := p.coordinator.enter(ctx, change.Account())
		if err != nil {
			r.Release()
			return nil, err
		}
		r.guards = append(r.guards, guard)
		guard.mutation, err = p.coordinator.authority.BeginMutation(change.Account())
		if err == nil {
			err = guard.mutation.ReservePolicy(change.Source())
		}
		if err != nil {
			r.Release()
			return nil, err
		}
	}
	return r, nil
}
func (r *activationPolicyReservation) Publish(ctx context.Context) error {
	if r == nil || nilReactiveInterface(ctx) {
		return storage.ErrSessionInvalid
	}
	for _, guard := range r.guards {
		// Confirm/refresh uses the existing primary reconciliation and retains
		// sessions under their new policy, as ordinary ReplacePolicy does.
		if err := guard.reconcilePrimary(ctx); err != nil {
			return err
		}
	}
	return nil
}
func (r *activationPolicyReservation) Quarantine() {
	if r == nil {
		return
	}
	for _, guard := range r.guards {
		guard.quarantine()
	}
}
func (r *activationPolicyReservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		for n := len(r.guards) - 1; n >= 0; n-- {
			r.guards[n].release()
		}
		r.ticket.release()
	})
}

func prepareConfigurationActivation(startup config.StartupInputs, admitted configactivation.AdmittedGeneration,
	store *storage.Store, projection *sequencer.Sequencer, plan RuntimePlan, configured bool) (*configactivation.Owner, error) {
	if !configured && plan.ConfigureActivation == nil {
		return nil, nil
	}
	repository, ok := plan.Identity.(*registry.Repository)
	if !ok || repository == nil {
		return nil, ErrIdentityNotConfigured
	}
	launch, err := admitted.LaunchConfig()
	if err != nil {
		return nil, err
	}
	settings, err := launch.PublicTransport()
	if err != nil {
		return nil, err
	}
	return configactivation.NewOwner(configactivation.OwnerInput{Store: store, Startup: startup, Current: admitted,
		Registry: repository, Projection: projection, Timeout: settings.ShutdownTimeout()})
}

func attachConfigurationActivation(owner *configactivation.Owner, plan RuntimePlan, host *integrations.Manager,
	commands *integrations.DurableCommandService, runs *automationruns.Controller, templates *reactiveTemplateRuntime,
	supervisor *integrations.Supervisor, sessions *sessionCoordinator, store *storage.Store, sequence *sequencer.Sequencer,
	catalog *contracts.CapabilityCatalog, authorization *cloudAuthorizationCoordinator, secrets *storage.SecretRepository, dependencies runDependencies, observed *observationRuntime) error {
	if owner == nil {
		return nil
	}
	if plan.OperationalCompiler != nil && nilReactiveInterface(plan.OperationalCompiler) {
		return configactivation.ErrInvalidStartup
	}
	runtime := configactivation.Runtime{Host: host, Commands: commands, Runs: runs, Supervisor: supervisor,
		Policies: activationPolicies{coordinator: sessions}, Operational: plan.OperationalCompiler, Sources: plan.SourceCompiler, SourceFactory: plan.SourceFactory}
	if observed != nil {
		runtime.Publication = activationHealthPublication{health: observed.health}
	}
	if absent := nilReactiveInterface(runtime.Operational); absent {
		repository, ok := plan.Identity.(*registry.Repository)
		if !ok {
			return ErrIdentityNotConfigured
		}
		runtime.Operational = &configuredOperationalCompiler{remoteCalendar: plan.remoteCalendar, manifest: plan.scopeManifest, repository: repository, store: store, sequence: sequence,
			host: host, catalog: catalog, authorization: authorization, templates: templates, secrets: secrets,
			solarClock: dependencies.solarClock, workdayClock: dependencies.workdayClock, supervisor: supervisor}
		compiler := runtime.Operational.(*configuredOperationalCompiler)
		if plan.observeOperationalCompiler != nil {
			plan.observeOperationalCompiler(compiler)
		}
		if plan.AutomationTriggers != nil {
			compiler.triggerPlan = *plan.AutomationTriggers
		}
		compiler.workday, _ = plan.WorkdayCalendar.(*activationWorkdayCalendar)
		if compiler.workday == nil {
			compiler.workday = &activationWorkdayCalendar{reader: plan.WorkdayCalendar}
		}
	}
	if templates != nil {
		runtime.Templates, runtime.Forwarding = templates.service, templates
	}
	if composition := plan.AutomationRuns; composition != nil && composition.scripts != nil && composition.scripts.scenes != nil {
		runtime.Scenes = composition.scripts.scenes.service
	}
	return owner.Attach(runtime)
}

type activationHealthPublication struct{ health *observability.HealthCell }

func (p activationHealthPublication) PublishConfigRevision(revision storage.ConfigGenerationRevision) {
	value, err := observability.NewActiveRevision(uint64(revision.Number()))
	if err == nil {
		_ = p.health.PublishActiveRevision(value)
	}
}
