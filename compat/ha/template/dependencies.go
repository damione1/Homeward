package template

import (
	"context"
	"sort"
	"strings"

	"github.com/damione1/Homeward/internal/domain"
)

// DependencyKind is the closed set of observable template reads. A zero kind
// cannot enter an index. Alias identifiers themselves remain an open set.
type DependencyKind uint8

const (
	DependencyUnspecified DependencyKind = iota
	DependencyState
	DependencyAttribute
	DependencyMembership
	DependencyDomainCatalog
	DependencyMinute
)

// DomainName is an open, admitted HA domain identity. There is deliberately no
// language construct in the current compiler that reads a domain catalogue.
type DomainName struct{ value string }

func ParseDomainName(value string) (DomainName, error) {
	if value == "" || len(value) > 128 {
		return DomainName{}, ErrInvalidInput
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
			return DomainName{}, ErrInvalidInput
		}
	}
	return DomainName{value: value}, nil
}
func (d DomainName) Value() (string, error) {
	if d.value == "" {
		return "", ErrInvalidInput
	}
	return d.value, nil
}

type Dependency struct {
	kind      DependencyKind
	alias     domain.HAAlias
	hasAlias  bool
	domain    DomainName
	hasDomain bool
	attribute string
}

func (d Dependency) Kind() DependencyKind { return d.kind }
func (d Dependency) Valid() bool {
	switch d.kind {
	case DependencyState, DependencyMembership:
		_, err := d.alias.Value()
		return d.hasAlias && !d.hasDomain && d.attribute == "" && err == nil
	case DependencyAttribute:
		_, err := d.alias.Value()
		return d.hasAlias && !d.hasDomain && err == nil && validKey(d.attribute) == nil
	case DependencyDomainCatalog:
		_, err := d.domain.Value()
		return !d.hasAlias && d.hasDomain && d.attribute == "" && err == nil
	case DependencyMinute:
		return !d.hasAlias && !d.hasDomain && d.attribute == ""
	default:
		return false
	}
}
func (d Dependency) Alias() (domain.HAAlias, bool) {
	return d.alias, d.hasAlias
}
func (d Dependency) Domain() (DomainName, bool) { return d.domain, d.hasDomain }
func (d Dependency) Attribute() string          { return d.attribute }

func newAliasDependency(kind DependencyKind, raw, attribute string) (Dependency, error) {
	if kind != DependencyState && kind != DependencyAttribute && kind != DependencyMembership {
		return Dependency{}, ErrInvalidInput
	}
	alias, err := domain.ParseHAAlias(raw)
	if err != nil || (kind == DependencyAttribute && validKey(attribute) != nil) || (kind != DependencyAttribute && attribute != "") {
		return Dependency{}, ErrInvalidInput
	}
	return Dependency{kind: kind, alias: alias, hasAlias: true, attribute: attribute}, nil
}

// NewDomainCatalogDependency admits a future catalogue source without
// extending the expression language or treating arbitrary strings as IDs.
func NewDomainCatalogDependency(name DomainName) (Dependency, error) {
	if _, err := name.Value(); err != nil {
		return Dependency{}, err
	}
	return Dependency{kind: DependencyDomainCatalog, domain: name, hasDomain: true}, nil
}

// Read records the source quality at the instant an executed expression asked
// for it. HasIdentity is false for an absent alias. Present is false for a
// known but unavailable source; neither case is a fabricated zero value.
type Read struct {
	dependency  Dependency
	entity      domain.EntityID
	quality     domain.Quality
	hasIdentity bool
	hasQuality  bool
	present     bool
}

func (r Read) Dependency() Dependency          { return r.dependency }
func (r Read) Entity() (domain.EntityID, bool) { return r.entity, r.hasIdentity }
func (r Read) Quality() (domain.Quality, bool) { return r.quality, r.hasQuality }
func (r Read) Present() bool                   { return r.present }

// TrackedResult preserves a partial trace after a failed render. A result is
// admitted only after the complete render succeeds.
type TrackedResult struct {
	result Result
	valid  bool
	deps   []Dependency
	reads  []Read
	steps  int
}

func (r TrackedResult) Result() (Result, bool) { return r.result, r.valid }
func (r TrackedResult) Dependencies() []Dependency {
	return append([]Dependency(nil), r.deps...)
}
func (r TrackedResult) Reads() []Read { return append([]Read(nil), r.reads...) }
func (r TrackedResult) Steps() int    { return r.steps }

