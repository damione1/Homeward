// Package server composes the cancellable runtime, supervised pilots, and one
// public HTTP transport. Business API handlers belong to later tickets.
package server

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/damione1/Homeward/contracts"
	obs "github.com/damione1/Homeward/gen/home/observability/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/backups"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/executionhistory"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/jobs"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/neviweb"
	"github.com/damione1/Homeward/internal/observability"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/recorder"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/workday"
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
	remoteCalendar *remoteCalendarRuntime
	scopeManifest  registry.Manifest
	// Internal observation seam retains the actual composed compiler in tests.
	observeOperationalCompiler        func(*configuredOperationalCompiler)
	ConfigureBackupArchives           func(context.Context, BackupArchiveServices) error
	ConfigureBackups                  func(context.Context, *backups.Registry) error
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
	WorkdayCalendar                   workday.CalendarReader
	ConfigureWorkdayCalendar          func(context.Context, workday.CalendarReader) error
	ConfigureActivation               func(context.Context, *configactivation.Owner) error
	OperationalCompiler               configactivation.OperationalCompiler
}

// RuntimeFactory is trusted server composition. The default runtime has no
// configured identity source, pilots, or services. Test assemblies use this
// same source-bearing SQL admission path.
type RuntimeFactory func(context.Context, *storage.Store) (RuntimePlan, error)

func RunWithStartup(ctx context.Context, startup config.StartupInputs, listen ListenFunc, ready func(net.Addr)) error {
	return RunWithStartupFactory(ctx, startup, listen, ready, nil)
}

func RunWithStartupFactory(ctx context.Context, startup config.StartupInputs, listen ListenFunc, ready func(net.Addr), factory RuntimeFactory) error {
	return runWithDependencies(ctx, startup, listen, ready, factory, runDependencies{})
}

func validRunContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v := reflect.ValueOf(ctx)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return false
		}
	}
	return true
}

