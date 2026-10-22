package configplans

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/logicselection"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/scope"
)

func nha055RollbackMember(t *testing.T, family Family, identity, entry, id, path, raw string) rollbackSourceMember {
	t.Helper()
	candidate := Candidate{family: family, identity: identity, entry: entry}
	descriptor, err := editableDescriptor(candidate)
	if err != nil {
		t.Fatal(err)
	}
	document, err := storage.NewConfigDocumentID(id)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.NewConfigDocumentBinding(document, path)
	if family == Ownership {
		binding, err = storage.NewJSONConfigDocumentBinding(document, path)
	}
	if err != nil {
		t.Fatal(err)
	}
	file, err := newFamilySource(family, path, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	revision, err := storage.NewConfigVersionRevision(3)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := storage.NewConfigVersionRef(document, revision)
	if err != nil {
		t.Fatal(err)
	}
	return rollbackSourceMember{descriptor: descriptor, binding: binding, ref: ref, file: file}
}

func nha055RollbackExpected(t *testing.T, members ...rollbackSourceMember) []configdocuments.ExpectedSource {
	t.Helper()
	var result []configdocuments.ExpectedSource
	for _, member := range members {
		// Current SQL version 11 is deliberately different from historical
		// version 3; workspace absence is independent of SQL existence.
		revision, err := storage.NewConfigVersionRevision(11)
		if err != nil {
			t.Fatal(err)
		}
		current, err := configdocuments.NewExpectedSource(member.binding, revision, sha256.Sum256([]byte("current SQL bytes")), false, configdocuments.ExpectAbsentMember())
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, current)
	}
	return result
}

func TestNHA055RollbackSourcesSelectsExactGroupWithSameNativeEntry(t *testing.T) {
	first := nha055RollbackMember(t, Dashboard, "dashboard.first", "dashboard.yaml", "source.dashboard.first", "dashboard.yaml", "# historical first exact bytes\nviews: !include parts/views.yaml\n")
	include := nha055RollbackMember(t, Dashboard, "dashboard.first", "dashboard.yaml", "source.dashboard.first.views", "parts/views.yaml", "# first include\n[]\n")
	second := nha055RollbackMember(t, Dashboard, "dashboard.second", "dashboard.yaml", "source.dashboard.second", "dashboard.yaml", "# historical second exact bytes\nviews: []\n")
	values := []rollbackSourceMember{second, include, first}
	for _, selected := range []struct {
		identity string
		members  []rollbackSourceMember
	}{{"dashboard.first", []rollbackSourceMember{first, include}}, {"dashboard.second", []rollbackSourceMember{second}}} {
		t.Run(selected.identity, func(t *testing.T) {
			expected := nha055RollbackExpected(t, selected.members...)
			before := append([]configdocuments.ExpectedSource(nil), expected...)
			candidate, err := rollbackSourceCandidate(Dashboard, selected.identity, values, expected)
			if err != nil || candidate.entry != "dashboard.yaml" || len(candidate.files) != len(selected.members) {
				t.Fatal("rollback confused entry path with group identity", err)
			}
			for _, member := range selected.members {
				found := false
				for _, file := range candidate.files {
					if file.Path() == member.file.Path() {
						found = bytes.Equal(file.Bytes(), member.file.Bytes()) && file.SHA256() == member.file.SHA256()
					}
				}
				if !found {
					t.Fatal("rollback omitted or normalized a historical member")
				}
			}
			for i := range expected {
				if expected[i] != before[i] {
					t.Fatal("historical revision replaced a current SQL/workspace precondition")
				}
			}
		})
	}
	if _, err := rollbackSourceCandidate(Dashboard, "dashboard.missing", values, nha055RollbackExpected(t, first)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("absent target borrowed another group/default source", err)
	}
	if _, err := rollbackSourceCandidate(Script, "dashboard.first", values, nha055RollbackExpected(t, first)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("family mismatch selected an entry-path match", err)
	}
}

func TestNHA055RollbackSourcesRejectsCrossGroupBindingsAndBrokenReferences(t *testing.T) {
	first := nha055RollbackMember(t, Dashboard, "dashboard.first", "dashboard.yaml", "source.dashboard.first", "dashboard.yaml", "views: []\n")
	other := nha055RollbackMember(t, Dashboard, "dashboard.second", "dashboard.yaml", "source.dashboard.second", "dashboard.yaml", "views: []\n")
	if _, err := rollbackSourceCandidate(Dashboard, "dashboard.first", []rollbackSourceMember{first, other}, nha055RollbackExpected(t, other)); !errors.Is(err, ErrInvalid) {
		t.Fatal("current precondition stole another group's document binding", err)
	}
	broken := first
	broken.ref = other.ref
	if _, err := rollbackSourceCandidate(Dashboard, "dashboard.first", []rollbackSourceMember{broken}, nha055RollbackExpected(t, first)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("historical source/ref cross-reference was ignored", err)
	}
	if _, err := rollbackSourceCandidate(Dashboard, "dashboard.first", []rollbackSourceMember{first, first}, nha055RollbackExpected(t, first, first)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("duplicate historical member was accepted", err)
	}
}

