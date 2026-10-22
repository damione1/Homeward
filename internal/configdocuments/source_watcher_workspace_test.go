package configdocuments

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/storage"
	"golang.org/x/sys/unix"
)

func nha055ObservedBinding(t *testing.T, id, logical string) storage.ConfigDocumentBinding {
	t.Helper()
	document, err := storage.NewConfigDocumentID(id)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(document, logical)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
func nha055ObservedFile(t *testing.T, operational string, group storage.ConfigDocumentID, binding storage.ConfigDocumentBinding, raw []byte) string {
	t.Helper()
	location, err := sourceMemberLocation(group, binding)
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(filepath.Dir(operational), filepath.FromSlash(location))
	if err := os.MkdirAll(filepath.Dir(physical), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(physical, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return physical
}

func TestNHA055SourceReaderConfinesGroupsAndDetectsPreservedDateEdits(t *testing.T) {
	workspace, operational, original := nha149Workspace(t)
	first, _ := storage.NewConfigDocumentID("dashboard.nha055_first")
	second, _ := storage.NewConfigDocumentID("dashboard.nha055_second")
	firstBinding := nha055ObservedBinding(t, "nha055.first", "nested/dashboard.yaml")
	secondBinding := nha055ObservedBinding(t, "nha055.second", "nested/dashboard.yaml")
	physical := nha055ObservedFile(t, operational, first, firstBinding, []byte("views: []\n"))
	nha055ObservedFile(t, operational, second, secondBinding, []byte("# separate group\nviews: []\n"))
	observed, err := workspace.ReadSources(first, []storage.ConfigDocumentBinding{firstBinding})
	if err != nil || len(observed) != 1 || !observed[0].Present() {
		t.Fatal(err)
	}
	before := observed[0].observation
	copyBytes := observed[0].Bytes()
	copyBytes[0] = 'x'
	if bytes.Equal(copyBytes, observed[0].Bytes()) {
		t.Fatal("caller mutated the pinned observation")
	}
	changed := []byte("# exact external bytes\nviews: []\n")
	if err := os.WriteFile(physical, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(physical, before.updated, before.updated); err != nil {
		t.Fatal(err)
	}
	after, err := workspace.ReadSources(first, []storage.ConfigDocumentBinding{firstBinding})
	if err != nil {
		t.Fatal(err)
	}
	if after[0].observation.hash == before.hash || !after[0].observation.updated.Equal(before.updated) || !bytes.Equal(after[0].Bytes(), changed) {
		t.Fatal("date-preserving edit was lost")
	}
	separate, err := workspace.ReadSources(second, []storage.ConfigDocumentBinding{secondBinding})
	if err != nil || bytes.Equal(separate[0].Bytes(), changed) {
		t.Fatal("same logical path crossed a group root", err)
	}
	actual, err := os.ReadFile(operational)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatal("observation wrote operational bytes", err)
	}
}

func TestNHA055SourceReaderDistinguishesAbsenceEmptyAndPendingIntent(t *testing.T) {
	workspace, operational, _ := nha149Workspace(t)
	group, _ := storage.NewConfigDocumentID("dashboard.nha055_presence")
	binding := nha055ObservedBinding(t, "nha055.presence", "dashboard.yaml")
	absent, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{binding})
	if err != nil || absent[0].Present() || !absent[0].absent || absent[0].observation.hash != ([32]byte{}) {
		t.Fatal("missing source inferred an existing revision", err)
	}
	nha055ObservedFile(t, operational, group, binding, nil)
	empty, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{binding})
	if err != nil || !empty[0].Present() || empty[0].observation.hash != sha256.Sum256(nil) {
		t.Fatal("empty dirty candidate was confused with absence", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(operational), workspace.intent), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	pending, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{binding})
	if err != nil || !pending[0].observation.pending {
		t.Fatal("shared149 intent was ignored", err)
	}
}

func TestNHA055SourceReaderRejectsSymlinkDirectoriesLeavesAndSpecialFiles(t *testing.T) {
	for _, mode := range []string{"directory", "leaf", "directory-inside"} {
		t.Run(mode, func(t *testing.T) {
			workspace, operational, _ := nha149Workspace(t)
			group, _ := storage.NewConfigDocumentID("dashboard.nha055_noescape")
			binding := nha055ObservedBinding(t, "nha055.noescape", "nested/dashboard.yaml")
			location, _ := sourceMemberLocation(group, binding)
			physical := filepath.Join(filepath.Dir(operational), filepath.FromSlash(location))
			if err := os.MkdirAll(filepath.Dir(filepath.Dir(physical)), 0700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "dashboard.yaml"), []byte("private outside source"), 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "directory":
				if err := os.Symlink(outside, filepath.Dir(physical)); err != nil {
					t.Fatal(err)
				}
			case "directory-inside":
				if err := os.Symlink(filepath.Dir(operational), filepath.Dir(physical)); err != nil {
					t.Fatal(err)
				}
			case "leaf":
				if err := os.MkdirAll(filepath.Dir(physical), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "dashboard.yaml"), physical); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{binding}); err == nil {
				t.Fatal("symlink was followed by an admitted source read")
			}
			current, err := os.ReadFile(filepath.Join(outside, "dashboard.yaml"))
			if err != nil || string(current) != "private outside source" {
				t.Fatal("unsafe observation changed outside bytes", err)
			}
		})
	}
	workspace, operational, _ := nha149Workspace(t)
	group, _ := storage.NewConfigDocumentID("dashboard.nha055_directory_leaf")
	binding := nha055ObservedBinding(t, "nha055.directory_leaf", "dashboard.yaml")
	location, _ := sourceMemberLocation(group, binding)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(operational), filepath.FromSlash(location)), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{binding}); err == nil {
		t.Fatal("directory was treated as a regular source")
	}
	if err := os.Remove(filepath.Join(filepath.Dir(operational), filepath.FromSlash(location))); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(filepath.Dir(operational), filepath.FromSlash(location)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{binding}); err == nil {
		t.Fatal("FIFO was treated as a regular source")
	}
}