func runWithDependencies(ctx context.Context, startup config.StartupInputs, listen ListenFunc, ready func(net.Addr), factory RuntimeFactory, dependencies runDependencies) (returnedErr error) {
	if !validRunContext(ctx) || listen == nil || !startup.Valid() {
		return ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	launch, err := startup.LaunchConfig()
	if err != nil {
		return err
	}
	scopeRevision, err := launch.ScopeRevision()
	if err != nil {
		return err
	}
	scopeManifest, err := registry.LoadManifest(scopeRevision)
	if err != nil {
		return err
	}
	settings, err := launch.PostgresSettings()
	if err != nil {
		return err
	}
	recorderLimits, err := launch.RecorderLimits()
	if err != nil {
		return err
	}
	if _, err := launch.ListenEndpoint(); err != nil {
		return err
	}
	publicSettings, err := launch.PublicTransport()
	if err != nil {
		return err
	}
	dataDir, err := launch.DataDir()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	observed, err := newObservationRuntime()
	if err != nil {
		return err
	}
	servingTransport, err := preparePublicServingTransport(launch, publicSettings)
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
	admitted, _, err := configactivation.ReconcileStartup(ctx, store, startup)
	if err != nil {
		store.Close()
		return err
	}
	observed.revision, err = observability.NewActiveRevision(uint64(admitted.Revision().Number()))
	if err != nil {
		store.Close()
		return err
	}
	c, err := admitted.OperationalConfig()
	if err != nil {
		store.Close()
		return err
	}
	secrets, err := admitted.Secrets()
	if err != nil {
		store.Close()
		return err
	}
	backupProviders, err := prepareBackupProviders(ctx, launch, secrets)
	if err != nil {
		store.Close()
		return err
	}
	defer func() {
		if closeErr := backupProviders.Close(); closeErr != nil {
			returnedErr = errors.Join(returnedErr, closeErr)
		}
	}()
	if dependencies.sequenceClock == nil {
		dependencies.sequenceClock = sequencer.RealClock{}
	}
	if dependencies.solarClock == nil {
		dependencies.solarClock = automationtriggers.RealClock{}
	}
	if dependencies.workdayClock == nil {
		dependencies.workdayClock = automationtriggers.RealClock{}
	}
	sequence, err := sequencer.NewWithObservation(dependencies.sequenceClock, 16384, sequencer.DefaultQueueLimits(), observed.cells)
	if err != nil {
		store.Close()
		return err
	}
	if err := sequence.AttachObservationHealth(observed.health); err != nil {
		store.Close()
		return err
	}
	recording, err := recorder.StartObserved(ctx, sequence, store, recorderLimits, observed.cells, observed.health)
	if err != nil {
		store.Close()
		return err
	}
	observed.sequence, observed.recording, observed.store = sequence, recording, store
	var remoteRuntime *remoteCalendarRuntime
	hiloSettings, hiloEnabled := c.HiloSettings()
	// The fixed admitted unit set also covers a later configured Hilo pilot.
	// No descriptor, client or authorization registration is activated here.
	units, err := newRunUnitCatalog(true)
	if err != nil {
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	catalog, err := contracts.NewCapabilityCatalog(units)
	if err != nil {
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	plan := RuntimePlan{Identity: unconfiguredIdentity{}}
	var hiloRun *hiloComposition
	if factory != nil {
		plan, err = factory(ctx, store)
		if err != nil {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
		}
	} else {
		activeCandidate, hasActiveGeneration := admitted.LegacyCandidate()
		var hasActiveHelpers bool
		if hasActiveGeneration {
			definitions, definitionErr := helpers.Definitions(activeCandidate)
			if definitionErr != nil {
				return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, definitionErr)
			}
			hasActiveHelpers = len(definitions) != 0
		}
		mqttSettings, mqttEnabled := c.MQTTSettings()
		neviwebSettings, neviwebEnabled := c.NeviwebSettings()
		workers := c.ExternalWorkers()
		solarSettings, solarEnabled := c.SolarSettings()
		workdaySettings, workdayEnabled := c.WorkdaySettings()
		ecobeeSettings, ecobeeEnabled := c.EcobeeSettings()
		_, remoteEnabled := c.RemoteCalendarSettings()
		if mqttEnabled || len(workers) != 0 || neviwebEnabled || hiloEnabled || len(c.EnergyBindings()) != 0 || c.DerivedPowerProfile().Enabled() || hasActiveHelpers || solarEnabled || workdayEnabled || remoteEnabled || ecobeeEnabled {
			var cloud []neviwebComposition
			if neviwebEnabled {
				initial, e := neviweb.InitialSessionValue()
				if e != nil {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, e)
				}
				if _, e = secrets.InitializeCredentialIfAbsent(ctx, neviwebSettings.SessionHandle(), initial); e != nil {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, e)
				}
				cloud = append(cloud, neviwebComposition{settings: neviwebSettings, catalog: catalog})
			}
			plan, err = defaultConfiguredRuntimePlanWithManifest(scopeManifest, mqttSettings, mqttEnabled, workers, store, sequence, cloud...)
			if err != nil {
				return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
			}
			if ecobeeEnabled {
				if err := addConfiguredEcobee(&plan, ecobeeSettings, scopeManifest); err != nil {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
				}
			}
			if hasActiveHelpers {
				if err := addActiveHelpers(&plan, activeCandidate, store); err != nil {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
				}
			}
			if solarEnabled {
				if err := addConfiguredSolar(&plan, solarSettings, dependencies.solarClock); err != nil {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
				}
			}
			if workdayEnabled {
				if err := addConfiguredWorkdayWithManifest(&plan, workdaySettings, dependencies.workdayClock, scopeManifest); err != nil {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
				}
				plan.ConfigureWorkdayCalendar = dependencies.configureWorkdayCalendar
			}
			if hiloEnabled {
				repository, ok := plan.Identity.(*registry.Repository)
				if !ok {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, ErrIdentityNotConfigured)
				}
				hiloRun, err = newHiloCompositionWithManifest(scopeManifest, hiloSettings, store, sequence, catalog, repository)
				if err != nil {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
				}
				if err := hiloRun.augment(&plan); err != nil {
					return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
				}
			}
		}
	}
	// The current launch selection is immutable across operational replay.
	plan.scopeManifest = scopeManifest
	if repository, ok := plan.Identity.(*registry.Repository); ok {
		actual, err := repository.ScopeManifest()
		if err != nil {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
		}
		wantDigest, _ := scopeManifest.Digest()
		actualDigest, _ := actual.Digest()
		wantRevision, _ := scopeManifest.Revision()
		actualRevision, _ := actual.Revision()
		if actualDigest != wantDigest || actualRevision != wantRevision {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, registry.ErrOutsideManifest)
		}
	}
	if plan.ConfigureBackups != nil {
		if err := plan.ConfigureBackups(ctx, backupProviders); err != nil {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
		}
	}
	if plan.ConfigureBackupArchives != nil {
		archives, archiveErr := newBackupArchiveServices(launch, backupProviders, store, secrets)
		if archiveErr != nil {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, archiveErr)
		}
		if err := plan.ConfigureBackupArchives(ctx, archives); err != nil {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
		}
	}
	var derivedConfiguration *derivedPowerConfiguration
	if c.DerivedPowerProfile().Enabled() {
		derivedConfiguration, err = addConfiguredDerivedPower(&plan, c)
		if err != nil {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	// Even an empty configured household has the same SQL registry authority.
	// This does not activate commands/scenes without NHA-115 definitions.
	if _, remoteEnabled := c.RemoteCalendarSettings(); factory == nil || remoteEnabled {
		if _, ok := plan.Identity.(*registry.Repository); !ok {
			if factory != nil && plan.Identity != nil {
				return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, ErrIdentityNotConfigured)
			}
			repository, repositoryErr := registry.NewRepository(store, scopeManifest)
			if repositoryErr != nil {
				return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, repositoryErr)
			}
			plan.Identity = repository
			plan.Configure = func(ctx context.Context, _ *integrations.Manager, _ *storage.Store) error {
				return repository.AttachProjection(ctx, sequence)
			}
		}
	}
	if _, remoteEnabled := c.RemoteCalendarSettings(); factory == nil || remoteEnabled {
		identity, ok := plan.Identity.(*registry.Repository)
		if !ok {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, ErrIdentityNotConfigured)
		}
		clock := dependencies.remoteCalendarClock
		if nilReactiveInterface(clock) {
			clock = automationtriggers.RealClock{}
		}
		remoteRuntime, err = newRemoteCalendarRuntime(store, identity, scopeManifest, clock)
		if err != nil {
			return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
		}
		plan.remoteCalendar = remoteRuntime
		if settings, enabled := c.RemoteCalendarSettings(); enabled {
			if err := remoteRuntime.add(&plan, settings); err != nil {
				return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
			}
		}
		if dependencies.observeRemoteCalendar != nil {
			dependencies.observeRemoteCalendar(remoteRuntime)
		}
	}
	if err := validateAutomationRunAssembly(plan.AutomationTriggers, plan.AutomationRuns, plan.ConfigureAutomationTriggerResults); err != nil {
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	commandComposition, err := resolveDurableCommands(plan)
	if err != nil {
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	activation, err := prepareConfigurationActivation(startup, admitted, store, sequence, plan, factory == nil)
	if err != nil {
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	if activation != nil {
		defer activation.BeginClose()
		if commandComposition == nil {
			commandComposition, err = NewDurableCommandsComposition(DurableCommandsInput{Generation: activation, Clock: realSessionClock{}})
			if err != nil {
				return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
			}
		} else {
			commandComposition.generation = activation
		}
		if plan.ReactiveTemplates == nil {
			empty, planErr := reactivetemplate.NewPlan(nil, nil, time.UTC)
			if planErr == nil {
				empty, planErr = empty.WithScriptWaits(time.UTC)
			}
			if planErr != nil {
				return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, planErr)
			}
			plan.ReactiveTemplates, planErr = NewReactiveTemplateComposition(empty, inactiveReactiveOutput{}, reactivetemplate.RealClock{})
			if planErr != nil {
				return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, planErr)
			}
		}
	}
	if err := installAutomationTriggerMQTTPilot(&plan); err != nil {
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	var sessionClockSource sessionClock = realSessionClock{}
	if plan.Public != nil && plan.Public.sessionClock != nil {
		sessionClockSource = plan.Public.sessionClock
	}
	sessions, err := prepareLocalAuthorizationWithSettings(ctx, store, sessionClockSource, publicSettings)
	if err != nil {
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	sessions.mfa, err = newMFARuntime(ctx, sessions, secrets, publicSettings)
	if err != nil {
		sessions.policy.closeAdmissions()
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, err)
	}
	host, err := integrations.NewHost(plan.Identity, sequence, catalog)
	if err != nil {
		// No route has admitted work yet; join this newly created owner before DB.
		mfaErr := sessions.mfa.close(context.Background())
		return closeRemoteStoreWithRecorder(remoteRuntime, store, recording, errors.Join(err, mfaErr))
	}
	if err := host.AttachObservation(observed.cells, observed.journal); err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, nil, store, err, nil, recording, nil, sessions.mfa)
	}
	observed.host, observed.authority = host, sessions.authority
	if err := host.AttachObservationHealth(observed.health); err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, nil, store, err, nil, recording, nil, sessions.mfa)
	}
	if err := host.AttachSecrets(secrets); err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, nil, store, err, nil, recording, nil, sessions.mfa)
	}
	if err := host.AttachAuthorization(sessions.authority); err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, nil, store, err, nil, recording, nil, sessions.mfa)
	}
	authorization, err := newCloudAuthorizationCoordinator(ctx, host, integrations.DefaultCloudAuthorizationLimits())
	if err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, nil, store, err, nil, recording, nil, sessions.mfa)
	}
	if plan.Configure != nil {
		if err := plan.Configure(ctx, host, store); err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	if derivedConfiguration != nil {
		if err := derivedConfiguration.prepare(ctx, host, &plan); err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	if activation != nil {
		plan.WorkdayCalendar = &activationWorkdayCalendar{reader: plan.WorkdayCalendar}
	}
	if plan.ConfigureWorkdayCalendar != nil {
		if err := workday.ValidateReader(plan.WorkdayCalendar); err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
		if err := plan.ConfigureWorkdayCalendar(ctx, plan.WorkdayCalendar); err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	if plan.ConfigureAuthorization != nil {
		composition, err := authorizationComposition(ctx, sessions, sequence, plan.Identity, activation)
		if err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
		if err := plan.ConfigureAuthorization(ctx, composition); err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	if dependencies.observe != nil {
		if err := dependencies.observe(ctx, sequence); err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	if plan.ConfigureCloudAuthorization != nil {
		if err := plan.ConfigureCloudAuthorization(ctx, authorization.composition()); err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
	}
	energySpecs, err := configuredEnergySpecsWithManifest(ctx, c, store, sequence, scopeManifest)
	if err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
	}
	plan.Pilots = append(plan.Pilots, energySpecs...)
	if hiloRun != nil {
		hiloEnergy, err := hiloRun.energySpec(ctx)
		if err != nil {
			return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
		}
		plan.Pilots = append(plan.Pilots, hiloEnergy)
	}
	// Register the unique command authority before constructing any borrower
	// and before Supervisor freezes Host, even when no durable jobs exist.
	commandWaiters := automationruns.MaxLiveRuns + automationruns.MaxPreparing + jobs.Workers + publicSettings.MaxConcurrentRequests()
	commands, err := prepareDurableCommandsWithGate(ctx, host, store, commandComposition, commandWaiters, publicSettings.ShutdownTimeout())
	if err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
	}
	reactiveComposition, err := scriptReactiveTemplateComposition(plan.ReactiveTemplates, plan.AutomationRuns)
	if err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
	}
	reactive, err := startReactiveTemplates(ctx, plan.Identity, host, sequence, reactiveComposition)
	if err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, nil, recording, nil, sessions.mfa)
	}
	var triggerClock automationtriggers.Clock = automationtriggers.RealClock{}
	if plan.AutomationRuns != nil && plan.AutomationRuns.scripts != nil {
		triggerClock = plan.AutomationRuns.scripts.input.Clock
	}
	if remoteRuntime != nil {
		triggerClock = remoteRuntime.clock
	}
	triggers, err := prepareAutomationTriggers(ctx, plan.AutomationTriggers, sequence, host, plan.Pilots, triggerClock)
	if err != nil {
		return closeRemotePreparedRuntime(remoteRuntime, host, authorization, store, err, reactive, recording, nil, sessions.mfa)
	}
	if err := remoteRuntime.attach(triggers); err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, nil, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
		})
	}
	var runs *automationruns.Controller
	if plan.ConfigureAutomationTriggerResults != nil {
		if triggers == nil {
			return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
				return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, ErrInvalidTriggerComposition, reactive, recording, nil, sessions.mfa, triggerDone)
			})
		}
		if err := plan.ConfigureAutomationTriggerResults(ctx, triggerResultReader{triggers}); err != nil {
			return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
				return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
			})
		}
	}
	epoch, err := sequence.CurrentAdmissionEpoch()
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
		})
	}
	history, err := executionhistory.New(ctx, store, epoch)
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, err, reactive, recording, nil, sessions.mfa, triggerDone)
		})
	}
	maintenance, err := prepareRecorderMaintenanceWithClock(sequence, store, dependencies.maintenanceClock)
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			historyErr := drainExecutionHistoryAfter(stopCtx, history, triggerDone)
			return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, errors.Join(err, historyErr), reactive, recording, nil, sessions.mfa, triggerDone, history.Done())
		})
	}
	if dependencies.observeRecorderMaintenance != nil {
		dependencies.observeRecorderMaintenance(maintenance)
	}
	scriptServices := scriptRunServices{host: host, projection: sequence, commands: commands, history: history}
	if reactive != nil {
		scriptServices.templates = reactive.service
	}
	runs, err = prepareAutomationRuns(ctx, triggers, plan.AutomationRuns, scriptServices)
	if err == nil && runs != nil {
		err = runs.AttachObservation(observed.cells)
	}
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			maintenanceDone := maintenance.BeginClose()
			historyErr := drainExecutionHistoryAfter(stopCtx, history, triggerDone)
			maintenanceErr := drainRecorderMaintenance(stopCtx, maintenance)
			return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, errors.Join(err, historyErr, maintenanceErr), reactive, recording, nil, sessions.mfa, triggerDone, history.Done(), maintenanceDone)
		})
	}
	observed.runs = runs
	authorization.freeze()
	if err := ctx.Err(); err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			maintenanceDone := maintenance.BeginClose()
			historyErr := drainExecutionHistoryAfter(stopCtx, history, triggerDone)
			maintenanceErr := drainRecorderMaintenance(stopCtx, maintenance)
			return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, errors.Join(err, historyErr, maintenanceErr), reactive, recording, nil, sessions.mfa, triggerDone, history.Done(), maintenanceDone)
		})
	}
	listRun, err := newListCompositionWithManifest(store, sessions.authority, scopeManifest)
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			maintenanceDone := maintenance.BeginClose()
			historyErr := drainExecutionHistoryAfter(stopCtx, history, triggerDone)
			maintenanceErr := drainRecorderMaintenance(stopCtx, maintenance)
			return closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, errors.Join(err, historyErr, maintenanceErr), reactive, recording, nil, sessions.mfa, triggerDone, history.Done(), maintenanceDone)
		})
	}
	listDrain := newListRuntimeDrain(listRun)
	closeListPrepared := func(stopCtx context.Context, cause error, jobs *durableJobsRuntime, triggerDone <-chan struct{}) error {
		maintenanceDone := maintenance.BeginClose()
		listDone := listDrain.beginClose()
		historyErr := drainExecutionHistoryAfter(stopCtx, history, triggerDone)
		maintenanceErr := drainRecorderMaintenance(stopCtx, maintenance)
		runtimeErr := closeRemotePreparedRuntimeWithContext(remoteRuntime, stopCtx, host, authorization, store, errors.Join(cause, historyErr, maintenanceErr), reactive, recording, jobs, sessions.mfa, triggerDone, listDone, history.Done(), maintenanceDone)
		return errors.Join(runtimeErr, listDrain.close(stopCtx))
	}
	if dependencies.observeLists != nil {
		dependencies.observeLists(listRun, sessions)
	}
	if err := ctx.Err(); err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	if plan.Public == nil {
		plan.Public, err = NewPublicPlan(nil, nil, accountTransportAudit{store: store})
		if err != nil {
			return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
				return closeListPrepared(stopCtx, err, nil, triggerDone)
			})
		}
	}
	documents, err := prepareConfigDocuments(startup, store, activation, secrets)
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	if documents != nil {
		defer documents.Close()
	}
	if err := registerConfigDocumentsRoutes(plan.Public, documents, sessions); err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	if err := registerIntegrationAdminRoutes(plan.Public, documents, sessions, observed); err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	configPreview, err := prepareConfigPlans(documents, activation, secrets, scopeManifest)
	if err == nil {
		err = registerConfigPlansRoutes(plan.Public, configPreview, documents, sessions)
	}
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	if err := listRun.Register(plan.Public); err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	if err := registerObservationRoutes(plan.Public, observed); err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	if activation != nil {
		core, coreErr := prepareCoreRPC(ctx, sessions, sequence, plan.Identity, activation, commands)
		if coreErr == nil {
			coreErr = registerCoreRPCRoutes(plan.Public, core)
		}
		if coreErr == nil {
			coreErr = registerMCPReads(plan.Public, publicSettings.MCPEnabled(), core, documents, sessions, observed)
		}
		if coreErr != nil {
			return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
				return closeListPrepared(stopCtx, coreErr, nil, triggerDone)
			})
		}
	}
	historyReads, historyReadErr := newHistoryRuntime(store, sessions.authority, sequence, activation.CurrentConfig)
	if historyReadErr == nil {
		historyReadErr = registerHistoryRoutes(plan.Public, historyReads)
	}
	if historyReadErr != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, historyReadErr, nil, triggerDone)
		})
	}
	// Preserve prepared owners until composition succeeds; errors return nil
	// outputs, but cleanup must still drain the original MFA runtime.
	composedPublic, composedSessions, err := composeSessionsWithCoordinator(plan.Public, store, sessions)
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	plan.Public, sessions = composedPublic, composedSessions
	public, err := compilePublicPlan(plan.Public, publicSettings)
	if err != nil {
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, nil, triggerDone)
		})
	}
	observed.public = public
	public.observation = observed
	// The one command service must be registered before Supervisor freezes Host.
	// Jobs borrow it; Supervisor owns its drain. No SQL runs under Host's lock.
	durableJobs, err := newDurableJobsRuntimeWithCommands(ctx, store, host, sessions, plan.DurableJobs, commands)
	if err != nil {
		durableJobs.CloseAdmissions()
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, durableJobs, triggerDone)
		})
	}
	supervisor, err := integrations.NewSupervisor(host, plan.Pilots, integrations.DefaultRetryPolicy(), integrations.RealTimer{}, integrations.RandomJitter{})
	if err != nil {
		durableJobs.CloseAdmissions()
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, durableJobs, triggerDone)
		})
	}
	if activation != nil {
		if err := supervisor.PrepareReplacements(); err != nil {
			durableJobs.CloseAdmissions()
			return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
				return closeListPrepared(stopCtx, err, durableJobs, triggerDone)
			})
		}
	}
	observed.supervisor = supervisor
	if dependencies.observeRuntime != nil {
		dependencies.observeRuntime(observed)
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := supervisor.Start(ctx); err != nil {
		lifecycleCancel()
		durableJobs.CloseAdmissions()
		return closeRemoteTriggerPreparedRuntime(remoteRuntime, host, triggers, runs, plan.AutomationRuns, func(stopCtx context.Context, triggerDone <-chan struct{}) error {
			return closeListPrepared(stopCtx, err, durableJobs, triggerDone)
		})
	}
	err = authorization.openAdmissions()
	if err == nil {
		err = durableJobs.Start(ctx)
	}
	if err == nil {
		err = history.Start()
	}
	if err == nil {
		err = maintenance.Start()
	}
	if err == nil && runs != nil {
		err = runs.Start(ctx)
	}
	if err == nil {
		err = attachConfigurationActivation(activation, plan, host, commands, runs, reactive, supervisor, sessions, store, sequence, catalog, authorization, secrets, dependencies, observed)
		if err == nil && dependencies.observeRemoteActivation != nil {
			dependencies.observeRemoteActivation(activation)
		}
	}
	if err == nil && plan.ConfigureActivation != nil {
		err = plan.ConfigureActivation(ctx, activation)
	}
	var configurationWatcher *configurationWatcherRuntime
	if err == nil {
		configurationWatcher, err = startConfigurationWatcher(lifecycleCtx, documents, dependencies.configurationWatcherClock)
	}
	if err != nil {
		configurationWatcher.BeginClose()
		activation.BeginClose()
		maintenance.BeginClose()
		durableJobs.CloseAdmissions()
		lifecycleCancel()
		_ = host.CloseAdmissions()
		authorization.CloseAdmissions()
		reactive.CloseAdmissions()
		remoteRuntime.closeAdmissions()
		triggers.CloseAdmissions()
		runs.CloseAdmissions()
		if runs != nil {
			runs.BeginClose()
		}
		sceneDone := beginCloseScriptScenes(plan.AutomationRuns)
		mfaDone := sessions.mfa.beginClose()
		listDone := listDrain.beginClose()
		stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		watcherErr := configurationWatcher.Close(stopCtx)
		jobsStop := make(chan error, 1)
		go func() { jobsStop <- durableJobs.Close(stopCtx) }()
		activationErr := activation.Close(stopCtx)
		stopErr := supervisor.Stop(stopCtx)
		authorizationErr := authorization.Close(stopCtx)
		reactiveErr := reactive.Close(stopCtx)
		remoteErr := remoteRuntime.close(stopCtx)
		triggerErr := drainAutomationTriggers(stopCtx, triggers)
		runErr := drainAutomationRuns(stopCtx, runs)
		sceneErr := drainScriptScenes(stopCtx, plan.AutomationRuns)
		historyErr := drainExecutionHistoryAfter(stopCtx, history, runs.Done(), sceneDone, triggers.Done())
		mfaErr := sessions.mfa.close(stopCtx)
		listErr := listDrain.close(stopCtx)
		jobsErr := <-jobsStop
		maintenanceErr := drainRecorderMaintenance(stopCtx, maintenance)
		recorderErr := drainRecorder(stopCtx, recording)
		cancel()
		retainStoreUntilDrained(store, remoteRuntime.Done(), configurationWatcher.Done(), activation.Done(), supervisor.Done(), authorization.Done(), reactive.Done(), triggers.Done(), runs.Done(), sceneDone, history.Done(), mfaDone, listDone, durableJobs.Done(), recording.Done(), maintenance.Done())
		return errors.Join(err, watcherErr, activationErr, stopErr, authorizationErr, reactiveErr, triggerErr, runErr, sceneErr, historyErr, mfaErr, listErr, jobsErr, recorderErr, maintenanceErr, remoteErr)
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
	listenerReady := func(address net.Addr) {
		revision := observed.revision
		configurationReady := true
		if activation != nil {
			stamp, err := activation.CurrentConfig(lifecycleCtx)
			if err != nil {
				configurationReady = false
			} else {
				number, _, err := stamp.Components()
				if err != nil {
					configurationReady = false
				} else {
					revision, err = observability.NewActiveRevision(number)
					configurationReady = err == nil
				}
			}
		}
		if _, err := revision.Wire(); err == nil && configurationReady {
			projectionReady, err := sequence.ObservationProjectionReady()
			if err == nil && projectionReady && host.ObservationCoreOpen() {
				_ = observed.health.PublishReady(revision)
			} else {
				_ = observed.health.PublishCoreUnavailable(obs.CoreCause_CORE_CAUSE_REGISTRY_UNAVAILABLE)
			}
		}
		observed.emit(obs.LogCode_LOG_CODE_PROCESS_READY, obs.LogLevel_LOG_LEVEL_INFO)
		if ready != nil {
			ready(address)
		}
	}
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = observed.health.PublishDraining()
			maintenance.BeginClose()
			durableJobs.CloseAdmissions()
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			reactive.CloseAdmissions()
			remoteRuntime.closeAdmissions()
			triggers.CloseAdmissions()
			runs.CloseAdmissions()
			lifecycleCancel()
		case <-reactiveDone:
			_ = observed.health.PublishCoreUnavailable(obs.CoreCause_CORE_CAUSE_OWNERS_NOT_STARTED)
			maintenance.BeginClose()
			durableJobs.CloseAdmissions()
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			remoteRuntime.closeAdmissions()
			triggers.CloseAdmissions()
			runs.CloseAdmissions()
			lifecycleCancel()
		case <-triggerDone:
			_ = observed.health.PublishCoreUnavailable(obs.CoreCause_CORE_CAUSE_OWNERS_NOT_STARTED)
			maintenance.BeginClose()
			durableJobs.CloseAdmissions()
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			reactive.CloseAdmissions()
			remoteRuntime.closeAdmissions()
			triggers.CloseAdmissions()
			runs.CloseAdmissions()
			lifecycleCancel()
		case <-runsDone:
			_ = observed.health.PublishCoreUnavailable(obs.CoreCause_CORE_CAUSE_OWNERS_NOT_STARTED)
			maintenance.BeginClose()
			durableJobs.CloseAdmissions()
			_ = host.CloseAdmissions()
			authorization.CloseAdmissions()
			reactive.CloseAdmissions()
			remoteRuntime.closeAdmissions()
			triggers.CloseAdmissions()
			runs.CloseAdmissions()
			lifecycleCancel()
		case <-watchStop:
		}
	}()
	operator, operatorStartErr := startOperatorListenerObserved(lifecycleCtx, dataDir, store, sessions, observed.journal)
	publicDone := closedPublicDone()
	listenerErr := operatorStartErr
	if operatorStartErr == nil {
		publicDone, listenerErr = runPublicListenerPrepared(lifecycleCtx, launch, listen, listenerReady, public, publicSettings, servingTransport)
	}
	_ = observed.health.PublishDraining()
	configurationWatcher.BeginClose()
	activation.BeginClose()
	maintenance.BeginClose()
	durableJobs.CloseAdmissions()
	_ = host.CloseAdmissions()
	authorization.CloseAdmissions()
	reactive.CloseAdmissions()
	remoteRuntime.closeAdmissions()
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
	listDone := listDrain.beginClose()
	stopCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	watcherErr := configurationWatcher.Close(stopCtx)
	authorizationStop := make(chan error, 1)
	go func() { authorizationStop <- authorization.Close(stopCtx) }()
	reactiveStop := make(chan error, 1)
	go func() { reactiveStop <- reactive.Close(stopCtx) }()
	jobsStop := make(chan error, 1)
	go func() { jobsStop <- durableJobs.Close(stopCtx) }()
	operatorDone, operatorErr := operator.stop(stopCtx)
	activationErr := activation.Close(stopCtx)
	stopErr := supervisor.Stop(stopCtx)
	authorizationErr := <-authorizationStop
	reactiveErr := <-reactiveStop
	remoteErr := remoteRuntime.close(stopCtx)
	triggerErr := drainAutomationTriggers(stopCtx, triggers)
	runErr := drainAutomationRuns(stopCtx, runs)
	sceneErr := drainScriptScenes(stopCtx, plan.AutomationRuns)
	historyErr := drainExecutionHistoryAfter(stopCtx, history, runs.Done(), sceneDone, triggers.Done())
	mfaErr := sessions.mfa.close(stopCtx)
	listErr := listDrain.close(stopCtx)
	jobsErr := <-jobsStop
	maintenanceErr := drainRecorderMaintenance(stopCtx, maintenance)
	recorderErr := drainRecorder(stopCtx, recording)
	cancel()
	retainStoreUntilDrained(store, remoteRuntime.Done(), configurationWatcher.Done(), activation.Done(), publicDone, operatorDone, supervisor.Done(), authorization.Done(), reactive.Done(), triggers.Done(), runs.Done(), sceneDone, history.Done(), mfaDone, listDone, durableJobs.Done(), recording.Done(), maintenance.Done())
	return errors.Join(listenerErr, operatorErr, watcherErr, activationErr, stopErr, authorizationErr, reactiveErr, triggerErr, runErr, sceneErr, historyErr, mfaErr, listErr, jobsErr, recorderErr, maintenanceErr, remoteErr)
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
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		return RuntimePlan{}, err
	}
	return defaultConfiguredRuntimePlanWithManifest(manifest, mqttSettings, mqttEnabled, workers, store, sequence, cloud...)
}

