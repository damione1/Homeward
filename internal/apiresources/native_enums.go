package apiresources

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
)

// The private Host and public resource contracts have different owners. Their
// generated vocabularies cross this explicit mapping, never an integer cast or
// a parallel string list. Descriptor coverage tests require each source value
// to remain deliberately mapped (apart from UNSPECIFIED).
var temperatureModes = map[registry.ClimateMode]api.TemperatureMode{
	registry.ClimateMode_CLIMATE_MODE_OFF:      api.TemperatureMode_TEMPERATURE_MODE_OFF,
	registry.ClimateMode_CLIMATE_MODE_HEAT:     api.TemperatureMode_TEMPERATURE_MODE_HEAT,
	registry.ClimateMode_CLIMATE_MODE_COOL:     api.TemperatureMode_TEMPERATURE_MODE_COOL,
	registry.ClimateMode_CLIMATE_MODE_AUTO:     api.TemperatureMode_TEMPERATURE_MODE_AUTO,
	registry.ClimateMode_CLIMATE_MODE_DRY:      api.TemperatureMode_TEMPERATURE_MODE_DRY,
	registry.ClimateMode_CLIMATE_MODE_FAN_ONLY: api.TemperatureMode_TEMPERATURE_MODE_FAN_ONLY,
}

var temperatureActivities = map[registry.ClimateActivity]api.TemperatureActivity{
	registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE:     api.TemperatureActivity_TEMPERATURE_ACTIVITY_IDLE,
	registry.ClimateActivity_CLIMATE_ACTIVITY_HEATING:  api.TemperatureActivity_TEMPERATURE_ACTIVITY_HEATING,
	registry.ClimateActivity_CLIMATE_ACTIVITY_COOLING:  api.TemperatureActivity_TEMPERATURE_ACTIVITY_COOLING,
	registry.ClimateActivity_CLIMATE_ACTIVITY_DRYING:   api.TemperatureActivity_TEMPERATURE_ACTIVITY_DRYING,
	registry.ClimateActivity_CLIMATE_ACTIVITY_FAN_ONLY: api.TemperatureActivity_TEMPERATURE_ACTIVITY_FAN_ONLY,
}

var temperatureFanTargets = map[registry.ClimateFanTarget]api.TemperatureFanTarget{
	registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON:   api.TemperatureFanTarget_TEMPERATURE_FAN_TARGET_ON,
	registry.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO: api.TemperatureFanTarget_TEMPERATURE_FAN_TARGET_AUTO,
}

var measurementSemantics = map[registry.MeasurementSemantic]api.MeasurementSemantic{
	registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT:      api.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT,
	registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL:            api.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL,
	registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING: api.MeasurementSemantic_MEASUREMENT_SEMANTIC_TOTAL_INCREASING,
}

var measurementAggregations = map[registry.MeasurementAggregation]api.MeasurementAggregation{
	registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT: api.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT,
	registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM:     api.MeasurementAggregation_MEASUREMENT_AGGREGATION_SUM,
	registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN:    api.MeasurementAggregation_MEASUREMENT_AGGREGATION_MEAN,
}

func mapped[K, V comparable](value K, mapping map[K]V) (V, error) {
	if result, known := mapping[value]; known {
		return result, nil
	}
	var zero V
	return zero, ErrProjection
}

func mappedOptions[K, V comparable](input []K, mapping map[K]V) ([]V, error) {
	output := make([]V, 0, len(input))
	seen := make(map[K]bool)
	for _, value := range input {
		converted, err := mapped(value, mapping)
		if err != nil || seen[value] {
			return nil, ErrProjection
		}
		seen[value] = true
		output = append(output, converted)
	}
	return output, nil
}
