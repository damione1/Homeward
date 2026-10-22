package configactivation

import (
	"github.com/damione1/Homeward/internal/heatingownership"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
)

// OwnershipDecision reads only the ownership source authenticated by this SQL
// generation. Absence is explicit; an invalid or duplicate source never falls
// back to the separately versioned baseline decision.
func (g AdmittedGeneration) OwnershipDecision(manifest registry.Manifest) (heatingownership.Decision, bool, error) {
	if !g.Valid() {
		return heatingownership.Decision{}, false, ErrInvalidStartup
	}
	return ownershipDecisionFromGroups(g.extraGroups, manifest)
}

func ownershipDecisionFromGroups(groups []SourceGroup, manifest registry.Manifest) (heatingownership.Decision, bool, error) {
	var decision heatingownership.Decision
	found := false
	for _, group := range groups {
		if group.descriptor.Kind() != storage.ConfigDocumentKindOwnershipSource {
			continue
		}
		if found || !group.valid() || group.descriptor.Identity() != "heating.ownership" || len(group.files) != 1 {
			return heatingownership.Decision{}, false, ErrStartupIntegrity
		}
		var err error
		decision, err = heatingownership.ParseForManifest(group.files[0].Bytes(), manifest)
		if err != nil {
			return heatingownership.Decision{}, false, ErrStartupIntegrity
		}
		found = true
	}
	return decision, found, nil
}
