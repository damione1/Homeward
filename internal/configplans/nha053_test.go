package configplans

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func nha053Manifest(t *testing.T) registry.Manifest {
	t.Helper()
	manifest, e := registry.LoadManifest(scope.BaselineV1())
	if e != nil {
		t.Fatal(e)
	}
	return manifest
}
func nha053Candidate(t *testing.T, f Family, identity, raw string) Candidate {
	t.Helper()
	file, e := parser.NewSourceFile("candidate.yaml", []byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	candidate, e := NewCandidate(f, identity, "candidate.yaml", []parser.SourceFile{file})
	if e != nil {
		t.Fatal(e)
	}
	return candidate
}
func nha053Compile(t *testing.T, candidate Candidate) image {
	t.Helper()
	compiled, d := compile(candidate, nha053Manifest(t))
	if d != DiagnosticInvalid {
		t.Fatalf("candidate refused with %d", d)
	}
	return compiled
}
func TestNHA053ExactAndSemanticHashesAndClosedValues(t *testing.T) {
	first := nha053Candidate(t, Declarative1, "legacy.test", "input_boolean:\n  away_mode:\n    initial: false\n    name: Away\n")
	second := nha053Candidate(t, Declarative1, "legacy.test", "# comment only\ninput_boolean:\n  away_mode:\n    name: Away\n    initial: false\n")
	left, right := nha053Compile(t, first), nha053Compile(t, second)
	if first.SourceSHA256() == second.SourceSHA256() || left.digest != right.digest || len(difference(left, right)) != 0 {
		t.Fatal("source bytes and semantic projection were conflated")
	}
	for _, family := range []Family{FamilyInvalid, Family(255)} {
		if _, e := NewCandidate(family, "test", "candidate.yaml", first.files); e == nil {
			t.Fatal("unadmitted family crossed the boundary")
		}
	}
	if (Candidate{}).Valid() || Family(255).Valid() || Effect(255).Valid() {
		t.Fatal("zero/future value admitted")
	}
	raw := []byte("input_boolean: {away_mode: {initial: false}}\n")
	file, e := parser.NewSourceFile("candidate.yaml", raw)
	if e != nil {
		t.Fatal(e)
	}
	candidate, e := NewCandidate(Declarative1, "legacy.test", "candidate.yaml", []parser.SourceFile{file})
	if e != nil {
		t.Fatal(e)
	}
	before := candidate.SourceSHA256()
	raw[0] = 'x'
	copyBytes := candidate.files[0].Bytes()
	copyBytes[0] = 'y'
	if before != candidate.SourceSHA256() || candidate.files[0].Bytes()[0] != 'i' {
		t.Fatal("candidate retained mutable source alias")
	}
	private := nha053Candidate(t, Operational, "operational.main", "# private canary NEVER_PUBLIC_NHA053\nexternal_workers: []\n")
	encoded, e := json.Marshal(private)
	if e != nil || bytes.Contains(encoded, []byte("NEVER_PUBLIC")) || strings.Contains(fmt.Sprintf("%#v", private), "NEVER_PUBLIC") {
		t.Fatal("private candidate exposed source")
	}
}
func TestNHA053RightsOwnershipRunTargetsAndDashboardRemainDistinct(t *testing.T) {
	policy := nha053Candidate(t, Authorization, "synthetic-admin", "schema: homeward.authorization/1\naccount_id: synthetic-admin\ngrants: []\n")
	rights := nha053Compile(t, policy)
	if rights.effect != RightsEffect {
		t.Fatal("policy was classified as layout")
	}
	ownership := nha053Candidate(t, Ownership, "heating.ownership", string(scope.HeatingOwnershipV1()))
	old := nha053Compile(t, ownership)
	var corpus map[string]any
	if e := json.Unmarshal(scope.HeatingOwnershipV1(), &corpus); e != nil {
		t.Fatal(e)
	}
	pair := corpus["candidates"].([]any)[0].(map[string]any)
	pair["relation"] = map[string]any{"kind": "corroborated_hardware_identity", "hilo": map[string]any{"manufacturer": "SYNTHETIC", "model": "fixture", "serial": "NHA053-only", "serial_scope": "manufacturer_global", "source_ref": "local-fixture-hilo"}, "neviweb": map[string]any{"manufacturer": "SYNTHETIC", "model": "fixture", "serial": "NHA053-only", "serial_scope": "manufacturer_global", "source_ref": "local-fixture-neviweb"}}
	raw, e := json.Marshal(corpus)
	if e != nil {
		t.Fatal(e)
	}
	next := nha053Compile(t, nha053Candidate(t, Ownership, "heating.ownership", string(raw)))
	if changes := difference(old, next); len(changes) != 1 || changes[0].effect != OwnershipEffect {
		t.Fatal("ownership relation was lost or classified as dashboard")
	}
	dashboard := nha053Candidate(t, Dashboard, "dashboard.preview", "views:\n  - type: sections\n    path: preview\n    title: Preview\n    sections:\n      - type: grid\n        cards:\n          - type: tile\n            entity: light.basement_lights\n")
	layout := nha053Compile(t, dashboard)
	if layout.effect != DashboardEffect {
		t.Fatal("dashboard became rights/physical action")
	}
	scriptA := nha053Candidate(t, Script, "script.preview", "alias: Preview\nsequence:\n  - action: light.turn_on\n    target:\n      entity_id: light.basement_lights\n    data: {}\n")
	scriptB := nha053Candidate(t, Script, "script.preview", "alias: Preview\nsequence:\n  - action: light.turn_on\n    target:\n      entity_id: light.basement_sitting_area_spot_lights\n    data: {}\n")
	// Preserve NHA042's explicit service-data and declared script namespace
	// contracts. The positive fixtures above were missing data:{}, so they
	// were invalid independently of the catalogue-composition defect.
	missingData := nha053Candidate(t, Script, "script.preview", "sequence:\n - action: light.turn_on\n   target: {entity_id: light.basement_lights}\n")
	if _, diagnostic := compile(missingData, nha053Manifest(t)); diagnostic == DiagnosticInvalid {
		t.Fatal("script service call without explicit data mapping became admitted")
	}
	foreignScript := nha053Candidate(t, Script, "script.preview", "sequence:\n - action: script.foreign_outside_catalogue\n   data: {}\n")
	if _, diagnostic := compile(foreignScript, nha053Manifest(t)); diagnostic == DiagnosticInvalid {
		t.Fatal("source strings invented an undeclared foreign script")
	}
	a, b := nha053Compile(t, scriptA), nha053Compile(t, scriptB)
	changes := difference(a, b)
	if len(changes) != 1 || changes[0].effect != RunTargetsEffect || changes[0].referencesAdded != 1 || changes[0].referencesRemoved != 1 {
		t.Fatal("actual IR target delta was not exposed separately")
	}
	if rights.effect == layout.effect || next.effect == layout.effect || a.effect == layout.effect {
		t.Fatal("sensitive effects collapsed into dashboard")
	}
}
func TestNHA053ExcludedUnknownAndUnsupportedSourcesFailClosed(t *testing.T) {
	cases := []Candidate{
		nha053Candidate(t, Declarative1, "legacy.test", "input_boolean: {excluded_outside_manifest: {initial: false}}\n"),
		nha053Candidate(t, Declarative1, "legacy.test", "future_helper: {}\n"),
		nha053Candidate(t, Operational, "operational.main", "unknown_future_key: fixture-secret-canary\n"),
		nha053Candidate(t, Operational, "operational.main", "external_workers: [\n"),
		nha053Candidate(t, Dashboard, "dashboard.preview", "views:\n  - type: sections\n    path: preview\n    title: Preview\n    sections:\n      - type: grid\n        cards:\n          - type: tile\n            entity: light.excluded_outside_manifest\n"),
		nha053Candidate(t, Dashboard, "dashboard.preview", "views:\n  - type: sections\n    path: preview\n    title: Preview\n    sections:\n      - type: grid\n        cards:\n          - type: custom:execute-javascript\n"),
		nha053Candidate(t, Script, "script.preview", "sequence:\n  - action: light.turn_on\n    target: {entity_id: light.excluded_outside_manifest}\n"),
	}
	for i, candidate := range cases {
		if _, d := compile(candidate, nha053Manifest(t)); d == DiagnosticInvalid {
			t.Fatalf("negative candidate %d was admitted", i)
		}
	}
}
func TestNHA053OperationalLaunchIsNotAnEffectOrScopeAuthority(t *testing.T) {
	a := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", "listen: 127.0.0.1:1234\nexternal_workers: []\n"))
	b := nha053Compile(t, nha053Candidate(t, Operational, "operational.main", "# launch remains independent\nlisten: 127.0.0.1:5678\nexternal_workers: []\n"))
	if a.digest != b.digest || len(difference(a, b)) != 0 {
		t.Fatal("ignored launch bytes invented an operational effect")
	}
}
func TestNHA053ReceiptProvesMetadataAndCannotGrantAuthority(t *testing.T) {
	// A receipt can be exercised without database calls; valid() still requires
	// real owner fields. Use signPayload's pure helper through an initialized
	// test service only for MAC tamper checks, never as a fake product Build.
	key := [32]byte{1, 2, 3}
	plan := Plan{state: Admitted, author: "original-admin", family: Dashboard, identity: "dashboard.preview", entry: "candidate.yaml", schema: Dashboard.Schema(), source: sha256.Sum256([]byte("candidate")), semantic: sha256.Sum256([]byte("semantic")), base: Base{kind: NoActiveDocument}, generation: 1, manifest: sha256.Sum256([]byte("manifest")), workspace: sha256.Sum256([]byte("workspace")), catalog: sha256.Sum256([]byte("catalog")), effect: DashboardEffect}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	token, e := signReceipt(key, plan, now)
	if e != nil {
		t.Fatal(e)
	}
	payload, e := verifyReceipt(key, token, now)
	if e != nil || !payload.matches(plan) {
		t.Fatal("authentic receipt failed", e)
	}
	raw, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte("exact_source")) {
		t.Fatal("receipt contains source")
	}
	raw[2] ^= 1
	if _, e := verifyReceipt(key, base64.RawURLEncoding.EncodeToString(raw), now); !errors.Is(e, ErrInvalid) {
		t.Fatal("tampered metadata admitted")
	}
	if _, e := verifyReceipt([32]byte{9}, token, now); !errors.Is(e, ErrInvalid) {
		t.Fatal("receipt crossed process epoch")
	}
	if _, e := verifyReceipt(key, token, now.Add(30*time.Minute)); !errors.Is(e, ErrStale) {
		t.Fatal("expired receipt admitted")
	}
	changed := plan
	changed.generation++
	if payload.matches(changed) {
		t.Fatal("generation update kept plan fresh")
	}
	changed = plan
	changed.workspace[0] ^= 1
	if payload.matches(changed) {
		t.Fatal("exact workspace change kept plan fresh")
	}
	changed = plan
	changed.base.source[0] ^= 1
	if payload.matches(changed) {
		t.Fatal("base source hash change kept plan fresh")
	}
	changed = plan
	changed.base.revision++
	if payload.matches(changed) {
		t.Fatal("base revision change kept plan fresh")
	}
	changed = plan
	changed.catalog[0] ^= 1
	if payload.matches(changed) {
		t.Fatal("off-manifest document/policy changed without staleness")
	}
	if plan.ApplicationAvailable() {
		t.Fatal("receipt became an apply permission")
	}
}
func TestNHA053NoActiveAndImmutableBasesAreHonest(t *testing.T) {
	own := nha053Candidate(t, Ownership, "heating.ownership", string(scope.HeatingOwnershipV1()))
	base, e := immutableSource(own)
	if e != nil || !base.Valid() || base.SourceSHA256() != sha256.Sum256(scope.HeatingOwnershipV1()) {
		t.Fatal("immutable source provenance lost", e)
	}
	dashboard := nha053Candidate(t, Dashboard, "dashboard.preview", "views: []\n")
	base, e = immutableSource(dashboard)
	if e != nil || base.Valid() {
		t.Fatal("dashboard invented SQL/source authority", e)
	}
	future := nha053Candidate(t, Automation, "automation.excluded_outside_selection", "id: excluded_outside_selection\ntriggers: []\nactions: []\n")
	if _, e := immutableSource(future); !errors.Is(e, ErrInvalid) {
		t.Fatal("deferred/unselected logic acquired retained identity")
	}
	var s *Service
	if _, e := s.Build(context.Background(), own, storage.ConfigAuthor{}); !errors.Is(e, ErrInvalid) {
		t.Fatal("nil service admitted")
	}
}

