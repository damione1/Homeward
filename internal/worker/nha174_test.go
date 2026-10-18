package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
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

func nha174Profile(t *testing.T, minor uint32) (*pilot, *fake.Fixture, *nha077CountingHost) {
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
	var expected string
	switch minor {
	case 0:
		expected = schemaDigestV10Hex
	case 1:
		expected = schemaDigestV11Hex
	case 2:
		expected = schemaDigestV12Hex
	case 3:
		expected = schemaDigestV13Hex
	default:
		t.Fatal("invalid test profile")
	}
	digest, err := hex.DecodeString(expected)
	if err != nil {
		t.Fatal(err)
	}
	request := nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: minor, DriverMajor: 1, DriverMinor: minor, SchemaSha256: digest, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER, wire.Capability_CAPABILITY_PUBLISH, wire.Capability_CAPABILITY_HEALTH}})
	response, err := p.Handshake(context.Background(), request)
	if err != nil || response.Msg.HostMinor != minor || response.Msg.DriverMinor != minor {
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

func TestNHA174LegacyProfilesRejectNewClimateBeforeHost(t *testing.T) {
	for _, minor := range []uint32{0, 1} {
		p, _, counted := nha174Profile(t, minor)
		for name, set := range map[string]func(*registry.ClimateConstraints){
			"temperature-wrapper-unknown-bounds": func(c *registry.ClimateConstraints) {
				c.ObservedTemperatureCelsius = &registry.ClimateTemperatureObservation{}
			},
			"setpoint-wrapper-unknown-bounds": func(c *registry.ClimateConstraints) {
				c.ObservedSetpointCelsius = &registry.ClimateTemperatureObservation{}
			},
			"range-unknown-bounds": func(c *registry.ClimateConstraints) {
				c.ObservedSetpointRangeCelsius = &registry.ClimateSetpointRangeConstraints{}
			},
			"observed-mode": func(c *registry.ClimateConstraints) {
				c.ObservedModeOptions = []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_HEAT}
			},
			"observes-fan-true": func(c *registry.ClimateConstraints) { c.ObservesFanActivity = true },
		} {
			t.Run(name+"-profile"+string(rune('0'+minor)), func(t *testing.T) {
				description := nha174WorkerDescription()
				set(description.GetClimate())
				if _, err := p.Discover(context.Background(), nha174Discover(description)); connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("new declaration crossed old profile: %v", err)
				}
			})
		}
		publish := nha174Request(&wire.PublishRequest{Observation: &wire.Observation{BindingHandle: make([]byte, 32), Endpoint: "climate", Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}, Value: &wire.Observation_Climate{Climate: &state.ClimateValue{FanActive: nha174Pointer(false)}}}})
		if _, err := p.Publish(context.Background(), publish); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("present fan false climate passed profile %d: %v", minor, err)
		}
		if counted.discoverCalls.Load() != 0 || counted.publishCalls.Load() != 0 || len(p.bindings) != 0 {
			t.Fatal("new climate reached Host through legacy profile")
		}
		// The declaration flag is non-optional proto3: false has no feature or
		// distinguishable presence. An otherwise old empty declaration is valid.
		if _, err := p.Discover(context.Background(), nha174Discover(nha174WorkerDescription())); err != nil {
			t.Fatalf("old empty climate description rejected: %v", err)
		}
		if counted.discoverCalls.Load() != 1 {
			t.Fatal("old declaration did not reach Host")
		}
	}
}

