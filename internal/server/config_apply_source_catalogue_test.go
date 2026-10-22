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
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/scope"
	"google.golang.org/protobuf/proto"
)

func nha055SemanticCatalog(t *testing.T) (*contracts.CapabilityCatalog, registry.Manifest) {
	t.Helper()
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	return catalog, manifest
}

func nha055SemanticRecord(t *testing.T, original string, retired bool, history ...string) (integrations.RegistryRecord, domain.EntityID) {
	t.Helper()
	entity, err := domain.ParseEntityID("nha055-entity:" + original)
	if err != nil {
		t.Fatal(err)
	}
	device, err := domain.ParseDeviceID("nha055-device:" + original)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha055-fixture")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("opaque-protocol-device", original)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := domain.ParseHAAlias(original)
	if err != nil {
		t.Fatal(err)
	}
	aliases := []domain.HAAlias{alias}
	for _, text := range history {
		historical, err := domain.ParseHAAlias(text)
		if err != nil {
			t.Fatal(err)
		}
		aliases = append(aliases, historical)
	}
	record, err := integrations.NewRegistryRecord(device, entity, instance, key, alias, aliases[len(aliases)-1], aliases, retired)
	if err != nil {
		t.Fatal(err)
	}
	return record, entity
}

func nha055SemanticSwitch(on bool, endpoint string) *wireregistry.CapabilityDescription {
	return &wireregistry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Synthetic switch", Endpoint: endpoint, Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: on}}}
}