func TestNHA055SourceReaderEnforcesMemberAndByteBounds(t *testing.T) {
	workspace, operational, _ := nha149Workspace(t)
	group, _ := storage.NewConfigDocumentID("dashboard.nha055_bounds")
	binding := nha055ObservedBinding(t, "nha055.bounds", "dashboard.yaml")
	nha055ObservedFile(t, operational, group, binding, bytes.Repeat([]byte{'x'}, (1<<20)+1))
	if _, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{binding}); err == nil {
		t.Fatal("oversize member escaped bounded read")
	}
	nha055ObservedFile(t, operational, group, binding, []byte("views: []\n"))
	_, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{binding, binding})
	nha149Failure(t, err, FailureInvalid)
	_, err = workspace.ReadSources(group, make([]storage.ConfigDocumentBinding, 65))
	nha149Failure(t, err, FailureInvalid)
	for _, logical := range []string{"../outside.yaml", "/absolute.yaml", "a\\escape.yaml"} {
		id, _ := storage.NewConfigDocumentID("nha055.path")
		candidate, err := storage.NewConfigDocumentBinding(id, logical)
		if err == nil {
			if _, err := sourceMemberLocation(group, candidate); err == nil {
				t.Fatalf("unsafe logical path %s admitted", strings.ReplaceAll(logical, "\\", "/"))
			}
		}
	}
}

func TestNHA055SourceGroupStabilityRestartsForAnyIncludeHashDateOrPresence(t *testing.T) {
	workspace, operational, _ := nha149Workspace(t)
	group, _ := storage.NewConfigDocumentID("dashboard.nha055_batch")
	entry := nha055ObservedBinding(t, "nha055.batch.entry", "dashboard.yaml")
	include := nha055ObservedBinding(t, "nha055.batch.include", "includes/cards.yaml")
	nha055ObservedFile(t, operational, group, entry, []byte("views: []\n"))
	included := nha055ObservedFile(t, operational, group, include, []byte("[]\n"))
	observe := func() Observation {
		t.Helper()
		members, err := workspace.ReadSources(group, []storage.ConfigDocumentBinding{entry, include})
		if err != nil {
			t.Fatal(err)
		}
		target := sourceWatchTarget{group: group}
		for _, observed := range members {
			target.members = append(target.members, sourceWatchMember{binding: observed.binding, observed: observed})
		}
		return sourceTargetObservation(target)
	}
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	var stable stableObservation
	first := observe()
	if stable.admit(first, now) || stable.admit(first, now.Add(499*time.Millisecond)) {
		t.Fatal("include set entered before500ms")
	}
	info, err := os.Stat(included)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(included, []byte("# new exact include\n[]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(included, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	next := observe()
	if next == first || stable.admit(next, now.Add(500*time.Millisecond)) || !stable.admit(next, now.Add(time.Second)) {
		t.Fatal("include digest did not restart the whole stable batch")
	}
	if err := os.Chtimes(included, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	dated := observe()
	if dated == next || stable.admit(dated, now.Add(1500*time.Millisecond)) {
		t.Fatal("include date was ignored")
	}
	if err := os.Remove(included); err != nil {
		t.Fatal(err)
	}
	missing := observe()
	if missing == dated {
		t.Fatal("missing include reused a complete batch digest")
	}
}

func TestNHA055SourceWorkspaceGroupSeparatesOpenIdentitiesAcrossFamilies(t *testing.T) {
	dashboard, err := storage.DashboardSourceDescriptor("script.nha055_synthetic", "shared.yaml")
	if err != nil {
		t.Fatal(err)
	}
	script, err := storage.ScriptSourceDescriptor("script.nha055_synthetic", "shared.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dashboardGroup, err := SourceWorkspaceGroup(dashboard)
	if err != nil {
		t.Fatal(err)
	}
	scriptGroup, err := SourceWorkspaceGroup(script)
	if err != nil {
		t.Fatal(err)
	}
	if dashboardGroup == scriptGroup {
		t.Fatal("open dashboard identity selected another family's physical root")
	}
	moved, err := storage.DashboardSourceDescriptor("script.nha055_synthetic", "next.yaml")
	if err != nil {
		t.Fatal(err)
	}
	movedGroup, err := SourceWorkspaceGroup(moved)
	if err != nil || movedGroup != dashboardGroup {
		t.Fatal("changing entry changed an object's stable physical root", err)
	}
	long, err := storage.DashboardSourceDescriptor(strings.Repeat("a", 255), "dashboard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SourceWorkspaceGroup(long); err != nil {
		t.Fatal("open identity length was silently reduced by internal prefix", err)
	}
}
