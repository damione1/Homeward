// Package server composes the cancellable runtime, supervised pilots, and one
// public HTTP transport. Business API handlers belong to later tickets.
package server

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/neviweb"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/worker"
	"github.com/damione1/Homeward/scope"
)

type ListenFunc func(context.Context, string, string) (net.Listener, error)

var ErrInvalidContext = errors.New("server: missing context")
var ErrIdentityNotConfigured = errors.New("server: integration identity authority is not configured")

type unconfiguredIdentity struct{}

func (unconfiguredIdentity) Resolve(context.Context, domain.IntegrationInstanceID, []integrations.SourceKey) ([]integrations.ResolvedIdentity, error) {
	return nil, ErrIdentityNotConfigured
}

// RuntimePlan declares the runtime before serving. Configure must complete
// synchronously, honor ctx, and register any durable service on host. The
// server owns those services until Supervisor.Start succeeds; then the
// supervisor drains them before PostgreSQL closes.
type RuntimePlan struct {
	Identity                    integrations.IdentityAuthority
	Pilots                      []integrations.PilotSpec
	Configure                   func(context.Context, *integrations.Manager, *storage.Store) error
	ConfigureCloudAuthorization func(context.Context, CloudAuthorizationComposition) error
	Public                      *PublicPlan
}

// RuntimeFactory is trusted server composition. The default runtime has no
// configured identity source, pilots, or services. Test assemblies use this
// same Run path after the same PostgreSQL migration.
type RuntimeFactory func(context.Context, *storage.Store) (RuntimePlan, error)

func Run(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr)) error {
	return RunWithFactory(ctx, c, listen, ready, nil)
}

func RunWithFactory(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr), factory RuntimeFactory) error {
	if ctx == nil || listen == nil {
		return ErrInvalidContext
	}
	settings, err := c.PostgresSettings()
	if err != nil {
		return err
	}
	if _, err := c.ListenEndpoint(); err != nil {
		return err
	}
	publicSettings, err := c.PublicTransport()
	if err != nil {
		return err
	}
	dataDir, err := c.DataDir()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	servingTransport, err := preparePublicServingTransport(c, publicSettings)
	if err != nil {
		return err
	}
	store, err := storage.Open(ctx, settings)
	if err != nil {
		return err
	}
	if _, err := store.Migrate(ctx); err != nil {
		store.Close()
		return err
	}
	secrets, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		store.Close()
		return err
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		store.Close()
		return err
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		store.Close()
		return err
	}
	plan := RuntimePlan{Identity: unconfiguredIdentity{}}
	if factory != nil {
		plan, err = factory(ctx, store)
		if err != nil {
			store.Close()
			return err
		}
	} else {
		mqttSettings, mqttEnabled := c.MQTTSettings()
		neviwebSettings, neviwebEnabled := c.NeviwebSettings()
		workers := c.ExternalWorkers()
		if mqttEnabled || len(workers) != 0 || neviwebEnabled || len(c.EnergyBindings()) != 0 {
			var cloud []neviwebComposition
			if neviwebEnabled {
				initial, e := neviweb.InitialSessionValue()
				if e != nil {
					store.Close()
					return e
				}
				if _, e = secrets.InitializeCredentialIfAbsent(ctx, neviwebSettings.SessionHandle(), initial); e != nil {
					store.Close()
					return e
				}
				cloud = append(cloud, neviwebComposition{settings: neviwebSettings, catalog: catalog})
			}
			plan, err = defaultConfiguredRuntimePlan(mqttSettings, mqttEnabled, workers, store, sequence, cloud...)
			if err != nil {
				store.Close()
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		store.Close()
		return err
	}
	host, err := integrations.NewHost(plan.Identity, sequence, catalog)
	if err != nil {
		store.Close()
		return err
	}
	if err := host.AttachSecrets(secrets); err != nil {
		store.Close()
		return err
	}
	authorization, err := newCloudAuthorizationCoordinator(ctx, host, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		return closePreparedRuntime(host, nil, store, err)
	}
	if plan.Configure != nil {
		if err := plan.Configure(ctx, host, store); err != nil {
			return closePreparedRuntime(host, authorization, store, err)
		}
	}
	if plan.ConfigureCloudAuthorization != nil {
		if err := plan.ConfigureCloudAuthorization(ctx, authorization.composition()); err != nil {
			return closePreparedRuntime(host, authorization, store, err)
		}
	}
	energySpecs, err := configuredEnergySpecs(ctx, c, store, sequence)
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err)
	}
	plan.Pilots = append(plan.Pilots, energySpecs...)
	authorization.freeze()
	if err := ctx.Err(); err != nil {
		return closePreparedRuntime(host, authorization, store, err)
	}
	plan.Public, err = registerAccountRoutes(plan.Public, store)
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err)
	}
	public, err := compilePublicPlan(plan.Public, publicSettings)
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err)
	}
	supervisor, err := integrations.NewSupervisor(host, plan.Pilots, integrations.DefaultRetryPolicy(), integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err)
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := supervisor.Start(ctx); err != nil {
		lifecycleCancel()
		return closePreparedRuntime(host, authorization, store, err)
	}
	if err := authorization.openAdmissions(); err != nil {
		lifecycleCancel()
		_ = host.CloseAdmissions()
		authorization.CloseAdmissions()
		stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		stopErr := supervisor.Stop(stopCtx)
		authorizationErr := authorization.Close(stopCtx)
		cancel()
		retainStoreUntilDrained(store, supervisor.Done(), authorization.Done())
		return errors.Join(err, stopErr, authorizationErr)
	}
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			lifecycleCancel()
		case <-watchStop:
		}
	}()
	operator, operatorStartErr := startOperatorListener(lifecycleCtx, dataDir, store)
	publicDone := closedPublicDone()
	listenerErr := operatorStartErr
	if operatorStartErr == nil {
		publicDone, listenerErr = runPublicListenerPrepared(lifecycleCtx, c, listen, ready, public, publicSettings, servingTransport)
	}
	_ = host.CloseAdmissions()
	authorization.CloseAdmissions()
	close(watchStop)
	<-watchDone
	lifecycleCancel()
	stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	authorizationStop := make(chan error, 1)
	go func() { authorizationStop <- authorization.Close(stopCtx) }()
	operatorDone, operatorErr := operator.stop(stopCtx)
	stopErr := supervisor.Stop(stopCtx)
	authorizationErr := <-authorizationStop
	cancel()
	retainStoreUntilDrained(store, publicDone, operatorDone, supervisor.Done(), authorization.Done())
	return errors.Join(listenerErr, operatorErr, stopErr, authorizationErr)
}

