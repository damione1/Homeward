package server

import (
	"context"
	"reflect"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/automationtriggers"
	"github.com/damione1/Homeward/internal/calendars"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/derivedpower"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/energy"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/helpers"
	"github.com/damione1/Homeward/internal/hilo"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/neviweb"
	"github.com/damione1/Homeward/internal/reactivetemplate"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/solar"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/workday"
	"github.com/damione1/Homeward/internal/worker"
	"github.com/damione1/Homeward/scope"
)

// This compiler consumes the exact parsed operational source selected by the
// activation owner. Constructors below only prepare the delivered factories;
// no pilot, client, process, Host discovery or credential mutation starts here.
type configuredOperationalCompiler struct {
	remoteCalendar *remoteCalendarRuntime
	manifest       registry.Manifest
	repository     *registry.Repository
	store          *storage.Store
	sequence       *sequencer.Sequencer
	host           *integrations.Manager
	catalog        *contracts.CapabilityCatalog
	authorization  *cloudAuthorizationCoordinator
	templates      *reactiveTemplateRuntime
	secrets        *storage.SecretRepository
	solarClock     automationtriggers.Clock
	workdayClock   automationtriggers.Clock
	workday        *activationWorkdayCalendar
	supervisor     *integrations.Supervisor
	triggerPlan    automationtriggers.Plan
}

type configuredOperationalPublication struct {
	compiler         *configuredOperationalCompiler
	cloud            *cloudRegistrationReplacement
	derivedChanged   bool
	derived          *derivedpower.Publisher
	derivedIDs       []reactivetemplate.ProgramID
	baseIDs          []reactivetemplate.ProgramID
	derivedPublished bool
	neviweb          *neviweb.Settings
	workdayChanged   bool
	workday          *workday.Settings
	workdayEntity    domain.EntityID
	derivedStarted   bool
	workdayStarted   bool
	mqttChanged      bool
	mqttInstance     domain.IntegrationInstanceID
	mqttEnabled      bool
	triggerPlan      automationtriggers.Plan
}

func (p *configuredOperationalPublication) Quiesce(ctx context.Context) error {
	if p == nil {
		return configactivation.ErrOperationalComposition
	}
	if p.cloud != nil {
		return p.cloud.Quiesce(ctx)
	}
	return ctx.Err()
}
func (p *configuredOperationalPublication) Publish(ctx context.Context) error {
	if p == nil || p.compiler == nil {
		return configactivation.ErrOperationalComposition
	}
	if p.cloud != nil {
		if err := p.cloud.Publish(); err != nil {
			return err
		}
	}
	if p.mqttChanged {
		if err := p.compiler.host.PublishConfigurationMQTTEdges(p.mqttInstance, p.mqttEnabled); err != nil {
			return err
		}
	}
	if p.triggerPlan.Valid() {
		p.compiler.triggerPlan = p.triggerPlan
	}
	if p.neviweb != nil {
		initial, err := neviweb.InitialSessionValue()
		if err != nil {
			return err
		}
		if _, err := p.compiler.secrets.InitializeCredentialIfAbsent(ctx, p.neviweb.SessionHandle(), initial); err != nil {
			return err
		}
	}
	if p.workdayChanged {
		p.workdayStarted = true
		var reader workday.CalendarReader
		if p.workday != nil {
			repository, err := calendars.NewRepository(p.compiler.store, p.compiler.manifest)
			if err != nil {
				return err
			}
			id, err := repository.Bind(ctx, p.workdayEntity)
			if err != nil {
				return err
			}
			reader, err = workday.NewCalendarReader(id, *p.workday)
			if err != nil {
				return err
			}
		}
		p.compiler.workday.publish(reader)
	}
	if p.derivedChanged && !p.derivedPublished {
		p.derivedStarted = true
		runtime := p.compiler.templates
		if runtime == nil {
			return configactivation.ErrOperationalComposition
		}
		runtime.mu.Lock()
		previous := runtime.derived
		runtime.mu.Unlock()
		if previous != nil {
			if err := previous.Retire(p.compiler.host); err != nil {
				return err
			}
		}
		if p.derived != nil {
			if err := p.derived.PrepareCommitted(ctx, p.compiler.host); err != nil {
				return err
			}
		}
		if err := runtime.replaceDerivedOutput(p.derived, p.derivedIDs, p.baseIDs); err != nil {
			return err
		}
		p.derivedPublished = true
	}
	return ctx.Err()
}
func (p *configuredOperationalPublication) Release() {
	if p != nil && p.cloud != nil {
		p.cloud.Release()
	}
}

