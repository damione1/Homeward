package mqtt

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
)

var errDiscoveryValue = errors.New("mqtt: discovery state is outside admitted component profile")

// This switch is the sole HA class/unit translation for the admitted sensor
// profile. Unsupported producer fields become diagnostics, never loose units.
type sensorProfile struct {
	constraints *wire.MeasurementConstraints
	semantic    domain.SemanticClass
	aggregation domain.AggregationKind
}

func sensorSemantics(c discoveryComponent) (sensorProfile, error) {
	var expected string
	switch c.deviceClass {
	case "power":
		expected = "W"
	case "energy":
		expected = "Wh"
	case "illuminance":
		expected = "lx"
	case "battery", "humidity":
		expected = "%"
	case "temperature":
		expected = "Cel"
	case "":
		return sensorProfile{}, errDiscoveryProfile
	default:
		return sensorProfile{}, errDiscoveryProfile
	}
	unit := c.unit
	if c.deviceClass == "energy" && unit == "kWh" {
		expected = "kWh"
	}
	if c.deviceClass == "power" && unit == "kW" {
		expected = "kW"
	}
	if unit != expected {
		return sensorProfile{}, errDiscoveryProfile
	}
	if _, err := domain.NewUnitCatalog().Parse(unit); err != nil {
		return sensorProfile{}, errDiscoveryProfile
	}
	result := sensorProfile{semantic: domain.SemanticMeasurement, aggregation: domain.AggregationCurrent}
	wireSemantic := wire.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT
	wireAggregation := wire.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT
	switch c.stateClass {
	case "", "measurement":
	case "total":
		result.semantic, result.aggregation = domain.SemanticTotal, domain.AggregationSum
		wireSemantic, wireAggregation = wire.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL, wire.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM
	case "total_increasing":
		result.semantic, result.aggregation = domain.SemanticTotalIncreasing, domain.AggregationSum
		wireSemantic, wireAggregation = wire.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING, wire.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM
	default:
		return sensorProfile{}, errDiscoveryProfile
	}
	result.constraints = &wire.MeasurementConstraints{Unit: unit, Precision: 3, Semantic: wireSemantic, Aggregation: wireAggregation}
	return result, nil
}

func discoveryDescription(c discoveryComponent, mode Mode, commands []TopicFilter) (*wire.CapabilityDescription, error) {
	if c.uniqueID == "" {
		return nil, errDiscoveryProfile
	}
	canCommand := mode == ModeActive && c.hasCommand
	if canCommand {
		allowed := false
		for _, filter := range commands {
			if filter.Matches(c.commandTopic) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, errDiscoveryProfile
		}
	}
	display := c.name
	if display == "" {
		display = c.uniqueID
	}
	d := &wire.CapabilityDescription{Major: 1, DisplayName: display, Endpoint: "mqtt"}
	switch c.kind {
	case componentBinary:
		switch c.deviceClass {
		case "", "battery", "occupancy", "moisture", "motion", "contact":
		default:
			return nil, errDiscoveryProfile
		}
		d.SchemaId = contracts.BinarySchema
		class := c.deviceClass
		var classPtr *string
		if class != "" {
			classPtr = &class
		}
		d.Constraints = &wire.CapabilityDescription_Binary{Binary: &wire.BinaryConstraints{DeviceClass: classPtr}}
	case componentSwitch:
		d.SchemaId = contracts.SwitchSchema
		d.Constraints = &wire.CapabilityDescription_Switch{Switch: &wire.SwitchConstraints{SetOn: canCommand}}
	case componentSensor:
		m, err := sensorSemantics(c)
		if err != nil {
			return nil, err
		}
		d.SchemaId = contracts.MeasurementSchema
		d.Constraints = &wire.CapabilityDescription_Measurement{Measurement: m.constraints}
	case componentLight:
		d.SchemaId = contracts.LightSchema
		light := &wire.LightConstraints{SetOn: canCommand}
		if c.brightness {
			light.BrightnessPercent = &wire.NumberBounds{Minimum: 0, Maximum: 100}
		}
		if c.colorTemp {
			light.ColorTempMireds = &wire.NumberBounds{Minimum: float64(c.minMireds), Maximum: float64(c.maxMireds)}
		}
		d.Constraints = &wire.CapabilityDescription_Light{Light: light}
	default:
		return nil, errDiscoveryProfile
	}
	return d, nil
}

func scalarMatches(value any, wanted scalar) bool {
	switch wanted.kind {
	case scalarText:
		text, ok := value.(string)
		return ok && text == wanted.text
	case scalarBool:
		boolean, ok := value.(bool)
		return ok && boolean == wanted.boolean
	default:
		return false
	}
}

func numericText(text string) (float64, error) {
	if text == "" || strings.TrimSpace(text) != text || len(text) > 128 {
		return 0, errDiscoveryValue
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, errDiscoveryValue
	}
	return value, nil
}

func numericValue(value any) (float64, error) {
	switch v := value.(type) {
	case string:
		return numericText(v)
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return 0, errDiscoveryValue
		}
		return v, nil
	default:
		return 0, errDiscoveryValue
	}
}
