package worker

import (
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
)

func TestNHA210WorkerControlObservationAdmission(t *testing.T) {
	closed := controls.CoverState_COVER_STATE_CLOSED
	zero := 0.0
	cases := []struct {
		name  string
		value *wire.Observation
		kind  domain.ValueKind
		valid bool
	}{
		{"cover", &wire.Observation{Value: &wire.Observation_Cover{Cover: &controls.CoverValue{State: &closed, PositionPercent: &zero}}}, domain.ValueCover, true},
		{"lock", &wire.Observation{Value: &wire.Observation_Lock{Lock: &controls.LockValue{State: controls.LockState_LOCK_STATE_UNLOCKED}}}, domain.ValueLock, true},
		{"alarm", &wire.Observation{Value: &wire.Observation_Alarm{Alarm: &controls.AlarmValue{State: controls.AlarmState_ALARM_STATE_DISARMED}}}, domain.ValueAlarm, true},
		{"unknown lock", &wire.Observation{Value: &wire.Observation_Lock{Lock: &controls.LockValue{State: 99}}}, 0, false},
		{"empty alarm", &wire.Observation{Value: &wire.Observation_Alarm{Alarm: &controls.AlarmValue{}}}, 0, false},
		{"empty cover", &wire.Observation{Value: &wire.Observation_Cover{Cover: &controls.CoverValue{}}}, 0, false},
		{"nil alarm", &wire.Observation{Value: (*wire.Observation_Alarm)(nil)}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, err := parseValue(tc.value)
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid observation admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			kind, err := value.Kind()
			if err != nil || kind != tc.kind {
				t.Fatalf("kind = %v, err = %v", kind, err)
			}
		})
	}
}
