package nha045

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type phaseID struct{ identifier }

// Only admitted value variants enter the runtime. phaseWire remains the JSON
// boundary shared with HA, including its explicit empty/nullable fields.
type phase struct {
	id        phaseID
	atMS      int64
	operation phaseOperation
	valid     bool
}
type phaseOperation interface{ isPhase() }
type invokePhase struct {
	run    runKey
	object objectID
}
type advancePhase struct{}
type startupPhase struct {
	run    runKey
	object objectID
}
type waitingPhase struct{ object objectID }
type terminalPhase struct {
	run    runKey
	object objectID
}
type cancelPhase struct {
	run    runKey
	object objectID
}
type runTerminalPhase struct{ object objectID }
type publishPhase struct{ state statePayload }
type emitPhase struct{}
type actionsPhase struct{ count int }

func (invokePhase) isPhase()      {}
func (advancePhase) isPhase()     {}
func (startupPhase) isPhase()     {}
func (waitingPhase) isPhase()     {}
func (terminalPhase) isPhase()    {}
func (cancelPhase) isPhase()      {}
func (runTerminalPhase) isPhase() {}
func (publishPhase) isPhase()     {}
func (emitPhase) isPhase()        {}
func (actionsPhase) isPhase()     {}

func decodePhaseBoundary(data []byte) (phaseWire, error) {
	if e := requireFields(data, "id", "kind", "at_ms", "run", "object", "entity", "state", "brightness", "count"); e != nil {
		return phaseWire{}, e
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(data, &fields); e != nil {
		return phaseWire{}, e
	}
	for _, key := range []string{"id", "kind", "at_ms", "run", "object", "entity", "state", "count"} {
		if bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return phaseWire{}, fmt.Errorf("phase primitive %s cannot be null", key)
		}
	}
	var w phaseWire
	if e := strictJSON(data, &w, MaxInputBytes); e != nil {
		return phaseWire{}, e
	}
	return w, nil
}

func phaseObject(text string, objects map[string]bool) (objectID, error) {
	id, e := parseID(text)
	if e != nil || !objects[text] {
		return objectID{}, fmt.Errorf("phase references unknown object")
	}
	return objectID{id}, nil
}
func phaseRun(text string) (runKey, error) {
	id, e := parseID(text)
	if e != nil {
		return runKey{}, fmt.Errorf("invalid phase run: %w", e)
	}
	return runKey{id}, nil
}

func admitPhase(w phaseWire, objects, entities map[string]bool, runs map[runKey]objectID) (phase, error) {
	id, e := parseID(w.ID)
	if e != nil || w.AtMS < 0 || w.AtMS > 86400000 {
		return phase{}, fmt.Errorf("invalid phase identity/time")
	}
	kind, e := parsePhaseKind(w.Kind)
	if e != nil {
		return phase{}, e
	}
	p := phase{id: phaseID{id}, atMS: w.AtMS, valid: true}
	switch kind {
	case Invoke:
		run, e := phaseRun(w.Run)
		if e != nil {
			return phase{}, e
		}
		object, e := phaseObject(w.Object, objects)
		if e != nil {
			return phase{}, e
		}
		if !strings.HasPrefix(object.value, "script.") {
			return phase{}, fmt.Errorf("only script invocations are admitted")
		}
		if _, present := runs[run]; present {
			return phase{}, fmt.Errorf("duplicate invocation")
		}
		p.operation = invokePhase{run, object}
	case AwaitStartup, AwaitTerminal, Cancel:
		run, e := phaseRun(w.Run)
		if e != nil {
			return phase{}, e
		}
		object, present := runs[run]
		if !present || object.value != w.Object {
			return phase{}, fmt.Errorf("phase invocation/object reference mismatch")
		}
		switch kind {
		case AwaitStartup:
			p.operation = startupPhase{run, object}
		case AwaitTerminal:
			p.operation = terminalPhase{run, object}
		case Cancel:
			p.operation = cancelPhase{run, object}
		default:
			return phase{}, fmt.Errorf("unhandled invocation phase")
		}
	case AwaitWaiting, AwaitRunTerminal:
		object, e := phaseObject(w.Object, objects)
		if e != nil {
			return phase{}, e
		}
		switch kind {
		case AwaitWaiting:
			p.operation = waitingPhase{object}
		case AwaitRunTerminal:
			p.operation = runTerminalPhase{object}
		default:
			return phase{}, fmt.Errorf("unhandled object phase")
		}
	case Publish:
		if !entities[w.Entity] {
			return phase{}, fmt.Errorf("unregistered publication entity")
		}
		if w.Brightness != nil && (!strings.HasPrefix(w.Entity, "light.") || math.IsNaN(*w.Brightness) || math.IsInf(*w.Brightness, 0) || *w.Brightness < 0 || *w.Brightness > 100 || w.State == "unknown") {
			return phase{}, fmt.Errorf("brightness outside admitted light profile")
		}
		payload, e := admitPayload(State, dataWire{Entity: ptr(w.Entity), State: ptr(w.State), Brightness: w.Brightness})
		if e != nil {
			return phase{}, e
		}
		state, ok := payload.(statePayload)
		if !ok {
			return phase{}, fmt.Errorf("invalid publication variant")
		}
		p.operation = publishPhase{state}
	case Emit:
		if w.Entity != "nha045_release" {
			return phase{}, fmt.Errorf("unknown synthetic event")
		}
		p.operation = emitPhase{}
	case AwaitActions:
		if w.Count < 0 || w.Count > MaxFacts {
			return phase{}, fmt.Errorf("invalid action count")
		}
		p.operation = actionsPhase{w.Count}
	case Advance:
		p.operation = advancePhase{}
	case PhaseInvalid:
		return phase{}, fmt.Errorf("zero phase")
	default:
		return phase{}, fmt.Errorf("unhandled phase")
	}
	canonical, e := p.boundary()
	if e != nil {
		return phase{}, e
	}
	if !samePhaseBoundary(w, canonical) {
		return phase{}, fmt.Errorf("phase carries unused or contradictory fields")
	}
	if invoke, ok := p.operation.(invokePhase); ok {
		runs[invoke.run] = invoke.object
	}
	return p, nil
}

