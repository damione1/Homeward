package worker

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
)

func nha174Pointer[T any](value T) *T { return &value }

func nha174Request[T any](value *T) *connect.Request[T] {
	request := connect.NewRequest(value)
	request.Header().Set("Authorization", "Bearer synthetic-nha174-token")
	return request
}

func nha174Profile(t *testing.T) (*pilot, *fake.Fixture, *nha077CountingHost) {
	t.Helper()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha174-worker")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("synthetic-thermostat", "climate")
	if err != nil {
		t.Fatal(err)
	}
	identities, err := fixture.Resolve(context.Background(), instance, []integrations.SourceKey{key})
	if err != nil || len(identities) != 1 {
		t.Fatalf("resolve: %v", err)
	}
	alias, err := domain.ParseHAAlias("climate.synthetic_nha174")
	if err != nil {
		t.Fatal(err)
	}
	record, err := integrations.NewRegistryRecord(identities[0].Device, identities[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.PublishRegistry(context.Background(), []integrations.RegistryRecord{record}); err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	counted := &nha077CountingHost{SessionHost: session}
	p := newPilot(Config{})
	p.active, p.token, p.ctx, p.host = true, "synthetic-nha174-token", context.Background(), counted
	digest, err := hex.DecodeString(schemaDigestHex)
	if err != nil {
		t.Fatal(err)
	}
	request := nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: hostProtocolMinor, DriverMajor: 1, DriverMinor: hostProtocolMinor, SchemaSha256: digest, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER, wire.Capability_CAPABILITY_PUBLISH, wire.Capability_CAPABILITY_HEALTH}})
	response, err := p.Handshake(context.Background(), request)
	if err != nil || response.Msg.HostMinor != hostProtocolMinor || response.Msg.DriverMinor != hostProtocolMinor {
		t.Fatalf("profile handshake: %v", err)
	}
	return p, fixture, counted
}

func nha174WorkerDescription() *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "climate", DisplayName: "Synthetic climate",
		Constraints: &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{}}}
}

func nha174Discover(description *registry.CapabilityDescription) *connect.Request[wire.DiscoverRequest] {
	return nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{Source: &wire.SourceKey{Device: "synthetic-thermostat", Entity: "climate"}, Descriptions: []*registry.CapabilityDescription{description}}}})
}