type traceRecorder struct {
	deps  map[Dependency]struct{}
	reads []Read
}

func (e *evaluator) record(dep Dependency) error {
	if e.trace == nil {
		return nil
	}
	if !dep.Valid() {
		return ErrInvalidInput
	}
	if err := e.charge(1); err != nil {
		return err
	}
	if _, exists := e.trace.deps[dep]; !exists && len(e.trace.deps) >= maxItems {
		return ErrBudget
	}
	e.trace.deps[dep] = struct{}{}
	return nil
}

func (e *evaluator) read(alias string, kind DependencyKind, attribute string) (State, bool, error) {
	dep, err := newAliasDependency(kind, alias, attribute)
	if err != nil {
		return State{}, false, err
	}
	if err := e.record(dep); err != nil {
		return State{}, false, err
	}
	if e.in.view == nil {
		state, ok := e.in.states[alias]
		if e.trace != nil {
			e.trace.reads = append(e.trace.reads, Read{dependency: dep, present: ok})
		}
		return state, ok, nil
	}
	source, ok := e.in.view.lookup(alias)
	if e.trace != nil {
		e.trace.reads = append(e.trace.reads, Read{dependency: dep, entity: source.entity, quality: source.quality, hasIdentity: ok, hasQuality: ok && source.hasQuality, present: ok && source.present})
		if len(e.trace.reads) > maxItems {
			return State{}, false, ErrBudget
		}
	}
	return source.state, ok, nil
}

func EvaluateTracked(ctx context.Context, program Program, input Inputs, limits Limits) (TrackedResult, error) {
	if nilInterface(ctx) || len(program.segments) == 0 || limits.steps < 1 || limits.steps > defaultSteps || limits.output < 1 || limits.output > defaultOutput || input.zone == nil || input.now.IsZero() {
		return TrackedResult{}, ErrInvalidInput
	}
	recorder := &traceRecorder{deps: make(map[Dependency]struct{})}
	e := evaluator{ctx: ctx, in: input, steps: limits.steps, output: limits.output, trace: recorder}
	result, err := e.evaluate(program)
	out := TrackedResult{valid: err == nil, result: result, steps: limits.steps - e.steps, reads: append([]Read(nil), recorder.reads...)}
	for dep := range recorder.deps {
		out.deps = append(out.deps, dep)
	}
	sortDependencies(out.deps)
	return out, err
}

// StaticDependencies visits the admitted IR itself. They are recovery hints,
// never evidence that a skipped branch executed or that now needs a timer.
func (p Program) StaticDependencies() []Dependency {
	seen := make(map[Dependency]struct{})
	var visit func(expression)
	visit = func(x expression) {
		if x.op == opCall {
			switch x.builtin {
			case builtinNow:
				seen[Dependency{kind: DependencyMinute}] = struct{}{}
			case builtinStates, builtinStateAttr, builtinIsState, builtinExpand:
				if len(x.children) != 0 && x.children[0].op == opLiteral && x.children[0].literal.kind == valueString {
					if alias, err := domain.ParseHAAlias(x.children[0].literal.s); err == nil {
						name, _ := alias.Value()
						dep := Dependency{kind: DependencyState, alias: alias, hasAlias: true}
						if x.builtin == builtinExpand && len(name) >= 6 && name[:6] == "group." {
							dep.kind = DependencyMembership
						}
						if x.builtin == builtinStateAttr && len(x.children) > 1 && x.children[1].op == opLiteral && x.children[1].literal.kind == valueString {
							dep.kind, dep.attribute = DependencyAttribute, x.children[1].literal.s
						}
						seen[dep] = struct{}{}
					}
				}
			}
		}
		for _, child := range x.children {
			visit(child)
		}
	}
	for _, segment := range p.segments {
		if segment.expr != nil {
			visit(*segment.expr)
		}
	}
	out := make([]Dependency, 0, len(seen))
	for dep := range seen {
		out = append(out, dep)
	}
	sortDependencies(out)
	return out
}

func sortDependencies(deps []Dependency) {
	sort.Slice(deps, func(i, j int) bool {
		a, b := deps[i], deps[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.alias != b.alias {
			an, _ := a.alias.Value()
			bn, _ := b.alias.Value()
			return an < bn
		}
		if a.domain != b.domain {
			return strings.Compare(a.domain.value, b.domain.value) < 0
		}
		return a.attribute < b.attribute
	})
}
