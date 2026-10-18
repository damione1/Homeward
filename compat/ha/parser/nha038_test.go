package parser_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
)

const nha038DefinitionPath = "blueprints/automation/homeward/laboratory.yaml"

func nha038Source(t *testing.T, path string, data []byte) parser.SourceFile {
	t.Helper()
	file, err := parser.NewSourceFile(path, data)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
func nha038Read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func nha038OriginalFiles(t *testing.T) []parser.SourceFile {
	t.Helper()
	root := filepath.Join("..", "blueprint", "fixtures")
	return []parser.SourceFile{
		nha038Source(t, "invocation.yaml", nha038Read(t, filepath.Join(root, "invocation.yaml"))),
		nha038Source(t, nha038DefinitionPath, nha038Read(t, filepath.Join(root, nha038DefinitionPath))),
	}
}
func nha038Expand(t *testing.T, files []parser.SourceFile) *parser.ExpandedBlueprint {
	t.Helper()
	result, err := parser.ExpandBlueprintSources("invocation.yaml", files)
	if err != nil || result == nil || !result.Valid() || result.Profile() != parser.BlueprintExpandedProfile {
		t.Fatalf("expansion=%v error=%v", result, err)
	}
	return result
}
func nha038Walk(n parser.Node, visit func(parser.Node)) {
	visit(n)
	for _, entry := range n.Entries() {
		nha038Walk(entry.Value, visit)
	}
	for _, item := range n.Items() {
		nha038Walk(item, visit)
	}
}
func nha038Signature(n parser.Node) string {
	var result strings.Builder
	var walk func(parser.Node)
	walk = func(value parser.Node) {
		fmt.Fprintf(&result, "k%d(", value.Kind())
		if scalar, ok := value.Scalar(); ok {
			fmt.Fprintf(&result, "s%d:%q", scalar.Kind(), scalar.Text())
		}
		if secret, ok := value.SecretKey(); ok {
			fmt.Fprintf(&result, "secret:%q", secret)
		}
		if input, ok := value.InputName(); ok {
			fmt.Fprintf(&result, "input:%q", input.String())
		}
		for _, entry := range value.Entries() {
			fmt.Fprintf(&result, "key:%q", entry.Key)
			walk(entry.Value)
		}
		for _, item := range value.Items() {
			walk(item)
		}
		result.WriteByte(')')
	}
	walk(n)
	return result.String()
}
func nha038Inputs(result *parser.ExpandedBlueprint) map[string]parser.EffectiveInput {
	inputs := make(map[string]parser.EffectiveInput)
	for _, input := range result.Inputs() {
		inputs[input.Name().String()] = input
	}
	return inputs
}

// Compare every original body node and every entire injected sequence, rather
// than accepting a few output substrings as evidence of full preservation.
func nha038VerifyExpansion(t *testing.T, definition, actual parser.Node, inputs map[string]parser.EffectiveInput, root bool) {
	t.Helper()
	if name, ok := definition.InputName(); ok {
		input, present := inputs[name.String()]
		if !present {
			t.Fatalf("missing effective input %s", name.String())
		}
		if nha038Signature(actual) != nha038Signature(input.Value()) {
			t.Fatalf("input %s sequence/value changed at %s", name.String(), actual.Path())
		}
		nha038Walk(actual, func(descendant parser.Node) {
			substitutions := descendant.Substitutions()
			if len(substitutions) != 1 {
				t.Fatalf("missing descendant provenance at %s", descendant.Path())
			}
			sub := substitutions[0]
			if sub.Input() != name || sub.At() != definition.Source() || sub.Origin() != input.Origin() || sub.Declaration() != input.Declaration() || sub.ValueSource() != input.Value().Source() || descendant.Source().File != input.Value().Source().File {
				t.Fatalf("incorrect substitution at %s: %+v", descendant.Path(), sub)
			}
		})
		return
	}
	if definition.Kind() != actual.Kind() {
		t.Fatalf("shape lost at %s", definition.Path())
	}
	if definition.Kind() == parser.KindScalar || definition.Kind() == parser.KindSecretRef {
		if nha038Signature(definition) != nha038Signature(actual) {
			t.Fatalf("scalar/template lost at %s", definition.Path())
		}
		return
	}
	if definition.Kind() == parser.KindSequence {
		expected, got := definition.Items(), actual.Items()
		if len(expected) != len(got) {
			t.Fatalf("sequence length changed at %s", definition.Path())
		}
		for i := range expected {
			nha038VerifyExpansion(t, expected[i], got[i], inputs, false)
		}
		return
	}
	expectedEntries := definition.Entries()
	gotEntries := actual.Entries()
	if !root && len(expectedEntries) != len(gotEntries) {
		t.Fatalf("mapping entries lost at %s", definition.Path())
	}
	for _, entry := range expectedEntries {
		if root && entry.Key == "blueprint" {
			continue
		}
		got, exists := actual.Lookup(entry.Key)
		if !exists {
			t.Fatalf("missing field %s", entry.Key)
		}
		nha038VerifyExpansion(t, entry.Value, got, inputs, false)
	}
}

func TestNHA038OriginalCompleteSyntaxAndPresence(t *testing.T) {
	files := nha038OriginalFiles(t)
	result := nha038Expand(t, files)
	definition, err := parser.LoadSources(nha038DefinitionPath, files[1:], parser.SyntaxProfileBlueprintDefinition)
	if err != nil {
		t.Fatal(err)
	}
	inputs := nha038Inputs(result)
	if len(inputs) != 13 {
		t.Fatalf("inputs=%d", len(inputs))
	}
	for name, want := range map[string]string{"tick": "0", "confirm": "false", "loops": "3", "cooldown": "10", "allow_report": "false"} {
		value, ok := inputs[name].Value().Scalar()
		if !ok || value.Text() != want {
			t.Fatalf("input %s=%v", name, inputs[name])
		}
	}
	if inputs["tick"].Origin() != parser.InputOriginInvocation || inputs["confirm"].Origin() != parser.InputOriginInvocation || inputs["loops"].Origin() != parser.InputOriginDefault || len(inputs["abort"].Value().Items()) != 0 {
		t.Fatal("presence/default/empty action changed")
	}
	nha038VerifyExpansion(t, definition.Root(), result.Root(), inputs, true)
	for _, name := range []string{"blueprint", "use_blueprint"} {
		if _, ok := result.Root().Lookup(name); ok {
			t.Fatalf("metadata leaked into body: %s", name)
		}
	}
	for _, name := range []string{"id", "alias", "description", "category"} {
		value, ok := result.Root().Lookup(name)
		if !ok || value.Source().File != "invocation.yaml" {
			t.Fatalf("invocation overlay lost: %s", name)
		}
	}
	nha038Walk(result.Root(), func(n parser.Node) {
		if n.Kind() == parser.KindInputRef || n.Path() == "" || n.Source().File == "" {
			t.Fatalf("unresolved or unsourced node %s", n.Path())
		}
	})
	if result.BlueprintSHA256() != files[1].SHA256() {
		t.Fatal("blueprint hash is not exact source hash")
	}
}

func TestNHA038DeterminismVariationAndUnusedBlueprint(t *testing.T) {
	files := nha038OriginalFiles(t)
	first := nha038Expand(t, files)
	second := nha038Expand(t, files)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same bytes changed expansion or provenance")
	}
	unused := nha038Source(t, "blueprints/automation/unused.yaml", []byte("not-even: [valid-blueprint]\n"))
	withUnused := nha038Expand(t, append(append([]parser.SourceFile(nil), files...), unused))
	if !reflect.DeepEqual(first, withUnused) || len(withUnused.Sources()) != 2 {
		t.Fatal("unused blueprint was parsed or added a result")
	}
	changed := append([]parser.SourceFile(nil), files...)
	changed[0] = nha038Source(t, "invocation.yaml", []byte(strings.Replace(string(files[0].Bytes()), "tick: 0", "tick: 1", 1)))
	third := nha038Expand(t, changed)
	if first.BlueprintSHA256() != third.BlueprintSHA256() || first.DefinitionSourcesSHA256() != third.DefinitionSourcesSHA256() || first.InputsSHA256() == third.InputsSHA256() {
		t.Fatal("incorrect hash variation")
	}
	var changedPaths []string
	var compare func(parser.Node, parser.Node)
	compare = func(a, b parser.Node) {
		if a.Kind() == parser.KindScalar {
			if nha038Signature(a) != nha038Signature(b) {
				changedPaths = append(changedPaths, a.Path())
			}
			return
		}
		ae, be := a.Entries(), b.Entries()
		for i := range ae {
			compare(ae[i].Value, be[i].Value)
		}
		ai, bi := a.Items(), b.Items()
		for i := range ai {
			compare(ai[i], bi[i])
		}
	}
	compare(first.Root(), third.Root())
	if len(changedPaths) != 1 || !strings.HasSuffix(changedPaths[0], ".timeout.milliseconds") {
		t.Fatalf("change was not localized: %v", changedPaths)
	}
}

