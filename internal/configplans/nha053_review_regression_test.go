package configplans

import (
	"fmt"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/config"
	"go.yaml.in/yaml/v3"
)

func TestNHA053RemovedWorkerBindingsRetainOwnershipEffect(t *testing.T) {
	before := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", nha053SelectionSource(t)))
	for name, source := range map[string]string{"omitted": "{}\n", "empty": "external_workers: []\n"} {
		t.Run(name, func(t *testing.T) {
			after := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", source))
			var ownership bool
			maximum := NoEffect
			for _, change := range difference(before, after) {
				if change.Effect() > maximum {
					maximum = change.Effect()
				}
				if change.Field() == "external_worker_bindings" {
					ownership = change.Effect() == OwnershipEffect && change.ReferencesRemoved() == 1 && change.ReferencesAdded() == 0
					if name == "omitted" && change.Operation() != Removed {
						t.Fatal("omitted binding field was not removed")
					}
				} else if change.Field() != "external_worker_processes" || change.Effect() != ConfigurationEffect {
					t.Fatal("worker removal changed unrelated effects")
				}
			}
			if !ownership || maximum != OwnershipEffect || after.refs["light.basement_lights"] {
				t.Fatal("removing every worker downgraded ownership or retained an active reference")
			}
			var addedOwnership bool
			for _, change := range difference(after, before) {
				if change.Field() == "external_worker_bindings" {
					addedOwnership = change.Effect() == OwnershipEffect && change.ReferencesAdded() == 1 && change.ReferencesRemoved() == 0
				}
			}
			if !addedOwnership {
				t.Fatal("adding the binding did not retain ownership in the reverse direction")
			}
		})
	}
}

func TestNHA053AdmittedWorkerLightAndFanProfilesChangeSemanticOwnership(t *testing.T) {
	for _, fixture := range []struct{ name, alias, field, profile string }{
		{"light", "light.basement_lights", "ha_light_profile", "caseta_ha_100_v1"},
		{"fan", "fan.living_room_ceiling_fan", "ha_fan_profile", "caseta_ha_fan_4_v1"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			raw := strings.Replace(nha053SelectionSource(t), "light.basement_lights", fixture.alias, 1)
			profiled := raw + fmt.Sprintf("      %s: %s\n", fixture.field, fixture.profile)
			plainCandidate := nha053Candidate(t, Operational, "operational.main", raw)
			profileCandidate := nha053Candidate(t, Operational, "operational.main", profiled)
			before, after := nha053Compile(t, plainCandidate), nha053Compile(t, profileCandidate)
			parsed, err := config.ParseOperationalWithEnv([]byte(profiled), func(string) (string, bool) { return "", false })
			if err != nil || len(parsed.ExternalWorkers()) != 1 || len(parsed.ExternalWorkers()[0].Bindings()) != 1 {
				t.Fatal("fixture did not reach the existing configured binding", err)
			}
			binding := parsed.ExternalWorkers()[0].Bindings()[0]
			if binding.CasetaHA100V1() != (fixture.name == "light") || binding.CasetaHAFan4V1() != (fixture.name == "fan") {
				t.Fatal("profile fixture disagrees with the parent worker's effective profile predicates")
			}
			for _, pair := range [][2]image{{before, after}, {after, before}} {
				changes := difference(pair[0], pair[1])
				if plainCandidate.SourceSHA256() == profileCandidate.SourceSHA256() || pair[0].digest == pair[1].digest || len(changes) != 1 || changes[0].Field() != "external_worker_bindings" || changes[0].Effect() != OwnershipEffect || changes[0].ReferencesAdded() != 0 || changes[0].ReferencesRemoved() != 0 {
					t.Fatal("effective light/fan profile change disappeared from semantic ownership")
				}
			}
			// A disabled worker is still admitted, while its profile has no active
			// runtime effect. Exact-source provenance must remain distinct.
			selector := "disabled_integrations: [nha053-selection-worker]\n"
			disabledPlain := nha053Candidate(t, Operational, "operational.main", raw+selector)
			disabledProfile := nha053Candidate(t, Operational, "operational.main", profiled+selector)
			left, right := nha053Compile(t, disabledPlain), nha053Compile(t, disabledProfile)
			if disabledPlain.SourceSHA256() == disabledProfile.SourceSHA256() || left.digest != right.digest || len(difference(left, right)) != 0 {
				t.Fatal("disabled profile became an active effect or lost exact-source provenance")
			}
		})
	}
}

func nha053BackupAliasDAG(levels int) string {
	var source strings.Builder
	source.WriteString("backups:\n  node0: &node0 [0]\n")
	for i := 1; i <= levels; i++ {
		fmt.Fprintf(&source, "  node%d: &node%d [*node%d, *node%d]\n", i, i, i-1, i-1)
	}
	return source.String()
}

func TestNHA053YAMLAliasExpansionBudgetIncludesIgnoredLaunchNodes(t *testing.T) {
	benign := nha053BackupAliasDAG(3)
	if _, err := admittedYAML([]byte(benign)); err != nil {
		t.Fatal("small acyclic shared YAML was rejected", err)
	}
	before := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", "{}\n"))
	after := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", benign))
	if before.digest != after.digest || len(difference(before, after)) != 0 {
		t.Fatal("launch-only aliases acquired operational meaning")
	}
	// The source is small, its expanded graph is over65K nodes, and its depth
	// remains below64. The historical depth/cycle guard alone admitted it.
	expansion := nha053BackupAliasDAG(15)
	if len(expansion) > 4096 {
		t.Fatal("fixture no longer exercises compact alias amplification")
	}
	if _, err := admittedYAML([]byte(expansion)); err == nil {
		t.Fatal("expanded alias DAG bypassed the shared occurrence budget")
	}
	if _, diagnostic := compile(nha053Candidate(t, Operational, "operational.main", expansion), nha053Manifest(t)); diagnostic != InvalidSchema {
		t.Fatal("opaque ignored backups node bypassed the operational preview bound")
	}
	// Sharing a node on a shallow path first must not exempt a deeper path
	// from the independent depth bound (a depth-insensitive memo would do so).
	shared := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "shared"}
	deep := shared
	for i := 0; i < 64; i++ {
		deep = &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{deep}}
	}
	root := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{shared, deep}}
	if uniqueYAML(root, 0, map[*yaml.Node]bool{}) {
		t.Fatal("shared shallow success bypassed a deeper path's depth limit")
	}
	if _, err := admittedYAML([]byte("backups: &cycle [*cycle]\n")); err == nil {
		t.Fatal("global budget weakened cycle rejection")
	}
}
