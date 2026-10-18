package hilo

import (
	"context"
	"math"
	"time"

	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

func descriptionFor(binding Binding) *registrywire.CapabilityDescription {
	entry := classifyAlias(binding.alias)
	desc := &registrywire.CapabilityDescription{Major: 1, DisplayName: binding.alias, Endpoint: endpointFor(entry)}
	switch binding.kind {
	case AliasClimate:
		desc.SchemaId = contracts.ClimateSchema
		desc.Constraints = &registrywire.CapabilityDescription_Climate{Climate: &registrywire.ClimateConstraints{
			ObservedTemperatureCelsius: &registrywire.ClimateTemperatureObservation{},
			ObservedSetpointCelsius:    &registrywire.ClimateTemperatureObservation{},
			ActivityOptions:            []registrywire.ClimateActivity{registrywire.ClimateActivity_CLIMATE_ACTIVITY_IDLE, registrywire.ClimateActivity_CLIMATE_ACTIVITY_HEATING},
		}}
	case AliasSwitch:
		desc.SchemaId = contracts.SwitchSchema
		desc.Constraints = &registrywire.CapabilityDescription_Switch{Switch: &registrywire.SwitchConstraints{SetOn: false}}
	case AliasGateway:
		desc.SchemaId = contracts.BinarySchema
		class := "connectivity"
		desc.Constraints = &registrywire.CapabilityDescription_Binary{Binary: &registrywire.BinaryConstraints{DeviceClass: &class}}
	case AliasPower, AliasTemperature, AliasNotifications, AliasWeather:
		desc.SchemaId = contracts.MeasurementSchema
		unit, precision := "Cel", uint32(1)
		if binding.kind == AliasPower {
			unit, precision = "W", 0
		}
		if binding.kind == AliasNotifications {
			unit, precision = "{notification}", 0
		}
		desc.Constraints = &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: unit, Precision: precision, Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}
	default:
		return nil
	}
	return desc
}

func (p *Pilot) discover(ctx context.Context, host integrations.SessionHost) error {
	candidates := make([]integrations.Candidate, 0, 39)
	direct := make([]Binding, 0, 39)
	instance, err := p.settings.Instance()
	if err != nil {
		return err
	}
	for _, binding := range p.settings.bindings {
		if !binding.kind.Direct() {
			continue
		}
		desc := descriptionFor(binding)
		if desc == nil {
			return ErrProviderShape
		}
		var candidate integrations.Candidate
		if binding.kind == AliasClimate || binding.kind == AliasSwitch {
			candidate, err = p.decision.CandidateFor(ctx, p.repository, instance, binding.key, []*registrywire.CapabilityDescription{desc}, p.catalog)
		} else {
			candidate, err = integrations.NewCandidate(binding.key, []*registrywire.CapabilityDescription{desc})
		}
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate)
		direct = append(direct, binding)
	}
	resolved, err := host.Discover(ctx, candidates)
	if err != nil || len(resolved) != len(direct) {
		return ErrProviderShape
	}
	for i, entry := range direct {
		p.bindings[entry.alias] = resolved[i]
	}
	return nil
}

func modelSupports(binding Binding, device hubDevice) bool {
	if device.ID == 0 || device.HiloID == "" {
		return false
	}
	switch binding.kind {
	case AliasClimate, AliasTemperature:
		if binding.alias == "sensor.chauffe_eau_temperature" {
			return device.Type == "Cee" || device.Type == "SinopeWaterHeater"
		}
		return device.Model != "" && isHiloThermostat(device.Type)
	case AliasPower:
		switch device.Model {
		case "43076", "43080", "43094", "43100", "TH6250WF":
			return false
		}
		return device.Type == "Meter" || isHiloThermostat(device.Type) || device.Type == "Cee" || device.Type == "SinopeWaterHeater" || device.Type == "Ccr"
	case AliasSwitch:
		return device.Type == "Cee" || device.Type == "SinopeWaterHeater"
	case AliasGateway:
		return device.Type == "Gateway" || device.Type == "Hub"
	default:
		return true
	}
}

