package server

import (
	"context"
	"math"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/compat/ha/ir"
	hascene "github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/configsimulation"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type configSimulationHandler struct {
	documents *configDocumentsHandler
}

func (h *configSimulationHandler) SimulateConfigurationPlan(ctx context.Context, request *connect.Request[wire.SimulateConfigurationPlanRequest]) (*connect.Response[wire.SimulateConfigurationPlanResponse], error) {
	if h == nil || h.documents == nil {
		return nil, configPlanError(ctx, configsimulation.ErrInvalid)
	}
	// The current native human/admin boundary precedes source access or
	// compilation. Fixture declarations never confer production permissions.
	binding, err := h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, nil)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	if request == nil || contracts.AdmitConfigurationSimulationRequest(request.Msg) != nil {
		return nil, configPlanError(ctx, configsimulation.ErrInvalid)
	}
	if err := h.documents.names.parent(ctx, request.Msg.Name); err != nil {
		return nil, configPlanError(ctx, err)
	}
	candidate, err := configPlanCandidate(request.Msg.Candidate)
	if err != nil {
		return nil, configPlanError(ctx, err)
	}
	snapshot, err := simulationSnapshot(request.Msg)
	if err != nil {
		return nil, configPlanError(ctx, configsimulation.ErrInvalid)
	}
	zone, err := time.LoadLocation(request.Msg.TimeZone)
	if err != nil {
		return nil, configPlanError(ctx, configsimulation.ErrInvalid)
	}
	report, err := configsimulation.Run(ctx, configsimulation.Input{Candidate: candidate, Snapshot: snapshot, Now: request.Msg.Now.AsTime(), Zone: zone, VirtualLimit: time.Duration(request.Msg.VirtualLimitMillis) * time.Millisecond})
	if err != nil {
		if ctx.Err() != nil {
			return nil, configPlanError(ctx, ctx.Err())
		}
		return nil, configPlanError(ctx, configsimulation.ErrInvalid)
	}
	response := &wire.SimulateConfigurationPlanResponse{Report: simulationReport(report)}
	if contracts.AdmitConfigurationSimulationResponse(response) != nil {
		return nil, configPlanError(ctx, errorsSimulationUnavailable)
	}
	// A policy/session change while simulation ran invalidates the result.
	if _, err := h.documents.admit(ctx, auth.ScopeOperation_SCOPE_OPERATION_ADMINISTER, &binding); err != nil {
		return nil, configPlanError(ctx, err)
	}
	return connect.NewResponse(response), nil
}

