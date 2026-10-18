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

func nha046FanDescription() *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: contracts.FanSchema, Major: 1, Endpoint: "speed", DisplayName: "Synthetic Caseta fan",
		Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{
			SetOn: false, SpeedOptions: []string{"Off", "Low", "Medium", "MediumHigh", "High"}}}}
}

func TestNHA046CasetaFanBindingRequiresExactNegotiatedHost(t *testing.T) {
	binding, err := NewConfiguredBinding("synthetic-bridge", "fan-1", "fan.synthetic")
	if err != nil {
		t.Fatal(err)
	}
	qualified, err := binding.WithCasetaHAFan4V1()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := qualified.WithCasetaHAFan4V1(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("double opt-in: %v", err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("nha046-fan-worker")
	config, err := (Config{instance: instance, executable: "/synthetic-worker"}).WithBindings([]ConfiguredBinding{qualified})
	if err != nil {
		t.Fatal(err)
	}
	copyBindings := config.Bindings()
	copyBindings[0] = binding
	if !config.Bindings()[0].CasetaHAFan4V1() {
		t.Fatal("opt-in escaped through binding copy")
	}
	for _, fixture := range []struct {
		minor    uint32
		digest   string
		admitted bool
	}{
		{0, schemaDigestV10Hex, false}, {1, schemaDigestV11Hex, true},
		{2, schemaDigestV12Hex, false}, {3, schemaDigestV13Hex, true},
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
				if err != nil || response == nil || !p.handshaken || p.hostMinor != fixture.minor {
					t.Fatalf("qualified Host refused: %v", err)
				}
			} else if response != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrCasetaHAFanProfileUnavailable) || p.handshaken {
				t.Fatalf("unqualified Host admitted: %v response=%v", err, response)
			}
		})
	}
}

func TestNHA046WorkerDiscoveryJoinsOnlyConfiguredFanSource(t *testing.T) {
	for _, fixture := range []struct {
		minor  uint32
		digest string
	}{{1, schemaDigestV11Hex}, {3, schemaDigestV13Hex}} {
		t.Run(fixture.digest[:8], func(t *testing.T) {
			ctx := context.Background()
			host, projection, err := fake.NewCommandFixture()
			if err != nil {
				t.Fatal(err)
			}
			instance, _ := domain.ParseIntegrationInstanceID("nha046-fan-worker")
			key, _ := integrations.NewSourceKey("synthetic-bridge", "fan-1")
			identities, err := projection.Resolve(ctx, instance, []integrations.SourceKey{key})
			if err != nil {
				t.Fatal(err)
			}
			alias, _ := domain.ParseHAAlias("fan.synthetic")
			record, err := integrations.NewRegistryRecord(identities[0].Device, identities[0].Entity, instance, key, alias, alias, []domain.HAAlias{alias}, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := projection.PublishRegistry(ctx, []integrations.RegistryRecord{record}); err != nil {
				t.Fatal(err)
			}
			session, err := host.OpenSession(instance)
			if err != nil {
				t.Fatal(err)
			}
			binding, _ := NewConfiguredBinding("synthetic-bridge", "fan-1", "fan.synthetic")
			binding, err = binding.WithCasetaHAFan4V1()
			if err != nil {
				t.Fatal(err)
			}
			p := newPilot(Config{instance: instance, executable: "/synthetic-worker", bindings: []ConfiguredBinding{binding}})
			p.active, p.token, p.ctx, p.host = true, "synthetic-nha174-token", ctx, session
			digest, _ := hex.DecodeString(fixture.digest)
			if _, err := p.Handshake(ctx, nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: fixture.minor, DriverMajor: 1, DriverMinor: fixture.minor,
				SchemaSha256: digest, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER, wire.Capability_CAPABILITY_COMMAND}})); err != nil {
				t.Fatal(err)
			}
			bad := proto.Clone(nha046FanDescription()).(*registry.CapabilityDescription)
			bad.GetFan().SpeedOptions = []string{"Off", "Low", "Medium", "High"}
			badRequest := nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{Source: &wire.SourceKey{Device: "synthetic-bridge", Entity: "fan-1"}, Descriptions: []*registry.CapabilityDescription{bad}}}})
			if response, err := p.Discover(ctx, badRequest); response != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !errors.Is(err, ErrCasetaHAFanProfileUnavailable) {
				t.Fatalf("incompatible descriptor fell back: %v response=%v", err, response)
			}
			request := nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{Source: &wire.SourceKey{Device: "synthetic-bridge", Entity: "fan-1"}, Descriptions: []*registry.CapabilityDescription{nha046FanDescription()}}}})
			if _, err := p.Discover(ctx, request); err != nil {
				t.Fatal(err)
			}
			if err := host.AttachCommandDriver(session, p); err != nil {
				t.Fatal(err)
			}
			quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
			value, _ := domain.NewFanStateValue(domain.FanSpeedLow)
			at := time.Now()
			observation, err := integrations.NewStateObservation("speed", quality, &value, &at, at, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, admitted := range p.bindings {
				if err := session.Publish(ctx, admitted, observation); err != nil {
					t.Fatal(err)
				}
			}
			router, err := integrations.NewCommandRouter(host, nha179Generation{})
			if err != nil {
				t.Fatal(err)
			}
			capture, err := router.CaptureFan(ctx, alias)
			if err != nil {
				t.Fatalf("configured fan not qualified: %v", err)
			}
			pct := uint8(75)
			id, _ := domain.ParseCommandID("nha046-worker-fan75")
			actor, _ := domain.ParseActorID("nha046-actor")
			prepared, err := capture.PrepareHA(actor, id, at.Add(time.Minute), true, &pct)
			if err != nil {
				t.Fatal(err)
			}
			wireRequest, err := prepared.Request()
			if err != nil || wireRequest.GetArguments().GetSetFan().GetSpeed() != "MediumHigh" || wireRequest.GetArguments().GetSetFan().On != nil {
				t.Fatalf("fan profile did not preserve HA75 as speed only: %v %v", wireRequest, err)
			}
			otherKey, _ := integrations.NewSourceKey("synthetic-bridge", "fan-unconfigured")
			otherIDs, err := projection.Resolve(ctx, instance, []integrations.SourceKey{otherKey})
			if err != nil {
				t.Fatal(err)
			}
			otherAlias, _ := domain.ParseHAAlias("fan.unconfigured")
			otherRecord, err := integrations.NewRegistryRecord(otherIDs[0].Device, otherIDs[0].Entity, instance, otherKey, otherAlias, otherAlias, []domain.HAAlias{otherAlias}, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := projection.PublishRegistry(ctx, []integrations.RegistryRecord{otherRecord}); err != nil {
				t.Fatal(err)
			}
			otherRequest := nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{Source: &wire.SourceKey{Device: "synthetic-bridge", Entity: "fan-unconfigured"}, Descriptions: []*registry.CapabilityDescription{nha046FanDescription()}}}})
			if _, err := p.Discover(ctx, otherRequest); err != nil {
				t.Fatal(err)
			}
			if _, err := router.CaptureFan(ctx, otherAlias); !errors.Is(err, integrations.ErrFanPreparationUnsupported) {
				t.Fatalf("opt-in leaked to another SourceKey: %v", err)
			}
		})
	}
}
