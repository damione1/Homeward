package integrations

import (
	"testing"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func lightState(t *testing.T, on bool, brightness float64, mireds *uint32) domain.StateValue {
	t.Helper()
	light, err := domain.NewLightValue(domain.LightValueInput{On: on, Brightness: &brightness, ColorTemperatureMireds: mireds})
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewLightStateValue(light)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestHalfStepToleranceDerivesFromDeclaredStepOnly(t *testing.T) {
	step := 1.0
	zero := 0.0
	cases := []struct {
		name   string
		bounds *wireregistry.NumberBounds
		near   float64
		far    float64
	}{
		{"declared step admits half a step", &wireregistry.NumberBounds{Minimum: 0, Maximum: 100, Step: &step}, 50.5, 50.51},
		{"no step is exact", &wireregistry.NumberBounds{Minimum: 0, Maximum: 100}, 50, 50.001},
		{"zero step is exact", &wireregistry.NumberBounds{Minimum: 0, Maximum: 100, Step: &zero}, 50, 50.001},
		{"absent bounds are exact", nil, 50, 50.001},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			tolerance := HalfStepTolerance(item.bounds)
			if !tolerance.Admits(item.near, 50) {
				t.Fatalf("%v rejected within tolerance", item.near)
			}
			if tolerance.Admits(item.far, 50) {
				t.Fatalf("%v admitted beyond tolerance", item.far)
			}
		})
	}
}

func TestContinuousPredicatesUseTheRouteTolerance(t *testing.T) {
	on := true
	brightness := 50.0
	mireds := uint32(300)
	request := &actions.CommandRequest{CapabilitySchemaId: contracts.LightSchema, Endpoint: "dimmer", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on, BrightnessPercent: &brightness, ColorTempMireds: &mireds}}}}
	step := 1.0
	description := &wireregistry.CapabilityDescription{SchemaId: contracts.LightSchema, Endpoint: "dimmer", Constraints: &wireregistry.CapabilityDescription_Light{Light: &wireregistry.LightConstraints{SetOn: true, BrightnessPercent: &wireregistry.NumberBounds{Minimum: 0, Maximum: 100, Step: &step}, ColorTempMireds: &wireregistry.NumberBounds{Minimum: 153, Maximum: 500}}}}
	tolerant := predicateForTarget(request, lightTarget{}, tolerancesFor(description))
	exact := predicateForTarget(request, lightTarget{}, RouteTolerances{})
	observedMireds := uint32(300)
	if !tolerant.matches(lightState(t, true, 50.4, &observedMireds), contracts.LightSchema) {
		t.Fatal("quantized brightness within half a step was not evidence")
	}
	if tolerant.matches(lightState(t, true, 50.6, &observedMireds), contracts.LightSchema) {
		t.Fatal("brightness beyond half a step confirmed")
	}
	if exact.matches(lightState(t, true, 50.4, &observedMireds), contracts.LightSchema) {
		t.Fatal("an exact route admitted a distant value")
	}
	offMireds := uint32(301)
	if tolerant.matches(lightState(t, true, 50, &offMireds), contracts.LightSchema) {
		t.Fatal("colour temperature without a step must stay exact")
	}
	if tolerant.matches(lightState(t, false, 50, &observedMireds), contracts.LightSchema) {
		t.Fatal("power mismatch confirmed")
	}
	level := 33.0
	fanRequest := &actions.CommandRequest{CapabilitySchemaId: contracts.FanSchema, Endpoint: "fan", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Level: &level}}}}
	fanStep := 5.0
	fanDescription := &wireregistry.CapabilityDescription{SchemaId: contracts.FanSchema, Endpoint: "fan", Constraints: &wireregistry.CapabilityDescription_Fan{Fan: &wireregistry.FanConstraints{Level: &wireregistry.NumberBounds{Minimum: 0, Maximum: 100, Step: &fanStep}}}}
	fanPredicate := predicateForTarget(fanRequest, lightTarget{}, tolerancesFor(fanDescription))
	observedLevel := 35.0
	fan, err := domain.NewFanValue(domain.FanValueInput{Level: &observedLevel})
	if err != nil {
		t.Fatal(err)
	}
	fanValue, err := domain.NewFanStateValue(fan)
	if err != nil {
		t.Fatal(err)
	}
	if !fanPredicate.matches(fanValue, contracts.FanSchema) {
		t.Fatal("fan level within half a step was not evidence")
	}
	if predicateForTarget(fanRequest, lightTarget{}, RouteTolerances{}).matches(fanValue, contracts.FanSchema) {
		t.Fatal("exact fan level admitted a distant value")
	}
}
