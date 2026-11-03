package configplans

import (
	"strings"
	"testing"
)

func TestSonosOperationalPlansTrackLocatorsAndValidateDisabledScope(t *testing.T) {
	manifest := nha053Manifest(t)
	source := "sonos:\n  instance: demo-sonos\n  players:\n    - {address: 192.168.50.10, uid: RINCON_00112233445501400, alias: media_player.garage, label: Demo}\n"
	first, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", source), manifest)
	if diagnostic != DiagnosticInvalid || !first.refs["media_player.garage"] {
		t.Fatalf("native settings rejected: %v", diagnostic)
	}
	second, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", strings.Replace(source, "192.168.50.10", "192.168.50.11", 1)), manifest)
	if diagnostic != DiagnosticInvalid {
		t.Fatal(diagnostic)
	}
	changes := difference(first, second)
	if len(changes) != 1 || changes[0].Field() != "sonos" || changes[0].Effect() != ConfigurationEffect || first.digest == second.digest {
		t.Fatal("native locator change lost operational effect")
	}
	for _, suffix := range []string{"", "disabled_integrations: [demo-sonos]\n"} {
		outside := strings.Replace(source, "media_player.garage", "media_player.not_in_manifest", 1) + suffix
		if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", outside), manifest); diagnostic != ExcludedReference {
			t.Fatalf("outside reference admitted: %v", diagnostic)
		}
	}
}