func nha055SemanticRegister(t *testing.T, catalog *contracts.CapabilityCatalog, entity domain.EntityID, descriptions ...*wireregistry.CapabilityDescription) {
	t.Helper()
	for _, description := range descriptions {
		if err := catalog.RegisterDriverCapability(entity, description); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNHA055SemanticCatalogueUsesAdmittedCapabilitiesAndHistoricalAliases(t *testing.T) {
	catalog, manifest := nha055SemanticCatalog(t)
	record, entity := nha055SemanticRecord(t, "switch.chauffe_eau", false, "switch.nha055_previous", "switch.nha055_current")
	nha055SemanticRegister(t, catalog, entity, nha055SemanticSwitch(false, "relay"))
	result, err := sourceSemanticCatalogueRecords(context.Background(), []integrations.RegistryRecord{record}, catalog.Descriptions, catalog, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid() || len(result.Entities()) != 3 || len(result.Devices()) != 0 || len(result.Events()) != 0 || len(result.Scripts()) != 0 {
		t.Fatalf("registry identities leaked into unproved namespaces: %#v", result)
	}
	for _, item := range result.Entities() {
		if item.Status() != ir.ReferencePresent || !reflect.DeepEqual(item.Supports(), []ir.DeclaredSupport{ir.SupportObserveState}) {
			t.Fatalf("readonly declaration became an action after rename: %v", item.Supports())
		}
	}
	// Replacing the authoritative declaration changes compilation support;
	// merely mentioning an action in source has no input to this decision.
	if err := catalog.ReplaceDriverCapabilitiesBatch([]contracts.DriverCapabilityRegistration{{Entity: entity, Description: nha055SemanticSwitch(true, "relay")}}); err != nil {
		t.Fatal(err)
	}
	result, err = sourceSemanticCatalogueRecords(context.Background(), []integrations.RegistryRecord{record}, catalog.Descriptions, catalog, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Entities() {
		if !reflect.DeepEqual(item.Supports(), []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportSwitchTurnOn}) {
			t.Fatalf("admitted writable declaration lost across alias history: %v", item.Supports())
		}
	}
}

func TestNHA055SemanticCatalogueRetiredAndUnconfiguredReferencesRemainClosed(t *testing.T) {
	catalog, manifest := nha055SemanticCatalog(t)
	record, _ := nha055SemanticRecord(t, "switch.chauffe_eau", true)
	called := false
	result, err := sourceSemanticCatalogueRecords(context.Background(), []integrations.RegistryRecord{record}, func(domain.EntityID) ([]*wireregistry.CapabilityDescription, error) {
		called = true
		return []*wireregistry.CapabilityDescription{nha055SemanticSwitch(true, "relay")}, nil
	}, catalog, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if called || len(result.Entities()) != 1 || result.Entities()[0].Status() != ir.ReferenceExcluded || len(result.Entities()[0].Supports()) != 0 {
		t.Fatal("retired identity recovered actionable support")
	}
	empty, err := sourceSemanticCatalogueRecords(context.Background(), nil, catalog.Descriptions, catalog, manifest)
	if err != nil || !empty.Valid() || len(empty.Entities()) != 0 {
		t.Fatalf("manifest-only identities became configured references: %v", err)
	}
	unbound, entity := nha055SemanticRecord(t, "switch.chauffe_eau", false)
	noDiscovery, err := sourceSemanticCatalogueRecords(context.Background(), []integrations.RegistryRecord{unbound}, catalog.Descriptions, catalog, manifest)
	if err != nil || len(noDiscovery.Entities()) != 1 || len(noDiscovery.Entities()[0].Supports()) != 0 {
		t.Fatalf("entity %v without a discovery acquired support: %v", entity, err)
	}
}

func TestNHA055SemanticCatalogueRejectsUnprovedProvenanceAndChangedDiscovery(t *testing.T) {
	catalog, manifest := nha055SemanticCatalog(t)
	record, entity := nha055SemanticRecord(t, "switch.chauffe_eau", false)
	nha055SemanticRegister(t, catalog, entity, nha055SemanticSwitch(false, "relay"))
	if _, err := sourceSemanticCatalogueRecords(context.Background(), []integrations.RegistryRecord{record}, func(domain.EntityID) ([]*wireregistry.CapabilityDescription, error) {
		return []*wireregistry.CapabilityDescription{nha055SemanticSwitch(true, "relay")}, nil
	}, catalog, manifest); !errors.Is(err, ir.ErrInvalidCatalogue) {
		t.Fatalf("different Host/catalogue declarations admitted: %v", err)
	}
	outside, _ := nha055SemanticRecord(t, "switch.nha055_outside_manifest", false)
	for _, records := range [][]integrations.RegistryRecord{{outside}, {record, record}, {{}}} {
		if _, err := sourceSemanticCatalogueRecords(context.Background(), records, catalog.Descriptions, catalog, manifest); !errors.Is(err, ir.ErrInvalidCatalogue) {
			t.Fatalf("invalid durable identity snapshot admitted: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sourceSemanticCatalogueRecords(canceled, nil, catalog.Descriptions, catalog, manifest); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot admitted: %v", err)
	}
	if _, err := newSourceSemanticCatalogue(context.Background(), nil, nil, catalog, manifest); !errors.Is(err, ir.ErrInvalidCatalogue) {
		t.Fatalf("missing production ports admitted: %v", err)
	}
}

func TestNHA055SemanticSupportsKeepObservationAndCommandDeclarationsDistinct(t *testing.T) {
	catalog, _ := nha055SemanticCatalog(t)
	climate := func(constraints *wireregistry.ClimateConstraints) *wireregistry.CapabilityDescription {
		return &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, DisplayName: "Synthetic thermostat", Endpoint: "climate", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: constraints}}
	}
	light := func(constraints *wireregistry.LightConstraints) *wireregistry.CapabilityDescription {
		return &wireregistry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, DisplayName: "Synthetic light", Endpoint: "level", Constraints: &wireregistry.CapabilityDescription_Light{Light: constraints}}
	}
	for _, example := range []struct {
		name         string
		descriptions []*wireregistry.CapabilityDescription
		want         []ir.DeclaredSupport
	}{
		{"observed setpoint", []*wireregistry.CapabilityDescription{climate(&wireregistry.ClimateConstraints{ObservedSetpointCelsius: &wireregistry.ClimateTemperatureObservation{}})}, []ir.DeclaredSupport{ir.SupportObserveState}},
		{"command setpoint", []*wireregistry.CapabilityDescription{climate(&wireregistry.ClimateConstraints{SetpointCelsius: &wireregistry.NumberBounds{Minimum: 5, Maximum: 30}})}, []ir.DeclaredSupport{ir.SupportClimateSetpoint}},
		{"light has no controls", []*wireregistry.CapabilityDescription{light(&wireregistry.LightConstraints{})}, []ir.DeclaredSupport{ir.SupportObserveState}},
		{"light explicit controls", []*wireregistry.CapabilityDescription{light(&wireregistry.LightConstraints{SetOn: true, BrightnessPercent: &wireregistry.NumberBounds{Minimum: 0, Maximum: 100}, TransitionSeconds: &wireregistry.NumberBounds{Minimum: 0, Maximum: 60}})}, []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportLightTurnOn, ir.SupportLightTurnOff, ir.SupportLightBrightnessAbsolute, ir.SupportLightTransition}},
		{"ambiguous switch endpoints", []*wireregistry.CapabilityDescription{nha055SemanticSwitch(true, "one"), nha055SemanticSwitch(true, "two")}, []ir.DeclaredSupport{ir.SupportObserveState}},
	} {
		t.Run(example.name, func(t *testing.T) {
			got, err := sourceSemanticSupports(catalog, example.descriptions)
			if err != nil || !reflect.DeepEqual(got, example.want) {
				t.Fatalf("got %v, want %v: %v", got, example.want, err)
			}
		})
	}
	invalid := nha055SemanticSwitch(true, "relay")
	invalid.SchemaId = contracts.LightSchema
	for _, descriptions := range [][]*wireregistry.CapabilityDescription{{nil}, {invalid}} {
		if _, err := sourceSemanticSupports(catalog, descriptions); !errors.Is(err, ir.ErrInvalidCatalogue) {
			t.Fatalf("invalid capability escaped admission: %v", err)
		}
	}
	declaration := nha055SemanticSwitch(true, "relay")
	if sameSourceSemanticCapabilities([]*wireregistry.CapabilityDescription{declaration, declaration}, []*wireregistry.CapabilityDescription{declaration, proto.Clone(declaration).(*wireregistry.CapabilityDescription)}) {
		t.Fatal("duplicate endpoint declarations admitted")
	}
}

func TestNHA055ClientActionCannotSupplyItsOwnCatalogueSupport(t *testing.T) {
	catalog, manifest := nha055SemanticCatalog(t)
	record, entity := nha055SemanticRecord(t, "switch.chauffe_eau", false)
	nha055SemanticRegister(t, catalog, entity, nha055SemanticSwitch(false, "relay"))
	semantic, err := sourceSemanticCatalogueRecords(context.Background(), []integrations.RegistryRecord{record}, catalog.Descriptions, catalog, manifest)
	if err != nil {
		t.Fatal(err)
	}
	body := "id: nha055_declared_action\ntriggers:\n- trigger: time\n  at: '10:00:00'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.chauffe_eau}\n"
	file, err := parser.NewSourceFile("automations.yaml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation.nha055_declared_action", "automations.yaml", []parser.SourceFile{file}, semantic)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Accepted()) != 0 || len(compiled.Rejected()) != 1 || compiled.Rejected()[0].Code() != ir.CodeMissingCapability {
		t.Fatalf("client YAML manufactured switch support: accepted=%d rejected=%v", len(compiled.Accepted()), compiled.Rejected())
	}
	if err := catalog.ReplaceDriverCapabilitiesBatch([]contracts.DriverCapabilityRegistration{{Entity: entity, Description: nha055SemanticSwitch(true, "relay")}}); err != nil {
		t.Fatal(err)
	}
	semantic, err = sourceSemanticCatalogueRecords(context.Background(), []integrations.RegistryRecord{record}, catalog.Descriptions, catalog, manifest)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err = ir.CompileSources("automation.nha055_declared_action", "automations.yaml", []parser.SourceFile{file}, semantic)
	if err != nil || len(compiled.Accepted()) != 1 || len(compiled.Rejected()) != 0 {
		t.Fatalf("same source with a real writable declaration refused: %v %+v", err, compiled.Rejected())
	}
}

