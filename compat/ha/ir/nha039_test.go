package ir_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/compat/ha/template"
)

func nha039Entity(t *testing.T, id string, status ir.ReferenceStatus, caps ...ir.DeclaredSupport) ir.Entity {
	t.Helper()
	entity, err := ir.NewEntity(id, status, caps...)
	if err != nil {
		t.Fatal(err)
	}
	return entity
}
func nha039Device(t *testing.T, id string) ir.Device {
	t.Helper()
	device, err := ir.NewDevice(id, ir.ReferencePresent)
	if err != nil {
		t.Fatal(err)
	}
	return device
}
func nha039Event(t *testing.T, name string) ir.Event {
	t.Helper()
	event, err := ir.NewEvent(name, ir.ReferencePresent)
	if err != nil {
		t.Fatal(err)
	}
	return event
}
func nha039Catalogue(t *testing.T) ir.Catalogue {
	t.Helper()
	catalogue, err := ir.NewCatalogue([]ir.Entity{
		nha039Entity(t, "input_boolean.heating_season", ir.ReferencePresent, ir.SupportObserveState),
		nha039Entity(t, "binary_sensor.workday_sensor", ir.ReferencePresent, ir.SupportObserveState),
		nha039Entity(t, "climate.neviweb130_climate_sous_sol", ir.ReferencePresent, ir.SupportClimateSetpoint),
		nha039Entity(t, "climate.neviweb130_climate_chambre_sous_sol", ir.ReferencePresent, ir.SupportClimateSetpoint),
		nha039Entity(t, "switch.basement_relay", ir.ReferencePresent, ir.SupportSwitchTurnOn),
		nha039Entity(t, "switch.synthetic", ir.ReferencePresent, ir.SupportSwitchTurnOn),
		nha039Entity(t, "switch.excluded", ir.ReferenceExcluded),
		nha039Entity(t, "light.synthetic", ir.ReferencePresent, ir.SupportLightTurnOn, ir.SupportLightTurnOff, ir.SupportLightBrightnessAbsolute, ir.SupportLightBrightnessRelative, ir.SupportLightTransition),
		nha039Entity(t, "light.absolute_only", ir.ReferencePresent, ir.SupportLightTurnOn, ir.SupportLightBrightnessAbsolute),
		nha039Entity(t, "scene.synthetic", ir.ReferencePresent, ir.SupportSceneTurnOn, ir.SupportSceneTransition),
	}, []ir.Device{nha039Device(t, "synthetic-panel")}, []ir.Event{nha039Event(t, "synthetic.press"), nha039Event(t, "synthetic.release")})
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}
func nha039Document(t *testing.T, name, body string) *parser.Document {
	t.Helper()
	file, err := parser.NewSourceFile(name, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.LoadSources(name, []parser.SourceFile{file}, parser.SyntaxProfileLogic)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestNHA039SelectedDirectAutomations(t *testing.T) {
	catalogue := nha039Catalogue(t)
	names := []string{
		"automation.heat_the_office_on_work_day.yaml",
		"automation.turn_off_basement_climate_the_evening.yaml",
		"automation.turn_off_heating_in_guest_bedroom_the_day.yaml",
		"automation.turn_on_lights_at_sunset.yaml",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", "..", "scope", "logic_mvp_sources", name))
			if err != nil {
				t.Fatal(err)
			}
			file, err := parser.NewSourceFile(name, body)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ir.CompileSources(strings.TrimSuffix(name, ".yaml"), name, []parser.SourceFile{file}, catalogue)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
				t.Fatalf("accepted=%d rejected=%+v", len(result.Accepted()), result.Rejected())
			}
			automation := result.Accepted()[0]
			if !automation.Valid() || automation.Source().At().File != name || automation.Actions()[0].Source().Object() != strings.TrimSuffix(name, ".yaml") {
				t.Fatalf("invalid automation or source: %+v", automation)
			}
			if strings.Contains(name, "sunset") {
				if automation.Triggers()[0].Kind() != ir.TriggerSunset || automation.Triggers()[0].OffsetSeconds() != -3600 {
					t.Fatalf("sun trigger lost: %+v", automation.Triggers())
				}
			} else if automation.Triggers()[0].Kind() != ir.TriggerTime {
				t.Fatalf("time trigger lost: %+v", automation.Triggers())
			}
		})
	}
}

