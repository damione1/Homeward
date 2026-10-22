package configsimulation_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/configsimulation"
)

func nha054Input(t *testing.T, family configplans.Family, identity, source string) configsimulation.Input {
	t.Helper()
	file, err := parser.NewSourceFile("fixture.yaml", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := configplans.NewCandidate(family, identity, "fixture.yaml", []parser.SourceFile{file})
	if err != nil {
		t.Fatal(err)
	}
	state := "off"
	fixtures := []configsimulation.Fixture{}
	for _, alias := range []string{"switch.fixture", "light.fixture", "switch.refused"} {
		fixture, err := configsimulation.NewFixture(alias, &state, nil, []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportSwitchTurnOn, ir.SupportLightTurnOn, ir.SupportLightBrightnessAbsolute}, alias == "switch.refused")
		if err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, fixture)
	}
	event, err := ir.NewEvent("nha054_release", ir.ReferencePresent)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := configsimulation.NewSnapshotWithEvents(fixtures, []ir.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	return configsimulation.Input{Candidate: candidate, Snapshot: snapshot, Now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), Zone: time.UTC, VirtualLimit: time.Minute}
}

const nha054Script = `sequence:
- if:
  - condition: state
    entity_id: switch.fixture
    state: 'off'
  then:
  - wait_for_trigger:
    - trigger: event
      event_type: nha054_release
    timeout: {seconds: 2}
    continue_on_timeout: true
  - action: light.turn_on
    target: {entity_id: light.fixture}
    data: {brightness_pct: 25}
  else:
  - action: switch.turn_on
    target: {entity_id: switch.refused}
    data: {}
`

func TestNHA054RealInterpreterFixtureSnapshotWaitAndTypedIntent(t *testing.T) {
	input := nha054Input(t, configplans.Script, "script.fixture", nha054Script)
	var first configsimulation.Report
	for repetition := 0; repetition < 2; repetition++ {
		report, err := configsimulation.Run(context.Background(), input)
		if err != nil || report.Status() != configsimulation.Completed {
			t.Fatalf("real interpreter: %v status=%v diagnostics=%+v", err, report.Status(), report.Diagnostics())
		}
		intents := report.Intents()
		if len(intents) != 1 || intents[0].Call().Kind() != ir.CallLightTurnOn || intents[0].Call().BrightnessPercent().Value() != 25 || intents[0].Outcome() != configsimulation.Recorded || intents[0].Position().File != "fixture.yaml" || intents[0].Position().Line != 12 || intents[0].Position().Path == "" {
			t.Fatalf("typed source/intention lost: %+v", intents)
		}
		if report.Elapsed() != 2*time.Second || !intents[0].At().Equal(input.Now.Add(2*time.Second)) {
			t.Fatal("wall clock escaped fixture")
		}
		waited, branch := false, false
		for _, node := range report.Nodes() {
			if node.Kind() == ir.ActionWait && node.WaitExit() == automationruns.WaitContinuedAfterTimeout {
				waited = true
			}
			if strings.Contains(node.Source().Path(), ".then") {
				branch = true
			}
			if node.At().Before(input.Now) || node.At().After(input.Now.Add(2*time.Second)) {
				t.Fatal("node used real time")
			}
		}
		if !waited || !branch {
			t.Fatal("actual wait/branch traces absent")
		}
		if !report.Simulated() || report.HardwareValidated() || len(report.Limitations()) < 3 {
			t.Fatal("coverage/hardware claim incorrect")
		}
		if repetition == 0 {
			first = report
		} else if !reflect.DeepEqual(first, report) {
			t.Fatal("same fixture changed domain report")
		}
	}
}

type nha054NilContext struct{ context.Context }

func TestNHA054ZeroAndTypedNilInputsAreRejected(t *testing.T) {
	input := nha054Input(t, configplans.Script, "script.fixture", nha054Script)
	var ctx *nha054NilContext
	if _, err := configsimulation.Run(ctx, input); !errors.Is(err, configsimulation.ErrInvalid) {
		t.Fatal("typed nil context admitted", err)
	}
	if _, err := configsimulation.NewSnapshot([]configsimulation.Fixture{{}}); !errors.Is(err, configsimulation.ErrInvalid) {
		t.Fatal("zero fixture admitted", err)
	}
	if _, err := configsimulation.NewFixture("switch.fixture", nil, nil, []ir.DeclaredSupport{255}, false); !errors.Is(err, configsimulation.ErrInvalid) {
		t.Fatal("future support admitted", err)
	}
}

