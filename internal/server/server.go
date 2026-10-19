// Package server composes the cancellable runtime, supervised pilots, and one
// public HTTP transport. Business API handlers belong to later tickets.
package server

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/neviweb"
	"github.com/damione1/Homeward/internal/recorder"
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
// server owns those services until Supervisor.Start succeeds; then Supervisor
// drains the command service. Run also joins its job scheduler before closing
// PostgreSQL, using the same shutdown budget for both owners.
type RuntimePlan struct {
	Identity                          integrations.IdentityAuthority
	Pilots                            []integrations.PilotSpec
	Configure                         func(context.Context, *integrations.Manager, *storage.Store) error
	ConfigureCloudAuthorization       func(context.Context, CloudAuthorizationComposition) error
	ConfigureAuthorization            func(context.Context, AuthorizationComposition) error
	ReactiveTemplates                 *ReactiveTemplateComposition
	DurableJobs                       *DurableJobsComposition
	DurableCommands                   *DurableCommandsComposition
	AutomationTriggers                *automationtriggers.Plan
	ConfigureAutomationTriggerResults func(context.Context, AutomationTriggerResults) error
	AutomationRuns                    *AutomationRunsComposition
	Public                            *PublicPlan
}

// RuntimeFactory is trusted server composition. The default runtime has no
// configured identity source, pilots, or services. Test assemblies use this
// same Run path after the same PostgreSQL migration.
type RuntimeFactory func(context.Context, *storage.Store) (RuntimePlan, error)

func Run(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr)) error {
	return RunWithFactory(ctx, c, listen, ready, nil)
}

func RunWithFactory(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr), factory RuntimeFactory) error {
	return runWithDependencies(ctx, c, listen, ready, factory, runDependencies{})
}

