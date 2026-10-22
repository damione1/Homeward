package configplans

import (
	"fmt"
	"github.com/damione1/Homeward/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nha053SelectionWorker = "external_workers:\n - instance: nha053-selection-worker\n   executable: /synthetic-nha053-worker-not-launched\n   bindings:\n    - device: synthetic-device\n      entity: synthetic-entity\n      alias: light.basement_lights\n"
const nha053SelectionMQTT = "mqtt:\n  profile: '5'\n  broker: mqtt://127.0.0.1:1883\n  instance: nha053-selection-mqtt\n  mode: passive\n  username_secret: nha053-absent-user\n  password_secret: nha053-absent-password\n  subscriptions: [homeassistant/#]\n"

func nha053SelectionSource(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "synthetic-worker")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 91\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return strings.Replace(nha053SelectionWorker, "/synthetic-nha053-worker-not-launched", fmt.Sprintf("%q", executable), 1)
}

func TestNHA053IntegrationSelectionChangesSemanticOwnershipAndActiveReferences(t *testing.T) {
	workerSource := nha053SelectionSource(t)
	enabled := nha053Candidate(t, Operational, "operational.main", workerSource+nha053SelectionMQTT)
	disabled := nha053Candidate(t, Operational, "operational.main", workerSource+nha053SelectionMQTT+"disabled_integrations: [nha053-selection-worker]\n")
	before, after := nha053Compile(t, enabled), nha053Compile(t, disabled)
	if enabled.SourceSHA256() == disabled.SourceSHA256() || before.digest == after.digest || !before.refs["light.basement_lights"] || after.refs["light.basement_lights"] {
		t.Fatal("disabled selection had no real semantic/reference effect")
	}
	var binding, selection bool
	for _, change := range difference(before, after) {
		switch change.Field() {
		case "disabled_integrations":
			selection = change.Effect() == ConfigurationEffect && change.ReferencesRemoved() == 1
		case "external_worker_bindings":
			binding = change.Effect() == OwnershipEffect
		default:
			t.Fatal("selection invented unrelated configuration/layout/run/rights effect", change.Field())
		}
	}
	if !binding || !selection {
		t.Fatal("disabled worker lost distinct ownership and selection changes")
	}
	// The selector is a set. Exact source proof still authenticates source order.
	a := nha053Candidate(t, Operational, "operational.main", workerSource+nha053SelectionMQTT+"disabled_integrations: [nha053-selection-worker, nha053-selection-mqtt]\n")
	b := nha053Candidate(t, Operational, "operational.main", workerSource+nha053SelectionMQTT+"disabled_integrations: [nha053-selection-mqtt, nha053-selection-worker]\n")
	left, right := nha053Compile(t, a), nha053Compile(t, b)
	if a.SourceSHA256() == b.SourceSHA256() || left.digest != right.digest || len(difference(left, right)) != 0 {
		t.Fatal("set ordering became runtime effect or exact proof was normalized")
	}
	empty := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", workerSource+nha053SelectionMQTT+"disabled_integrations: []\n"))
	if before.digest != empty.digest || len(difference(before, empty)) != 0 {
		t.Fatal("empty selector changed historic enabled behavior")
	}
	launchA := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", string(disabled.files[0].Bytes())+"listen: 127.0.0.1:1111\n"))
	launchB := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", string(disabled.files[0].Bytes())+"listen: 127.0.0.1:2222\n"))
	if launchA.digest != launchB.digest || len(difference(launchA, launchB)) != 0 {
		t.Fatal("selection gave bootstrap bytes operational authority")
	}
}

func TestNHA053DisabledDeclarationsCannotHideExcludedReferencesOrUnknownSelection(t *testing.T) {
	workerSource := nha053SelectionSource(t)
	excluded := strings.Replace(workerSource, "light.basement_lights", "light.excluded_outside_manifest", 1) + "disabled_integrations: [nha053-selection-worker]\n"
	// Real148 schema admission succeeds;053 must still inspect its declared refs.
	parsed, err := config.ParseOperationalWithEnv([]byte(excluded), func(string) (string, bool) { return "", false })
	if err != nil || len(parsed.ExternalWorkers()) != 0 {
		t.Fatal("fixture did not exercise an admitted, actually disabled declaration", err)
	}
	if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", excluded), nha053Manifest(t)); diagnostic != ExcludedReference {
		t.Fatal("selection concealed excluded declaration from preview admission", diagnostic)
	}
	for name, source := range map[string]string{
		"unknown-id":              workerSource + "disabled_integrations: [unknown]\n",
		"duplicate":               workerSource + "disabled_integrations: [nha053-selection-worker, nha053-selection-worker]\n",
		"invalid-disabled-config": strings.Replace(workerSource, "alias: light.basement_lights", "alias: INVALID ALIAS", 1) + "disabled_integrations: [nha053-selection-worker]\n",
		"future-field":            workerSource + "disabled_integrations: [nha053-selection-worker]\nfuture_selector: true\n",
		"remote-not-selectable":   nha053RemoteSource("https://calendar-a.invalid/calendar.ics", "UTC") + "disabled_integrations: [homeward-remote-calendar-v1]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", source), nha053Manifest(t)); diagnostic == DiagnosticInvalid {
				t.Fatal("unadmitted selector/configuration acquired preview authority")
			}
		})
	}
	if _, err := config.ParseBootstrapLaunchWithEnv([]byte("disabled_integrations: []\n"), func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("bootstrap acquired selection authority")
	}
}