func samePhaseBoundary(a, b phaseWire) bool {
	brightness := a.Brightness == nil && b.Brightness == nil || a.Brightness != nil && b.Brightness != nil && *a.Brightness == *b.Brightness
	return a.ID == b.ID && a.Kind == b.Kind && a.AtMS == b.AtMS && a.Run == b.Run && a.Object == b.Object && a.Entity == b.Entity && a.State == b.State && a.Count == b.Count && brightness
}

// This conversion is only for the fixture/raw-evidence boundary. In particular
// runtime decisions never decode or dispatch on this nullable wire union.
func (p phase) boundary() (phaseWire, error) {
	if !p.valid || p.id.value == "" || p.atMS < 0 || p.atMS > 86400000 {
		return phaseWire{}, fmt.Errorf("zero or invalid phase")
	}
	w := phaseWire{ID: p.id.value, AtMS: p.atMS}
	switch v := p.operation.(type) {
	case invokePhase:
		w.Kind = phaseName(Invoke)
		w.Run = v.run.value
		w.Object = v.object.value
	case advancePhase:
		w.Kind = phaseName(Advance)
	case startupPhase:
		w.Kind = phaseName(AwaitStartup)
		w.Run = v.run.value
		w.Object = v.object.value
	case waitingPhase:
		w.Kind = phaseName(AwaitWaiting)
		w.Object = v.object.value
	case terminalPhase:
		w.Kind = phaseName(AwaitTerminal)
		w.Run = v.run.value
		w.Object = v.object.value
	case cancelPhase:
		w.Kind = phaseName(Cancel)
		w.Run = v.run.value
		w.Object = v.object.value
	case runTerminalPhase:
		w.Kind = phaseName(AwaitRunTerminal)
		w.Object = v.object.value
	case publishPhase:
		entity, state, brightness, e := publicationInputs(v.state)
		if e != nil {
			return phaseWire{}, e
		}
		w.Kind = phaseName(Publish)
		w.Entity = entity
		w.State = state
		w.Brightness = brightness
	case emitPhase:
		w.Kind = phaseName(Emit)
		w.Entity = "nha045_release"
	case actionsPhase:
		if v.count < 0 || v.count > MaxFacts {
			return phaseWire{}, fmt.Errorf("invalid action phase")
		}
		w.Kind = phaseName(AwaitActions)
		w.Count = v.count
	case nil:
		return phaseWire{}, fmt.Errorf("absent phase operation")
	default:
		return phaseWire{}, fmt.Errorf("unhandled or typed-nil phase operation")
	}
	switch v := p.operation.(type) {
	case invokePhase:
		if v.run.value == "" || v.object.value == "" {
			return phaseWire{}, fmt.Errorf("zero invocation phase")
		}
	case startupPhase:
		if v.run.value == "" || v.object.value == "" {
			return phaseWire{}, fmt.Errorf("zero startup phase")
		}
	case terminalPhase:
		if v.run.value == "" || v.object.value == "" {
			return phaseWire{}, fmt.Errorf("zero terminal phase")
		}
	case cancelPhase:
		if v.run.value == "" || v.object.value == "" {
			return phaseWire{}, fmt.Errorf("zero cancel phase")
		}
	case waitingPhase:
		if v.object.value == "" {
			return phaseWire{}, fmt.Errorf("zero waiting phase")
		}
	case runTerminalPhase:
		if v.object.value == "" {
			return phaseWire{}, fmt.Errorf("zero run-terminal phase")
		}
	}
	return w, nil
}

func publicationInputs(s statePayload) (string, string, *float64, error) {
	if !syntheticEntity(s.entity.value) {
		return "", "", nil, fmt.Errorf("invalid publication identity")
	}
	state := ""
	switch s.state {
	case stateOn:
		state = "on"
	case stateOff:
		state = "off"
	case stateUnknown:
		state = "unknown"
	case stateInvalid:
		return "", "", nil, fmt.Errorf("zero publication state")
	default:
		return "", "", nil, fmt.Errorf("unhandled publication state")
	}
	var brightness *float64
	if s.brightness.present {
		v := s.brightness.value
		if !strings.HasPrefix(s.entity.value, "light.") || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 || s.state == stateUnknown {
			return "", "", nil, fmt.Errorf("brightness outside admitted light profile")
		}
		brightness = ptr(v)
	}
	return s.entity.value, state, brightness, nil
}