func TestNHA053OperationalBindingChangesAreTechnicalOwnershipWithoutLaunching(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "MUST_NOT_EXIST")
	executable := filepath.Join(directory, "synthetic-worker")
	script := "#!/bin/sh\ntouch '" + marker + "'\nexit 1\n"
	if e := os.WriteFile(executable, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	source := func(alias string) string {
		return fmt.Sprintf("external_workers:\n - instance: synthetic-plan-worker\n   executable: %q\n   args: [PRIVATE_PROCESS_CANARY]\n   bindings:\n    - device: synthetic-device\n      entity: synthetic-entity\n      alias: %s\n", executable, alias)
	}
	first := nha053Candidate(t, Operational, "operational.main", source("light.basement_lights"))
	second := nha053Candidate(t, Operational, "operational.main", source("light.basement_sitting_area_spot_lights"))
	left, right := nha053Compile(t, first), nha053Compile(t, second)
	changes := difference(left, right)
	if len(changes) != 1 || changes[0].Effect() != OwnershipEffect || changes[0].Field() != "external_worker_bindings" || changes[0].ReferencesAdded() != 1 || changes[0].ReferencesRemoved() != 1 {
		t.Fatal("real binding change was lost or classified as generic configuration")
	}
	if _, e := os.Stat(marker); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("plan compiler launched the configured worker", e)
	}
}

