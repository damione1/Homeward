package automationruns

import (
	"context"
	"sort"
	"strings"

	"github.com/damione1/Homeward/compat/ha/ir"
	"github.com/damione1/Homeward/compat/ha/template"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/reactivetemplate"
)

// Admission conservatively counts names across the bounded program, including
// runtime wait/repeat/trigger slots, before the first action. A child caller's
// parameters are checked against this same set before arena admission.
func programBindingNames(nodes []executableNode, variables []template.Binding, automation bool) ([]string, error) {
	names := make(map[string]bool)
	for _, binding := range variables {
		names[binding.Name] = true
	}
	if automation {
		names["trigger"] = true
	}
	var visit func([]executableNode) error
	visit = func(nodes []executableNode) error {
		for _, node := range nodes {
			for _, binding := range node.variables {
				names[binding.Name] = true
			}
			if node.kind == ir.ActionRepeat {
				names["repeat"] = true
			}
			if node.kind == ir.ActionWait || node.kind == ir.ActionWaitTemplate {
				names["wait"] = true
			}
			if len(names) > MaxBindings {
				return ErrCapacity
			}
			if err := visit(node.children); err != nil {
				return err
			}
			if err := visit(node.otherwise); err != nil {
				return err
			}
			for _, branch := range node.branches {
				if err := visit(branch.sequence); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(nodes); err != nil {
		return nil, err
	}
	if len(names) > MaxBindings {
		return nil, ErrCapacity
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}
func fitProgramBindings(program executableProgram, bindings []template.Binding) error {
	names := make(map[string]bool, MaxBindings)
	for _, name := range program.bindingNames {
		names[name] = true
	}
	for _, binding := range bindings {
		names[binding.Name] = true
		if len(names) > MaxBindings {
			return ErrCapacity
		}
	}
	return admittedBindings(bindings)
}
func automationBindings(automation ir.Automation, variables []template.Binding, trigger Trigger) ([]template.Binding, error) {
	var metadata template.Value
	found := false
	for index, candidate := range automation.Triggers() {
		if candidate.Key() != trigger.TriggerKey() {
			continue
		}
		var err error
		metadata, err = triggerMetadata(candidate.ID(), index, candidate.Kind())
		if err != nil {
			return nil, err
		}
		found = true
		break
	}
	if !found {
		return nil, ErrInvalidPlan
	}
	bindings := make([]template.Binding, 0, len(variables)+1)
	for _, binding := range variables {
		if binding.Name != "trigger" {
			bindings = append(bindings, template.Binding{Name: binding.Name, Value: binding.Value.Copy()})
		}
	}
	bindings = append(bindings, template.Binding{Name: "trigger", Value: metadata})
	return bindings, admittedBindings(bindings)
}

func literalValue(value ir.Value) (template.Value, error) {
	switch value.Kind() {
	case ir.ValueString:
		return template.String(value.Text())
	case ir.ValueBool:
		return template.Bool(value.Bool()), nil
	case ir.ValueInteger:
		return template.Int(value.Integer()), nil
	case ir.ValueDecimal:
		return template.Float(value.Decimal())
	case ir.ValueNull:
		return template.None(), nil
	case ir.ValueInvalid:
		return template.Value{}, ErrInvalidPlan
	}
	return template.Value{}, ErrInvalidPlan
}

// evaluateConditions follows HA condition.async_template: only text "true"
// ignoring case is true. Python truthiness of a nonempty string is unrelated.
func (i *Interpreter) evaluateConditions(ctx context.Context, conditions []executableCondition, bindings []template.Binding, trigger Trigger) (bool, error) {
	for _, condition := range conditions {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		switch condition.kind {
		case ir.ConditionTrigger:
			matched := false
			for _, id := range condition.triggerIDs {
				if trigger.TriggerKey() == "id:"+id {
					matched = true
					break
				}
			}
			if !matched {
				return false, nil
			}
		case ir.ConditionState, ir.ConditionTemplate:
			var tracked template.TrackedResult
			var err error
			if shared, ok := i.input.Conditions.(*reactivetemplate.Service); ok {
				tracked, err = shared.EvaluateConditionAtZone(ctx, condition.program, bindings, i.input.Zone)
			} else {
				tracked, err = i.input.Conditions.EvaluateCondition(ctx, condition.program, bindings)
			}
			if err != nil {
				return false, executionCause(ExecutionConditionError, condition.source, err)
			}
			for _, read := range tracked.Reads() {
				quality, present := read.Quality()
				if !present || !read.Present() {
					return false, executionFailure(ExecutionSourceUnavailable, condition.source)
				}
				availability, freshness, provenance, err := quality.Components()
				if err != nil || availability != domain.AvailabilityAvailable || freshness != domain.FreshnessFresh ||
					(provenance != domain.ProvenanceObserved && provenance != domain.ProvenanceEstimated) {
					return false, executionFailure(ExecutionSourceUnavailable, condition.source)
				}
			}
			result, present := tracked.Result()
			if !present {
				return false, executionFailure(ExecutionConditionError, condition.source)
			}
			if !strings.EqualFold(result.Text(), "true") {
				return false, nil
			}
		case ir.ConditionInvalid:
			return false, ErrInvalidPlan
		default:
			return false, ErrInvalidPlan
		}
	}
	return true, nil
}
