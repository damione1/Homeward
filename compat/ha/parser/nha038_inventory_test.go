package parser_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
)

// This test never downloads or embeds third-party blueprint source. Its
// inventory-derived private bytes are qualified explicitly in the local gate;
// a public gate without that bundle reports a skip, not an IKEA success.
func TestNHA038InventoryBlueprint(t *testing.T) {
	bundle := os.Getenv("HOMEWARD_NHA038_INVENTORY_BUNDLE")
	if bundle == "" {
		t.Skip("private inventory bundle absent; this run does not qualify the observed IKEA blueprint")
	}
	const definitionPath = "blueprints/automation/damru/ikea_E2201-E2213_ZHA-Z2M_control-anything.yaml"
	const definitionSHA = "973e8fec2bae1cb847d9e201e98deea431a7ca5d0dce339688bc0365cec2afe9"
	const invocationSHA = "cc6aada5ee9a4ae1060b173efa94ceb865c0853a4821c10c6b521e8f15e2688c"
	definitionBytes := nha038Read(t, filepath.Join(bundle, definitionPath))
	invocationBytes := nha038Read(t, filepath.Join(bundle, "invocation.yaml"))
	for _, check := range []struct {
		name string
		data []byte
		sha  string
	}{{"reconstructed documentary blueprint, not HA raw bytes", definitionBytes, definitionSHA}, {"retained NHA-158 invocation", invocationBytes, invocationSHA}} {
		got := fmt.Sprintf("%x", sha256.Sum256(check.data))
		if got != check.sha {
			t.Fatalf("%s hash=%s want=%s", check.name, got, check.sha)
		}
		t.Logf("%s bytes=%d sha256=%s", check.name, len(check.data), got)
	}
	retained := nha038Read(t, filepath.Join("..", "..", "..", "scope", "logic_mvp_sources", "automation.ikea_kitchen_lights_remote.yaml"))
	if !reflect.DeepEqual(retained, invocationBytes) {
		t.Fatal("private invocation differs from retained NHA-158 bytes")
	}
	result, err := parser.ExpandBlueprint(bundle, "invocation.yaml")
	if err != nil || result == nil || !result.Valid() {
		t.Fatalf("observed blueprint expansion=%v error=%v", result, err)
	}
	if fmt.Sprintf("%x", result.BlueprintSHA256()) != definitionSHA {
		t.Fatal("artifact did not retain exact parsed blueprint hash")
	}
	definition, err := parser.LoadSources(definitionPath, []parser.SourceFile{nha038Source(t, definitionPath, definitionBytes)}, parser.SyntaxProfileBlueprintDefinition)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := parser.LoadSources("invocation.yaml", []parser.SourceFile{nha038Source(t, "invocation.yaml", invocationBytes)}, parser.SyntaxProfileBlueprintInstance)
	if err != nil {
		t.Fatal(err)
	}
	use, _ := invocation.Root().Lookup("use_blueprint")
	supplied, _ := use.Lookup("input")
	inputs := nha038Inputs(result)
	if len(inputs) != 12 {
		t.Fatalf("effective inputs=%d want12", len(inputs))
	}
	for _, name := range []string{"on_press_action", "off_press_action", "on_hold_action", "off_hold_action", "on_double_press_action", "off_double_press_action"} {
		before, present := supplied.Lookup(name)
		input, exists := inputs[name]
		if !present || !exists || input.Selector() != parser.SelectorAction || input.Origin() != parser.InputOriginInvocation || len(before.Items()) != 1 || nha038Signature(before) != nha038Signature(input.Value()) {
			t.Fatalf("complete action input changed: %s", name)
		}
	}
	for name, want := range map[string]string{"helper_hold_delay": "250", "helper_max_loops": "20", "helper_double_press_delay": "250"} {
		input := inputs[name]
		value, ok := input.Value().Scalar()
		if !ok || value.Text() != want || input.Selector() != parser.SelectorNumber || input.Origin() != parser.InputOriginDefault {
			t.Fatalf("nested helper default changed: %s", name)
		}
	}
	for _, name := range []string{"on_double_press_exposed", "off_double_press_exposed"} {
		input := inputs[name]
		value, ok := input.Value().Scalar()
		if !ok || value.Kind() != parser.ScalarBool || value.Text() != "true" || input.Origin() != parser.InputOriginInvocation {
			t.Fatalf("double-press boolean changed: %s", name)
		}
	}
	for name, want := range map[string]string{"on_press_action": "1", "off_press_action": "2", "on_hold_action": "1", "off_hold_action": "1", "on_double_press_action": "2", "off_double_press_action": "10"} {
		data, _ := inputs[name].Value().Items()[0].Lookup("data")
		transition, _ := data.Lookup("transition")
		scalar, ok := transition.Scalar()
		if !ok || scalar.Text() != want {
			t.Fatalf("transition changed: %s", name)
		}
	}
	for name, want := range map[string]string{"on_hold_action": "15", "off_hold_action": "-15"} {
		data, _ := inputs[name].Value().Items()[0].Lookup("data")
		step, _ := data.Lookup("brightness_step_pct")
		scalar, ok := step.Scalar()
		if !ok || scalar.Text() != want {
			t.Fatalf("relative brightness changed: %s", name)
		}
	}
	triggers, _ := result.Root().Lookup("triggers")
	if len(triggers.Items()) != 26 {
		t.Fatalf("trigger count=%d", len(triggers.Items()))
	}
	references := 0
	nha038Walk(definition.Root(), func(n parser.Node) {
		if n.Kind() == parser.KindInputRef {
			references++
		}
	})
	sites := make(map[parser.Position]bool)
	nha038Walk(result.Root(), func(n parser.Node) {
		if n.Kind() == parser.KindInputRef {
			t.Fatalf("unresolved input at %s", n.Path())
		}
		for _, sub := range n.Substitutions() {
			sites[sub.At()] = true
		}
	})
	if references != 64 || len(sites) != 64 {
		t.Fatalf("substitution coverage references=%d distinct_output_sites=%d", references, len(sites))
	}
	nha038VerifyExpansion(t, definition.Root(), result.Root(), inputs, true)
	// Qualify the observed default/selector mismatch on the real definition:
	// a valid supplied device overrides [], while omission must reject it.
	privateFiles := []parser.SourceFile{nha038Source(t, "invocation.yaml", invocationBytes), nha038Source(t, definitionPath, definitionBytes)}
	for _, check := range []struct {
		name, input, replacement string
		code                     parser.Code
	}{
		{"chosen invalid remote default", "remote_device", "", parser.CodeBlueprintInputType},
		{"double press boolean as string", "on_double_press_exposed", "'true'", parser.CodeBlueprintInputType},
		{"action sequence as null", "on_press_action", "null", parser.CodeBlueprintInputType},
	} {
		t.Run(check.name, func(t *testing.T) {
			changed := append([]parser.SourceFile(nil), privateFiles...)
			changed[0] = nha038Source(t, "invocation.yaml", nha038InventoryInput(t, invocationBytes, check.input, check.replacement))
			diagnostic := nha038Rejected(t, changed, check.code, check.input)
			if check.replacement == "" && diagnostic.At.File != definitionPath {
				t.Fatal("invalid selected default was not located in the blueprint")
			}
		})
	}
	second, err := parser.ExpandBlueprint(bundle, "invocation.yaml")
	if err != nil || !reflect.DeepEqual(result, second) {
		t.Fatalf("observed expansion not deterministic: %v", err)
	}
	// Restore from new detached SourceFiles after deleting only a temporary
	// copy. The durable private source bundle and conversion manifest survive.
	temporary := t.TempDir()
	snapshots := result.Sources()
	persisted := make([]parser.SourceFile, len(snapshots))
	for i, file := range snapshots {
		write(t, temporary, file.Path(), string(file.Bytes()))
		persisted[i] = nha038Source(t, file.Path(), file.Bytes())
	}
	copied, err := parser.ExpandBlueprint(temporary, "invocation.yaml")
	if err != nil || !reflect.DeepEqual(result, copied) {
		t.Fatalf("copied observed expansion differs: %v", err)
	}
	if err := os.RemoveAll(temporary); err != nil {
		t.Fatal(err)
	}
	restored, err := parser.ExpandBlueprintSources("invocation.yaml", persisted)
	if err != nil || !reflect.DeepEqual(result, restored) {
		t.Fatalf("observed restoration differs: %v", err)
	}
	changed := result.Inputs()
	changed[0] = parser.EffectiveInput{}
	sourceCopies := result.Sources()
	sourceCopies[0] = parser.SourceFile{}
	nha038Walk(result.Root(), func(n parser.Node) {
		if entries := n.Entries(); len(entries) > 0 {
			entries[0].Value = parser.Node{}
		}
		if items := n.Items(); len(items) > 0 {
			items[0] = parser.Node{}
		}
		if subs := n.Substitutions(); len(subs) > 0 {
			subs[0] = parser.Substitution{}
		}
	})
	if !reflect.DeepEqual(result, restored) {
		t.Fatal("mutating observed accessor results changed artifact")
	}
	t.Logf("IKEA complete: 12 effective inputs, 26 triggers, 64 substitution sites, all six full action sequences preserved; inputs_sha256=%x definition_sources_sha256=%x", result.InputsSHA256(), result.DefinitionSourcesSHA256())
}

// Change one fixture input block by indentation, without reconstructing the
// remaining invocation or publishing any third-party definition bytes.
func nha038InventoryInput(t *testing.T, data []byte, name, replacement string) []byte {
	t.Helper()
	lines := strings.SplitAfter(string(data), "\n")
	start, end := -1, len(lines)
	for i, line := range lines {
		if strings.HasPrefix(line, "    "+name+":") {
			start = i
			continue
		}
		if start >= 0 && len(line) > 4 && (!strings.HasPrefix(line, " ") || (strings.HasPrefix(line, "    ") && line[4] != ' ' && line[4] != '-')) {
			end = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("inventory input %q absent", name)
	}
	out := strings.Join(lines[:start], "")
	if replacement != "" {
		out += "    " + name + ": " + replacement + "\n"
	}
	return []byte(out + strings.Join(lines[end:], ""))
}
