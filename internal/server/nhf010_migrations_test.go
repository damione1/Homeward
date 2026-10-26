package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/internal/storage"
)

// This is the delivered NHF009 SQL27 baseline, not a current build with an
// older version label. Its retained race executable is also usable by a normal
// parent test process; the child keeps its own compiled identity and profile.
const nhf010Prior27Manifest = "7b699a9d3f1afa7722a2c80c0299f7bea451aecc4e26396640a433bce9901525"
const nhf010Prior27Sources = "7d25566ed6ed7f57b1da9a8a0b8db6c653c090666803d8c194da61b32f57aa15"
const nhf010Prior27Plan = "4059f122e08a86cc978804b61edb356a3c32116dc0921f0dbf8c153a2e08a009"
const nhf010Prior27Release = "homeward-window-27-28-baseline"

func nhf010Prior27Artifact(t *testing.T) (nha036Artifact, nha036SchemaProfile) {
	t.Helper()
	path := os.Getenv("HOMEWARD_NHF010_PARENT27_ARTIFACTS")
	if !filepath.IsAbs(path) {
		t.Fatal("NHF010 requires the retained delivered SQL27 artifact manifest")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != nhf010Prior27Manifest {
		t.Fatal("NHF010 predecessor manifest differs from the delivered SQL27 evidence")
	}
	var manifest nha036Artifacts
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	want := nha036SchemaProfile{27, 28, nhf010Prior27Release, "homeward-window-27-28-successor"}
	a, present := manifest.Artifacts["baseline"]
	if manifest.Format != 1 || !manifest.Race || !strings.HasPrefix(manifest.Go, "go version go1.27.2 ") || manifest.Profile != want ||
		!present || a.Schema != 27 || a.Release != nhf010Prior27Release || a.MinimumReader != a.Release || a.MinimumWriter != a.Release ||
		a.PlanSHA256 != nhf010Prior27Plan || a.SourcesSHA256 != nhf010Prior27Sources {
		t.Fatal("NHF010 predecessor does not identify the actual delivered SQL27 baseline")
	}
	for path, expected := range map[string]string{a.Test: a.TestSHA256, a.Homed: a.HomedSHA256, a.SourceManifest: nhf010Prior27Sources} {
		if !filepath.IsAbs(path) || len(expected) != 64 || expected == strings.Repeat("0", 64) || nha036FileDigest(t, path) != expected {
			t.Fatal("NHF010 delivered SQL27 executable or source manifest changed")
		}
	}
	raw, err = os.ReadFile(a.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	var sources map[string]string
	if err := json.Unmarshal(raw, &sources); err != nil {
		t.Fatal(err)
	}
	if err := nha036VerifySourceInventory(filepath.Join(filepath.Dir(a.SourceManifest), "source"), sources); err != nil {
		t.Fatalf("NHF010 delivered SQL27 source changed: %v", err)
	}
	return a, manifest.Profile
}

func TestNHF010Migration27ToCurrentPreservesDataAndRefusesActualOldHomed(t *testing.T) {
	old, profile := nhf010Prior27Artifact(t)
	current := nha036LoadArtifacts(t).Artifacts["baseline"]
	if current.Schema != storage.CurrentSchemaBuild().Version().Number() || current.Release != storage.CurrentSchemaBuild().Release() || current.MinimumReader != current.Release || current.MinimumWriter != current.Release {
		t.Fatal("NHF010 current artifact must require its own compiled readers and writers")
	}
	f := nha029C5NewFixture(t)
	child := nha036Start(t, f, old.Test, false)
	ready := child.ready(t, nil)
	var identity nha036BuildIdentity
	want := nha036BuildIdentity{old.Release, old.Schema, old.PlanSHA256, profile, old.MinimumReader, old.MinimumWriter}
	if json.Unmarshal([]byte(ready.value), &identity) != nil || identity != want || ready.count != 27 {
		t.Fatal("NHF010 database was not created by the actual delivered SQL27 executable")
	}
	nha036Operate(t, child, "seed", nil)
	child.stop(t)
	nha036ObserveBusiness(t, f, false, 1, false)
	nha036Homed(t, f, old, true)
	a := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	history := nha029C5RestoreDigest(t, f.ctx, f.observer, nha036HistoryDigest, 27)
	before := nha055MigrationSnapshot(t, f)
	version, err := f.store.Migrate(f.ctx)
	if err != nil || version.Number() != current.Schema {
		t.Fatalf("actual delivered SQL27 to current migration failed: %v", err)
	}
	nha055AssertCurrentWindow(t, f, current)
	nha036ObserveBusiness(t, f, false, 1, false)
	after := nha055MigrationSnapshot(t, f)
	if after.business != before.business || after.registry != before.registry || after.humans != before.humans ||
		nha029C5RestoreDigest(t, f.ctx, f.observer, nha036HistoryDigest, 27) != history || len(f.effects) != 0 {
		t.Fatal("NHF010 migration changed predecessor data or dispatched an action")
	}
	if version, err := f.store.Migrate(f.ctx); err != nil || version.Number() != current.Schema || nha055MigrationSnapshot(t, f) != after {
		t.Fatal("NHF010 repeated migration changed committed DDL, history or data", err)
	}
	nha036Homed(t, f, current, true)
	b := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	if b.revision != a.revision || b.manifest != a.manifest || b.source != a.source || len(f.effects) != 0 {
		t.Fatal("NHF010 current cold start changed active authority or replayed an action")
	}
	for _, native := range []bool{false, true} {
		if native {
			nha055RetainNative26Sources(t, f)
		}
		beforeRefusal := nha055MigrationSnapshot(t, f)
		nha036Homed(t, f, old, false)
		if nha055MigrationSnapshot(t, f) != beforeRefusal || len(f.effects) != 0 {
			t.Fatal("NHF010 actual SQL27 refusal changed the current database")
		}
	}
	t.Logf("NHF010 real27-to-current schema=%d sources=%s old_homed=%s current_homed=%s current_plan=%s old27_refused_before_READY=true effects=0", current.Schema, old.SourcesSHA256, old.HomedSHA256, current.HomedSHA256, current.PlanSHA256)
}
