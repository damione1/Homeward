package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/contracts"
	wireregistry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/hilo"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/mqtt"
	"github.com/damione1/Homeward/internal/neviweb"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
)

func nha055OwnershipFixture(t *testing.T, manifest registry.Manifest, provenance string) heatingownership.Decision {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(scope.HeatingOwnershipV1(), &raw); err != nil {
		t.Fatal(err)
	}
	candidate := raw["candidates"].([]any)[0].(map[string]any)
	candidate["source_refs"] = append(candidate["source_refs"].([]any), provenance)
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := heatingownership.ParseForManifest(encoded, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func nha055OwnershipProviderSettings(t *testing.T) (hilo.Settings, neviweb.Settings) {
	t.Helper()
	hiloInput := hilo.Input{Instance: "nha055-source-hilo", AccountID: "nha055-source-account", LocationID: 10000, LocationHiloID: "urn:hilo:synthetic:ownership", CredentialSecret: "credential", DailyLimit: 500, LeaseWindowSeconds: 180, EnergyMaxGapSeconds: 300, APIBaseURL: "http://127.0.0.1:1", PlatformBaseURL: "http://127.0.0.1:1"}
	for i, alias := range hilo.RequiredDeviceAliases() {
		hiloInput.Bindings = append(hiloInput.Bindings, hilo.BindingInput{Alias: alias, DeviceID: i + 1})
	}
	h, err := hilo.ParseSettings(hiloInput)
	if err != nil {
		t.Fatal(err)
	}
	neviwebInput := neviweb.Input{Instance: "nha055-source-neviweb", BaseURL: "http://127.0.0.1:1", AccountID: "123", NetworkID: "456", UsernameSecret: "username", PasswordSecret: "password", SessionSecret: "session"}
	for i, alias := range nha091Aliases {
		id := fmt.Sprintf("fixture-%02d", i)
		if i == 9 {
			id = ""
		}
		neviwebInput.Bindings = append(neviwebInput.Bindings, neviweb.BindingInput{Alias: alias, DeviceID: id})
	}
	n, err := neviweb.ParseSettings(neviwebInput)
	if err != nil {
		t.Fatal(err)
	}
	return h, n
}

// SQL24 is sufficient for these real constructor/policy checks. No source is
// persisted as SQL26, and neither provider Pilot.Start nor any network runs.
func TestNHA055OwnershipInitialHotAndRestoreConstructorsUseTheExactDecision(t *testing.T) {
	store := nha034ServerStore(t)
	catalog, manifest := nha055SemanticCatalog(t)
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 32)
	if err != nil {
		t.Fatal(err)
	}
	h, n := nha055OwnershipProviderSettings(t)
	initial := nha055OwnershipFixture(t, manifest, "fixture:nha055:initial")
	edited := nha055OwnershipFixture(t, manifest, "fixture:nha055:edited")
	for _, phase := range []struct {
		name     string
		decision heatingownership.Decision
	}{{"initial", initial}, {"hot", edited}, {"restore", initial}} {
		t.Run(phase.name, func(t *testing.T) {
			composition, err := newHiloCompositionWithDecision(manifest, phase.decision, h, store, sequence, catalog, repository)
			if err != nil || !sameSourceOwnership(composition.decision, phase.decision) {
				t.Fatal("Hilo constructor reloaded baseline instead of selected source", err)
			}
			if phase.name == "hot" {
				compiler := &configuredOperationalCompiler{manifest: manifest, repository: repository, store: store, sequence: sequence, catalog: catalog, sourceOwnership: &phase.decision}
				spec, err := compiler.prepareNeviweb(n)
				if err != nil {
					t.Fatal("hot Neviweb constructor rejected the shared source decision", err)
				}
				actual, _ := spec.Instance()
				expected, _ := n.Instance()
				if actual != expected {
					t.Fatal("ownership edit changed native Neviweb source identity")
				}
			} else {
				plan, err := defaultConfiguredRuntimePlanWithManifest(manifest, mqtt.Settings{}, false, nil, store, sequence, neviwebComposition{settings: n, catalog: catalog, ownership: &phase.decision})
				if err != nil || len(plan.Pilots) != 1 {
					t.Fatal("initial/restore Neviweb constructor lost explicit source decision", err)
				}
			}
		})
	}
	invalid := heatingownership.Decision{}
	if _, err := newHiloCompositionWithDecision(manifest, invalid, h, store, sequence, catalog, repository); err == nil {
		t.Fatal("zero source decision reached Hilo constructor")
	}
	if _, err := defaultConfiguredRuntimePlanWithManifest(manifest, mqtt.Settings{}, false, nil, store, sequence, neviwebComposition{settings: n, catalog: catalog, ownership: &invalid}); err == nil {
		t.Fatal("explicit invalid Neviweb decision fell back to baseline")
	}
	var commands int64
	if err := store.InTransaction(context.Background(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM public.command_intents`).Scan(&commands)
	}); err != nil || commands != 0 {
		t.Fatal("constructing ownership participants dispatched actions", err)
	}
}

func TestNHA055EditedOwnershipStillRefusesHiloActionsAndKeepsNeviwebCommandPolicy(t *testing.T) {
	store := nha034ServerStore(t)
	catalog, manifest := nha055SemanticCatalog(t)
	repository, err := registry.NewRepository(store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	decision := nha055OwnershipFixture(t, manifest, "fixture:nha055:edited-action-fence")
	h, n := nha055OwnershipProviderSettings(t)
	instanceH, _ := h.Instance()
	instanceN, _ := n.Instance()
	aliasH, aliasN := decision.Pairs()[0].Aliases()
	nameH, _ := aliasH.Value()
	nameN, _ := aliasN.Value()
	keyH, err := integrations.NewSourceKey("fixture-hilo", nameH)
	if err != nil {
		t.Fatal(err)
	}
	keyN, err := integrations.NewSourceKey("fixture-neviweb", nameN)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := repository.ConfigureBinding(ctx, instanceH, keyH, nameH); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ConfigureBinding(ctx, instanceN, keyN, nameN); err != nil {
		t.Fatal(err)
	}
	observed := &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "climate", DisplayName: "Synthetic observed thermostat", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{}}}
	writable := &wireregistry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "climate", DisplayName: "Synthetic actionable thermostat", Constraints: &wireregistry.CapabilityDescription_Climate{Climate: &wireregistry.ClimateConstraints{SetpointCelsius: &wireregistry.NumberBounds{Minimum: 5, Maximum: 23}}}}
	if _, err := decision.CandidateFor(ctx, repository, instanceH, keyH, []*wireregistry.CapabilityDescription{observed}, catalog); err != nil {
		t.Fatal("edited decision rejected Hilo observation", err)
	}
	if _, err := decision.CandidateFor(ctx, repository, instanceH, keyH, []*wireregistry.CapabilityDescription{writable}, catalog); !errors.Is(err, contracts.ErrActionableCapability) {
		t.Fatal("edited decision granted a Hilo command", err)
	}
	if _, err := decision.CandidateFor(ctx, repository, instanceN, keyN, []*wireregistry.CapabilityDescription{writable}, catalog); err != nil {
		t.Fatal("edited decision lost existing Neviweb ownership", err)
	}
}
