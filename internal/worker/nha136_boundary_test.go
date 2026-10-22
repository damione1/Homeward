package worker

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
)

func TestNHA136HistoricalWorkersRejectNewClimateBeforeHost(t *testing.T) {
	for _, minor := range []uint32{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("profile-%d", minor), func(t *testing.T) {
			p, _, counted := nha174Profile(t, minor)
			for name, set := range map[string]func(*registry.ClimateConstraints){
				"command-range": func(c *registry.ClimateConstraints) {
					c.SetpointRangeCelsius = &registry.ClimateSetpointRangeConstraints{}
				},
				"command-fan": func(c *registry.ClimateConstraints) {
					c.FanTargetOptions = []registry.ClimateFanTarget{registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON}
				},
				"observed-humidity": func(c *registry.ClimateConstraints) {
					c.ObservedHumidityPercent = &registry.NumberBounds{Minimum: 0, Maximum: 100}
				},
				"observed-target": func(c *registry.ClimateConstraints) {
					c.ObservedFanTargetOptions = []registry.ClimateFanTarget{registry.ClimateFanTarget_CLIMATE_FAN_TARGET_AUTO}
				},
			} {
				t.Run(name, func(t *testing.T) {
					description := nha174WorkerDescription()
					set(description.GetClimate())
					if _, err := p.Discover(context.Background(), nha174Discover(description)); connect.CodeOf(err) != connect.CodeInvalidArgument {
						t.Fatalf("new field passed historical profile: %v", err)
					}
				})
			}
			zero := 0.0
			on := registry.ClimateFanTarget_CLIMATE_FAN_TARGET_ON
			for _, climate := range []*state.ClimateValue{{HumidityPercent: &zero}, {FanTarget: &on}} {
				request := nha174Request(&wire.PublishRequest{Observation: &wire.Observation{BindingHandle: make([]byte, 32), Endpoint: "climate", Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}, Value: &wire.Observation_Climate{Climate: climate}}})
				if _, err := p.Publish(context.Background(), request); connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("new observation passed historical profile: %v", err)
				}
			}
			if counted.discoverCalls.Load() != 0 || counted.publishCalls.Load() != 0 {
				t.Fatal("new field reached Host")
			}
			if _, err := p.Discover(context.Background(), nha174Discover(nha174WorkerDescription())); err != nil {
				t.Fatal("old climate witness refused", err)
			}
			if counted.discoverCalls.Load() != 1 {
				t.Fatal("old climate witness did not reach Host")
			}
		})
	}
}