func TestNHA055RollbackSourcesFiveFamiliesKeepNativeBytes(t *testing.T) {
	for _, test := range []struct {
		family               Family
		identity, entry, raw string
	}{
		{Dashboard, "dashboard.rollback", "dashboard.yaml", "# private rollback dashboard\nviews: []\n"},
		{Automation, "automation.rollback", "automation.yaml", "# private rollback automation\nid: rollback\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - stop: complete\n"},
		{Script, "script.rollback", "script.yaml", "# private rollback script\nsequence:\n - stop: complete\n"},
		{Scene, "scene.ceilling_fan_night", "scene.yaml", "# private rollback scene\nid: fixture\nname: Preview\nentities:\n switch.arc_soundbar_crossfade: 'off'\n"},
		{Ownership, "heating.ownership", "ownership.json", string(scope.HeatingOwnershipV1())},
	} {
		t.Run(test.identity, func(t *testing.T) {
			member := nha055RollbackMember(t, test.family, test.identity, test.entry, "source."+test.identity, test.entry, test.raw)
			candidate, err := rollbackSourceCandidate(test.family, test.identity, []rollbackSourceMember{member}, nha055RollbackExpected(t, member))
			if err != nil || candidate.entry != test.entry || len(candidate.files) != 1 || !bytes.Equal(candidate.files[0].Bytes(), []byte(test.raw)) {
				t.Fatal("native rollback source bytes or descriptor changed", err)
			}
		})
	}
}

func TestNHA055RollbackSourcesStillRejectsAll72DeferredIdentitiesAtCurrentCompiler(t *testing.T) {
	selection, err := logicselection.LoadV1()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	deferred := 0
	for _, entry := range selection.Entries() {
		if entry.Disposition() != logicselection.DispositionDeferred {
			continue
		}
		identity, err := entry.ID().Value()
		if err != nil {
			t.Fatal(err)
		}
		// A syntactically harmless source must not bypass a deferred identity
		// merely because its historical bytes are retained and authentic.
		family, path, raw := Automation, "automation.yaml", "id: rollback\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - stop: complete\n"
		if strings.HasPrefix(identity, "script.") {
			family, path, raw = Script, "script.yaml", "sequence:\n - stop: complete\n"
		} else if strings.HasPrefix(identity, "scene.") {
			family, path, raw = Scene, "scene.yaml", "id: fixture\nname: Rollback\nentities:\n switch.arc_soundbar_crossfade: 'off'\n"
		}
		member := nha055RollbackMember(t, family, identity, path, "source."+identity, path, raw)
		candidate, err := rollbackSourceCandidate(family, identity, []rollbackSourceMember{member}, nha055RollbackExpected(t, member))
		if err != nil {
			t.Fatal(err)
		}
		group, err := configactivation.NewSourceGroup(member.descriptor, candidate.files, []storage.ConfigDocumentBinding{member.binding})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := configactivation.CompileSourceGroups([]configactivation.SourceGroup{group}, manifest, catalogue); err == nil {
			t.Fatal("historical rollback widened the deferred identity partition", identity)
		}
		deferred++
	}
	if deferred != 72 {
		t.Fatalf("expected all 72 deferred identities, got %d", deferred)
	}
}

