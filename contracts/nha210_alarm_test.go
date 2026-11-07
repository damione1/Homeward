package contracts

import (
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	"google.golang.org/protobuf/proto"
)

func TestNHA210AllFourAlarmOperationsAreAdmittedTogether(t *testing.T) {
	description := nhf009ControlDescriptions()[2]
	description.GetAlarm().Operations = []controls.AlarmOperation{
		controls.AlarmOperation_ALARM_OPERATION_ARM_HOME,
		controls.AlarmOperation_ALARM_OPERATION_ARM_AWAY,
		controls.AlarmOperation_ALARM_OPERATION_ARM_NIGHT,
		controls.AlarmOperation_ALARM_OPERATION_DISARM,
	}
	if len(description.GetAlarm().ObservedStates) != 10 {
		t.Fatal("incomplete observed-state fixture")
	}
	catalog, entity := testNHA014Catalog(t)
	if err := catalog.RegisterDriverCapability(entity, description); err != nil {
		t.Fatal(err)
	}
	for _, operation := range description.GetAlarm().Operations {
		args := &actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &controls.AlarmAction{Operation: operation}}}
		if err := catalog.AdmitCapabilityCommand(testNHA014Command(AlarmSchema, "alarm", args), "epoch-a", 1); err != nil {
			t.Fatalf("operation %v: %v", operation, err)
		}
	}
	for _, operations := range [][]controls.AlarmOperation{
		{1, 2, 3, 4, 4}, {1, 2, 3, 99}, {1, 2, 3, 0}, {1, 2, 3, 3},
	} {
		bad := proto.CloneOf(description)
		bad.GetAlarm().Operations = operations
		if err := catalog.AdmitDriverCapability(bad); err == nil {
			t.Fatal("unknown, duplicate or oversized operation set admitted")
		}
	}
}