func TestNHA038CopiesAndFilesystemFreeRestoration(t *testing.T) {
	files := nha038OriginalFiles(t)
	bundle := t.TempDir()
	for _, file := range files {
		write(t, bundle, file.Path(), string(file.Bytes()))
	}
	loaded, err := parser.ExpandBlueprint(bundle, "invocation.yaml")
	if err != nil {
		t.Fatal(err)
	}
	original := nha038Expand(t, files)
	if !reflect.DeepEqual(original, loaded) {
		t.Fatal("filesystem and detached loaders diverged")
	}
	snapshots := loaded.Sources()
	persisted := make([]parser.SourceFile, len(snapshots))
	for i, file := range snapshots {
		persisted[i] = nha038Source(t, file.Path(), file.Bytes())
		bytes := file.Bytes()
		bytes[0] = 'X'
		if file.SHA256() != sha256.Sum256(file.Bytes()) {
			t.Fatal("source bytes mutated")
		}
	}
	if err := os.RemoveAll(bundle); err != nil {
		t.Fatal(err)
	}
	restored := nha038Expand(t, persisted)
	if !reflect.DeepEqual(loaded, restored) {
		t.Fatal("restoration changed hashes, provenance or syntax")
	}
	inputs := loaded.Inputs()
	inputs[0] = parser.EffectiveInput{}
	sources := loaded.Sources()
	sources[0] = parser.SourceFile{}
	entries := loaded.Root().Entries()
	entries[0].Value = parser.Node{}
	nha038Walk(loaded.Root(), func(n parser.Node) {
		if subs := n.Substitutions(); len(subs) > 0 {
			refs := subs[0].ReferenceChain()
			if len(refs) > 0 {
				refs[0].At.File = "changed"
			}
			subs[0] = parser.Substitution{}
		}
		if items := n.Items(); len(items) > 0 {
			items[0] = parser.Node{}
		}
	})
	if !reflect.DeepEqual(loaded, restored) {
		t.Fatal("accessor copies mutated expansion")
	}
}

