package ir_test

import (
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
)

func nha042ScriptCatalogue(t *testing.T) ir.Catalogue {
	t.Helper()
	catalogue, err := ir.NewCatalogue(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent, _ := ir.ParseScriptID("script.nha042_parent")
	child, _ := ir.ParseScriptID("script.nha042_child")
	catalogue, err = catalogue.WithScripts([]ir.ScriptID{parent, child})
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func nha042CompileScript(t *testing.T, body string) ir.ScriptResult {
	t.Helper()
	file, err := parser.NewSourceFile("script.nha042_parent.yaml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.CompileScriptSources("script.nha042_parent", "script.nha042_parent.yaml", []parser.SourceFile{file}, nha042ScriptCatalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNHA042RealScriptNamespaceAndAdmittedWaitNodes(t *testing.T) {
	result := nha042CompileScript(t, `mode: parallel
max: 2
variables: {enabled: false, attempts: 0}
sequence:
- delay: {milliseconds: 1}
- wait_template: '{{ enabled }}'
  timeout: {seconds: 0}
  continue_on_timeout: false
- action: script.nha042_child
  data: {enabled: false}
- action: script.turn_on
  target: {entity_id: script.nha042_child}
  data: {variables: {attempts: 0}}
- stop: completed
`)
	if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		t.Fatalf("compile: %+v", result.Rejected())
	}
	script := result.Accepted()[0]
	id, err := script.ID().Value()
	if err != nil || id != "script.nha042_parent" || script.Mode() != ir.AutomationParallel {
		t.Fatalf("identity/mode %q %v", id, err)
	}
	actions := script.Actions()
	if actions[0].Kind() != ir.ActionDelay || actions[0].DelayMillis() != 1 {
		t.Fatal("delay lost")
	}
	if actions[1].Kind() != ir.ActionWaitTemplate || !actions[1].WaitProgram().Valid() {
		t.Fatal("wait template lost")
	}
	if milliseconds, present := actions[1].TimeoutMillis(); !present || milliseconds != 0 {
		t.Fatal("zero timeout became absent")
	}
	if continuation, present := actions[1].ContinueOnTimeout(); !present || continuation {
		t.Fatal("explicit false lost")
	}
	if actions[2].Call().ScriptMode() != ir.ScriptCallDirect || actions[3].Call().ScriptMode() != ir.ScriptCallDetached {
		t.Fatal("waiting modes merged")
	}
	if variables := actions[3].Call().ScriptVariables(); len(variables) != 1 || variables[0].Value().Integer() != 0 {
		t.Fatal("script argument zero lost")
	}
	if actions[1].Source().Object() != id || actions[1].Source().At().Line != 6 {
		t.Fatalf("source %+v", actions[1].Source())
	}
	actions[0] = ir.Action{}
	if script.Actions()[0].Kind() != ir.ActionDelay {
		t.Fatal("accessor mutated script")
	}
}

func TestNHA042ScriptAdmissionRefusesUnrepresentedFormsAndReferences(t *testing.T) {
	for _, action := range []string{
		"- action: script.nha042_missing",
		"- delay: '{{ 1 }}'",
		"- repeat: {while: [], sequence: []}",
		"- stop: done\n  response_variable: returned",
		"- action: script.turn_on\n  target: {entity_id: automation.nha042_child}",
		"- action: script.nha042_child\n  target: {entity_id: script.nha042_child}",
	} {
		t.Run(action, func(t *testing.T) {
			result := nha042CompileScript(t, "sequence:\n"+action+"\n")
			if len(result.Accepted()) != 0 || len(result.Rejected()) != 1 {
				t.Fatalf("unrepresented form admitted: %s %+v", action, result.Rejected())
			}
			if result.Rejected()[0].Source().At().File == "" {
				t.Fatal("refusal lost source")
			}
		})
	}
	for _, name := range []string{"", "nha042_parent", "automation.nha042_parent", "script.", "script.parent.child", "script.turn on", "script.turn_on", "script.turn_off", "script.toggle", "script.reload"} {
		if _, err := ir.ParseScriptID(name); err == nil {
			t.Fatalf("invalid script ID %q admitted", name)
		}
	}
	if _, err := (ir.ScriptID{}).Value(); err == nil {
		t.Fatal("zero script identity admitted")
	}
	if (ir.Script{}).Valid() {
		t.Fatal("zero script admitted")
	}
}

func TestNHA042WrappedScriptKeepsSourceAndLiteralScopes(t *testing.T) {
	body := "script:\n  nha042_parent:\n    sequence:\n    - repeat:\n        count: 2\n        sequence:\n        - variables: {counter: 0}\n    - stop: done\n"
	result := nha042CompileScript(t, body)
	if len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
		t.Fatalf("wrapped script: %+v", result.Rejected())
	}
	action := result.Accepted()[0].Actions()[0]
	if action.Count() != 2 || action.Kind() != ir.ActionRepeat || !strings.Contains(action.Source().Path(), "sequence") {
		t.Fatal("repeat/source changed")
	}
}

func TestNHA042EmptyScriptIsExplicitProfileRefusal(t *testing.T) {
	result := nha042CompileScript(t, "sequence: []\n")
	if len(result.Accepted()) != 0 || len(result.Rejected()) != 1 || result.Rejected()[0].Source().At().File == "" {
		t.Fatalf("empty script profile changed: %+v", result.Rejected())
	}
}