// defaultMQTTRuntimePlan is the sole composition used by Run's configured
// MQTT path. It never configures identities from discovery; the repository
// resolves only bindings explicitly persisted by an operator.
func defaultMQTTRuntimePlan(settings mqtt.Settings, store *storage.Store, sequence *sequencer.Sequencer) (RuntimePlan, error) {
	return defaultConfiguredRuntimePlan(settings, true, nil, store, sequence)
}

type neviwebComposition struct {
	settings neviweb.Settings
	catalog  *contracts.CapabilityCatalog
}

func defaultConfiguredRuntimePlan(mqttSettings mqtt.Settings, mqttEnabled bool, workers []worker.Config, store *storage.Store, sequence *sequencer.Sequencer, cloud ...neviwebComposition) (RuntimePlan, error) {
	if len(cloud) > 1 {
		return RuntimePlan{}, integrations.ErrInvalidInput
	}
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		return RuntimePlan{}, err
	}
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		return RuntimePlan{}, err
	}
	specs := make([]integrations.PilotSpec, 0, len(workers)+1)
	if mqttEnabled {
		instance, err := mqttSettings.Instance()
		if err != nil {
			return RuntimePlan{}, err
		}
		pilotFactory, err := mqtt.NewDiscoveryPilotFactory(mqttSettings, repository)
		if err != nil {
			return RuntimePlan{}, err
		}
		spec, err := integrations.NewPilotSpec(instance, pilotFactory)
		if err != nil {
			return RuntimePlan{}, err
		}
		specs = append(specs, spec)
	}
	for _, configured := range workers {
		spec, err := worker.NewPilotSpec(configured)
		if err != nil {
			return RuntimePlan{}, err
		}
		specs = append(specs, spec)
	}
	if len(cloud) == 1 {
		settings := cloud[0].settings
		instance, e := settings.Instance()
		if e != nil {
			return RuntimePlan{}, e
		}
		decision, e := heatingownership.Parse(scope.HeatingOwnershipV1(), manifest)
		if e != nil {
			return RuntimePlan{}, e
		}
		limit, _, zone := settings.Budget()
		quota, e := storage.NewCloudAttemptQuota(store, settings.AccountText(), limit, zone)
		if e != nil {
			return RuntimePlan{}, e
		}
		limits := integrations.DefaultCloudLimits().Input()
		limits.MaxInFlight = 2
		limits.ObservationAttempts = 2
		limits.AttemptTimeout = 30 * time.Second
		limits.TotalTimeout = 65 * time.Second
		limits.RefreshBudget = 30 * time.Second
		admitted, e := integrations.AdmitCloudLimits(limits)
		if e != nil {
			return RuntimePlan{}, e
		}
		accountBinding, e := integrations.NewCloudAccountBinding(instance, settings.Account())
		if e != nil {
			return RuntimePlan{}, e
		}
		policy, e := integrations.NewCloudPolicy(admitted, []integrations.CloudAccountBinding{accountBinding}, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
		if e != nil {
			return RuntimePlan{}, e
		}
		factory, e := neviweb.NewPilotFactory(settings, repository, decision, cloud[0].catalog, policy, quota)
		if e != nil {
			return RuntimePlan{}, e
		}
		spec, e := integrations.NewPilotSpec(instance, factory)
		if e != nil {
			return RuntimePlan{}, e
		}
		specs = append(specs, spec)
	}
	return RuntimePlan{Identity: repository, Pilots: specs, Configure: func(ctx context.Context, _ *integrations.Manager, _ *storage.Store) error {
		if len(cloud) == 1 {
			instance, e := cloud[0].settings.Instance()
			if e != nil {
				return e
			}
			for _, binding := range cloud[0].settings.Bindings() {
				if _, e := repository.ConfigureBinding(ctx, instance, binding.SourceKey(), binding.Alias()); e != nil {
					return e
				}
			}
		}
		for _, configured := range workers {
			instance, err := configured.Instance()
			if err != nil {
				return err
			}
			for _, binding := range configured.Bindings() {
				key, alias, err := binding.Components()
				if err != nil {
					return err
				}
				if _, err := repository.ConfigureBinding(ctx, instance, key, alias); err != nil {
					return err
				}
			}
		}
		return repository.AttachProjection(ctx, sequence)
	}}, nil
}

