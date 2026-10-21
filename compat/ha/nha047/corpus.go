package nha047

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type Scenario uint8

const (
	ScenarioInvalid Scenario = iota
	TimeClimate
	SunSwitch
	SceneProjection
	LegacyAction
	ExcludedAction
	MissingBlueprint
)

func (s Scenario) String() string {
	switch s {
	case TimeClimate:
		return "time_climate"
	case SunSwitch:
		return "sun_switch"
	case SceneProjection:
		return "scene_projection"
	case LegacyAction:
		return "legacy_action"
	case ExcludedAction:
		return "excluded_action"
	case MissingBlueprint:
		return "missing_blueprint"
	case ScenarioInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

type DemandKind uint8

const (
	DemandInvalid DemandKind = iota
	LightOff
	LightOn
	SwitchOff
	SwitchOn
	FanOff
	FanOn
	ClimateTemperature
	ClimateMode
	ClimateHumidity
	ClimateFanMode
	ClimatePresetMode
	Refusal
)

var demandNames = [...]string{"", "light_off", "light_on", "switch_off", "switch_on", "fan_off", "fan_on", "climate_temperature", "climate_mode", "climate_humidity", "climate_fan_mode", "climate_preset_mode", "refusal"}

func (k DemandKind) String() string {
	if k <= DemandInvalid || int(k) >= len(demandNames) {
		return "invalid"
	}
	return demandNames[k]
}

// Demand is the admitted observation boundary, never an instruction to a Host.
// The two numeric fields have meaning only in their closed demand variant.
type Demand struct {
	kind      DemandKind
	target    string
	value     float64
	secondary float64
	code      string
}

type DemandWire struct {
	Kind      string  `json:"kind"`
	Target    string  `json:"target"`
	Value     float64 `json:"value"`
	Secondary float64 `json:"secondary"`
	Code      string  `json:"code"`
}

func (w *DemandWire) UnmarshalJSON(data []byte) error {
	type fields DemandWire
	var value fields
	if err := strictObjectJSON(data, &value, MaxCaptureBytes, "kind", "target", "value", "secondary", "code"); err != nil {
		return err
	}
	*w = DemandWire(value)
	return nil
}

func NewDemand(kind DemandKind, target string, value, secondary float64, code string) (Demand, error) {
	if !validID(target) || !strings.Contains(target, ".nha047_") {
		return Demand{}, ErrInvalidCapture
	}
	valid := false
	switch kind {
	case LightOff, SwitchOff, SwitchOn, FanOff:
		valid = value == 0 && secondary == 0 && code == ""
	case LightOn:
		_, err := NewFeature(SceneLightBrightness, value, 0)
		valid = err == nil && value > 0 && (secondary == 0 || secondary == 250 || secondary == 357 || secondary == 383) && code == ""
	case FanOn:
		valid = value == 75 && secondary == 0 && code == ""
	case ClimateTemperature:
		valid = !math.IsNaN(value) && !math.IsInf(value, 0) && value >= -273.15 && value <= 1000 && secondary == 0 && code == ""
	case ClimateMode:
		valid = value == 0 && secondary == 0 && code == "heat"
	case ClimateHumidity:
		valid = value == 40 && secondary == 0 && code == ""
	case ClimateFanMode:
		valid = value == 0 && secondary == 0 && code == "auto"
	case ClimatePresetMode:
		valid = value == 0 && secondary == 0 && code == "none"
	case Refusal:
		valid = value == 0 && secondary == 0 && (code == "unsupported_climate_attributes" || code == "excluded_reference" || code == "blueprint_source_missing")
	case DemandInvalid:
		valid = false
	default:
		valid = false
	}
	if !valid {
		return Demand{}, ErrInvalidCapture
	}
	return Demand{kind, target, value, secondary, code}, nil
}

func (d Demand) Kind() DemandKind   { return d.kind }
func (d Demand) Target() string     { return d.target }
func (d Demand) Value() float64     { return d.value }
func (d Demand) Secondary() float64 { return d.secondary }
func (d Demand) Code() string       { return d.code }
func (d Demand) wire() DemandWire {
	return DemandWire{d.kind.String(), d.target, d.value, d.secondary, d.code}
}
func demandFromWire(w DemandWire) (Demand, error) {
	for k := LightOff; k <= Refusal; k++ {
		if k.String() == w.Kind {
			return NewDemand(k, w.Target, w.Value, w.Secondary, w.Code)
		}
	}
	return Demand{}, ErrInvalidCapture
}

type caseWire struct {
	Schema     string        `json:"schema"`
	ID         string        `json:"id"`
	Scenario   string        `json:"scenario"`
	Object     string        `json:"object"`
	YAML       string        `json:"yaml"`
	Candidate  string        `json:"candidate"`
	At         string        `json:"at"`
	Season     bool          `json:"season"`
	Workday    bool          `json:"workday"`
	Features   []FeatureWire `json:"features"`
	HAExpected []DemandWire  `json:"ha_expected"`
	GoExpected []DemandWire  `json:"go_expected"`
}

type Case struct {
	wire       caseWire
	scenario   Scenario
	features   []Feature
	haExpected []Demand
	goExpected []Demand
	digest     Digest
}

func (c Case) ID() string          { return c.wire.ID }
func (c Case) Digest() Digest      { return c.digest }
func (c Case) Scenario() Scenario  { return c.scenario }
func (c Case) Object() string      { return c.wire.Object }
func (c Case) YAML() string        { return c.wire.YAML }
func (c Case) Candidate() string   { return c.wire.Candidate }
func (c Case) At() string          { return c.wire.At }
func (c Case) Season() bool        { return c.wire.Season }
func (c Case) Workday() bool       { return c.wire.Workday }
func (c Case) Features() []Feature { return append([]Feature(nil), c.features...) }
func (c Case) Valid() bool         { return c.digest.Valid() && c.scenario != ScenarioInvalid }
func (c Case) Role() ComparisonRole {
	if !c.Valid() {
		return RoleInvalid
	}
	if c.wire.Candidate != "" {
		return OriginalToCandidate
	}
	return SameSource
}

// This is the expected provenance used only at comparison. Producers hash
// their consumed snapshot instead of using this expected value as evidence.
func (c Case) expectedInput(runtime string) (Digest, error) {
	if !c.Valid() || runtime != "ha" && runtime != "go" {
		return Digest{}, ErrInvalidInput
	}
	input := c.wire.YAML
	if c.Role() == OriginalToCandidate && runtime == "go" {
		input = c.wire.Candidate
	}
	return Hash([]byte(input)), nil
}

func (c Case) JSON() ([]byte, error) {
	if !c.Valid() {
		return nil, ErrInvalidInput
	}
	return json.Marshal(c.wire)
}

// DecodeCase admits exact registered cases only. The caller cannot turn a
// supplied feature list or a test name into a qualification certificate.
func DecodeCase(data []byte) (Case, error) {
	var raw caseWire
	if err := strictObjectJSON(data, &raw, MaxInputBytes, "schema", "id", "scenario", "object", "yaml", "candidate", "at", "season", "workday", "features", "ha_expected", "go_expected"); err != nil {
		return Case{}, err
	}
	canonical, err := json.Marshal(raw)
	if err != nil {
		return Case{}, err
	}
	all, err := Corpus()
	if err != nil {
		return Case{}, err
	}
	for _, c := range all {
		if c.ID() == raw.ID && c.digest == Hash(canonical) {
			return c, nil
		}
	}
	return Case{}, ErrInvalidInput
}

func Corpus() ([]Case, error) {
	var result []Case
	feature := func(c Construction, value, secondary float64) Feature {
		f, _ := NewFeature(c, value, secondary)
		return f
	}
	demand := func(k DemandKind, value, secondary float64) Demand {
		d, _ := NewDemand(k, "light.nha047_value", value, secondary, "")
		return d
	}
	add := func(id string, scenario Scenario, object, yaml, candidate, at string, season, workday bool, features []Feature, ha, goSide []Demand) error {
		if !validID(id) || len(result) >= MaxCases || len(yaml) == 0 || len(yaml)+len(candidate) > 1<<20 {
			return ErrInvalidInput
		}
		w := caseWire{Schema: Schema, ID: id, Scenario: scenario.String(), Object: object, YAML: yaml, Candidate: candidate, At: at, Season: season, Workday: workday, Features: []FeatureWire{}, HAExpected: []DemandWire{}, GoExpected: []DemandWire{}}
		for _, f := range features {
			if !f.Valid() {
				return ErrInvalidInput
			}
			w.Features = append(w.Features, f.wire())
		}
		for _, d := range ha {
			w.HAExpected = append(w.HAExpected, d.wire())
		}
		for _, d := range goSide {
			w.GoExpected = append(w.GoExpected, d.wire())
		}
		encoded, err := json.Marshal(w)
		if err != nil {
			return err
		}
		result = append(result, Case{w, scenario, append([]Feature(nil), features...), append([]Demand(nil), ha...), append([]Demand(nil), goSide...), Hash(encoded)})
		return nil
	}
	for _, setpoint := range []int{15, 18, 20} {
		for _, enabled := range []bool{false, true} {
			hour := 9
			if setpoint == 18 {
				hour = 20
			}
			at := fmt.Sprintf("%02d:00:00", hour)
			condition := "- condition: state\n  entity_id: switch.nha047_season\n  state: 'on'\n"
			if setpoint == 20 {
				condition += "- condition: state\n  entity_id: switch.nha047_workday\n  state: 'on'\n"
			}
			yaml := "id: nha047_time\ntriggers:\n- trigger: time\n  at: '" + at + "'\nconditions:\n" + condition + "actions:\n- action: climate.set_temperature\n  target: {entity_id: climate.nha047_value}\n  data: {temperature: " + fmt.Sprint(setpoint) + "}\nmode: single\n"
			var facts []Demand
			if enabled {
				d, _ := NewDemand(ClimateTemperature, "climate.nha047_value", float64(setpoint), 0, "")
				facts = append(facts, d)
			}
			features := []Feature{feature(TriggerTime, float64(hour*3600), 0), feature(ConditionState, 0, 0), feature(ClimateSetpoint, float64(setpoint), 0)}
			if err := add(fmt.Sprintf("time-%d-%t", setpoint, enabled), TimeClimate, "automation.nha047_time", yaml, "", at, enabled, enabled, features, facts, facts); err != nil {
				return nil, err
			}
			// Exercise the second AND operand independently of the first.
			if setpoint == 20 && !enabled {
				if err := add("time-20-workday-false", TimeClimate, "automation.nha047_time", yaml, "", at, true, false, features, nil, nil); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, pair := range []struct {
		b      int
		ct     int
		kelvin int
	}{{3, 0, 0}, {25, 0, 0}, {51, 0, 0}, {76, 0, 0}, {89, 0, 0}, {102, 0, 0}, {128, 0, 0}, {153, 0, 0}, {255, 0, 0}, {89, 357, 2801}, {255, 250, 4000}, {76, 383, 2610}, {153, 357, 2801}} {
		body := fmt.Sprintf("    state: 'on'\n    brightness: %d\n    color_mode: brightness\n", pair.b)
		features := []Feature{feature(SceneLightBrightness, float64(pair.b), 0), feature(SceneMetadata, 0, 0)}
		if pair.ct != 0 {
			body = fmt.Sprintf("    state: 'on'\n    brightness: %d\n    color_mode: color_temp\n    color_temp_kelvin: %d\n    rgb_color: [255, 170, 95]\n    hs_color: [28.185, 62.588]\n    xy_color: [0.516, 0.386]\n", pair.b, pair.kelvin)
			features = append(features, feature(SceneLightCT, float64(pair.b), float64(pair.ct)))
		}
		body += "    friendly_name: Synthetic Light\n    supported_features: 32\n"
		yaml := "id: '1'\nname: Synthetic\nentities:\n  light.nha047_value:\n" + body + "metadata: {}\n"
		facts := []Demand{demand(LightOn, float64(pair.b), float64(pair.ct))}
		if err := add(fmt.Sprintf("scene-light-%d-%d", pair.b, pair.ct), SceneProjection, "scene.nha047_value", yaml, "", "", false, false, features, facts, facts); err != nil {
			return nil, err
		}
	}
	for _, kind := range []string{"light-off", "switch-on", "switch-off", "fan-on", "fan-off"} {
		entity, body, k := "light.nha047_value", "    state: 'off'\n    brightness: 255\n    color_temp_kelvin: 2801\n    color_mode: color_temp\n", LightOff
		f := feature(SceneLightOff, 0, 0)
		switch kind {
		case "switch-on":
			entity, body, k, f = "switch.nha047_value", "    state: 'on'\n", SwitchOn, feature(SceneSwitch, 1, 0)
		case "switch-off":
			entity, body, k, f = "switch.nha047_value", "    state: 'off'\n", SwitchOff, feature(SceneSwitch, 0, 0)
		case "fan-on":
			entity, body, k, f = "fan.nha047_value", "    state: 'on'\n    percentage: 75\n    preset_mode: null\n", FanOn, feature(SceneFan, 75, 0)
		case "fan-off":
			entity, body, k, f = "fan.nha047_value", "    state: 'off'\n    percentage: 75\n    preset_mode: null\n", FanOff, feature(SceneFan, 0, 0)
		}
		value := float64(0)
		if k == FanOn {
			value = 75
		}
		d, err := NewDemand(k, entity, value, 0, "")
		if err != nil {
			return nil, err
		}
		yaml := "id: '1'\nname: Synthetic\nentities:\n  " + entity + ":\n" + body + "metadata: {}\n"
		if err := add("scene-"+kind, SceneProjection, "scene.nha047_value", yaml, "", "", false, false, []Feature{f, feature(SceneMetadata, 0, 0)}, []Demand{d}, []Demand{d}); err != nil {
			return nil, err
		}
	}
	canonical := "id: nha047_legacy\ntriggers:\n- trigger: time\n  at: '09:00:00'\nconditions: []\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.nha047_value}\n  data: {}\nmode: single\n"
	legacy := strings.Replace(canonical, "- action: switch.turn_on", "- service: switch.turn_on", 1)
	d, _ := NewDemand(SwitchOn, "switch.nha047_value", 0, 0, "")
	if err := add("legacy-service-alias", LegacyAction, "automation.nha047_legacy", legacy, canonical, "09:00:00", false, false, []Feature{feature(LegacyServiceAlias, 0, 0)}, []Demand{d}, []Demand{d}); err != nil {
		return nil, err
	}
	if err := add("legacy-action-canonical", LegacyAction, "automation.nha047_legacy", canonical, "", "09:00:00", false, false, []Feature{feature(TriggerTime, 9*3600, 0), feature(SwitchTurnOn, 0, 0)}, []Demand{d}, []Demand{d}); err != nil {
		return nil, err
	}
	sunset := "id: nha047_sun\ntriggers:\n- trigger: sun\n  event: sunset\n  offset: '-01:00:00'\nconditions: []\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.nha047_value}\n  data: {}\nmode: single\n"
	if err := add("sunset-switch-on", SunSwitch, "automation.nha047_sun", sunset, "", "", false, false, []Feature{feature(TriggerSunset, -3600, 0), feature(SwitchTurnOn, 0, 0)}, []Demand{d}, []Demand{d}); err != nil {
		return nil, err
	}
	climateYAML := "id: '1'\nname: Synthetic climate\nentities:\n  climate.nha047_value:\n    state: heat\n    temperature: 20\n    humidity: 40\n    fan_mode: auto\n    preset_mode: none\nmetadata: {}\n"
	var climateHA []Demand
	for _, row := range []struct {
		kind  DemandKind
		value float64
		code  string
	}{{ClimateMode, 0, "heat"}, {ClimateTemperature, 20, ""}, {ClimatePresetMode, 0, "none"}, {ClimateFanMode, 0, "auto"}, {ClimateHumidity, 40, ""}} {
		d, e := NewDemand(row.kind, "climate.nha047_value", row.value, 0, row.code)
		if e != nil {
			return nil, e
		}
		climateHA = append(climateHA, d)
	}
	refused, e := NewDemand(Refusal, "climate.nha047_value", 0, 0, "unsupported_climate_attributes")
	if e != nil {
		return nil, e
	}
	if e = add("scene-climate-unrepresentable", SceneProjection, "scene.nha047_value", climateYAML, "", "", false, false, []Feature{feature(SceneClimateUnsupported, 0, 0)}, climateHA, []Demand{refused}); e != nil {
		return nil, e
	}
	return result, nil
}
