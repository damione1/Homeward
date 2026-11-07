package worker

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	env "github.com/damione1/Homeward/gen/home/environment/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
)

func TestNHF010CurrentWorkerRejectsUnsupportedEnvironmentDescriptions(t *testing.T) {
	for _, minor := range []uint32{hostProtocolMinor} {
		pilot, _, counted := nha174Profile(t)
		for _, description := range []*registry.CapabilityDescription{
			{SchemaId: contracts.WeatherSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic weather", Constraints: &registry.CapabilityDescription_Weather{Weather: &env.WeatherConstraints{}}},
			{SchemaId: contracts.PresenceSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic presence", Constraints: &registry.CapabilityDescription_Presence{Presence: &env.PresenceConstraints{}}},
			{SchemaId: contracts.PresenceDetailsSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic private position", Constraints: &registry.CapabilityDescription_PresenceDetails{PresenceDetails: &env.PresenceDetailsConstraints{}}},
			{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: "Crossed native branch", Constraints: &registry.CapabilityDescription_Presence{Presence: &env.PresenceConstraints{}}},
			{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: "Typed nil native branch", Constraints: (*registry.CapabilityDescription_PresenceDetails)(nil)},
		} {
			response, err := pilot.Discover(context.Background(), nha174Discover(description))
			if response != nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatal("current worker acquired native data semantics", minor, err)
			}
		}
		if counted.discoverCalls.Load() != 0 || len(pilot.bindings) != 0 {
			t.Fatal("refused discovery reached Host or created bindings")
		}
	}
}
