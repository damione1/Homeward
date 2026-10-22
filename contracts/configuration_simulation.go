package contracts

import (
	"errors"
	"math"
	"unicode/utf8"

	plan "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationsimulation/v1"
	"google.golang.org/protobuf/proto"
)

var ErrInvalidConfigurationSimulationContract = errors.New("invalid configuration simulation contract")

func simulationSupport(value wire.Support) bool {
	switch value {
	case wire.Support_SUPPORT_OBSERVE_STATE, wire.Support_SUPPORT_CLIMATE_SETPOINT, wire.Support_SUPPORT_SWITCH_TURN_ON, wire.Support_SUPPORT_LIGHT_TURN_ON, wire.Support_SUPPORT_LIGHT_TURN_OFF, wire.Support_SUPPORT_LIGHT_BRIGHTNESS_ABSOLUTE, wire.Support_SUPPORT_LIGHT_BRIGHTNESS_RELATIVE, wire.Support_SUPPORT_LIGHT_TRANSITION, wire.Support_SUPPORT_SCENE_TURN_ON, wire.Support_SUPPORT_SCENE_TRANSITION:
		return true
	case wire.Support_SUPPORT_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func simulationAttribute(value *wire.Attribute) bool {
	if value == nil || !planLabel(value.Name, 128) {
		return false
	}
	switch item := value.Value.(type) {
	case *wire.Attribute_Text:
		return item != nil && utf8.ValidString(item.Text) && len(item.Text) <= 4096
	case *wire.Attribute_Boolean:
		return item != nil
	case *wire.Attribute_Integer:
		return item != nil
	case *wire.Attribute_Number:
		return item != nil && !math.IsNaN(item.Number) && !math.IsInf(item.Number, 0)
	case *wire.Attribute_NullValue:
		return item != nil && item.NullValue
	case nil:
		return false
	default:
		return false
	}
}
func AdmitConfigurationSimulationRequest(message *wire.SimulatePlanRequest) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) || !planCandidate(message.Candidate) || proto.Size(message) > 2<<20 || !configurationTime(message.Now) || message.Now.AsTime().Year() < 1900 || message.Now.AsTime().Year() > 2100 || !planLabel(message.TimeZone, 256) || message.VirtualLimitMillis == 0 || message.VirtualLimitMillis > 3600000 || len(message.Fixtures) > 128 || len(message.EventTypes) > 128 {
		return ErrInvalidConfigurationSimulationContract
	}
	switch message.Candidate.Family {
	case plan.SourceFamily_SOURCE_FAMILY_AUTOMATION, plan.SourceFamily_SOURCE_FAMILY_SCRIPT, plan.SourceFamily_SOURCE_FAMILY_SCENE:
	default:
		return ErrInvalidConfigurationSimulationContract
	}
	seen := map[string]bool{}
	bytes := 0
	for _, fixture := range message.Fixtures {
		if fixture == nil || !planLabel(fixture.Alias, 255) || seen[fixture.Alias] || len(fixture.Supports) > 10 || len(fixture.Attributes) > 32 || fixture.State == nil && len(fixture.Attributes) != 0 {
			return ErrInvalidConfigurationSimulationContract
		}
		seen[fixture.Alias] = true
		bytes += proto.Size(fixture)
		if bytes > 64<<10 {
			return ErrInvalidConfigurationSimulationContract
		}
		if fixture.State != nil && (!utf8.ValidString(*fixture.State) || len(*fixture.State) > 4096) {
			return ErrInvalidConfigurationSimulationContract
		}
		supports := map[wire.Support]bool{}
		for _, support := range fixture.Supports {
			if !simulationSupport(support) || supports[support] {
				return ErrInvalidConfigurationSimulationContract
			}
			supports[support] = true
		}
		attributes := map[string]bool{}
		for _, attribute := range fixture.Attributes {
			if !simulationAttribute(attribute) || attributes[attribute.Name] {
				return ErrInvalidConfigurationSimulationContract
			}
			attributes[attribute.Name] = true
		}
	}
	events := map[string]bool{}
	for _, event := range message.EventTypes {
		if !planLabel(event, 128) || events[event] {
			return ErrInvalidConfigurationSimulationContract
		}
		events[event] = true
	}
	return nil
}
func simulationPosition(position *wire.Position) bool {
	if position == nil {
		return false
	}
	if position.File == "" {
		return position.Path == "" && position.Line == 0 && position.Column == 0
	}
	return planLabel(position.File, 4096) && len(position.Path) <= 4096 && utf8.ValidString(position.Path) && position.Line > 0 && position.Column > 0
}
func simulationDiagnostic(value wire.DiagnosticCode) bool {
	switch value {
	case wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSUPPORTED_EXTENSION, wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSUPPORTED_CONCURRENCY, wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSUPPORTED_WAIT, wire.DiagnosticCode_DIAGNOSTIC_CODE_INVALID_SOURCE, wire.DiagnosticCode_DIAGNOSTIC_CODE_MISSING_FIXTURE, wire.DiagnosticCode_DIAGNOSTIC_CODE_FIXTURE_REFUSED, wire.DiagnosticCode_DIAGNOSTIC_CODE_EXECUTION_FAILED, wire.DiagnosticCode_DIAGNOSTIC_CODE_CAPACITY_EXCEEDED:
		return true
	case wire.DiagnosticCode_DIAGNOSTIC_CODE_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func simulationFinite(value *float64, min, max float64) bool {
	return value == nil || !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= min && *value <= max
}
func simulationIntent(value *wire.Intent) bool {
	if value == nil || !planLabel(value.Alias, 255) || !simulationPosition(value.Position) || value.Position.File == "" || !configurationTime(value.At) {
		return false
	}
	switch value.Outcome {
	case wire.IntentOutcome_INTENT_OUTCOME_RECORDED, wire.IntentOutcome_INTENT_OUTCOME_REFUSED:
	case wire.IntentOutcome_INTENT_OUTCOME_UNSPECIFIED:
		return false
	default:
		return false
	}
	switch item := value.Parameters.(type) {
	case *wire.Intent_Switch:
		return item != nil && item.Switch != nil
	case *wire.Intent_Light:
		if item == nil || item.Light == nil {
			return false
		}
		light := item.Light
		present := 0
		if light.BrightnessPercent != nil {
			present++
		}
		if light.BrightnessStepPercent != nil {
			present++
		}
		if light.Brightness_255 != nil {
			present++
		}
		return present <= 1 && (light.On != nil || present > 0 || light.ColorTempMireds != nil) && simulationFinite(light.BrightnessPercent, 0, 100) && simulationFinite(light.BrightnessStepPercent, -100, 100) && simulationFinite(light.TransitionSeconds, 0, 86400) && (light.Brightness_255 == nil || *light.Brightness_255 <= 255) && (light.ColorTempMireds == nil || *light.ColorTempMireds > 0)
	case *wire.Intent_Climate:
		if item == nil || item.Climate == nil {
			return false
		}
		climate := item.Climate
		if climate.Mode != nil {
			if _, err := AdmitWireClimateMode(*climate.Mode); err != nil {
				return false
			}
		}
		return (climate.TemperatureCelsius != nil || climate.Mode != nil || climate.HumidityPercent != nil || climate.FanMode != nil || climate.PresetMode != nil) && simulationFinite(climate.TemperatureCelsius, -273.15, 1000) && simulationFinite(climate.HumidityPercent, 0, 100) && (climate.FanMode == nil || planLabel(*climate.FanMode, 128)) && (climate.PresetMode == nil || planLabel(*climate.PresetMode, 128))
	case *wire.Intent_Fan:
		return item != nil && item.Fan != nil && item.Fan.Percentage <= 100
	case nil:
		return value.Outcome == wire.IntentOutcome_INTENT_OUTCOME_REFUSED
	default:
		return false
	}
}
func simulationAction(value wire.ActionKind) bool {
	switch value {
	case wire.ActionKind_ACTION_KIND_VARIABLES, wire.ActionKind_ACTION_KIND_SEQUENCE, wire.ActionKind_ACTION_KIND_CHOOSE, wire.ActionKind_ACTION_KIND_IF, wire.ActionKind_ACTION_KIND_REPEAT, wire.ActionKind_ACTION_KIND_PARALLEL, wire.ActionKind_ACTION_KIND_WAIT, wire.ActionKind_ACTION_KIND_STOP, wire.ActionKind_ACTION_KIND_CALL, wire.ActionKind_ACTION_KIND_DELAY, wire.ActionKind_ACTION_KIND_WAIT_TEMPLATE:
		return true
	case wire.ActionKind_ACTION_KIND_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func simulationStage(value wire.NodeStage) bool {
	switch value {
	case wire.NodeStage_NODE_STAGE_ENTERED, wire.NodeStage_NODE_STAGE_COMPLETED, wire.NodeStage_NODE_STAGE_STOPPED, wire.NodeStage_NODE_STAGE_CANCELLED, wire.NodeStage_NODE_STAGE_FAILED, wire.NodeStage_NODE_STAGE_WAITING:
		return true
	case wire.NodeStage_NODE_STAGE_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func AdmitConfigurationSimulationResponse(message *wire.SimulatePlanResponse) error {
	if message == nil || !configurationKnown(message.ProtoReflect()) || proto.Size(message) > 512<<10 || message.Report == nil {
		return ErrInvalidConfigurationSimulationContract
	}
	r := message.Report
	if !r.Simulated || r.HardwareValidated || r.ElapsedMillis > 3600000 || len(r.Intents) > 256 || len(r.Nodes) > 1024 || len(r.Diagnostics) > 512 || len(r.Limitations) < 1 || len(r.Limitations) > 8 {
		return ErrInvalidConfigurationSimulationContract
	}
	switch r.Status {
	case wire.Status_STATUS_COMPLETED:
		if len(r.Diagnostics) != 0 {
			return ErrInvalidConfigurationSimulationContract
		}
	case wire.Status_STATUS_PARTIAL, wire.Status_STATUS_FAILED:
		if len(r.Diagnostics) == 0 {
			return ErrInvalidConfigurationSimulationContract
		}
	case wire.Status_STATUS_UNSPECIFIED:
		return ErrInvalidConfigurationSimulationContract
	default:
		return ErrInvalidConfigurationSimulationContract
	}
	for _, limit := range r.Limitations {
		if !planLabel(limit, 1024) {
			return ErrInvalidConfigurationSimulationContract
		}
	}
	for _, intent := range r.Intents {
		if !simulationIntent(intent) {
			return ErrInvalidConfigurationSimulationContract
		}
	}
	for _, diagnostic := range r.Diagnostics {
		if diagnostic == nil || !simulationDiagnostic(diagnostic.Code) || !simulationPosition(diagnostic.Position) {
			return ErrInvalidConfigurationSimulationContract
		}
	}
	for _, node := range r.Nodes {
		if node == nil || !simulationPosition(node.Position) || node.Position.File == "" || !simulationAction(node.Action) || !simulationStage(node.Stage) || !configurationTime(node.At) || len(node.Branches) > 8 || len(node.Iterations) > 8 {
			return ErrInvalidConfigurationSimulationContract
		}
	}
	return nil
}