func TestNHA054ScenePartialUsesExistingCompilerMemberIntent(t *testing.T) {
	input := nha054Input(t, configplans.Scene, "scene.fixture", "id: fixture\nname: Fixture\nentities:\n  light.fixture:\n    state: 'on'\n    brightness: 64\n    color_mode: brightness\n  switch.refused:\n    state: 'off'\n")
	report, err := configsimulation.Run(context.Background(), input)
	if err != nil || report.Status() != configsimulation.Partial || len(report.Intents()) != 2 || len(report.Diagnostics()) != 1 {
		t.Fatalf("partial scene: %v %+v", err, report)
	}
	intents := report.Intents()
	light, present := intents[0].SceneMember().Light()
	on, brightness, _ := light.Components()
	if !present || !on || brightness == nil || *brightness != 64 || intents[0].Outcome() != configsimulation.Recorded || intents[1].Outcome() != configsimulation.Refused || intents[0].Position().Line != 5 {
		t.Fatal("scene compiler result/positions not preserved")
	}
}

func TestNHA054UnsupportedNetworkExtensionFailsBeforeAnyIntent(t *testing.T) {
	var networkCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { networkCalls.Add(1) }))
	defer server.Close()
	input := nha054Input(t, configplans.Script, "script.fixture", "sequence:\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n- action: rest_command.nha054_network\n  data: {url: '"+server.URL+"/DO_NOT_CONNECT'}\n")
	report, err := configsimulation.Run(context.Background(), input)
	if err != nil || report.Status() != configsimulation.Failed || len(report.Intents()) != 0 || len(report.Diagnostics()) != 1 || report.Diagnostics()[0].Code != configsimulation.UnsupportedExtension || report.Diagnostics()[0].Position.File != "fixture.yaml" || networkCalls.Load() != 0 {
		t.Fatalf("extension reached an effect: %v %+v network=%d", err, report, networkCalls.Load())
	}
}

func TestNHA054RealTimeTriggerUsesFixtureClockAndConditions(t *testing.T) {
	input := nha054Input(t, configplans.Automation, "automation.fixture", "id: fixture\ntriggers:\n- trigger: time\n  at: '00:00:02'\nconditions:\n- condition: state\n  entity_id: switch.fixture\n  state: 'off'\nactions:\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n")
	report, err := configsimulation.Run(context.Background(), input)
	if err != nil || report.Status() != configsimulation.Completed || len(report.Intents()) != 1 || report.Elapsed() != 2*time.Second || !report.Intents()[0].At().Equal(input.Now.Add(2*time.Second)) {
		t.Fatalf("real trigger/controller: %v %+v", err, report)
	}
}

func TestNHA054TemplateClockChangesOnlyWithInjectedFixture(t *testing.T) {
	// The admitted profile supports date subtraction and operand-returning
	// `or`, while comparisons are outside its grammar. On the fixture date
	// zero-or-true renders True; one day later it renders 1. The real runtime
	// admits only rendered True as a matching template condition.
	input := nha054Input(t, configplans.Script, "script.fixture", `sequence:
- if:
  - condition: template
    value_template: '{{ (now().date() - as_datetime("2026-10-22").date()).days or true }}'
  then:
  - action: switch.turn_on
    target: {entity_id: switch.fixture}
    data: {}
`)
	first, err := configsimulation.Run(context.Background(), input)
	if err != nil || first.Status() != configsimulation.Completed || len(first.Intents()) != 1 {
		t.Fatal("template did not read injected time", err, first.Diagnostics())
	}
	input.Now = input.Now.Add(24 * time.Hour)
	second, err := configsimulation.Run(context.Background(), input)
	if err != nil || second.Status() != configsimulation.Completed || len(second.Intents()) != 0 {
		t.Fatal("template reused real/frozen time", err, second.Diagnostics())
	}
}

