package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/integrations/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
)

func nha178Description() *registry.CapabilityDescription {
	return &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1,
		DisplayName: "Synthetic fading light", Endpoint: "light",
		Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{
			SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100},
			TransitionSeconds: &registry.NumberBounds{Minimum: 0, Maximum: 6553, Step: nha174Pointer(1.0)},
		}}}
}

func TestNHA178HistoricalDescriptorsAndExactHost13(t *testing.T) {
	for _, profile := range []struct{ file, digest string }{
		{"host-v1.0.pb", schemaDigestV10Hex},
		{"host-v1.1.pb", schemaDigestV11Hex},
		{"host-v1.2.pb", schemaDigestV12Hex},
	} {
		data, err := os.ReadFile("../../testworker/fixtures/" + profile.file)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != profile.digest {
			t.Fatalf("historical Host %s changed", profile.file)
		}
	}
	for _, minor := range []uint32{0, 1, 2} {
		p, _, counted := nha174Profile(t, minor)
		request := nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{
			Source:       &wire.SourceKey{Device: "synthetic-thermostat", Entity: "climate"},
			Descriptions: []*registry.CapabilityDescription{nha178Description()},
		}}})
		if _, err := p.Discover(context.Background(), request); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("Host 1.%d admitted future transition declaration: %v", minor, err)
		}
		if counted.discoverCalls.Load() != 0 {
			t.Fatal("legacy declaration reached true Host")
		}
	}
	p, _, counted := nha174Profile(t, 3)
	request := nha174Request(&wire.DiscoverRequest{Candidates: []*wire.Candidate{{
		Source:       &wire.SourceKey{Device: "synthetic-thermostat", Entity: "climate"},
		Descriptions: []*registry.CapabilityDescription{nha178Description()},
	}}})
	response, err := p.Discover(context.Background(), request)
	if err != nil || len(response.Msg.Bindings) != 1 || counted.discoverCalls.Load() != 1 {
		t.Fatalf("Host 1.3 refused admitted light: %v", err)
	}
	for _, digest := range []string{schemaDigestV10Hex, schemaDigestV11Hex, schemaDigestV12Hex} {
		other := newPilot(Config{})
		other.active, other.token = true, "synthetic-nha174-token"
		bytes, _ := hex.DecodeString(digest)
		_, err := other.Handshake(context.Background(), nha174Request(&wire.HandshakeRequest{HostMajor: 1, HostMinor: 3, DriverMajor: 1, DriverMinor: 3, SchemaSha256: bytes, MaxFrameBytes: 65536, Capabilities: []wire.Capability{wire.Capability_CAPABILITY_DISCOVER}}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || other.handshaken {
			t.Fatalf("crossed historical digest admitted to 1.3: %v", err)
		}
	}
}
