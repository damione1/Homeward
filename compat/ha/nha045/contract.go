// Package nha045 owns the bounded, synthetic differential qualification contract.
// It does not expose a production execution or history API.
package nha045

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const Schema = "nha045/v1"
const (
	MaxInputBytes   = 1 << 20
	MaxObjects      = 16
	MaxEntities     = 32
	MaxPhases       = 128
	MaxRuns         = 64
	MaxFacts        = 1024
	MaxEdges        = 4096
	MaxPayloadBytes = 8 << 10
	MaxCaptureBytes = 8 << 20
)

type IssueKind uint8

const (
	IssueInvalid IssueKind = iota
	InvalidFixture
	InvalidObservation
	InfrastructureUnavailable
	VersionMismatch
	HarnessTimeout
	CaptureOverflow
	CleanupFailed
	SemanticDivergence
)

var issueNames = [...]string{"", "invalid_fixture", "invalid_observation", "infrastructure_unavailable", "version_mismatch", "harness_timeout", "capture_overflow", "cleanup_failed", "semantic_divergence"}

type Issue struct {
	kind   IssueKind
	detail string
}

func (e *Issue) Kind() IssueKind {
	if e == nil {
		return IssueInvalid
	}
	return e.kind
}
func (e *Issue) Error() string {
	if e == nil || e.kind <= IssueInvalid || int(e.kind) >= len(issueNames) {
		return "nha045: invalid issue"
	}
	return "nha045: " + issueNames[e.kind] + ": " + e.detail
}
func fail(kind IssueKind, detail string) error { return &Issue{kind, detail} }

type FactKind uint8

const (
	FactInvalid FactKind = iota
	Admission
	Started
	Action
	State
	Wait
	Terminal
	Cancellation
)

var factVariants = [...]variant{{}, {"Admission", "admission"}, {"Started", "start"}, {"Action", "action"}, {"State", "state"}, {"Wait", "wait"}, {"Terminal", "terminal"}, {"Cancellation", "cancel"}}

func (k FactKind) token() (string, error) {
	if k <= FactInvalid || int(k) >= len(factVariants) {
		return "", fail(InvalidObservation, "invalid fact kind")
	}
	return factVariants[k].Token, nil
}
func factName(k FactKind) string { token, _ := k.token(); return token }
func parseFactKind(s string) (FactKind, error) {
	for k := Admission; k <= Cancellation; k++ {
		if factVariants[k].Token == s {
			return k, nil
		}
	}
	return FactInvalid, fail(InvalidObservation, "unknown fact kind: "+s)
}

type PhaseKind uint8

const (
	PhaseInvalid PhaseKind = iota
	Invoke
	Advance
	AwaitStartup
	AwaitWaiting
	AwaitTerminal
	Cancel
	Publish
	Emit
	AwaitActions
	AwaitRunTerminal
)

var phaseVariants = [...]variant{{}, {"Invoke", "invoke"}, {"Advance", "advance"}, {"AwaitStartup", "await_startup"}, {"AwaitWaiting", "await_waiting"}, {"AwaitTerminal", "await_terminal"}, {"Cancel", "cancel"}, {"Publish", "publish"}, {"Emit", "emit"}, {"AwaitActions", "await_actions"}, {"AwaitRunTerminal", "await_run_terminal"}}

func phaseName(k PhaseKind) string {
	if k <= PhaseInvalid || int(k) >= len(phaseVariants) {
		return ""
	}
	return phaseVariants[k].Token
}
func parsePhaseKind(s string) (PhaseKind, error) {
	for k := Invoke; k <= AwaitRunTerminal; k++ {
		if phaseVariants[k].Token == s {
			return k, nil
		}
	}
	return PhaseInvalid, fail(InvalidFixture, "unknown phase kind: "+s)
}

// These tokens are emitted from this authority to the HA worker. Python never
// maintains a second handwritten semantic vocabulary.
type Outcome uint8

const (
	OutcomeInvalid Outcome = iota
	Accepted
	Queued
	SingleBusy
	MaxExceeded
	Finished
	Cancelled
	TimeoutStopped
	CommandFailed
	Completed
)

