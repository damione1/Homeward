package apiv2

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/reflect/protoreflect"
	"testing"
)

func pointer[T any](v T) *T { return &v }
func traitFixtures() map[protoreflect.Name]*wire.ExecuteCapabilityRequest {
	return map[protoreflect.Name]*wire.ExecuteCapabilityRequest{
		"media":               {Command: &wire.ExecuteCapabilityRequest_Media{Media: &wire.MediaCommand{Route: &wire.MediaRoute{Entity: "homes/demo/entities/player", Coordinator: "homes/demo/entities/player", Endpoint: "transport", TopologyRevision: 1, AdmissionEpoch: "11111111111111111111111111111111", ConfigRevision: 1}, Command: &wire.MediaCommand_Operation{Operation: wire.MediaOperation_MEDIA_OPERATION_PLAY}}}},
		"light":               {Command: &wire.ExecuteCapabilityRequest_Light{Light: &wire.SetLightCommand{Power: &wire.SetPowerCommand{Value: wire.PowerValue_POWER_VALUE_OFF}}}},
		"fan":                 {Command: &wire.ExecuteCapabilityRequest_Fan{Fan: &wire.SetFanCommand{Power: &wire.SetPowerCommand{Value: wire.PowerValue_POWER_VALUE_OFF}}}},
		"temperature_control": {Command: &wire.ExecuteCapabilityRequest_TemperatureControl{TemperatureControl: &wire.SetTemperatureControlCommand{Mode: pointer(wire.TemperatureMode_TEMPERATURE_MODE_OFF)}}},
		"cover":               {Command: &wire.ExecuteCapabilityRequest_Cover{Cover: &wire.SetCoverCommand{Target: &wire.SetCoverCommand_PositionPercent{PositionPercent: 0}}}},
		"lock":                {Command: &wire.ExecuteCapabilityRequest_Lock{Lock: &wire.SetLockCommand{Operation: wire.LockOperation_LOCK_OPERATION_LOCK}}},
		"alarm":               {Command: &wire.ExecuteCapabilityRequest_Alarm{Alarm: &wire.SetAlarmCommand{Operation: wire.AlarmOperation_ALARM_OPERATION_DISARM}}},
		"helper":              {Command: &wire.ExecuteCapabilityRequest_Helper{Helper: &wire.SetHelperCommand{Operation: wire.HelperOperation_HELPER_OPERATION_TOGGLE, ExpectedHelperRevision: pointer(int64(0))}}},
	}
}
func TestTraitCommandEnumCoverage(t *testing.T) {
	tests := []struct {
		descriptor protoreflect.EnumDescriptor
		parse      func(int32) error
	}{
		{wire.MediaOperation(0).Descriptor(), func(v int32) error { _, e := ParseMediaOperation(wire.MediaOperation(v)); return e }},
		{wire.TemperatureMode(0).Descriptor(), func(v int32) error { _, e := ParseTemperatureMode(wire.TemperatureMode(v)); return e }},
		{wire.TemperatureFanTarget(0).Descriptor(), func(v int32) error { _, e := ParseTemperatureFanTarget(wire.TemperatureFanTarget(v)); return e }},
		{wire.CoverOperation(0).Descriptor(), func(v int32) error { _, e := ParseCoverOperation(wire.CoverOperation(v)); return e }},
		{wire.LockOperation(0).Descriptor(), func(v int32) error { _, e := ParseLockOperation(wire.LockOperation(v)); return e }},
		{wire.AlarmOperation(0).Descriptor(), func(v int32) error { _, e := ParseAlarmOperation(wire.AlarmOperation(v)); return e }},
		{wire.HelperOperation(0).Descriptor(), func(v int32) error { _, e := ParseHelperOperation(wire.HelperOperation(v)); return e }},
	}
	for _, test := range tests {
		values := test.descriptor.Values()
		for i := 0; i < values.Len(); i++ {
			v := values.Get(i)
			err := test.parse(int32(v.Number()))
			if (v.Number() == 0) != (err != nil) {
				t.Fatalf("unhandled declared value %s: %v", v.FullName(), err)
			}
		}
		if test.parse(999) == nil {
			t.Fatalf("unknown admitted for %s", test.descriptor.FullName())
		}
	}
}

func TestDefinitionETagIsPreservedAsOpaqueRepresentation(t *testing.T) {
	input := sample()
	input.Etag = "definition-abc"
	command, err := ParseCommand(input)
	if err != nil || command.DefinitionETag() != input.Etag {
		t.Fatal("definition token lost", err)
	}
}
