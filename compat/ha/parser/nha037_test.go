package parser_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T, dir, name string) *parser.Document {
	t.Helper()
	doc, d := parser.Load(dir, name)
	if d != nil {
		t.Fatal(d)
	}
	return doc
}

func reject(t *testing.T, dir, name string, code parser.Code, file string, line int) *parser.Diagnostic {
	t.Helper()
	doc, err := parser.Load(dir, name)
	var d *parser.Diagnostic
	if doc != nil || !errors.As(err, &d) || d.Code != code || d.At.File != file || d.At.Line != line {
		t.Fatalf("Load(%q) = doc %v, diagnostic %v; want %s %s:%d", name, doc, d, code, file, line)
	}
	if !strings.HasPrefix(d.Code.String(), "HA_") {
		t.Fatalf("unstable diagnostic code %q", d.Code.String())
	}
	return d
}

func walk(t *testing.T, n parser.Node) {
	t.Helper()
	if n.Source().File == "" || n.Source().Line <= 0 || n.Source().Column <= 0 || n.Path() == "" {
		t.Fatalf("missing provenance: %+v %s", n.Source(), n.Path())
	}
	for _, e := range n.Entries() {
		if e.KeySource.Line <= 0 {
			t.Fatalf("missing key source: %s", e.Key)
		}
		walk(t, e.Value)
	}
	for _, item := range n.Items() {
		walk(t, item)
	}
}

