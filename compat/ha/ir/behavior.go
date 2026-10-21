package ir

import "reflect"

// SameBehavior compares admitted programs, excluding source locations and
// display metadata. Compiled template trees remain part of the comparison.
// Thus moving a document or editing a comment cannot restart a live run.
func (a Automation) SameBehavior(b Automation) bool {
	if !a.Valid() || !b.Valid() {
		return false
	}
	a, b = a.clone(), b.clone()
	normalizeAutomation(&a)
	normalizeAutomation(&b)
	return reflect.DeepEqual(a, b)
}

func (a Script) SameBehavior(b Script) bool {
	if !a.Valid() || !b.Valid() {
		return false
	}
	a, b = a.clone(), b.clone()
	a.source, b.source = Source{}, Source{}
	a.alias, a.description, b.alias, b.description = "", "", "", ""
	normalizeVariables(a.variables)
	normalizeVariables(b.variables)
	normalizeActions(a.actions)
	normalizeActions(b.actions)
	return reflect.DeepEqual(a, b)
}

func normalizeAutomation(a *Automation) {
	a.source = Source{}
	a.alias, a.description, a.category = "", "", ""
	normalizeTriggers(a.triggers)
	normalizeConditions(a.conditions)
	normalizeVariables(a.variables)
	normalizeActions(a.actions)
}
func normalizeTriggers(values []Trigger) {
	for n := range values {
		values[n].source = Source{}
	}
}
func normalizeConditions(values []Condition) {
	for n := range values {
		values[n].source = Source{}
		values[n].templateSource = ""
	}
}
func normalizeVariables(values []Variable) {
	for n := range values {
		values[n].source, values[n].value.source = Source{}, Source{}
	}
}
func normalizeActions(values []Action) {
	for n := range values {
		a := &values[n]
		a.source = Source{}
		normalizeVariables(a.variables)
		normalizeConditions(a.conditions)
		normalizeTriggers(a.triggers)
		normalizeActions(a.children)
		normalizeActions(a.otherwise)
		for j := range a.branches {
			b := &a.branches[j]
			b.source = Source{}
			normalizeConditions(b.conditions)
			normalizeActions(b.actions)
		}
		c := &a.call
		c.temperature.source, c.brightness.percent.source, c.transitionSeconds.source = Source{}, Source{}, Source{}
		for j := range c.targets {
			c.targets[j].source = Source{}
		}
		normalizeVariables(c.scriptVariables)
	}
}