func TestNHA039SyntaxRefusalIsLocalToOneSelectedObject(t *testing.T) {
	invalid := "id: bad\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nunsupported_field: true\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n"
	valid := "id: good\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n"
	badFile, err := parser.NewSourceFile("automation.bad.yaml", []byte(invalid))
	if err != nil {
		t.Fatal(err)
	}
	goodFile, err := parser.NewSourceFile("automation.good.yaml", []byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	catalogue := nha039Catalogue(t)
	bad, err := ir.CompileSources("automation.bad", badFile.Path(), []parser.SourceFile{badFile}, catalogue)
	if err != nil {
		t.Fatal(err)
	}
	good, err := ir.CompileSources("automation.good", goodFile.Path(), []parser.SourceFile{goodFile}, catalogue)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad.Rejected()) != 1 || bad.Rejected()[0].SyntaxCode() != parser.CodeUnknownField.String() || bad.Rejected()[0].Source().Path() != "$.unsupported_field" {
		t.Fatalf("bad=%+v", bad.Rejected())
	}
	if len(good.Accepted()) != 1 || len(good.Rejected()) != 0 {
		t.Fatalf("good=%+v", good)
	}
}

func TestNHA039IncludeAliasProvenanceSurvivesCompilation(t *testing.T) {
	root, err := parser.NewSourceFile("configuration.yaml", []byte("automation: !include parts/automation.yaml\n"))
	if err != nil {
		t.Fatal(err)
	}
	included, err := parser.NewSourceFile("parts/automation.yaml", []byte("- id: included\n  triggers:\n  - &press\n    trigger: event\n    event_type: synthetic.press\n  - *press\n  actions:\n  - action: switch.turn_on\n    data: {}\n    target: {entity_id: switch.synthetic}\n"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.LoadSources(root.Path(), []parser.SourceFile{root, included}, parser.SyntaxProfileLogic)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.CompileDocument(doc, nha039Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		t.Fatalf("result=%+v", result)
	}
	triggers := result.Accepted()[0].Triggers()
	if len(triggers) != 2 || triggers[1].Source().At().File != "parts/automation.yaml" {
		t.Fatalf("included trigger source lost: %+v", triggers)
	}
	refs := triggers[1].Source().References()
	if len(refs) != 2 || refs[0].Kind != ir.ReferenceInclude || refs[1].Kind != ir.ReferenceAlias {
		t.Fatalf("include/alias chain lost: %+v", refs)
	}
	refs[0] = ir.Reference{}
	if triggers[1].Source().References()[0].Kind != ir.ReferenceInclude {
		t.Fatal("caller mutated reference chain")
	}
}

func TestNHA039OriginalBlueprintNestedActionsAndSourceCopies(t *testing.T) {
	paths := []string{"invocation.yaml", "blueprints/automation/homeward/logic_profile.yaml"}
	files := make([]parser.SourceFile, 0, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(filepath.Join("fixtures", path))
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.NewSourceFile(path, body)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	expanded, err := parser.ExpandBlueprintSources("invocation.yaml", files)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ir.CompileExpanded(expanded, nha039Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		t.Fatalf("accepted=%d rejected=%+v", len(result.Accepted()), result.Rejected())
	}
	automation := result.Accepted()[0]
	if automation.Mode() != ir.AutomationQueued || automation.MaxExceeded() != ir.MaxExceededSilent || len(automation.Triggers()) != 2 {
		t.Fatalf("mode/max_exceeded/triggers lost: %s %s %d", automation.Mode(), automation.MaxExceeded(), len(automation.Triggers()))
	}
	variables := automation.Variables()
	if len(variables) != 1 || variables[0].Name() != "enabled" || variables[0].Value().Kind() != ir.ValueBool || variables[0].Value().Bool() || len(variables[0].Value().Source().Inputs()) == 0 {
		t.Fatalf("precondition variable or provenance lost: %+v", variables)
	}
	if automation.Actions()[0].Kind() == ir.ActionVariables {
		t.Fatal("automation variables must precede conditions, not become a script action")
	}
	counts := map[ir.CallKind]int{}
	var zeroTimeout, explicitFalse, repeatTwo bool
	var walk func([]ir.Action)
	walk = func(actions []ir.Action) {
		for _, action := range actions {
			if action.Kind() == ir.ActionCall {
				call := action.Call()
				counts[call.Kind()]++
			}
			if action.Kind() == ir.ActionWait {
				value, present := action.TimeoutMillis()
				zeroTimeout = zeroTimeout || (present && value == 0)
				enabled, has := action.ContinueOnTimeout()
				explicitFalse = explicitFalse || (has && !enabled)
			}
			if action.Kind() == ir.ActionRepeat && action.Count() == 2 {
				repeatTwo = true
			}
			for _, branch := range action.Branches() {
				walk(branch.Actions())
			}
			walk(action.Children())
			walk(action.Otherwise())
		}
	}
	walk(automation.Actions())
	if counts[ir.CallLightTurnOn] != 3 || counts[ir.CallLightTurnOff] != 1 || counts[ir.CallSceneTurnOn] != 2 || !zeroTimeout || !explicitFalse || !repeatTwo {
		t.Fatalf("nested profile lost: calls=%v zero=%t false=%t repeat=%t", counts, zeroTimeout, explicitFalse, repeatTwo)
	}
	var absolute50, stepPlus15, stepMinus15, lightOff2, scene2, scene10 bool
	walk = func(actions []ir.Action) {
		for _, action := range actions {
			if action.Kind() == ir.ActionCall {
				call := action.Call()
				if call.Kind() == ir.CallLightTurnOn {
					if call.BrightnessPercent().Present() {
						if call.Brightness().Kind() != ir.BrightnessAbsolutePercent || call.BrightnessPercent().Value() != 50 || call.TransitionSeconds().Value() != 1 {
							t.Fatal("absolute light values lost")
						}
						absolute50 = len(call.BrightnessPercent().Source().Inputs()) > 0
					}
					if call.BrightnessStepPercent().Present() {
						if call.Brightness().Kind() != ir.BrightnessRelativePercent || call.TransitionSeconds().Value() != 1 {
							t.Fatal("relative light values lost")
						}
						sourced := len(call.BrightnessStepPercent().Source().Inputs()) > 0
						switch call.BrightnessStepPercent().Value() {
						case 15:
							stepPlus15 = sourced
						case -15:
							stepMinus15 = sourced
						default:
							t.Fatal("relative brightness value lost")
						}
					}
				}
				if call.Kind() == ir.CallLightTurnOff {
					lightOff2 = call.TransitionSeconds().Present() && call.TransitionSeconds().Value() == 2 && len(call.TransitionSeconds().Source().Inputs()) > 0
				}
				if call.Kind() == ir.CallSceneTurnOn {
					sourced := len(call.TransitionSeconds().Source().Inputs()) > 0
					switch call.TransitionSeconds().Value() {
					case 2:
						scene2 = sourced
					case 10:
						scene10 = sourced
					default:
						t.Fatal("scene transition lost")
					}
				}
			}
			for _, branch := range action.Branches() {
				walk(branch.Actions())
			}
			walk(action.Children())
			walk(action.Otherwise())
		}
	}
	walk(automation.Actions())
	if !absolute50 || !stepPlus15 || !stepMinus15 || !lightOff2 || !scene2 || !scene10 {
		t.Fatalf("action parameter/provenance lost: absolute=%t plus=%t minus=%t off2=%t scene2=%t scene10=%t", absolute50, stepPlus15, stepMinus15, lightOff2, scene2, scene10)
	}
	inputs := variables[0].Value().Source().Inputs()
	original := inputs[0].Name()
	inputs[0] = ir.InputUse{}
	variables[0] = ir.Variable{}
	if automation.Variables()[0].Value().Source().Inputs()[0].Name() != original {
		t.Fatal("caller mutated variable or input provenance")
	}
	actions := automation.Actions()
	actions[0] = ir.Action{}
	if automation.Actions()[0].Kind() == ir.ActionInvalid {
		t.Fatal("caller mutated action slice")
	}
	accepted := result.Accepted()
	accepted[0] = ir.Automation{}
	if !result.Accepted()[0].Valid() {
		t.Fatal("caller mutated result")
	}
}

func TestNHA039PerObjectRefusalAndUnknownAction(t *testing.T) {
	body := "automation:\n- id: good\n  triggers:\n  - trigger: mqtt\n    topic: synthetic/press\n  actions:\n  - action: switch.turn_on\n    data: {}\n    target: {entity_id: switch.synthetic}\n- id: unsupported\n  triggers:\n  - trigger: mqtt\n    topic: synthetic/press\n  actions:\n  - action: unsupported.nothing\n    data: {}\n    target: {entity_id: switch.synthetic}\n- id: excluded\n  triggers:\n  - trigger: mqtt\n    topic: synthetic/press\n  actions:\n  - action: switch.turn_on\n    data: {}\n    target: {entity_id: switch.excluded}\n"
	result, err := ir.CompileDocument(nha039Document(t, "bundle.yaml", body), nha039Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Accepted()) != 1 || len(result.Rejected()) != 2 {
		t.Fatalf("accepted=%d rejected=%+v", len(result.Accepted()), result.Rejected())
	}
	if result.Rejected()[0].Code() != ir.CodeUnknownService || result.Rejected()[0].Object() != "automation.unsupported" {
		t.Fatalf("first refusal=%+v", result.Rejected()[0])
	}
	if result.Rejected()[1].Code() != ir.CodeExcludedReference || result.Rejected()[1].Object() != "automation.excluded" {
		t.Fatalf("second refusal=%+v", result.Rejected()[1])
	}
	for _, diagnostic := range result.Rejected() {
		if diagnostic.Source().At().File != "bundle.yaml" || diagnostic.Source().Path() == "" {
			t.Fatalf("unsourced refusal=%+v", diagnostic)
		}
	}
}

func TestNHA039WrappedObjectsKeepDistinctIDs(t *testing.T) {
	body := "automation:\n- id: first\n  triggers:\n  - trigger: mqtt\n    topic: synthetic/first\n  actions:\n  - action: switch.turn_on\n    data: {}\n    target: {entity_id: switch.synthetic}\n- id: second\n  triggers:\n  - trigger: mqtt\n    topic: synthetic/second\n  actions:\n  - action: switch.turn_on\n    data: {}\n    target: {entity_id: switch.synthetic}\n- id: first\n  triggers:\n  - trigger: mqtt\n    topic: synthetic/duplicate\n  actions:\n  - action: switch.turn_on\n    data: {}\n    target: {entity_id: switch.synthetic}\n"
	result, err := ir.CompileDocument(nha039Document(t, "automation.bundle.yaml", body), nha039Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Accepted()) != 2 || len(result.Rejected()) != 1 {
		t.Fatalf("accepted=%d rejected=%+v", len(result.Accepted()), result.Rejected())
	}
	if result.Accepted()[0].Source().Object() != "automation.first" || result.Accepted()[1].Source().Object() != "automation.second" {
		t.Fatalf("object IDs collapsed onto filename: %+v", result.Accepted())
	}
	if result.Rejected()[0].Code() != ir.CodeInvalidValue || result.Rejected()[0].Object() != "automation.first" || result.Rejected()[0].Field() != "id" {
		t.Fatalf("duplicate ID refusal=%+v", result.Rejected()[0])
	}
}

func TestNHA039OptionalFilterPresence(t *testing.T) {
	body := "id: optional-filters\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\n- trigger: mqtt\n  topic: synthetic/press\n  payload: ''\n- trigger: event\n  event_type: synthetic.press\n- trigger: event\n  event_type: synthetic.press\n  event_data: {command: '', args: []}\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n"
	result, err := ir.CompileDocument(nha039Document(t, "automation.optional-filters.yaml", body), nha039Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		t.Fatalf("result=%+v", result)
	}
	triggers := result.Accepted()[0].Triggers()
	if len(triggers) != 4 {
		t.Fatalf("trigger count=%d", len(triggers))
	}
	if topic, err := triggers[0].Topic().Value(); err != nil || topic != "synthetic/press" {
		t.Fatalf("typed MQTT filter=%q, %v", topic, err)
	}
	if payload, present := triggers[0].Payload(); payload != "" || present {
		t.Fatalf("absent payload=%q present=%t", payload, present)
	}
	if payload, present := triggers[1].Payload(); payload != "" || !present {
		t.Fatalf("empty payload=%q present=%t", payload, present)
	}
	if command, present := triggers[2].Data().Command(); command != "" || present {
		t.Fatalf("absent event command=%q present=%t", command, present)
	}
	if args, present := triggers[2].Data().Args(); len(args) != 0 || present {
		t.Fatalf("absent event args=%v present=%t", args, present)
	}
	if command, present := triggers[3].Data().Command(); command != "" || !present {
		t.Fatalf("empty event command=%q present=%t", command, present)
	}
	if args, present := triggers[3].Data().Args(); len(args) != 0 || !present {
		t.Fatalf("empty event args=%v present=%t", args, present)
	}
}

func TestNHA039EventTemplatesAreNotLiteralCatalogueReferences(t *testing.T) {
	catalogue, err := ir.NewCatalogue(
		[]ir.Entity{nha039Entity(t, "switch.synthetic", ir.ReferencePresent, ir.SupportSwitchTurnOn)},
		[]ir.Device{nha039Device(t, "{{device}}")},
		[]ir.Event{nha039Event(t, "{{event}}"), nha039Event(t, "synthetic.press")},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, trigger, field string
	}{
		{"event-type", "event_type: '{{event}}'", "event_type"},
		{"event-device", "event_type: synthetic.press\n  event_data: {device_id: '{{device}}'}", "device_id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := "id: event-template\ntriggers:\n- trigger: event\n  " + test.trigger + "\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n"
			result, err := ir.CompileDocument(nha039Document(t, test.name+".yaml", body), catalogue)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Accepted()) != 0 || len(result.Rejected()) != 1 || result.Rejected()[0].Code() != ir.CodeUnsupportedTemplate || result.Rejected()[0].Field() != test.field {
				t.Fatalf("event template accepted as literal reference: %+v", result)
			}
		})
	}
}

func TestNHA039LiteralBracesRemainLiteral(t *testing.T) {
	body := "id: literal-braces\ntriggers:\n- trigger: device\n  domain: mqtt\n  device_id: synthetic-panel\n  type: action\n  subtype: '{{button}}'\nconditions:\n- condition: state\n  entity_id: input_boolean.heating_season\n  state: '{{literal}}'\nactions:\n- stop: 'text with {{braces}}'\n"
	result, err := ir.CompileDocument(nha039Document(t, "literal-braces.yaml", body), nha039Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		t.Fatalf("literal HA strings refused: %+v", result)
	}
	automation := result.Accepted()[0]
	if automation.Triggers()[0].Subtype() != "{{button}}" || automation.Conditions()[0].State() != "{{literal}}" || automation.Actions()[0].Stop() != "text with {{braces}}" {
		t.Fatalf("literal strings altered: %+v", automation)
	}
}

func TestNHA039TemplateAndUnknownDataRejectPrecisely(t *testing.T) {
	for _, test := range []struct {
		name, body string
		want       ir.Code
		field      string
	}{
		{"unsupported-template-test", "id: unsupported-template\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nconditions:\n- condition: template\n  value_template: '{{ wait.trigger.idx is not defined }}'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "value_template"},
		{"unknown-data", "id: data\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {unsupported: 1}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnknownField, "unsupported"},
		{"unknown-reference", "id: missing\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: climate.set_temperature\n  data: {temperature: 20}\n  target: {entity_id: climate.home}\n", ir.CodeUnknownReference, "entity_id"},
		{"wait-trigger-not-automation-trigger", "id: wait-scope\ntriggers:\n- trigger: mqtt\n  id: main\n  topic: synthetic/press\nactions:\n- wait_for_trigger:\n  - trigger: mqtt\n    id: wait_only\n    topic: synthetic/wait\n- choose:\n  - conditions:\n    - condition: trigger\n      id: wait_only\n    sequence:\n    - action: switch.turn_on\n      data: {}\n      target: {entity_id: switch.synthetic}\n", ir.CodeUnknownReference, "id"},
		{"missing-capability", "id: missing-capability\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: light.turn_on\n  data: {brightness_step_pct: 15}\n  target: {entity_id: light.absolute_only}\n", ir.CodeMissingCapability, "brightness_step_pct"},
		{"invalid-target-alias", "id: invalid-alias\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: 'switch.foo bar'}\n", ir.CodeInvalidValue, "entity_id"},
		{"invalid-mode", "id: invalid-mode\nmode: fanout\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeInvalidValue, "mode"},
		{"empty-mode", "id: empty-mode\nmode: ''\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeInvalidValue, "mode"},
		{"empty-max-exceeded", "id: empty-max\nmax_exceeded: ''\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeInvalidValue, "max_exceeded"},
		{"malformed-topic-hash", "id: bad-topic\ntriggers:\n- trigger: mqtt\n  topic: foo#\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeInvalidValue, "topic"},
		{"malformed-topic-plus", "id: bad-topic\ntriggers:\n- trigger: mqtt\n  topic: a/+b\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeInvalidValue, "topic"},
		{"signed-minute", "id: bad-offset\ntriggers:\n- trigger: sun\n  event: sunset\n  offset: '00:-1:00'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeInvalidValue, "offset"},
		{"signed-second", "id: bad-offset\ntriggers:\n- trigger: sun\n  event: sunset\n  offset: '00:00:+1'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeInvalidValue, "offset"},
		{"double-sign-hour", "id: bad-offset\ntriggers:\n- trigger: sun\n  event: sunset\n  offset: '--1:00:00'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeInvalidValue, "offset"},
		{"templated-automation-variable", "id: bad-variable\nvariables:\n  computed: '{{ 1 + 1 }}'\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "computed"},
		{"templated-action-variable", "id: bad-variable\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- variables:\n    computed: '{{ 1 + 1 }}'\n", ir.CodeUnsupportedTemplate, "computed"},
		{"templated-mqtt-payload", "id: bad-payload\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\n  payload: '{{ event }}'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "payload"},
		{"templated-mqtt-topic", "id: bad-topic\ntriggers:\n- trigger: mqtt\n  topic: '{{ topic }}'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "topic"},
		{"templated-event-command", "id: bad-command\ntriggers:\n- trigger: event\n  event_type: synthetic.press\n  event_data: {command: '{{ command }}'}\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "command"},
		{"templated-event-cluster", "id: bad-cluster\ntriggers:\n- trigger: event\n  event_type: synthetic.press\n  event_data: {cluster_id: '{{cluster}}'}\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "cluster_id"},
		{"templated-event-arg", "id: bad-arg\ntriggers:\n- trigger: event\n  event_type: synthetic.press\n  event_data: {args: ['{{arg}}']}\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "args"},
		{"templated-time", "id: bad-time\ntriggers:\n- trigger: time\n  at: '{{time}}'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "at"},
		{"templated-service", "id: bad-service\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: '{{service}}'\n  data: {}\n  target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "action"},
		{"templated-service-target", "id: bad-target\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: '{{entity}}'}\n", ir.CodeUnsupportedTemplate, "entity_id"},
		{"templated-service-data", "id: bad-data\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: climate.set_temperature\n  data: {temperature: '{{temperature}}'}\n  target: {entity_id: climate.neviweb130_climate_sous_sol}\n", ir.CodeUnsupportedTemplate, "temperature"},
		{"templated-repeat-count", "id: bad-count\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- repeat:\n    count: '{{times}}'\n    sequence:\n    - action: switch.turn_on\n      data: {}\n      target: {entity_id: switch.synthetic}\n", ir.CodeUnsupportedTemplate, "count"},
		{"templated-timeout-component", "id: bad-timeout\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- wait_for_trigger:\n  - trigger: mqtt\n    topic: synthetic/wait\n  timeout: {seconds: '{{seconds}}'}\n", ir.CodeUnsupportedTemplate, "seconds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := ir.CompileDocument(nha039Document(t, test.name+".yaml", test.body), nha039Catalogue(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Accepted()) != 0 || len(result.Rejected()) != 1 {
				t.Fatalf("result=%+v", result)
			}
			d := result.Rejected()[0]
			if d.Code() != test.want || d.Field() != test.field || d.Source().At().Line < 1 || d.Source().Path() == "" {
				t.Fatalf("refusal=%+v", d)
			}
		})
	}
}

func TestNHA039DefinedTemplateUsesNHA176Contract(t *testing.T) {
	body := "id: defined\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nconditions:\n- condition: template\n  value_template: '{{ wait.trigger.idx is defined }}'\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n"
	result, err := ir.CompileDocument(nha039Document(t, "defined.yaml", body), nha039Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rejected()) != 0 || len(result.Accepted()) != 1 {
		t.Fatalf("defined admission: accepted=%d rejected=%+v", len(result.Accepted()), result.Rejected())
	}
	conditions := result.Accepted()[0].Conditions()
	if len(conditions) != 1 || conditions[0].Kind() != ir.ConditionTemplate || conditions[0].TemplateSource() != "{{ wait.trigger.idx is defined }}" {
		t.Fatalf("defined condition lost: %+v", conditions)
	}
	idx, err := template.Object([]template.Binding{{Name: "idx", Value: template.Int(0)}})
	if err != nil {
		t.Fatal(err)
	}
	wait, err := template.Object([]template.Binding{{Name: "trigger", Value: idx}})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := template.NewInputs(nil, []template.Binding{{Name: "wait", Value: wait}}, time.Unix(0, 0).UTC(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	evaluated, err := template.Evaluate(context.Background(), conditions[0].Program(), inputs, template.DefaultLimits())
	if err != nil || evaluated.Text() != "True" {
		t.Fatalf("bound defined witness: %q %v", evaluated.Text(), err)
	}
}

func TestNHA039InvalidCatalogueAndInput(t *testing.T) {
	if _, err := ir.CompileDocument(nil, nha039Catalogue(t)); !errors.Is(err, ir.ErrInvalidInput) {
		t.Fatalf("nil document: %v", err)
	}
	if _, err := ir.NewEntity("light.synthetic", ir.ReferenceStatusInvalid); !errors.Is(err, ir.ErrInvalidCatalogue) {
		t.Fatalf("zero status: %v", err)
	}
	if _, err := ir.NewEntity("light.foo bar", ir.ReferencePresent, ir.SupportLightTurnOn); !errors.Is(err, ir.ErrInvalidCatalogue) {
		t.Fatalf("invalid alias: %v", err)
	}
	if _, err := ir.NewCatalogue([]ir.Entity{nha039Entity(t, "switch.synthetic", ir.ReferencePresent, ir.SupportSwitchTurnOn), nha039Entity(t, "switch.synthetic", ir.ReferencePresent, ir.SupportSwitchTurnOn)}, nil, nil); !errors.Is(err, ir.ErrInvalidCatalogue) {
		t.Fatalf("duplicate catalogue: %v", err)
	}
	doc := nha039Document(t, "sample.yaml", "id: sample\ntriggers:\n- trigger: mqtt\n  topic: synthetic/press\nactions:\n- action: switch.turn_on\n  data: {}\n  target: {entity_id: switch.synthetic}\n")
	if _, err := ir.CompileDocument(doc, ir.Catalogue{}); !errors.Is(err, ir.ErrInvalidInput) {
		t.Fatalf("zero catalogue: %v", err)
	}
}

func TestNHA039ProfileIsAdditive(t *testing.T) {
	body := "id: additive\ntriggers:\n- trigger: time\n  at: '09:00:00'\nconditions:\n- condition: state\n  entity_id: input_boolean.heating_season\n  state: 'on'\nactions:\n- action: climate.set_temperature\n  metadata: {}\n  data: {temperature: 20}\n  target: {entity_id: climate.neviweb130_climate_sous_sol}\n"
	file, err := parser.NewSourceFile("automation.additive.yaml", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []parser.SyntaxProfile{parser.SyntaxProfileCore, parser.SyntaxProfileDeclarative} {
		if _, err := parser.LoadSources(file.Path(), []parser.SourceFile{file}, profile); err == nil {
			t.Fatalf("historical profile %s admitted time trigger", profile.String())
		}
	}
	doc, err := parser.LoadSources(file.Path(), []parser.SourceFile{file}, parser.SyntaxProfileLogic)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Profile() != parser.LogicProfile {
		t.Fatalf("profile=%s", doc.Profile())
	}
	result, err := ir.CompileSources("automation.additive", file.Path(), []parser.SourceFile{file}, nha039Catalogue(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Accepted()) != 1 || len(result.Rejected()) != 0 {
		t.Fatalf("result=%+v", result)
	}
}
