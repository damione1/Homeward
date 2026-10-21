package configdocuments

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func nha149Workspace(t *testing.T) (*Workspace, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	base := []byte("# synthetic private source\nexternal_workers: []\n")
	if err := os.WriteFile(path, base, 0600); err != nil {
		t.Fatal(err)
	}
	w, err := OpenWorkspace(path, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, path, base
}
func nha149Failure(t *testing.T, err error, kind Failure) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Kind() != kind {
		t.Fatalf("wanted closed failure %d, got %v", kind, err)
	}
}
func TestNHA149WorkspaceExactHashDetectsExternalEditWithUnchangedDate(t *testing.T) {
	w, path, base := nha149Workspace(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	external := []byte("# external writer\nexternal_workers: []\n")
	if err := os.WriteFile(path, external, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, err = w.BeginEdit(sha256.Sum256(base), sha256.Sum256(base), []byte("external_workers: []\n"))
	nha149Failure(t, err, Conflict)
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, external) {
		t.Fatal("conflict overwrote external editor", err)
	}
}
func TestNHA149WorkspaceExternalEditAfterStagingSurvivesFinishAndRecovery(t *testing.T) {
	w, path, base := nha149Workspace(t)
	candidate := []byte("# candidate\nexternal_workers: []\n")
	edit, err := w.BeginEdit(sha256.Sum256(base), sha256.Sum256(base), candidate)
	if err != nil {
		t.Fatal(err)
	}
	external := []byte("# external after staging\nexternal_workers: []\n")
	if err := os.WriteFile(path, external, 0600); err != nil {
		t.Fatal(err)
	}
	nha149Failure(t, edit.Finish(sha256.Sum256(candidate), true), ReconciliationRequired)
	observation, err := w.Observe()
	if err != nil || !observation.ReconciliationPending() {
		t.Fatal("lost interrupted intent", err)
	}
	_, err = w.Reconcile(sha256.Sum256(candidate))
	nha149Failure(t, err, ReconciliationRequired)
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, external) {
		t.Fatal("recovery overwrote external writer", err)
	}
}
func TestNHA149WorkspaceCrashAcrossFilesystemAndSQLIsExplicit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			w, path, base := nha149Workspace(t)
			candidate := []byte("# interrupted candidate\nexternal_workers: []\n")
			edit, err := w.BeginEdit(sha256.Sum256(base), sha256.Sum256(base), candidate)
			if err != nil {
				t.Fatal(err)
			}
			nha149Failure(t, edit.Finish([32]byte{}, false), ReconciliationRequired)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := OpenWorkspace(path, "configuration.yaml")
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			observation, err := restarted.Observe()
			if err != nil || !observation.ReconciliationPending() {
				t.Fatal("restart hid interrupted FS/SQL edit", err)
			}
			sql := sha256.Sum256(base)
			if committed {
				sql = sha256.Sum256(candidate)
			}
			found, err := restarted.Reconcile(sql)
			if !found {
				t.Fatal("recovery failed to find its exact intention")
			}
			if err != nil {
				t.Fatal("resolved SQL outcome failed exact-artifact recovery", err)
			}
			observation, err = restarted.Observe()
			if err != nil || observation.ReconciliationPending() {
				t.Fatal("resolved exact candidate failed recovery", err)
			}
			current, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(current, candidate) {
				t.Fatal("recovery rewrote editable candidate", err)
			}
		})
	}
}
func TestNHA149KnownRejectedCandidateRemainsEditableAndHasNoPlaintextFormatter(t *testing.T) {
	w, path, base := nha149Workspace(t)
	candidate := []byte("# synthetic secret canary\ninvalid: [yaml\n")
	edit, err := w.BeginEdit(sha256.Sum256(base), sha256.Sum256(base), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(fmt.Sprintf("%v %#v", edit, edit)), []byte("canary")) {
		t.Fatal("default formatting disclosed exact source")
	}
	if err := edit.Finish(sha256.Sum256(base), true); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, candidate) {
		t.Fatal("rejected editable source was rolled back", err)
	}
	correction := []byte("# corrected\nexternal_workers: []\n")
	next, err := w.BeginEdit(sha256.Sum256(candidate), sha256.Sum256(base), correction)
	if err != nil {
		t.Fatal("known rejection prevented correction", err)
	}
	if err := next.Finish(sha256.Sum256(correction), true); err != nil {
		t.Fatal(err)
	}
}

