package contracts

import (
	"math"
	"testing"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func nha178LightCommand(light *actions.SetLight) *actions.CommandRequest {
	return testNHA014Command(LightSchema, "mqtt", &actions.ActionArgs{
		Arguments: &actions.ActionArgs_SetLight{SetLight: light},
	})
}

func TestNHA178TransitionPresenceBoundsAndNoImplicitSupport(t *testing.T) {
	catalog, err := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	entity := testNHA014Entity(t)
	step := 0.1
	description := &registry.CapabilityDescription{SchemaId: LightSchema, Major: 1,
		DisplayName: "Synthetic dimmer", Endpoint: "mqtt",
		Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{
			SetOn:             true,
			BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100},
			TransitionSeconds: &registry.NumberBounds{Minimum: 0, Maximum: 6553, Step: &step},
		}}}
	if err := catalog.RegisterDriverCapability(entity, description); err != nil {
		t.Fatal(err)
	}
	// The catalogue owns a copy; a worker cannot widen admitted bounds later.
	description.GetLight().TransitionSeconds.Maximum = 7000
	if got, err := catalog.Descriptions(entity); err != nil || got[0].GetLight().TransitionSeconds.Maximum != 6553 {
		t.Fatalf("admitted constraints changed: %v %v", got, err)
	}
	on := true
	zero := 0.0
	command := nha178LightCommand(&actions.SetLight{On: &on, TransitionSeconds: &zero})
	encoded, err := proto.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(actions.CommandRequest)
	if err := proto.Unmarshal(encoded, decoded); err != nil || decoded.GetArguments().GetSetLight().TransitionSeconds == nil || *decoded.GetArguments().GetSetLight().TransitionSeconds != 0 {
		t.Fatalf("present zero vanished over wire: %v", err)
	}
	if err := catalog.AdmitCapabilityCommand(decoded, "epoch-a", 1); err != nil {
		t.Fatal(err)
	}
	for _, value := range []float64{1, 2, 10} {
		candidate := proto.Clone(decoded).(*actions.CommandRequest)
		candidate.Arguments.GetSetLight().TransitionSeconds = &value
		if got := catalog.AdmitCapabilityCommand(candidate, "epoch-a", 1); got != nil {
			t.Fatalf("supported %v seconds rejected: %v", value, got)
		}
	}
	without := proto.Clone(decoded).(*actions.CommandRequest)
	without.Arguments.GetSetLight().TransitionSeconds = nil
	if err := catalog.AdmitCapabilityCommand(without, "epoch-a", 1); err != nil {
		t.Fatalf("historical command changed: %v", err)
	}
	for _, value := range []float64{0.05, 6553.1, -0.1, math.NaN(), math.Inf(1)} {
		candidate := proto.Clone(decoded).(*actions.CommandRequest)
		candidate.Arguments.GetSetLight().TransitionSeconds = &value
		if got := catalog.AdmitCapabilityCommand(candidate, "epoch-a", 1); got == nil || got.FieldPath != "arguments.set_light.transition_seconds" {
			t.Fatalf("invalid seconds %v admitted: %v", value, got)
		}
	}
	for _, invalid := range []*actions.SetLight{
		{TransitionSeconds: &zero},
		{On: func() *bool { off := false; return &off }(), BrightnessPercent: &zero, TransitionSeconds: &zero},
		{On: func() *bool { off := false; return &off }(), ColorTempMireds: func() *uint32 { ct := uint32(300); return &ct }(), TransitionSeconds: &zero},
	} {
		if got := catalog.AdmitCapabilityCommand(nha178LightCommand(invalid), "epoch-a", 1); got == nil {
			t.Fatalf("invalid transition target admitted: %v", invalid)
		}
	}
	legacy, err := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	oldDescription := proto.Clone(description).(*registry.CapabilityDescription)
	oldDescription.GetLight().TransitionSeconds = nil
	if err := legacy.RegisterDriverCapability(entity, oldDescription); err != nil {
		t.Fatal(err)
	}
	if got := legacy.AdmitCapabilityCommand(command, "epoch-a", 1); got == nil || got.Code != actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY || got.FieldPath != "arguments.set_light.transition_seconds" {
		t.Fatalf("unadvertised transition admitted: %v", got)
	}
	if got := legacy.AdmitCapabilityCommand(without, "epoch-a", 1); got != nil {
		t.Fatalf("old light command rejected: %v", got)
	}
}
