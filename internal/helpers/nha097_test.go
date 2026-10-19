package helpers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/scope"
)

func nha097Definitions(t *testing.T) []Definition {
	t.Helper()
	candidate, err := bundle.CompileV2(filepath.Join("..", "config", "bundle", "testdata", "nha097"), "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := Definitions(candidate)
	if err != nil || len(definitions) != 26 {
		t.Fatalf("definitions: %v, count=%d", err, len(definitions))
	}
	return definitions
}

func nha097Definition(t *testing.T, definitions []Definition, text string) Definition {
	t.Helper()
	for _, definition := range definitions {
		alias, _ := definition.alias().Value()
		if alias == text {
			return definition
		}
	}
	t.Fatalf("missing %s", text)
	return Definition{}
}

func nha097Bound(t *testing.T, fixture nha033Fixture, definition Definition) Handle {
	t.Helper()
	alias, _ := definition.alias().Value()
	instance, err := domain.ParseIntegrationInstanceID("homeward-helpers-v1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := ConfiguredSourceKey(definition.alias())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fixture.registry.ConfigureBinding(fixture.ctx, instance, key, alias)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := fixture.repo.Bind(fixture.ctx, identity.Entity, definition)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func TestNHA097HelperValuePersistenceAndCAS(t *testing.T) {
	fixture := nha033Setup(t, false)
	definitions := nha097Definitions(t)
	date, _ := domain.NewCivilDate(2026, 9, 2)
	clock, _ := domain.NewCivilTime(8, 30, 0)
	civil, _ := domain.NewCivilDateTime(date, clock)
	button, _ := domain.NewHelperButtonPress(time.Date(2026, 10, 18, 12, 34, 56, 0, time.UTC))
	dateValue, _ := domain.NewHelperDate(date)
	timeValue, _ := domain.NewHelperTime(clock)
	datetimeValue, _ := domain.NewHelperDateTime(civil)
	selectValue, _ := domain.NewHelperSelect("Morning")
	textValue, _ := domain.NewHelperText("")
	values := map[string]Value{
		"input_boolean.away_mode":                    Boolean(false),
		"counter.bathroom_heating_used_today":        Counter(5),
		"input_button.trash_out":                     button,
		"input_datetime.gazon_date_de_semis":         dateValue,
		"input_datetime.bathroom_heating_time":       timeValue,
		"input_datetime.sdb1_occupancy_last_time":    datetimeValue,
		"input_select.time_of_day":                   selectValue,
		"input_text.guest_bedroom_remote_last_event": textValue,
	}
	for alias, desired := range values {
		t.Run(alias, func(t *testing.T) {
			definition := nha097Definition(t, definitions, alias)
			handle := nha097Bound(t, fixture, definition)
			before, err := fixture.repo.Restore(fixture.ctx, handle, definition)
			if err != nil {
				t.Fatal(err)
			}
			committed, err := fixture.repo.CompareAndSwap(fixture.ctx, handle, definition, before.Revision(), desired)
			if err != nil {
				t.Fatal(err)
			}
			if value, present := committed.Value(); !present || value != desired {
				t.Fatalf("committed value drift: present=%v", present)
			}
			after, err := fixture.repo.Restore(fixture.ctx, handle, definition)
			if err != nil {
				t.Fatal(err)
			}
			if value, present := after.Value(); !present || value != desired {
				t.Fatalf("round-trip drift: present=%v", present)
			}
			if _, err := fixture.repo.CompareAndSwap(fixture.ctx, handle, definition, before.Revision(), desired); err == nil {
				t.Fatal("stale CAS admitted")
			} else {
				var helperError *Error
				if !errors.As(err, &helperError) || helperError.Code() != Conflict {
					t.Fatalf("wrong stale CAS error: %v", err)
				}
			}
		})
	}
}

func TestNHA097CounterSetAlignmentOnly(t *testing.T) {
	fixture := nha033Setup(t, false)
	source, err := parser.NewSourceFile("configuration.yaml", []byte("counter:\n  bathroom_heating_used_today: {initial: 3, minimum: 0, maximum: 6, step: 2, restore: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileSourcesV2("configuration.yaml", []parser.SourceFile{source})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := CounterDefinition(candidate.Counters()[0])
	if err != nil {
		t.Fatal(err)
	}
	handle := nha097Bound(t, fixture, definition)
	initial, err := fixture.repo.Restore(fixture.ctx, handle, definition)
	if err != nil {
		t.Fatal(err)
	}
	if current, ok := initial.Value(); !ok || current != Counter(3) {
		t.Fatal("historical non-aligned initial rejected")
	}
	if _, err := Compute(definition, initial, OperationSet, Counter(3), time.Now()); err == nil {
		t.Fatal("set accepted unaligned target")
	}
	step, err := Compute(definition, initial, OperationIncrement, Value{}, time.Now())
	if err != nil || step != Counter(5) {
		t.Fatalf("increment should preserve offset: %v", err)
	}
	committed, err := fixture.repo.CompareAndSwap(fixture.ctx, handle, definition, initial.Revision(), step)
	if err != nil {
		t.Fatal(err)
	}
	clamped, err := Compute(definition, committed, OperationIncrement, Value{}, time.Now())
	if err != nil || clamped != Counter(6) {
		t.Fatalf("upper clamp failed: %v", err)
	}
	if _, err := fixture.repo.CompareAndSwap(context.Background(), handle, definition, committed.Revision(), Counter(3)); err != nil {
		t.Fatalf("repository CAS must retain historical admission: %v", err)
	}
}

func TestNHA097RetainedCorpusHasManifestBindingsButFutureNeedsOne(t *testing.T) {
	fixture := nha033Setup(t, false)
	for _, definition := range nha097Definitions(t) {
		_ = nha097Bound(t, fixture, definition)
	}
	instance, err := domain.ParseIntegrationInstanceID("homeward-helpers-v1")
	if err != nil {
		t.Fatal(err)
	}
	future, _ := domain.ParseHAAlias("input_boolean.future_valid_definition")
	key, err := ConfiguredSourceKey(future)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.registry.ConfigureBinding(fixture.ctx, instance, key, "input_boolean.future_valid_definition"); err == nil {
		t.Fatal("out-of-manifest test identity acquired a registry binding")
	}
}

type nha097PassiveTasks struct{}

func (nha097PassiveTasks) Go(integrations.Task) error { return nil }

func TestNHA097TwentySeventhAdmittedDefinitionBindsWithoutProductCap(t *testing.T) {
	fixture := nha033Setup(t, false)
	original, err := os.ReadFile(filepath.Join("..", "config", "bundle", "testdata", "nha097", "configuration.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const added = "input_boolean:\n  future_valid_definition: {name: Future valid definition}\n"
	source := strings.Replace(string(original), "input_boolean:\n", added, 1)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "configuration.yaml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	candidate, err := bundle.CompileV2(root, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := Definitions(candidate)
	if err != nil || len(definitions) != 27 {
		t.Fatalf("27th valid definition rejected: %d %v", len(definitions), err)
	}
	policy, err := scope.RetainedHelperButtonPolicy()
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewPilotFactory(fixture.repo, definitions, policy)
	if err != nil {
		t.Fatalf("factory imposed corpus cap: %v", err)
	}
	// Keep the frozen 850-entry production manifest untouched. This independent
	// synthetic snapshot replaces an unrelated test alias and recalculates its
	// digest, proving a future admitted manifest can bind the new definition.
	manifestSource, err := os.ReadFile(filepath.Join("..", "..", "scope", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version  int    `json:"version"`
		Digest   string `json:"ids_sha256"`
		Entities []struct {
			ID     string `json:"entity_id"`
			Source string `json:"source"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(manifestSource, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Entities[0].ID = "input_boolean.future_valid_definition"
	manifest.Entities[0].Source = "nha097-synthetic-only"
	ids := make([]string, 0, len(manifest.Entities))
	for _, item := range manifest.Entities {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	manifest.Digest = hex.EncodeToString(digest[:])
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := registry.ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := registry.NewRepository(fixture.store, admitted)
	if err != nil {
		t.Fatal(err)
	}
	instance, _ := domain.ParseIntegrationInstanceID("homeward-helpers-v1")
	for _, definition := range definitions {
		alias, _ := definition.Alias()
		text, _ := alias.Value()
		key, err := ConfiguredSourceKey(alias)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := identities.ConfigureBinding(fixture.ctx, instance, key, text); err != nil {
			t.Fatalf("admitted 27th binding %s: %v", text, err)
		}
	}
	sequence, err := sequencer.New(sequencer.RealClock{}, 16384)
	if err != nil {
		t.Fatal(err)
	}
	if err := identities.AttachProjection(fixture.ctx, sequence); err != nil {
		t.Fatal(err)
	}
	catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	host, err := integrations.NewHost(identities, sequence, catalog)
	if err != nil {
		t.Fatal(err)
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	pilot := factory()
	if err := pilot.Start(fixture.ctx, session, nha097PassiveTasks{}); err != nil {
		t.Fatalf("27th source failed Host binding/restore/publication: %v", err)
	}
	future, _ := domain.ParseHAAlias("input_boolean.future_valid_definition")
	key, _ := ConfiguredSourceKey(future)
	resolved, err := identities.Resolve(fixture.ctx, instance, []integrations.SourceKey{key})
	if err != nil || len(resolved) != 1 {
		t.Fatalf("27th source resolution: %v", err)
	}
	if _, present, err := sequence.State(resolved[0].Entity); err != nil || !present {
		t.Fatalf("27th helper state not published: %t %v", present, err)
	}
	if err := pilot.Stop(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.Revoke(session); err != nil {
		t.Fatal(err)
	}
}
