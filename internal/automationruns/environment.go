package automationruns

import (
	"sync"
	"time"

	"github.com/damione1/Homeward/compat/ha/template"
)

type memoryPool struct {
	mu   sync.Mutex
	used int
}

// adjust transfers already reserved charge without a gap in admission.
func (p *memoryPool) adjust(old, next int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old < 0 || next < 0 || old > p.used || next > MaxMemoryBytes-(p.used-old) {
		return false
	}
	p.used = p.used - old + next
	return true
}

type runArena struct {
	mu           sync.Mutex
	pool         *memoryPool
	charge       int
	forks        int
	closed       bool
	environments []*Environment
}

// Environment owns copy-on-fork values, including nested template values.
// A run arena accounts for every live copy and invalidates all forks together.
type Environment struct {
	arena  *runArena
	depth  int
	data   map[string]template.Value
	charge int
	closed bool
}

func validBindingName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func admittedBindings(bindings []template.Binding) error {
	if len(bindings) > MaxBindings {
		return ErrCapacity
	}
	for _, binding := range bindings {
		if !validBindingName(binding.Name) {
			return ErrInvalidPlan
		}
		kind, err := binding.Value.Kind()
		if err != nil || kind == template.KindUndefined {
			return ErrInvalidPlan
		}
	}
	// This admission validates duplicate/reserved names, value depth, nodes and
	// bytes with the existing NHA-043 authority. The time is inert here.
	_, err := template.NewInputs(nil, bindings, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		return ErrInvalidPlan
	}
	return nil
}

func environmentCharge(count int) int { return environmentBase + count*bindingCharge }

// newRunArena consumes a preparation reservation and transfers its charge to
// one run. Trigger payload has already been captured within that reservation.
func newRunArena(pool *memoryPool, bindings []template.Binding, payloadBytes int) (*runArena, *Environment, error) {
	if err := admittedBindings(bindings); err != nil {
		return nil, nil, err
	}
	if payloadBytes < 0 || payloadBytes > MaxTriggerBytes {
		return nil, nil, ErrCapacity
	}
	charge := MaxTriggerBytes + environmentCharge(len(bindings))
	a := &runArena{pool: pool, charge: charge}
	e := &Environment{arena: a, data: make(map[string]template.Value, len(bindings)), charge: environmentCharge(len(bindings))}
	for _, binding := range bindings {
		e.data[binding.Name] = binding.Value.Copy()
	}
	a.environments = []*Environment{e}
	// Keep the full reservation through validation and the retained deep copy.
	// The transfer only releases temporary capacity after both have finished.
	if !pool.adjust(preparationCharge, charge) {
		return nil, nil, ErrCapacity
	}
	return a, e, nil
}

// ownsEnvironment admits the root or a live registered fork of this run's
// arena. Sharing a Controller pool alone never proves run ownership.
func (a *runArena) ownsEnvironment(env *Environment) bool {
	if a == nil || env == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if env.arena != a || a.closed || env.closed {
		return false
	}
	for _, admitted := range a.environments {
		if admitted == env {
			return true
		}
	}
	return false
}

func (a *runArena) close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	charge := a.charge
	for _, env := range a.environments {
		env.data = nil
	}
	a.environments = nil
	a.charge = 0
	a.mu.Unlock()
	a.pool.adjust(charge, 0)
}

func (e *Environment) Get(name string) (template.Value, bool) {
	if e == nil || e.arena == nil {
		return template.Value{}, false
	}
	e.arena.mu.Lock()
	defer e.arena.mu.Unlock()
	if e.arena.closed || e.closed {
		return template.Value{}, false
	}
	v, ok := e.data[name]
	// template.Value is externally immutable; returning its value wrapper does
	// not allocate an unaccounted mutable copy of nested data.
	return v, ok
}

func (e *Environment) Set(name string, value template.Value) error {
	if e == nil || e.arena == nil || !validBindingName(name) {
		return ErrInvalidPlan
	}
	kind, err := value.Kind()
	if err != nil || kind == template.KindUndefined {
		return ErrInvalidPlan
	}
	a := e.arena
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || e.closed {
		return ErrClosed
	}
	_, exists := e.data[name]
	if !exists && len(e.data) >= MaxBindings {
		return ErrCapacity
	}
	// NewInputs validates the complete candidate environment, including
	// reserved names and the aggregate 64 KiB logical input limit. Reserve
	// one full environment for its temporary copy before constructing it.
	persistent := 0
	if !exists {
		persistent = bindingCharge
	}
	temporary := MaxEnvironmentBytes + environmentBase
	if !a.pool.adjust(0, persistent+temporary) {
		return ErrCapacity
	}
	candidate := make([]template.Binding, 0, len(e.data)+1)
	for existingName, existingValue := range e.data {
		if existingName != name {
			candidate = append(candidate, template.Binding{Name: existingName, Value: existingValue})
		}
	}
	candidate = append(candidate, template.Binding{Name: name, Value: value})
	if err := admittedBindings(candidate); err != nil {
		a.pool.adjust(persistent+temporary, 0)
		return err
	}
	e.data[name] = value.Copy()
	a.pool.adjust(temporary, 0)
	if !exists {
		a.charge += bindingCharge
		e.charge += bindingCharge
	}
	return nil
}

func (e *Environment) ForkBranch() (*Environment, error) {
	if e == nil || e.arena == nil {
		return nil, ErrInvalidPlan
	}
	a := e.arena
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || e.closed {
		return nil, ErrClosed
	}
	if e.depth >= MaxBranchDepth || a.forks >= MaxBranchesPerRun {
		return nil, ErrCapacity
	}
	charge := environmentCharge(len(e.data))
	if !a.pool.adjust(0, charge) {
		return nil, ErrCapacity
	}
	child := &Environment{arena: a, depth: e.depth + 1, data: make(map[string]template.Value, len(e.data)), charge: charge}
	for name, value := range e.data {
		child.data[name] = value.Copy()
	}
	a.environments = append(a.environments, child)
	a.forks++
	a.charge += charge
	return child, nil
}

// snapshot reserves both the retained binding copy and NewInputs' temporary
// validation/evaluation copy before constructing either one. The caller keeps
// the lease until synchronous evaluation or wait registration has returned.
func (e *Environment) snapshot() ([]template.Binding, func(), error) {
	if e == nil || e.arena == nil {
		return nil, nil, ErrInvalidPlan
	}
	a := e.arena
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || e.closed {
		return nil, nil, ErrClosed
	}
	const temporary = 2*MaxEnvironmentBytes + 2*environmentBase
	if !a.pool.adjust(0, temporary) {
		return nil, nil, ErrCapacity
	}
	bindings := make([]template.Binding, 0, len(e.data))
	for name, value := range e.data {
		bindings = append(bindings, template.Binding{Name: name, Value: value.Copy()})
	}
	var once sync.Once
	return bindings, func() { once.Do(func() { a.pool.adjust(temporary, 0) }) }, nil
}

// closeBranch follows the join of all tasks using this private branch. The
// root environment remains owned by the controller until actual run completion.
func (e *Environment) closeBranch() {
	if e == nil || e.arena == nil || e.depth == 0 {
		return
	}
	a := e.arena
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || e.closed {
		return
	}
	e.closed = true
	e.data = nil
	for index, candidate := range a.environments {
		if candidate != e {
			continue
		}
		copy(a.environments[index:], a.environments[index+1:])
		a.environments[len(a.environments)-1] = nil
		a.environments = a.environments[:len(a.environments)-1]
		break
	}
	charge := e.charge
	e.charge = 0
	a.charge -= charge
	a.forks--
	a.pool.adjust(charge, 0)
}