func runWithDependencies(ctx context.Context, c config.Config, listen ListenFunc, ready func(net.Addr), factory RuntimeFactory, dependencies runDependencies) error {
	if ctx == nil || listen == nil {
		return ErrInvalidContext
	}
	settings, err := c.PostgresSettings()
	if err != nil {
		return err
	}
	recorderLimits, err := c.RecorderLimits()
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
	// Versioned security metadata precedes factory/operator account creation,
	// including deployments with no HTTPS/provider or existing secret file.
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		store.Close()
		return err
	}
	secrets, err := store.BootstrapSecrets(ctx, dataDir)
	if err != nil {
		store.Close()
		return err
	}
	if dependencies.sequenceClock == nil {
		dependencies.sequenceClock = sequencer.RealClock{}
	}
	if dependencies.solarClock == nil {
		dependencies.solarClock = automationtriggers.RealClock{}
	}
	sequence, err := sequencer.New(dependencies.sequenceClock, 16384)
	if err != nil {
		store.Close()
		return err
	}
	recording, err := prepareRecorder(ctx, sequence, store, recorderLimits)
	if err != nil {
		store.Close()
		return err
	}
	hiloSettings, hiloEnabled := c.HiloSettings()
	units, err := newRunUnitCatalog(hiloEnabled)
	if err != nil {
		return closeStoreWithRecorder(store, recording, err)
	}
	catalog, err := contracts.NewCapabilityCatalog(units)
	if err != nil {
		return closeStoreWithRecorder(store, recording, err)
	}
	plan := RuntimePlan{Identity: unconfiguredIdentity{}}
	var hiloRun *hiloComposition
	if factory != nil {
		plan, err = factory(ctx, store)
		if err != nil {
			return closeStoreWithRecorder(store, recording, err)
		}
	} else {
		activeGeneration, hasActiveGeneration, e := store.ReadActiveConfig(ctx)
		if e != nil {
			return closeStoreWithRecorder(store, recording, e)
		}
		var activeCandidate bundle.Candidate
		var hasActiveHelpers bool
		if hasActiveGeneration {
			activeCandidate, e = activeGeneration.Restore()
			if e != nil {
				return closeStoreWithRecorder(store, recording, e)
			}
			definitions, definitionErr := helpers.Definitions(activeCandidate)
			if definitionErr != nil {
				return closeStoreWithRecorder(store, recording, definitionErr)
			}
			hasActiveHelpers = len(definitions) != 0
		}
		mqttSettings, mqttEnabled := c.MQTTSettings()
		neviwebSettings, neviwebEnabled := c.NeviwebSettings()
		workers := c.ExternalWorkers()
		solarSettings, solarEnabled := c.SolarSettings()
		if mqttEnabled || len(workers) != 0 || neviwebEnabled || hiloEnabled || len(c.EnergyBindings()) != 0 || hasActiveHelpers || solarEnabled {
			var cloud []neviwebComposition
			if neviwebEnabled {
				initial, e := neviweb.InitialSessionValue()
				if e != nil {
					return closeStoreWithRecorder(store, recording, e)
				}
				if _, e = secrets.InitializeCredentialIfAbsent(ctx, neviwebSettings.SessionHandle(), initial); e != nil {
					return closeStoreWithRecorder(store, recording, e)
				}
				cloud = append(cloud, neviwebComposition{settings: neviwebSettings, catalog: catalog})
			}
			plan, err = defaultConfiguredRuntimePlan(mqttSettings, mqttEnabled, workers, store, sequence, cloud...)
			if err != nil {
				return closeStoreWithRecorder(store, recording, err)
			}
			if hasActiveHelpers {
				if err := addActiveHelpers(&plan, activeCandidate, store); err != nil {
					return closeStoreWithRecorder(store, recording, err)
				}
			}
			if solarEnabled {
				if err := addConfiguredSolar(&plan, solarSettings, dependencies.solarClock); err != nil {
					return closeStoreWithRecorder(store, recording, err)
				}
			}
			if hiloEnabled {
				repository, ok := plan.Identity.(*registry.Repository)
				if !ok {
					return closeStoreWithRecorder(store, recording, ErrIdentityNotConfigured)
				}
				hiloRun, err = newHiloComposition(hiloSettings, store, sequence, catalog, repository)
				if err != nil {
					return closeStoreWithRecorder(store, recording, err)
				}
				if err := hiloRun.augment(&plan); err != nil {
					return closeStoreWithRecorder(store, recording, err)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return closeStoreWithRecorder(store, recording, err)
	}
	if err := validateAutomationRunAssembly(plan.AutomationTriggers, plan.AutomationRuns, plan.ConfigureAutomationTriggerResults); err != nil {
		return closeStoreWithRecorder(store, recording, err)
	}
	commandComposition, err := resolveDurableCommands(plan)
	if err != nil {
		return closeStoreWithRecorder(store, recording, err)
	}
	if err := installAutomationTriggerMQTTPilot(&plan); err != nil {
		return closeStoreWithRecorder(store, recording, err)
	}
	var sessionClockSource sessionClock = realSessionClock{}
	if plan.Public != nil && plan.Public.sessionClock != nil {
		sessionClockSource = plan.Public.sessionClock
	}
	sessions, err := prepareLocalAuthorizationWithSettings(ctx, store, sessionClockSource, publicSettings)
	if err != nil {
		return closeStoreWithRecorder(store, recording, err)
	}
	sessions.mfa, err = newMFARuntime(ctx, sessions, secrets, publicSettings)
	if err != nil {
		sessions.policy.closeAdmissions()
		return closeStoreWithRecorder(store, recording, err)
	}
	host, err := integrations.NewHost(plan.Identity, sequence, catalog)
	if err != nil {
		// No route has admitted work yet; join this newly created owner before DB.
		mfaErr := sessions.mfa.close(context.Background())
		return closeStoreWithRecorder(store, recording, errors.Join(err, mfaErr))
	}
	if err := host.AttachSecrets(secrets); err != nil {
		return closePreparedRuntime(host, nil, store, err, nil, recording, nil, sessions.mfa)
	}
	if err := host.AttachAuthorization(sessions.authority); err != nil {
		return closePreparedRuntime(host, nil, store, err, nil, recording, nil, sessions.mfa)
	}
	authorization, err := newCloudAuthorizationCoordinator(ctx, host, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		return closePreparedRuntime(host, nil, store, err, nil, recording, nil, sessions.mfa)
	}
	if plan.Configure != nil {
		if err := plan.Configure(ctx, host, store); err != nil {
			return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	if plan.ConfigureAuthorization != nil {
		composition, err := authorizationComposition(ctx, sessions, sequence, plan.Identity)
		if err != nil {
			return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
		if err := plan.ConfigureAuthorization(ctx, composition); err != nil {
			return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	if dependencies.observe != nil {
		if err := dependencies.observe(ctx, sequence); err != nil {
			return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	if plan.ConfigureCloudAuthorization != nil {
		if err := plan.ConfigureCloudAuthorization(ctx, authorization.composition()); err != nil {
			return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	energySpecs, err := configuredEnergySpecs(ctx, c, store, sequence)
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
	}
	plan.Pilots = append(plan.Pilots, energySpecs...)
	if hiloRun != nil {
		hiloEnergy, err := hiloRun.energySpec(ctx)
		if err != nil {
			return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
		plan.Pilots = append(plan.Pilots, hiloEnergy)
	}
	// Register the unique command authority before constructing any borrower
	// and before Supervisor freezes Host, even when no durable jobs exist.
	commands, err := prepareDurableCommands(ctx, host, store, commandComposition)
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
	}
	reactiveComposition, err := scriptReactiveTemplateComposition(plan.ReactiveTemplates, plan.AutomationRuns)
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
	}
	reactive, err := startReactiveTemplates(ctx, plan.Identity, host, sequence, reactiveComposition)
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err, nil, recording, nil, sessions.mfa)
	}
	var triggerClock automationtriggers.Clock = automationtriggers.RealClock{}
	if plan.AutomationRuns != nil && plan.AutomationRuns.scripts != nil {
		triggerClock = plan.AutomationRuns.scripts.input.Clock
	}
	triggers, err := prepareAutomationTriggers(ctx, plan.AutomationTriggers, sequence, host, plan.Pilots, triggerClock)
	if err != nil {
		return closePreparedRuntime(host, authorization, store, err, reactive, recording, nil, sessions.mfa)
	}
	var runs *automationruns.Controller
	if plan.ConfigureAutomationTriggerResults != nil {
		if triggers == nil {
			return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
				return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, ErrInvalidTriggerComposition, reactive, recording, nil, sessions.mfa, triggerDone)
			})
		}
		if err := plan.ConfigureAutomationTriggerResults(ctx, triggerResultReader{triggers}); err != nil {
			return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
				return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
			})
		}
	}
	scriptServices := scriptRunServices{host: host, projection: sequence, commands: commands}
	if reactive != nil {
		scriptServices.templates = reactive.service
	}
	runs, err = prepareAutomationRuns(ctx, triggers, plan.AutomationRuns, scriptServices)
	if err != nil {
		return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
		})
	}
	authorization.freeze()
	if err := ctx.Err(); err != nil {
		return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
		})
	}
	// Preserve prepared owners until composition succeeds; errors return nil
	// outputs, but cleanup must still drain the original MFA runtime.
	composedPublic, composedSessions, err := composeSessionsWithCoordinator(plan.Public, store, sessions)
	if err != nil {
		return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
		})
	}
	plan.Public, sessions = composedPublic, composedSessions
	public, err := compilePublicPlan(plan.Public, publicSettings)
	if err != nil {
		return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
		})
	}
	// The one command service must be registered before Supervisor freezes Host.
	// Jobs borrow it; Supervisor owns its drain. No SQL runs under Host's lock.
	durableJobs, err := newDurableJobsRuntimeWithCommands(ctx, store, host, sessions, plan.DurableJobs, commands)
	if err != nil {
		durableJobs.CloseAdmissions()
		return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, err, reactive, recording, durableJobs, sessions.mfa, triggerDone)
		})
	}
	supervisor, err := integrations.NewSupervisor(host, plan.Pilots, integrations.DefaultRetryPolicy(), integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		durableJobs.CloseAdmissions()
		return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, err, reactive, recording, durableJobs, sessions.mfa, triggerDone)
		})
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := supervisor.Start(ctx); err != nil {
		lifecycleCancel()
		durableJobs.CloseAdmissions()
		return closeTriggerPreparedRuntime(host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closePreparedRuntimeWithContext(stopCtx, host, authorization, store, err, reactive, recording, durableJobs, sessions.mfa, triggerDone)
		})
	}
	err = authorization.openAdmissions()
	if err == nil {
		err = durableJobs.Start(ctx)
	}
	if err == nil && runs != nil {
		err = runs.Start(ctx)
	}
	if err != nil {
		durableJobs.CloseAdmissions()
		lifecycleCancel()
		_ = host.CloseAdmissions()
		authorization.CloseAdmissions()
		reactive.CloseAdmissions()
		triggers.CloseAdmissions()
		runs.CloseAdmissions()
		if runs != nil {
			runs.BeginClose()
		}
		sceneDone := beginCloseScriptScenes(plan.AutomationRuns)
		mfaDone := sessions.mfa.beginClose()
		stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		jobsStop := make(chan error, 1)
		go func() { jobsStop <- durableJobs.Close(stopCtx) }()
		stopErr := supervisor.Stop(stopCtx)
		authorizationErr := authorization.Close(stopCtx)
		reactiveErr := reactive.Close(stopCtx)
		triggerErr := drainAutomationTriggers(stopCtx, triggers)
		runErr := drainAutomationRuns(stopCtx, runs)
		sceneErr := drainScriptScenes(stopCtx, plan.AutomationRuns)
		mfaErr := sessions.mfa.close(stopCtx)
		jobsErr := <-jobsStop
		recorderErr := drainRecorder(stopCtx, recording)
		cancel()
		retainStoreUntilDrained(store, supervisor.Done(), authorization.Done(), reactive.Done(), triggers.Done(), runs.Done(), sceneDone, mfaDone, durableJobs.Done(), recording.Done())
		return errors.Join(err, stopErr, authorizationErr, reactiveErr, triggerErr, runErr, sceneErr, mfaErr, jobsErr, recorderErr)
	}
	var reactiveDone <-chan struct{}
	if reactive != nil {
		reactiveDone = reactive.Done()
	}
	var triggerDone <-chan struct{}
	if triggers != nil {
		triggerDone = triggers.ReaderDone()
	}
	runsDone := runs.Done()
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			durableJobs.CloseAdmissions()
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			reactive.CloseAdmissions()
			triggers.CloseAdmissions()
			runs.CloseAdmissions()
			lifecycleCancel()
		case <-reactiveDone:
			durableJobs.CloseAdmissions()
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			triggers.CloseAdmissions()
			runs.CloseAdmissions()
			lifecycleCancel()
		case <-triggerDone:
			durableJobs.CloseAdmissions()
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			reactive.CloseAdmissions()
			triggers.CloseAdmissions()
			runs.CloseAdmissions()
			lifecycleCancel()
		case <-runsDone:
			durableJobs.CloseAdmissions()
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			reactive.CloseAdmissions()
			triggers.CloseAdmissions()
			runs.CloseAdmissions()
			lifecycleCancel()
		case <-watchStop:
		}
	}()
	operator, operatorStartErr := startOperatorListener(lifecycleCtx, dataDir, store, sessions)
	publicDone := closedPublicDone()
	listenerErr := operatorStartErr
	if operatorStartErr == nil {
		publicDone, listenerErr = runPublicListenerPrepared(lifecycleCtx, c, listen, ready, public, publicSettings, servingTransport)
	}
	durableJobs.CloseAdmissions()
	_ = host.CloseAdmissions()
	authorization.CloseAdmissions()
	reactive.CloseAdmissions()
	triggers.CloseAdmissions()
	runs.CloseAdmissions()
	close(watchStop)
	<-watchDone
	lifecycleCancel()
	if runs != nil {
		runs.BeginClose()
	}
	sceneDone := beginCloseScriptScenes(plan.AutomationRuns)
	mfaDone := sessions.mfa.beginClose()
	stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	authorizationStop := make(chan error, 1)
	go func() { authorizationStop <- authorization.Close(stopCtx) }()
	reactiveStop := make(chan error, 1)
	go func() { reactiveStop <- reactive.Close(stopCtx) }()
	jobsStop := make(chan error, 1)
	go func() { jobsStop <- durableJobs.Close(stopCtx) }()
	operatorDone, operatorErr := operator.stop(stopCtx)
	stopErr := supervisor.Stop(stopCtx)
	authorizationErr := <-authorizationStop
	reactiveErr := <-reactiveStop
	triggerErr := drainAutomationTriggers(stopCtx, triggers)
	runErr := drainAutomationRuns(stopCtx, runs)
	sceneErr := drainScriptScenes(stopCtx, plan.AutomationRuns)
	mfaErr := sessions.mfa.close(stopCtx)
	jobsErr := <-jobsStop
	recorderErr := drainRecorder(stopCtx, recording)
	cancel()
	retainStoreUntilDrained(store, publicDone, operatorDone, supervisor.Done(), authorization.Done(), reactive.Done(), triggers.Done(), runs.Done(), sceneDone, mfaDone, durableJobs.Done(), recording.Done())
	return errors.Join(listenerErr, operatorErr, stopErr, authorizationErr, reactiveErr, triggerErr, runErr, sceneErr, mfaErr, jobsErr, recorderErr)
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

