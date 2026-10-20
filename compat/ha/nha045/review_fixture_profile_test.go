package nha045

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/domain"
)

const fixture10FloatCondition = "{{ (states('sensor.nha045_missing') | float(0)) or true }}"

func fixture10ProfileCopy(t *testing.T, edit func(*fixtureWire)) Fixture {
	t.Helper()
	all, err := Corpus()
	if err != nil || len(all) != 10 || all[9].ID() != "10-template-parallel-error-join" {
		t.Fatalf("fixture10 corpus absent: %v", err)
	}
	encoded, err := all[9].JSON()
	if err != nil {
		t.Fatal(err)
	}
	var wire fixtureWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&wire)
	}
	encoded, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := DecodeFixture(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func replaceFixture10Float(t *testing.T, wire *fixtureWire, expression string) {
	t.Helper()
	if len(wire.Objects) != 1 || strings.Count(wire.Objects[0].YAML, fixture10FloatCondition) != 1 {
		t.Fatal("fixture10 float condition is not unique")
	}
	wire.Objects[0].YAML = strings.Replace(wire.Objects[0].YAML, fixture10FloatCondition, expression, 1)
}

func fixture10ProfileRig(t *testing.T, fixture Fixture) (context.Context, *rig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	r, err := buildRig(ctx, fixture)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
	})
	if err := r.flush(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, r
}