func (p *configuredOperationalPublication) Supersede(ctx context.Context) ([]domain.IntegrationInstanceID, error) {
	if p == nil {
		return nil, configactivation.ErrOperationalComposition
	}
	var forced []domain.IntegrationInstanceID
	if p.cloud != nil {
		values, err := p.cloud.Supersede(ctx)
		if err != nil {
			return nil, err
		}
		forced = append(forced, values...)
	}
	if p.derivedStarted {
		instance, _ := derivedpower.Instance()
		forced = append(forced, instance)
	}
	if p.workdayStarted {
		instance, _ := workday.InstanceID()
		forced = append(forced, instance)
	}
	return forced, nil
}

func (c *configuredOperationalCompiler) PrepareOperational(ctx context.Context, old, next config.Config,
	oldDefinitions, nextDefinitions bundle.Candidate, baseTemplates reactivetemplate.Plan, triggerPlan automationtriggers.Plan, forcedInstances []domain.IntegrationInstanceID, committed *registry.CommittedBindings) (configactivation.OperationalImage, error) {
	if c == nil || c.repository == nil || c.store == nil || c.sequence == nil || nilReactiveInterface(ctx) {
		return configactivation.OperationalImage{}, configactivation.ErrOperationalComposition
	}
	if _, ok := c.manifest.Revision(); !ok {
		return configactivation.OperationalImage{}, configactivation.ErrOperationalComposition
	}
	publication := &configuredOperationalPublication{compiler: c}
	forced := make(map[domain.IntegrationInstanceID]bool, len(forcedInstances))
	for _, id := range forcedInstances {
		if _, err := id.Value(); err != nil {
			return configactivation.OperationalImage{}, err
		}
		forced[id] = true
	}
	complete := false
	defer func() {
		if !complete {
			publication.Release()
		}
	}()
	requests := make([]registry.BindingRequest, 0)
	addBinding := func(instance domain.IntegrationInstanceID, key integrations.SourceKey, alias string) error {
		request, err := registry.NewBindingRequest(instance, key, alias)
		if err != nil {
			return err
		}
		requests = append(requests, request)
		return nil
	}
	oldHelpers, err := admittedHelpers(oldDefinitions)
	if err != nil {
		return configactivation.OperationalImage{}, err
	}
	nextHelpers, err := admittedHelpers(nextDefinitions)
	if err != nil {
		return configactivation.OperationalImage{}, err
	}
	helperInstance, _ := domain.ParseIntegrationInstanceID("homeward-helpers-v1")
	for _, definition := range nextHelpers {
		alias, err := definition.Alias()
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		key, err := helpers.ConfiguredSourceKey(alias)
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		name, _ := alias.Value()
		if err := addBinding(helperInstance, key, name); err != nil {
			return configactivation.OperationalImage{}, err
		}
	}
	for _, configured := range next.ExternalWorkers() {
		instance, err := configured.Instance()
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		for _, binding := range configured.Bindings() {
			key, alias, err := binding.Components()
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			if err := addBinding(instance, key, alias); err != nil {
				return configactivation.OperationalImage{}, err
			}
		}
	}
	newNeviweb, hasNewNeviweb := next.NeviwebSettings()
	if hasNewNeviweb {
		instance, err := newNeviweb.Instance()
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		for _, binding := range newNeviweb.Bindings() {
			if err := addBinding(instance, binding.SourceKey(), binding.Alias()); err != nil {
				return configactivation.OperationalImage{}, err
			}
		}
	}
	newHilo, hasNewHilo := next.HiloSettings()
	hiloEnergyInstance, _ := domain.ParseIntegrationInstanceID("homeward-hilo-energy-v1")
	if hasNewHilo {
		instance, err := newHilo.Instance()
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		for _, binding := range newHilo.Bindings() {
			if err := addBinding(instance, binding.SourceKey(), binding.Alias()); err != nil {
				return configactivation.OperationalImage{}, err
			}
		}
		for _, pair := range newHilo.EnergyPairs() {
			key, err := integrations.NewSourceKey("computed-hilo-energy", pair.OutputAlias)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			if err := addBinding(hiloEnergyInstance, key, pair.OutputAlias); err != nil {
				return configactivation.OperationalImage{}, err
			}
		}
	}
	solarInstance, _ := solar.InstanceID()
	newSolar, hasNewSolar := next.SolarSettings()
	if hasNewSolar {
		bindings, err := solar.ConfiguredBindings()
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		for _, binding := range bindings {
			alias, _ := binding.Alias().Value()
			if err := addBinding(solarInstance, binding.SourceKey(), alias); err != nil {
				return configactivation.OperationalImage{}, err
			}
		}
	}
	workdayInstance, _ := workday.InstanceID()
	newWorkday, hasNewWorkday := next.WorkdaySettings()
	if hasNewWorkday {
		bindings, err := workday.ConfiguredBindings()
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		for _, binding := range bindings {
			alias, _ := binding.Alias().Value()
			if err := addBinding(workdayInstance, binding.SourceKey(), alias); err != nil {
				return configactivation.OperationalImage{}, err
			}
		}
	}
	energyInstance, _ := domain.ParseIntegrationInstanceID("homeward-energy-v1")
	for _, row := range next.EnergyBindings() {
		key, err := integrations.NewSourceKey("computed-energy", row.OutputAlias())
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		if err := addBinding(energyInstance, key, row.OutputAlias()); err != nil {
			return configactivation.OperationalImage{}, err
		}
	}
	derivedInstance, _ := derivedpower.Instance()
	if next.DerivedPowerProfile().Enabled() {
		for _, kind := range derivedpower.Kinds() {
			alias, _ := kind.OutputAlias()
			key, _ := kind.SourceKey()
			if err := addBinding(derivedInstance, key, alias); err != nil {
				return configactivation.OperationalImage{}, err
			}
		}
	}
	if request, enabled, requestErr := remoteCalendarBinding(next); requestErr != nil {
		return configactivation.OperationalImage{}, requestErr
	} else if enabled {
		requests = append(requests, request)
	}
	var bindings *registry.PreparedBindings
	if committed != nil {
		bindings, err = committed.PrepareBindings(requests)
	} else {
		bindings, err = c.repository.PrepareBindings(ctx, requests)
	}
	if err != nil {
		return configactivation.OperationalImage{}, err
	}
	nextPilots := make([]integrations.PilotSpec, 0)
	remove := make([]domain.IntegrationInstanceID, 0)
	replace := func(previous domain.IntegrationInstanceID, hadPrevious bool, spec *integrations.PilotSpec) {
		delete(forced, previous)
		if hadPrevious {
			_, hadPrevious = c.supervisor.Status(previous)
		}
		if spec != nil {
			nextPilots = append(nextPilots, *spec)
			id, _ := spec.Instance()
			delete(forced, id)
			if hadPrevious && id != previous {
				remove = append(remove, previous)
			}
		} else if hadPrevious {
			remove = append(remove, previous)
		}
	}
	remoteInstance, hadRemote, remoteChanged, remoteSpec, remoteErr := c.prepareRemoteCalendar(old, next, bindings, forced)
	if remoteErr != nil {
		return configactivation.OperationalImage{}, remoteErr
	}
	if remoteChanged {
		replace(remoteInstance, hadRemote, remoteSpec)
	}
	oldWorkers := make(map[domain.IntegrationInstanceID]worker.Config)
	for _, configured := range old.ExternalWorkers() {
		id, _ := configured.Instance()
		oldWorkers[id] = configured
	}
	for _, configured := range next.ExternalWorkers() {
		id, _ := configured.Instance()
		previous, had := oldWorkers[id]
		delete(oldWorkers, id)
		if had && reflect.DeepEqual(previous, configured) && !forced[id] {
			continue
		}
		spec, err := worker.NewPilotSpec(configured)
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		replace(id, had, &spec)
	}
	for id := range oldWorkers {
		delete(forced, id)
		if _, present := c.supervisor.Status(id); present {
			remove = append(remove, id)
		}
	}
	oldMQTT, hadMQTT := old.MQTTSettings()
	newMQTT, hasMQTT := next.MQTTSettings()
	oldMQTTID, _ := oldMQTT.Instance()
	newMQTTID, _ := newMQTT.Instance()
	oldTriggerMQTT, hadTriggerMQTT := c.triggerPlan.MQTTInstance()
	nextTriggerMQTT, hasTriggerMQTT := triggerPlan.MQTTInstance()
	mqttPlanChanged := hadTriggerMQTT != hasTriggerMQTT || oldTriggerMQTT != nextTriggerMQTT || !reflect.DeepEqual(c.triggerPlan.MQTTFilters(), triggerPlan.MQTTFilters())
	if hadMQTT != hasMQTT || !reflect.DeepEqual(oldMQTT, newMQTT) || forced[oldMQTTID] || forced[newMQTTID] || mqttPlanChanged {
		previous, _ := oldMQTT.Instance()
		var spec *integrations.PilotSpec
		if hasMQTT {
			instance, err := newMQTT.Instance()
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			factory, err := mqtt.NewDiscoveryPilotFactory(newMQTT, c.repository)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			if triggerPlan.Valid() {
				if _, needed := triggerPlan.MQTTInstance(); needed {
					proof, err := mqtt.NewTriggerEdgePilot(newMQTT, c.repository, triggerPlan.MQTTFilters())
					if err != nil {
						return configactivation.OperationalImage{}, err
					}
					triggerPlan, err = triggerPlan.WithMQTT(newMQTT, proof)
					if err != nil {
						return configactivation.OperationalImage{}, err
					}
					factory, err = proof.Factory()
					if err != nil {
						return configactivation.OperationalImage{}, err
					}
					publication.mqttEnabled, publication.mqttInstance = true, instance
				}
			}
			value, err := integrations.NewPilotSpec(instance, factory)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			spec = &value
		} else if triggerPlan.Valid() {
			if _, needed := triggerPlan.MQTTInstance(); needed {
				return configactivation.OperationalImage{}, automationtriggers.ErrInvalidPlan
			}
		}
		publication.mqttChanged = true
		replace(previous, hadMQTT, spec)
	} else if triggerPlan.Valid() && c.triggerPlan.Valid() {
		triggerPlan, err = triggerPlan.PreserveMQTTEnvironment(c.triggerPlan)
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
	}
	oldSolar, hadSolar := old.SolarSettings()
	if hadSolar != hasNewSolar || !reflect.DeepEqual(oldSolar, newSolar) || forced[solarInstance] {
		if triggerPlan.Valid() {
			var input automationtriggers.PlanInput
			if hasNewSolar {
				input, err = solar.ConfigureTriggerInput(newSolar, input)
				if err != nil {
					return configactivation.OperationalImage{}, err
				}
			}
			triggerPlan, err = triggerPlan.WithSolar(input)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
		}
		var spec *integrations.PilotSpec
		if hasNewSolar {
			factory, err := solar.NewPilotFactory(newSolar, c.solarClock)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			value, err := integrations.NewPilotSpec(solarInstance, factory)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			spec = &value
		}
		replace(solarInstance, hadSolar, spec)
	}
	oldWorkday, hadWorkday := old.WorkdaySettings()
	if hadWorkday != hasNewWorkday || !reflect.DeepEqual(oldWorkday, newWorkday) || forced[workdayInstance] {
		publication.workdayChanged = true
		var spec *integrations.PilotSpec
		if hasNewWorkday {
			factory, err := workday.NewPilotFactory(newWorkday, c.workdayClock)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			value, err := integrations.NewPilotSpec(workdayInstance, factory)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			spec = &value
			settings := newWorkday
			publication.workday = &settings
			calendar, err := bindings.LookupAlias("calendar.workday_sensor_calendar")
			if err != nil || calendar.Retired || !calendar.Current {
				return configactivation.OperationalImage{}, configactivation.ErrOperationalComposition
			}
			publication.workdayEntity = calendar.Entity
		}
		replace(workdayInstance, hadWorkday, spec)
	}
	if !reflect.DeepEqual(oldHelpers, nextHelpers) || forced[helperInstance] {
		var spec *integrations.PilotSpec
		if len(nextHelpers) != 0 {
			repository, err := helpers.NewRepository(c.store)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			policy, err := scope.RetainedHelperButtonPolicy()
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			factory, err := helpers.NewPilotFactory(repository, nextHelpers, policy)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			value, err := integrations.NewPilotSpec(helperInstance, factory)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			spec = &value
		}
		replace(helperInstance, len(oldHelpers) != 0, spec)
	}
	if !reflect.DeepEqual(old.EnergyBindings(), next.EnergyBindings()) || forced[energyInstance] {
		var spec *integrations.PilotSpec
		if len(next.EnergyBindings()) != 0 {
			value, err := c.prepareEnergy(next, bindings)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			spec = &value
		}
		replace(energyInstance, len(old.EnergyBindings()) != 0, spec)
	}
	oldNeviweb, hadNeviweb := old.NeviwebSettings()
	oldNeviwebID, _ := oldNeviweb.Instance()
	newNeviwebID, _ := newNeviweb.Instance()
	if hadNeviweb != hasNewNeviweb || !reflect.DeepEqual(oldNeviweb, newNeviweb) || forced[oldNeviwebID] || forced[newNeviwebID] {
		previous, _ := oldNeviweb.Instance()
		var spec *integrations.PilotSpec
		if hasNewNeviweb {
			value, err := c.prepareNeviweb(newNeviweb)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			spec = &value
			valueSettings := newNeviweb
			publication.neviweb = &valueSettings
		}
		replace(previous, hadNeviweb, spec)
	}
	oldHilo, hadHilo := old.HiloSettings()
	oldHiloID, _ := oldHilo.Instance()
	newHiloID, _ := newHilo.Instance()
	if hadHilo != hasNewHilo || !reflect.DeepEqual(oldHilo, newHilo) || forced[oldHiloID] || forced[newHiloID] || c.authorization.replacingRegistration() {
		previous, _ := oldHilo.Instance()
		var oldID *domain.IntegrationInstanceID
		if hadHilo {
			oldID = &previous
		}
		var registration *CloudAuthorizationRegistration
		var preparedHilo *hiloComposition
		if hasNewHilo {
			preparedHilo, err = newHiloCompositionWithManifest(c.manifest, newHilo, c.store, c.sequence, c.catalog, c.repository)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			instance, _ := newHilo.Instance()
			profile, err := integrations.ParseCloudAuthorizationProfileID("hilo-android-b2c-v1")
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			provider, err := hilo.NewAuthorizationProvider(profile)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			value, err := NewCloudAuthorizationRegistration(instance, newHilo.CredentialHandle(), profile, preparedHilo.policy, provider, preparedHilo.quota)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			registration = &value
		}
		var binder CloudAuthorizationBinder
		publication.cloud, binder, err = c.authorization.reserveReplacement(oldID, registration)
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		var spec *integrations.PilotSpec
		if hasNewHilo {
			factory, err := hilo.NewPilotFactory(newHilo, c.repository, preparedHilo.decision, c.catalog, preparedHilo.policy, preparedHilo.quota, binder, preparedHilo.leases)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			instance, _ := newHilo.Instance()
			value, err := integrations.NewPilotSpec(instance, factory)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			spec = &value
			energySpec, err := preparedHilo.energySpec(ctx, bindings)
			if err != nil {
				return configactivation.OperationalImage{}, err
			}
			nextPilots = append(nextPilots, energySpec)
			delete(forced, hiloEnergyInstance)
		} else if hadHilo {
			delete(forced, hiloEnergyInstance)
			if _, present := c.supervisor.Status(hiloEnergyInstance); present {
				remove = append(remove, hiloEnergyInstance)
			}
		}
		replace(previous, hadHilo, spec)
	}
	var templatePlan *reactivetemplate.Plan
	if old.DerivedPowerProfile() != next.DerivedPowerProfile() || !reflect.DeepEqual(oldDefinitions.Groups(), nextDefinitions.Groups()) || forced[derivedInstance] {
		if c.templates == nil || !baseTemplates.Active() {
			return configactivation.OperationalImage{}, configactivation.ErrOperationalComposition
		}
		var definitions []reactivetemplate.Definition
		removeIDs := make([]reactivetemplate.ProgramID, 0, 3)
		if old.DerivedPowerProfile().Enabled() || next.DerivedPowerProfile().Enabled() || forced[derivedInstance] {
			for _, kind := range derivedpower.Kinds() {
				name, _ := kind.ProgramName()
				id, _ := reactivetemplate.ParseProgramID(name)
				removeIDs = append(removeIDs, id)
			}
		}
		if !old.DerivedPowerProfile().Enabled() && next.DerivedPowerProfile().Enabled() && !forced[derivedInstance] {
			for _, existing := range baseTemplates.ProgramIDs() {
				for _, owned := range removeIDs {
					if existing == owned {
						return configactivation.OperationalImage{}, ErrDerivedPowerComposition
					}
				}
			}
		}
		publication.derivedChanged = old.DerivedPowerProfile() != next.DerivedPowerProfile() || forced[derivedInstance]
		delete(forced, derivedInstance)
		if next.DerivedPowerProfile().Enabled() {
			configured := make([]derivedpower.Binding, 0, 3)
			for _, kind := range derivedpower.Kinds() {
				alias, _ := kind.OutputAlias()
				target, err := bindings.LookupAlias(alias)
				if err != nil || target.Retired || !target.Current {
					return configactivation.OperationalImage{}, configactivation.ErrOperationalComposition
				}
				binding, err := derivedpower.NewBinding(kind, target.Entity)
				if err != nil {
					return configactivation.OperationalImage{}, err
				}
				configured = append(configured, binding)
				definition, err := derivedpower.Definition(kind, target.Entity)
				if err != nil {
					return configactivation.OperationalImage{}, err
				}
				definitions = append(definitions, definition)
				publication.derivedIDs = append(publication.derivedIDs, definition.ID())
			}
			if publication.derivedChanged {
				publication.derived, err = derivedpower.NewPublisher(configured)
				if err != nil {
					return configactivation.OperationalImage{}, err
				}
			}
		}
		updated, err := baseTemplates.ReplacePermanent(removeIDs, definitions, nextDefinitions.Groups())
		if err != nil {
			return configactivation.OperationalImage{}, err
		}
		for _, id := range updated.ProgramIDs() {
			retained := true
			for _, owned := range publication.derivedIDs {
				if id == owned {
					retained = false
				}
			}
			if retained {
				publication.baseIDs = append(publication.baseIDs, id)
			}
		}
		templatePlan = &updated
	}
	for id := range forced {
		if _, present := c.supervisor.Status(id); present {
			remove = append(remove, id)
		}
	}
	var triggerImage *automationtriggers.Plan
	if triggerPlan.Valid() {
		triggerImage = &triggerPlan
		publication.triggerPlan = triggerPlan
	}
	image, err := configactivation.NewOperationalImage(nextPilots, remove, bindings, publication, templatePlan, triggerImage)
	if err != nil {
		return configactivation.OperationalImage{}, err
	}
	complete = true
	return image, nil
}

