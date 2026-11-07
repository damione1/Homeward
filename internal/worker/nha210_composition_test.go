package worker

import (
	"context"
	"encoding/hex"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	"github.com/damione1/Homeward/gen/home/integrations/v1/integrationsv1connect"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The transport and Host handlers, capability admission, registry projection,
// sequencer and command router are real. Only repository storage and the device
// peer are in-memory fixtures. This catches admission fences a fake Host omits.
func TestNHA210ControlsCrossRealHostDiscoveryPublicationAndCommandStream(t *testing.T) {
	closed := controls.CoverState_COVER_STATE_CLOSED
	zero := 0.0
	for _, tc := range []struct {
		name, alias string
		description *registry.CapabilityDescription
		observation *wire.Observation
		arguments   *actions.ActionArgs
		kind        domain.ValueKind
	}{
		{"cover", "cover.synthetic", &registry.CapabilityDescription{SchemaId: contracts.CoverSchema, Constraints: &registry.CapabilityDescription_Cover{Cover: &controls.CoverConstraints{Operations: []controls.CoverOperation{controls.CoverOperation_COVER_OPERATION_CLOSE}, ObservedStates: []controls.CoverState{closed}, ObservesPositionPercent: true}}}, &wire.Observation{Value: &wire.Observation_Cover{Cover: &controls.CoverValue{State: &closed, PositionPercent: &zero}}}, &actions.ActionArgs{Arguments: &actions.ActionArgs_Cover{Cover: &controls.CoverAction{Command: &controls.CoverAction_Operation{Operation: controls.CoverOperation_COVER_OPERATION_CLOSE}}}}, domain.ValueCover},
		{"lock", "lock.synthetic", &registry.CapabilityDescription{SchemaId: contracts.LockSchema, Constraints: &registry.CapabilityDescription_Lock{Lock: &controls.LockConstraints{Operations: []controls.LockOperation{controls.LockOperation_LOCK_OPERATION_LOCK}, ObservedStates: []controls.LockState{controls.LockState_LOCK_STATE_UNLOCKED, controls.LockState_LOCK_STATE_LOCKED}}}}, &wire.Observation{Value: &wire.Observation_Lock{Lock: &controls.LockValue{State: controls.LockState_LOCK_STATE_UNLOCKED}}}, &actions.ActionArgs{Arguments: &actions.ActionArgs_Lock{Lock: &controls.LockAction{Operation: controls.LockOperation_LOCK_OPERATION_LOCK}}}, domain.ValueLock},
		{"alarm", "alarm_control_panel.synthetic", &registry.CapabilityDescription{SchemaId: contracts.AlarmSchema, Constraints: &registry.CapabilityDescription_Alarm{Alarm: &controls.AlarmConstraints{Operations: []controls.AlarmOperation{controls.AlarmOperation_ALARM_OPERATION_ARM_NIGHT}, ObservedStates: []controls.AlarmState{controls.AlarmState_ALARM_STATE_DISARMED, controls.AlarmState_ALARM_STATE_ARMED_NIGHT}}}}, &wire.Observation{Value: &wire.Observation_Alarm{Alarm: &controls.AlarmValue{State: controls.AlarmState_ALARM_STATE_ARMED_NIGHT}}}, &actions.ActionArgs{Arguments: &actions.ActionArgs_Alarm{Alarm: &controls.AlarmAction{Operation: controls.AlarmOperation_ALARM_OPERATION_ARM_NIGHT}}}, domain.ValueAlarm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			host, fixture, err := fake.NewCommandFixture()
			if err != nil {
				t.Fatal(err)
			}
			instance, _ := domain.ParseIntegrationInstanceID("nha210-" + tc.name)
			key, _ := integrations.NewSourceKey("synthetic-device", tc.name)
			identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
			if err != nil || len(identities) != 1 {
				t.Fatalf("resolve: %v", err)
			}
			alias, err := domain.ParseHAAlias(tc.alias)
			if err != nil {
				t.Fatal(err)
			}
			record, err := integrations.NewRegistryRecord(identities[0].Device, identities[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
			if err != nil {
				t.Fatal(err)
			}
			if err = fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
				t.Fatal(err)
			}
			session, err := host.OpenSession(instance)
			if err != nil {
				t.Fatal(err)
			}
			p := newPilot(Config{instance: instance})
			p.active, p.token, p.ctx, p.host = true, "synthetic-nha174-token", ctx, session
			_, handler := integrationsv1connect.NewHostServiceHandler(p)
			server := httptest.NewUnstartedServer(handler)
			server.EnableHTTP2 = true
			server.StartTLS()
			defer func() { cancel(); server.Close() }()
			client := integrationsv1connect.NewHostServiceClient(server.Client(), server.URL, connect.WithGRPC())
			digest, _ := hex.DecodeString(schemaDigestHex)
			_, err = client.Handshake(ctx, nha174Request(&wire.HandshakeRequest{HostMajor: hostProtocolMajor, HostMinor: hostProtocolMinor, DriverMajor: hostProtocolMajor, DriverMinor: hostProtocolMinor, SchemaSha256: digest, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER, wire.Capability_CAPABILITY_PUBLISH, wire.Capability_CAPABILITY_COMMAND, wire.Capability_CAPABILITY_HEALTH}}))
			if err != nil {
				t.Fatal(err)
			}
			tc.description.Major, tc.description.Endpoint, tc.description.DisplayName = 1, "native", "Synthetic control"
			discovered, err := client.Discover(ctx, nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{Source: &wire.SourceKey{Device: "synthetic-device", Entity: tc.name}, Descriptions: []*registry.CapabilityDescription{tc.description}}}}))
			if err != nil {
				t.Fatalf("real Host discovery: %v", err)
			}
			if len(discovered.Msg.Bindings) != 1 || len(discovered.Msg.Bindings[0].Handle) != 32 {
				t.Fatal("missing admitted binding")
			}
			observation := tc.observation
			observation.BindingHandle, observation.Endpoint = discovered.Msg.Bindings[0].Handle, "native"
			observation.ObservedAt = timestamppb.Now()
			observation.Quality = &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}
			if _, err = client.Publish(ctx, nha174Request(&wire.PublishRequest{Observation: observation})); err != nil {
				t.Fatalf("real Host publication: %v", err)
			}
			snapshot, err := fixture.Sequencer().Capture()
			if err != nil || len(snapshot.States) != 1 {
				t.Fatalf("projection: %v", err)
			}
			for _, observed := range snapshot.States {
				value, present := observed.CurrentValue()
				kind, e := value.Kind()
				if !present || e != nil || kind != tc.kind {
					t.Fatal("typed state missing from sequencer")
				}
				switch tc.kind {
				case domain.ValueCover:
					cover, ok := value.Cover()
					s, position, obstruction, e := cover.Components()
					if !ok || e != nil || s == nil || *s != domain.CoverStateClosed || position == nil || *position != 0 || obstruction != nil {
						t.Fatal("cover zero/absence changed in projection")
					}
				case domain.ValueLock:
					lock, ok := value.Lock()
					if !ok || lock != domain.LockStateUnlocked {
						t.Fatal("lock state changed in projection")
					}
				case domain.ValueAlarm:
					alarm, ok := value.Alarm()
					if !ok || alarm != domain.AlarmStateArmedNight {
						t.Fatal("alarm Night changed in projection")
					}
				}
			}
			if err = host.AttachCommandDriver(session, p); err != nil {
				t.Fatal(err)
			}
			stream, err := client.Commands(ctx, nha174Request(&wire.CommandsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if _, err = client.ReportHealth(ctx, nha174Request(&wire.ReportHealthRequest{Status: wire.HealthStatus_HEALTH_STATUS_READY})); err != nil {
				t.Fatal(err)
			}
			service, err := integrations.NewVolatileCommandService(host, fixedGeneration{}, integrations.DefaultVolatileCommandLimits())
			if err != nil {
				t.Fatal(err)
			}
			epoch, err := fixture.CurrentAdmissionEpoch()
			if err != nil {
				t.Fatal(err)
			}
			entity, _ := identities[0].Entity.Value()
			request := &actions.CommandRequest{CommandId: "nha210-" + tc.name, AdmissionEpoch: epoch, ConfigRevision: 7, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entity}}, CapabilitySchemaId: tc.description.SchemaId, Endpoint: "native", Arguments: tc.arguments, ExpiresAt: timestamppb.New(time.Now().Add(5 * time.Second))}
			actor, _ := domain.ParseActorID("trusted-actor")
			result := make(chan error, 1)
			go func() {
				response, e := service.Dispatch(ctx, actor, request)
				if e == nil && (response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED) {
					e = ErrCommandFailed
				}
				result <- e
			}()
			if !stream.Receive() {
				t.Fatalf("command stream: %v", stream.Err())
			}
			frame := stream.Msg()
			if frame.Request.CommandId != request.CommandId || !proto.Equal(frame.Request.Arguments, tc.arguments) {
				t.Fatal("typed action changed in worker routing")
			}
			if _, err = client.CompleteCommand(ctx, nha174Request(&wire.CompleteCommandRequest{CommandId: request.CommandId, Kind: wire.CommandResultKind_COMMAND_RESULT_KIND_ACCEPTED})); err != nil {
				t.Fatal(err)
			}
			if err = <-result; err != nil {
				t.Fatalf("routed command: %v", err)
			}
		})
	}
}