func TestNHA054VirtualBudgetCancellationAndUnsupportedCoverage(t *testing.T) {
	input := nha054Input(t, configplans.Script, "script.fixture", "sequence:\n- delay: {seconds: 2}\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n")
	input.VirtualLimit = time.Second
	report, err := configsimulation.Run(context.Background(), input)
	if err != nil || report.Status() != configsimulation.Failed || len(report.Intents()) != 0 {
		t.Fatalf("virtual budget escaped: %v %+v", err, report)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := configsimulation.Run(ctx, input); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	for _, source := range []string{"sequence:\n- parallel:\n  - stop: done\n", "sequence:\n- wait_template: '{{ true }}'\n"} {
		input := nha054Input(t, configplans.Script, "script.fixture", source)
		report, err := configsimulation.Run(context.Background(), input)
		if err != nil || report.Status() != configsimulation.Failed || len(report.Intents()) != 0 || len(report.Diagnostics()) == 0 {
			t.Fatal("unsupported coverage became success", err, report)
		}
	}
}

func TestNHA054VirtualBudgetReportRemainsCompleteAndDeterministicThroughRetirement(t *testing.T) {
	input := nha054Input(t, configplans.Script, "script.fixture", "sequence:\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n- delay: {seconds: 2}\n- action: switch.turn_on\n  target: {entity_id: switch.fixture}\n  data: {}\n")
	input.VirtualLimit = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var baseline configsimulation.Report
	for repetition := 0; repetition < 64; repetition++ {
		report, err := configsimulation.Run(ctx, input)
		if err != nil || report.Status() != configsimulation.Failed {
			t.Fatalf("budget result %d: %v status=%v", repetition, err, report.Status())
		}
		diagnostics := report.Diagnostics()
		if len(diagnostics) != 1 || diagnostics[0].Code != configsimulation.CapacityExceeded {
			t.Fatalf("budget cancellation gained a scheduling-dependent diagnostic: %v", diagnostics)
		}
		intents := report.Intents()
		if len(intents) != 1 || intents[0].Outcome() != configsimulation.Recorded || intents[0].Call().Kind() != ir.CallSwitchTurnOn || intents[0].Position().Line != 2 || !intents[0].At().Equal(input.Now) {
			t.Fatal("pre-budget intention lost or action after exhausted delay recorded", intents)
		}
		nodes := report.Nodes()
		if len(nodes) != 4 || nodes[0].Kind() != ir.ActionCall || nodes[0].Stage() != automationruns.NodeEntered || nodes[1].Stage() != automationruns.NodeCompleted || nodes[2].Kind() != ir.ActionDelay || nodes[2].Stage() != automationruns.NodeEntered || nodes[3].Kind() != ir.ActionDelay || nodes[3].Stage() != automationruns.NodeStopped {
			t.Fatalf("joined trace prefix lost through image retirement: %v", nodes)
		}
		for _, node := range nodes {
			if node.Source().At().File != "fixture.yaml" || node.Source().At().Line == 6 || !node.At().Equal(input.Now) {
				t.Fatal("trace escaped fixture or entered action after budget")
			}
		}
		if repetition == 0 {
			baseline = report
		} else if !reflect.DeepEqual(baseline, report) {
			t.Fatalf("entire budget report changed at repetition %d", repetition)
		}
	}
}

func TestNHA054CandidateCopiesAndUnobservedSnapshotFailClosed(t *testing.T) {
	input := nha054Input(t, configplans.Script, "script.fixture", nha054Script)
	files := input.Candidate.Files()
	bytes := files[0].Bytes()
	bytes[0] = '!'
	if reflect.DeepEqual(bytes, input.Candidate.Files()[0].Bytes()) || (configplans.Candidate{}).Files() != nil || (configplans.Candidate{}).Entry() != "" {
		t.Fatal("source escaped immutable admission")
	}
	fixture, err := configsimulation.NewFixture("switch.fixture", nil, nil, []ir.DeclaredSupport{ir.SupportObserveState, ir.SupportSwitchTurnOn}, false)
	if err != nil {
		t.Fatal(err)
	}
	input.Snapshot, err = configsimulation.NewSnapshot([]configsimulation.Fixture{fixture})
	if err != nil {
		t.Fatal(err)
	}
	input = nha054Input(t, configplans.Script, "script.fixture", "sequence:\n- if:\n  - condition: state\n    entity_id: switch.fixture\n    state: 'off'\n  then:\n  - stop: done\n")
	input.Snapshot, _ = configsimulation.NewSnapshot([]configsimulation.Fixture{fixture})
	report, err := configsimulation.Run(context.Background(), input)
	if err != nil || report.Status() != configsimulation.Failed || len(report.Intents()) != 0 {
		t.Fatal("unobserved state became known false/true", err, report)
	}
}
