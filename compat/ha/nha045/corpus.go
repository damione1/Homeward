package nha045

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type objectWire struct {
	ID   string `json:"id"`
	YAML string `json:"yaml"`
}
type entityWire struct {
	ID         string   `json:"id"`
	State      string   `json:"state"`
	Brightness *float64 `json:"brightness"`
}
type phaseWire struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	AtMS       int64    `json:"at_ms"`
	Run        string   `json:"run"`
	Object     string   `json:"object"`
	Entity     string   `json:"entity"`
	State      string   `json:"state"`
	Brightness *float64 `json:"brightness"`
	Count      int      `json:"count"`
}
type checkWire struct {
	Kind    string  `json:"kind"`
	Run     *string `json:"run"`
	Entity  *string `json:"entity"`
	AtMS    int64   `json:"at_ms"`
	Outcome *string `json:"outcome"`
}
type fixtureWire struct {
	Schema    string       `json:"schema"`
	ID        string       `json:"id"`
	Origin    string       `json:"origin"`
	Zone      string       `json:"zone"`
	Root      string       `json:"root"`
	Objects   []objectWire `json:"objects"`
	Entities  []entityWire `json:"entities"`
	Phases    []phaseWire  `json:"phases"`
	Checks    []checkWire  `json:"checks"`
	Forbidden []string     `json:"forbidden"`
}
type Fixture struct {
	wire   fixtureWire
	phases []phase
	checks []expectedCheck
	origin time.Time
	zone   *time.Location
	hash   string
	valid  bool
}