func fixture10ProfileSource(t *testing.T, ctx context.Context, r *rig, state string, availability domain.Availability) {
	t.Helper()
	found := false
	for _, binding := range r.entities {
		if binding.alias != "sensor.nha045_missing" {
			continue
		}
		found = true
		observed, present := r.fixture.State(binding.entity)
		if !present {
			t.Fatal("real Host source absent")
		}
		_, quality, _, _, _, err := observed.Components()
		if err != nil {
			t.Fatal(err)
		}
		actual, freshness, provenance, err := quality.Components()
		if err != nil || actual != availability || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceObserved {
			t.Fatalf("Host quality was relabelled: %v %v %v %v", actual, freshness, provenance, err)
		}
		value, hasValue := observed.CurrentValue()
		if hasValue != (availability == domain.AvailabilityAvailable) {
			t.Fatal("Host absence became a present zero value")
		}
		if hasValue {
			on, switchValue := value.Switch()
			if !switchValue || on || state != "off" {
				t.Fatal("known source is not the actual nonnumeric off value")
			}
		}
	}
	if !found {
		t.Fatal("fixture10 source binding absent")
	}
	program, err := template.Compile("{{ states('sensor.nha045_missing') }}")
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := r.reactive.EvaluateCondition(ctx, program, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, present := tracked.Result()
	if !present || result.Text() != state {
		t.Fatalf("actual helper state got %q want %q", result.Text(), state)
	}
	reads := tracked.Reads()
	if len(reads) != 1 {
		t.Fatal("states helper lost its real tracked source")
	}
	fixture10ProfileRead(t, reads[0], availability)
}

func fixture10ProfileRead(t *testing.T, read template.Read, availability domain.Availability) {
	t.Helper()
	quality, present := read.Quality()
	if !present || read.Present() != (availability == domain.AvailabilityAvailable) {
		t.Fatal("tracked read invented availability")
	}
	actual, freshness, provenance, err := quality.Components()
	if err != nil || actual != availability || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceObserved {
		t.Fatal("tracked source quality changed")
	}
	alias, present := read.Dependency().Alias()
	name, err := alias.Value()
	if !present || err != nil || name != "sensor.nha045_missing" {
		t.Fatal("tracked read belongs to another source")
	}
}

func fixture10ProfileConditions(t *testing.T, ctx context.Context, r *rig, dateText, floatText string, floatReads bool, availability domain.Availability) []ir.Condition {
	t.Helper()
	actions := r.scripts[r.f.wire.Root].Actions()
	if len(actions) != 1 || actions[0].Kind() != ir.ActionIf {
		t.Fatal("fixture10 lost its actual if")
	}
	conditions := actions[0].Conditions()
	if len(conditions) != 2 {
		t.Fatal("fixture10 requires the two real compiled conditions")
	}
	for index, condition := range conditions {
		if condition.Kind() != ir.ConditionTemplate || !condition.Program().Valid() {
			t.Fatal("condition bypassed real template admission")
		}
		tracked, err := r.reactive.EvaluateCondition(ctx, condition.Program(), nil)
		if err != nil {
			t.Fatal(err)
		}
		result, present := tracked.Result()
		want := dateText
		if index == 1 {
			want = floatText
		}
		if !present || result.Text() != want {
			t.Fatalf("condition %d got %q want %q", index, result.Text(), want)
		}
		if want == "True" {
			boolean, admitted := result.Native().Bool()
			if !admitted || !boolean {
				t.Fatal("true condition lost its native boolean")
			}
		}
		reads := tracked.Reads()
		if index == 1 && floatReads {
			if len(reads) != 1 {
				t.Fatal("float fallback stopped reading the actual source")
			}
			fixture10ProfileRead(t, reads[0], availability)
		} else if len(reads) != 0 {
			t.Fatal("literal/date condition acquired an unrelated source read")
		}
	}
	return conditions
}

func fixture10ProfileJoin(t *testing.T, r *rig, decision automationruns.Decision, parallel bool) nativeObservation {
	t.Helper()
	handle := r.handles["a"]
	if handle == nil || !channelJoined(handle.Done()) {
		t.Fatal("semantic handle must join before cleanup")
	}
	before, err := r.observeNative()
	if err != nil {
		t.Fatal(err)
	}
	requireNativeOrderSnapshot(t, before, false)
	terminals, parallels := 0, 0
	for _, trace := range before.decisions {
		if trace.RunID != handle.ID() {
			continue
		}
		if trace.Decision == automationruns.DecisionFinished || trace.Decision == automationruns.DecisionCancelled || trace.Decision == automationruns.DecisionExecutionError {
			if trace.Decision != decision {
				t.Fatalf("unexpected native terminal %v", trace.Decision)
			}
			terminals++
		}
	}
	for _, node := range before.nodes {
		if node.Kind() == ir.ActionParallel {
			parallels++
		}
		if !parallel && (len(node.Branches()) != 0 || node.Stage() == automationruns.NodeWaiting) {
			t.Fatal("false/unavailable condition started a parallel branch")
		}
	}
	if terminals != 1 || (parallels != 0) != parallel {
		t.Fatal("native terminal or real parallel observation absent")
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	if !channelJoined(r.controller.Done()) || !channelJoined(r.triggers.Done()) || !channelJoined(r.reactive.Done()) || !channelJoined(r.session.Done()) {
		t.Fatal("profile fixture producer join incomplete")
	}
	after, err := r.observeNative()
	if err != nil {
		t.Fatal(err)
	}
	requireNativeOrderSnapshot(t, after, true)
	if !reflect.DeepEqual(before.decisions, after.decisions) || !reflect.DeepEqual(before.nodes, after.nodes) {
		t.Fatal("cleanup manufactured native profile observations")
	}
	for _, timer := range r.clock.records() {
		if !timer.Fired && !timer.Stopped {
			t.Fatal("profile fixture retained a live timer")
		}
	}
	return after
}

// The original 04:30Z origin falls on March11 in Toronto after DST, and on
// March10 under fixed EST. The shifted zero-date predicate keeps that boundary
// sensitivity; it does not replace 043's separate multi-day subtraction oracle.
func TestNHA045Fixture10ProfileNominalAndZero(t *testing.T) {
	for _, test := range []struct {
		name, expression string
		sourceRead       bool
	}{
		{"known_off_fallback_zero", fixture10FloatCondition, true},
		{"numeric_zero", "{{ ('0' | float(0)) or true }}", false},
		{"numeric_negative_zero", "{{ ('-0' | float(0)) or true }}", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := fixture10ProfileCopy(t, func(wire *fixtureWire) { replaceFixture10Float(t, wire, test.expression) })
			ctx, r := fixture10ProfileRig(t, fixture)
			fixture10ProfileSource(t, ctx, r, "off", domain.AvailabilityAvailable)
			fixture10ProfileConditions(t, ctx, r, "True", "True", test.sourceRead, domain.AvailabilityAvailable)
			for _, phase := range fixture.phases {
				if err := r.step(ctx, phase); err != nil {
					t.Fatalf("phase %s: %v", phase.id.value, err)
				}
			}
			handle := r.handles["a"]
			if handle == nil {
				t.Fatal("nominal script was not actually admitted")
			}
			err := handle.Result(ctx)
			var execution *automationruns.ExecutionError
			if !errors.As(err, &execution) || execution.Kind() != automationruns.ExecutionCommandError {
				t.Fatalf("parallel lost its exact command failure: %v", err)
			}
			observation := fixture10ProfileJoin(t, r, automationruns.DecisionExecutionError, true)
			// The original checks/forbidden list still decide success. No graph is
			// changed here, and no HA execution is claimed by this memory oracle.
			projectedNativeOrder(t, r, observation)
		})
	}
}

func TestNHA045Fixture10ProfileDateAndNonzeroRejectThen(t *testing.T) {
	for _, test := range []struct {
		name                string
		edit                func(*fixtureWire)
		dateText, floatText string
		sourceRead          bool
	}{
		{"local_date_10", func(wire *fixtureWire) { wire.Origin = "2024-03-10T05:30:00Z" }, "-1", "True", true},
		{"local_date_12", func(wire *fixtureWire) { wire.Origin = "2024-03-12T04:30:00Z" }, "1", "True", true},
		{"known_off_default_one", func(wire *fixtureWire) {
			replaceFixture10Float(t, wire, "{{ (states('sensor.nha045_missing') | float(1)) or true }}")
		}, "True", "1", true},
		{"numeric_nonzero", func(wire *fixtureWire) { replaceFixture10Float(t, wire, "{{ ('3' | float(0)) or true }}") }, "True", "3.0", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := fixture10ProfileCopy(t, test.edit)
			ctx, r := fixture10ProfileRig(t, fixture)
			fixture10ProfileSource(t, ctx, r, "off", domain.AvailabilityAvailable)
			fixture10ProfileConditions(t, ctx, r, test.dateText, test.floatText, test.sourceRead, domain.AvailabilityAvailable)
			// A false condition must terminate in else. Waiting for the normal
			// corpus's AwaitWaiting phase would hide the actual wrong verdict.
			if err := r.step(ctx, fixture.phases[0]); err != nil {
				t.Fatal(err)
			}
			handle := r.handles["a"]
			if handle == nil {
				t.Fatal("control script was not actually admitted")
			}
			if err := handle.Result(ctx); err != nil {
				t.Fatalf("else control failed: %v", err)
			}
			fixture10ProfileJoin(t, r, automationruns.DecisionFinished, false)
			r.mu.Lock()
			actions := append([]observedAction(nil), r.actions...)
			r.mu.Unlock()
			if len(actions) != 1 || actions[0].Entity != "switch.nha045_wrong" || actions[0].Failed || actions[0].AtMS != 0 {
				t.Fatalf("false predicate did not execute only else: %+v", actions)
			}
		})
	}
}