func nha038SmallDefinition(inputDefinitions, body string) string {
	return "blueprint:\n  name: Original negative-case fixture\n  domain: automation\n  input:\n" + inputDefinitions + body
}
func nha038SmallFiles(t *testing.T, definition, values string) []parser.SourceFile {
	t.Helper()
	invocation := "use_blueprint:\n  path: homeward/laboratory.yaml\n"
	if values != "" {
		invocation += "  input:\n" + values
	}
	return []parser.SourceFile{nha038Source(t, "invocation.yaml", []byte(invocation)), nha038Source(t, nha038DefinitionPath, []byte(definition))}
}
func nha038Rejected(t *testing.T, files []parser.SourceFile, want parser.Code, name string) *parser.Diagnostic {
	t.Helper()
	result, err := parser.ExpandBlueprintSources("invocation.yaml", files)
	var diagnostic *parser.Diagnostic
	if result != nil || !errors.As(err, &diagnostic) || diagnostic.Code != want || diagnostic.At.File == "" || diagnostic.At.Line <= 0 || diagnostic.At.Column <= 0 || diagnostic.Path == "" || (name != "" && !strings.Contains(diagnostic.Message, name)) {
		t.Fatalf("expansion=%v error=%v; want %s input=%s", result, err, want.String(), name)
	}
	return diagnostic
}

