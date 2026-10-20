package nha045

import (
	"encoding/json"
	"fmt"
	"strings"
)

type optionalEntityID struct {
	value   entityID
	present bool
}
type checkTarget struct {
	run  optionalRunKey
	atMS int64
}
type expectedCheck interface{ isExpectedCheck() }
type admissionCheck struct {
	target  checkTarget
	outcome Outcome
}
type actionCheck struct {
	target  checkTarget
	entity  optionalEntityID
	outcome Outcome
}
type terminalCheck struct {
	target  checkTarget
	outcome Outcome
}

func (admissionCheck) isExpectedCheck() {}
func (actionCheck) isExpectedCheck()    {}
func (terminalCheck) isExpectedCheck()  {}

func decodeCheckBoundary(data []byte) (checkWire, error) {
	if e := requireFields(data, "kind", "run", "entity", "at_ms", "outcome"); e != nil {
		return checkWire{}, e
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(data, &fields); e != nil {
		return checkWire{}, e
	}
	for _, key := range []string{"kind", "at_ms", "outcome"} {
		if strings.TrimSpace(string(fields[key])) == "null" {
			return checkWire{}, fmt.Errorf("expected check primitive %s cannot be null", key)
		}
	}
	var w checkWire
	if e := strictJSON(data, &w, MaxInputBytes); e != nil {
		return checkWire{}, e
	}
	return w, nil
}

func validCheckOutcome(kind FactKind, outcome Outcome) bool {
	switch kind {
	case Admission:
		return outcome == Accepted || outcome == Queued || outcome == SingleBusy || outcome == MaxExceeded
	case Action:
		return outcome == Completed || outcome == CommandFailed
	case Terminal:
		return outcome == Finished || outcome == Cancelled || outcome == TimeoutStopped || outcome == CommandFailed
	case FactInvalid, Started, State, Wait, Cancellation:
		return false
	default:
		return false
	}
}

func admitCheck(w checkWire) (expectedCheck, error) {
	kind, e := parseFactKind(w.Kind)
	if e != nil {
		return nil, e
	}
	if w.Outcome == nil {
		return nil, fmt.Errorf("expected outcome absent")
	}
	outcome, e := parseOutcome(*w.Outcome)
	if e != nil || !validCheckOutcome(kind, outcome) {
		return nil, fmt.Errorf("contradictory expected kind/outcome")
	}
	if w.AtMS < 0 || w.AtMS > 86400000 {
		return nil, fmt.Errorf("invalid expected tick")
	}
	target := checkTarget{atMS: w.AtMS}
	if w.Run != nil {
		id, e := parseID(*w.Run)
		if e != nil {
			return nil, e
		}
		target.run = optionalRunKey{runKey{id}, true}
	}
	entity := optionalEntityID{}
	if w.Entity != nil {
		if kind != Action || !syntheticEntity(*w.Entity) {
			return nil, fmt.Errorf("unexpected expected entity")
		}
		id, e := parseID(*w.Entity)
		if e != nil {
			return nil, e
		}
		entity = optionalEntityID{entityID{id}, true}
	}
	switch kind {
	case Admission:
		return admissionCheck{target, outcome}, nil
	case Action:
		return actionCheck{target, entity, outcome}, nil
	case Terminal:
		return terminalCheck{target, outcome}, nil
	case FactInvalid, Started, State, Wait, Cancellation:
		return nil, fmt.Errorf("invalid expected kind")
	default:
		return nil, fmt.Errorf("unhandled expected kind")
	}
}

type checkDescription struct {
	kind    FactKind
	target  checkTarget
	entity  optionalEntityID
	outcome Outcome
}

func describeCheck(check expectedCheck) (checkDescription, error) {
	var d checkDescription
	switch v := check.(type) {
	case admissionCheck:
		d = checkDescription{kind: Admission, target: v.target, outcome: v.outcome}
	case actionCheck:
		d = checkDescription{kind: Action, target: v.target, entity: v.entity, outcome: v.outcome}
	case terminalCheck:
		d = checkDescription{kind: Terminal, target: v.target, outcome: v.outcome}
	case nil:
		return d, fmt.Errorf("absent expected check")
	default:
		return d, fmt.Errorf("unhandled or typed-nil expected check")
	}
	if !validCheckOutcome(d.kind, d.outcome) || d.target.atMS < 0 || d.target.atMS > 86400000 {
		return checkDescription{}, fmt.Errorf("zero or invalid expected check")
	}
	if d.target.run.present {
		if _, e := parseID(d.target.run.value.value); e != nil {
			return checkDescription{}, e
		}
	}
	if d.entity.present && !syntheticEntity(d.entity.value.value) {
		return checkDescription{}, fmt.Errorf("invalid expected entity")
	}
	return d, nil
}

func (d checkDescription) matches(got fact, logical runKey) bool {
	if got.kind != d.kind || got.tick != d.target.atMS || d.target.run.present && logical != d.target.run.value {
		return false
	}
	switch actual := got.payload.(type) {
	case admissionPayload:
		return d.kind == Admission && actual.outcome == d.outcome
	case actionPayload:
		return d.kind == Action && actual.outcome == d.outcome && (!d.entity.present || actual.entity == d.entity.value)
	case terminalPayload:
		return d.kind == Terminal && actual.outcome == d.outcome
	default:
		return false
	}
}