// This uses the actual 149 filesystem writer with synthetic files; it needs no
// PostgreSQL, Host, scheduler, run engine or physical command port.
func TestNHA055RollbackSourcesRechecksWorkspaceCASAndPreservesUntargetedInclude(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "configuration.yaml")
	if err := os.WriteFile(path, []byte("external_workers: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := configdocuments.OpenWorkspace(path, "configuration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	member := nha055RollbackMember(t, Dashboard, "dashboard.rollback", "dashboard.yaml", "source.dashboard.rollback", "dashboard.yaml", "# current\nviews: []\n")
	group, _ := storage.NewConfigDocumentID(member.descriptor.Identity())
	initial, err := configdocuments.NewWorkspaceMember(group, member.binding, configdocuments.ExpectAbsentMember(), configdocuments.ExpectAbsentMember(), member.file)
	if err != nil {
		t.Fatal(err)
	}
	edit, err := workspace.BeginSetEdit([]configdocuments.WorkspaceMember{initial})
	if err != nil {
		t.Fatal(err)
	}
	existing, err := configdocuments.ExpectExistingMember(member.file.SHA256())
	if err != nil {
		t.Fatal(err)
	}
	if err := edit.Finish(map[storage.ConfigDocumentID]configdocuments.MemberExpectation{member.binding.ID(): existing}, true); err != nil {
		t.Fatal(err)
	}
	groups, err := os.ReadDir(filepath.Join(root, "configuration.d"))
	if err != nil || len(groups) != 1 {
		t.Fatal("source namespace was not created", err)
	}
	target := filepath.Join(root, "configuration.d", groups[0].Name(), "dashboard.yaml")
	external := []byte("# externally edited after current precondition\nviews: []\n")
	if err := os.WriteFile(target, external, 0600); err != nil {
		t.Fatal(err)
	}
	untargeted := filepath.Join(filepath.Dir(target), "unused-include.yaml")
	untargetedBytes := []byte("# non-target file must survive exact SQL membership replacement\n")
	if err := os.WriteFile(untargeted, untargetedBytes, 0600); err != nil {
		t.Fatal(err)
	}
	historical, err := parser.NewSourceFile("dashboard.yaml", []byte("# historical\nviews: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	restore, err := configdocuments.NewWorkspaceMember(group, member.binding, existing, existing, historical)
	if err != nil {
		t.Fatal(err)
	}
	if edit, err := workspace.BeginSetEdit([]configdocuments.WorkspaceMember{restore}); err == nil || edit != nil {
		t.Fatal("stale target file CAS was admitted")
	}
	for file, want := range map[string][]byte{target: external, untargeted: untargetedBytes} {
		got, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("rollback refusal overwrote an external file", err)
		}
	}
	// A caller that has observed the new workspace bytes can submit a fresh
	// workspace CAS without changing the independent current SQL expectation.
	observed, err := configdocuments.ExpectExistingMember(sha256.Sum256(external))
	if err != nil {
		t.Fatal(err)
	}
	restore, err = configdocuments.NewWorkspaceMember(group, member.binding, observed, existing, historical)
	if err != nil {
		t.Fatal(err)
	}
	edit, err = workspace.BeginSetEdit([]configdocuments.WorkspaceMember{restore})
	if err != nil {
		t.Fatal("fresh workspace observation did not admit restoration", err)
	}
	restored, err := configdocuments.ExpectExistingMember(historical.SHA256())
	if err != nil {
		t.Fatal(err)
	}
	if err := edit.Finish(map[storage.ConfigDocumentID]configdocuments.MemberExpectation{member.binding.ID(): restored}, true); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string][]byte{target: historical.Bytes(), untargeted: untargetedBytes} {
		got, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("exact restore touched an untargeted include or changed historical bytes", err)
		}
	}
}

// Historical bytes are data. The same current compiler that admits ordinary
// ApplySources must reject a capability contraction and a current tombstone.
func TestNHA055RollbackSourcesReadmitsCurrentCapabilitiesAndTombstones(t *testing.T) {
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	member := nha055RollbackMember(t, Automation, "automation.nha055_rollback_action", "automation.yaml", "source.automation.nha055.rollback", "automation.yaml", "id: rollback_action\ntriggers:\n - trigger: time\n   at: '18:00:00'\nactions:\n - action: switch.turn_on\n   target: {entity_id: switch.chauffe_eau}\n   data: {}\n")
	candidate, err := rollbackSourceCandidate(Automation, member.descriptor.Identity(), []rollbackSourceMember{member}, nha055RollbackExpected(t, member))
	if err != nil {
		t.Fatal(err)
	}
	group, err := configactivation.NewSourceGroup(member.descriptor, candidate.files, []storage.ConfigDocumentBinding{member.binding})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		status       ir.ReferenceStatus
		support      []ir.DeclaredSupport
		wantAdmitted bool
	}{
		{"current writable", ir.ReferencePresent, []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportSwitchTurnOn}, true},
		{"current contraction", ir.ReferencePresent, []ir.DeclaredSupport{ir.SupportObserveState}, false},
		{"current tombstone", ir.ReferenceExcluded, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			entity, err := ir.NewEntity("switch.chauffe_eau", test.status, test.support...)
			if err != nil {
				t.Fatal(err)
			}
			catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			compiled, err := configactivation.CompileSourceGroups([]configactivation.SourceGroup{group}, manifest, catalogue)
			if test.wantAdmitted {
				if err != nil || !compiled.Valid() || len(compiled.Automations()) != 1 {
					t.Fatal("control historical action did not compile against a current declaration", err)
				}
			} else if err == nil || compiled.Valid() {
				t.Fatal("historical source overrode current admission", test.name)
			}
		})
	}
}
