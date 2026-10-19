package mqtt

import (
	"testing"
	"time"

	"github.com/damione1/Homeward/contracts"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func nha182CommandCapabilities(descriptions []*registry.CapabilityDescription, schema string) bool {
	if len(descriptions) != 1 || descriptions[0] == nil {
		return false
	}
	description := descriptions[0]
	if description.SchemaId != schema || description.Major != 1 || description.Endpoint != "mqtt" {
		return false
	}
	switch schema {
	case contracts.SwitchSchema:
		return description.GetSwitch() != nil && proto.Equal(description.GetSwitch(), &registry.SwitchConstraints{SetOn: true})
	case contracts.LightSchema:
		return description.GetLight() != nil && proto.Equal(description.GetLight(), &registry.LightConstraints{
			SetOn:             true,
			BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100},
			ColorTempMireds:   &registry.NumberBounds{Minimum: 250, Maximum: 454},
		})
	default:
		return false
	}
}

func nha182WaitCommandCapabilities(t *testing.T, r *nha069Rig, tasks *nha068Tasks, switchEntity, lightEntity domain.EntityID) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		switches, switchErr := r.manager.Capabilities(switchEntity)
		lights, lightErr := r.manager.Capabilities(lightEntity)
		select {
		case workerErr := <-tasks.errors:
			t.Fatalf("discovery worker failed: %v; switch=%v (%v), light=%v (%v); diagnostics=%v", workerErr, switches, switchErr, lights, lightErr, r.mapper.Diagnostics())
		default:
		}
		if switchErr == nil && lightErr == nil && nha182CommandCapabilities(switches, contracts.SwitchSchema) && nha182CommandCapabilities(lights, contracts.LightSchema) {
			if diagnostics := r.mapper.Diagnostics(); len(diagnostics) != 0 {
				t.Fatalf("valid command discovery produced diagnostics: %v", diagnostics)
			}
			return
		}
		select {
		case workerErr := <-tasks.errors:
			t.Fatalf("discovery worker failed: %v; switch=%v (%v), light=%v (%v); diagnostics=%v", workerErr, switches, switchErr, lights, lightErr, r.mapper.Diagnostics())
		case <-deadline.C:
			t.Fatalf("exact command descriptions did not reach Host: switch=%v (%v), light=%v (%v); diagnostics=%v", switches, switchErr, lights, lightErr, r.mapper.Diagnostics())
		case <-poll.C:
		}
	}
}

func TestNHA182CommandCapabilityBarrierRejectsEmptyAndPartialDescriptions(t *testing.T) {
	switchDescription := &registry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "mqtt", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}
	lightDescription := &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "mqtt", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100}, ColorTempMireds: &registry.NumberBounds{Minimum: 250, Maximum: 454}}}}
	for _, fixture := range []struct {
		name, schema string
		description  *registry.CapabilityDescription
	}{
		{"switch", contracts.SwitchSchema, switchDescription},
		{"light", contracts.LightSchema, lightDescription},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if !nha182CommandCapabilities([]*registry.CapabilityDescription{fixture.description}, fixture.schema) {
				t.Fatal("exact description was rejected")
			}
			for _, descriptions := range [][]*registry.CapabilityDescription{nil, {}, {nil}, {fixture.description, fixture.description}} {
				if nha182CommandCapabilities(descriptions, fixture.schema) {
					t.Fatalf("empty, nil or ambiguous descriptions admitted: %v", descriptions)
				}
			}
			for _, mutate := range []func(*registry.CapabilityDescription){
				func(d *registry.CapabilityDescription) { d.Major = 0 },
				func(d *registry.CapabilityDescription) { d.Endpoint = "other" },
				func(d *registry.CapabilityDescription) { d.SchemaId = contracts.MeasurementSchema },
				func(d *registry.CapabilityDescription) { d.Constraints = nil },
			} {
				changed := proto.Clone(fixture.description).(*registry.CapabilityDescription)
				mutate(changed)
				if nha182CommandCapabilities([]*registry.CapabilityDescription{changed}, fixture.schema) {
					t.Fatalf("partial description admitted: %v", changed)
				}
			}
		})
	}
	for _, mutate := range []func(*registry.LightConstraints){
		func(c *registry.LightConstraints) { c.SetOn = false },
		func(c *registry.LightConstraints) { c.BrightnessPercent = nil },
		func(c *registry.LightConstraints) { c.BrightnessPercent.Maximum = 254 },
		func(c *registry.LightConstraints) { c.ColorTempMireds = nil },
		func(c *registry.LightConstraints) { c.ColorTempMireds.Minimum = 0 },
		func(c *registry.LightConstraints) { c.ColorTempMireds.Maximum = 500 },
	} {
		changed := proto.Clone(lightDescription).(*registry.CapabilityDescription)
		mutate(changed.GetLight())
		if nha182CommandCapabilities([]*registry.CapabilityDescription{changed}, contracts.LightSchema) {
			t.Fatalf("wrong light command constraints admitted: %v", changed)
		}
	}
	changedSwitch := proto.Clone(switchDescription).(*registry.CapabilityDescription)
	changedSwitch.GetSwitch().SetOn = false
	if nha182CommandCapabilities([]*registry.CapabilityDescription{changedSwitch}, contracts.SwitchSchema) {
		t.Fatal("non-commandable switch admitted")
	}
	if nha182CommandCapabilities([]*registry.CapabilityDescription{lightDescription}, "future-schema") {
		t.Fatal("unknown schema admitted")
	}
}
