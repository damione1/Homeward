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

// This is the delivered NHF008 SQL26 baseline, not a current build with an
// older version label. Its retained race executable is also usable by a normal
// parent test process; the child keeps its own compiled identity and profile.
const nhf009Prior26Manifest = "d1882938f01ed5f84a4e2ec6c92085c7649f619aea358898e4174282712f773c"
const nhf009Prior26Sources = "58b313b154c3477ad98326888d3fbed8a0522f16b02a65fd286fe08048d86a89"
const nhf009Prior26Plan = "7d74bb6ac7442ba1639a43c61e98a51e0d4846b6e9c1703b2b78e0b6ff34d4eb"
const nhf009Prior26Release = "homeward-window-26-27-baseline"

func nhf009Prior26Artifact(t *testing.T) (nha036Artifact, nha036SchemaProfile) {
	t.Helper()
	path := os.Getenv("HOMEWARD_NHF009_PARENT26_ARTIFACTS")
	if !filepath.IsAbs(path) {
		t.Fatal("NHF009 requires the retained delivered SQL26 artifact manifest")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != nhf009Prior26Manifest {
		t.Fatal("NHF009 predecessor manifest differs from the delivered SQL26 evidence")
	}
	var manifest nha036Artifacts
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	want := nha036SchemaProfile{26, 27, nhf009Prior26Release, "homeward-window-26-27-successor"}
	a, present := manifest.Artifacts["baseline"]
	if manifest.Format != 1 || !manifest.Race || !strings.HasPrefix(manifest.Go, "go version go1.27.2 ") || manifest.Profile != want ||
		!present || a.Schema != 26 || a.Release != nhf009Prior26Release || a.MinimumReader != a.Release || a.MinimumWriter != a.Release ||
		a.PlanSHA256 != nhf009Prior26Plan || a.SourcesSHA256 != nhf009Prior26Sources {
		t.Fatal("NHF009 predecessor does not identify the actual delivered SQL26 baseline")
	}
	for path, expected := range map[string]string{a.Test: a.TestSHA256, a.Homed: a.HomedSHA256, a.SourceManifest: nhf009Prior26Sources} {
		if !filepath.IsAbs(path) || len(expected) != 64 || expected == strings.Repeat("0", 64) || nha036FileDigest(t, path) != expected {
			t.Fatal("NHF009 delivered SQL26 executable or source manifest changed")
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
		t.Fatalf("NHF009 delivered SQL26 source changed: %v", err)
	}
	return a, manifest.Profile
}

func TestNHF009Migration26ToCurrentPreservesDataAndRefusesActualOldHomed(t *testing.T) {
	old, profile := nhf009Prior26Artifact(t)
	current := nha036LoadArtifacts(t).Artifacts["baseline"]
	if current.Schema != storage.CurrentSchemaBuild().Version().Number() || current.Release != storage.CurrentSchemaBuild().Release() || current.MinimumReader != current.Release || current.MinimumWriter != current.Release {
		t.Fatal("NHF009 current artifact must require its own compiled readers and writers")
	}
	f := nha029C5NewFixture(t)
	child := nha036Start(t, f, old.Test, false)
	ready := child.ready(t, nil)
	var identity nha036BuildIdentity
	want := nha036BuildIdentity{old.Release, old.Schema, old.PlanSHA256, profile, old.MinimumReader, old.MinimumWriter}
	if json.Unmarshal([]byte(ready.value), &identity) != nil || identity != want || ready.count != 26 {
		t.Fatal("NHF009 database was not created by the actual delivered SQL26 executable")
	}
	nha036Operate(t, child, "seed", nil)
	child.stop(t)
	nha036ObserveBusiness(t, f, false, 1, false)
	nha036Homed(t, f, old, true)
	a := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	history := nha029C5RestoreDigest(t, f.ctx, f.observer, nha036HistoryDigest, 26)
	before := nha055MigrationSnapshot(t, f)
	version, err := f.store.Migrate(f.ctx)
	if err != nil || version.Number() != current.Schema {
		t.Fatalf("actual delivered SQL26 to current migration failed: %v", err)
	}
	nha055AssertCurrentWindow(t, f, current)
	nha036ObserveBusiness(t, f, false, 1, false)
	after := nha055MigrationSnapshot(t, f)
	if after.business != before.business || after.registry != before.registry || after.humans != before.humans ||
		nha029C5RestoreDigest(t, f.ctx, f.observer, nha036HistoryDigest, 26) != history || len(f.effects) != 0 {
		t.Fatal("NHF009 migration changed predecessor data or dispatched an action")
	}
	if version, err := f.store.Migrate(f.ctx); err != nil || version.Number() != current.Schema || nha055MigrationSnapshot(t, f) != after {
		t.Fatal("NHF009 repeated migration changed committed DDL, history or data", err)
	}
	nha036Homed(t, f, current, true)
	b := nha029C5RestoreMarkSQL(t, f.ctx, f.store)
	if b.revision != a.revision || b.manifest != a.manifest || b.source != a.source || len(f.effects) != 0 {
		t.Fatal("NHF009 current cold start changed active authority or replayed an action")
	}
	for _, native := range []bool{false, true} {
		if native {
			nha055RetainNative26Sources(t, f)
		}
		beforeRefusal := nha055MigrationSnapshot(t, f)
		nha036Homed(t, f, old, false)
		if nha055MigrationSnapshot(t, f) != beforeRefusal || len(f.effects) != 0 {
			t.Fatal("NHF009 actual SQL26 refusal changed the current database")
		}
	}
	t.Logf("NHF009 real26-to-current schema=%d sources=%s old_homed=%s current_homed=%s current_plan=%s old26_refused_before_READY=true effects=0", current.Schema, old.SourcesSHA256, old.HomedSHA256, current.HomedSHA256, current.PlanSHA256)
}
