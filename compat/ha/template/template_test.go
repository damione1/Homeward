package template

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/damione1/Homeward/internal/domain"
)

type fixtureSet struct {
	ClockUTC string `json:"clock_utc"`
	Timezone string `json:"timezone"`
	States   []struct {
		EntityID   string                     `json:"entity_id"`
		State      string                     `json:"state"`
		Attributes map[string]json.RawMessage `json:"attributes"`
	} `json:"states"`
	Cases []struct {
		ID       string `json:"id"`
		Template string `json:"template"`
	} `json:"cases"`
}

type expectedSet struct {
	HA []struct {
		ID  string `json:"id"`
		Raw struct {
			Text   string          `json:"text"`
			Native json.RawMessage `json:"native"`
		} `json:"raw"`
	} `json:"ha"`
}

func fixtureInputs(t *testing.T) (Inputs, fixtureSet) {
	t.Helper()
	data, err := os.ReadFile("../nha006/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures fixtureSet
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	states := make([]State, 0, len(fixtures.States))
	for _, raw := range fixtures.States {
		alias, err := domain.ParseHAAlias(raw.EntityID)
		if err != nil {
			t.Fatal(err)
		}
		attrs := make([]Binding, 0, len(raw.Attributes))
		for key, value := range raw.Attributes {
			parsed, err := DecodeJSON(value)
			if err != nil {
				t.Fatal(err)
			}
			attrs = append(attrs, Binding{Name: key, Value: parsed})
		}
		state, err := NewState(alias, raw.State, attrs)
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, state)
	}
	now, err := time.Parse(time.RFC3339, fixtures.ClockUTC)
	if err != nil {
		t.Fatal(err)
	}
	zone, err := time.LoadLocation(fixtures.Timezone)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := NewInputs(states, nil, now, zone)
	if err != nil {
		t.Fatal(err)
	}
	return inputs, fixtures
}

func typedValue(value Value) any {
	switch value.kind {
	case valueNone:
		return map[string]any{"type": "none", "value": nil}
	case valueBool:
		return map[string]any{"type": "bool", "value": value.b}
	case valueInt:
		return map[string]any{"type": "int", "value": value.i}
	case valueFloat:
		return map[string]any{"type": "float", "value": value.f}
	case valueString:
		return map[string]any{"type": "str", "value": value.s}
	case valueList:
		items := make([]any, len(value.list))
		for i, item := range value.list {
			items[i] = typedValue(item)
		}
		return map[string]any{"type": "list", "value": items}
	default:
		return map[string]any{"type": value.Type()}
	}
}

func expectedNative(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value["type"] == "Wrapper" && value["category"] == "list" {
		value["type"] = "list"
		delete(value, "category")
		delete(value, "render_result")
	}
	return value
}

