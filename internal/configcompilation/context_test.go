package configcompilation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func nha055ContextIdentity(t *testing.T, name string) Identity {
	t.Helper()
	device, err := domain.ParseDeviceID("dev_" + name)
	if err != nil {
		t.Fatal(err)
	}
	entity, err := domain.ParseEntityID("ent_" + name)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("fixture")
	if err != nil {
		t.Fatal(err)
	}
	original, err := domain.ParseHAAlias("switch." + name)
	if err != nil {
		t.Fatal(err)
	}
	current, err := domain.ParseHAAlias("switch.current_" + name)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{Device: device, Entity: entity, Instance: instance, SourceDevice: "protocol-" + name, SourceEntity: "relay", Original: original, Current: current, Aliases: []domain.HAAlias{current, original}}
}

func nha055ContextSwitch(endpoint string) *wireregistry.CapabilityDescription {
	return &wireregistry.CapabilityDescription{SchemaId: "core.switch.v1", Major: 1, DisplayName: "Synthetic switch", Endpoint: endpoint, Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}}
}

func nha055ContextFixture(t *testing.T) (Context, []Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
	t.Helper()
	identities := []Identity{nha055ContextIdentity(t, "a"), nha055ContextIdentity(t, "b"), nha055ContextIdentity(t, "c")}
	capabilities := map[domain.EntityID][]*wireregistry.CapabilityDescription{
		identities[0].Entity: {nha055ContextSwitch("two"), nha055ContextSwitch("one")},
		identities[1].Entity: {},
	}
	context, err := NewContext("v1", sha256.Sum256([]byte("fixture manifest")), identities, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	return context, identities, capabilities
}

func TestNHA055ContextRoundTripAndAbsenceAreExplicit(t *testing.T) {
	context, identities, _ := nha055ContextFixture(t)
	decoded, err := ParseContext(context.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Valid() || !bytes.Equal(decoded.Bytes(), context.Bytes()) || decoded.SHA256() != context.SHA256() || decoded.SHA256() != sha256.Sum256(decoded.Bytes()) || decoded.ManifestRevision() != "v1" || decoded.ManifestSHA256() != context.ManifestSHA256() || len(decoded.Identities()) != 3 {
		t.Fatal("canonical persisted input changed across round trip")
	}
	capabilities := decoded.Capabilities()
	if known, found := capabilities[identities[1].Entity]; !found || known == nil || len(known) != 0 {
		t.Fatal("declared empty capability set became absent")
	}
	if _, found := capabilities[identities[2].Entity]; found {
		t.Fatal("absent discovery became a declaration")
	}
	if len(capabilities[identities[0].Entity]) != 2 || capabilities[identities[0].Entity][0].Endpoint != "one" {
		t.Fatal("capability order is not canonical")
	}
	if (Context{}).Valid() || (Context{}).Bytes() != nil || (Context{}).Identities() != nil || (Context{}).Capabilities() != nil {
		t.Fatal("zero Context appears admitted")
	}
	empty, err := NewContext("v1", context.ManifestSHA256(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseContext(empty.Bytes()); err != nil {
		t.Fatalf("explicit empty snapshot cannot round trip: %v", err)
	}
}

func TestNHA055ContextCanonicalDigestIgnoresIndexOrderAndDetectsChange(t *testing.T) {
	first, identities, capabilities := nha055ContextFixture(t)
	for i := range identities {
		identities[i].Aliases[0], identities[i].Aliases[1] = identities[i].Aliases[1], identities[i].Aliases[0]
	}
	identities[0], identities[2] = identities[2], identities[0]
	for _, descriptions := range capabilities {
		if len(descriptions) == 2 {
			descriptions[0], descriptions[1] = descriptions[1], descriptions[0]
		}
	}
	reordered, err := NewContext("v1", first.ManifestSHA256(), identities, capabilities)
	if err != nil || first.SHA256() != reordered.SHA256() || !bytes.Equal(first.Bytes(), reordered.Bytes()) {
		t.Fatalf("index order changed persisted meaning: %v", err)
	}
	// JSON field order and insignificant whitespace also normalize to one digest.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(first.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	spaced, err := json.MarshalIndent(fields, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseContext(spaced)
	if err != nil || parsed.SHA256() != first.SHA256() {
		t.Fatalf("JSON ordering changed the digest: %v", err)
	}
	identities[0].SourceEntity = "different-source"
	changed, err := NewContext("v1", first.ManifestSHA256(), identities, capabilities)
	if err != nil || changed.SHA256() == first.SHA256() {
		t.Fatalf("source binding change was invisible to the digest: %v", err)
	}
}

func TestNHA055ContextOwnsInputsAndAllGetterCopies(t *testing.T) {
	context, identities, capabilities := nha055ContextFixture(t)
	before := context.Bytes()
	identities[0].Aliases[0] = domain.HAAlias{}
	capabilities[identities[0].Entity][0].GetSwitch().SetOn = false
	delete(capabilities, identities[1].Entity)
	returnedIdentities := context.Identities()
	returnedIdentities[0].Aliases[0] = domain.HAAlias{}
	returnedCapabilities := context.Capabilities()
	for entity, descriptions := range returnedCapabilities {
		if len(descriptions) > 0 {
			descriptions[0].Endpoint = "mutated"
			descriptions[0].GetSwitch().SetOn = false
		}
		delete(returnedCapabilities, entity)
	}
	returnedBytes := context.Bytes()
	returnedBytes[0] = '!'
	if !bytes.Equal(before, context.Bytes()) || context.SHA256() != sha256.Sum256(before) {
		t.Fatal("caller mutated immutable context")
	}
	decoded, err := ParseContext(context.Bytes())
	if err != nil || !decoded.Valid() {
		t.Fatalf("caller mutation corrupted stored bytes: %v", err)
	}
	for _, descriptions := range context.Capabilities() {
		for _, description := range descriptions {
			if !description.GetSwitch().SetOn || description.Endpoint == "mutated" {
				t.Fatal("protobuf getter returned a shared object")
			}
		}
	}
}

func TestNHA055ContextRejectsAmbiguousIdentityAndCapabilityBindings(t *testing.T) {
	context, identities, capabilities := nha055ContextFixture(t)
	for _, scenario := range []struct {
		name   string
		change func([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription)
	}{
		{"duplicate entity", func(ids []Identity, caps map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
			return append(ids, ids[0]), caps
		}},
		{"duplicate alias", func(ids []Identity, caps map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
			ids[1].Aliases = append(ids[1].Aliases, ids[0].Aliases[0])
			return ids, caps
		}},
		{"missing current", func(ids []Identity, caps map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
			ids[0].Aliases = []domain.HAAlias{ids[0].Original}
			return ids, caps
		}},
		{"duplicate source key", func(ids []Identity, caps map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
			ids[1].SourceDevice = ids[0].SourceDevice
			ids[1].SourceEntity = ids[0].SourceEntity
			ids[1].Device = ids[0].Device
			return ids, caps
		}},
		{"source device identity conflict", func(ids []Identity, caps map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
			ids[1].SourceDevice = ids[0].SourceDevice
			ids[1].SourceEntity = "other-relay"
			return ids, caps
		}},
		{"retired capability", func(ids []Identity, caps map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
			ids[0].Retired = true
			return ids, caps
		}},
		{"unknown entity capability", func(ids []Identity, caps map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
			caps[domain.EntityID{}] = nil
			return ids, caps
		}},
		{"duplicate endpoint", func(ids []Identity, caps map[domain.EntityID][]*wireregistry.CapabilityDescription) ([]Identity, map[domain.EntityID][]*wireregistry.CapabilityDescription) {
			caps[ids[0].Entity] = []*wireregistry.CapabilityDescription{nha055ContextSwitch("same"), nha055ContextSwitch("same")}
			return ids, caps
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			copyIDs := context.Identities()
			copyCaps := context.Capabilities()
			copyIDs, copyCaps = scenario.change(copyIDs, copyCaps)
			if _, err := NewContext("v1", context.ManifestSHA256(), copyIDs, copyCaps); !errors.Is(err, ErrInvalidContext) {
				t.Fatalf("ambiguous snapshot admitted: %v", err)
			}
		})
	}
	if _, err := NewContext("future_revision", context.ManifestSHA256(), identities, capabilities); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("unknown revision admitted: %v", err)
	}
	if _, err := NewContext("v1", [32]byte{}, identities, capabilities); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("absent manifest digest admitted: %v", err)
	}
}

func TestNHA055ContextStrictWireRejectsUnknownDuplicateMissingAndOversizedInput(t *testing.T) {
	context, _, _ := nha055ContextFixture(t)
	raw := string(context.Bytes())
	for name, invalid := range map[string]string{
		"unknown root":       strings.Replace(raw, "{", "{\"observed_state\":{},", 1),
		"duplicate root":     strings.Replace(raw, "{", "{\"version\":1,", 1),
		"wrong case":         strings.Replace(raw, "\"version\"", "\"Version\"", 1),
		"duplicate nested":   strings.Replace(raw, "\"device_id\":", "\"device_id\":\"other\",\"device_id\":", 1),
		"unknown nested":     strings.Replace(raw, "\"device_id\":", "\"state\":{},\"device_id\":", 1),
		"missing retirement": strings.Replace(raw, "\"retired\":false", "\"retired\":null", 1),
		"missing identities": strings.Replace(raw, "\"identities\":[", "\"identities\":null,\"discarded\":[", 1),
		"future format":      strings.Replace(raw, "\"version\":1", "\"version\":2", 1),
		"trailing document":  raw + "{}",
		"oversized":          strings.Repeat(" ", MaxContextBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseContext([]byte(invalid)); !errors.Is(err, ErrInvalidContext) {
				t.Fatalf("untrusted wire admitted: %v", err)
			}
		})
	}
}

func TestNHA055ContextProtobufUnknownsEnumsAndSizeStayClosed(t *testing.T) {
	context, identities, _ := nha055ContextFixture(t)
	outerUnknown := nha055ContextSwitch("relay")
	outerUnknown.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	nestedUnknown := nha055ContextSwitch("relay")
	nestedUnknown.GetSwitch().ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	unknownEnum := &wireregistry.CapabilityDescription{SchemaId: "core.climate.v1", Major: 1, DisplayName: "Synthetic", Endpoint: "climate", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{ModeOptions: []wireregistry.ClimateMode{99}}}}
	nonfinite := proto.Clone(unknownEnum).(*wireregistry.CapabilityDescription)
	nonfinite.GetClimate().ModeOptions = nil
	nonfinite.GetClimate().SetpointCelsius = &wireregistry.NumberBounds{Minimum: math.NaN(), Maximum: 30}
	oversized := nha055ContextSwitch("relay")
	oversized.DescriptorSet = make([]byte, MaxCapabilityBytes+1)
	for _, description := range []*wireregistry.CapabilityDescription{nil, outerUnknown, nestedUnknown, unknownEnum, nonfinite, oversized} {
		if _, err := NewContext("v1", context.ManifestSHA256(), identities, map[domain.EntityID][]*wireregistry.CapabilityDescription{identities[0].Entity: {description}}); !errors.Is(err, ErrInvalidContext) {
			t.Fatalf("unrepresentable protobuf admitted: %v", err)
		}
	}
	// A protobuf can be decodable yet not canonical (duplicate scalar field).
	var wire wireContext
	if err := json.Unmarshal(context.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	wire.Capabilities[0].Descriptions[0] = append(wire.Capabilities[0].Descriptions[0], 0x10, 0x01)
	duplicated, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseContext(duplicated); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("noncanonical protobuf wire admitted: %v", err)
	}
	tooMany := make([]Identity, MaxIdentities+1)
	if _, err := NewContext("v1", context.ManifestSHA256(), tooMany, nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("unbounded identity allocation admitted: %v", err)
	}
}