var outcomeVariants = [...]variant{{}, {"Accepted", "accepted"}, {"Queued", "queued"}, {"SingleBusy", "single_busy"}, {"MaxExceeded", "max_exceeded"}, {"Finished", "finished"}, {"Cancelled", "cancelled"}, {"TimeoutStopped", "timeout_stopped"}, {"CommandFailed", "command_failed"}, {"Completed", "completed"}}

func (o Outcome) token() (string, error) {
	if o <= OutcomeInvalid || int(o) >= len(outcomeVariants) {
		return "", fail(InvalidObservation, "invalid outcome")
	}
	return outcomeVariants[o].Token, nil
}
func outcomeName(o Outcome) string { token, _ := o.token(); return token }
func parseOutcome(s string) (Outcome, error) {
	for o := Accepted; o <= Completed; o++ {
		if outcomeVariants[o].Token == s {
			return o, nil
		}
	}
	return OutcomeInvalid, fail(InvalidObservation, "unknown outcome: "+s)
}

type identifier struct{ value string }

func parseID(s string) (identifier, error) {
	if s == "" || len(s) > 256 || strings.TrimSpace(s) != s {
		return identifier{}, errors.New("empty or oversized identity")
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return identifier{}, errors.New("non-ASCII identity")
		}
	}
	return identifier{s}, nil
}

type optionalID struct {
	id      identifier
	present bool
}

func optional(s *string) (optionalID, error) {
	if s == nil {
		return optionalID{}, nil
	}
	id, e := parseID(*s)
	return optionalID{id, e == nil}, e
}
func (v optionalID) wire() *string {
	if !v.present {
		return nil
	}
	s := v.id.value
	return &s
}

// strictJSON rejects duplicate keys as well as unknown fields. A last-wins
// decoder must not make two contradictory fixture or observation fields agree.
func strictJSON(data []byte, dst any, limit int) error {
	if len(data) == 0 || len(data) > limit {
		return errors.New("JSON size outside bound")
	}
	scan := json.NewDecoder(bytes.NewReader(data))
	scan.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON depth exceeded")
		}
		tok, err := scan.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := make(map[string]struct{}) // boundary-only key admission, not domain data
			for scan.More() {
				key, e := scan.Token()
				if e != nil {
					return e
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("invalid object key")
				}
				if _, ok := seen[name]; ok {
					return errors.New("duplicate JSON key: " + name)
				}
				seen[name] = struct{}{}
				if e = walk(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for scan.More() {
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected closing delimiter")
		}
		_, err = scan.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := scan.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func requireFields(data []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("object required")
	}
	for _, name := range names {
		if _, ok := fields[name]; !ok {
			return errors.New("required field absent: " + name)
		}
	}
	return nil
}

type variant struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}
type manifest struct {
	Schema    string       `json:"schema"`
	Facts     []variant    `json:"facts"`
	Phases    []variant    `json:"phases"`
	Outcomes  []variant    `json:"outcomes"`
	Issues    []variant    `json:"issues"`
	Sources   []sourceWire `json:"sources"`
	Image     string       `json:"image"`
	HAVersion string       `json:"ha_version"`
	Python    string       `json:"python"`
	Jinja     string       `json:"jinja"`
}

func Manifest() ([]byte, error) {
	m := manifest{Schema: Schema, Sources: append([]sourceWire(nil), haSources...), Image: Image, HAVersion: HAVersion, Python: HAPython, Jinja: HAJinja}
	for k := Admission; k <= Cancellation; k++ {
		m.Facts = append(m.Facts, factVariants[k])
	}
	for k := Invoke; k <= AwaitRunTerminal; k++ {
		m.Phases = append(m.Phases, phaseVariants[k])
	}
	for k := Accepted; k <= Completed; k++ {
		m.Outcomes = append(m.Outcomes, outcomeVariants[k])
	}
	for k := InvalidFixture; k <= SemanticDivergence; k++ {
		m.Issues = append(m.Issues, variant{fmt.Sprint(uint8(k)), issueNames[k]})
	}
	return json.Marshal(m)
}