func TestNHA038RefusalsAreSourcedAndAtomic(t *testing.T) {
	leaf := func(selector, extra string) string {
		return "    value:\n" + extra + "      selector:\n        " + selector + ": {}\n"
	}
	body := "actions: []\n"
	cases := []struct {
		name, defs, values, body string
		code                     parser.Code
		input                    string
	}{
		{"required absent", leaf("device", ""), "", body, parser.CodeBlueprintInputMissing, "value"},
		{"invalid chosen device default", leaf("device", "      default: []\n"), "", body, parser.CodeBlueprintInputType, "value"},
		{"device null", leaf("device", ""), "    value: null\n", body, parser.CodeBlueprintInputType, "value"},
		{"empty device", leaf("device", ""), "    value: ''\n", body, parser.CodeBlueprintInputType, "value"},
		{"boolean string", leaf("boolean", ""), "    value: 'false'\n", body, parser.CodeBlueprintInputType, "value"},
		{"boolean zero", leaf("boolean", ""), "    value: 0\n", body, parser.CodeBlueprintInputType, "value"},
		{"boolean null", leaf("boolean", ""), "    value: null\n", body, parser.CodeBlueprintInputType, "value"},
		{"action mapping", leaf("action", ""), "    value: {action: example.record}\n", body, parser.CodeBlueprintInputType, "value"},
		{"action scalar", leaf("action", ""), "    value: example.record\n", body, parser.CodeBlueprintInputType, "value"},
		{"action null", leaf("action", ""), "    value: null\n", body, parser.CodeBlueprintInputType, "value"},
		{"number string", leaf("number", ""), "    value: '0'\n", body, parser.CodeBlueprintInputType, "value"},
		{"number nonfinite", leaf("number", ""), "    value: .inf\n", body, parser.CodeBlueprintInputType, "value"},
		{"unknown provided input", leaf("boolean", "      default: false\n"), "    unknown: true\n", body, parser.CodeBlueprintInputUnknown, "unknown"},
		{"undeclared reference", leaf("boolean", "      default: false\n"), "", "variables:\n  missing: !input unknown\nactions: []\n", parser.CodeBlueprintInputUnknown, "unknown"},
		{"input reference cycle", "    a:\n      default: !input b\n      selector:\n        boolean: {}\n    b:\n      default: !input a\n      selector:\n        boolean: {}\n", "", body, parser.CodeBlueprintInputReference, "a"},
		{"collision across sections", "    first:\n      input:\n        value:\n          default: false\n          selector:\n            boolean: {}\n    second:\n      input:\n        value:\n          default: true\n          selector:\n            boolean: {}\n", "", body, parser.CodeBlueprintInputCollision, "value"},
		{"unknown action field", leaf("action", ""), "    value:\n    - invented: true\n", body, parser.CodeUnknownField, "value"},
		{"input incompatible with use shape", leaf("device", ""), "    value: synthetic-panel\n", "actions: !input value\n", parser.CodeUnsupportedForm, "value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nha038Rejected(t, nha038SmallFiles(t, nha038SmallDefinition(tc.defs, tc.body), tc.values), tc.code, tc.input)
		})
	}
}