// A rejected version cannot open any data or secret path, even with a valid
// process token and the current digest. No SessionHost is installed deliberately.
func nha174RejectedProfile(t *testing.T, minor uint32) *pilot {
	t.Helper()
	p := newPilot(Config{})
	p.active, p.token = true, "synthetic-nha174-token"
	digest, _ := hex.DecodeString(schemaDigestHex)
	response, err := p.Handshake(context.Background(), nha174Request(&wire.HandshakeRequest{
		HostMajor: 1, HostMinor: minor, DriverMajor: 1, DriverMinor: minor,
		SchemaSha256: digest, MaxFrameBytes: 65536,
		Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER, wire.Capability_CAPABILITY_PUBLISH, wire.Capability_CAPABILITY_SECRET_READ}}))
	if response != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || p.handshaken {
		t.Fatalf("old Host admitted: %v", err)
	}
	if _, err := p.Discover(context.Background(), nha174Discover(nha174WorkerDescription())); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Discover after refused handshake: %v", err)
	}
	if _, err := p.Publish(context.Background(), nha174Request(&wire.PublishRequest{})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Publish after refused handshake: %v", err)
	}
	if _, err := p.ReadSecret(context.Background(), nha174Request(&wire.ReadSecretRequest{Name: "synthetic-secret"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("ReadSecret after refused handshake: %v", err)
	}
	if len(p.bindings) != 0 {
		t.Fatal("refused handshake created bindings")
	}
	return p
}
func TestNHA174OldProfilesRefusedBeforeHostAndSecrets(t *testing.T) {
	for _, minor := range []uint32{0, 1, 2, 3, 5} {
		nha174RejectedProfile(t, minor)
	}
}

func TestNHA174CurrentProfilePublishesCoherentClimateThroughTrueHost(t *testing.T) {
	p, fixture, counted := nha174Profile(t)
	description := nha174WorkerDescription()
	constraints := description.GetClimate()
	constraints.ObservedTemperatureCelsius = &registry.ClimateTemperatureObservation{}
	constraints.ObservedSetpointCelsius = &registry.ClimateTemperatureObservation{}
	constraints.ObservedModeOptions = []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_HEAT}
	constraints.ActivityOptions = []registry.ClimateActivity{registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE}
	constraints.ObservesFanActivity = true
	response, err := p.Discover(context.Background(), nha174Discover(description))
	if err != nil || len(response.Msg.Bindings) != 1 {
		t.Fatalf("1.4 discovery: %v", err)
	}
	publish := nha174Request(&wire.PublishRequest{Observation: &wire.Observation{BindingHandle: response.Msg.Bindings[0].Handle, Endpoint: "climate", Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}, Value: &wire.Observation_Climate{Climate: &state.ClimateValue{CurrentTemperatureCelsius: nha174Pointer(0.0), Setpoint: &state.ClimateValue_SetpointCelsius{SetpointCelsius: 21}, Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_HEAT), Activity: nha174Pointer(registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE), FanActive: nha174Pointer(false)}}}})
	if _, err := p.Publish(context.Background(), publish); err != nil {
		t.Fatalf("1.4 publish: %v", err)
	}
	snapshot, err := fixture.Sequencer().Capture()
	if err != nil || len(snapshot.States) != 1 {
		t.Fatalf("1.4 projection: %v", err)
	}
	for _, observed := range snapshot.States {
		value, present := observed.CurrentValue()
		if !present {
			t.Fatal("1.4 current climate absent")
		}
		climate, ok := value.Climate()
		if !ok {
			t.Fatal("1.4 climate variant absent")
		}
		parts, err := climate.Components()
		if err != nil || parts.CurrentTemperature == nil || parts.Setpoint == nil || parts.Mode == nil || parts.Activity == nil || parts.FanActive == nil || *parts.FanActive {
			t.Fatal("1.4 climate lost fields")
		}
	}
	if counted.discoverCalls.Load() != 1 || counted.publishCalls.Load() != 1 {
		t.Fatal("1.4 failed shared Host composition")
	}
	for _, status := range []wire.HealthStatus{wire.HealthStatus_HEALTH_STATUS_READY, wire.HealthStatus_HEALTH_STATUS_DEGRADED} {
		if _, err := p.ReportHealth(context.Background(), nha174Request(&wire.ReportHealthRequest{Status: status})); err != nil {
			t.Fatal(err)
		}
		if p.ready != (status == wire.HealthStatus_HEALTH_STATUS_READY) {
			t.Fatal("1.4 lost readiness fence")
		}
	}
	if _, err := parseValue(&wire.Observation{Value: (*wire.Observation_Climate)(nil)}); err == nil {
		t.Fatal("typed-nil worker climate admitted")
	}
}

func TestNHA174CurrentProfileRejectsWrongDigest(t *testing.T) {
	p := newPilot(Config{})
	p.active, p.token = true, "synthetic-nha174-token"
	digest, _ := hex.DecodeString(schemaDigestHex)
	digest[0] ^= 1
	if _, err := p.Handshake(context.Background(), nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: hostProtocolMinor, DriverMajor: 1, DriverMinor: hostProtocolMinor, SchemaSha256: digest, MaxFrameBytes: 65536})); connect.CodeOf(err) != connect.CodeFailedPrecondition || p.handshaken {
		t.Fatalf("wrong digest admitted: %v", err)
	}
}

func TestNHA174CurrentWorkerRunsOverUnixTransport(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "hold", true)
	waitMarker(t, rig.marker, "second_stream_rejected", 5*time.Second)
	rig.pilot.mu.Lock()
	handshaken := rig.pilot.handshaken
	rig.pilot.mu.Unlock()
	if !handshaken {
		t.Fatal("current worker did not handshake")
	}
	snapshot, err := rig.fixture.Sequencer().Capture()
	if err != nil || len(snapshot.States) != 2 {
		t.Fatalf("current worker did not publish through true Host: %v", err)
	}
}
