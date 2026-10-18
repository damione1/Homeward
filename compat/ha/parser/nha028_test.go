package parser_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
)

func TestNHA028SnapshotAdmissionBoundedBeforeCopy(t *testing.T) {
	if _, err := parser.NewSourceFile(strings.Repeat("a", 4097)+".yaml", []byte("input_boolean: {}\n")); err == nil {
		t.Fatal("oversized path accepted")
	}
	for _, path := range []string{"bad\x00name.yaml", "bad\xffname.yaml"} {
		if _, err := parser.NewSourceFile(path, []byte("input_boolean: {}\n")); err == nil {
			t.Fatalf("invalid path accepted: %q", path)
		}
	}
	entry, err := parser.NewSourceFile("configuration.yaml", []byte("input_boolean: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	mutable := []byte("input_boolean: {}\n")
	detached, err := parser.NewSourceFile("detached.yaml", mutable)
	if err != nil {
		t.Fatal(err)
	}
	mutable[0] = 'X'
	if detached.Bytes()[0] != 'i' {
		t.Fatal("source snapshot retained mutable caller bytes")
	}
	files := []parser.SourceFile{entry}
	for i := 0; i < 256; i++ {
		f, err := parser.NewSourceFile(fmt.Sprintf("unused/%03d.yaml", i), []byte("counter: {}\n"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	_, err = parser.LoadSources("configuration.yaml", files, parser.SyntaxProfileDeclarative)
	var d *parser.Diagnostic
	if !errors.As(err, &d) || d.Code != parser.CodeSizeLimit {
		t.Fatalf("source count: %v", err)
	}
	_, err = parser.LoadSources("configuration.yaml", files[:2], parser.SyntaxProfileDeclarative)
	if !errors.As(err, &d) || d.Code != parser.CodeUnsupportedForm {
		t.Fatalf("unused source: %v", err)
	}
	large := []parser.SourceFile{entry}
	for i := 0; i < 5; i++ {
		f, err := parser.NewSourceFile(fmt.Sprintf("large/%d.yaml", i), bytes.Repeat([]byte("x"), 900_000))
		if err != nil {
			t.Fatal(err)
		}
		large = append(large, f)
	}
	_, err = parser.LoadSources("configuration.yaml", large, parser.SyntaxProfileDeclarative)
	if !errors.As(err, &d) || d.Code != parser.CodeSizeLimit {
		t.Fatalf("snapshot byte budget: %v", err)
	}
}

func TestNHA028DeclarativeVocabularyCoverage(t *testing.T) {
	for family := parser.FamilyAutomation; family <= parser.FamilyGroup; family++ {
		if family.Key() == "" || parser.FamilyFromKey(family.Key()) != family {
			t.Fatalf("family %d has no unique spelling", family)
		}
	}
	for field := parser.FieldInitial; field <= parser.FieldAll; field++ {
		if field.Key() == "" || parser.DeclarativeFieldFromKey(field.Key()) != field {
			t.Fatalf("field %d has no unique spelling", field)
		}
	}
}
