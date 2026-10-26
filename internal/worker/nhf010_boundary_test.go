package worker

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	control "github.com/damione1/Homeward/gen/home/controls/v1"
	env "github.com/damione1/Homeward/gen/home/environment/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
)

func TestNHF010HistoricalWorkerProfilesDoNotAcquireNewNativeObservations(t *testing.T) {
	for minor := uint32(0); minor <= 3; minor++ {
		pilot, _, counted := nha174Profile(t, minor)
		for _, description := range []*registry.CapabilityDescription{
			{SchemaId: contracts.WeatherSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic weather", Constraints: &registry.CapabilityDescription_Weather{Weather: &env.WeatherConstraints{}}},
			{SchemaId: contracts.PresenceSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic presence", Constraints: &registry.CapabilityDescription_Presence{Presence: &env.PresenceConstraints{}}},
			{SchemaId: contracts.PresenceDetailsSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic private position", Constraints: &registry.CapabilityDescription_PresenceDetails{PresenceDetails: &env.PresenceDetailsConstraints{}}},
			{SchemaId: contracts.CoverSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic cover", Constraints: &registry.CapabilityDescription_Cover{Cover: &control.CoverConstraints{ObservesPositionPercent: true}}},
			{SchemaId: contracts.LockSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic lock", Constraints: &registry.CapabilityDescription_Lock{Lock: &control.LockConstraints{ObservedStates: []control.LockState{control.LockState_LOCK_STATE_LOCKED}}}},
			{SchemaId: contracts.AlarmSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic alarm", Constraints: &registry.CapabilityDescription_Alarm{Alarm: &control.AlarmConstraints{ObservedStates: []control.AlarmState{control.AlarmState_ALARM_STATE_DISARMED}}}},
			{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: "Crossed native branch", Constraints: &registry.CapabilityDescription_Presence{Presence: &env.PresenceConstraints{}}},
			{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: "Typed nil native branch", Constraints: (*registry.CapabilityDescription_PresenceDetails)(nil)},
		} {
			response, err := pilot.Discover(context.Background(), nha174Discover(description))
			if response != nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatal("historical worker profile acquired native data semantics", minor, err)
			}
		}
		if counted.discoverCalls.Load() != 0 || len(pilot.bindings) != 0 {
			t.Fatal("refused discovery reached Host or created bindings")
		}
	}
}