func closePreparedRuntime(host *integrations.Manager, authorization *cloudAuthorizationCoordinator, store *storage.Store, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	if authorization != nil {
		authorization.CloseAdmissions()
	}
	authorizationStop := make(chan error, 1)
	if authorization != nil {
		go func() { authorizationStop <- authorization.Close(ctx) }()
	}
	done, closeErr := host.ClosePreparedRuntime(ctx)
	var authorizationErr error
	if authorization != nil {
		authorizationErr = <-authorizationStop
	}
	cancel()
	if authorization != nil {
		retainStoreUntilDrained(store, done, authorization.Done())
	} else {
		retainStoreUntilDrained(store, done)
	}
	return errors.Join(cause, closeErr, authorizationErr)
}

// A timed-out drain keeps PostgreSQL until every owner really finishes. A nil
// Done means that preparation never created that owner.
func retainStoreUntilDrained(store *storage.Store, drains ...<-chan struct{}) {
	allDone := true
	for _, done := range drains {
		if done == nil {
			continue
		}
		select {
		case <-done:
		default:
			allDone = false
		}
	}
	if allDone {
		store.Close()
		return
	}
	go func() {
		for _, done := range drains {
			if done != nil {
				<-done
			}
		}
		store.Close()
	}()
}

func runListener(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr)) error {
	settings, err := c.PublicTransport()
	if err != nil {
		return err
	}
	public, err := compilePublicPlan(nil, settings)
	if err != nil {
		return err
	}
	_, err = runPublicListener(ctx, c, listen, ready, public, settings)
	return err
}
