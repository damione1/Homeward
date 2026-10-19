package worker

import (
	"connectrpc.com/connect"
	"context"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	solarwire "github.com/damione1/Homeward/gen/home/solar/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"reflect"
	"testing"
	"unsafe"
)

func TestNHA105HistoricalWorkersRemainClosed(t *testing.T) {
	for minor := uint32(0); minor <= 3; minor++ {
		pilot, _, counted := nha174Profile(t, minor)
		for _, description := range []*registrywire.CapabilityDescription{
			{SchemaId: contracts.SolarSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic sun", Constraints: &registrywire.CapabilityDescription_Solar{Solar: &solarwire.SolarConstraints{}}},
			{SchemaId: contracts.SolarEventSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic dawn", Constraints: &registrywire.CapabilityDescription_SolarEvent{SolarEvent: &solarwire.SolarEventConstraints{Event: solarwire.SolarEventKind_SOLAR_EVENT_KIND_DAWN}}},
			{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: "Crossed branch", Constraints: &registrywire.CapabilityDescription_Solar{Solar: &solarwire.SolarConstraints{}}},
			{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: "Typed nil", Constraints: (*registrywire.CapabilityDescription_SolarEvent)(nil)},
			{SchemaId: contracts.SolarSchema, Major: 1, Endpoint: "value", DisplayName: "Crossed schema", Constraints: &registrywire.CapabilityDescription_Switch{Switch: &registrywire.SwitchConstraints{}}},
		} {
			response, err := pilot.Discover(context.Background(), nha174Discover(description))
			if response != nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatal("historical worker admitted native solar discovery")
			}
		}
		if counted.discoverCalls.Load() != 0 || len(pilot.bindings) != 0 {
			t.Fatal("solar reached historical Host or created a binding")
		}
	}
}

func TestNHA105ForgedSolarCommandIsRefusedBeforeWorkerQueue(t *testing.T) {
	// No operational constructor can admit a solar command. This test-only
	// fabrication deliberately bypasses private admission to exercise the
	// external worker's independent fence; production exposes no such hook.
	id, _ := domain.ParseCommandID("nha105-forged")
	actor, _ := domain.ParseActorID("nha105-test")
	entity, _ := domain.ParseEntityID("nha105-test")
	for _, schema := range []string{contracts.SolarSchema, contracts.SolarEventSchema} {
		command := integrations.AdmittedCommand{}
		fields := reflect.ValueOf(&command).Elem()
		for name, value := range map[string]any{"id": id, "actor": actor, "entity": entity, "request": &actions.CommandRequest{CommandId: "nha105-forged", CapabilitySchemaId: schema}} {
			field := fields.FieldByName(name)
			reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(value))
		}
		pilot := newPilot(Config{})
		if err := pilot.Dispatch(context.Background(), command); err != ErrUnavailable {
			t.Fatal("forged solar command escaped native-only fence")
		}
		if len(pilot.queue) != 0 || len(pilot.pending) != 0 || len(pilot.correlations) != 0 {
			t.Fatal("forged solar command allocated worker delivery")
		}
	}
}
