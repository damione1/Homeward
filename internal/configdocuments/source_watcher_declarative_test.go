package configdocuments

import (
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA055DeclarativeWatcherCompilesOnlyTheClosedAdmittedVariant(t *testing.T) {
	group, err := storage.NewConfigDocumentID("nha055.declarative.arbitrary.group")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := parser.NewSourceFile("configuration.yaml", []byte("input_boolean: !include parts/boolean.yaml\n"))
	if err != nil {
		t.Fatal(err)
	}
	included, err := parser.NewSourceFile("parts/boolean.yaml", []byte("ready: {initial: false}\n"))
	if err != nil {
		t.Fatal(err)
	}
	files := []parser.SourceFile{entry, included}
	target := sourceWatchTarget{group: group, entry: entry.Path(), members: make([]sourceWatchMember, len(files))}
	for _, variant := range []sourceWatchVariant{sourceWatchDeclarative1, sourceWatchDeclarative2} {
		target.variant = variant
		candidate, err := compileObservedDeclarative(target, files)
		if err != nil || !candidate.Valid() || candidate.Entry() != entry.Path() || len(candidate.Sources()) != 2 {
			t.Fatal("real bundle compiler was not selected", err)
		}
	}
	for _, variant := range []sourceWatchVariant{sourceWatchInvalid, sourceWatchNative, sourceWatchPolicy, sourceWatchVariant(255)} {
		target.variant = variant
		if _, err := compileObservedDeclarative(target, files); err == nil {
			t.Fatal("nondeclarative variant selected a bundle compiler")
		}
	}
	target.variant = sourceWatchDeclarative1
	if _, err := compileObservedDeclarative(target, files[:1]); err == nil {
		t.Fatal("incomplete include set was admitted")
	}
	reserved, err := storage.NewConfigDocumentID("source.nha055-reserved")
	if err != nil {
		t.Fatal(err)
	}
	target.group = reserved
	if _, err := compileObservedDeclarative(target, files); err == nil {
		t.Fatal("declarative writer could occupy a native namespace")
	}
	target.group = group
	target.entry = "missing-entry.yaml"
	if _, err := compileObservedDeclarative(target, files); err == nil {
		t.Fatal("missing entry selected another member")
	}
}
