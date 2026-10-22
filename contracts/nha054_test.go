package contracts

import (
	"math"
	"testing"
	"time"

	plan "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	wire "github.com/damione1/Homeward/gen/home/configurationsimulation/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nha054Request() *wire.SimulatePlanRequest {
	state := "off"
	return &wire.SimulatePlanRequest{Candidate: &plan.Candidate{Family: plan.SourceFamily_SOURCE_FAMILY_SCRIPT, Identity: "script.fixture", Entry: "fixture.yaml", Files: []*plan.SourceFile{{LogicalPath: "fixture.yaml", ExactSource: []byte("sequence:\n- stop: done\n")}}}, Now: timestamppb.New(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)), TimeZone: "UTC", VirtualLimitMillis: 1000, Fixtures: []*wire.Fixture{{Alias: "switch.fixture", State: &state, Supports: []wire.Support{wire.Support_SUPPORT_OBSERVE_STATE, wire.Support_SUPPORT_SWITCH_TURN_ON}}}}
}
func TestNHA054RequestAdmissionRejectsFutureAndAmbiguousFixtures(t *testing.T) {
	base := nha054Request()
	if err := AdmitConfigurationSimulationRequest(base); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*wire.SimulatePlanRequest){
		func(v *wire.SimulatePlanRequest) { v.Candidate.Family = plan.SourceFamily(999) },
		func(v *wire.SimulatePlanRequest) { v.Fixtures[0].Supports = []wire.Support{999} },
		func(v *wire.SimulatePlanRequest) { v.Fixtures[0].Supports = []wire.Support{1, 1} },
		func(v *wire.SimulatePlanRequest) {
			v.Fixtures = append(v.Fixtures, proto.Clone(v.Fixtures[0]).(*wire.Fixture))
		},
		func(v *wire.SimulatePlanRequest) { v.Fixtures[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) },
		func(v *wire.SimulatePlanRequest) { v.VirtualLimitMillis = 0 },
		func(v *wire.SimulatePlanRequest) { v.VirtualLimitMillis = 3600001 },
		func(v *wire.SimulatePlanRequest) {
			v.Fixtures[0].Attributes = []*wire.Attribute{{Name: "nan", Value: &wire.Attribute_Number{Number: math.NaN()}}}
		},
		func(v *wire.SimulatePlanRequest) {
			v.Fixtures[0].Attributes = []*wire.Attribute{{Name: "missing", Value: &wire.Attribute_NullValue{NullValue: false}}}
		},
		func(v *wire.SimulatePlanRequest) {
			v.Fixtures[0].State = nil
			v.Fixtures[0].Attributes = []*wire.Attribute{{Name: "unobserved", Value: &wire.Attribute_Boolean{Boolean: false}}}
		},
	} {
		value := proto.Clone(base).(*wire.SimulatePlanRequest)
		mutate(value)
		if AdmitConfigurationSimulationRequest(value) == nil {
			t.Fatalf("malformed fixture admitted: %v", value)
		}
	}
}
func nha054Response() *wire.SimulatePlanResponse {
	return &wire.SimulatePlanResponse{Report: &wire.Report{Simulated: true, Status: wire.Status_STATUS_COMPLETED, Limitations: []string{"Fixture only; no hardware validation."}, Intents: []*wire.Intent{{Alias: "switch.fixture", Position: &wire.Position{File: "fixture.yaml", Path: "sequence[0]", Line: 2, Column: 3}, At: nha054Request().Now, Outcome: wire.IntentOutcome_INTENT_OUTCOME_RECORDED, Parameters: &wire.Intent_Switch{Switch: &wire.SwitchIntent{On: false}}}}}}
}
func TestNHA054ReportAdmissionRejectsHardwareClaimsAndFutureVariants(t *testing.T) {
	base := nha054Response()
	if err := AdmitConfigurationSimulationResponse(base); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*wire.SimulatePlanResponse){
		func(v *wire.SimulatePlanResponse) { v.Report.Simulated = false },
		func(v *wire.SimulatePlanResponse) { v.Report.HardwareValidated = true },
		func(v *wire.SimulatePlanResponse) { v.Report.Status = 999 },
		func(v *wire.SimulatePlanResponse) { v.Report.Intents[0].Outcome = 999 },
		func(v *wire.SimulatePlanResponse) { v.Report.Intents[0].Parameters = nil },
		func(v *wire.SimulatePlanResponse) { v.Report.Limitations = nil },
		func(v *wire.SimulatePlanResponse) { v.Report.Intents[0].Position.Line = 0 },
		func(v *wire.SimulatePlanResponse) {
			v.Report.Intents[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
		},
		func(v *wire.SimulatePlanResponse) { v.Report.Status = wire.Status_STATUS_PARTIAL },
		func(v *wire.SimulatePlanResponse) {
			nan := math.NaN()
			v.Report.Intents[0].Parameters = &wire.Intent_Light{Light: &wire.LightIntent{BrightnessPercent: &nan}}
		},
	} {
		value := proto.Clone(base).(*wire.SimulatePlanResponse)
		mutate(value)
		if AdmitConfigurationSimulationResponse(value) == nil {
			t.Fatalf("false report admitted: %v", value)
		}
	}
}