func (f Fixture) ID() string     { return f.wire.ID }
func (f Fixture) SHA256() string { return f.hash }
func (f Fixture) JSON() ([]byte, error) {
	if !f.valid {
		return nil, fail(InvalidFixture, "zero fixture")
	}
	return json.Marshal(f.wire)
}
func DecodeFixture(data []byte) (Fixture, error) {
	var document json.RawMessage
	if err := strictJSON(data, &document, MaxInputBytes); err != nil {
		return Fixture{}, fail(InvalidFixture, err.Error())
	}
	if e := requireFields(data, "schema", "id", "origin", "zone", "root", "objects", "entities", "phases", "checks", "forbidden"); e != nil {
		return Fixture{}, fail(InvalidFixture, e.Error())
	}
	var envelope struct {
		Phases   []json.RawMessage `json:"phases"`
		Entities []json.RawMessage `json:"entities"`
		Objects  []json.RawMessage `json:"objects"`
		Checks   []json.RawMessage `json:"checks"`
	}
	if e := json.Unmarshal(data, &envelope); e != nil {
		return Fixture{}, fail(InvalidFixture, e.Error())
	}
	phases := make([]phaseWire, 0, len(envelope.Phases))
	for _, data := range envelope.Phases {
		p, e := decodePhaseBoundary(data)
		if e != nil {
			return Fixture{}, fail(InvalidFixture, e.Error())
		}
		phases = append(phases, p)
	}
	for _, v := range envelope.Entities {
		if e := requireFields(v, "id", "state", "brightness"); e != nil {
			return Fixture{}, fail(InvalidFixture, e.Error())
		}
	}
	for _, v := range envelope.Objects {
		if e := requireFields(v, "id", "yaml"); e != nil {
			return Fixture{}, fail(InvalidFixture, e.Error())
		}
	}
	checks := make([]checkWire, 0, len(envelope.Checks))
	for _, data := range envelope.Checks {
		check, e := decodeCheckBoundary(data)
		if e != nil {
			return Fixture{}, fail(InvalidFixture, e.Error())
		}
		checks = append(checks, check)
	}
	var w fixtureWire
	if err := strictJSON(data, &w, MaxInputBytes); err != nil {
		return Fixture{}, fail(InvalidFixture, err.Error())
	}
	w.Phases, w.Checks = phases, checks
	return admitFixture(w)
}
func admitFixture(w fixtureWire) (Fixture, error) {
	bad := func(s string) (Fixture, error) { return Fixture{}, fail(InvalidFixture, s) }
	if w.Schema != Schema || w.Zone != "America/Toronto" {
		return bad("schema or zone outside corpus")
	}
	if _, e := parseID(w.ID); e != nil {
		return bad(e.Error())
	}
	origin, e := time.Parse(time.RFC3339, w.Origin)
	if e != nil || origin.IsZero() || origin.Location() != time.UTC {
		return bad("explicit UTC origin required")
	}
	zone, e := time.LoadLocation(w.Zone)
	if e != nil {
		return bad(e.Error())
	}
	if len(w.Objects) == 0 || len(w.Objects) > MaxObjects || len(w.Entities) == 0 || len(w.Entities) > MaxEntities || len(w.Phases) == 0 || len(w.Phases) > MaxPhases || len(w.Checks) == 0 || len(w.Checks) > MaxFacts {
		return bad("fixture cardinality outside bounds")
	}
	objects := make(map[string]bool)
	for _, o := range w.Objects {
		if !(strings.HasPrefix(o.ID, "script.nha045_") || strings.HasPrefix(o.ID, "automation.nha045_")) || objects[o.ID] || len(o.YAML) == 0 || len(o.YAML) > 64<<10 {
			return bad("invalid or duplicate synthetic object")
		}
		objects[o.ID] = true
	}
	if !objects[w.Root] {
		return bad("root object absent")
	}
	entities := make(map[string]bool)
	for _, v := range w.Entities {
		if !syntheticEntity(v.ID) || entities[v.ID] || v.State != "off" && v.State != "on" && v.State != "unknown" {
			return bad("invalid initial entity")
		}
		entities[v.ID] = true
		if e := validateData(State, dataWire{Entity: ptr(v.ID), State: ptr(v.State), Brightness: v.Brightness}); e != nil {
			return bad(e.Error())
		}
	}
	f := Fixture{wire: w, origin: origin, zone: zone, valid: true}
	last := int64(0)
	phaseIDs := make(map[string]bool)
	runs := make(map[runKey]objectID)
	for _, p := range w.Phases {
		if _, e := parseID(p.ID); e != nil || phaseIDs[p.ID] {
			return bad("invalid/duplicate phase identity")
		}
		phaseIDs[p.ID] = true
		if p.AtMS < last || p.AtMS > 86400000 {
			return bad("phase time is not bounded monotone")
		}
		last = p.AtMS
		admitted, e := admitPhase(p, objects, entities, runs)
		if e != nil {
			return bad(e.Error())
		}
		f.phases = append(f.phases, admitted)
	}
	for _, c := range w.Checks {
		check, e := admitCheck(c)
		if e != nil {
			return bad(e.Error())
		}
		f.checks = append(f.checks, check)
	}
	for _, id := range w.Forbidden {
		if !syntheticEntity(id) {
			return bad("invalid forbidden action")
		}
	}
	encoded, e := json.Marshal(w)
	if e != nil || len(encoded) > MaxInputBytes {
		return bad("fixture too large")
	}
	digest := sha256.Sum256(encoded)
	f.hash = hex.EncodeToString(digest[:])
	return f, nil
}