func isHiloThermostat(deviceType string) bool {
	switch deviceType {
	case "Thermostat", "FloorThermostat", "Thermostat24V", "MysaThermostat", "TCC24V", "SinopeThermostat", "SinopeFloorThermostat", "Sinope24V":
		return true
	default:
		return false
	}
}

func (p *Pilot) applyReading(ctx context.Context, host integrations.SessionHost, id int, update deviceReading, initial bool, observedAt *time.Time) error {
	device, ok := p.devices[id]
	if !ok {
		return ErrProviderShape
	}
	state := p.states[id]
	state.merge(update)
	connectivityLost := update.disconnected.present && (state.disconnected.value == nil || *state.disconnected.value)
	if connectivityLost {
		// Reconnection without a new measurement cannot resurrect an old
		// value or renew the NHA-108 source lease.
		state.power, state.ambient, state.target, state.probe = fieldNumber{}, fieldNumber{}, fieldNumber{}, fieldNumber{}
		state.on, state.heating = fieldBool{}, fieldBool{}
	}
	p.states[id] = state
	for _, binding := range p.settings.bindings {
		if binding.deviceID != id || !binding.kind.Direct() || binding.kind == AliasWeather || binding.kind == AliasNotifications {
			continue
		}
		owned, ok := p.bindings[binding.alias]
		if !ok {
			continue
		}
		changed := initial || connectivityLost
		switch binding.kind {
		case AliasPower:
			changed = changed || update.power.present
		case AliasTemperature:
			if endpointFor(classifyAlias(binding.alias)) == "target_temperature" {
				changed = changed || update.target.present
			} else if binding.alias == "sensor.chauffe_eau_temperature" {
				changed = changed || update.probe.present
			} else {
				changed = changed || update.ambient.present
			}
		case AliasClimate:
			changed = changed || update.ambient.present || update.target.present || update.heating.present
		case AliasSwitch:
			changed = changed || update.on.present
		case AliasGateway:
			changed = changed || update.disconnected.present
		}
		if !changed {
			continue
		}
		entity := p.entityOf(owned)
		if !modelSupports(binding, device) || state.disconnected.value == nil || *state.disconnected.value {
			if binding.kind == AliasPower && entity != (domain.EntityID{}) {
				p.leases.Forget(entity)
			}
			if err := p.publishValue(ctx, host, owned, endpointFor(classifyAlias(binding.alias)), nil, observedAt); err != nil {
				return err
			}
			continue
		}
		var value *domain.StateValue
		var err error
		switch binding.kind {
		case AliasPower:
			if state.power.value != nil && *state.power.value >= 0 {
				value, err = p.measurementValue(owned, "power", *state.power.value, "W")
			}
		case AliasTemperature:
			field := state.ambient
			if endpointFor(classifyAlias(binding.alias)) == "target_temperature" {
				field = state.target
			}
			if binding.alias == "sensor.chauffe_eau_temperature" {
				field = state.probe
			}
			if field.value != nil {
				value, err = p.measurementValue(owned, endpointFor(classifyAlias(binding.alias)), *field.value, "Cel")
			}
		case AliasClimate:
			value, err = climateValue(state)
		case AliasSwitch:
			if state.on.value != nil {
				v := domain.NewSwitchValue(*state.on.value)
				value = &v
			}
		case AliasGateway:
			connected := !*state.disconnected.value
			v := domain.NewSwitchValue(connected)
			value = &v
		}
		if err != nil {
			return err
		}
		if binding.kind == AliasPower {
			if value == nil || entity == (domain.EntityID{}) {
				p.leases.Forget(entity)
				if err := p.publishValue(ctx, host, owned, "power", nil, observedAt); err != nil {
					return err
				}
				continue
			}
			observation, err := newHiloObservation("power", value, observedAt)
			if err != nil {
				return err
			}
			if err := p.leases.PublishPower(ctx, host, owned, observation, entity, p.generation); err != nil {
				return err
			}
			continue
		}
		if err := p.publishValue(ctx, host, owned, endpointFor(classifyAlias(binding.alias)), value, observedAt); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pilot) entityOf(binding integrations.Binding) domain.EntityID {
	_, entity, err := binding.IDs()
	if err != nil {
		return domain.EntityID{}
	}
	return entity
}

func climateValue(state deviceReading) (*domain.StateValue, error) {
	var input domain.ClimateValueInput
	if state.ambient.value != nil {
		t, err := domain.NewCelsiusTemperature(*state.ambient.value)
		if err != nil {
			return nil, ErrProviderShape
		}
		input.CurrentTemperature = &t
	}
	if state.target.value != nil {
		t, err := domain.NewCelsiusTemperature(*state.target.value)
		if err != nil {
			return nil, ErrProviderShape
		}
		setpoint, err := domain.NewSingleClimateSetpoint(t)
		if err != nil {
			return nil, ErrProviderShape
		}
		input.Setpoint = &setpoint
	}
	if state.heating.value != nil {
		activity := domain.ClimateActivityIdle
		if *state.heating.value {
			activity = domain.ClimateActivityHeating
		}
		input.Activity = &activity
	}
	if input.CurrentTemperature == nil && input.Setpoint == nil && input.Activity == nil {
		return nil, nil
	}
	climate, err := domain.NewClimateValue(input)
	if err != nil {
		return nil, err
	}
	value, err := domain.NewClimateStateValue(climate)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (p *Pilot) measurementValue(binding integrations.Binding, endpoint string, number float64, unit string) (*domain.StateValue, error) {
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, ErrProviderShape
	}
	entity := p.entityOf(binding)
	if entity == (domain.EntityID{}) {
		return nil, ErrProviderShape
	}
	measurement, err := p.catalog.AdmitMeasurement(entity, endpoint, &statewire.Measurement{Value: number, Unit: unit})
	if err != nil {
		return nil, err
	}
	value, err := domain.NewMeasurementValue(measurement)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func (p *Pilot) publishNumber(ctx context.Context, host integrations.SessionHost, binding integrations.Binding, endpoint string, number float64, observedAt *time.Time) error {
	unit := "Cel"
	if endpoint == "notifications" {
		unit = "{notification}"
	}
	value, err := p.measurementValue(binding, endpoint, number, unit)
	if err != nil {
		return err
	}
	return p.publishValue(ctx, host, binding, endpoint, value, observedAt)
}

func (p *Pilot) publishValue(ctx context.Context, host integrations.SessionHost, binding integrations.Binding, endpoint string, value *domain.StateValue, observedAt *time.Time) error {
	observation, err := newHiloObservation(endpoint, value, observedAt)
	if err != nil {
		return err
	}
	return host.Publish(ctx, binding, observation)
}

func newHiloObservation(endpoint string, value *domain.StateValue, observedAt *time.Time) (integrations.StateObservation, error) {
	availability, freshness := domain.AvailabilityAvailable, domain.FreshnessFresh
	if value == nil {
		availability, freshness = domain.AvailabilityUnavailable, domain.FreshnessStale
	}
	quality, err := domain.NewQuality(availability, freshness, domain.ProvenanceObserved)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	observation, err := integrations.NewStateObservation(endpoint, quality, value, observedAt, time.Now().UTC(), nil)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	return observation, nil
}

func (p *Pilot) markUnavailable(ctx context.Context, host integrations.SessionHost) error {
	for _, row := range p.settings.bindings {
		if !row.kind.Direct() {
			continue
		}
		binding, ok := p.bindings[row.alias]
		if !ok {
			continue
		}
		if err := p.publishValue(ctx, host, binding, endpointFor(classifyAlias(row.alias)), nil, nil); err != nil {
			return err
		}
	}
	clear(p.states)
	return nil
}
