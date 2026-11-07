package apiresources

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/contracts/apiv2"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA210FullAlarmDefinitionSurvivesAPI2ProjectionAndCommandDecoding(t *testing.T) {
	operations := []controls.AlarmOperation{controls.AlarmOperation_ALARM_OPERATION_ARM_HOME, controls.AlarmOperation_ALARM_OPERATION_ARM_AWAY, controls.AlarmOperation_ALARM_OPERATION_ARM_NIGHT, controls.AlarmOperation_ALARM_OPERATION_DISARM}
	states := []controls.AlarmState{controls.AlarmState_ALARM_STATE_DISARMED, controls.AlarmState_ALARM_STATE_ARMED_HOME, controls.AlarmState_ALARM_STATE_ARMED_AWAY, controls.AlarmState_ALARM_STATE_ARMED_NIGHT, controls.AlarmState_ALARM_STATE_ARMED_VACATION, controls.AlarmState_ALARM_STATE_ARMED_CUSTOM_BYPASS, controls.AlarmState_ALARM_STATE_PENDING, controls.AlarmState_ALARM_STATE_ARMING, controls.AlarmState_ALARM_STATE_DISARMING, controls.AlarmState_ALARM_STATE_TRIGGERED}
	d := &registry.CapabilityDescription{SchemaId: contracts.AlarmSchema, Major: 1, Endpoint: "native", DisplayName: "Synthetic alarm", Constraints: &registry.CapabilityDescription_Alarm{Alarm: &controls.AlarmConstraints{Operations: operations, ObservedStates: states}}}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if err = catalog.AdmitDriverCapability(d); err != nil {
		t.Fatal(err)
	}
	projected, err := projectAlarm(d, nil)
	if err != nil {
		t.Fatal(err)
	}
	resource := byTrait(t, projected, Alarm)
	encoded, err := proto.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(api.Capability)
	if err = proto.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	definition := decoded.GetAlarm().GetDefinition()
	want := []api.AlarmOperation{api.AlarmOperation_ALARM_OPERATION_ARM_HOME, api.AlarmOperation_ALARM_OPERATION_ARM_AWAY, api.AlarmOperation_ALARM_OPERATION_ARM_NIGHT, api.AlarmOperation_ALARM_OPERATION_DISARM}
	if len(definition.Operations) != 4 || len(definition.ObservedStates) != 10 {
		t.Fatal("API2 dropped advertised operation or state")
	}
	for i, operation := range definition.Operations {
		if operation != want[i] {
			t.Fatal("API2 operation mapping changed")
		}
		request := &api.ExecuteCapabilityRequest{Name: "homes/demo/entities/alarm/capabilities/control", ExpireTime: timestamppb.New(time.Unix(1800000000, 0)), Command: &api.ExecuteCapabilityRequest_Alarm{Alarm: &api.SetAlarmCommand{Operation: operation}}}
		encoded, err = proto.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		decodedRequest := new(api.ExecuteCapabilityRequest)
		if err = proto.Unmarshal(encoded, decodedRequest); err != nil {
			t.Fatal(err)
		}
		if _, err = apiv2.ParseCommand(decodedRequest); err != nil {
			t.Fatalf("API2 operation %v rejected: %v", operation, err)
		}
	}
}
