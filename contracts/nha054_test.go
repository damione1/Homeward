package contracts

import (
	"math"
	"testing"
	"time"

	plan "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nha054Request() *wire.SimulateConfigurationPlanRequest {
	state := "off"
	return &wire.SimulateConfigurationPlanRequest{Name: "homes/synthetic", Candidate: &plan.ConfigurationPlanCandidate{Family: plan.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_SCRIPT, Identity: "script.fixture", Entry: "fixture.yaml", Files: []*plan.ConfigurationPlanSourceFile{{LogicalPath: "fixture.yaml", ExactSource: []byte("sequence:\n- stop: done\n")}}}, Now: timestamppb.New(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)), TimeZone: "UTC", VirtualLimitMillis: 1000, Fixtures: []*wire.Fixture{{Alias: "switch.fixture", State: &state, Supports: []wire.ConfigurationSimulationSupport{wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_OBSERVE_STATE, wire.ConfigurationSimulationSupport_CONFIGURATION_SIMULATION_SUPPORT_SWITCH_TURN_ON}}}}
}
func TestNHA054RequestAdmissionRejectsFutureAndAmbiguousFixtures(t *testing.T) {
	base := nha054Request()
	if err := AdmitConfigurationSimulationRequest(base); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*wire.SimulateConfigurationPlanRequest){
		func(v *wire.SimulateConfigurationPlanRequest) {
			v.Candidate.Family = plan.ConfigurationPlanSourceFamily(999)
		},
		func(v *wire.SimulateConfigurationPlanRequest) {
			v.Fixtures[0].Supports = []wire.ConfigurationSimulationSupport{999}
		},
		func(v *wire.SimulateConfigurationPlanRequest) {
			v.Fixtures[0].Supports = []wire.ConfigurationSimulationSupport{1, 1}
		},
		func(v *wire.SimulateConfigurationPlanRequest) {
			v.Fixtures = append(v.Fixtures, proto.Clone(v.Fixtures[0]).(*wire.Fixture))
		},
		func(v *wire.SimulateConfigurationPlanRequest) {
			v.Fixtures[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		},
		func(v *wire.SimulateConfigurationPlanRequest) { v.VirtualLimitMillis = 0 },
		func(v *wire.SimulateConfigurationPlanRequest) { v.VirtualLimitMillis = 3600001 },
		func(v *wire.SimulateConfigurationPlanRequest) {
			v.Fixtures[0].Attributes = []*wire.ConfigurationSimulationAttribute{{Name: "nan", Value: &wire.ConfigurationSimulationAttribute_Number{Number: math.NaN()}}}
		},
		func(v *wire.SimulateConfigurationPlanRequest) {
			v.Fixtures[0].Attributes = []*wire.ConfigurationSimulationAttribute{{Name: "missing", Value: &wire.ConfigurationSimulationAttribute_NullValue{NullValue: false}}}
		},
		func(v *wire.SimulateConfigurationPlanRequest) {
			v.Fixtures[0].State = nil
			v.Fixtures[0].Attributes = []*wire.ConfigurationSimulationAttribute{{Name: "unobserved", Value: &wire.ConfigurationSimulationAttribute_Boolean{Boolean: false}}}
		},
	} {
		value := proto.Clone(base).(*wire.SimulateConfigurationPlanRequest)
		mutate(value)
		if AdmitConfigurationSimulationRequest(value) == nil {
			t.Fatalf("malformed fixture admitted: %v", value)
		}
	}
}
func nha054Response() *wire.SimulateConfigurationPlanResponse {
	return &wire.SimulateConfigurationPlanResponse{Report: &wire.ConfigurationSimulationReport{Simulated: true, Status: wire.ConfigurationSimulationStatus_CONFIGURATION_SIMULATION_STATUS_COMPLETED, Limitations: []string{"Fixture only; no hardware validation."}, Intents: []*wire.ConfigurationSimulationIntent{{Alias: "switch.fixture", Position: &wire.ConfigurationSimulationPosition{File: "fixture.yaml", Path: "sequence[0]", Line: 2, Column: 3}, At: nha054Request().Now, Outcome: wire.IntentOutcome_INTENT_OUTCOME_RECORDED, Parameters: &wire.ConfigurationSimulationIntent_Switch{Switch: &wire.SwitchIntent{On: false}}}}}}
}
func TestNHA054ReportAdmissionRejectsHardwareClaimsAndFutureVariants(t *testing.T) {
	base := nha054Response()
	if err := AdmitConfigurationSimulationResponse(base); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*wire.SimulateConfigurationPlanResponse){
		func(v *wire.SimulateConfigurationPlanResponse) { v.Report.Simulated = false },
		func(v *wire.SimulateConfigurationPlanResponse) { v.Report.HardwareValidated = true },
		func(v *wire.SimulateConfigurationPlanResponse) { v.Report.Status = 999 },
		func(v *wire.SimulateConfigurationPlanResponse) { v.Report.Intents[0].Outcome = 999 },
		func(v *wire.SimulateConfigurationPlanResponse) { v.Report.Intents[0].Parameters = nil },
		func(v *wire.SimulateConfigurationPlanResponse) { v.Report.Limitations = nil },
		func(v *wire.SimulateConfigurationPlanResponse) { v.Report.Intents[0].Position.Line = 0 },
		func(v *wire.SimulateConfigurationPlanResponse) {
			v.Report.Intents[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		},
		func(v *wire.SimulateConfigurationPlanResponse) {
			v.Report.Status = wire.ConfigurationSimulationStatus_CONFIGURATION_SIMULATION_STATUS_PARTIAL
		},
		func(v *wire.SimulateConfigurationPlanResponse) {
			nan := math.NaN()
			v.Report.Intents[0].Parameters = &wire.ConfigurationSimulationIntent_Light{Light: &wire.LightIntent{BrightnessPercent: &nan}}
		},
	} {
		value := proto.Clone(base).(*wire.SimulateConfigurationPlanResponse)
		mutate(value)
		if AdmitConfigurationSimulationResponse(value) == nil {
			t.Fatalf("false report admitted: %v", value)
		}
	}
}
