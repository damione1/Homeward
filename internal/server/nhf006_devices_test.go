package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/damione1/Homeward/contracts"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"io"
	"os"
	"path/filepath"
	"time"
)

type nhf006DeviceKind string

const (
	nhf006Light       nhf006DeviceKind = "light"
	nhf006Switch      nhf006DeviceKind = "switch"
	nhf006Binary      nhf006DeviceKind = "binary"
	nhf006Measurement nhf006DeviceKind = "measurement"
)

func (k *nhf006DeviceKind) UnmarshalJSON(raw []byte) error {
	var input string
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	value := nhf006DeviceKind(input)
	switch value {
	case nhf006Light, nhf006Switch, nhf006Binary, nhf006Measurement:
		*k = value
		return nil
	default:
		return errors.New("unknown fake-device kind")
	}
}

type nhf006Endpoint string
type nhf006Device struct {
	kind     nhf006DeviceKind
	alias    domain.HAAlias
	key      integrations.SourceKey
	endpoint nhf006Endpoint
	title    string
}

func nhf006Devices() ([]nhf006Device, error) {
	file := os.Getenv("HOMEWARD_NHF006_DEVICES")
	if file == "" {
		alias, err := domain.ParseHAAlias("switch.basement_relay")
		if err != nil {
			return nil, err
		}
		key, err := integrations.NewSourceKey("synthetic", "relay")
		if err != nil {
			return nil, err
		}
		return []nhf006Device{{nhf006Switch, alias, key, "relay", "Isolated fake switch"}}, nil
	}
	if !filepath.IsAbs(file) {
		return nil, errors.New("absolute synthetic device source required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	if len(raw) > 16384 {
		return nil, errors.New("synthetic source exceeds bound")
	}
	var input []struct {
		Kind     nhf006DeviceKind `json:"kind"`
		Alias    string           `json:"alias"`
		Title    string           `json:"title"`
		Endpoint string           `json:"endpoint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing synthetic source data")
	}
	if len(input) != 4 {
		return nil, errors.New("exact four-family native fixture required")
	}
	devices := make([]nhf006Device, 0, len(input))
	seen := make(map[nhf006DeviceKind]bool)
	for _, item := range input {
		switch item.Kind {
		case nhf006Light, nhf006Switch, nhf006Binary, nhf006Measurement:
		default:
			return nil, errors.New("missing fake-device kind")
		}
		if seen[item.Kind] || item.Title == "" || len(item.Title) > 128 {
			return nil, errors.New("invalid synthetic device declaration")
		}
		seen[item.Kind] = true
		alias, err := domain.ParseHAAlias(item.Alias)
		if err != nil {
			return nil, err
		}
		key, err := integrations.NewSourceKey(string(item.Kind), item.Endpoint)
		if err != nil {
			return nil, err
		}
		devices = append(devices, nhf006Device{item.Kind, alias, key, nhf006Endpoint(item.Endpoint), item.Title})
	}
	return devices, nil
}
func (d nhf006Device) capability() *registrywire.CapabilityDescription {
	c := &registrywire.CapabilityDescription{Major: 1, Endpoint: string(d.endpoint), DisplayName: d.title}
	switch d.kind {
	case nhf006Light:
		c.SchemaId = contracts.LightSchema
		c.Constraints = &registrywire.CapabilityDescription_Light{Light: &registrywire.LightConstraints{SetOn: true, BrightnessPercent: &registrywire.NumberBounds{Minimum: 20, Maximum: 80}}}
	case nhf006Switch:
		c.SchemaId = contracts.SwitchSchema
		c.Constraints = &registrywire.CapabilityDescription_Switch{Switch: &registrywire.SwitchConstraints{SetOn: true}}
	case nhf006Binary:
		c.SchemaId = contracts.BinarySchema
		c.Constraints = &registrywire.CapabilityDescription_Binary{Binary: &registrywire.BinaryConstraints{}}
	case nhf006Measurement:
		c.SchemaId = contracts.MeasurementSchema
		c.Constraints = &registrywire.CapabilityDescription_Measurement{Measurement: &registrywire.MeasurementConstraints{Unit: "Cel", Precision: 1, Semantic: registrywire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registrywire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}
	default:
		panic("unparsed synthetic device kind")
	}
	return c
}
func (d nhf006Device) observation() (integrations.StateObservation, error) {
	var value domain.StateValue
	var err error
	switch d.kind {
	case nhf006Light:
		brightness := 30.
		light, e := domain.NewLightValue(false, &brightness, nil)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewLightStateValue(light)
	case nhf006Switch, nhf006Binary:
		value = domain.NewSwitchValue(false)
	case nhf006Measurement:
		unit, e := domain.NewUnitCatalog().Parse("Cel")
		if e != nil {
			return integrations.StateObservation{}, e
		}
		measurement, e := domain.NewMeasurement(0, unit, 1, domain.SemanticMeasurement, domain.AggregationCurrent)
		if e != nil {
			return integrations.StateObservation{}, e
		}
		value, err = domain.NewMeasurementValue(measurement)
	default:
		return integrations.StateObservation{}, errors.New("unparsed synthetic device kind")
	}
	if err != nil {
		return integrations.StateObservation{}, err
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		return integrations.StateObservation{}, err
	}
	at := time.Now().UTC()
	return integrations.NewStateObservation(string(d.endpoint), quality, &value, &at, at, nil)
}
