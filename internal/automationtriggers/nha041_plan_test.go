package automationtriggers

import (
	"reflect"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

func nha041PlanAutomation(t *testing.T, id string) ir.Automation {
	t.Helper()
	entity, err := ir.NewEntity("switch.fixture", ir.ReferencePresent, ir.SupportSwitchTurnOn)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := ir.NewCatalogue([]ir.Entity{entity}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.NewSourceFile(id+".yaml", []byte("id: "+id+"\ntriggers:\n- trigger: time_pattern\n  seconds: '*'\n- trigger: time_pattern\n  seconds: '/2'\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := ir.CompileSources("automation."+id, id+".yaml", []parser.SourceFile{file}, catalogue)
	if err != nil || len(compiled.Accepted()) != 1 {
		t.Fatalf("compile %q: %v rejected=%+v", id, err, compiled.Rejected())
	}
	return compiled.Accepted()[0]
}

func TestNHA041AutomationIDsAreUniqueSortedAndOwned(t *testing.T) {
	entity, err := domain.ParseEntityID("Host:unused")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(PlanInput{
		Automations: []ir.Automation{nha041PlanAutomation(t, "zeta"), nha041PlanAutomation(t, "alpha")},
		Zone:        "America/Toronto", Resolver: nha040Resolver{entity: entity},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "zeta"}
	first := plan.AutomationIDs()
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("IDs not unique/sorted: %v", first)
	}
	first[0] = "mutated"
	if got := plan.AutomationIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("caller mutated plan through ID slice: %v", got)
	}
	if got := (Plan{}).AutomationIDs(); got != nil {
		t.Fatalf("invalid plan exposed IDs: %v", got)
	}
}
