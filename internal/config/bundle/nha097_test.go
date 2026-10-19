package bundle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
)

func nha097Source(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "nha097", "configuration.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func nha097CompileText(t *testing.T, text string) (bundle.Candidate, error) {
	t.Helper()
	source, err := parser.NewSourceFile("configuration.yaml", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return bundle.CompileSourcesV2("configuration.yaml", []parser.SourceFile{source})
}

func TestNHA097Exact26NoDuplicateAcrossSources(t *testing.T) {
	candidate, err := bundle.Compile(filepath.Join("testdata", "nha097"), "configuration.yaml")
	if err == nil || candidate.Valid() {
		t.Fatal("v1 must not accept v2 declarations")
	}
	candidate, err = bundle.CompileV2(filepath.Join("testdata", "nha097"), "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.Valid() || candidate.Schema() != bundle.SchemaDeclarative2 || candidate.Profile() != parser.SyntaxProfileDeclarative2 {
		t.Fatal("v2 candidate not admitted")
	}
	if len(candidate.Booleans()) != 14 || len(candidate.Buttons()) != 4 || len(candidate.Datetimes()) != 4 || len(candidate.Selects()) != 2 || len(candidate.Texts()) != 1 || len(candidate.Counters()) != 1 || len(candidate.Numbers()) != 0 {
		t.Fatal("retained helper family counts drifted")
	}
	seen := map[domain.HAAlias]bool{}
	add := func(alias domain.HAAlias) {
		t.Helper()
		if seen[alias] {
			t.Fatalf("duplicate %v", alias)
		}
		seen[alias] = true
	}
	for _, item := range candidate.Booleans() {
		add(item.ID())
	}
	for _, item := range candidate.Buttons() {
		add(item.ID())
	}
	for _, item := range candidate.Datetimes() {
		add(item.ID())
	}
	for _, item := range candidate.Selects() {
		add(item.ID())
	}
	for _, item := range candidate.Texts() {
		add(item.ID())
	}
	for _, item := range candidate.Counters() {
		add(item.ID())
	}
	if len(seen) != 26 {
		t.Fatalf("got %d distinct helpers", len(seen))
	}
	for _, item := range candidate.Booleans() {
		alias, _ := item.ID().Value()
		_, present := item.Initial()
		if present != (alias == "input_boolean.default_dashboard" || alias == "input_boolean.gazon_mode_germination") {
			t.Fatalf("boolean initial drift: %s", alias)
		}
	}
	for _, item := range candidate.Selects() {
		alias, _ := item.ID().Value()
		value, _ := item.Initial()
		if alias == "input_select.time_of_day" && (value != "Night" || item.ExplicitInitial()) {
			t.Fatal("first option initial not derived")
		}
	}
	for _, item := range candidate.Datetimes() {
		alias, _ := item.ID().Value()
		initial, hasInitial := item.Initial()
		if alias == "input_datetime.gazon_date_de_semis" && (!hasInitial || item.Mode() != domain.HelperDate || !initial.Valid()) {
			t.Fatal("civil date initial drift")
		}
		if alias == "input_datetime.bathroom_heating_time" && (hasInitial || item.Mode() != domain.HelperTime) {
			t.Fatal("time-only profile drift")
		}
	}
}

func TestNHA097Generic27thWithConfiguredManifest(t *testing.T) {
	text := nha097Source(t)
	text = strings.Replace(text, "input_text:\n", "input_text:\n  future_note: {min: 0, max: 10, mode: text}\n", 1)
	candidate, err := nha097CompileText(t, text)
	if err != nil || !candidate.Valid() || len(candidate.Texts()) != 2 {
		t.Fatalf("a 27th valid definition must be admissible: %v", err)
	}
}

func TestNHA097InvalidSelectInitialAndDatetimeMode(t *testing.T) {
	text := nha097Source(t)
	for _, mutant := range []string{
		strings.Replace(text, "initial: lovelace", "initial: outside", 1),
		strings.Replace(text, "has_date: false, has_time: true", "has_date: false, has_time: false", 1),
		strings.Replace(text, "initial: '2026-09-01'", "initial: '2026-02-30'", 1),
	} {
		if candidate, err := nha097CompileText(t, mutant); err == nil || candidate.Valid() {
			t.Fatal("invalid retained variant admitted")
		}
	}
}