func TestNHA038NumericBoundsAndSelectorConfiguration(t *testing.T) {
	definition := nha038SmallDefinition("    value:\n      default: 5\n      selector:\n        number:\n          min: 0\n          max: 10\n          step: 3\n          mode: slider\n", "variables:\n  measured: !input value\nactions: []\n")
	for _, value := range []string{"0", "10", "1.25"} {
		t.Run("accepted "+value, func(t *testing.T) {
			result := nha038Expand(t, nha038SmallFiles(t, definition, "    value: "+value+"\n"))
			scalar, _ := nha038Inputs(result)["value"].Value().Scalar()
			if scalar.Text() != value {
				t.Fatal("number was rounded to selector step")
			}
		})
	}
	for _, value := range []string{"-0.1", "11", ".nan", "true", "[]", "null"} {
		t.Run("rejected "+value, func(t *testing.T) {
			nha038Rejected(t, nha038SmallFiles(t, definition, "    value: "+value+"\n"), parser.CodeBlueprintInputType, "value")
		})
	}
	invalid := strings.Replace(definition, "max: 10", "max: -1", 1)
	nha038Rejected(t, nha038SmallFiles(t, invalid, ""), parser.CodeBlueprintDefinition, "value")
	invalid = strings.Replace(definition, "step: 3", "step: 0", 1)
	nha038Rejected(t, nha038SmallFiles(t, invalid, ""), parser.CodeBlueprintDefinition, "value")
	invalid = strings.Replace(definition, "          max: 10\n", "", 1)
	nha038Rejected(t, nha038SmallFiles(t, invalid, ""), parser.CodeBlueprintDefinition, "value")
}

func TestNHA038IncludesAliasesAndDefinitionSourceHash(t *testing.T) {
	definition := nha038SmallDefinition("    work:\n      default: !include defaults.yaml\n      selector:\n        action: {}\n", "variables:\n  first: &work !input work\n  second: *work\nactions:\n- sequence: !input work\n")
	files := nha038SmallFiles(t, definition, "")
	const defaultsPath = "blueprints/automation/homeward/defaults.yaml"
	files = append(files, nha038Source(t, defaultsPath, []byte("- action: example.record\n  data:\n    measurement: 4\n")))
	result := nha038Expand(t, files)
	variables, _ := result.Root().Lookup("variables")
	second, _ := variables.Lookup("second")
	if second.Source().File != defaultsPath || len(second.References()) != 1 || second.References()[0].Kind != parser.ReferenceInclude {
		t.Fatal("input value include provenance lost")
	}
	subs := second.Substitutions()
	if len(subs) != 1 || subs[0].Origin() != parser.InputOriginDefault || len(subs[0].ReferenceChain()) != 1 || subs[0].ReferenceChain()[0].Kind != parser.ReferenceAlias {
		t.Fatal("tag alias chain lost")
	}
	refs := subs[0].ReferenceChain()
	refs[0].At.File = "mutated"
	valueRefs := second.References()
	valueRefs[0].At.File = "mutated"
	reloaded := nha038Expand(t, files)
	if !reflect.DeepEqual(result, reloaded) {
		t.Fatal("provenance accessor exposed mutable backing slice")
	}
	files[2] = nha038Source(t, defaultsPath, []byte("- action: example.record\n  data:\n    measurement: 9\n"))
	changed := nha038Expand(t, files)
	if result.BlueprintSHA256() != changed.BlueprintSHA256() || result.DefinitionSourcesSHA256() == changed.DefinitionSourcesSHA256() || result.InputsSHA256() == changed.InputsSHA256() {
		t.Fatal("included source was not bound by definition/input hash")
	}
}