func TestNHA043TenSpikeCases(t *testing.T) {
	inputs, fixtures := fixtureInputs(t)
	data, err := os.ReadFile("../nha006/observed.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected expectedSet
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	if len(expected.HA) != 10 || len(fixtures.Cases) != 10 {
		t.Fatal("ten-case fixture drift")
	}
	for i, fixture := range fixtures.Cases {
		t.Run(fixture.ID, func(t *testing.T) {
			if expected.HA[i].ID != fixture.ID {
				t.Fatal("fixture order drift")
			}
			program, err := Compile(fixture.Template)
			if err != nil {
				t.Fatal(err)
			}
			result, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if result.Text() != expected.HA[i].Raw.Text {
				t.Fatalf("text got %q want %q", result.Text(), expected.HA[i].Raw.Text)
			}
			got, _ := json.Marshal(typedValue(result.Native()))
			want, _ := json.Marshal(expectedNative(t, expected.HA[i].Raw.Native))
			if string(got) != string(want) {
				t.Fatalf("native got %s want %s", got, want)
			}
		})
	}
}

func TestNHA043DatesAndVariables(t *testing.T) {
	inputs, _ := fixtureInputs(t)
	cases := []struct{ source, text, kind string }{
		{"{{ as_datetime('2024-03-09').date() }}", "2024-03-09", "str"},
		{"{{ (now().date() - as_datetime('2024-03-09').date()).days }}", "728", "int"},
		{"{{ (as_datetime('2024-03-10').date() - as_datetime('2024-03-09').date()).days }}", "1", "int"},
		{"{{ (as_datetime('2024-03-11T00:00:00-04:00').date() - as_datetime('2024-03-10T00:00:00-05:00').date()).days }}", "1", "int"},
		{"{{ (as_datetime('2024-11-04T00:00:00-05:00').date() - as_datetime('2024-11-03T00:00:00-04:00').date()).days }}", "1", "int"},
		{"{{ now() }}", "2026-03-07 23:30:00-05:00", "str"},
		{"{{ as_datetime('2024-03-09') }}", "2024-03-09 00:00:00", "str"},
	}
	for _, tc := range cases {
		program, err := Compile(tc.source)
		if err != nil {
			t.Fatalf("%s: %v", tc.source, err)
		}
		got, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
		if err != nil {
			t.Fatalf("%s: %v", tc.source, err)
		}
		if got.Text() != tc.text || got.Native().Type() != tc.kind {
			t.Fatalf("%s got %q/%s", tc.source, got.Text(), got.Native().Type())
		}
	}
	jsonValue, err := DecodeJSON([]byte(`{"state":"on","battery":0,"energy":12.5,"illuminance":1,"occupancy":false,"power":0,"water_leak":null}`))
	if err != nil {
		t.Fatal(err)
	}
	jsonInputs, err := NewInputs(nil, []Binding{{Name: "value_json", Value: jsonValue}}, inputs.now, inputs.zone)
	if err != nil {
		t.Fatal(err)
	}
	forms := []struct{ source, text, kind string }{
		{`{{ value_json.state }}`, "on", "str"},
		{`{{ value_json["battery"] }}`, "0", "int"},
		{`{{ value_json["energy"] }}`, "12.5", "float"},
		{`{{ value_json["illuminance"] }}`, "1", "int"},
		{`{{ value_json["occupancy"] }}`, "False", "bool"},
		{`{{ value_json["power"] }}`, "0", "int"},
		{`{{ value_json["state"] }}`, "on", "str"},
		{`{{ value_json["water_leak"] }}`, "None", "none"},
	}
	for _, form := range forms {
		program, err := Compile(form.source)
		if err != nil {
			t.Fatalf("%s: %v", form.source, err)
		}
		got, err := Evaluate(context.Background(), program, jsonInputs, DefaultLimits())
		if err != nil {
			t.Fatalf("%s: %v", form.source, err)
		}
		if got.Text() != form.text || got.Native().Type() != form.kind {
			t.Fatalf("%s got %q/%s", form.source, got.Text(), got.Native().Type())
		}
	}
}

func TestNHA043MeasuredNativeEdges(t *testing.T) {
	inputs, _ := fixtureInputs(t)
	cases := []struct{ source, text, kind string }{
		{"  {{ 1 }}  ", "1", "int"},
		{"  hi  ", "hi", "str"},
		{`{{ "'hello'" }}`, "'hello'", "str"},
		{"{{ '.5' }}", ".5", "float"},
		{"{{ '1e3' }}", "1e3", "str"},
		{"{{ '001' }}", "001", "str"},
		{"{{ true | float(0) }}", "1.0", "float"},
		{"{{ 0.0000001 }}", "1e-07", "str"},
		{"{{ 100000000000000000000.0 }}", "1e+20", "str"},
		{`{{ ["a'b", "c\\d"] }}`, `["a'b", 'c\\d']`, "list"},
		{"{{ [1.2, 2] }}", "[1.2, 2]", "list"},
		{"{{ '[1, 2]' }}", "[1, 2]", "list"},
		{"{{ (as_datetime('2024-03-09').date() - as_datetime('2024-03-10').date()) }}", "-1 day, 0:00:00", "str"},
		{"{{ (as_datetime('2024-03-09').date() - as_datetime('2024-03-09').date()) }}", "0:00:00", "str"},
		{"{{ as_datetime('2024-03-09T12:34:56.123456+00:00') }}", "2024-03-09 12:34:56.123456+00:00", "str"},
		{"{{ [true, none, 0, 0.0] }}", "[True, None, 0, 0.0]", "list"},
		{"{{ 1000000.0 }}", "1000000.0", "float"},
		{"{{ 1000000000000000.0 }}", "1000000000000000.0", "float"},
		{"{{ 10000000000000000.0 }}", "1e+16", "str"},
		{"{{ 0.0001 }}", "0.0001", "float"},
		{"{{ 0.00001 }}", "1e-05", "str"},
		{"{{ 0.000001 }}", "1e-06", "str"},
		{"{{ [100000000000000000000.0] }}", "[1e+20]", "list"},
		{"{{ ['\u0080'] }}", "['\\x80']", "list"},
		{"{{ ['\u00a0'] }}", "['\\xa0']", "list"},
		{"{{ ['\u2028'] }}", "['\\u2028']", "list"},
	}
	for _, tc := range cases {
		program, err := Compile(tc.source)
		if err != nil {
			t.Fatalf("%s: %v", tc.source, err)
		}
		result, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
		if err != nil {
			t.Fatalf("%s: %v", tc.source, err)
		}
		if result.Text() != tc.text || result.Native().Type() != tc.kind {
			t.Fatalf("%s got %q/%s, want %q/%s", tc.source, result.Text(), result.Native().Type(), tc.text, tc.kind)
		}
	}
	for _, tc := range []struct{ source, character string }{
		{"{{ ['\u0080'] }}", "\u0080"},
		{"{{ ['\u00a0'] }}", "\u00a0"},
		{"{{ ['\u2028'] }}", "\u2028"},
	} {
		program, err := Compile(tc.source)
		if err != nil {
			t.Fatal(err)
		}
		result, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if native := result.Native(); native.kind != valueList || len(native.list) != 1 || native.list[0].s != tc.character {
			t.Fatalf("Unicode native list did not preserve the source rune: %s", tc.source)
		}
	}
	for _, source := range []string{
		"{{ 'Ä'.lower() }}",
		"{{ '9223372036854775808' }}",
		"{{ '[9223372036854775808]' }}",
		"{{ \"{'a': 1}\" }}",
		"{{ '(1, 2)' }}",
		"{{ [missing] }}",
		"{{ [[missing]] }}",
		"{{ [as_datetime('2024-03-09').date()] }}",
		"{{ [now()] }}",
		"{{ [(as_datetime('2024-03-09').date() - as_datetime('2024-03-10').date())] }}",
	} {
		program, err := Compile(source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Evaluate(context.Background(), program, inputs, DefaultLimits()); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("expected explicit refusal for %s: %v", source, err)
		}
	}
	jsonValue, err := DecodeJSON([]byte(`{"state":"on"}`))
	if err != nil {
		t.Fatal(err)
	}
	jsonInputs, err := NewInputs(nil, []Binding{{Name: "value_json", Value: jsonValue}}, inputs.now, inputs.zone)
	if err != nil {
		t.Fatal(err)
	}
	program, err := Compile("{{ [value_json.missing] }}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(context.Background(), program, jsonInputs, DefaultLimits()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing JSON member in list: %v", err)
	}
}

