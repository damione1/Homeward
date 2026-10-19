package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	helperwire "github.com/damione1/Homeward/gen/home/helpers/v1"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestNHA097AllHistoricalWorkersRejectHelperDescriptionsBeforeHost(t *testing.T) {
	for minor := uint32(0); minor <= 3; minor++ {
		p, _, counted := nha174Profile(t, minor)
		for _, description := range []*registry.CapabilityDescription{
			{SchemaId: contracts.HelperSchema, Major: 1, Endpoint: "value", DisplayName: "synthetic helper",
				Constraints: &registry.CapabilityDescription_Helper{Helper: &helperwire.HelperConstraints{Kind: helperwire.HelperKind_HELPER_KIND_BOOLEAN, Operations: []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET}}}},
			{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "value", DisplayName: "mismatched helper branch",
				Constraints: &registry.CapabilityDescription_Helper{Helper: &helperwire.HelperConstraints{Kind: helperwire.HelperKind_HELPER_KIND_BOOLEAN}}},
		} {
			response, err := p.Discover(context.Background(), nha174Discover(description))
			if response != nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("Host 1.%d admitted helper description: %v", minor, err)
			}
		}
		if counted.discoverCalls.Load() != 0 || len(p.bindings) != 0 {
			t.Fatalf("Host 1.%d helper reached Host", minor)
		}
	}
}

func TestNHA097Host13DigestRemainsHistorical(t *testing.T) {
	const expected = "14291d23f9102ecb910bea14dc4cb38d5ba8137cec073bb5402e3ffbe2306bf3"
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testworker", "fixtures", "host-v1.3.pb"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := sha256.Sum256(fixture)
	if schemaDigestV13Hex != expected || hex.EncodeToString(legacy[:]) != expected {
		t.Fatal("Host 1.3 digest drifted from the delivered profile")
	}
	descriptor := filepath.Join(t.TempDir(), "current-host.pb")
	command := exec.Command("buf", "build", "--path", "proto/home/integrations/v1/host.proto", "--exclude-source-info", "-o", descriptor)
	command.Dir = filepath.Join("..", "..")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("current descriptor build: %v: %s", err, output)
	}
	current, err := os.ReadFile(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	currentDigest := sha256.Sum256(current)
	if currentDigest == legacy {
		t.Fatal("test did not exercise a transitively extended Host descriptor")
	}
	for _, digest := range [][]byte{currentDigest[:], make([]byte, 32)} {
		p := newPilot(Config{})
		p.active, p.token = true, "synthetic-nha174-token"
		request := nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: 3, DriverMajor: 1, DriverMinor: 3,
			SchemaSha256: digest, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER}})
		if response, err := p.Handshake(context.Background(), request); response != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || p.handshaken {
			t.Fatalf("nonhistorical digest admitted: %v", err)
		}
	}
	p, _, _ := nha174Profile(t, 3)
	if !p.handshaken {
		t.Fatal("historical 1.3 profile no longer handshakes")
	}
}

func TestNHA097HistoricalWorkerRejectsAdmittedHelperBeforeQueue(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha097-worker-boundary")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("synthetic-helper", "value")
	if err != nil {
		t.Fatal(err)
	}
	identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil || len(identities) != 1 {
		t.Fatalf("resolve: %v", err)
	}
	alias, err := domain.ParseHAAlias("input_boolean.away_mode")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(identities[0].Device, identities[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
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
	description := &registry.CapabilityDescription{SchemaId: contracts.HelperSchema, Major: 1, DisplayName: "synthetic helper", Endpoint: "value",
		Constraints: &registry.CapabilityDescription_Helper{Helper: &helperwire.HelperConstraints{Kind: helperwire.HelperKind_HELPER_KIND_BOOLEAN,
			Operations: []helperwire.HelperOperation{helperwire.HelperOperation_HELPER_OPERATION_SET}}}}
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
	zero := uint64(0)
	request := &actions.CommandRequest{CommandId: "nha097-helper-worker-guard", AdmissionEpoch: epoch, ConfigRevision: 7,
		Target:             &registry.EntityRef{Reference: &registry.EntityRef_EntityId{EntityId: entity}},
		CapabilitySchemaId: contracts.HelperSchema, Endpoint: "value",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_Helper{Helper: &helperwire.HelperAction{
			Operation: helperwire.HelperOperation_HELPER_OPERATION_SET,
			Value:     &helperwire.HelperValue{Value: &helperwire.HelperValue_Boolean{Boolean: false}}, ExpectedHelperRevision: &zero}}},
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	actor, err := domain.ParseActorID("trusted-actor")
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Dispatch(ctx, actor, request)
	if err != nil || response == nil || response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatalf("native Host failed to admit helper command: %v %v", response, err)
	}
	p := newPilot(Config{})
	p.active, p.handshaken, p.streaming, p.ready, p.hostMinor = true, true, true, true, 3
	p.ctx, p.lastHeartbeat, p.frameLimit = ctx, time.Now(), 65536
	p.capabilities = map[wire.Capability]bool{wire.Capability_CAPABILITY_COMMAND: true}
	p.entityHandles[entity] = make([]byte, 32)
	if err := p.Dispatch(ctx, driver.command); err != ErrUnavailable {
		t.Fatalf("helper crossed worker command boundary: %v", err)
	}
	if len(p.queue) != 0 || len(p.pending) != 0 || len(p.correlations) != 0 {
		t.Fatal("helper command reached worker delivery queue")
	}
}
