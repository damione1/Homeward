package configplans

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

func nha055DefinitionRollbackMember(t *testing.T, family Family, id, path, raw string) rollbackDefinitionMember {
	t.Helper()
	document, err := storage.NewConfigDocumentID(id)
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
	schema, profile := bundle.SchemaDeclarative1, parser.SyntaxProfileDeclarative
	if family == Declarative2 {
		schema, profile = bundle.SchemaDeclarative2, parser.SyntaxProfileDeclarative2
	}
	return rollbackDefinitionMember{ref: ref, path: path, hash: sha256.Sum256([]byte(raw)), raw: []byte(raw), schema: schema, profile: profile}
}
func nha055DefinitionRollbackExpected(t *testing.T, members ...rollbackDefinitionMember) []configdocuments.ExpectedSource {
	t.Helper()
	var result []configdocuments.ExpectedSource
	for _, member := range members {
		binding, err := storage.NewConfigDocumentBinding(member.ref.Document(), member.path)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := storage.NewConfigVersionRevision(17)
		if err != nil {
			t.Fatal(err)
		}
		// Historical revision3 is NOT a CAS on current SQL version17.
		value, err := configdocuments.NewExpectedSource(binding, revision, sha256.Sum256([]byte("current immutable SQL bytes")), false, configdocuments.ExpectAbsentMember())
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	return result
}
func nha055DefinitionRollbackLocator(t *testing.T, identity string) storage.DeclarativeWorkspaceLocator {
	t.Helper()
	group, err := storage.NewConfigDocumentID(identity)
	if err != nil {
		t.Fatal(err)
	}
	locator, err := storage.NewDeclarativeWorkspaceLocator(group)
	if err != nil {
		t.Fatal(err)
	}
	return locator
}

func TestNHA055RollbackDefinitionsPreservesExactHistoricalMembersAndCurrentPreconditions(t *testing.T) {
	for _, family := range []Family{Declarative1, Declarative2} {
		t.Run(family.Schema(), func(t *testing.T) {
			entry := nha055DefinitionRollbackMember(t, family, "document.entry", "entry.yaml", "# historical exact entry\ninput_boolean: !include parts/booleans.yaml\n")
			include := nha055DefinitionRollbackMember(t, family, "document.include", "parts/booleans.yaml", "# exact historical include\naway: {initial: false}\n")
			expected := nha055DefinitionRollbackExpected(t, include, entry)
			before := append([]configdocuments.ExpectedSource(nil), expected...)
			candidate, err := rollbackDefinitionCandidate(family, "workspace.identity", entry.path, nha055DefinitionRollbackLocator(t, "workspace.identity"), []rollbackDefinitionMember{include, entry}, expected)
			if err != nil || !candidate.Valid() || candidate.family != family || candidate.identity != "workspace.identity" || candidate.entry != entry.path || len(candidate.files) != 2 {
				t.Fatal("exact historical definition set was not restored", err)
			}
			for _, member := range []rollbackDefinitionMember{entry, include} {
				found := false
				for _, file := range candidate.files {
					if file.Path() == member.path {
						found = bytes.Equal(file.Bytes(), member.raw) && file.SHA256() == member.hash
					}
				}
				if !found {
					t.Fatal("source bytes/path/membership changed")
				}
			}
			for i := range expected {
				if expected[i] != before[i] {
					t.Fatal("current SQL/workspace preconditions were replaced by historical refs")
				}
			}
			if _, err := rollbackDefinitionCandidate(family, "workspace.different", entry.path, nha055DefinitionRollbackLocator(t, "workspace.identity"), []rollbackDefinitionMember{entry, include}, expected); !errors.Is(err, ErrInvalid) {
				t.Fatal("recorded namespace was silently changed", err)
			}
		})
	}
}

func TestNHA055RollbackDefinitionsUnobservedHistoryUsesOnlyExplicitRequestNamespace(t *testing.T) {
	entry := nha055DefinitionRollbackMember(t, Declarative1, "document.unrelated", "entry.yaml", "input_boolean: {}\n")
	expected := nha055DefinitionRollbackExpected(t, entry)
	candidate, err := rollbackDefinitionCandidate(Declarative1, "new.explicit.namespace", entry.path, storage.NoDeclarativeWorkspaceLocator(), []rollbackDefinitionMember{entry}, expected)
	if err != nil || candidate.identity != "new.explicit.namespace" || candidate.identity == entry.ref.Document().String() {
		t.Fatal("old unobserved history inferred a group from a document", err)
	}
	if _, err := rollbackDefinitionCandidate(Declarative1, "", entry.path, storage.NoDeclarativeWorkspaceLocator(), []rollbackDefinitionMember{entry}, expected); !errors.Is(err, ErrInvalid) {
		t.Fatal("absent request identity acquired a guessed namespace", err)
	}
	if _, err := rollbackDefinitionCandidate(Declarative1, "new.explicit.namespace", entry.path, storage.DeclarativeWorkspaceLocator{}, []rollbackDefinitionMember{entry}, expected); !errors.Is(err, ErrUnavailable) {
		t.Fatal("invalid locator was confused with admitted absence", err)
	}
}

func TestNHA055RollbackDefinitionsRejectsWrongFamiliesSchemasReferencesAndPaths(t *testing.T) {
	entry := nha055DefinitionRollbackMember(t, Declarative1, "document.entry", "entry.yaml", "input_boolean: {}\n")
	part := nha055DefinitionRollbackMember(t, Declarative1, "document.part", "parts/part.yaml", "{}\n")
	for _, test := range []string{"unknown family", "wrong family", "mixed schema", "unknown schema", "wrong profile", "invalid ref", "duplicate ID", "duplicate path", "missing entry", "hash mismatch", "traversal", "absolute path", "missing member", "extra member", "wrong current binding"} {
		t.Run(test, func(t *testing.T) {
			family, root := Declarative1, entry.path
			members := []rollbackDefinitionMember{entry, part}
			expected := nha055DefinitionRollbackExpected(t, members...)
			switch test {
			case "unknown family":
				family = Family(255)
			case "wrong family":
				family = Declarative2
			case "mixed schema":
				members[1].schema = bundle.SchemaDeclarative2
				members[1].profile = parser.SyntaxProfileDeclarative2
			case "unknown schema":
				members[1].schema = bundle.Schema(255)
			case "wrong profile":
				members[1].profile = parser.SyntaxProfileCore
			case "invalid ref":
				members[1].ref = storage.ConfigVersionRef{}
			case "duplicate ID":
				members[1].ref = members[0].ref
			case "duplicate path":
				members[1].path = members[0].path
			case "missing entry":
				root = "not-retained.yaml"
			case "hash mismatch":
				members[1].hash[0] ^= 1
			case "traversal":
				members[1].path = "../escape.yaml"
			case "absolute path":
				members[1].path = "/tmp/escape.yaml"
			case "missing member":
				expected = expected[:1]
			case "extra member":
				expected = append(expected, expected[0])
			case "wrong current binding":
				expected[1] = expected[0]
			}
			if candidate, err := rollbackDefinitionCandidate(family, "workspace.test", root, storage.NoDeclarativeWorkspaceLocator(), members, expected); err == nil || candidate.Valid() {
				t.Fatal("invalid history/preconditions returned a partial candidate", test)
			}
		})
	}
}

func nha055DefinitionSizedSource(t *testing.T, path string, size int) parser.SourceFile {
	t.Helper()
	raw := bytes.Repeat([]byte{'#'}, size)
	if size > 0 {
		raw[size-1] = '\n'
	}
	file, err := parser.NewSourceFile(path, raw)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// Domain history is bounded by the existing 4MiB manifest, independently from
// the unchanged 1MiB wire/Connect limit. Each file remains bounded by 1MiB.
func TestNHA055RollbackDefinitionCandidateDomainAllowsFourMiBWithOneMiBMembers(t *testing.T) {
	files := []parser.SourceFile{
		nha055DefinitionSizedSource(t, "entry.yaml", 1<<20),
		nha055DefinitionSizedSource(t, "part-a.yaml", 1<<20),
		nha055DefinitionSizedSource(t, "part-b.yaml", 1<<20),
		nha055DefinitionSizedSource(t, "part-c.yaml", 1<<20),
	}
	if candidate, err := NewCandidate(Declarative1, "workspace.large", "entry.yaml", files); err != nil || !candidate.Valid() {
		t.Fatal("exact four-MiB admitted domain history was lost", err)
	}
	tooMuch := append(append([]parser.SourceFile(nil), files...), nha055DefinitionSizedSource(t, "extra.yaml", 1))
	if _, err := NewCandidate(Declarative1, "workspace.large", "entry.yaml", tooMuch); !errors.Is(err, ErrInvalid) {
		t.Fatal("domain exceeded four MiB", err)
	}
	// A SourceFile cannot represent an oversized member: its constructor
	// rejects it before NewCandidate can run. Check that real boundary and
	// the raw historical-member boundary without manufacturing an invalid file.
	oversizedRaw := bytes.Repeat([]byte{'#'}, (1<<20)+1)
	oversizedRaw[len(oversizedRaw)-1] = '\n'
	oversizedFile, snapshotErr := parser.NewSourceFile("entry.yaml", oversizedRaw)
	var snapshotDiagnostic *parser.Diagnostic
	if !errors.As(snapshotErr, &snapshotDiagnostic) || snapshotDiagnostic.Code != parser.CodeInvalidEntry || oversizedFile.Path() != "" || oversizedFile.ByteLen() != 0 {
		t.Fatal("one-MiB-plus-one snapshot was not rejected atomically", snapshotErr)
	}
	oversizedMember := nha055DefinitionRollbackMember(t, Declarative1, "document.oversized", "entry.yaml", string(oversizedRaw))
	if candidate, err := rollbackDefinitionCandidate(Declarative1, "workspace.large", "entry.yaml", storage.NoDeclarativeWorkspaceLocator(), []rollbackDefinitionMember{oversizedMember}, nha055DefinitionRollbackExpected(t, oversizedMember)); !errors.Is(err, ErrUnavailable) || candidate.Valid() {
		t.Fatal("historical per-file one-MiB bound was relaxed", err)
	}
	var members []rollbackDefinitionMember
	for i, file := range files {
		members = append(members, nha055DefinitionRollbackMember(t, Declarative1, fmt.Sprintf("document.large.%d", i), file.Path(), string(file.Bytes())))
	}
	if candidate, err := rollbackDefinitionCandidate(Declarative1, "workspace.large", "entry.yaml", storage.NoDeclarativeWorkspaceLocator(), members, nha055DefinitionRollbackExpected(t, members...)); err != nil || !candidate.Valid() {
		t.Fatal("exact source set above one MiB did not enter ordinary candidate admission", err)
	}
	members = append(members, nha055DefinitionRollbackMember(t, Declarative1, "document.extra", "extra.yaml", "#"))
	if _, err := rollbackDefinitionCandidate(Declarative1, "workspace.large", "entry.yaml", storage.NoDeclarativeWorkspaceLocator(), members, nha055DefinitionRollbackExpected(t, members...)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("historical selection exceeded four MiB", err)
	}
}

func TestNHA055RollbackDefinitionsKeepsThe64MemberBound(t *testing.T) {
	var members []rollbackDefinitionMember
	for index := 0; index < 64; index++ {
		members = append(members, nha055DefinitionRollbackMember(t, Declarative1, fmt.Sprintf("document.%d", index), fmt.Sprintf("part-%d.yaml", index), "#\n"))
	}
	expected := nha055DefinitionRollbackExpected(t, members...)
	if candidate, err := rollbackDefinitionCandidate(Declarative1, "workspace.64", members[0].path, storage.NoDeclarativeWorkspaceLocator(), members, expected); err != nil || !candidate.Valid() {
		t.Fatal("exact 64-member set was lost", err)
	}
	members = append(members, nha055DefinitionRollbackMember(t, Declarative1, "document.65", "part-65.yaml", "#\n"))
	if _, err := rollbackDefinitionCandidate(Declarative1, "workspace.64", members[0].path, storage.NoDeclarativeWorkspaceLocator(), members, nha055DefinitionRollbackExpected(t, members...)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("historical set exceeded 64 members", err)
	}
	var files []parser.SourceFile
	for _, member := range members {
		file, err := parser.NewSourceFile(member.path, member.raw)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if _, err := NewCandidate(Declarative1, "workspace.64", members[0].path, files); !errors.Is(err, ErrInvalid) {
		t.Fatal("domain 64-member bound was relaxed", err)
	}
}
