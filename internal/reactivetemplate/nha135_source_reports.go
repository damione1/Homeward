package reactivetemplate

import (
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
)

// WithSourceReports retains each Host source revision as a publication
// boundary even when its attribute is unchanged. This is required for an
// external integrator to receive new interval bounds; it adds no timer or
// inferred reports. Ordinary templates retain semantic deduplication.
// The option admits only one static attribute read and no internal target,
// so graph convergence cannot manufacture a new source report.
func (d Definition) WithSourceReports() (Definition, error) {
	if _, err := d.id.Value(); err != nil || !d.program.Valid() || d.hasTarget || len(d.static) != 1 || d.static[0].Kind() != template.DependencyAttribute {
		return Definition{}, ErrInvalidPlan
	}
	d.sourceReports = true
	return d, nil
}

func sourceReportRevision(definition Definition, update Update, states map[domain.EntityID]domain.ObservedState) (uint64, error) {
	reads := update.reads
	if len(definition.static) != 1 {
		return 0, ErrInvalidPlan
	}
	// The existing graph/work-budget isolation can degrade a component before
	// its first render. Preserve that explicitly invalid, trace-free result
	// without inventing a source revision or failing independent programs.
	if len(reads) == 0 && !update.valid && update.diagnostic == DiagnosticWorkBudget {
		return 0, nil
	}
	// Static deduplication can hide two executed reads of the same field.
	// Refuse that shape instead of manufacturing an interval identity.
	if len(reads) != 1 || reads[0].Dependency() != definition.static[0] {
		return 0, ErrInvalidPlan
	}
	entity, present := reads[0].Entity()
	if !present {
		return 0, nil
	}
	state, present := states[entity]
	if !present {
		return 0, nil
	}
	_, _, _, _, revision, err := state.Components()
	if err != nil {
		return 0, nil
	}
	return revision, nil
}
