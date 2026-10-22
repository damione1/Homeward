package configactivation

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func TestNHA055OwnershipSourceSelectionPreservesProvenanceAndNeverFallsBackOnInvalidInput(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := storage.OwnershipSourceDescriptor("heating.ownership", "ownership.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(scope.HeatingOwnershipV1(), &raw); err != nil {
		t.Fatal(err)
	}
	candidates := raw["candidates"].([]any)
	candidate := candidates[0].(map[string]any)
	refs := candidate["source_refs"].([]any)
	candidate["source_refs"] = append(refs, "fixture:nha055:edited-exact-source")
	edited, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	group := nha055SourceGroup(t, descriptor, "source.ownership.exact", string(edited))
	decision, present, err := ownershipDecisionFromGroups([]SourceGroup{group}, manifest)
	if err != nil || !present || decision.HiloActionPolicy() != heatingownership.ActionDenied || decision.HeatingOwner() != heatingownership.ProviderNeviweb {
		t.Fatal("edited exact ownership did not reach startup selection", err)
	}
	found := false
	for _, ref := range decision.Pairs()[0].SourceRefs() {
		found = found || ref == "fixture:nha055:edited-exact-source"
	}
	if !found {
		t.Fatal("startup silently replaced edited source provenance with the baseline")
	}
	if _, present, err := ownershipDecisionFromGroups(nil, manifest); err != nil || present {
		t.Fatal("absence was not explicit", err)
	}
	for _, groups := range [][]SourceGroup{
		{group, group},
		{nha055SourceGroup(t, descriptor, "source.ownership.invalid", "{}")},
	} {
		if _, present, err := ownershipDecisionFromGroups(groups, manifest); !errors.Is(err, ErrStartupIntegrity) || present {
			t.Fatal("invalid/duplicate admitted source fell back to baseline", err)
		}
	}
	if _, _, err := (AdmittedGeneration{}).OwnershipDecision(manifest); !errors.Is(err, ErrInvalidStartup) {
		t.Fatal("unadmitted generation supplied startup ownership", err)
	}
}