// Corpus is executable source material, not a claim that an oracle was run.
// Every returned fixture owns a decoded snapshot and has a stable input hash.
func Corpus() ([]Fixture, error) {
	action := func(target string) string {
		return "- action: switch.turn_on\n  target: {entity_id: " + target + "}\n  data: {}\n"
	}
	effect := "switch.nha045_effect"
	gate := "switch.nha045_gate"
	child := "script.nha045_child"
	root := "script.nha045_root"
	object := func(id, yaml string) objectWire { return objectWire{id, yaml} }
	step := func(k PhaseKind, at int64, run, obj string) phaseWire {
		return phaseWire{Kind: phaseName(k), AtMS: at, Run: run, Object: obj}
	}
	check := func(k FactKind, run, entity string, at int64, outcome Outcome) checkWire {
		c := checkWire{Kind: factName(k), AtMS: at, Outcome: ptr(outcomeName(outcome))}
		if run != "" {
			c.Run = ptr(run)
		}
		if entity != "" {
			c.Entity = ptr(entity)
		}
		return c
	}
	base := func(id string) fixtureWire {
		return fixtureWire{Schema: Schema, ID: id, Origin: "2024-03-11T04:30:00Z", Zone: "America/Toronto", Root: root, Entities: []entityWire{{effect, "off", nil}, {gate, "off", nil}}, Forbidden: []string{}}
	}
	var all []fixtureWire
	for _, mode := range []string{"restart", "queued", "single", "parallel"} {
		id := map[string]string{"restart": "01-restart-delay", "queued": "02-queued-max", "single": "03-single-busy", "parallel": "04-parallel-max"}[mode]
		f := base(id)
		yaml := "mode: " + mode + "\n"
		if mode == "queued" || mode == "parallel" {
			yaml += "max: 2\n"
		}
		yaml += "sequence:\n- delay: 5\n" + action(effect)
		f.Objects = []objectWire{object(root, yaml)}
		f.Phases = []phaseWire{step(Invoke, 0, "a", root), step(AwaitStartup, 0, "a", root)}
		switch mode {
		case "restart":
			f.Phases = append(f.Phases, step(Advance, 2000, "", ""), step(Invoke, 2000, "b", root), step(AwaitStartup, 2000, "b", root), step(Advance, 5000, "", ""), step(Advance, 7000, "", ""), step(AwaitTerminal, 7000, "a", root), step(AwaitTerminal, 7000, "b", root))
			f.Checks = []checkWire{check(Action, "b", effect, 7000, Completed), check(Terminal, "a", "", 2000, Cancelled), check(Terminal, "b", "", 7000, Finished)}
		case "queued":
			f.Phases = append(f.Phases, step(Advance, 1000, "", ""), step(Invoke, 1000, "b", root), step(AwaitStartup, 1000, "b", root), step(Advance, 2000, "", ""), step(Invoke, 2000, "c", root), step(AwaitStartup, 2000, "c", root), step(Advance, 5000, "", ""), step(AwaitTerminal, 5000, "a", root), step(AwaitStartup, 5000, "b", root), step(Advance, 10000, "", ""), step(AwaitTerminal, 10000, "b", root))
			f.Checks = []checkWire{check(Action, "a", effect, 5000, Completed), check(Action, "b", effect, 10000, Completed), check(Admission, "c", "", 2000, MaxExceeded), check(Terminal, "b", "", 10000, Finished)}
		case "single":
			f.Phases = append(f.Phases, step(Advance, 1000, "", ""), step(Invoke, 1000, "b", root), step(AwaitStartup, 1000, "b", root), step(Advance, 5000, "", ""), step(AwaitTerminal, 5000, "a", root))
			f.Checks = []checkWire{check(Admission, "b", "", 1000, SingleBusy), check(Action, "a", effect, 5000, Completed), check(Terminal, "a", "", 5000, Finished)}
		case "parallel":
			f.Entities[0].State = "on"
			f.Phases = append(f.Phases, step(Invoke, 0, "b", root), step(AwaitStartup, 0, "b", root), step(Invoke, 0, "c", root), step(AwaitStartup, 0, "c", root), step(Advance, 5000, "", ""), step(AwaitTerminal, 5000, "a", root), step(AwaitTerminal, 5000, "b", root))
			f.Checks = []checkWire{check(Admission, "c", "", 0, MaxExceeded), check(Action, "a", effect, 5000, Completed), check(Action, "b", effect, 5000, Completed), check(Terminal, "a", "", 5000, Finished), check(Terminal, "b", "", 5000, Finished)}
		}
		all = append(all, f)
	}
	waitYAML := "sequence:\n- wait_for_trigger:\n  - trigger: event\n    event_type: nha045_release\n    id: release\n  timeout: {seconds: 5}\n  continue_on_timeout: false\n" + action(effect)
	for _, release := range []bool{true, false} {
		id := "06-wait-timeout-abort"
		if release {
			id = "05-wait-event-release"
		}
		f := base(id)
		f.Objects = []objectWire{object(root, waitYAML)}
		f.Phases = []phaseWire{step(Invoke, 0, "a", root), step(AwaitWaiting, 0, "", root)}
		if release {
			emit := step(Emit, 4000, "", "")
			emit.Entity = "nha045_release"
			f.Phases = append(f.Phases, step(Advance, 4000, "", ""), emit, step(AwaitTerminal, 4000, "a", root))
			f.Checks = []checkWire{check(Action, "a", effect, 4000, Completed), check(Terminal, "a", "", 4000, Finished)}
		} else {
			emit := step(Emit, 6000, "", "")
			emit.Entity = "nha045_release"
			f.Phases = append(f.Phases, step(Advance, 5000, "", ""), step(AwaitTerminal, 5000, "a", root), step(Advance, 6000, "", ""), emit)
			f.Checks = []checkWire{check(Terminal, "a", "", 5000, TimeoutStopped)}
			f.Forbidden = []string{effect}
		}
		all = append(all, f)
	}
	{
		f := base("07-state-for-attribute")
		f.Root = "automation.nha045_state"
		f.Entities = append(f.Entities, entityWire{"light.nha045_source", "off", ptr(0.0)})
		f.Objects = []objectWire{object(f.Root, "id: nha045_state\ntriggers:\n- trigger: state\n  entity_id: light.nha045_source\n  to: 'on'\n  for: '00:00:05'\nactions:\n"+action(effect)), object("script.nha045_barrier", "sequence:\n- wait_for_trigger:\n  - trigger: event\n    event_type: nha045_release\n    id: release\n")}
		for _, v := range []struct {
			at         int64
			state      string
			brightness float64
		}{{1000, "on", 0}, {2000, "on", 1}, {3000, "off", 1}, {4000, "on", 1}, {7000, "on", 2}} {
			p := step(Publish, v.at, "", "")
			p.Entity = "light.nha045_source"
			p.State = v.state
			p.Brightness = ptr(v.brightness)
			f.Phases = append(f.Phases, step(Advance, v.at, "", ""), p)
		}
		p := step(AwaitActions, 9000, "", "")
		p.Count = 1
		f.Phases = append(f.Phases, step(Advance, 9000, "", ""), p, step(AwaitRunTerminal, 9000, "", f.Root))
		f.Checks = []checkWire{check(Action, "auto/1", effect, 9000, Completed), check(Terminal, "auto/1", "", 9000, Finished)}
		all = append(all, f)
	}
	for _, detached := range []bool{false, true} {
		id := "08-direct-parent-cancel"
		call := "- action: " + child + "\n"
		if detached {
			id = "09-detached-parent-cancel"
			call = "- action: script.turn_on\n  target: {entity_id: " + child + "}\n"
		}
		f := base(id)
		for _, name := range []string{"child_enter", "child_exit", "parent_after", "parent_exit"} {
			f.Entities = append(f.Entities, entityWire{"switch.nha045_" + name, "off", nil})
		}
		parent := "sequence:\n" + call + action("switch.nha045_parent_after")
		if detached {
			parent += "- wait_template: '{{ false }}'\n" + action("switch.nha045_parent_exit")
		}
		f.Objects = []objectWire{object(root, parent), object(child, "mode: parallel\nsequence:\n"+action("switch.nha045_child_enter")+"- wait_template: \"{{ is_state('switch.nha045_gate', 'on') }}\"\n"+action("switch.nha045_child_exit"))}
		f.Phases = []phaseWire{step(Invoke, 0, "a", root), step(AwaitWaiting, 0, "", child)}
		if detached {
			f.Phases = append(f.Phases, step(AwaitWaiting, 0, "", root))
		}
		p := step(Publish, 0, "", "")
		p.Entity = gate
		p.State = "on"
		f.Phases = append(f.Phases, step(Cancel, 0, "a", root), step(AwaitTerminal, 0, "a", root), p, step(AwaitRunTerminal, 0, "", child))
		f.Checks = []checkWire{check(Action, "a/child/1", "switch.nha045_child_enter", 0, Completed), check(Terminal, "a", "", 0, Cancelled)}
		f.Forbidden = []string{"switch.nha045_parent_exit"}
		if detached {
			f.Checks = append(f.Checks, check(Action, "a", "switch.nha045_parent_after", 0, Completed), check(Action, "a/child/1", "switch.nha045_child_exit", 0, Completed), check(Terminal, "a/child/1", "", 0, Finished))
		} else {
			f.Checks = append(f.Checks, check(Terminal, "a/child/1", "", 0, Cancelled))
			f.Forbidden = append(f.Forbidden, "switch.nha045_parent_after", "switch.nha045_child_exit")
		}
		all = append(all, f)
	}
	{
		f := base("10-template-parallel-error-join")
		for _, name := range []string{"fail", "other", "forbidden", "wrong"} {
			f.Entities = append(f.Entities, entityWire{"switch.nha045_" + name, "off", nil})
		}
		f.Entities = append(f.Entities, entityWire{"sensor.nha045_missing", "off", nil})
		yaml := "sequence:\n- if:\n  - condition: template\n    value_template: \"{{ (now().date() - as_datetime('2024-03-11').date()).days or true }}\"\n  - condition: template\n    value_template: \"{{ (states('sensor.nha045_missing') | float(0)) or true }}\"\n  then:\n  - parallel:\n    - sequence:\n" + indent(action("switch.nha045_fail"), 6) + "    - sequence:\n      - wait_template: \"{{ is_state('switch.nha045_gate', 'on') }}\"\n" + indent(action("switch.nha045_other"), 6) + indent(action("switch.nha045_forbidden"), 2) + "  else:\n" + indent(action("switch.nha045_wrong"), 2)
		f.Objects = []objectWire{object(root, yaml)}
		p := step(Publish, 0, "", "")
		p.Entity = gate
		p.State = "on"
		count := step(AwaitActions, 0, "", "")
		count.Count = 1
		f.Phases = []phaseWire{step(Invoke, 0, "a", root), step(AwaitWaiting, 0, "", root), count, p, step(AwaitTerminal, 0, "a", root)}
		f.Checks = []checkWire{check(Action, "a", "switch.nha045_fail", 0, CommandFailed), check(Action, "a", "switch.nha045_other", 0, Completed), check(Terminal, "a", "", 0, CommandFailed)}
		f.Forbidden = []string{"switch.nha045_forbidden", "switch.nha045_wrong"}
		all = append(all, f)
	}
	result := make([]Fixture, 0, len(all))
	for _, w := range all {
		for i := range w.Phases {
			w.Phases[i].ID = fmt.Sprintf("phase-%03d", i+1)
		}
		data, e := json.Marshal(w)
		if e != nil {
			return nil, e
		}
		f, e := DecodeFixture(data)
		if e != nil {
			return nil, fmt.Errorf("%s: %w", w.ID, e)
		}
		result = append(result, f)
	}
	return result, nil
}
func indent(s string, n int) string {
	prefix := strings.Repeat(" ", n)
	return prefix + strings.TrimSuffix(strings.ReplaceAll(s, "\n", "\n"+prefix), prefix)
}

