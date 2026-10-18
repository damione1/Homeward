package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha077CountingHost struct {
	integrations.SessionHost
	discoverCalls atomic.Int32
	publishCalls  atomic.Int32
}

func (h *nha077CountingHost) Discover(ctx context.Context, candidates []integrations.Candidate) ([]integrations.Binding, error) {
	h.discoverCalls.Add(1)
	return h.SessionHost.Discover(ctx, candidates)
}
func (h *nha077CountingHost) Publish(ctx context.Context, binding integrations.Binding, observation integrations.StateObservation) error {
	h.publishCalls.Add(1)
	return h.SessionHost.Publish(ctx, binding, observation)
}

func TestNHA077LegacyProfileRejectsFanDiscoverAndPublishBeforeHost(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "hold", true)
	waitMarker(t, rig.marker, "second_stream_rejected", 5*time.Second)
	rig.pilot.mu.Lock()
	token := rig.pilot.token
	before := len(rig.pilot.bindings)
	var handle []byte
	for key := range rig.pilot.bindings {
		handle = []byte(key)
		break
	}
	counted := &nha077CountingHost{SessionHost: rig.pilot.host}
	rig.pilot.host = counted
	rig.pilot.mu.Unlock()
	if len(handle) != 32 {
		t.Fatal("legacy binding missing")
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.FanSchema,
		Major: 1, Endpoint: "speed", DisplayName: "Synthetic fan",
		Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{
			SpeedOptions: []string{"Off", "High"}}}}
	key, err := integrations.NewSourceKey("valid-fan-device", "valid-fan-zone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description}); err != nil {
		t.Fatalf("fan candidate was malformed before profile guard: %v", err)
	}
	discover := connect.NewRequest(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{
		Source:       &wire.SourceKey{Device: "valid-fan-device", Entity: "valid-fan-zone"},
		Descriptions: []*registry.CapabilityDescription{description}}}})
	discover.Header().Set("Authorization", "Bearer "+token)
	if _, err := rig.pilot.Discover(context.Background(), discover); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Host 1.0 admitted fan Discover: %v", err)
	}
	publish := connect.NewRequest(&wire.PublishRequest{Observation: &wire.Observation{
		BindingHandle: handle, Endpoint: "speed", Quality: &state.Quality{
			Availability: state.Availability_AVAILABILITY_AVAILABLE,
			Freshness:    state.Freshness_FRESHNESS_FRESH,
			Provenance:   state.Provenance_PROVENANCE_OBSERVED},
		Value: &wire.Observation_Fan{Fan: &state.FanValue{Speed: state.FanSpeed_FAN_SPEED_OFF}},
	}})
	publish.Header().Set("Authorization", "Bearer "+token)
	if _, err := rig.pilot.Publish(context.Background(), publish); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Host 1.0 admitted fan Publish: %v", err)
	}
	rig.pilot.mu.Lock()
	after := len(rig.pilot.bindings)
	rig.pilot.mu.Unlock()
	if before != after || counted.discoverCalls.Load() != 0 || counted.publishCalls.Load() != 0 {
		t.Fatalf("legacy fan crossed Host: bindings %d→%d, discover=%d publish=%d",
			before, after, counted.discoverCalls.Load(), counted.publishCalls.Load())
	}
}

type nha077CaptureDriver struct{ command integrations.AdmittedCommand }

func (d *nha077CaptureDriver) Dispatch(_ context.Context, command integrations.AdmittedCommand) error {
	d.command = command
	return nil
}

func TestNHA077LegacyCommandDriverRejectsRealAdmittedFan(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha077-legacy-fan-route")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("synthetic-fan", "speed")
	if err != nil {
		t.Fatal(err)
	}
	identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil || len(identities) != 1 {
		t.Fatalf("fan fixture identity: %v", err)
	}
	alias, err := domain.ParseHAAlias("fan.synthetic")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(identities[0].Device, identities[0].Entity,
		instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	description := &registry.CapabilityDescription{SchemaId: contracts.FanSchema,
		Major: 1, Endpoint: "speed", DisplayName: "Synthetic fan",
		Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{
			SpeedOptions: []string{"Off", "High"}}}}
	candidate, err := integrations.NewCandidate(key, []*registry.CapabilityDescription{description})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	driver := &nha077CaptureDriver{}
	if err := host.AttachCommandDriver(session, driver); err != nil {
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
	entity, err := identities[0].Entity.Value()
	if err != nil {
		t.Fatal(err)
	}
	speed := "High"
	request := &actions.CommandRequest{CommandId: "nha077-fan-legacy-route", AdmissionEpoch: epoch,
		ConfigRevision: 7, Target: &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entity}},
		CapabilitySchemaId: contracts.FanSchema, Endpoint: "speed",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: &speed}}},
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Dispatch(ctx, actor, request)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("real Host did not admit valid fan command: %v %v", response, err)
	}
	if _, _, _, err := driver.command.Identity(); err != nil || !driver.command.ReadyForTransport() {
		t.Fatalf("captured fan command is not live: %v", err)
	}
	p := newPilot(Config{})
	p.active, p.handshaken, p.streaming, p.hostMinor = true, true, true, 0
	p.ctx, p.lastHeartbeat, p.frameLimit = context.Background(), time.Now(), 65536
	p.capabilities = map[wire.Capability]bool{wire.Capability_CAPABILITY_COMMAND: true}
	p.entityHandles[entity] = make([]byte, 32)
	guardCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	if err := p.Dispatch(guardCtx, driver.command); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("legacy CommandDriver admitted fan: %v", err)
	}
	if len(p.queue) != 0 || len(p.pending) != 0 {
		t.Fatal("legacy fan command reached Host queue or waited for transport")
	}
}

