// Package configsimulation executes admitted HA logic with fixture observations
// and a command recorder. No driver, network client, durable command authority,
// or production Host can be supplied to this package.
package configsimulation

import (
	"errors"
	"time"

	"github.com/damione1/Homeward/compat/ha/ir"
	hascene "github.com/damione1/Homeward/compat/ha/scene"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/automationruns"
	"github.com/damione1/Homeward/internal/configplans"
	"github.com/damione1/Homeward/internal/domain"
)

var ErrInvalid = errors.New("configuration simulation: invalid fixture")
var errRefused = errors.New("configuration simulation: fixture refused intent")

const MaxFixtures = 128
const MaxIntents = 256
const MaxTraces = 1024
const MaxVirtualDuration = time.Hour

type Status uint8

const (
	StatusInvalid Status = iota
	Completed
	Partial
	Failed
)

type DiagnosticCode uint8

const (
	DiagnosticInvalid DiagnosticCode = iota
	UnsupportedExtension
	UnsupportedConcurrency
	UnsupportedWait
	InvalidSource
	MissingFixture
	FixtureRefused
	ExecutionFailed
	CapacityExceeded
)

type Position struct {
	File, Path   string
	Line, Column int
}
type Diagnostic struct {
	Code     DiagnosticCode
	Position Position
}
type IntentOutcome uint8

const (
	IntentOutcomeInvalid IntentOutcome = iota
	Recorded
	Refused
)

// Intent retains the existing authoritative IR call or scene member. It never
// claims dispatch, acknowledgement, or observed device success.
type Intent struct {
	call     ir.Call
	member   hascene.Member
	position Position
	alias    domain.HAAlias
	outcome  IntentOutcome
	at       time.Time
}

func (i Intent) Call() ir.Call               { return i.call }
func (i Intent) SceneMember() hascene.Member { return i.member }
func (i Intent) Position() Position          { return i.position }
func (i Intent) Alias() domain.HAAlias       { return i.alias }
func (i Intent) Outcome() IntentOutcome      { return i.outcome }
func (i Intent) At() time.Time               { return i.at }

type Fixture struct {
	entity   ir.Entity
	state    template.State
	read     template.ReadSource
	observed bool
	refuse   bool
}

// An absent observation differs from an explicitly supplied "unknown" state.
func NewFixture(alias string, state *string, attributes []template.Binding, supports []ir.DeclaredSupport, refuse bool) (Fixture, error) {
	entity, err := ir.NewEntity(alias, ir.ReferencePresent, supports...)
	if err != nil {
		return Fixture{}, ErrInvalid
	}
	result := Fixture{entity: entity, observed: state != nil, refuse: refuse}
	id, err := domain.ParseEntityID("simulation-fixture/" + alias)
	if err != nil {
		return Fixture{}, ErrInvalid
	}
	if state == nil {
		if len(attributes) != 0 {
			return Fixture{}, ErrInvalid
		}
		result.read, err = template.NewUnobservedReadSource(id, []domain.HAAlias{entity.Alias()})
	} else {
		result.state, err = template.NewState(entity.Alias(), *state, attributes)
		if err != nil {
			return Fixture{}, ErrInvalid
		}
		availability := domain.AvailabilityAvailable
		text := state
		if *state == "unknown" {
			availability, text = domain.AvailabilityUnknown, nil
		}
		if *state == "unavailable" {
			availability, text = domain.AvailabilityUnavailable, nil
		}
		quality, e := domain.NewQuality(availability, domain.FreshnessFresh, domain.ProvenanceObserved)
		if e != nil {
			return Fixture{}, ErrInvalid
		}
		result.read, err = template.NewReadSource(id, []domain.HAAlias{entity.Alias()}, text, attributes, quality)
	}
	if err != nil {
		return Fixture{}, ErrInvalid
	}
	return result, nil
}

type Snapshot struct {
	fixtures  []Fixture
	catalogue ir.Catalogue
	states    []template.State
	view      template.ReadView
	valid     bool
}

