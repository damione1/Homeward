package configplans

import (
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/scope"
	"testing"
)

func TestNHA055NativeOwnershipJSONRemainsClosedToItsFamily(t *testing.T) {
	file, err := parser.NewJSONSourceFile("ownership.json", scope.HeatingOwnershipV1())
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewCandidate(Ownership, "heating.ownership", "ownership.json", []parser.SourceFile{file})
	if err != nil || !candidate.Valid() {
		t.Fatal("native ownership snapshot did not enter exact candidate", err)
	}
	if _, err := NewCandidate(Automation, "automation.nha055", "ownership.json", []parser.SourceFile{file}); err == nil {
		t.Fatal("native JSON widened unrelated YAML family")
	}
}
