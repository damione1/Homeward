package configplans

import (
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/ecobee"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/scope"
)

const nha053EcobeeSource = "ecobee:\n  instance: nha053-private-ecobee\n  address: '127.0.0.1:12345'\n  accessory_id: 'CA:FE:00:11:22:33'\n  credential_secret: nha053-absent-pairing\n"

func TestNHA053EcobeeSemanticProjectionUsesExistingAliasAndEnabledSelection(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	admit := func(source string) (Candidate, image) {
		t.Helper()
		candidate := nha053Candidate(t, Operational, "operational.main", source)
		value, diagnostic := compile(candidate, manifest)
		if diagnostic != DiagnosticInvalid {
			t.Fatalf("existing136 source refused: %v", diagnostic)
		}
		return candidate, value
	}
	first, before := admit(nha053EcobeeSource)
	second, after := admit(strings.Replace(nha053EcobeeSource, "127.0.0.1:12345", "127.0.0.1:12346", 1))
	changes := difference(before, after)
	if first.SourceSHA256() == second.SourceSHA256() || before.digest == after.digest || len(changes) != 1 || changes[0].Field() != "ecobee" || changes[0].Effect() != ConfigurationEffect || changes[0].ReferencesAdded() != 0 || changes[0].ReferencesRemoved() != 0 || !before.refs[ecobee.Alias] || !after.refs[ecobee.Alias] {
		t.Fatal("Ecobee locator change lost its private operational effect or existing fixed alias")
	}
	_, disabled := admit(nha053EcobeeSource + "disabled_integrations: [nha053-private-ecobee]\n")
	changes = difference(before, disabled)
	if disabled.refs[ecobee.Alias] || len(changes) != 1 || changes[0].Field() != "disabled_integrations" || changes[0].Effect() != ConfigurationEffect || changes[0].ReferencesRemoved() != 1 {
		t.Fatal("disabled Ecobee reached active references or changed effect family")
	}
	_, removed := admit("{}\n")
	changes = difference(before, removed)
	if len(changes) != 1 || changes[0].Field() != "ecobee" || changes[0].Operation() != Removed || changes[0].ReferencesRemoved() != 1 {
		t.Fatal("removing Ecobee lost the operational reference removal")
	}
	_, changedHandle := admit(strings.Replace(nha053EcobeeSource, "nha053-absent-pairing", "nha053-absent-other-pairing", 1))
	if changedHandle.digest == before.digest {
		t.Fatal("opaque credential reference change was ignored")
	}
}

func TestNHA053EcobeeDisabledDeclarationCannotElevateScopeOrHideInvalidSettings(t *testing.T) {
	baseline := nha053Manifest(t)
	before, _ := baseline.Digest()
	for name, source := range map[string]string{
		"enabled":                  nha053EcobeeSource,
		"disabled":                 nha053EcobeeSource + "disabled_integrations: [nha053-private-ecobee]\n",
		"candidate-scope-selector": "scope_revision: ecobee_mvp_v1\n" + nha053EcobeeSource,
		"disabled-with-selector":   "scope_revision: ecobee_mvp_v1\n" + nha053EcobeeSource + "disabled_integrations: [nha053-private-ecobee]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", source), baseline); diagnostic != ExcludedReference {
				t.Fatalf("Ecobee bypassed the active baseline manifest: %v", diagnostic)
			}
		})
	}
	amended, err := registry.LoadManifest(scope.EcobeeMVPV1())
	if err != nil {
		t.Fatal(err)
	}
	invalid := strings.Replace(nha053EcobeeSource, "127.0.0.1:12345", "localhost:12345", 1) + "disabled_integrations: [nha053-private-ecobee]\n"
	if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", invalid), amended); diagnostic != InvalidSchema {
		t.Fatal("disabled selection bypassed existing136 settings admission")
	}
	after, _ := baseline.Digest()
	if before != after {
		t.Fatal("preview mutated baseline scope")
	}
}
