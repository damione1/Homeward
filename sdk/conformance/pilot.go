// Package conformance exposes the production Host contract and a device-free
// conformance harness to third-party Go modules. Its aliases preserve the
// validated domain constructors and the single in-process Host authority.
package conformance

import (
	"time"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/worker"
)

type Pilot = integrations.Pilot
type PilotSpec = integrations.PilotSpec
type SessionHost = integrations.SessionHost
type Tasks = integrations.Tasks
type AdmittedCommand = integrations.AdmittedCommand
type CommandDriver = integrations.CommandDriver
type SupervisedCommandPilot = integrations.SupervisedCommandPilot
type ProtocolConfirmation = integrations.ProtocolConfirmation
type SourceKey = integrations.SourceKey
type Candidate = integrations.Candidate
type Binding = integrations.Binding
type StateObservation = integrations.StateObservation
type DeviceEvent = integrations.DeviceEvent
type HealthReport = integrations.HealthReport
type HealthStatus = integrations.HealthStatus
type Causality = domain.Causality
type Quality = domain.Quality
type StateValue = domain.StateValue
type LightValue = domain.LightValue
type IntegrationInstanceID = domain.IntegrationInstanceID
type CommandID = domain.CommandID

const (
	HealthReady       = integrations.HealthReady
	HealthDegraded    = integrations.HealthDegraded
	HealthUnavailable = integrations.HealthUnavailable
)

func ParseIntegrationInstanceID(text string) (IntegrationInstanceID, error) {
	return domain.ParseIntegrationInstanceID(text)
}

func ParseCommandID(text string) (CommandID, error) {
	return domain.ParseCommandID(text)
}

func NewPilotSpec(instance IntegrationInstanceID, factory func() Pilot) (PilotSpec, error) {
	return integrations.NewPilotSpec(instance, factory)
}

// NewWorkerSpec adapts an explicitly chosen local executable through the real
// NHA-094 supervisor. It neither installs nor launches a worker by itself.
func NewWorkerSpec(instance IntegrationInstanceID, executable string, args []string) (PilotSpec, error) {
	text, err := instance.Value()
	if err != nil {
		return PilotSpec{}, err
	}
	config, err := worker.NewConfig(text, executable, args, nil)
	if err != nil {
		return PilotSpec{}, err
	}
	return worker.NewPilotSpec(config)
}

func NewSourceKey(device, entity string) (SourceKey, error) {
	return integrations.NewSourceKey(device, entity)
}

func NewCandidate(key SourceKey, descriptions []*registry.CapabilityDescription) (Candidate, error) {
	return integrations.NewCandidate(key, descriptions)
}

func NewStateObservation(endpoint string, quality Quality, value *StateValue, observedAt *time.Time, receivedAt time.Time, sequence *uint64) (StateObservation, error) {
	return integrations.NewStateObservation(endpoint, quality, value, observedAt, receivedAt, sequence)
}

func NewDeviceEvent(code string, observedAt time.Time, sequence *uint64) (DeviceEvent, error) {
	return integrations.NewDeviceEvent(code, observedAt, sequence)
}

func NewHealthReport(status HealthStatus, detail string) (HealthReport, error) {
	return integrations.NewHealthReport(status, detail)
}

func ObservedQuality() (Quality, error) {
	return domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
}

func StaleQuality() (Quality, error) {
	return domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceObserved)
}

func NewSwitchValue(on bool) StateValue { return domain.NewSwitchValue(on) }

func NewLightValue(on bool, brightness *float64, colorTemp *uint32) (StateValue, error) {
	light, err := domain.NewLightValue(on, brightness, colorTemp)
	if err != nil {
		return StateValue{}, err
	}
	return domain.NewLightStateValue(light)
}

func NewCommandCausality(correlation string) (Causality, error) {
	id, err := domain.ParseCorrelationID(correlation)
	if err != nil {
		return Causality{}, err
	}
	return domain.NewCausality(nil, &id)
}

// Climate aliases preserve the same admitted domain values for external drivers.
type ClimateValue = domain.ClimateValue
type ClimateValueInput = domain.ClimateValueInput
type CelsiusTemperature = domain.CelsiusTemperature
type ClimateSetpoint = domain.ClimateSetpoint
type RelativeHumidity = domain.RelativeHumidity
type ClimateFanTarget = domain.ClimateFanTarget

func NewCelsiusTemperature(number float64) (CelsiusTemperature, error) {
	return domain.NewCelsiusTemperature(number)
}
func NewRelativeHumidity(number float64) (RelativeHumidity, error) {
	return domain.NewRelativeHumidity(number)
}
func NewClimateFanTarget(automatic bool) ClimateFanTarget {
	return domain.NewClimateFanTarget(automatic)
}
func NewClimateValue(input ClimateValueInput) (ClimateValue, error) {
	return domain.NewClimateValue(input)
}
func NewClimateStateValue(value ClimateValue) (StateValue, error) {
	return domain.NewClimateStateValue(value)
}
