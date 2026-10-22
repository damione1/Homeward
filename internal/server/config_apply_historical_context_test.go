package server

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/configcompilation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func nha055HistoricalIdentity(t *testing.T, original string, retired bool) (configcompilation.Identity, integrations.RegistryRecord) {
	t.Helper()
	record, _ := nha055SemanticRecord(t, original, retired, "switch.nha055_historical_current")
	device, entity, err := record.IDs()
	if err != nil {
		t.Fatal(err)
	}
	instance, key, err := record.Source()
	if err != nil {
		t.Fatal(err)
	}
	sourceDevice, sourceEntity, err := key.Components()
	if err != nil {
		t.Fatal(err)
	}
	origin, current, aliases, err := record.Aliases()
	if err != nil {
		t.Fatal(err)
	}
	return configcompilation.Identity{Device: device, Entity: entity, Instance: instance, SourceDevice: sourceDevice, SourceEntity: sourceEntity, Original: origin, Current: current, Aliases: aliases, Retired: retired}, record
}

func nha055HistoricalContext(t *testing.T, manifest registry.Manifest, identity configcompilation.Identity, descriptions []*wireregistry.CapabilityDescription) configcompilation.Context {
	t.Helper()
	revision, ok := manifest.Revision()
	if !ok {
		t.Fatal("fixture manifest has no canonical revision")
	}
	text, err := revision.Value()
	if err != nil {
		t.Fatal(err)
	}
	digest, ok := manifest.Digest()
	if !ok {
		t.Fatal("fixture manifest has no digest")
	}
	capabilities := make(map[domain.EntityID][]*wireregistry.CapabilityDescription)
	if descriptions != nil {
		capabilities[identity.Entity] = descriptions
	}
	input, err := configcompilation.NewContext(text, digest, []configcompilation.Identity{identity}, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func TestNHA055HistoricalCompilationWorksOfflineWithoutPopulatingHost(t *testing.T) {
	_, manifest := nha055SemanticCatalog(t)
	identity, expectedRecord := nha055HistoricalIdentity(t, "switch.chauffe_eau", false)
	input := nha055HistoricalContext(t, manifest, identity, []*wireregistry.CapabilityDescription{nha055SemanticSwitch(true, "relay")})
	// Model cold start with persisted bytes and a genuinely empty live Host.
	decoded, err := configcompilation.ParseContext(input.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := fake.NewFixture()
	if err != nil {
		t.Fatal(err)
	}
	before, err := host.Capabilities(identity.Entity)
	if err != nil || len(before) != 0 {
		t.Fatalf("cold Host unexpectedly discovered a device: %v", err)
	}
	catalogue, records, err := historicalSourceCompilationInputs(context.Background(), decoded, manifest)
	if err != nil || !catalogue.Valid() || len(records) != 1 || !records[0].Equal(expectedRecord) {
		t.Fatalf("historical inputs did not reconstruct: records=%d err=%v", len(records), err)
	}
	if len(catalogue.Entities()) != 2 || len(catalogue.Devices()) != 0 || len(catalogue.Events()) != 0 || len(catalogue.Scripts()) != 0 {
		t.Fatal("opaque source identity became a HA device/event/script namespace")
	}
	file, err := parser.NewSourceFile("offline.yaml", []byte("id: nha055_offline\ntriggers:\n- trigger: time\n  at: '10:00:00'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.chauffe_eau}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha055_offline", "offline.yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		t.Fatalf("historical source did not compile offline: %v %+v", err, compiled.Rejected())
	}
	after, err := host.Capabilities(identity.Entity)
	if err != nil || len(after) != 0 {
		t.Fatalf("compilation populated live capability authority: %v", err)
	}
}

func TestNHA055HistoricalCompilationUsesExactDeclarationAndPreservesAbsence(t *testing.T) {
	_, manifest := nha055SemanticCatalog(t)
	identity, _ := nha055HistoricalIdentity(t, "switch.chauffe_eau", false)
	var previous configcompilation.Context
	for _, example := range []struct {
		name         string
		descriptions []*wireregistry.CapabilityDescription
		want         []ir.DeclaredSupport
	}{
		{"writable", []*wireregistry.CapabilityDescription{nha055SemanticSwitch(true, "relay")}, []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportSwitchTurnOn}},
		{"readonly", []*wireregistry.CapabilityDescription{nha055SemanticSwitch(false, "relay")}, []ir.DeclaredSupport{ir.SupportObserveState}},
		{"absent", nil, nil},
		{"explicit empty", []*wireregistry.CapabilityDescription{}, nil},
	} {
		t.Run(example.name, func(t *testing.T) {
			input := nha055HistoricalContext(t, manifest, identity, example.descriptions)
			catalogue, records, err := historicalSourceCompilationInputs(context.Background(), input, manifest)
			if err != nil || len(records) != 1 {
				t.Fatalf("historical inputs: %v", err)
			}
			for _, entity := range catalogue.Entities() {
				if entity.Status() != ir.ReferencePresent || !reflect.DeepEqual(entity.Supports(), example.want) {
					t.Fatalf("declaration was expanded or a capability invented: %v", entity.Supports())
				}
			}
			if previous.Valid() && input.SHA256() == previous.SHA256() {
				t.Fatal("different historical declaration became the same context")
			}
			previous = input
		})
	}
}

func TestNHA055HistoricalCompilationKeepsTombstonesAndRejectsOutsideProvenance(t *testing.T) {
	_, manifest := nha055SemanticCatalog(t)
	identity, _ := nha055HistoricalIdentity(t, "switch.chauffe_eau", true)
	input := nha055HistoricalContext(t, manifest, identity, nil)
	catalogue, records, err := historicalSourceCompilationInputs(context.Background(), input, manifest)
	if err != nil || len(records) != 1 || !records[0].Retired() {
		t.Fatalf("retired identity changed: %v", err)
	}
	for _, entity := range catalogue.Entities() {
		if entity.Status() != ir.ReferenceExcluded || len(entity.Supports()) != 0 {
			t.Fatal("tombstoned alias became present or actionable")
		}
	}
	outside, _ := nha055HistoricalIdentity(t, "switch.nha055_outside_manifest", false)
	outsideInput := nha055HistoricalContext(t, manifest, outside, []*wireregistry.CapabilityDescription{nha055SemanticSwitch(true, "relay")})
	if _, records, err := historicalSourceCompilationInputs(context.Background(), outsideInput, manifest); !errors.Is(err, ir.ErrInvalidCatalogue) || records != nil {
		t.Fatalf("out-of-scope provenance admitted: %v", err)
	}
	reserved, _ := domain.ParseHAAlias("switch.neviweb130_switch_chauffe_eau")
	if _, present := manifest.SourceFor(reserved); !present {
		t.Fatal("reserved-alias fixture is absent from manifest")
	}
	identity.Retired = false
	identity.Current = reserved
	identity.Aliases = []domain.HAAlias{identity.Original, reserved}
	conflicting := nha055HistoricalContext(t, manifest, identity, nil)
	if _, records, err := historicalSourceCompilationInputs(context.Background(), conflicting, manifest); !errors.Is(err, ir.ErrInvalidCatalogue) || records != nil {
		t.Fatalf("another manifest identity was borrowed as an alias: %v", err)
	}
}

func TestNHA055HistoricalCompilationReadmitsSemanticConstraints(t *testing.T) {
	_, manifest := nha055SemanticCatalog(t)
	identity, _ := nha055HistoricalIdentity(t, "switch.chauffe_eau", false)
	wrongSchema := nha055SemanticSwitch(true, "relay")
	wrongSchema.SchemaId = contracts.LightSchema
	invalidBounds := &wireregistry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, DisplayName: "Synthetic invalid light", Endpoint: "level", Constraints: &wireregistry.CapabilityDescription_Light{Light: &wireregistry.LightConstraints{SetOn: true, BrightnessPercent: &wireregistry.NumberBounds{Minimum: 100, Maximum: 0}}}}
	for _, description := range []*wireregistry.CapabilityDescription{wrongSchema, invalidBounds} {
		// The value codec admits representation, leaving semantic admission to
		// the existing contract validator exercised by the historical builder.
		input := nha055HistoricalContext(t, manifest, identity, []*wireregistry.CapabilityDescription{description})
		if _, records, err := historicalSourceCompilationInputs(context.Background(), input, manifest); !errors.Is(err, ir.ErrInvalidCatalogue) || records != nil {
			t.Fatalf("wire compatibility became semantic admission: %v", err)
		}
	}
}

func TestNHA055HistoricalCompilationRequiresExactScopeBinding(t *testing.T) {
	_, manifest := nha055SemanticCatalog(t)
	identity, _ := nha055HistoricalIdentity(t, "switch.chauffe_eau", false)
	input := nha055HistoricalContext(t, manifest, identity, nil)
	other, err := registry.LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest := input.ManifestSHA256()
	wrongDigest[0] ^= 1
	digestInput, err := configcompilation.NewContext(input.ManifestRevision(), wrongDigest, input.Identities(), input.Capabilities())
	if err != nil {
		t.Fatal(err)
	}
	for _, example := range []struct {
		input    configcompilation.Context
		manifest registry.Manifest
	}{{input, other}, {digestInput, manifest}, {configcompilation.Context{}, manifest}, {input, registry.Manifest{}}} {
		if _, records, err := historicalSourceCompilationInputs(context.Background(), example.input, example.manifest); !errors.Is(err, ir.ErrInvalidCatalogue) || records != nil {
			t.Fatalf("unbound historical context admitted: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, records, err := historicalSourceCompilationInputs(canceled, input, manifest); !errors.Is(err, context.Canceled) || records != nil {
		t.Fatalf("canceled historical compile admitted: %v", err)
	}
}