// This is an explicit native profile refusal, not a fabricated HA-equivalent
// unknown-source fallback. A numeric fallback cannot turn Unknown into Available.
func TestNHA045Fixture10ProfileUnknownSourceIsUnavailable(t *testing.T) {
	fixture := fixture10ProfileCopy(t, func(wire *fixtureWire) {
		found := false
		for index := range wire.Entities {
			if wire.Entities[index].ID == "sensor.nha045_missing" {
				wire.Entities[index].State, found = "unknown", true
			}
		}
		if !found {
			t.Fatal("unknown-source counterexample lost its binding")
		}
	})
	ctx, r := fixture10ProfileRig(t, fixture)
	fixture10ProfileSource(t, ctx, r, "unknown", domain.AvailabilityUnknown)
	// The scalar expression really renders True; its tracked quality remains
	// Unknown/absent and the actual Interpreter must still refuse that source.
	conditions := fixture10ProfileConditions(t, ctx, r, "True", "True", true, domain.AvailabilityUnknown)
	if err := r.step(ctx, fixture.phases[0]); err != nil {
		t.Fatal(err)
	}
	handle := r.handles["a"]
	if handle == nil {
		t.Fatal("unknown-source run was not actually admitted")
	}
	err := handle.Result(ctx)
	var execution *automationruns.ExecutionError
	if !errors.As(err, &execution) || execution.Kind() != automationruns.ExecutionSourceUnavailable || execution.Source().Object() != conditions[1].Source().Object() || execution.Source().Path() != conditions[1].Source().Path() || execution.Source().At() != conditions[1].Source().At() {
		t.Fatalf("unknown source lost its native refusal and exact condition: %v", err)
	}
	fixture10ProfileSource(t, ctx, r, "unknown", domain.AvailabilityUnknown)
	observation := fixture10ProfileJoin(t, r, automationruns.DecisionExecutionError, false)
	failures := 0
	for _, node := range observation.nodes {
		if node.Failure() == automationruns.ExecutionSourceUnavailable {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("unknown quality did not reach the native node failure")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.actions) != 0 {
		t.Fatalf("unknown condition executed a synthetic action: %+v", r.actions)
	}
}