func simulationSupport(value wire.ConfigurationSimulationSupport) (ir.DeclaredSupport, error) {
	switch value {
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_OBSERVE_STATE:
		return ir.SupportObserveState, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_CLIMATE_SETPOINT:
		return ir.SupportClimateSetpoint, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_SWITCH_TURN_ON:
		return ir.SupportSwitchTurnOn, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_LIGHT_TURN_ON:
		return ir.SupportLightTurnOn, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_LIGHT_TURN_OFF:
		return ir.SupportLightTurnOff, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_LIGHT_BRIGHTNESS_ABSOLUTE:
		return ir.SupportLightBrightnessAbsolute, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_LIGHT_BRIGHTNESS_RELATIVE:
		return ir.SupportLightBrightnessRelative, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_LIGHT_TRANSITION:
		return ir.SupportLightTransition, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_SCENE_TURN_ON:
		return ir.SupportSceneTurnOn, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_SCENE_TRANSITION:
		return ir.SupportSceneTransition, nil
	case wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_UNSPECIFIED:
		return ir.SupportInvalid, configsimulation.ErrInvalid
	default:
		return ir.SupportInvalid, configsimulation.ErrInvalid
	}
}
func simulationSnapshot(request *wire.SimulateConfigurationPlanRequest) (configsimulation.Snapshot, error) {
	fixtures := make([]configsimulation.Fixture, 0, len(request.Fixtures))
	for _, fixture := range request.Fixtures {
		attributes := make([]template.Binding, 0, len(fixture.Attributes))
		for _, attribute := range fixture.Attributes {
			var value template.Value
			var err error
			switch item := attribute.Value.(type) {
			case *wire.ConfigurationSimulationAttribute_Text:
				value, err = template.String(item.Text)
			case *wire.ConfigurationSimulationAttribute_Boolean:
				value = template.Bool(item.Boolean)
			case *wire.ConfigurationSimulationAttribute_Integer:
				value = template.Int(item.Integer)
			case *wire.ConfigurationSimulationAttribute_Number:
				value, err = template.Float(item.Number)
			case *wire.ConfigurationSimulationAttribute_NullValue:
				value = template.None()
			case nil:
				return configsimulation.Snapshot{}, configsimulation.ErrInvalid
			default:
				return configsimulation.Snapshot{}, configsimulation.ErrInvalid
			}
			if err != nil {
				return configsimulation.Snapshot{}, err
			}
			attributes = append(attributes, template.Binding{Name: attribute.Name, Value: value})
		}
		supports := make([]ir.DeclaredSupport, 0, len(fixture.Supports))
		for _, support := range fixture.Supports {
			admitted, err := simulationSupport(support)
			if err != nil {
				return configsimulation.Snapshot{}, err
			}
			supports = append(supports, admitted)
		}
		admitted, err := configsimulation.NewFixture(fixture.Alias, fixture.State, attributes, supports, fixture.RefuseIntent)
		if err != nil {
			return configsimulation.Snapshot{}, err
		}
		fixtures = append(fixtures, admitted)
	}
	events := make([]ir.Event, 0, len(request.EventTypes))
	for _, name := range request.EventTypes {
		event, err := ir.NewEvent(name, ir.ReferencePresent)
		if err != nil {
			return configsimulation.Snapshot{}, err
		}
		events = append(events, event)
	}
	return configsimulation.NewSnapshotWithEvents(fixtures, events)
}
func simulationPosition(value configsimulation.Position) *wire.ConfigurationSimulationPosition {
	if value.Line < 0 || value.Column < 0 || int64(value.Line) > math.MaxInt32 || int64(value.Column) > math.MaxInt32 {
		return nil
	}
	return &wire.ConfigurationSimulationPosition{File: value.File, Path: value.Path, Line: int32(value.Line), Column: int32(value.Column)}
}
func simulationSource(value ir.Source) *wire.ConfigurationSimulationPosition {
	at := value.At()
	return simulationPosition(configsimulation.Position{File: at.File, Path: value.Path(), Line: at.Line, Column: at.Column})
}
func simulationPointer[T any](value T) *T { return &value }
func simulationIntent(value configsimulation.Intent) *wire.ConfigurationSimulationIntent {
	alias, _ := value.Alias().Value()
	out := &wire.ConfigurationSimulationIntent{Alias: alias, Position: simulationPosition(value.Position()), At: timestamppb.New(value.At())}
	switch value.Outcome() {
	case configsimulation.Recorded:
		out.Outcome = wire.IntentOutcome_INTENT_OUTCOME_RECORDED
	case configsimulation.Refused:
		out.Outcome = wire.IntentOutcome_INTENT_OUTCOME_REFUSED
	case configsimulation.IntentOutcomeInvalid:
		return out
	default:
		return out
	}
	call := value.Call()
	switch call.Kind() {
	case ir.CallSwitchTurnOn:
		out.Parameters = &wire.ConfigurationSimulationIntent_Switch{Switch: &wire.SwitchIntent{On: true}}
	case ir.CallClimateSetTemperature:
		out.Parameters = &wire.ConfigurationSimulationIntent_Climate{Climate: &wire.ClimateIntent{TemperatureCelsius: simulationPointer(call.Temperature().Value())}}
	case ir.CallLightTurnOn, ir.CallLightTurnOff:
		light := &wire.LightIntent{On: simulationPointer(call.Kind() == ir.CallLightTurnOn)}
		if number := call.BrightnessPercent(); number.Present() {
			light.BrightnessPercent = simulationPointer(number.Value())
		}
		if number := call.BrightnessStepPercent(); number.Present() {
			light.BrightnessStepPercent = simulationPointer(number.Value())
		}
		if number := call.TransitionSeconds(); number.Present() {
			light.TransitionSeconds = simulationPointer(number.Value())
		}
		out.Parameters = &wire.ConfigurationSimulationIntent_Light{Light: light}
	case ir.CallSceneTurnOn, ir.CallScript:
		return out
	case ir.CallInvalid:
		member := value.SceneMember()
		if len(member.Diagnostics()) != 0 {
			return out
		}
		switch member.Kind() {
		case hascene.MemberLight:
			intent, _ := member.Light()
			on, brightness, ct := intent.Components()
			light := &wire.LightIntent{On: &on, ColorTempMireds: simulationMireds(ct)}
			if brightness != nil {
				light.Brightness_255 = simulationPointer(int32(*brightness))
			}
			out.Parameters = &wire.ConfigurationSimulationIntent_Light{Light: light}
		case hascene.MemberSwitch:
			on, _ := member.Switch()
			out.Parameters = &wire.ConfigurationSimulationIntent_Switch{Switch: &wire.SwitchIntent{On: on}}
		case hascene.MemberFan:
			intent, _ := member.Fan()
			out.Parameters = &wire.ConfigurationSimulationIntent_Fan{Fan: &wire.FanIntent{On: intent.On(), Percentage: int32(intent.Percentage())}}
		case hascene.MemberClimate:
			intent, _ := member.Climate()
			mode, temperature, humidity, fan, preset := intent.Components()
			climate := &wire.ClimateIntent{TemperatureCelsius: temperature, HumidityPercent: humidity, FanMode: fan, PresetMode: preset}
			if mode != domain.ClimateModeUnspecified {
				converted := simulationClimateMode(mode)
				if converted != wire.TemperatureMode_TEMPERATURE_MODE_UNSPECIFIED {
					climate.Mode = &converted
				}
			}
			out.Parameters = &wire.ConfigurationSimulationIntent_Climate{Climate: climate}
		case hascene.MemberInvalid:
			return out
		default:
			return out
		}
	default:
		return out
	}
	return out
}
func simulationAction(value ir.ActionKind) wire.ConfigurationSimulationActionKind {
	switch value {
	case ir.ActionVariables:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_VARIABLES
	case ir.ActionSequence:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_SEQUENCE
	case ir.ActionChoose:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_CHOOSE
	case ir.ActionIf:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_IF
	case ir.ActionRepeat:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_REPEAT
	case ir.ActionParallel:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_PARALLEL
	case ir.ActionWait:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_WAIT
	case ir.ActionStop:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_STOP
	case ir.ActionCall:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_CALL
	case ir.ActionDelay:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_DELAY
	case ir.ActionWaitTemplate:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_WAIT_TEMPLATE
	case ir.ActionInvalid:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_UNSPECIFIED
	default:
		return wire.ConfigurationSimulationActionKind_CONFIGURATION_SIMULATION_ACTION_KIND_UNSPECIFIED
	}
}
func simulationStage(value automationruns.NodeStage) wire.ConfigurationSimulationNodeStage {
	switch value {
	case automationruns.NodeEntered:
		return wire.ConfigurationSimulationNodeStage_CONFIGURATION_SIMULATION_NODE_STAGE_ENTERED
	case automationruns.NodeCompleted:
		return wire.ConfigurationSimulationNodeStage_CONFIGURATION_SIMULATION_NODE_STAGE_COMPLETED
	case automationruns.NodeStopped:
		return wire.ConfigurationSimulationNodeStage_CONFIGURATION_SIMULATION_NODE_STAGE_STOPPED
	case automationruns.NodeCancelled:
		return wire.ConfigurationSimulationNodeStage_CONFIGURATION_SIMULATION_NODE_STAGE_CANCELLED
	case automationruns.NodeFailed:
		return wire.ConfigurationSimulationNodeStage_CONFIGURATION_SIMULATION_NODE_STAGE_FAILED
	case automationruns.NodeWaiting:
		return wire.ConfigurationSimulationNodeStage_CONFIGURATION_SIMULATION_NODE_STAGE_WAITING
	case automationruns.NodeStageInvalid:
		return wire.ConfigurationSimulationNodeStage_CONFIGURATION_SIMULATION_NODE_STAGE_UNSPECIFIED
	default:
		return wire.ConfigurationSimulationNodeStage_CONFIGURATION_SIMULATION_NODE_STAGE_UNSPECIFIED
	}
}
func simulationDiagnostic(value configsimulation.DiagnosticCode) wire.ConfigurationSimulationDiagnosticCode {
	switch value {
	case configsimulation.UnsupportedExtension:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_UNSUPPORTED_EXTENSION
	case configsimulation.UnsupportedConcurrency:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_UNSUPPORTED_CONCURRENCY
	case configsimulation.UnsupportedWait:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_UNSUPPORTED_WAIT
	case configsimulation.InvalidSource:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_INVALID_SOURCE
	case configsimulation.MissingFixture:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_MISSING_FIXTURE
	case configsimulation.FixtureRefused:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_FIXTURE_REFUSED
	case configsimulation.ExecutionFailed:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_EXECUTION_FAILED
	case configsimulation.CapacityExceeded:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_CAPACITY_EXCEEDED
	case configsimulation.DiagnosticInvalid:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_UNSPECIFIED
	default:
		return wire.ConfigurationSimulationDiagnosticCode_CONFIGURATION_SIMULATION_DIAGNOSTIC_CODE_UNSPECIFIED
	}
}
func simulationReport(value configsimulation.Report) *wire.ConfigurationSimulationReport {
	out := &wire.ConfigurationSimulationReport{Simulated: value.Simulated(), HardwareValidated: value.HardwareValidated(), ElapsedMillis: int64(value.Elapsed() / time.Millisecond), Limitations: value.Limitations()}
	switch value.Status() {
	case configsimulation.Completed:
		out.Status = wire.ConfigurationSimulationStatus_CONFIGURATION_SIMULATION_STATUS_COMPLETED
	case configsimulation.Partial:
		out.Status = wire.ConfigurationSimulationStatus_CONFIGURATION_SIMULATION_STATUS_PARTIAL
	case configsimulation.Failed:
		out.Status = wire.ConfigurationSimulationStatus_CONFIGURATION_SIMULATION_STATUS_FAILED
	case configsimulation.StatusInvalid:
	default:
	}
	for _, intent := range value.Intents() {
		out.Intents = append(out.Intents, simulationIntent(intent))
	}
	for _, diagnostic := range value.Diagnostics() {
		out.Diagnostics = append(out.Diagnostics, &wire.ConfigurationSimulationDiagnostic{Code: simulationDiagnostic(diagnostic.Code), Position: simulationPosition(diagnostic.Position)})
	}
	for _, node := range value.Nodes() {
		trace := &wire.ConfigurationSimulationNodeTrace{Position: simulationSource(node.Source()), Action: simulationAction(node.Kind()), Stage: simulationStage(node.Stage()), At: timestamppb.New(node.At())}
		for _, branch := range node.Branches() {
			if uint64(branch) > math.MaxInt32 {
				return nil
			}
			trace.Branches = append(trace.Branches, int32(branch))
		}
		for _, iteration := range node.Iterations() {
			if uint64(iteration) > math.MaxInt32 {
				return nil
			}
			trace.Iterations = append(trace.Iterations, int32(iteration))
		}
		out.Nodes = append(out.Nodes, trace)
	}
	return out
}

func simulationClimateMode(value domain.ClimateMode) wire.TemperatureMode {
	switch value {
	case domain.ClimateModeOff:
		return wire.TemperatureMode_TEMPERATURE_MODE_OFF
	case domain.ClimateModeHeat:
		return wire.TemperatureMode_TEMPERATURE_MODE_HEAT
	case domain.ClimateModeCool:
		return wire.TemperatureMode_TEMPERATURE_MODE_COOL
	case domain.ClimateModeAuto:
		return wire.TemperatureMode_TEMPERATURE_MODE_AUTO
	case domain.ClimateModeDry:
		return wire.TemperatureMode_TEMPERATURE_MODE_DRY
	case domain.ClimateModeFanOnly:
		return wire.TemperatureMode_TEMPERATURE_MODE_FAN_ONLY
	case domain.ClimateModeUnspecified:
		return wire.TemperatureMode_TEMPERATURE_MODE_UNSPECIFIED
	default:
		return wire.TemperatureMode_TEMPERATURE_MODE_UNSPECIFIED
	}
}

func simulationMireds(value *uint32) *int64 {
	if value == nil {
		return nil
	}
	result := int64(*value)
	return &result
}
