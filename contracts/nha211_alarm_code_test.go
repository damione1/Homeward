package contracts

import (
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	"testing"
)

func TestNHA211InteractiveCodeRequirementIsAnAdmittedOperationSubset(t *testing.T) {
	declaration := nhf009ControlDescriptions()[2]
	constraints := declaration.GetAlarm()
	constraints.Operations = []controls.AlarmOperation{controls.AlarmOperation_ALARM_OPERATION_ARM_HOME, controls.AlarmOperation_ALARM_OPERATION_DISARM}
	constraints.CodeRequiredOperations = []controls.AlarmOperation{controls.AlarmOperation_ALARM_OPERATION_DISARM}
	catalog, _ := testNHA014Catalog(t)
	if err := catalog.AdmitDriverCapability(declaration); err != nil {
		t.Fatal(err)
	}
	for _, operations := range [][]controls.AlarmOperation{{0}, {99}, {3, 3}, {4}} {
		constraints.CodeRequiredOperations = operations
		if catalog.AdmitDriverCapability(declaration) == nil {
			t.Fatal("invalid credential policy admitted", operations)
		}
	}
	if !validActionStatus(actions.ActionStatus_ACTION_STATUS_SENT_UNVERIFIED) || validActionStatus(actions.ActionStatus(99)) {
		t.Fatal("dispatch result enum admission changed")
	}
}
