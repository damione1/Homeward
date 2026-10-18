package bundle_test

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
)

func writeNHA028(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const validNHA028 = `group:
  upper:
    name: !secret group_display_name
    entities:
      - group.lower
      - input_boolean.ready
  lower:
    entities:
      - input_boolean.ready
input_boolean: !include parts/boolean.yaml
input_number:
  target:
    min: 0
    max: 20
    step: 0.5
    initial: 0
    mode: box
counter:
  visits:
    initial: 0
    step: 1
`

func TestNHA028TypedCandidateSourcesAndRestore(t *testing.T) {
	dir := t.TempDir()
	writeNHA028(t, dir, "configuration.yaml", validNHA028)
	writeNHA028(t, dir, "parts/boolean.yaml", "ready:\n  initial: false\n  name: Ready\n")
	c, err := bundle.Compile(dir, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := bundle.Compile(dir, "./configuration.yaml")
	if err != nil || canonical.Entry() != "configuration.yaml" || canonical.SHA256() != c.SHA256() {
		t.Fatalf("noncanonical entry changed identity: %v", err)
	}
	if !c.Valid() || c.Profile() != parser.SyntaxProfileDeclarative || c.Schema() != bundle.SchemaDeclarative1 {
		t.Fatalf("candidate identity: %v %v %v", c.Valid(), c.Profile(), c.Schema())
	}
	initial, present := c.Booleans()[0].Initial()
	if !present || initial {
		t.Fatal("present false was not preserved")
	}
	num, present := c.Numbers()[0].Initial()
	value, valid := num.Float64()
	if !present || !valid || value != 0 || c.Numbers()[0].Mode() != bundle.NumberModeBox {
		t.Fatal("number initial zero or mode lost")
	}
	if count, present := c.Counters()[0].Initial(); !present || count != 0 {
		t.Fatal("counter zero lost")
	}
	groups := c.Groups()
	if len(groups) != 2 || len(groups[0].Members()) != 2 {
		t.Fatal("cross references not resolved")
	}
	if text, ok := groups[0].Name(); !ok || text.Kind() != bundle.TextSecret {
		t.Fatal("secret handle not typed")
	} else {
		ref, ok := text.Secret()
		key, valid := ref.Key()
		if !ok || !valid || key != "group_display_name" {
			t.Fatal("secret handle changed")
		}
	}
	if id, _ := groups[0].ID().Value(); strings.Contains(id, "display_name") {
		t.Fatal("secret leaked into identity")
	}
	sources := c.Sources()
	if len(sources) != 2 {
		t.Fatalf("sources = %d", len(sources))
	}
	for _, source := range sources {
		got := source.SHA256()
		if want := sha256.Sum256(source.Bytes()); got != want {
			t.Fatal("source hash does not cover exact bytes")
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, "parts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "configuration.yaml")); err != nil {
		t.Fatal(err)
	}
	restored, err := bundle.CompileSources("configuration.yaml", sources)
	if err != nil || !restored.Valid() || restored.SHA256() != c.SHA256() {
		t.Fatalf("restoration: %v", err)
	}
	reversed := []parser.SourceFile{sources[1], sources[0]}
	restored, err = bundle.CompileSources("./configuration.yaml", reversed)
	if err != nil || restored.SHA256() != c.SHA256() {
		t.Fatalf("source order changed manifest: %v", err)
	}
	copySources := c.Sources()
	copySources[0] = parser.SourceFile{}
	bytes := sources[0].Bytes()
	bytes[0] = 'X'
	copyGroups := c.Groups()
	copyGroups[0] = bundle.Group{}
	members := groups[0].Members()
	memberBefore, _ := members[0].Value()
	members[0] = members[1]
	memberAfter, _ := c.Groups()[0].Members()[0].Value()
	if len(c.Sources()) != 2 || c.Sources()[0].SHA256() != sha256.Sum256(c.Sources()[0].Bytes()) || len(c.Groups()[0].Members()) != 2 {
		t.Fatal("candidate mutated through returned values")
	}
	if memberAfter != memberBefore {
		t.Fatal("group member mutated through copy")
	}
	var zero bundle.Candidate
	if zero.Valid() || zero.Sources() != nil || zero.Schema() != bundle.SchemaInvalid {
		t.Fatal("zero candidate admitted")
	}
	localCopy := c
	localCopy = bundle.Candidate{}
	if localCopy.Valid() || !c.Valid() {
		t.Fatal("candidate value copy shares mutable admission")
	}
	if (bundle.BoolHelper{}).Valid() || (bundle.NumberHelper{}).Valid() || (bundle.CounterHelper{}).Valid() || (bundle.Group{}).Valid() || (bundle.Number{}).Valid() || (bundle.Text{}).Valid() || (bundle.SecretRef{}).Valid() {
		t.Fatal("zero public value admitted")
	}
}

func TestNHA028SourceHashesExactAndRepeatable(t *testing.T) {
	dir := t.TempDir()
	writeNHA028(t, dir, "configuration.yaml", "input_boolean:\n  a: !include parts/boolean.yaml\n  b: !include parts/boolean.yaml\n")
	writeNHA028(t, dir, "parts/boolean.yaml", "initial: false\n")
	first, err := bundle.Compile(dir, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	second, err := bundle.Compile(dir, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256() != second.SHA256() || len(first.Sources()) != 2 {
		t.Fatal("repeated include hash unstable or duplicated")
	}
	writeNHA028(t, dir, "parts/boolean.yaml", "initial: false\n# changed\n")
	third, err := bundle.Compile(dir, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256() == third.SHA256() {
		t.Fatal("include comment did not change manifest")
	}
	writeNHA028(t, dir, "configuration.yaml", "# changed\ninput_boolean:\n  a: !include parts/boolean.yaml\n  b: !include parts/boolean.yaml\n")
	fourth, err := bundle.Compile(dir, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if third.SHA256() == fourth.SHA256() {
		t.Fatal("entry comment did not change manifest")
	}
}

func TestNHA028AdmittedLexemesAndAbsentInitial(t *testing.T) {
	dir := t.TempDir()
	writeNHA028(t, dir, "configuration.yaml", "input_boolean:\n  flag:\n    initial: !!bool TRUE\n  unset: {}\ninput_number:\n  n:\n    min: !!int -2\n    max: !!float 2.5e1\n    step: !!float 0.001\ncounter:\n  c:\n    step: !!int 1\n")
	c, err := bundle.Compile(dir, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := c.Booleans()[0].Initial(); !ok || !v {
		t.Fatal("explicit YAML boolean was lost")
	}
	if _, ok := c.Booleans()[1].Initial(); ok {
		t.Fatal("absent boolean initial became false")
	}
	if _, ok := c.Numbers()[0].Initial(); ok {
		t.Fatal("absent number initial became zero")
	}
	if _, ok := c.Counters()[0].Initial(); ok {
		t.Fatal("absent counter initial became zero")
	}
	if v, ok := c.Numbers()[0].Min().Float64(); !ok || v != -2 {
		t.Fatal("decimal integer was not admitted")
	}
	if v, ok := c.Numbers()[0].Max().Float64(); !ok || v != 25 {
		t.Fatal("decimal exponent was not admitted")
	}
}

func TestNHA028RefusalsWithSource(t *testing.T) {
	cases := []struct {
		name, yaml string
		code       bundle.Code
		parserCode parser.Code
		line       int
	}{
		{"unknown field", "input_boolean:\n  a:\n    bogus: true\n", 0, parser.CodeUnknownField, 3},
		{"unknown mode", "input_number:\n  a:\n    min: 0\n    max: 1\n    mode: wheel\n", bundle.CodeInvalidValue, 0, 5},
		{"zero step", "input_number:\n  a:\n    min: 0\n    max: 1\n    step: 0\n", bundle.CodeInvalidValue, 0, 5},
		{"counter zero step", "counter:\n  a:\n    step: 0\n", bundle.CodeInvalidValue, 0, 3},
		{"missing bound", "input_number:\n  a:\n    min: 0\n", bundle.CodeMissingField, 0, 2},
		{"missing reference", "group:\n  a:\n    entities:\n      - input_boolean.missing\n", bundle.CodeMissingReference, 0, 4},
		{"unsupported target", "group:\n  a:\n    entities:\n      - light.lamp\n", bundle.CodeMissingReference, 0, 4},
		{"cycle", "group:\n  a:\n    entities:\n      - group.b\n  b:\n    entities:\n      - group.a\n", bundle.CodeReferenceCycle, 0, 7},
		{"bad integer lexeme", "counter:\n  a:\n    step: !!int nope\n", bundle.CodeInvalidValue, 0, 3},
		{"bad integer number", "input_number:\n  a:\n    min: !!int 1.5\n    max: 2\n", bundle.CodeInvalidValue, 0, 3},
		{"bad exponent integer", "input_number:\n  a:\n    min: !!int 1e2\n    max: 200\n", bundle.CodeInvalidValue, 0, 3},
		{"ambiguous integer", "input_number:\n  a:\n    min: !!int 012\n    max: 20\n", bundle.CodeInvalidValue, 0, 3},
		{"bad boolean lexeme", "input_boolean:\n  a:\n    initial: !!bool t\n", bundle.CodeInvalidValue, 0, 3},
		{"bad numeric boolean", "input_boolean:\n  a:\n    initial: !!bool 1\n", bundle.CodeInvalidValue, 0, 3},
		{"bad legacy boolean", "input_boolean:\n  a:\n    initial: !!bool on\n", bundle.CodeInvalidValue, 0, 3},
		{"bad float lexeme", "input_number:\n  a:\n    min: !!float nope\n    max: 1\n", bundle.CodeInvalidValue, 0, 3},
		{"hex float lexeme", "input_number:\n  a:\n    min: !!float 0x1p2\n    max: 5\n", bundle.CodeInvalidValue, 0, 3},
		{"separator float lexeme", "input_number:\n  a:\n    min: !!float 1_0.0\n    max: 20\n", bundle.CodeInvalidValue, 0, 3},
		{"nonfinite", "input_number:\n  a:\n    min: !!float .nan\n    max: 1\n", bundle.CodeInvalidValue, 0, 3},
		{"secret numeric", "input_number:\n  a:\n    min: !secret private_min\n    max: 1\n", bundle.CodeInvalidValue, 0, 3},
		{"secret unit", "input_number:\n  a:\n    min: 0\n    max: 1\n    unit_of_measurement: !secret private_unit\n", bundle.CodeInvalidValue, 0, 5},
		{"program pending", "script:\n  a:\n    sequence: []\n", bundle.CodeUnsupportedCategory, 0, 1},
		{"automation pending", "automation: []\n", bundle.CodeUnsupportedCategory, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeNHA028(t, dir, "configuration.yaml", tc.yaml)
			c, err := bundle.Compile(dir, "configuration.yaml")
			if c.Valid() || err == nil {
				t.Fatalf("accepted invalid config: %v", err)
			}
			if tc.parserCode != 0 {
				var d *parser.Diagnostic
				if !errors.As(err, &d) || d.Code != tc.parserCode || d.At.File != "configuration.yaml" || d.At.Line != tc.line {
					t.Fatalf("parser diagnostic: %v", err)
				}
			} else {
				var d *bundle.Diagnostic
				if !errors.As(err, &d) || d.Code != tc.code || d.At.File != "configuration.yaml" || d.At.Line != tc.line {
					t.Fatalf("bundle diagnostic: %v", err)
				}
			}
			if strings.Contains(err.Error(), "private_min") || strings.Contains(err.Error(), "private_unit") {
				t.Fatal("secret handle leaked to diagnostic")
			}
		})
	}
}

func TestNHA028IncludeEscapeFailsBeforeOutsideRead(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "bundle")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeNHA028(t, parent, "secret.yaml", "unreadable: !secret actual_value\n")
	writeNHA028(t, root, "configuration.yaml", "input_boolean: !include ../secret.yaml\n")
	_, err := bundle.Compile(root, "configuration.yaml")
	var d *parser.Diagnostic
	if !errors.As(err, &d) || d.Code != parser.CodeIncludeOutside || d.At.File != "configuration.yaml" || d.At.Line != 1 {
		t.Fatalf("include escape: %v", err)
	}
	if strings.Contains(err.Error(), "actual_value") {
		t.Fatal("outside source leaked")
	}
}
