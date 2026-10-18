package template

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type definedCase struct {
	ID        string                     `json:"id"`
	Template  string                     `json:"template"`
	Variables map[string]json.RawMessage `json:"variables"`
}

type definedOutcome struct {
	Status string          `json:"status"`
	Type   string          `json:"type"`
	Value  json.RawMessage `json:"value"`
	Cause  *struct {
		Error string `json:"error"`
	} `json:"cause"`
}

func TestNHA176PinnedHAObservations(t *testing.T) {
	casesBytes, err := os.ReadFile("cases_defined.json")
	if err != nil {
		t.Fatal(err)
	}
	workerBytes, err := os.ReadFile("ha_defined_worker.py")
	if err != nil {
		t.Fatal(err)
	}
	observedBytes, err := os.ReadFile("observed_defined.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Cases []definedCase `json:"cases"`
	}
	var observed struct {
		Reference struct {
			FixturesSHA256 string `json:"fixtures_sha256"`
			WorkerSHA256   string `json:"worker_sha256"`
			ImageIndex     string `json:"image_index"`
			ImageManifest  string `json:"image_manifest"`
			Runtime        struct {
				HomeAssistant string `json:"homeassistant"`
				Jinja2        string `json:"jinja2"`
			} `json:"runtime"`
		} `json:"reference"`
		Cases []struct {
			ID       string         `json:"id"`
			HAText   definedOutcome `json:"ha_text"`
			HANative definedOutcome `json:"ha_native"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(casesBytes, &fixtures); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(observedBytes, &observed); err != nil {
		t.Fatal(err)
	}
	if len(fixtures.Cases) != 31 || len(observed.Cases) != len(fixtures.Cases) ||
		fmt.Sprintf("%x", sha256.Sum256(casesBytes)) != observed.Reference.FixturesSHA256 ||
		fmt.Sprintf("%x", sha256.Sum256(workerBytes)) != observed.Reference.WorkerSHA256 ||
		observed.Reference.ImageIndex != "sha256:130241f28d01fa80dfa3b44f7d89781c188d8642ecab9024f079f19f9f82baa0" ||
		observed.Reference.ImageManifest != "sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7" ||
		observed.Reference.Runtime.HomeAssistant != "2026.10.1" ||
		observed.Reference.Runtime.Jinja2 != "3.1.6" {
		t.Fatal("pinned HA observation drift")
	}

	for i, fixture := range fixtures.Cases {
		if observed.Cases[i].ID != fixture.ID {
			t.Fatalf("case order drift at %d", i)
		}
		if strings.HasPrefix(fixture.ID, "outside-") {
			continue // Ordinary access retains the narrower NHA-043 profile.
		}
		t.Run(fixture.ID, func(t *testing.T) {
			variables := make([]Binding, 0, len(fixture.Variables))
			for name, raw := range fixture.Variables {
				value, err := DecodeJSON(raw)
				if err != nil {
					t.Fatal(err)
				}
				variables = append(variables, Binding{Name: name, Value: value})
			}
			inputs, err := NewInputs(nil, variables, time.Unix(0, 0).UTC(), time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			program, err := Compile(fixture.Template)
			if err != nil {
				t.Fatal(err)
			}
			result, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
			want := observed.Cases[i]
			if want.HAText.Status != want.HANative.Status {
				t.Fatal("HA text/native status mismatch")
			}
			if want.HAText.Status == "error" {
				if want.HAText.Cause == nil || want.HAText.Cause.Error != "jinja2.exceptions.UndefinedError" ||
					!errors.Is(err, ErrEvaluation) {
					t.Fatalf("Go error %v, HA %+v", err, want.HAText)
				}
				if result.Text() != "" {
					t.Fatalf("partial result after error: %q", result.Text())
				}
				if _, valid := result.Native().Kind(); valid == nil {
					t.Fatal("partial native result after error")
				}
				return
			}
			if err != nil || want.HAText.Status != "ok" || want.HAText.Type != "str" ||
				want.HANative.Type != "bool" {
				t.Fatalf("Go %v, HA text/native %+v/%+v", err, want.HAText, want.HANative)
			}
			var wantText string
			var wantBool bool
			if err := json.Unmarshal(want.HAText.Value, &wantText); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(want.HANative.Value, &wantBool); err != nil {
				t.Fatal(err)
			}
			gotBool, ok := result.Native().Bool()
			if result.Text() != wantText || !ok || gotBool != wantBool {
				t.Fatalf("Go %q/%v/%v, HA %q/%v", result.Text(), gotBool, ok, wantText, wantBool)
			}
		})
	}
}

func TestNHA176RefusesOtherTestsAndOperators(t *testing.T) {
	bad := []string{
		"{{ wait.trigger.idx is not defined }}",
		"{{ wait.trigger.idx is none }}",
		"{{ wait.trigger.idx is defined(0) }}",
		"{{ wait.trigger.idx > defined }}",
		"{{ wait.trigger.idx not defined }}",
		"{{ wait.trigger.idx in defined }}",
		"{{ wait.trigger.idx is defined.__class__ }}",
		"{{ wait.__class__ is defined }}",
		"{{ wait.trigger.idx() is defined }}",
		"{{ now() is defined }}",
	}
	for _, source := range bad {
		program, err := Compile(source)
		if err == nil || len(program.segments) != 0 ||
			!errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q admitted: %+v, %v", source, program, err)
		}
	}
}

func TestNHA176BudgetsCopiesAndOrdinaryAccess(t *testing.T) {
	inner, err := Object([]Binding{{Name: "idx", Value: Int(0)}})
	if err != nil {
		t.Fatal(err)
	}
	wait, err := Object([]Binding{{Name: "trigger", Value: inner}})
	if err != nil {
		t.Fatal(err)
	}
	variables := []Binding{{Name: "wait", Value: wait}}
	inputs, err := NewInputs(nil, variables, time.Unix(0, 0).UTC(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	delete(variables[0].Value.obj, "trigger")
	program, err := Compile("{{ wait['trigger']['idx'] is defined }}")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
	if err != nil || result.Text() != "True" {
		t.Fatalf("copied item path: %q %v", result.Text(), err)
	}
	limited, _ := NewLimits(1, defaultOutput)
	if _, err := Evaluate(context.Background(), program, inputs, limited); !errors.Is(err, ErrBudget) {
		t.Fatalf("step budget: %v", err)
	}
	shortOutput, _ := NewLimits(defaultSteps, 3)
	if _, err := Evaluate(context.Background(), program, inputs, shortOutput); !errors.Is(err, ErrBudget) {
		t.Fatalf("output budget: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Evaluate(ctx, program, inputs, DefaultLimits()); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := Compile("{{ wait" + strings.Repeat(".trigger", maxParseDrivers+1) + " is defined }}"); !errors.Is(err, ErrBudget) {
		t.Fatalf("source driver budget: %v", err)
	}
	ordinary, err := Compile("{{ wait.trigger.idx }}")
	if err != nil {
		t.Fatal(err)
	}
	waitNone, err := Object([]Binding{{Name: "trigger", Value: None()}})
	if err != nil {
		t.Fatal(err)
	}
	noneInputs, err := NewInputs(nil, []Binding{{Name: "wait", Value: waitNone}}, time.Unix(0, 0).UTC(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(context.Background(), ordinary, noneInputs, DefaultLimits()); !errors.Is(err, ErrEvaluation) {
		t.Fatalf("ordinary access changed: %v", err)
	}
	if result, err := Evaluate(context.Background(), program, inputs, DefaultLimits()); err != nil || result.Text() != "True" {
		t.Fatalf("evaluation after failure: %q %v", result.Text(), err)
	}
}

func TestNHA176QualifiedAttributeProfile(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		value        Value
		wantText     string
		wantErr      error
	}{
		{name: "duration days", source: "{{ subject.days is defined }}", value: Value{kind: valueDuration, i: 2}, wantText: "True"},
		{name: "duration unknown", source: "{{ subject.hours is defined }}", value: Value{kind: valueDuration, i: 2}, wantErr: ErrUnsupported},
		{name: "mapping method", source: "{{ subject.items is defined }}", value: Value{kind: valueObject, obj: map[string]Value{}}, wantErr: ErrUnsupported},
		{name: "mapping method item syntax", source: "{{ subject['items'] is defined }}", value: Value{kind: valueObject, obj: map[string]Value{}}, wantErr: ErrUnsupported},
		{name: "mapping dot shadows key", source: "{{ subject.items is defined }}", value: Value{kind: valueObject, obj: map[string]Value{"items": Int(0)}}, wantErr: ErrUnsupported},
		{name: "mapping item key", source: "{{ subject['items'] is defined }}", value: Value{kind: valueObject, obj: map[string]Value{"items": Int(0)}}, wantText: "True"},
		{name: "mapping dot chain", source: "{{ subject.items.idx is defined }}", value: Value{kind: valueObject, obj: map[string]Value{"items": Value{kind: valueObject, obj: map[string]Value{"idx": Int(1)}}}}, wantErr: ErrUnsupported},
		{name: "mapping item chain", source: "{{ subject['items'].idx is defined }}", value: Value{kind: valueObject, obj: map[string]Value{"items": Value{kind: valueObject, obj: map[string]Value{"idx": Int(1)}}}}, wantText: "True"},
		{name: "scalar property", source: "{{ subject.real is defined }}", value: Int(0), wantErr: ErrUnsupported},
		{name: "scalar absent", source: "{{ subject.missing is defined }}", value: Int(0), wantErr: ErrUnsupported},
		{name: "string method", source: "{{ subject.upper is defined }}", value: Value{kind: valueString, s: "text"}, wantErr: ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program, err := Compile(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			// Duration is an evaluator-produced private kind; inject it into the
			// private Inputs here to verify its already admitted .days property.
			inputs := Inputs{vars: map[string]Value{"subject": tc.value}, now: time.Unix(0, 0).UTC(), zone: time.UTC}
			result, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || result.Text() != "" {
					t.Fatalf("got %q, %v; want %v", result.Text(), err, tc.wantErr)
				}
				return
			}
			gotBool, ok := result.Native().Bool()
			if err != nil || result.Text() != tc.wantText || !ok || !gotBool {
				t.Fatalf("got %q, %v, %v, %v", result.Text(), gotBool, ok, err)
			}
		})
	}
}