func TestNHA174Profile12PublishesCoherentClimateThroughTrueHost(t *testing.T) {
	p, fixture, counted := nha174Profile(t, 2)
	description := nha174WorkerDescription()
	constraints := description.GetClimate()
	constraints.ObservedTemperatureCelsius = &registry.ClimateTemperatureObservation{}
	constraints.ObservedSetpointCelsius = &registry.ClimateTemperatureObservation{}
	constraints.ObservedModeOptions = []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_HEAT}
	constraints.ActivityOptions = []registry.ClimateActivity{registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE}
	constraints.ObservesFanActivity = true
	response, err := p.Discover(context.Background(), nha174Discover(description))
	if err != nil || len(response.Msg.Bindings) != 1 {
		t.Fatalf("1.2 discovery: %v", err)
	}
	publish := nha174Request(&wire.PublishRequest{Observation: &wire.Observation{BindingHandle: response.Msg.Bindings[0].Handle, Endpoint: "climate", Quality: &state.Quality{Availability: state.Availability_AVAILABILITY_AVAILABLE, Freshness: state.Freshness_FRESHNESS_FRESH, Provenance: state.Provenance_PROVENANCE_OBSERVED}, Value: &wire.Observation_Climate{Climate: &state.ClimateValue{CurrentTemperatureCelsius: nha174Pointer(0.0), Setpoint: &state.ClimateValue_SetpointCelsius{SetpointCelsius: 21}, Mode: nha174Pointer(registry.ClimateMode_CLIMATE_MODE_HEAT), Activity: nha174Pointer(registry.ClimateActivity_CLIMATE_ACTIVITY_IDLE), FanActive: nha174Pointer(false)}}}})
	if _, err := p.Publish(context.Background(), publish); err != nil {
		t.Fatalf("1.2 publish: %v", err)
	}
	snapshot, err := fixture.Sequencer().Capture()
	if err != nil || len(snapshot.States) != 1 {
		t.Fatalf("1.2 projection: %v", err)
	}
	for _, observed := range snapshot.States {
		value, present := observed.CurrentValue()
		if !present {
			t.Fatal("1.2 current climate absent")
		}
		climate, ok := value.Climate()
		if !ok {
			t.Fatal("1.2 climate variant absent")
		}
		parts, err := climate.Components()
		if err != nil || parts.CurrentTemperature == nil || parts.Setpoint == nil || parts.Mode == nil || parts.Activity == nil || parts.FanActive == nil || *parts.FanActive {
			t.Fatal("1.2 climate lost fields")
		}
	}
	if counted.discoverCalls.Load() != 1 || counted.publishCalls.Load() != 1 {
		t.Fatal("1.2 failed shared Host composition")
	}
	for _, status := range []wire.HealthStatus{wire.HealthStatus_HEALTH_STATUS_READY, wire.HealthStatus_HEALTH_STATUS_DEGRADED} {
		if _, err := p.ReportHealth(context.Background(), nha174Request(&wire.ReportHealthRequest{Status: status})); err != nil {
			t.Fatal(err)
		}
		if p.ready != (status == wire.HealthStatus_HEALTH_STATUS_READY) {
			t.Fatal("1.2 lost 1.1 readiness fence")
		}
	}
	if _, err := parseValue(&wire.Observation{Value: (*wire.Observation_Climate)(nil)}); err == nil {
		t.Fatal("typed-nil worker climate admitted")
	}
}

func TestNHA174ExactProfilesKeepHistoricalDigestsAndRejectCrossedVersions(t *testing.T) {
	for _, profile := range []struct{ file, digest string }{{"host-v1.0.pb", schemaDigestV10Hex}, {"host-v1.1.pb", schemaDigestV11Hex}} {
		bytes, err := os.ReadFile("../../testworker/fixtures/" + profile.file)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(bytes)
		if hex.EncodeToString(digest[:]) != profile.digest {
			t.Fatal("historical descriptor changed")
		}
	}
	for _, test := range []struct {
		host, driver uint32
		digest       string
	}{{1, 2, schemaDigestV12Hex}, {2, 1, schemaDigestV11Hex}, {1, 1, schemaDigestV12Hex}, {2, 2, schemaDigestV11Hex}, {3, 3, schemaDigestV12Hex}} {
		p := newPilot(Config{})
		p.active, p.token = true, "synthetic-nha174-token"
		digest, err := hex.DecodeString(test.digest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Handshake(context.Background(), nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: test.host, DriverMajor: 1, DriverMinor: test.driver, SchemaSha256: digest, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_PUBLISH}})); connect.CodeOf(err) != connect.CodeFailedPrecondition || p.handshaken {
			t.Fatalf("crossed/future profile admitted: %v", err)
		}
	}
}

func TestNHA174UnchangedProfile11WorkerStillRunsOverUnixTransport(t *testing.T) {
	rig := startWorkerRigWithCapture(t, "new_hold", true)
	waitMarker(t, rig.marker, "second_stream_rejected", 5*time.Second)
	rig.pilot.mu.Lock()
	minor := rig.pilot.hostMinor
	rig.pilot.mu.Unlock()
	if minor != 1 {
		t.Fatal("old worker was silently promoted to a new profile")
	}
	snapshot, err := rig.fixture.Sequencer().Capture()
	if err != nil || len(snapshot.States) != 2 {
		t.Fatalf("old worker did not publish through true Host: %v", err)
	}
}