// This composes the production builder with the existing migrated SQL registry,
// actual Host discovery and a sequencer; no command driver or device is started.
func TestNHA055SemanticCatalogueComposesSQLRegistryAndRealHost(t *testing.T) {
	catalog, manifest := nha055SemanticCatalog(t)
	store := nha034ServerStore(t)
	ctx := context.Background()
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AttachProjection(ctx, sequence); err != nil {
		t.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha055-sql-host-fixture")
	if err != nil {
		t.Fatal(err)
	}
	key, err := integrations.NewSourceKey("opaque-fixture-device", "relay")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ConfigureBinding(ctx, instance, key, "switch.chauffe_eau"); err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(repository, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Revoke(session) })
	candidate, err := integrations.NewCandidate(key, []*wireregistry.CapabilityDescription{nha055SemanticSwitch(true, "relay")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Discover(ctx, []integrations.Candidate{candidate}); err != nil {
		t.Fatal(err)
	}
	semantic, err := newSourceSemanticCatalogue(ctx, repository, host, catalog, manifest)
	if err != nil || len(semantic.Entities()) != 1 {
		t.Fatalf("production SQL/Host catalogue was not composed: %v", err)
	}
	if got := semantic.Entities()[0].Supports(); !reflect.DeepEqual(got, []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportSwitchTurnOn}) {
		t.Fatalf("actual Host declarations changed during conversion: %v", got)
	}
	other, err := registry.LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSourceSemanticCatalogue(ctx, repository, host, catalog, other); !errors.Is(err, ir.ErrInvalidCatalogue) {
		t.Fatalf("different registry scope was admitted: %v", err)
	}
}
