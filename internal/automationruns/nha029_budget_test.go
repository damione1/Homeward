package automationruns

import (
	"context"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/internal/automationtriggers"
)

// A second candidate cannot spend the memory budget or compile before the
// first candidate's one-owner reservation has been rejected.
func TestNHA029PendingImageRefusedBeforePoolAdmission(t *testing.T) {
	id, err := ir.ParseScriptID("script.nha029_budget")
	if err != nil {
		t.Fatal(err)
	}
	script := nha042Compile(t, "script.nha029_budget", "sequence:\n"+nha042Action("child_enter"), []ir.ScriptID{id})
	conditions := nha042SharedConditions(t)
	newImage := func() (Plan, *Interpreter) {
		t.Helper()
		plan, err := NewPlanWithScripts(nil, []ir.Script{script})
		if err != nil {
			t.Fatal(err)
		}
		interpreter, err := NewInterpreter(InterpreterInput{Plan: plan, Conditions: conditions,
			Commands: &nha029ImageCommands{entered: make(chan nha029ImageCall, 1), release: make(map[RunID]chan struct{})},
			Clock:    automationtriggers.RealClock{}, Zone: time.UTC})
		if err != nil {
			t.Fatal(err)
		}
		return plan, interpreter
	}
	planA, interpreterA := newImage()
	controller, err := NewController(planA, nil, interpreterA, interpreterA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Close(context.Background()) })
	planB, interpreterB := newImage()
	first, err := controller.ReserveImage(planB, interpreterB, interpreterB)
	if err != nil {
		t.Fatal(err)
	}
	planC, interpreterC := newImage()
	controller.pool.mu.Lock()
	remaining := MaxMemoryBytes - controller.pool.used
	controller.pool.mu.Unlock()
	if remaining <= 1 {
		t.Fatalf("fixture has no budget to saturate: %d", remaining)
	}
	padding := remaining - 1
	if !controller.pool.adjust(0, padding) {
		t.Fatal("fixture could not reserve its competing memory charge")
	}
	second, admissionErr := controller.ReserveImage(planC, interpreterC, interpreterC)
	if second != nil || admissionErr != ErrInvalidPlan {
		t.Errorf("pending image was not refused before pool admission: reservation=%v err=%v", second, admissionErr)
	}
	if second != nil {
		if err := second.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if !controller.pool.adjust(padding, 0) {
		t.Fatal("fixture memory charge could not be released")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	fresh, err := controller.ReserveImage(planC, interpreterC, interpreterC)
	if err != nil {
		t.Fatalf("capacity did not return after release: %v", err)
	}
	if err := fresh.Release(); err != nil {
		t.Fatal(err)
	}
}
