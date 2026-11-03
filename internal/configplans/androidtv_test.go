package configplans

import (
	"strings"
	"testing"
)

func TestAndroidTVOperationalPlansTrackLocatorsAndValidateDisabledScope(t *testing.T) {
	manifest := nha053Manifest(t)
	source := "android_tv:\n  instance: demo-android_tv\n  players:\n    - {address: 192.168.50.10, mac: '02:22:33:44:55:66', credential_secret: pairing, alias: media_player.garage, label: Demo}\n"
	first, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", source), manifest)
	if diagnostic != DiagnosticInvalid || !first.refs["media_player.garage"] {
		t.Fatalf("native settings rejected: %v", diagnostic)
	}
	second, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", strings.Replace(source, "192.168.50.10", "192.168.50.11", 1)), manifest)
	if diagnostic != DiagnosticInvalid {
		t.Fatal(diagnostic)
	}
	changes := difference(first, second)
	if len(changes) != 1 || changes[0].Field() != "android_tv" || changes[0].Effect() != ConfigurationEffect || first.digest == second.digest {
		t.Fatal("native locator change lost operational effect")
	}
	for _, suffix := range []string{"", "disabled_integrations: [demo-android_tv]\n"} {
		outside := strings.Replace(source, "media_player.garage", "media_player.not_in_manifest", 1) + suffix
		if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", outside), manifest); diagnostic != ExcludedReference {
			t.Fatalf("outside reference admitted: %v", diagnostic)
		}
	}
}
