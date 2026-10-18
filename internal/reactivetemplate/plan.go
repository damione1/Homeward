package reactivetemplate

import (
	"errors"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/sequencer"
)

var ErrInvalidPlan = errors.New("reactive template: invalid plan")
var ErrOutputFull = errors.New("reactive template: output queue full")
var ErrClosed = errors.New("reactive template: service closed")
var ErrNotConverged = errors.New("reactive template: dependency cycle did not converge")
var ErrWorkBudget = errors.New("reactive template: wake work budget exceeded")

const (
	MaxPrograms    = 64
	MaxWorkPerWake = 262144
)

type ProgramID struct{ value string }

func ParseProgramID(value string) (ProgramID, error) {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return ProgramID{}, ErrInvalidPlan
	}
	return ProgramID{value: value}, nil
}
func (id ProgramID) Value() (string, error) {
	if _, err := ParseProgramID(id.value); err != nil {
		return "", err
	}
	return id.value, nil
}

type Definition struct {
	id        ProgramID
	program   template.Program
	static    []template.Dependency
	variables []template.Binding
	target    domain.EntityID
	hasTarget bool
}

func NewDefinition(id ProgramID, program template.Program, variables []template.Binding, target *domain.EntityID) (Definition, error) {
	if _, err := id.Value(); err != nil || !program.Valid() {
		return Definition{}, ErrInvalidPlan
	}
	if _, err := template.NewInputs(nil, variables, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); err != nil {
		return Definition{}, err
	}
	definition := Definition{id: id, program: program, static: program.StaticDependencies(), variables: make([]template.Binding, len(variables))}
	for i, variable := range variables {
		definition.variables[i] = template.Binding{Name: variable.Name, Value: variable.Value.Copy()}
	}
	if target != nil {
		if _, err := target.Value(); err != nil {
			return Definition{}, ErrInvalidPlan
		}
		definition.target, definition.hasTarget = *target, true
	}
	return definition, nil
}

func (d Definition) ID() ProgramID                   { return d.id }
func (d Definition) Target() (domain.EntityID, bool) { return d.target, d.hasTarget }

type Plan struct {
	definitions []Definition
	groups      []bundle.Group
	zone        *time.Location
}

func NewPlan(definitions []Definition, groups []bundle.Group, zone *time.Location) (Plan, error) {
	if len(definitions) > MaxPrograms || len(groups) > 128 || (len(definitions) != 0 && zone == nil) {
		return Plan{}, ErrInvalidPlan
	}
	seen := make(map[ProgramID]struct{}, len(definitions))
	targets := make(map[domain.EntityID]struct{}, len(definitions))
	copyDefinitions := make([]Definition, len(definitions))
	for i, definition := range definitions {
		if _, err := definition.id.Value(); err != nil || !definition.program.Valid() {
			return Plan{}, ErrInvalidPlan
		}
		if _, exists := seen[definition.id]; exists {
			return Plan{}, ErrInvalidPlan
		}
		seen[definition.id] = struct{}{}
		copyDefinitions[i] = definition
		copyDefinitions[i].static = append([]template.Dependency(nil), definition.static...)
		copyDefinitions[i].variables = make([]template.Binding, len(definition.variables))
		for j, variable := range definition.variables {
			copyDefinitions[i].variables[j] = template.Binding{Name: variable.Name, Value: variable.Value.Copy()}
		}
		if definition.hasTarget {
			if _, exists := targets[definition.target]; exists {
				return Plan{}, ErrInvalidPlan
			}
			targets[definition.target] = struct{}{}
		}
	}
	copyGroups := make([]bundle.Group, len(groups))
	for i, group := range groups {
		if !group.Valid() {
			return Plan{}, ErrInvalidPlan
		}
		copyGroups[i] = group
	}
	return Plan{definitions: copyDefinitions, groups: copyGroups, zone: zone}, nil
}

func (p Plan) Active() bool { return len(p.definitions) > 0 }

type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type RealClock struct{}

func (RealClock) Now() time.Time                 { return time.Now() }
func (RealClock) NewTimer(d time.Duration) Timer { return realTimer{timer: time.NewTimer(d)} }

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool          { return t.timer.Stop() }

type Diagnostic uint8

const (
	DiagnosticUnspecified Diagnostic = iota
	DiagnosticHealthy
	DiagnosticEvaluation
	DiagnosticLoss
	DiagnosticOscillation
	DiagnosticWorkBudget
)

// Update is a completed fixed-point publication or an explicit degraded
// state. Result is absent until a full render succeeds.
type Update struct {
	id         ProgramID
	result     template.Result
	valid      bool
	quality    domain.Quality
	diagnostic Diagnostic
	cursor     sequencer.Cursor
}

func (u Update) ID() ProgramID { return u.id }

// DisplayResult may hold the last successful value after a loss or failure.
func (u Update) DisplayResult() (template.Result, bool) { return u.result, u.valid }

// CurrentResult admits a calculated value for current-value consumers only
// while its inputs remain available and fresh. Its provenance is Estimated.
func (u Update) CurrentResult() (template.Result, bool) {
	if !u.valid || u.diagnostic != DiagnosticHealthy {
		return template.Result{}, false
	}
	availability, freshness, provenance, err := u.quality.Components()
	if err != nil || availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh || provenance != domain.ProvenanceEstimated {
		return template.Result{}, false
	}
	return u.result, true
}
func (u Update) Quality() domain.Quality  { return u.quality }
func (u Update) Diagnostic() Diagnostic   { return u.diagnostic }
func (u Update) Cursor() sequencer.Cursor { return u.cursor }
func (u Update) Equal(other Update) bool {
	return u.id == other.id && u.valid == other.valid && u.quality == other.quality && u.diagnostic == other.diagnostic && (!u.valid || u.result.Equal(other.result))
}

func nilClock(clock Clock) bool {
	return nilReactiveValue(clock)
}

// Interfaces can carry a nil pointer while comparing unequal to nil. Check
// that boundary before invoking a context, clock, or output method.
func nilReactiveValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
