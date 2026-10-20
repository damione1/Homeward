package automationruns

import (
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/storage"
)

func TestNHA186ProvenanceAdmissionKeepsEarlierImage(t *testing.T) {
	scriptID, err := ir.ParseScriptID("script.nha186_fixture")
	if err != nil {
		t.Fatal(err)
	}
	script := nha042Compile(t, "script.nha186_fixture", "sequence:\n- stop: done\n", []ir.ScriptID{scriptID})
	base, err := NewPlanWithScripts(nil, []ir.Script{script})
	if err != nil {
		t.Fatal(err)
	}
	if base.Provenance().Kind() != UnboundFixturePlan || !base.Provenance().Valid() {
		t.Fatal("fixture origin is not explicit")
	}
	revisionA, _ := storage.NewConfigGenerationRevision(11)
	revisionB, _ := storage.NewConfigGenerationRevision(12)
	hashA, hashB := [32]byte{1}, [32]byte{2}
	stampA, err := NewBoundProgram(revisionA, hashA)
	if err != nil {
		t.Fatal(err)
	}
	stampB, err := NewBoundProgram(revisionB, hashB)
	if err != nil {
		t.Fatal(err)
	}
	imageA, err := base.WithProvenance(stampA)
	if err != nil {
		t.Fatal(err)
	}
	imageB, err := base.WithProvenance(stampB)
	if err != nil {
		t.Fatal(err)
	}
	if imageA.scripts[scriptID].provenance != stampA || imageB.scripts[scriptID].provenance != stampB ||
		base.scripts[scriptID].provenance.Kind() != UnboundFixturePlan {
		t.Fatal("hot image update changed an earlier admitted script")
	}
	if _, err := NewBoundProgram(storage.ConfigGenerationRevision{}, hashA); err == nil {
		t.Fatal("zero revision admitted")
	}
	if _, err := NewBoundProgram(revisionA, [32]byte{}); err == nil {
		t.Fatal("zero manifest admitted")
	}
	if _, err := base.WithProvenance(DefinitionProvenance{}); err == nil {
		t.Fatal("zero variant admitted")
	}
}