func ValidateExpected(f Fixture, c Capture) error {
	if !f.valid || !c.valid || f.ID() != c.scenario.value || c.wire.FixtureSHA256 != f.SHA256() {
		return fail(InvalidObservation, "fixture/capture mismatch")
	}
	bindings := make(map[rawRunID]runKey)
	for _, r := range c.runs {
		bindings[r.raw] = r.key
	}
	actionCount := 0
	expectedActions := 0
	for _, check := range f.checks {
		want, e := describeCheck(check)
		if e != nil {
			return fail(InvalidFixture, e.Error())
		}
		if want.kind == Action {
			expectedActions++
		}
		found := 0
		for _, got := range c.facts {
			logical := runKey{}
			if got.run.present {
				logical = bindings[got.run.value]
			} else if got.kind == Admission {
				id, e := parseID(strings.TrimSuffix(got.key.value, "/admission"))
				if e != nil {
					return fail(InvalidObservation, e.Error())
				}
				logical = runKey{id}
			}
			if want.matches(got, logical) {
				found++
			}
		}
		if found != 1 {
			run := "absent"
			if want.target.run.present {
				run = want.target.run.value.value
			}
			return fail(SemanticDivergence, fmt.Sprintf("expected %s run %s at %d matched %d times", factName(want.kind), run, want.target.atMS, found))
		}
	}
	for _, got := range c.facts {
		if got.kind != Action {
			continue
		}
		actionCount++
		action, present := got.payload.(actionPayload)
		if !present {
			return fail(InvalidObservation, "action lost its admitted payload")
		}
		for _, id := range f.wire.Forbidden {
			if action.entity.value == id {
				return fail(SemanticDivergence, "forbidden action: "+id)
			}
		}
	}
	if actionCount != expectedActions {
		return fail(SemanticDivergence, "unexpected action cardinality")
	}
	return nil
}
