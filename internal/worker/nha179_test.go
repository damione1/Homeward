package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"google.golang.org/protobuf/proto"
)

type nha179Generation struct{}

func (nha179Generation) WithCurrent(ctx context.Context, admit func(uint64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return admit(7)
}

func TestNHA179CasetaBindingOptInRequiresExactNegotiatedHost13(t *testing.T) {
	configured, err := NewConfiguredBinding("synthetic-bridge", "zone-1", "light.synthetic")
	if err != nil {
		t.Fatal(err)
	}
	qualified, err := configured.WithCasetaHA100V1()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qualified.WithCasetaHA100V1(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("second opt-in: %v", err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha179-synthetic-worker")
	if err != nil {
		t.Fatal(err)
	}
	config, err := (Config{instance: instance, executable: "/synthetic-worker"}).WithBindings([]ConfiguredBinding{qualified})
	if err != nil {
		t.Fatal(err)
	}
	copyBindings := config.Bindings()
	copyBindings[0] = configured
	if config.Bindings()[0].lightProfile != configuredLightProfileCasetaHA100V1 {
		t.Fatal("binding opt-in escaped by slice alias")
	}
	for _, fixture := range []struct {
		minor    uint32
		digest   string
		admitted bool
	}{
		{1, schemaDigestV11Hex, false}, {2, schemaDigestV12Hex, false}, {3, schemaDigestV13Hex, true},
	} {
		t.Run(fixture.digest[:8], func(t *testing.T) {
			p := newPilot(config)
			p.active, p.token = true, "synthetic-nha174-token"
			digest, err := hex.DecodeString(fixture.digest)
			if err != nil {
				t.Fatal(err)
			}
			request := nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: fixture.minor, DriverMajor: 1, DriverMinor: fixture.minor,
				SchemaSha256: digest, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER, wire.Capability_CAPABILITY_COMMAND}})
			response, err := p.Handshake(context.Background(), request)
			if fixture.admitted {
				if err != nil || response == nil || !p.handshaken || p.hostMinor != 3 {
					t.Fatalf("qualified 1.3 rejected: %v", err)
				}
			} else if response != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrCasetaHAProfileUnavailable) || p.handshaken {
				t.Fatalf("unqualified Host minor admitted: %v response=%v", err, response)
			}
		})
	}
}

func TestNHA179WorkerDiscoveryJoinsConfiguredSourceToCasetaProfile(t *testing.T) {
	ctx := context.Background()
	host, fixture, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha179-caseta-worker")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("synthetic-bridge", "zone-1")
	if err != nil {
		t.Fatal(err)
	}
	identities, err := fixture.Resolve(ctx, instance, []integrations.SourceKey{key})
	if err != nil {
		t.Fatal(err)
	}
	alias, err := domain.ParseHAAlias("light.synthetic")
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
	binding, err := NewConfiguredBinding("synthetic-bridge", "zone-1", "light.synthetic")
	if err != nil {
		t.Fatal(err)
	}
	binding, err = binding.WithCasetaHA100V1()
	if err != nil {
		t.Fatal(err)
	}
	p := newPilot(Config{instance: instance, executable: "/synthetic-worker", bindings: []ConfiguredBinding{binding}})
	p.active, p.token, p.ctx, p.host = true, "synthetic-nha174-token", ctx, session
	digest, err := hex.DecodeString(schemaDigestV13Hex)
	if err != nil {
		t.Fatal(err)
	}
	handshake := nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: 3, DriverMajor: 1, DriverMinor: 3, SchemaSha256: digest, MaxFrameBytes: 65536,
		Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER, wire.Capability_CAPABILITY_COMMAND}})
	if _, err := p.Handshake(ctx, handshake); err != nil {
		t.Fatal(err)
	}
	step := 1.0
	description := &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, DisplayName: "Synthetic dimmer", Endpoint: "level",
		Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true,
			BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100},
			TransitionSeconds: &registry.NumberBounds{Minimum: 0, Maximum: 6553, Step: &step}}}}
	incompatible := proto.Clone(description).(*registry.CapabilityDescription)
	incompatible.Endpoint = "unqualified-level"
	invalid := nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{Source: &wire.SourceKey{Device: "synthetic-bridge", Entity: "zone-1"},
		Descriptions: []*registry.CapabilityDescription{incompatible}}}})
	if response, err := p.Discover(ctx, invalid); response != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrCasetaHAProfileUnavailable) {
		t.Fatalf("configured Caseta source silently fell back after incompatible descriptor: %v %v", response, err)
	}
	request := nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{Source: &wire.SourceKey{Device: "synthetic-bridge", Entity: "zone-1"},
		Descriptions: []*registry.CapabilityDescription{description}}}})
	if _, err := p.Discover(ctx, request); err != nil {
		t.Fatalf("qualified discovery: %v", err)
	}
	if err := host.AttachCommandDriver(session, p); err != nil {
		t.Fatal(err)
	}
	level := 25.0
	light, err := domain.NewLightValue(true, &level, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewLightStateValue(light)
	if err != nil {
		t.Fatal(err)
	}
	quality, err := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	observation, err := integrations.NewStateObservation("level", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved := p.bindings
	if len(resolved) != 1 {
		t.Fatal("worker did not keep source binding")
	}
	for _, admitted := range resolved {
		if err := session.Publish(ctx, admitted, observation); err != nil {
			t.Fatal(err)
		}
	}
	router, err := integrations.NewCommandRouter(host, nha179Generation{})
	if err != nil {
		t.Fatal(err)
	}
	capture, err := router.CaptureLight(ctx, alias)
	if err != nil {
		t.Fatalf("configured Host 1.3 did not attach Caseta profile: %v", err)
	}
	b, _, err := capture.ObservedHABrightness()
	if err != nil || b != 63 {
		t.Fatalf("HA Caseta observed level 25%% = %d, %v", b, err)
	}
}
