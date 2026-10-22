package configdocuments

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/storage"
)

func nha055Member(t *testing.T, group, id, logical, raw string, expected, sql MemberExpectation) WorkspaceMember {
	t.Helper()
	groupID, _ := storage.NewConfigDocumentID(group)
	document, _ := storage.NewConfigDocumentID(id)
	binding, err := storage.NewConfigDocumentBinding(document, logical)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile(logical, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	member, err := NewWorkspaceMember(groupID, binding, expected, sql, file)
	if err != nil {
		t.Fatal(err)
	}
	return member
}

func TestNHA055WorkspaceSetAdmitsAbsenceAndPreservesExternalInclude(t *testing.T) {
	w, path, original := nha149Workspace(t)
	members := []WorkspaceMember{
		nha055Member(t, "helpers.main", "helpers.main", "helpers.yaml", "input_boolean: !include includes/booleans.yaml\n", ExpectAbsentMember(), ExpectAbsentMember()),
		nha055Member(t, "helpers.main", "helpers.booleans", "includes/booleans.yaml", "away_mode: {initial: false}\n", ExpectAbsentMember(), ExpectAbsentMember()),
	}
	edit, err := w.BeginSetEdit(members)
	if err != nil {
		t.Fatal(err)
	}
	if err := edit.Confirm(); err != nil {
		t.Fatal("complete include set did not confirm", err)
	}
	include := filepath.Join(filepath.Dir(path), filepath.FromSlash(members[1].location))
	external := []byte("# external editor owns these later bytes\naway_mode: {initial: true}\n")
	if err := os.WriteFile(include, external, 0600); err != nil {
		t.Fatal(err)
	}
	admitted := map[storage.ConfigDocumentID]MemberExpectation{}
	for _, member := range members {
		admitted[member.binding.ID()], _ = ExpectExistingMember(member.file.SHA256())
	}
	nha149Failure(t, edit.Finish(admitted, true), ReconciliationRequired)
	current, err := os.ReadFile(include)
	if err != nil || !bytes.Equal(current, external) {
		t.Fatal("multi-file finish overwrote an external include", err)
	}
	operational, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(operational, original) {
		t.Fatal("fixed server source root collided with the operational file", err)
	}
	observation, err := w.Observe()
	if err != nil || !observation.ReconciliationPending() {
		t.Fatal("uncertain source-set edit lost its shared intent", err)
	}
}

func TestNHA055WorkspaceSetChecksAllMembersBeforeAnyStaging(t *testing.T) {
	w, path, _ := nha149Workspace(t)
	members := []WorkspaceMember{
		nha055Member(t, "helpers.main", "helpers.main", "helpers.yaml", "input_boolean: {}\n", ExpectAbsentMember(), ExpectAbsentMember()),
		nha055Member(t, "helpers.main", "helpers.include", "include.yaml", "away_mode: {initial: false}\n", ExpectAbsentMember(), ExpectAbsentMember()),
	}
	collision := filepath.Join(filepath.Dir(path), filepath.FromSlash(members[1].location))
	if err := os.MkdirAll(filepath.Dir(collision), 0700); err != nil {
		t.Fatal(err)
	}
	external := []byte("# must remain owned by the external editor\n")
	if err := os.WriteFile(collision, external, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := w.BeginSetEdit(members)
	nha149Failure(t, err, Conflict)
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), filepath.FromSlash(members[0].location))); !os.IsNotExist(err) {
		t.Fatal("earlier member was written before admitting the later absence CAS", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".homeward-config-") {
			t.Fatal("failed set precondition created an intent or staging file")
		}
	}
	current, err := os.ReadFile(collision)
	if err != nil || sha256.Sum256(current) != sha256.Sum256(external) {
		t.Fatal("absence conflict overwrote an external source", err)
	}
}

func TestNHA055WorkspaceSetRejectsSymlinkDirectoryBeforeWriting(t *testing.T) {
	w, path, original := nha149Workspace(t)
	member := nha055Member(t, "helpers.main", "helpers.main", "nested/booleans.yaml", "away_mode: {initial: false}\n", ExpectAbsentMember(), ExpectAbsentMember())
	location := filepath.Join(filepath.Dir(path), filepath.FromSlash(member.location))
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(location)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(path), filepath.Dir(location)); err != nil {
		t.Fatal(err)
	}
	_, err := w.BeginSetEdit([]WorkspaceMember{member})
	nha149Failure(t, err, Conflict)
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("symlink directory redirected the configuration source write", err)
	}
}

func TestNHA055WorkspaceSetNamespacesTwoObjectsSharingNativeEntry(t *testing.T) {
	w, path, _ := nha149Workspace(t)
	members := []WorkspaceMember{
		nha055Member(t, "dashboard.first", "source.dashboard.first", "dashboard.yaml", "# first\nviews: []\n", ExpectAbsentMember(), ExpectAbsentMember()),
		nha055Member(t, "dashboard.second", "source.dashboard.second", "dashboard.yaml", "# second\nviews: []\n", ExpectAbsentMember(), ExpectAbsentMember()),
	}
	if members[0].location == members[1].location {
		t.Fatal("distinct source identities shared a physical locator")
	}
	edit, err := w.BeginSetEdit(members)
	if err != nil {
		t.Fatal(err)
	}
	if err := edit.Confirm(); err != nil {
		t.Fatal(err)
	}
	admitted := map[storage.ConfigDocumentID]MemberExpectation{}
	for _, member := range members {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), filepath.FromSlash(member.location)))
		if err != nil || !bytes.Equal(raw, member.file.Bytes()) {
			t.Fatal("source identity did not preserve its own exact native file", err)
		}
		admitted[member.binding.ID()], err = ExpectExistingMember(member.file.SHA256())
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := edit.Finish(admitted, true); err != nil {
		t.Fatal(err)
	}
}