func closePreparedRuntime(host *integrations.Manager, authorization *cloudAuthorizationCoordinator, store *storage.Store, cause error, templates *reactiveTemplateRuntime, recording *recorder.Recorder, durableJobs *durableJobsRuntime, mfa *mfaRuntime) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return closePreparedRuntimeWithContext(ctx, host, authorization, store, cause, templates, recording, durableJobs, mfa)
}

func closePreparedRuntimeWithContext(ctx context.Context, host *integrations.Manager, authorization *cloudAuthorizationCoordinator, store *storage.Store, cause error, templates *reactiveTemplateRuntime, recording *recorder.Recorder, durableJobs *durableJobsRuntime, mfa *mfaRuntime, extraDone ...<-chan struct{}) error {
	mfaDone := mfa.beginClose()
	mfaStop := make(chan error, 1)
	go func() { mfaStop <- mfa.close(ctx) }()
	durableJobs.CloseAdmissions()
	if authorization != nil {
		authorization.CloseAdmissions()
	}
	templates.CloseAdmissions()
	authorizationStop := make(chan error, 1)
	if authorization != nil {
		go func() { authorizationStop <- authorization.Close(ctx) }()
	}
	reactiveStop := make(chan error, 1)
	if templates != nil {
		go func() { reactiveStop <- templates.Close(ctx) }()
	}
	jobsStop := make(chan error, 1)
	go func() { jobsStop <- durableJobs.Close(ctx) }()
	done, closeErr := host.ClosePreparedRuntime(ctx)
	var authorizationErr error
	if authorization != nil {
		authorizationErr = <-authorizationStop
	}
	var reactiveErr error
	if templates != nil {
		reactiveErr = <-reactiveStop
	}
	jobsErr := <-jobsStop
	mfaErr := <-mfaStop
	recorderErr := drainRecorder(ctx, recording)
	drains := []<-chan struct{}{done, authorization.Done(), templates.Done(), durableJobs.Done(), recording.Done(), mfaDone}
	drains = append(drains, extraDone...)
	retainStoreUntilDrained(store, drains...)
	return errors.Join(cause, closeErr, authorizationErr, reactiveErr, jobsErr, mfaErr, recorderErr)
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
