package nha045

import "fmt"

// Open identity domains are distinct even where their lexical admission is
// shared. In particular a raw runtime ID cannot substitute for a logical run.
type scenarioID struct{ identifier }
type runKey struct{ identifier }
type rawRunID struct{ identifier }
type nodeKey struct{ identifier }
type factKey struct{ identifier }
type objectID struct{ identifier }
type entityID struct{ identifier }
type callsiteID struct{ identifier }
type optionalRunKey struct {
	value   runKey
	present bool
}
type optionalRawRunID struct {
	value   rawRunID
	present bool
}
type optionalCallsiteID struct {
	value   callsiteID
	present bool
}
type optionalNumber struct {
	value   float64
	present bool
}
type optionalDuration struct {
	milliseconds int64
	present      bool
}
type triggerProjection struct {
	id       string
	index    int
	platform string
}
type optionalTrigger struct {
	value   triggerProjection
	present bool
}
type stateKind uint8

const (
	stateInvalid stateKind = iota
	stateOn
	stateOff
	stateUnknown
)

// Only these value variants implement the private marker. No wire pointers,
// nullable union or arbitrary JSON map survives domain admission.
type admittedPayload interface {
	isPayload()
	payloadKind() FactKind
}
type admissionPayload struct{ outcome Outcome }
type startedPayload struct{}
type actionPayload struct {
	entity  entityID
	outcome Outcome
}
type statePayload struct {
	entity     entityID
	state      stateKind
	brightness optionalNumber
}
type waitPayload struct {
	outcome   Outcome
	completed bool
	remaining optionalDuration
	trigger   optionalTrigger
}
type terminalPayload struct{ outcome Outcome }
type cancellationPayload struct{}

func (admissionPayload) isPayload()               {}
func (startedPayload) isPayload()                 {}
func (actionPayload) isPayload()                  {}
func (statePayload) isPayload()                   {}
func (waitPayload) isPayload()                    {}
func (terminalPayload) isPayload()                {}
func (cancellationPayload) isPayload()            {}
func (admissionPayload) payloadKind() FactKind    { return Admission }
func (startedPayload) payloadKind() FactKind      { return Started }
func (actionPayload) payloadKind() FactKind       { return Action }
func (statePayload) payloadKind() FactKind        { return State }
func (waitPayload) payloadKind() FactKind         { return Wait }
func (terminalPayload) payloadKind() FactKind     { return Terminal }
func (cancellationPayload) payloadKind() FactKind { return Cancellation }
func admitPayload(kind FactKind, d dataWire) (admittedPayload, error) {
	if err := validateData(kind, d); err != nil {
		return nil, err
	}
	outcome := OutcomeInvalid
	if d.Outcome != nil {
		outcome, _ = parseOutcome(*d.Outcome)
	}
	switch kind {
	case Admission:
		return admissionPayload{outcome}, nil
	case Started:
		return startedPayload{}, nil
	case Cancellation:
		return cancellationPayload{}, nil
	case Terminal:
		return terminalPayload{outcome}, nil
	case Action:
		id, e := parseID(*d.Entity)
		if e != nil {
			return nil, e
		}
		return actionPayload{entityID{id}, outcome}, nil
	case State:
		id, e := parseID(*d.Entity)
		if e != nil {
			return nil, e
		}
		state := stateInvalid
		switch *d.State {
		case "on":
			state = stateOn
		case "off":
			state = stateOff
		case "unknown":
			state = stateUnknown
		default:
			return nil, fmt.Errorf("unknown state")
		}
		brightness := optionalNumber{}
		if d.Brightness != nil {
			brightness = optionalNumber{*d.Brightness, true}
		}
		return statePayload{entityID{id}, state, brightness}, nil
	case Wait:
		remaining := optionalDuration{}
		if d.Wait.RemainingMS != nil {
			remaining = optionalDuration{*d.Wait.RemainingMS, true}
		}
		trigger := optionalTrigger{}
		if v := d.Wait.Trigger; v != nil {
			trigger = optionalTrigger{triggerProjection{v.ID, v.Index, v.Platform}, true}
		}
		return waitPayload{outcome, *d.Wait.Completed, remaining, trigger}, nil
	case FactInvalid:
		return nil, fmt.Errorf("zero payload kind")
	default:
		return nil, fmt.Errorf("unhandled payload kind")
	}
}
func payloadWire(p admittedPayload) (dataWire, error) {
	data, e := payloadBoundary(p)
	if e != nil {
		return dataWire{}, e
	}
	if e = validateData(p.payloadKind(), data); e != nil {
		return dataWire{}, e
	}
	return data, nil
}
func payloadBoundary(p admittedPayload) (dataWire, error) {
	switch v := p.(type) {
	case admissionPayload:
		if v.outcome != Accepted && v.outcome != Queued && v.outcome != SingleBusy && v.outcome != MaxExceeded {
			return dataWire{}, fmt.Errorf("invalid admission payload")
		}
		return dataWire{Outcome: ptr(outcomeName(v.outcome))}, nil
	case startedPayload:
		return dataWire{}, nil
	case cancellationPayload:
		return dataWire{}, nil
	case terminalPayload:
		if v.outcome != Finished && v.outcome != Cancelled && v.outcome != TimeoutStopped && v.outcome != CommandFailed {
			return dataWire{}, fmt.Errorf("invalid terminal payload")
		}
		return dataWire{Outcome: ptr(outcomeName(v.outcome))}, nil
	case actionPayload:
		return dataWire{Outcome: ptr(outcomeName(v.outcome)), Entity: ptr(v.entity.value), Service: ptr("switch.turn_on")}, nil
	case statePayload:
		state := ""
		switch v.state {
		case stateOn:
			state = "on"
		case stateOff:
			state = "off"
		case stateUnknown:
			state = "unknown"
		case stateInvalid:
			return dataWire{}, fmt.Errorf("zero state payload")
		default:
			return dataWire{}, fmt.Errorf("unhandled state payload")
		}
		d := dataWire{Entity: ptr(v.entity.value), State: ptr(state)}
		if v.brightness.present {
			d.Brightness = ptr(v.brightness.value)
		}
		return d, nil
	case waitPayload:
		d := waitWire{Completed: ptr(v.completed)}
		if v.remaining.present {
			d.RemainingMS = ptr(v.remaining.milliseconds)
		}
		if v.trigger.present {
			d.Trigger = &triggerWire{v.trigger.value.id, v.trigger.value.index, v.trigger.value.platform}
		}
		return dataWire{Outcome: ptr(outcomeName(v.outcome)), Wait: &d}, nil
	case nil:
		return dataWire{}, fmt.Errorf("absent payload")
	default:
		return dataWire{}, fmt.Errorf("unhandled or typed-nil payload")
	}
}