func defaultConfiguredRuntimePlanWithManifest(manifest registry.Manifest, mqttSettings mqtt.Settings, mqttEnabled bool, workers []worker.Config, store *storage.Store, sequence *sequencer.Sequencer, cloud ...neviwebComposition) (RuntimePlan, error) {
	if len(cloud) > 1 {
		return RuntimePlan{}, integrations.ErrInvalidInput
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
		decision, e := heatingownership.ParseForManifest(scope.HeatingOwnershipV1(), manifest)
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
		if cells, err := sequence.ObservationCells(); err == nil {
			_ = policy.AttachObservation(cells)
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

// listRuntimeDrain owns exactly one close goroutine. Service.Close can wait
// beyond the runtime budget on an inherited Authority barrier. Its true Done
// remains part of Store retention even after close reports that budget.
type listRuntimeDrain struct {
	lists *listComposition
	once  sync.Once
	done  chan struct{}
	err   error
}

func newListRuntimeDrain(lists *listComposition) *listRuntimeDrain {
	return &listRuntimeDrain{lists: lists, done: make(chan struct{})}
}

func (d *listRuntimeDrain) beginClose() <-chan struct{} {
	d.once.Do(func() {
		go func() {
			d.err = d.lists.Close()
			close(d.done)
		}()
	})
	return d.done
}

func (d *listRuntimeDrain) close(ctx context.Context) error {
	done := d.beginClose()
	select {
	case <-done:
		return d.err
	default:
	}
	select {
	case <-done:
		return d.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
