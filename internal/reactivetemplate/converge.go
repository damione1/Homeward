package reactivetemplate

import (
	"errors"
	"time"
	"unsafe"

	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
)

var errSourcePending = errors.New("reactive template: newer source entry pending")

func (e *engine) checkControl() error {
	if err := e.service.ctx.Err(); err != nil {
		return err
	}
	if e.rebasing || e.sub == nil {
		return nil
	}
	status, err := e.sub.Status()
	if err != nil {
		return err
	}
	if status.PendingLoss != nil || !status.ReplayValid || status.Queued != 0 {
		return errSourcePending
	}
	return nil
}

func (e *engine) recompute() ([]Update, error) {
	return e.recomputeAt(e.service.clock.Now())
}

// Every program and SCC round in one fixed point observes the same clock
// sample. now() retains its seconds and microseconds; the minute timer only
// decides when another fixed point is due.
func (e *engine) recomputeAt(now time.Time) ([]Update, error) {
	base, err := ReadViewFromSnapshot(e.snapshot, e.service.plan.groups)
	if err != nil {
		return nil, err
	}
	definitions := e.service.plan.definitions
	values := make(map[ProgramID]candidate, len(definitions))
	for _, definition := range definitions {
		if old, ok := e.outputs[definition.id]; ok {
			values[definition.id] = candidate{result: old.result, valid: old.valid, quality: old.quality, reads: append([]template.Read(nil), old.reads...), deps: append([]template.Dependency(nil), e.readers[definition.id]...), diagnostic: old.diagnostic}
		}
	}
	maxWork := MaxWorkPerWake
	if e.replayBudget != nil {
		maxWork = *e.replayBudget
	}
	// Reserve the maximum admitted reader-index cost before sharing the rest
	// among SCCs. A costly component cannot exhaust the index admission budget
	// and thereby degrade independent components after they have converged.
	renderMax := maxWork - len(definitions)*128
	if renderMax < 0 {
		return nil, ErrWorkBudget
	}
	work := len(e.snapshot.Registry) + len(e.snapshot.States) + len(e.service.plan.groups)
	var oldGraph dependencyGraph
	stableGraph := false
	for graphPass := 0; graphPass < 4; graphPass++ {
		if err := e.checkControl(); err != nil {
			return nil, err
		}
		deps := make(map[ProgramID][]template.Dependency, len(definitions))
		for _, definition := range definitions {
			if current, ok := values[definition.id]; ok && (current.deps != nil || graphPass != 0) {
				deps[definition.id] = current.deps
			} else if prior, ok := e.readers[definition.id]; ok {
				deps[definition.id] = prior
			}
		}
		graph, err := buildDependencyGraph(definitions, e.snapshot, deps)
		if err != nil {
			return nil, err
		}
		work += graph.cost
		if work > renderMax {
			return nil, ErrWorkBudget
		}
		if graphPass != 0 && graph.sameEdges(oldGraph) {
			stableGraph = true
			break
		}
		oldGraph = graph
		remainingComponents := len(graph.order)
		for _, component := range graph.order {
			if err := e.checkControl(); err != nil {
				return nil, err
			}
			remaining := renderMax - work
			share := 0
			if remainingComponents != 0 {
				share = remaining / remainingComponents
			}
			if share < 1 || component.depth > 32 {
				e.degradeComponent(values, component, DiagnosticWorkBudget)
			} else {
				spent, err := e.renderComponent(base, values, component, share, now)
				if err != nil {
					return nil, err
				}
				work += spent
			}
			remainingComponents--
		}
	}
	if !stableGraph {
		// A changing graph is not a fixed point. Keep independent values but
		// mark every component that changed edges and its downstream readers.
		deps := make(map[ProgramID][]template.Dependency, len(definitions))
		for _, definition := range definitions {
			deps[definition.id] = values[definition.id].deps
		}
		current, err := buildDependencyGraph(definitions, e.snapshot, deps)
		if err != nil {
			return nil, err
		}
		work += current.cost
		if work > renderMax {
			return nil, ErrWorkBudget
		}
		affected := make(map[int]bool)
		for i := range current.edges {
			if !sameInts(current.edges[i], oldGraph.edges[i]) {
				affected[i] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for i, producers := range current.edges {
				if affected[i] {
					continue
				}
				for _, producer := range producers {
					if affected[producer] {
						affected[i], changed = true, true
						break
					}
				}
			}
		}
		for _, component := range current.order {
			for _, index := range component.members {
				if affected[index] {
					e.degradeComponent(values, component, DiagnosticWorkBudget)
					break
				}
			}
		}
	}
	if err := e.checkControl(); err != nil {
		return nil, err
	}
	// Reader admission is an index rebuild, not a free side effect of rendering.
	// It uses only reads retained by the completed graph (recovery hints after
	// errors), so a skipped branch cannot create a lasting listener.
	watchCost := 0
	for _, definition := range definitions {
		watchCost += len(values[definition.id].deps)
	}
	if watchCost > maxWork-work {
		return nil, ErrWorkBudget
	}
	watchAliases := make(map[domain.HAAlias]struct{}, watchCost)
	for _, definition := range definitions {
		for _, dep := range values[definition.id].deps {
			if alias, ok := dep.Alias(); ok {
				watchAliases[alias] = struct{}{}
			}
		}
	}
	work += watchCost
	changes := make([]Update, 0, len(definitions))
	e.activeMinute = false
	retainedReadBytes := 0
	for _, definition := range definitions {
		value := values[definition.id]
		if len(value.reads) > MaxReadsPerUpdate {
			return nil, ErrWorkBudget
		}
		retainedReadBytes += len(value.reads) * int(unsafe.Sizeof(template.Read{}))
		if retainedReadBytes > MaxRetainedReadBytes {
			return nil, ErrWorkBudget
		}
		for _, dep := range value.active {
			if dep.Kind() == template.DependencyMinute {
				e.activeMinute = true
			}
		}
		update := Update{id: definition.id, result: value.result, valid: value.valid, quality: value.quality, reads: append([]template.Read(nil), value.reads...), diagnostic: value.diagnostic, cursor: e.snapshot.Cursor}
		if definition.sourceReports {
			revision, err := sourceReportRevision(definition, update, e.snapshot.States)
			if err != nil {
				return nil, err
			}
			update.sourceRevision = revision
		}
		old, exists := e.outputs[definition.id]
		if !exists || !old.Equal(update) {
			changes = append(changes, update)
		}
		e.outputs[definition.id] = update
		e.readers[definition.id] = append([]template.Dependency(nil), value.deps...)
	}
	e.watchAliases = watchAliases
	if e.replayBudget != nil {
		*e.replayBudget -= work
	}
	return changes, nil
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (e *engine) degradeComponent(values map[ProgramID]candidate, component graphComponent, reason Diagnostic) {
	e.service.setError(ErrWorkBudget)
	for _, index := range component.members {
		definition := e.service.plan.definitions[index]
		value := values[definition.id]
		if deps, err := unionDependencies(e.readers[definition.id], value.deps, definition.static); err == nil {
			value.deps = deps
		}
		value.active = append([]template.Dependency(nil), value.deps...)
		if old, ok := e.outputs[definition.id]; ok {
			value.result, value.valid = old.result, old.valid
		}
		value.quality = staleQuality(value.quality)
		value.diagnostic = reason
		values[definition.id] = value
	}
}

func (e *engine) renderComponent(base template.ReadView, values map[ProgramID]candidate, component graphComponent, limit int, now time.Time) (int, error) {
	used := 0
	rounds := 1
	if component.cyclic {
		rounds = 16
	}
	stable := false
	for round := 0; round < rounds; round++ {
		if err := e.checkControl(); err != nil {
			return used, err
		}
		view := base
		for _, definition := range e.service.plan.definitions {
			value := values[definition.id]
			if !definition.hasTarget || !value.valid {
				continue
			}
			availability, _, _, err := value.quality.Components()
			if err != nil || availability != domain.AvailabilityAvailable {
				continue
			}
			record := e.snapshot.Registry[definition.target]
			_, _, aliases, err := record.Aliases()
			if err != nil {
				return used, err
			}
			cost := view.OverlayCost(aliases) + 1
			if used+cost > limit {
				e.degradeComponent(values, component, DiagnosticWorkBudget)
				return used, nil
			}
			view, err = view.WithCandidate(aliases, value.result.Text(), value.quality)
			if err != nil {
				return used, err
			}
			used += cost
		}
		next := make(map[ProgramID]candidate, len(component.members))
		equal := round != 0
		for offset, index := range component.members {
			if offset%8 == 0 {
				if err := e.checkControl(); err != nil {
					return used, err
				}
			}
			definition := e.service.plan.definitions[index]
			remaining := limit - used
			if remaining < 2 {
				e.degradeComponent(values, component, DiagnosticWorkBudget)
				return used, nil
			}
			zone := e.service.plan.zone
			if definition.zone != nil {
				zone = definition.zone
			}
			inputs, err := template.NewInputsFromReadView(view, definition.variables, now, zone)
			if err != nil {
				return used, err
			}
			renderLimit, err := template.DefaultLimits().WithSteps(remaining - 1)
			limited := err == nil && renderLimit.Steps() < template.DefaultLimits().Steps()
			if err != nil {
				// Per-render maximum is stricter than a component allocation.
				renderLimit = template.DefaultLimits()
			}
			tracked, renderErr := template.EvaluateTracked(e.service.ctx, definition.program, inputs, renderLimit)
			if limited && errors.Is(renderErr, template.ErrBudget) {
				e.degradeComponent(values, component, DiagnosticWorkBudget)
				return used + tracked.Steps(), nil
			}
			deps := tracked.Dependencies()
			reads := tracked.Reads()
			cost := tracked.Steps() + len(deps) + len(reads) + 1
			value := candidate{deps: deps, active: deps, reads: reads, quality: qualityFromReads(reads), diagnostic: DiagnosticHealthy}
			if renderErr == nil {
				value.result, value.valid = tracked.Result()
				cost += value.result.WorkUnits()
			} else {
				value.diagnostic = DiagnosticEvaluation
				value.deps, err = unionDependencies(e.readers[definition.id], deps, definition.static)
				if err != nil {
					return used, err
				}
				cost += len(value.deps)
				if old, ok := e.outputs[definition.id]; ok {
					value.result, value.valid = old.result, old.valid
				}
				value.quality = staleQuality(value.quality)
			}
			if used+cost > limit {
				e.degradeComponent(values, component, DiagnosticWorkBudget)
				return used, nil
			}
			used += cost
			next[definition.id] = value
			if !equalCandidate(values[definition.id], value) {
				equal = false
			}
		}
		for id, value := range next {
			values[id] = value
		}
		if !component.cyclic || equal {
			stable = true
			break
		}
	}
	if !stable {
		for _, index := range component.members {
			definition := e.service.plan.definitions[index]
			value := values[definition.id]
			if old, ok := e.outputs[definition.id]; ok {
				value.result, value.valid = old.result, old.valid
			} else {
				value.result, value.valid = template.Result{}, false
			}
			value.quality = staleQuality(value.quality)
			value.diagnostic = DiagnosticOscillation
			values[definition.id] = value
		}
	}
	return used, nil
}