func TestNHA038LimitsCyclesConfinementAndHistoricalProfiles(t *testing.T) {
	base := nha038SmallFiles(t, nha038SmallDefinition("    value:\n      default: []\n      selector:\n        action: {}\n", "actions: !input value\n"), "")
	t.Run("alias cycle", func(t *testing.T) {
		files := append([]parser.SourceFile(nil), base...)
		files[1] = nha038Source(t, nha038DefinitionPath, []byte("blueprint: &loop {name: *loop}\n"))
		nha038Rejected(t, files, parser.CodeAliasCycle, "")
	})
	t.Run("include cycle", func(t *testing.T) {
		files := append([]parser.SourceFile(nil), base...)
		files[1] = nha038Source(t, nha038DefinitionPath, []byte("!include loop.yaml\n"))
		files = append(files, nha038Source(t, "blueprints/automation/homeward/loop.yaml", []byte("!include laboratory.yaml\n")))
		nha038Rejected(t, files, parser.CodeIncludeCycle, "")
	})
	t.Run("copy amplification", func(t *testing.T) {
		values := "    value:\n" + strings.Repeat("    - action: example.record\n", 300)
		body := "actions:\n" + strings.Repeat("- sequence: !input value\n", 100)
		files := nha038SmallFiles(t, nha038SmallDefinition("    value:\n      selector:\n        action: {}\n", body), values)
		nha038Rejected(t, files, parser.CodeSyntaxLimit, "")
	})
	for _, path := range []string{"../outside.yaml", "/outside.yaml", "https://example.test/a.yaml", "homeward/../laboratory.yaml"} {
		t.Run(path, func(t *testing.T) {
			files := append([]parser.SourceFile(nil), base...)
			files[0] = nha038Source(t, "invocation.yaml", []byte("use_blueprint:\n  path: "+path+"\n"))
			nha038Rejected(t, files, parser.CodeIncludeOutside, "")
		})
	}
	t.Run("historical grammars remain closed", func(t *testing.T) {
		for _, profile := range []parser.SyntaxProfile{parser.SyntaxProfileCore, parser.SyntaxProfileDeclarative} {
			source := nha038Source(t, "configuration.yaml", []byte("automation:\n- use_blueprint:\n    path: homeward/laboratory.yaml\n"))
			doc, err := parser.LoadSources("configuration.yaml", []parser.SourceFile{source}, profile)
			var diagnostic *parser.Diagnostic
			if doc != nil || !errors.As(err, &diagnostic) || diagnostic.Code != parser.CodeUnknownField {
				t.Fatalf("historical profile accepted blueprint: %v", err)
			}
			source = nha038Source(t, "configuration.yaml", []byte("automation: !input value\n"))
			doc, err = parser.LoadSources("configuration.yaml", []parser.SourceFile{source}, profile)
			if doc != nil || !errors.As(err, &diagnostic) || diagnostic.Code != parser.CodeUnsupportedTag {
				t.Fatalf("historical profile accepted tag: %v", err)
			}
		}
	})
	t.Run("symlink definition", func(t *testing.T) {
		bundle := t.TempDir()
		write(t, bundle, "invocation.yaml", string(base[0].Bytes()))
		outside := t.TempDir()
		write(t, outside, "outside.yaml", string(base[1].Bytes()))
		if err := os.MkdirAll(filepath.Join(bundle, "blueprints", "automation", "homeward"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "outside.yaml"), filepath.Join(bundle, nha038DefinitionPath)); err != nil {
			t.Fatal(err)
		}
		result, err := parser.ExpandBlueprint(bundle, "invocation.yaml")
		var diagnostic *parser.Diagnostic
		if result != nil || !errors.As(err, &diagnostic) || diagnostic.Code != parser.CodeIncludeOutside {
			t.Fatalf("symlink admitted: %v", err)
		}
	})
}

func TestNHA038ZeroValuesAndTypedErrors(t *testing.T) {
	var result parser.ExpandedBlueprint
	var nilResult *parser.ExpandedBlueprint
	var name parser.InputName
	if result.Valid() || result.Profile() != "" || nilResult.Valid() || nilResult.Profile() != "" || name.Valid() || parser.SelectorInvalid.String() != "" || parser.InputOriginInvalid.String() != "" {
		t.Fatal("zero value admitted")
	}
	files := nha038OriginalFiles(t)
	resultPointer, err := parser.ExpandBlueprintSources("invocation.yaml", files)
	if err != nil || resultPointer == nil {
		t.Fatalf("successful expansion returned non-nil error: %v", err)
	}
	if _, err := parser.ExpandBlueprintSources("invocation.yaml", []parser.SourceFile{{}}); err == nil {
		t.Fatal("zero source admitted")
	}
}
