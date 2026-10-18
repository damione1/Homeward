package heatingownership

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func nha011Manifest(t *testing.T) registry.Manifest {
	t.Helper()
	manifest, err := registry.ParseManifest(scope.ManifestV1())
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func nha011Alias(t *testing.T, text string) domain.HAAlias {
	t.Helper()
	alias, err := domain.ParseHAAlias(text)
	if err != nil {
		t.Fatal(err)
	}
	return alias
}

func nha011DecisionJSON(t *testing.T) rawDecision {
	t.Helper()
	var raw rawDecision
	if err := json.Unmarshal(scope.HeatingOwnershipV1(), &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func nha011ParseRaw(t *testing.T, raw rawDecision) (Decision, error) {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return Parse(encoded, nha011Manifest(t))
}

func TestNHA011CorpusCoversSelectedHeatingSources(t *testing.T) {
	decision, err := LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	if decision.HeatingOwner() != ProviderNeviweb || decision.HiloActionPolicy() != ActionDenied || decision.EnergyRelation() != EnergyIndependent {
		t.Fatal("MVP authority or energy policy changed")
	}
	var selected struct {
		Entities []struct {
			ID     string `json:"entity_id"`
			Source string `json:"source"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(scope.ManifestV1(), &selected); err != nil {
		t.Fatal(err)
	}
	aliases := make(map[string]bool)
	hiloCount, neviwebCount, selectedHeating := 0, 0, 0
	for _, entity := range selected.Entities {
		switch entity.Source {
		case "hilo":
			hiloCount++
		case "neviweb130":
			neviwebCount++
		default:
			continue
		}
		if strings.HasPrefix(entity.ID, "climate.") || strings.HasPrefix(entity.ID, "switch.") {
			selectedHeating++
			aliases[entity.ID] = false
		}
	}
	if hiloCount != 58 || neviwebCount != 11 || selectedHeating != 18 || len(decision.Pairs()) != 9 {
		t.Fatalf("source/coverage drift: Hilo=%d Neviweb=%d heating=%d pairs=%d", hiloCount, neviwebCount, selectedHeating, len(decision.Pairs()))
	}
	for _, pair := range decision.Pairs() {
		_, hasProof := pair.PhysicalProof()
		if pair.Relation() != RelationUnproven || hasProof || len(pair.SourceRefs()) == 0 {
			t.Fatal("real corpus claims unsupported physical identity or omits provenance")
		}
		hilo, neviweb := pair.Aliases()
		for _, alias := range []domain.HAAlias{hilo, neviweb} {
			value, _ := alias.Value()
			if _, exists := aliases[value]; !exists || aliases[value] {
				t.Fatalf("missing or repeated selected heating alias %s", value)
			}
			aliases[value] = true
		}
	}
	for alias, covered := range aliases {
		if !covered {
			t.Fatalf("selected heating alias absent from decisions: %s", alias)
		}
	}
	first := decision.Pairs()[0]
	refs := first.SourceRefs()
	refs[0] = "mutated"
	if decision.Pairs()[0].SourceRefs()[0] == "mutated" {
		t.Fatal("caller mutated decision provenance")
	}
	copyBytes := scope.HeatingOwnershipV1()
	copyBytes[0] = 'x'
	if scope.HeatingOwnershipV1()[0] == 'x' {
		t.Fatal("caller mutated embedded decision")
	}
}

func TestNHA011CorroboratedFixtureDoesNotChangeAuthority(t *testing.T) {
	raw := nha011DecisionJSON(t)
	raw.Candidates[0].Relation = rawRelation{
		Kind:    "corroborated_hardware_identity",
		Hilo:    &rawHardware{Manufacturer: "Sinopé", Model: "TH1123ZB", Serial: "SYNTHETIC-GLOBAL-001", SerialScope: "manufacturer_global", SourceRef: "fixture:hilo:device"},
		Neviweb: &rawHardware{Manufacturer: "Sinopé", Model: "TH1123ZB", Serial: "SYNTHETIC-GLOBAL-001", SerialScope: "manufacturer_global", SourceRef: "fixture:neviweb:device"},
	}
	decision, err := nha011ParseRaw(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Pairs()[0].Relation() != RelationCorroborated || decision.HeatingOwner() != ProviderNeviweb ||
		decision.HiloActionPolicy() != ActionDenied || decision.EnergyRelation() != EnergyIndependent {
		t.Fatal("synthetic proof changed command or energy policy")
	}
	proof, present := decision.Pairs()[0].PhysicalProof()
	if !present {
		t.Fatal("corroborated fixture lost its auditable evidence")
	}
	hilo, neviweb := proof.Observations()
	hManufacturer, hModel, hSerial := hilo.Identity()
	nManufacturer, nModel, nSerial := neviweb.Identity()
	if hManufacturer != nManufacturer || hModel != nModel || hSerial != nSerial || hilo.SourceRef() == neviweb.SourceRef() {
		t.Fatal("proof observations were not retained independently")
	}
	// An identical provider-local ID and model is not a global hardware proof.
	raw.Candidates[0].Relation.Hilo.SerialScope = "provider_local"
	if _, err := nha011ParseRaw(t, raw); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("provider-local identifier admitted as hardware proof: %v", err)
	}
	raw.Candidates[0].Relation.Hilo.SerialScope = "manufacturer_global"
	raw.Candidates[0].Relation.Neviweb.SourceRef = raw.Candidates[0].Relation.Hilo.SourceRef
	if _, err := nha011ParseRaw(t, raw); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("one repeated provenance admitted as corroboration: %v", err)
	}
}

func TestNHA011HiloCandidateIsReadOnlyAcrossFrozenSource(t *testing.T) {
	decision, err := LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	var selected struct {
		Entities []struct {
			ID     string `json:"entity_id"`
			Source string `json:"source"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(scope.ManifestV1(), &selected); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range selected.Entities {
		if entry.Source != "hilo" {
			continue
		}
		alias := nha011Alias(t, entry.ID)
		var description *wireregistry.CapabilityDescription
		switch {
		case strings.HasPrefix(entry.ID, "climate."):
			description = &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, DisplayName: "Fixture thermostat observation", Endpoint: "climate", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{}}}
		case strings.HasPrefix(entry.ID, "switch."):
			description = &wireregistry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Fixture water heater observation", Endpoint: "relay", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: false}}}
		default:
			description = &wireregistry.CapabilityDescription{SchemaId: contracts.MeasurementSchema, Major: 1, DisplayName: "Fixture measurement", Endpoint: "value", Constraints: &wireregistry.CapabilityDescription_Measurement{Measurement: &wireregistry.MeasurementConstraints{Unit: "W", Semantic: wireregistry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: wireregistry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}}
		}
		if err := decision.admitDescriptions(alias, []*wireregistry.CapabilityDescription{description}, catalog); err != nil {
			t.Fatalf("Hilo observation %s rejected: %v", entry.ID, err)
		}
		checked++
	}
	if checked != 58 {
		t.Fatalf("only %d Hilo aliases checked", checked)
	}
	writable := &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, DisplayName: "Writable climate", Endpoint: "climate", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{SetpointCelsius: &wireregistry.NumberBounds{Minimum: 5, Maximum: 23}}}}
	if err := decision.admitDescriptions(nha011Alias(t, "climate.chambre_eugenie"), []*wireregistry.CapabilityDescription{writable}, catalog); !errors.Is(err, contracts.ErrActionableCapability) {
		t.Fatalf("Hilo heat command admitted: %v", err)
	}
	if err := decision.admitDescriptions(nha011Alias(t, "climate.home"), []*wireregistry.CapabilityDescription{writable}, catalog); !errors.Is(err, ErrOutsideDecision) {
		t.Fatalf("unrelated source admitted: %v", err)
	}
	if err := (Decision{}).admitDescriptions(nha011Alias(t, "climate.chambre_eugenie"), []*wireregistry.CapabilityDescription{writable}, catalog); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("zero decision admitted candidate: %v", err)
	}
}

func TestNHA011RejectsMissingProvenanceAndInvalidPolicy(t *testing.T) {
	raw := nha011DecisionJSON(t)
	raw.Policy.HeatingOwner = "hilo"
	if _, err := nha011ParseRaw(t, raw); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("alternate command owner accepted: %v", err)
	}
	raw = nha011DecisionJSON(t)
	raw.Candidates[0].NeviwebAlias = "climate.chambre_eugenie"
	if _, err := nha011ParseRaw(t, raw); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("provider provenance inferred from alias: %v", err)
	}
	if _, err := Parse(scope.HeatingOwnershipV1(), registry.Manifest{}); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("manifest without parsed digest admitted: %v", err)
	}
	bad := append(scope.HeatingOwnershipV1(), []byte(` {"extra":true}`)...)
	if _, err := Parse(bad, nha011Manifest(t)); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("trailing JSON admitted: %v", err)
	}
}