func TestNHA037CorpusPositions(t *testing.T) {
	dir := filepath.Join("..", "fixtures")
	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []string `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Fixtures) != 10 {
		t.Fatalf("corpus has %d fixtures, want ten", len(index.Fixtures))
	}
	for _, name := range index.Fixtures {
		t.Run(name, func(t *testing.T) {
			doc := mustLoad(t, dir, name+".yaml")
			if doc.Profile() != parser.Profile {
				t.Fatalf("profile %q", doc.Profile())
			}
			walk(t, doc.Root())
		})
	}
}

func TestNHA037LoadErrorInterface(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "configuration.yaml", "automation: []\n")
	loadAsError := func() error {
		_, err := parser.Load(dir, "configuration.yaml")
		return err
	}
	if err := loadAsError(); err != nil {
		t.Fatalf("successful load returned typed nil error: %v", err)
	}
}

func TestNHA037IncludeAliasProvenanceAndCopies(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "configuration.yaml", "automation: !include parts/automation.yaml\n")
	write(t, dir, "parts/automation.yaml", "- id: test\n  triggers:\n    - &event\n      trigger: event\n      event_type: synthetic\n    - *event\n  actions:\n    - action: example.fire\n      data:\n        arbitrary_future_field: kept\n")
	doc := mustLoad(t, dir, "configuration.yaml")
	auto, _ := doc.Root().Lookup("automation")
	if auto.Source().File != "parts/automation.yaml" || auto.Source().Line != 1 || len(auto.References()) != 1 || auto.References()[0].At.Line != 1 {
		t.Fatalf("include provenance: %+v %+v", auto.Source(), auto.References())
	}
	triggers, _ := auto.Items()[0].Lookup("triggers")
	second := triggers.Items()[1]
	if second.Source().File != "parts/automation.yaml" || second.Source().Line != 3 || second.References()[1].Kind != parser.ReferenceAlias || second.References()[1].At.Line != 6 {
		t.Fatalf("alias provenance: %+v %+v", second.Source(), second.References())
	}
	items := triggers.Items()
	items[0] = parser.Node{}
	rootCopy := doc.Root()
	rootCopy = parser.Node{}
	_ = rootCopy
	refs := second.References()
	refs[0].At.File = "changed"
	entries := second.Entries()
	entries[0].Key = "changed"
	entries[0].Value = parser.Node{}
	if doc.Root().Kind() != parser.KindMapping || triggers.Items()[0].Kind() == parser.KindInvalid || second.References()[0].At.File != "configuration.yaml" || second.Entries()[0].Key != "trigger" {
		t.Fatal("returned copies mutated admitted syntax")
	}
	actions, _ := auto.Items()[0].Lookup("actions")
	data, _ := actions.Items()[0].Lookup("data")
	if kept, ok := data.Lookup("arbitrary_future_field"); !ok || kept.Source().Line != 10 {
		t.Fatal("open service data was lost")
	}
}

func TestNHA037UnsafeIncludesAndCycles(t *testing.T) {
	t.Run("parent traversal", func(t *testing.T) {
		parent := t.TempDir()
		bundle := filepath.Join(parent, "bundle")
		if err := os.Mkdir(bundle, 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, parent, "outside.yaml", "automation: []\n")
		write(t, bundle, "configuration.yaml", "automation: !include ../outside.yaml\n")
		d := reject(t, bundle, "configuration.yaml", parser.CodeIncludeOutside, "configuration.yaml", 1)
		if d.Path != "$.automation" {
			t.Fatal(d.Path)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		parent := t.TempDir()
		bundle := filepath.Join(parent, "bundle")
		if err := os.Mkdir(bundle, 0o700); err != nil {
			t.Fatal(err)
		}
		write(t, parent, "outside.yaml", "automation: []\n")
		if err := os.Symlink(filepath.Join(parent, "outside.yaml"), filepath.Join(bundle, "link.yaml")); err != nil {
			t.Fatal(err)
		}
		write(t, bundle, "configuration.yaml", "automation: !include link.yaml\n")
		reject(t, bundle, "configuration.yaml", parser.CodeIncludeOutside, "configuration.yaml", 1)
	})
	t.Run("cycle", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "configuration.yaml", "automation: !include a.yaml\n")
		write(t, dir, "a.yaml", "!include configuration.yaml\n")
		reject(t, dir, "configuration.yaml", parser.CodeIncludeCycle, "a.yaml", 1)
	})
	t.Run("nested sibling", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "configuration.yaml", "automation: !include parts/a.yaml\n")
		write(t, dir, "parts/a.yaml", "!include ../shared/automations.yaml\n")
		write(t, dir, "shared/automations.yaml", "[]\n")
		mustLoad(t, dir, "configuration.yaml")
	})
	t.Run("absolute", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "configuration.yaml", "automation: !include /tmp/external.yaml\n")
		reject(t, dir, "configuration.yaml", parser.CodeIncludeOutside, "configuration.yaml", 1)
	})
	t.Run("diagnostic in included file", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "configuration.yaml", "automation: !include parts/a.yaml\n")
		write(t, dir, "parts/a.yaml", "- id: synthetic\n  mystery: true\n")
		reject(t, dir, "configuration.yaml", parser.CodeUnknownField, "parts/a.yaml", 2)
	})
}

func TestNHA037Diagnostics(t *testing.T) {
	tests := []struct {
		name, body string
		code       parser.Code
		line       int
	}{
		{"unknown root", "automation: []\nunknown: true\n", parser.CodeUnknownField, 2},
		{"unknown nested", "automation:\n  - id: x\n    mystery: true\n", parser.CodeUnknownField, 3},
		{"duplicate", "automation: []\nautomation: []\n", parser.CodeDuplicateKey, 2},
		{"unsupported include", "automation: !include_dir_list parts\n", parser.CodeUnsupportedTag, 1},
		{"unsupported trigger", "automation:\n  - triggers:\n      - trigger: sun\n", parser.CodeUnsupportedForm, 3},
		{"non-string trigger", "automation:\n  - triggers:\n      - trigger: !!int event\n", parser.CodeUnsupportedForm, 3},
		{"non-string mode", "automation:\n  - mode: !!bool single\n", parser.CodeUnsupportedForm, 2},
		{"secret mode", "automation:\n  - mode: !secret mode_name\n", parser.CodeUnsupportedForm, 2},
		{"malformed", "automation:\n  - id: [\n", parser.CodeYAML, 2},
		{"alias cycle", "automation: &a [*a]\n", parser.CodeAliasCycle, 1},
		{"multiple docs", "automation: []\n---\nautomation: []\n", parser.CodeMultipleDocs, 2},
		{"missing include", "automation: !include missing.yaml\n", parser.CodeReadFile, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "configuration.yaml", tc.body)
			reject(t, dir, "configuration.yaml", tc.code, "configuration.yaml", tc.line)
		})
	}
}

func TestNHA037LimitsAndOpaqueSecret(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "configuration.yaml", "automation: !include chain/0.yaml\n")
	for i := 0; i < 9; i++ {
		name := string(rune('0' + i))
		next := string(rune('1' + i))
		write(t, dir, "chain/"+name+".yaml", "!include "+next+".yaml\n")
	}
	reject(t, dir, "configuration.yaml", parser.CodeIncludeDepth, "chain/7.yaml", 1)
	large := t.TempDir()
	write(t, large, "configuration.yaml", "automation: !include large.yaml\n")
	write(t, large, "large.yaml", "#"+strings.Repeat("a", 1<<20)+"\n")
	reject(t, large, "configuration.yaml", parser.CodeSizeLimit, "configuration.yaml", 1)
	secretDir := t.TempDir()
	write(t, secretDir, "configuration.yaml", "automation:\n  - id: test\n    actions:\n      - action: example.fire\n        data:\n          token: !secret synthetic_reference\n")
	doc := mustLoad(t, secretDir, "configuration.yaml")
	auto, _ := doc.Root().Lookup("automation")
	actions, _ := auto.Items()[0].Lookup("actions")
	data, _ := actions.Items()[0].Lookup("data")
	secret, _ := data.Lookup("token")
	if key, ok := secret.SecretKey(); !ok || key != "synthetic_reference" {
		t.Fatal("secret reference not preserved")
	}
	if _, ok := secret.Scalar(); ok {
		t.Fatal("secret exposed as plain scalar")
	}
}

func TestNHA037AggregateBudgetAndNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	var root strings.Builder
	root.WriteString("script:\n")
	for i := 0; i < 5; i++ {
		name := string(rune('a' + i))
		root.WriteString("  " + name + ": !include " + name + ".yaml\n")
		write(t, dir, name+".yaml", "sequence: []\n#"+strings.Repeat("x", 900_000)+"\n")
	}
	write(t, dir, "configuration.yaml", root.String())
	reject(t, dir, "configuration.yaml", parser.CodeSizeLimit, "configuration.yaml", 6)

	fifoDir := t.TempDir()
	write(t, fifoDir, "configuration.yaml", "automation: !include pipe.yaml\n")
	if err := syscall.Mkfifo(filepath.Join(fifoDir, "pipe.yaml"), 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	reject(t, fifoDir, "configuration.yaml", parser.CodeReadFile, "configuration.yaml", 1)
}

func TestNHA037AliasExpansionAndKeyLimit(t *testing.T) {
	dir := t.TempDir()
	var body strings.Builder
	body.WriteString("automation:\n  - actions:\n      - data:\n")
	body.WriteString("          k0: &a0 value\n")
	for i := 1; i <= 17; i++ {
		body.WriteString(fmt.Sprintf("          k%d: &a%d [*a%d]\n", i, i, i-1))
	}
	write(t, dir, "configuration.yaml", body.String())
	doc, err := parser.Load(dir, "configuration.yaml")
	var d *parser.Diagnostic
	if doc != nil || !errors.As(err, &d) || d.Code != parser.CodeAliasLimit {
		t.Fatalf("alias budget: %v", err)
	}

	keyDir := t.TempDir()
	write(t, keyDir, "configuration.yaml", "script:\n  "+strings.Repeat("x", 257)+":\n    sequence: []\n")
	reject(t, keyDir, "configuration.yaml", parser.CodeSyntaxLimit, "configuration.yaml", 2)

	bombDir := t.TempDir()
	var bomb strings.Builder
	bomb.WriteString("automation:\n  - actions:\n      - data:\n          k0: &b0 [x]\n")
	for i := 1; i <= 16; i++ {
		bomb.WriteString(fmt.Sprintf("          k%d: &b%d [*b%d, *b%d]\n", i, i, i-1, i-1))
	}
	write(t, bombDir, "configuration.yaml", bomb.String())
	_, err = parser.Load(bombDir, "configuration.yaml")
	if !errors.As(err, &d) || d.Code != parser.CodeSyntaxLimit {
		t.Fatalf("expanded alias budget was not enforced: %v", err)
	}
}

func TestNHA037CombinedSyntaxDepthAndPathLimits(t *testing.T) {
	rootYAML := "automation:\n  - actions:\n      - data: !include nested.yaml\n"
	anchored := func(levels int) string {
		var b strings.Builder
		b.WriteString("base: &base\n")
		for i := 0; i < levels; i++ {
			b.WriteString(strings.Repeat("  ", i+1) + "k:\n")
		}
		b.WriteString(strings.Repeat("  ", levels+1) + "leaf: ok\ncopy: *base\n")
		return b.String()
	}
	for _, tc := range []struct {
		name   string
		levels int
		want   parser.Code
	}{
		{"depth below limit through include and alias", 53, parser.CodeInvalid},
		{"depth above limit through include and alias", 56, parser.CodeSyntaxLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "configuration.yaml", rootYAML)
			write(t, dir, "nested.yaml", anchored(tc.levels))
			if tc.want == parser.CodeInvalid {
				mustLoad(t, dir, "configuration.yaml")
			} else {
				doc, err := parser.Load(dir, "configuration.yaml")
				var d *parser.Diagnostic
				if doc != nil || !errors.As(err, &d) || d.Code != tc.want || d.At.File != "nested.yaml" || d.At.Line == 0 {
					t.Fatalf("combined depth limit: doc %v, err %v", doc, err)
				}
			}
		})
	}
	longPath := func(levels int) string {
		var b strings.Builder
		key := strings.Repeat("p", 240)
		for i := 0; i < levels; i++ {
			b.WriteString(strings.Repeat("  ", i) + key + ":\n")
		}
		b.WriteString(strings.Repeat("  ", levels) + "leaf: ok\n")
		return b.String()
	}
	for _, tc := range []struct {
		name   string
		levels int
		want   parser.Code
	}{
		{"path below limit", 16, parser.CodeInvalid},
		{"path above limit", 17, parser.CodeSyntaxLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "configuration.yaml", rootYAML)
			write(t, dir, "nested.yaml", longPath(tc.levels))
			if tc.want == parser.CodeInvalid {
				mustLoad(t, dir, "configuration.yaml")
			} else {
				doc, err := parser.Load(dir, "configuration.yaml")
				var d *parser.Diagnostic
				if doc != nil || !errors.As(err, &d) || d.Code != tc.want || d.At.File != "nested.yaml" || d.At.Line != 17 {
					t.Fatalf("path limit: doc %v, err %v", doc, err)
				}
			}
		})
	}
}