func admittedHelpers(candidate bundle.Candidate) ([]helpers.Definition, error) {
	if !candidate.Valid() {
		return nil, nil
	}
	return helpers.Definitions(candidate)
}

func (c *configuredOperationalCompiler) prepareEnergy(configuration config.Config, identities *registry.PreparedBindings) (integrations.PilotSpec, error) {
	instance, _ := domain.ParseIntegrationInstanceID("homeward-energy-v1")
	unit, err := domain.NewUnitCatalog().Parse("kWh")
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	bindings := make([]energy.OutputBinding, 0, len(configuration.EnergyBindings()))
	for _, row := range configuration.EnergyBindings() {
		source, err := identities.LookupAlias(row.SourceAlias())
		if err != nil || source.Retired || !source.Current {
			return integrations.PilotSpec{}, configactivation.ErrOperationalComposition
		}
		output, err := identities.LookupAlias(row.OutputAlias())
		if err != nil || output.Retired || !output.Current {
			return integrations.PilotSpec{}, configactivation.ErrOperationalComposition
		}
		profile, err := energy.NewProfile("mvp-trapezoidal-v1", 1, source.Entity, output.Entity, energy.MethodTrapezoidal, row.TimeBasis(), unit, row.Precision(), domain.SemanticTotal, row.MaxGap(), 0)
		if err != nil {
			return integrations.PilotSpec{}, err
		}
		key, err := integrations.NewSourceKey("computed-energy", row.OutputAlias())
		if err != nil {
			return integrations.PilotSpec{}, err
		}
		binding, err := energy.NewOutputBinding(profile, key)
		if err != nil {
			return integrations.PilotSpec{}, err
		}
		bindings = append(bindings, binding)
	}
	repository, err := energy.NewRepository(c.store)
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	factory, err := energy.NewPilotFactory(c.sequence, repository, bindings, nil, energy.RealClock{Sequence: c.sequence})
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	return integrations.NewPilotSpec(instance, factory)
}