func TestNHA149TwoRejectedDirtyCandidatesUseAdmittedSQLBase(t *testing.T) {
	w, path, admitted := nha149Workspace(t)
	sql := sha256.Sum256(admitted)
	_, err := w.BeginEdit(sql, [32]byte{}, []byte("invalid: [\n"))
	nha149Failure(t, err, FailureInvalid)
	physical := sql
	for _, candidate := range [][]byte{[]byte("# rejected B\ninvalid: [\n"), []byte("# rejected C\ninvalid: {\n")} {
		edit, err := w.BeginEdit(physical, sql, candidate)
		if err != nil {
			t.Fatal("dirty candidate could not be staged", err)
		}
		if err := edit.Finish(sql, true); err != nil {
			t.Fatal("known rejection confused physical and SQL bases", err)
		}
		observation, err := w.Observe()
		if err != nil || observation.ReconciliationPending() {
			t.Fatal("known rejection left an uncertain intent", err)
		}
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(current, candidate) {
			t.Fatal("known rejection rewrote editable source", err)
		}
		physical = sha256.Sum256(candidate)
	}
	corrected := []byte("# corrected D\nexternal_workers: []\n")
	edit, err := w.BeginEdit(physical, sql, corrected)
	if err != nil {
		t.Fatal("two known rejections blocked correction", err)
	}
	if err := edit.Finish(sha256.Sum256(corrected), true); err != nil {
		t.Fatal(err)
	}
}

func TestNHA149PersistedDirtyIntentResolvesRollbackWithoutOverwritingExternalSource(t *testing.T) {
	for _, mode := range []string{"rollback", "published", "unexpected-sql", "external-current", "external-displaced", "legacy-intent"} {
		t.Run(mode, func(t *testing.T) {
			w, path, admitted := nha149Workspace(t)
			sql := sha256.Sum256(admitted)
			dirty := []byte("# rejected B\ninvalid: [\n")
			first, err := w.BeginEdit(sql, sql, dirty)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Finish(sql, true); err != nil {
				t.Fatal(err)
			}
			candidate := []byte("# uncertain C\nexternal_workers: []\n")
			edit, err := w.BeginEdit(sha256.Sum256(dirty), sql, candidate)
			if err != nil {
				t.Fatal(err)
			}
			nha149Failure(t, edit.Finish(sql, false), ReconciliationRequired)
			// The proof must survive losing the Edit and reopening the real directory.
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			external := []byte("# independent external source\nexternal_workers: []\n")
			if mode == "external-current" {
				if err := os.WriteFile(path, external, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "external-displaced" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), edit.record.displaced()), external, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "legacy-intent" {
				legacy := edit.record
				legacy.Format = 1
				legacy.SQLBase = [32]byte{}
				raw, err := json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), w.intent), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := OpenWorkspace(path, "configuration.yaml")
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			resolved := sql
			if mode == "published" {
				resolved = sha256.Sum256(candidate)
			}
			if mode == "unexpected-sql" {
				resolved = sha256.Sum256(dirty)
			}
			found, err := restarted.Reconcile(resolved)
			if !found {
				t.Fatal("persistent intent disappeared")
			}
			accepted := mode == "rollback" || mode == "published"
			if accepted {
				if err != nil {
					t.Fatal("exact resolved SQL/artifacts could not retire intent", err)
				}
			} else {
				nha149Failure(t, err, ReconciliationRequired)
			}
			observation, err := restarted.Observe()
			if err != nil || observation.ReconciliationPending() == accepted {
				t.Fatal("incorrect persistent recovery state", err)
			}
			want := candidate
			if mode == "external-current" {
				want = external
			}
			current, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(current, want) {
				t.Fatal("reconciliation overwrote editable source", err)
			}
			if accepted {
				correction := []byte("# correction D after recovery\nexternal_workers: []\n")
				next, err := restarted.BeginEdit(sha256.Sum256(candidate), resolved, correction)
				if err != nil {
					t.Fatal("resolved rollback left correction blocked", err)
				}
				if err := next.Finish(sha256.Sum256(correction), true); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