func TestNHA053DuplicateNestedShapeAndAliasCyclesAreRefused(t *testing.T) {
	duplicate := strings.Replace(string(scope.HeatingOwnershipV1()), "\"version\": 1,", "\"version\": 1, \"version\": 1,", 1)
	if _, diagnostic := compile(nha053Candidate(t, Ownership, "heating.ownership", duplicate), nha053Manifest(t)); diagnostic == DiagnosticInvalid {
		t.Fatal("ownership duplicate JSON member admitted")
	}
	if uniqueJSON([]byte(`{"x":{"same":1,"same":2}}`)) || uniqueJSON([]byte(`{"x":1} {"x":2}`)) {
		t.Fatal("structural guard admitted contradictory/trailing source")
	}
	if _, e := admittedYAML([]byte("solar: {latitude: 1, latitude: 2}\n")); e == nil {
		t.Fatal("nested duplicate YAML admitted")
	}
	if _, e := admittedYAML([]byte("x: &cycle [*cycle]\n")); e == nil {
		t.Fatal("alias cycle admitted")
	}
}

func TestNHA053AllNineFamiliesUseExistingAdmissionPaths(t *testing.T) {
	cases := []Candidate{
		nha053Candidate(t, Operational, "operational.main", "external_workers: []\n"),
		nha053Candidate(t, Declarative1, "helpers.preview", "input_boolean: {away_mode: {initial: false}}\n"),
		nha053Candidate(t, Declarative2, "helpers.preview", "input_boolean: {away_mode: {initial: false}}\n"),
		nha053Candidate(t, Authorization, "synthetic-admin", "schema: homeward.authorization/1\naccount_id: synthetic-admin\ngrants: []\n"),
		nha053Candidate(t, Ownership, "heating.ownership", string(scope.HeatingOwnershipV1())),
		nha053Candidate(t, Dashboard, "dashboard.preview", "views: []\n"),
		nha053Candidate(t, Automation, "automation.turn_on_lights_at_sunset", "id: fixture\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - stop: complete\n"),
		nha053Candidate(t, Script, "script.preview", "sequence:\n - stop: complete\n"),
		nha053Candidate(t, Scene, "scene.ceilling_fan_night", "id: fixture\nname: Preview\nentities:\n switch.arc_soundbar_crossfade: 'off'\n"),
	}
	seen := map[Family]bool{}
	for _, candidate := range cases {
		t.Run(candidate.Schema()+fmt.Sprint(candidate.family), func(t *testing.T) {
			compiled := nha053Compile(t, candidate)
			if compiled.digest == ([32]byte{}) || !compiled.effect.Valid() {
				t.Fatal("admitted family has no semantic projection/effect")
			}
			seen[candidate.family] = true
		})
	}
	if len(seen) != 9 {
		t.Fatal("source-family proof silently narrowed")
	}
}