func (c *configuredOperationalCompiler) prepareNeviweb(settings neviweb.Settings) (integrations.PilotSpec, error) {
	decision, err := heatingownership.ParseForManifest(scope.HeatingOwnershipV1(), c.manifest)
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	instance, err := settings.Instance()
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	limit, _, zone := settings.Budget()
	quota, err := storage.NewCloudAttemptQuota(c.store, settings.AccountText(), limit, zone)
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	limits := integrations.DefaultCloudLimits().Input()
	limits.MaxInFlight, limits.ObservationAttempts = 2, 2
	limits.AttemptTimeout, limits.TotalTimeout, limits.RefreshBudget = 30*time.Second, 65*time.Second, 30*time.Second
	admitted, err := integrations.AdmitCloudLimits(limits)
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	account, err := integrations.NewCloudAccountBinding(instance, settings.Account())
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	policy, err := integrations.NewCloudPolicy(admitted, []integrations.CloudAccountBinding{account}, integrations.RealCloudClock{}, integrations.RealCloudTimer{}, integrations.RandomJitter{})
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	factory, err := neviweb.NewPilotFactory(settings, c.repository, decision, c.catalog, policy, quota)
	if err != nil {
		return integrations.PilotSpec{}, err
	}
	return integrations.NewPilotSpec(instance, factory)
}