func NewSnapshot(fixtures []Fixture) (Snapshot, error) {
	return NewSnapshotWithEvents(fixtures, nil)
}
func NewSnapshotWithEvents(fixtures []Fixture, events []ir.Event) (Snapshot, error) {
	if len(fixtures) > MaxFixtures || len(events) > MaxFixtures {
		return Snapshot{}, ErrInvalid
	}
	entities := make([]ir.Entity, 0, len(fixtures))
	states := make([]template.State, 0, len(fixtures))
	reads := make([]template.ReadSource, 0, len(fixtures))
	seen := map[domain.HAAlias]bool{}
	for _, fixture := range fixtures {
		if _, err := fixture.entity.Alias().Value(); err != nil || seen[fixture.entity.Alias()] {
			return Snapshot{}, ErrInvalid
		}
		seen[fixture.entity.Alias()] = true
		entities = append(entities, fixture.entity)
		reads = append(reads, fixture.read)
		if fixture.observed {
			states = append(states, fixture.state)
		}
	}
	catalogue, err := ir.NewCatalogue(entities, nil, events)
	if err != nil {
		return Snapshot{}, ErrInvalid
	}
	view, err := template.NewReadView(reads, nil)
	if err != nil {
		return Snapshot{}, ErrInvalid
	}
	return Snapshot{fixtures: append([]Fixture(nil), fixtures...), catalogue: catalogue, states: states, view: view, valid: true}, nil
}

type Input struct {
	Candidate    configplans.Candidate
	Snapshot     Snapshot
	Now          time.Time
	Zone         *time.Location
	VirtualLimit time.Duration
}

// NodeTrace retains semantic execution evidence without process-global run or
// generation identifiers. Identical fixtures therefore produce identical
// reports even when other controllers have run in the same process.
type NodeTrace struct {
	source               ir.Source
	kind                 ir.ActionKind
	stage                automationruns.NodeStage
	at                   time.Time
	branches, iterations []int
	wait                 automationruns.WaitExit
}

func (n NodeTrace) Source() ir.Source                 { return n.source }
func (n NodeTrace) Kind() ir.ActionKind               { return n.kind }
func (n NodeTrace) Stage() automationruns.NodeStage   { return n.stage }
func (n NodeTrace) At() time.Time                     { return n.at }
func (n NodeTrace) Branches() []int                   { return append([]int(nil), n.branches...) }
func (n NodeTrace) Iterations() []int                 { return append([]int(nil), n.iterations...) }
func (n NodeTrace) WaitExit() automationruns.WaitExit { return n.wait }

type Report struct {
	status      Status
	intents     []Intent
	diagnostics []Diagnostic
	nodes       []NodeTrace
	elapsed     time.Duration
}

func (r Report) Status() Status { return r.status }
func (r Report) Simulated() bool {
	switch r.status {
	case Completed, Partial, Failed:
		return true
	case StatusInvalid:
		return false
	default:
		return false
	}
}
func (Report) HardwareValidated() bool     { return false }
func (r Report) Intents() []Intent         { return append([]Intent(nil), r.intents...) }
func (r Report) Diagnostics() []Diagnostic { return append([]Diagnostic(nil), r.diagnostics...) }
func (r Report) Nodes() []NodeTrace {
	return append([]NodeTrace(nil), r.nodes...)
}
func (r Report) Elapsed() time.Duration { return r.elapsed }
func (Report) Limitations() []string {
	return []string{"Fixture observations and virtual time only; no hardware validation.", "Intents are recorded, never dispatched; physical routing, permissions, availability and device results are unverified.", "Parallel actions, script calls, template waits, scene calls, and external extensions are refused before execution.", "Automations require one time trigger; only its first occurrence is exercised. Event waits exercise fixture timeouts only.", "Traces are bounded and cover visited branches only; other branches, future observations and recurrence remain unverified."}
}
func sourcePosition(source ir.Source) Position {
	at := source.At()
	return Position{File: at.File, Path: source.Path(), Line: at.Line, Column: at.Column}
}