type nilContext []byte

func (nilContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (nilContext) Done() <-chan struct{}       { return nil }
func (nilContext) Err() error                  { return nil }
func (nilContext) Value(any) any               { return nil }

type interruptContext struct{ calls atomic.Int32 }

func (*interruptContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*interruptContext) Done() <-chan struct{}       { return nil }
func (c *interruptContext) Err() error {
	if c.calls.Add(1) > 5 {
		return context.Canceled
	}
	return nil
}
func (*interruptContext) Value(any) any { return nil }

func TestNHA043BoundsAndIsolation(t *testing.T) {
	inputs, _ := fixtureInputs(t)
	bad := []string{
		strings.Repeat("x", maxSourceBytes+1),
		"{% include '/etc/passwd' %}",
		"{{ open('/etc/passwd') }}",
		"{{ requests.get('https://example.com') }}",
		"{{ now().__class__.__mro__ }}",
		"{{ value_json.__class__() }}",
		"{{ " + strings.Repeat("-", maxParseDrivers+1) + "1 }}",
		"{{ value_json" + strings.Repeat(".state", maxParseDrivers+1) + " }}",
	}
	for _, source := range bad {
		if _, err := Compile(source); err == nil {
			t.Fatalf("unsafe source admitted: %q", source)
		}
	}
	program, err := Compile("{{ 'abc' }}")
	if err != nil {
		t.Fatal(err)
	}
	shortOutput, _ := NewLimits(defaultSteps, 2)
	if _, err := Evaluate(context.Background(), program, inputs, shortOutput); !errors.Is(err, ErrBudget) {
		t.Fatalf("output limit: %v", err)
	}
	shortCompute, _ := NewLimits(1, defaultOutput)
	if _, err := Evaluate(context.Background(), program, inputs, shortCompute); !errors.Is(err, ErrBudget) {
		t.Fatalf("compute limit: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Evaluate(ctx, program, inputs, DefaultLimits()); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancellation: %v", err)
	}
	loop, err := Compile("{{ expand('group.nha_rooms') | map(attribute='entity_id') | list }}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(&interruptContext{}, loop, inputs, DefaultLimits()); !errors.Is(err, ErrCancelled) {
		t.Fatalf("in-flight cancellation: %v", err)
	}
	old, _ := String(strings.Repeat("a", 1024))
	newText, _ := String(strings.Repeat("b", 1024))
	bombInputs, err := NewInputs(nil, []Binding{{Name: "old", Value: old}, {Name: "new", Value: newText}}, inputs.now, inputs.zone)
	if err != nil {
		t.Fatal(err)
	}
	bomb, err := Compile("{{ old.replace('a',new) }}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(context.Background(), bomb, bombInputs, DefaultLimits()); !errors.Is(err, ErrBudget) {
		t.Fatalf("replacement allocation guard: %v", err)
	}
	control, _ := String(strings.Repeat("\x7f", maxStringBytes))
	quoteInputs, err := NewInputs(nil, []Binding{{Name: "payload", Value: control}}, inputs.now, inputs.zone)
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := Compile("{{ [payload] }}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Evaluate(context.Background(), quoted, quoteInputs, DefaultLimits()); !errors.Is(err, ErrBudget) {
		t.Fatalf("quoted output expansion guard: %v", err)
	}
	var typedNil nilContext
	if _, err := Evaluate(typedNil, program, inputs, DefaultLimits()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("typed nil context: %v", err)
	}
	for i := 0; i < 20; i++ {
		got, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
		if err != nil || got.Text() != "abc" {
			t.Fatalf("independent render: %q, %v", got.Text(), err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := Evaluate(context.Background(), program, inputs, DefaultLimits())
			if err != nil || got.Text() != "abc" {
				t.Errorf("concurrent: %q %v", got.Text(), err)
			}
		}()
	}
	wg.Wait()
}

func TestNHA043IRNodeBoundary(t *testing.T) {
	boundarySource := func(names int) string {
		items := make([]string, names)
		for i := range items {
			if i < 20 {
				items[i] = "1-1"
			} else {
				items[i] = "1"
			}
		}
		return "{{ [" + strings.Join(items, ",") + "]" + strings.Repeat("|list", 11) + " }}"
	}
	program, err := Compile(boundarySource(75))
	if err != nil {
		t.Fatalf("near-boundary program rejected: %v", err)
	}
	var count func(expression) int
	count = func(x expression) int {
		result := 1
		for _, child := range x.children {
			result += count(child)
		}
		return result
	}
	irNodes := 0
	for _, segment := range program.segments {
		if segment.expr != nil {
			irNodes += count(*segment.expr)
		}
	}
	if irNodes != 127 || irNodes > maxIRNodes {
		t.Fatalf("unexpected admitted IR size %d", irNodes)
	}
	if _, err := Compile(boundarySource(80)); !errors.Is(err, ErrBudget) {
		t.Fatalf("over-boundary IR must be refused: %v", err)
	}
}

func TestNHA043ValueAdmission(t *testing.T) {
	if _, err := DecodeJSON([]byte(`9223372036854775808`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("integer overflow: %v", err)
	}
	if _, err := DecodeJSON([]byte(`1.5`)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInputs(nil, []Binding{{Name: "value_json", Value: Value{}}}, time.Now(), time.UTC); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero value: %v", err)
	}
	unit, _ := String(strings.Repeat("x", 100))
	items := make([]Value, 80)
	for i := range items {
		items[i] = unit
	}
	if _, err := List(items); err != nil {
		t.Fatal(err)
	}
	tooMany := make([]Value, 90)
	for i := range tooMany {
		tooMany[i] = unit
	}
	if _, err := List(tooMany); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("aggregate value budget: %v", err)
	}
	child, _ := List(items)
	vars := make([]Binding, 12)
	for i := range vars {
		vars[i] = Binding{Name: "v" + string(rune('a'+i)), Value: child}
	}
	if _, err := NewInputs(nil, vars, time.Now(), time.UTC); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("aggregate input budget: %v", err)
	}
	object, err := Object([]Binding{{Name: "state", Value: unit}})
	if err != nil {
		t.Fatal(err)
	}
	in, err := NewInputs(nil, []Binding{{Name: "Trigger", Value: object}}, time.Now(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	object.obj["state"] = Bool(false)
	program, err := Compile("{{ Trigger.state }}")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Evaluate(context.Background(), program, in, DefaultLimits())
	if err != nil || result.Text() != strings.Repeat("x", 100) {
		t.Fatalf("copy/admission: %q %v", result.Text(), err)
	}
	if _, err := NewInputs(nil, nil, time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("date range: %v", err)
	}
}
