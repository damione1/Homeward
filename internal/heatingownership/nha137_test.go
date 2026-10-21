package heatingownership_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

func TestNHA137HeatingOwnershipKeepsFrozenPolicyAndRefusesArbitraryLineage(t *testing.T) {
	baseline, err := heatingownership.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []scope.Revision{scope.BaselineV1(), scope.EcobeeMVPV1()} {
		manifest, err := registry.LoadManifest(revision)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := heatingownership.ParseForManifest(scope.HeatingOwnershipV1(), manifest)
		if err != nil || len(decision.Pairs()) != 9 || !reflect.DeepEqual(decision.Pairs(), baseline.Pairs()) || decision.HeatingOwner() != baseline.HeatingOwner() || decision.EnergyRelation() != baseline.EnergyRelation() {
			t.Fatalf("scope changed frozen heating ownership: %v", err)
		}
		if decision.HiloActionPolicy() != heatingownership.ActionDenied || decision.HeatingOwner() != heatingownership.ProviderNeviweb || decision.EnergyRelation() != heatingownership.EnergyIndependent {
			t.Fatal("amendment changed the domestic heating policy")
		}
		// These expected aliases are independent of the parsed decision asset.
		for _, expected := range [][2]string{
			{"climate.chambre_eugenie", "climate.neviweb130_climate_chambre_eugenie"},
			{"climate.chambre_principale", "climate.neviweb130_climate_chambre_principale"},
			{"climate.chambre_sous_sol", "climate.neviweb130_climate_chambre_sous_sol"},
			{"climate.living_room", "climate.neviweb130_climate_living_room"},
			{"climate.plancher_cuisine", "climate.neviweb130_climate_plancher_cuisine"},
			{"climate.salle_a_manger", "climate.neviweb130_climate_salle_a_manger"},
			{"climate.salle_de_bain", "climate.neviweb130_climate_salle_de_bain"},
			{"climate.sous_sol", "climate.neviweb130_climate_sous_sol"},
			{"switch.chauffe_eau", "switch.neviweb130_switch_chauffe_eau"},
		} {
			left, err := domain.ParseHAAlias(expected[0])
			if err != nil {
				t.Fatal(err)
			}
			right, err := domain.ParseHAAlias(expected[1])
			if err != nil {
				t.Fatal(err)
			}
			pair, ok := decision.PairFor(left)
			hilo, neviweb := pair.Aliases()
			if !ok || hilo != left || neviweb != right || pair.Relation() != heatingownership.RelationUnproven {
				t.Fatalf("scope changed candidate pair %s/%s", expected[0], expected[1])
			}
			fromRight, ok := decision.PairFor(right)
			if !ok || !reflect.DeepEqual(fromRight, pair) {
				t.Fatal("pair lookup lost provider identity")
			}
			if _, proved := pair.PhysicalProof(); proved {
				t.Fatal("scope amendment invented physical pairing proof")
			}
		}
		ecobee, _ := domain.ParseHAAlias("climate.home")
		if _, ok := decision.PairFor(ecobee); ok {
			t.Fatal("Ecobee became a Hilo/Neviweb candidate")
		}
		if revision == scope.EcobeeMVPV1() {
			if _, err := heatingownership.Parse(scope.HeatingOwnershipV1(), manifest); !errors.Is(err, heatingownership.ErrInvalidDecision) {
				t.Fatalf("legacy parser accepted amended digest: %v", err)
			}
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(scope.ManifestV1(), &raw); err != nil {
		t.Fatal(err)
	}
	raw["entities"].([]any)[0].(map[string]any)["source"] = "untrusted_homekit_source"
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	arbitrary, err := registry.ParseManifest(encoded)
	if err != nil {
		t.Fatal("negative fixture must pass legacy set parsing:", err)
	}
	for _, manifest := range []registry.Manifest{{}, arbitrary} {
		if _, err := heatingownership.ParseForManifest(scope.HeatingOwnershipV1(), manifest); !errors.Is(err, heatingownership.ErrInvalidDecision) {
			t.Fatalf("unadmitted lineage acquired frozen heating policy: %v", err)
		}
	}
	amended, err := registry.LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := heatingownership.ParseForManifest([]byte(`{"version":1}`), amended); !errors.Is(err, heatingownership.ErrInvalidDecision) {
		t.Fatalf("corrupt historical decision admitted: %v", err)
	}
}
