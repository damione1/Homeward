package integrations

import (
	"errors"
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA025StructuredProjectionIsNotCompleteProof(t *testing.T) {
	on := true
	brightness := 50.0
	colorTemp := uint32(300)
	zero := 0.0
	off := false
	speed := "medium"
	canonicalSpeed := "Medium"
	cases := []struct {
		name    string
		request *actions.CommandRequest
		want    actionProofKind
	}{
		{"light on only", &actions.CommandRequest{CapabilitySchemaId: contracts.LightSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on}}}}, proofSwitch},
		{"light with brightness", &actions.CommandRequest{CapabilitySchemaId: contracts.LightSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on, BrightnessPercent: &brightness}}}}, proofLight},
		{"light with color temperature", &actions.CommandRequest{CapabilitySchemaId: contracts.LightSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &on, ColorTempMireds: &colorTemp}}}}, proofLight},
		{"light color temperature without on", &actions.CommandRequest{CapabilitySchemaId: contracts.LightSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{ColorTempMireds: &colorTemp}}}}, proofLight},
		{"light off with present zero brightness", &actions.CommandRequest{CapabilitySchemaId: contracts.LightSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{On: &off, BrightnessPercent: &zero}}}}, proofLight},
		{"light without fields", &actions.CommandRequest{CapabilitySchemaId: contracts.LightSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: &actions.SetLight{}}}}, proofUnavailable},
		{"fan on only", &actions.CommandRequest{CapabilitySchemaId: contracts.FanSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{On: &on}}}}, proofFan},
		{"fan with speed", &actions.CommandRequest{CapabilitySchemaId: contracts.FanSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{On: &on, Speed: &speed}}}}, proofUnavailable},
		{"fan with canonical speed", &actions.CommandRequest{CapabilitySchemaId: contracts.FanSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: &canonicalSpeed}}}}, proofFan},
		{"climate has no Boolean proof", &actions.CommandRequest{CapabilitySchemaId: contracts.ClimateSchema, Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{}}}}, proofUnavailable},
		{"missing arguments", &actions.CommandRequest{CapabilitySchemaId: contracts.SwitchSchema}, proofUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := predicateFor(tc.request); got.kind != tc.want {
				t.Fatalf("proof kind %d, want %d", got.kind, tc.want)
			}
		})
	}
}

type nha025NilClock struct{}

func (*nha025NilClock) MonoNow() time.Duration             { return 0 }
func (*nha025NilClock) NewTimer(time.Duration) ActionTimer { return nil }

func TestNHA025ZeroTypedNilAndTrackerBudget(t *testing.T) {
	host := &Manager{actions: newActionTracker(realActionClock{})}
	var nilClock *nha025NilClock
	if err := host.SetActionClock(nilClock); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("typed nil clock: %v", err)
	}
	if err := (&Manager{}).SetActionClock(realActionClock{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero manager clock: %v", err)
	}
	if _, err := host.ActionStatus(domain.CommandID{}); err == nil {
		t.Fatal("zero command ID admitted")
	}
	id, _ := domain.ParseCommandID("command-budget")
	entity, _ := domain.ParseEntityID("entity-budget")
	device, _ := domain.ParseDeviceID("device-budget")
	instance, _ := domain.ParseIntegrationInstanceID("instance-budget")
	correlation, _ := domain.ParseCorrelationID("correlation-budget")
	key, _ := NewSourceKey("source-device", "source-entity")
	binding := Binding{key: key, device: device, entity: entity, instance: instance, session: "test-session"}
	request := &actions.CommandRequest{CommandId: "command-budget", ConfigRevision: 7, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}}
	point, _ := NewActionPoint(1, 1, 0)
	if _, err := host.actions.accept(domain.CommandID{}, entity, binding, request, point, correlation, time.Minute); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero ID accepted: %v", err)
	}
	host.actions.retained = 128 << 20 // bounded seam; no large allocation
	if _, err := host.actions.accept(id, entity, binding, request, point, correlation, time.Minute); !errors.Is(err, ErrActionProgressFull) {
		t.Fatalf("memory limit: %v", err)
	}
}