func TestNHA077AcknowledgementRejectsExpiredAndOldSession(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "new_hold", true)
	waitMarker(t, rig.marker, "second_stream_rejected", 5*time.Second)
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	const id = "nha077-late-ack"
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := rig.service.Dispatch(context.Background(), actor,
			commandRequest(t, rig.fixture, rig.entities[0], id))
		done <- outcome{response, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var token string
	for {
		rig.pilot.mu.Lock()
		item := rig.pilot.pending[id]
		delivered := item != nil && item.delivered
		token = rig.pilot.token
		if delivered {
			item.expires = time.Now().Add(-time.Second)
		}
		rig.pilot.mu.Unlock()
		if delivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Host 1.1 command did not enter transport")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ack := connect.NewRequest(&wire.CompleteCommandRequest{
		CommandId: id, Kind: wire.CommandResultKind_COMMAND_RESULT_KIND_ACKNOWLEDGED})
	ack.Header().Set("Authorization", "Bearer "+token)
	if _, err := rig.pilot.CompleteCommand(context.Background(), ack); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expired ACK admitted: %v", err)
	}
	rig.pilot.mu.Lock()
	_, exists := rig.pilot.pending[id]
	rig.pilot.mu.Unlock()
	if !exists {
		t.Fatal("expired ACK removed a pending command")
	}
	rig.pilot.mu.Lock()
	rig.pilot.pending[id].expires = time.Now().Add(time.Minute)
	rig.pilot.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	if err := rig.supervisor.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, integrations.ErrCommandOutcomeIndeterminate) || got.response == nil ||
			got.response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
			t.Fatalf("revoked command outcome: %v %v", got.response, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked dispatch did not join")
	}
	if _, err := rig.pilot.CompleteCommand(context.Background(), ack); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("old session ACK admitted: %v", err)
	}
}

func TestNHA077LegacyProfileRejectsNewACKOnDeliveredCommand(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "hold", true)
	waitMarker(t, rig.marker, "second_stream_rejected", 5*time.Second)
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	const id = "nha077-legacy-pending"
	type outcome struct {
		response *actions.CommandResponse
		err      error
	}
	done := make(chan outcome, 1)
	request := commandRequest(t, rig.fixture, rig.entities[0], id)
	go func() {
		response, err := rig.service.Dispatch(context.Background(), actor, request)
		done <- outcome{response, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var token string
	for {
		rig.pilot.mu.Lock()
		item := rig.pilot.pending[id]
		delivered := item != nil && item.delivered
		token = rig.pilot.token
		rig.pilot.mu.Unlock()
		if delivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("legacy command did not reach real Commands stream")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ack := connect.NewRequest(&wire.CompleteCommandRequest{
		CommandId: id, Kind: wire.CommandResultKind_COMMAND_RESULT_KIND_ACKNOWLEDGED})
	ack.Header().Set("Authorization", "Bearer "+token)
	if _, err := rig.pilot.CompleteCommand(context.Background(), ack); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("Host 1.0 admitted 1.1 ACK on delivered command: %v", err)
	}
	rig.pilot.mu.Lock()
	_, pending := rig.pilot.pending[id]
	rig.pilot.mu.Unlock()
	if !pending {
		t.Fatal("rejected Host 1.1 ACK consumed legacy pending command")
	}
	commandID, err := domain.ParseCommandID(id)
	if err != nil {
		t.Fatal(err)
	}
	status, err := rig.host.ActionStatus(commandID)
	if err != nil || status.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("rejected ACK changed progress: %v %v", status, err)
	}
	accepted := connect.NewRequest(&wire.CompleteCommandRequest{
		CommandId: id, Kind: wire.CommandResultKind_COMMAND_RESULT_KIND_ACCEPTED})
	accepted.Header().Set("Authorization", "Bearer "+token)
	if _, err := rig.pilot.CompleteCommand(context.Background(), accepted); err != nil {
		t.Fatalf("legacy ACCEPTED regression: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.response == nil || got.response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
			t.Fatalf("legacy transport completion: %v %v", got.response, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("legacy dispatch did not join after ACCEPTED")
	}
	status, err = rig.host.ActionStatus(commandID)
	if err != nil || status.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("legacy ACCEPTED invented physical proof: %v %v", status, err)
	}
}
