package registry_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/integrations/fake"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func nha011Command(entity domain.EntityID, schema, endpoint string, args *actions.ActionArgs) *actions.CommandRequest {
	text, _ := entity.Value()
	return &actions.CommandRequest{
		CommandId: "fixture-command", AdmissionEpoch: "fixture-epoch", ConfigRevision: 1,
		Target:             &wireregistry.EntityRef{Reference: &wireregistry.EntityRef_EntityId{EntityId: text}},
		CapabilitySchemaId: schema, Endpoint: endpoint, Arguments: args,
	}
}

func nha011ProofFixture(t *testing.T) []byte {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(scope.HeatingOwnershipV1(), &raw); err != nil {
		t.Fatal(err)
	}
	first := raw["candidates"].([]any)[0].(map[string]any)
	first["relation"] = map[string]any{
		"kind":    "corroborated_hardware_identity",
		"hilo":    map[string]any{"manufacturer": "Sinopé", "model": "TH1123ZB", "serial": "SYNTHETIC-GLOBAL-001", "serial_scope": "manufacturer_global", "source_ref": "fixture:hilo:serial"},
		"neviweb": map[string]any{"manufacturer": "Sinopé", "model": "TH1123ZB", "serial": "SYNTHETIC-GLOBAL-001", "serial_scope": "manufacturer_global", "source_ref": "fixture:neviweb:serial"},
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestNHA011DecisionConsumesRegistryAndCapabilityAuthorities(t *testing.T) {
	store, ctx, _ := nha015Store(t)
	repo := nha015Repository(t, store)
	decision, err := heatingownership.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	hiloInstance := nha015Instance(t, "fixture-hilo")
	neviwebInstance := nha015Instance(t, "fixture-neviweb")
	type example struct {
		hilo, neviweb string
		device        string
		schema        string
		endpoint      string
		readonly      *wireregistry.CapabilityDescription
		writable      *wireregistry.CapabilityDescription
		args          *actions.ActionArgs
	}
	setpoint := 19.0
	examples := []example{
		{
			hilo: "climate.chambre_eugenie", neviweb: "climate.neviweb130_climate_chambre_eugenie", device: "same-readable-room-name", schema: contracts.ClimateSchema, endpoint: "climate",
			readonly: &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, DisplayName: "Fixture thermostat observation", Endpoint: "climate", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{}}},
			writable: &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, DisplayName: "Fixture thermostat command", Endpoint: "climate", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{SetpointCelsius: &wireregistry.NumberBounds{Minimum: 5, Maximum: 23}}}},
			args:     &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &setpoint}}},
		},
		{
			hilo: "switch.chauffe_eau", neviweb: "switch.neviweb130_switch_chauffe_eau", device: "same-readable-water-heater-name", schema: contracts.SwitchSchema, endpoint: "relay",
			readonly: &wireregistry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Fixture water heater observation", Endpoint: "relay", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: false}}},
			writable: &wireregistry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, DisplayName: "Fixture water heater command", Endpoint: "relay", Constraints: &wireregistry.CapabilityDescription_Switch{Switch: &wireregistry.SwitchConstraints{SetOn: true}}},
			args:     &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: false}}},
		},
	}
	// Configure both providers with identical SourceKey text in different
	// instances. The Registry keeps their bindings and IDs independent.
	type configured struct {
		example
		key     integrations.SourceKey
		hilo    integrations.ResolvedIdentity
		neviweb integrations.ResolvedIdentity
	}
	bound := make([]configured, 0, len(examples))
	for _, item := range examples {
		key := nha015Source(t, item.device, item.endpoint)
		hilo, err := repo.ConfigureBinding(ctx, hiloInstance, key, item.hilo)
		if err != nil {
			t.Fatal(err)
		}
		neviweb, err := repo.ConfigureBinding(ctx, neviwebInstance, key, item.neviweb)
		if err != nil {
			t.Fatal(err)
		}
		if hilo.Device == neviweb.Device || hilo.Entity == neviweb.Entity {
			t.Fatal("homonymous source bindings merged")
		}
		bound = append(bound, configured{example: item, key: key, hilo: hilo, neviweb: neviweb})
	}
	// Rename both current aliases and leave a historical alias behind. Source
	// provenance must still come from the original configured manifest alias.
	if err := repo.Rename(ctx, bound[0].hilo.Entity, "climate.fixture_hilo_previous"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Rename(ctx, bound[0].hilo.Entity, "climate.fixture_hilo_current"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Rename(ctx, bound[0].neviweb.Entity, "climate.fixture_neviweb_current"); err != nil {
		t.Fatal(err)
	}
	records, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkedHistory := false
	for _, record := range records {
		_, entity, err := record.IDs()
		if err != nil {
			t.Fatal(err)
		}
		if entity != bound[0].hilo.Entity {
			continue
		}
		original, current, history, err := record.Aliases()
		if err != nil {
			t.Fatal(err)
		}
		originalText, _ := original.Value()
		currentText, _ := current.Value()
		if originalText != bound[0].example.hilo || currentText != "climate.fixture_hilo_current" || len(history) != 3 {
			t.Fatal("Registry lost the original, current, or historical alias")
		}
		previous := false
		for _, alias := range history {
			value, _ := alias.Value()
			previous = previous || value == "climate.fixture_hilo_previous"
		}
		if !previous {
			t.Fatal("Registry lost the historical alias")
		}
		checkedHistory = true
	}
	if !checkedHistory {
		t.Fatal("renamed Hilo binding absent from Registry.List")
	}
	_, sink, err := fake.NewCommandFixture()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AttachProjection(ctx, sink); err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(repo, sink, catalog)
	if err != nil {
		t.Fatal(err)
	}
	hiloSession, err := host.OpenSession(hiloInstance)
	if err != nil {
		t.Fatal(err)
	}
	neviwebSession, err := host.OpenSession(neviwebInstance)
	if err != nil {
		t.Fatal(err)
	}
	var firstHiloCandidate integrations.Candidate
	for index, item := range bound {
		// This was the dangerous combination: writable Neviweb-like data
		// with a key actually bound to Hilo. There is no free alias argument.
		if _, err := decision.CandidateFor(ctx, repo, hiloInstance, item.key, []*wireregistry.CapabilityDescription{item.writable}, catalog); !errors.Is(err, contracts.ErrActionableCapability) {
			t.Fatalf("Hilo command description admitted: %v", err)
		}
		hiloCandidate, err := decision.CandidateFor(ctx, repo, hiloInstance, item.key, []*wireregistry.CapabilityDescription{item.readonly}, catalog)
		if err != nil {
			t.Fatalf("Hilo observation rejected: %v", err)
		}
		neviwebCandidate, err := decision.CandidateFor(ctx, repo, neviwebInstance, item.key, []*wireregistry.CapabilityDescription{item.writable}, catalog)
		if err != nil {
			t.Fatalf("Neviweb heating command description rejected: %v", err)
		}
		if index == 0 {
			firstHiloCandidate = hiloCandidate
		}
		hiloBindings, err := hiloSession.Discover(ctx, []integrations.Candidate{hiloCandidate})
		if err != nil || len(hiloBindings) != 1 {
			t.Fatalf("Hilo Host.Discover failed: %v", err)
		}
		neviwebBindings, err := neviwebSession.Discover(ctx, []integrations.Candidate{neviwebCandidate})
		if err != nil || len(neviwebBindings) != 1 {
			t.Fatalf("Neviweb Host.Discover failed: %v", err)
		}
		hiloDevice, hiloEntity, err := hiloBindings[0].IDs()
		if err != nil || hiloDevice != item.hilo.Device || hiloEntity != item.hilo.Entity {
			t.Fatalf("Hilo Host identity differs from Registry: %v", err)
		}
		neviwebDevice, neviwebEntity, err := neviwebBindings[0].IDs()
		if err != nil || neviwebDevice != item.neviweb.Device || neviwebEntity != item.neviweb.Entity {
			t.Fatalf("Neviweb Host identity differs from Registry: %v", err)
		}
		if _, err := host.Capabilities(hiloEntity); err != nil {
			t.Fatal(err)
		}
		if admission := catalog.AdmitCapabilityCommand(nha011Command(hiloEntity, item.schema, item.endpoint, item.args), "fixture-epoch", 1); admission == nil {
			t.Fatal("Hilo command accepted by capability authority")
		}
		if admission := catalog.AdmitCapabilityCommand(nha011Command(neviwebEntity, item.schema, item.endpoint, item.args), "fixture-epoch", 1); admission != nil {
			t.Fatalf("Neviweb command refused by capability authority: %v", admission)
		}
	}
	if _, err := decision.CandidateFor(ctx, repo, nha015Instance(t, "fixture-absent"), bound[0].key, []*wireregistry.CapabilityDescription{bound[0].writable}, catalog); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("same key in unconfigured instance admitted: %v", err)
	}
	if _, err := decision.CandidateFor(ctx, repo, hiloInstance, nha015Source(t, "fixture-absent", "climate"), []*wireregistry.CapabilityDescription{bound[0].readonly}, catalog); !errors.Is(err, registry.ErrUnconfigured) {
		t.Fatalf("absent source key admitted: %v", err)
	}
	if _, err := (heatingownership.Decision{}).CandidateFor(ctx, repo, hiloInstance, bound[0].key, []*wireregistry.CapabilityDescription{bound[0].readonly}, catalog); !errors.Is(err, heatingownership.ErrInvalidDecision) {
		t.Fatalf("zero decision admitted: %v", err)
	}
	if _, err := decision.CandidateFor(ctx, nil, hiloInstance, bound[0].key, []*wireregistry.CapabilityDescription{bound[0].readonly}, catalog); !errors.Is(err, heatingownership.ErrInvalidDecision) {
		t.Fatalf("absent repository admitted: %v", err)
	}
	if _, err := decision.CandidateFor(ctx, repo, domain.IntegrationInstanceID{}, bound[0].key, []*wireregistry.CapabilityDescription{bound[0].readonly}, catalog); !errors.Is(err, heatingownership.ErrInvalidDecision) {
		t.Fatalf("zero instance admitted: %v", err)
	}
	if _, err := decision.CandidateFor(ctx, repo, hiloInstance, integrations.SourceKey{}, []*wireregistry.CapabilityDescription{bound[0].readonly}, catalog); !errors.Is(err, heatingownership.ErrInvalidDecision) {
		t.Fatalf("zero source key admitted: %v", err)
	}
	if err := repo.Tombstone(ctx, bound[0].hilo.Entity); err != nil {
		t.Fatal(err)
	}
	if _, err := decision.CandidateFor(ctx, repo, hiloInstance, bound[0].key, []*wireregistry.CapabilityDescription{bound[0].readonly}, catalog); !errors.Is(err, registry.ErrTombstoned) {
		t.Fatalf("retired Hilo binding admitted: %v", err)
	}
	if _, err := hiloSession.Discover(ctx, []integrations.Candidate{firstHiloCandidate}); !errors.Is(err, registry.ErrTombstoned) {
		t.Fatalf("Host rediscovered retired Hilo binding: %v", err)
	}
	if _, err := repo.Resolve(ctx, neviwebInstance, []integrations.SourceKey{bound[0].key}); err != nil {
		t.Fatalf("retiring Hilo closed independent Neviweb source: %v", err)
	}
	// A corroborated *synthetic* physical tuple can qualify the relationship,
	// but it does not rebind either registry entity or change capability policy.
	corroborated, err := heatingownership.Parse(nha011ProofFixture(t), nha015Manifest(t))
	if err != nil || corroborated.Pairs()[0].Relation() != heatingownership.RelationCorroborated {
		t.Fatalf("synthetic relation rejected: %v", err)
	}
	if corroborated.HeatingOwner() != heatingownership.ProviderNeviweb || corroborated.EnergyRelation() != heatingownership.EnergyIndependent {
		t.Fatal("corroboration changed command or energy authority")
	}
	if bound[0].hilo.Device == bound[0].neviweb.Device || bound[0].hilo.Entity == bound[0].neviweb.Entity {
		t.Fatal("synthetic proof merged registry identities")
	}
}
