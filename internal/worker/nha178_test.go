package worker

import (
	"context"
	"testing"

	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
)

func nha178Description() *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1,
		DisplayName: "Synthetic fading light", Endpoint: "light",
		Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{
			SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100},
			TransitionSeconds: &registry.NumberBounds{Minimum: 0, Maximum: 6553, Step: nha174Pointer(1.0)},
		}}}
}

func TestNHA178CurrentHostAdmitsTransition(t *testing.T) {
	p, _, counted := nha174Profile(t)
	request := nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{
		Source:       &wire.SourceKey{Device: "synthetic-thermostat", Entity: "climate"},
		Descriptions: []*registry.CapabilityDescription{nha178Description()},
	}}})
	response, err := p.Discover(context.Background(), request)
	if err != nil || len(response.Msg.Bindings) != 1 || counted.discoverCalls.Load() != 1 {
		t.Fatalf("Host 1.4 refused admitted light: %v", err)
	}
}
