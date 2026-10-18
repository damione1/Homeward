package ir

import (
	"fmt"
	"math"
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
)

func (c *semanticCompiler) actionList(node parser.Node, required bool) ([]Action, Diagnostic) {
	if node.Kind() != parser.KindSequence {
		return nil, c.fail(CodeUnsupportedForm, "actions", node, "action list required")
	}
	items := node.Items()
	if required && len(items) == 0 {
		return nil, c.fail(CodeInvalidValue, "actions", node, "at least one action required")
	}
	result := make([]Action, 0, len(items))
	for _, item := range items {
		action, d := c.action(item)
		if d.Valid() {
			return nil, d
		}
		result = append(result, action)
	}
	return result, Diagnostic{}
}

func (c *semanticCompiler) action(node parser.Node) (Action, Diagnostic) {
	if node.Kind() != parser.KindMapping {
		return Action{}, c.fail(CodeUnsupportedForm, "action", node, "action mapping required")
	}
	selection := ""
	for _, key := range []string{"variables", "sequence", "choose", "if", "repeat", "parallel", "wait_for_trigger", "stop", "action"} {
		if _, present := node.Lookup(key); present {
			if selection != "" {
				return Action{}, c.fail(CodeUnsupportedForm, key, node, "exactly one action variant is required")
			}
			selection = key
		}
	}
	if selection == "" {
		return Action{}, c.fail(CodeUnsupportedForm, "action", node, "action variant is missing")
	}
	action := Action{source: c.source(node)}
	switch selection {
	case "variables":
		if d := c.mapping(node, "variables"); d.Valid() {
			return Action{}, d
		}
		value, _ := node.Lookup("variables")
		variables, d := c.variables(value)
		if d.Valid() {
			return Action{}, d
		}
		action.kind = ActionVariables
		action.variables = variables
	case "sequence":
		if d := c.mapping(node, "sequence"); d.Valid() {
			return Action{}, d
		}
		value, _ := node.Lookup("sequence")
		children, d := c.actionList(value, false)
		if d.Valid() {
			return Action{}, d
		}
		action.kind = ActionSequence
		action.children = children
	case "choose":
		if d := c.mapping(node, "choose", "default"); d.Valid() {
			return Action{}, d
		}
		value, _ := node.Lookup("choose")
		if value.Kind() != parser.KindSequence {
			return Action{}, c.fail(CodeUnsupportedForm, "choose", value, "branch list required")
		}
		if len(value.Items()) == 0 {
			return Action{}, c.fail(CodeInvalidValue, "choose", value, "at least one branch required")
		}
		for _, item := range value.Items() {
			if d := c.mapping(item, "conditions", "sequence"); d.Valid() {
				return Action{}, d
			}
			conditions, diag := c.required(item, "conditions")
			if diag.Valid() {
				return Action{}, diag
			}
			compiledConditions, diag := c.conditionList(conditions)
			if diag.Valid() {
				return Action{}, diag
			}
			if len(compiledConditions) == 0 {
				return Action{}, c.fail(CodeInvalidValue, "conditions", conditions, "branch condition required")
			}
			sequence, diag := c.required(item, "sequence")
			if diag.Valid() {
				return Action{}, diag
			}
			compiledActions, diag := c.actionList(sequence, false)
			if diag.Valid() {
				return Action{}, diag
			}
			action.branches = append(action.branches, Branch{conditions: compiledConditions, actions: compiledActions, source: c.source(item)})
		}
		if otherwise, ok := node.Lookup("default"); ok {
			children, d := c.actionList(otherwise, false)
			if d.Valid() {
				return Action{}, d
			}
			action.otherwise = children
		}
		action.kind = ActionChoose
	case "if":
		if d := c.mapping(node, "if", "then", "else"); d.Valid() {
			return Action{}, d
		}
		value, _ := node.Lookup("if")
		conditions, d := c.conditionList(value)
		if d.Valid() {
			return Action{}, d
		}
		if len(conditions) == 0 {
			return Action{}, c.fail(CodeInvalidValue, "if", value, "if condition required")
		}
		then, diag := c.required(node, "then")
		if diag.Valid() {
			return Action{}, diag
		}
		children, diag := c.actionList(then, false)
		if diag.Valid() {
			return Action{}, diag
		}
		if otherwise, ok := node.Lookup("else"); ok {
			alternative, diag := c.actionList(otherwise, false)
			if diag.Valid() {
				return Action{}, diag
			}
			action.otherwise = alternative
		}
		action.kind = ActionIf
		action.conditions = conditions
		action.children = children
	case "repeat":
		if d := c.mapping(node, "repeat"); d.Valid() {
			return Action{}, d
		}
		value, _ := node.Lookup("repeat")
		if d := c.mapping(value, "count", "sequence"); d.Valid() {
			return Action{}, d
		}
		countNode, diag := c.required(value, "count")
		if diag.Valid() {
			return Action{}, diag
		}
		if literal, valid := plain(countNode); valid && containsTemplateSyntax(literal) {
			return Action{}, c.fail(CodeUnsupportedTemplate, "count", countNode, "templated repeat count is outside static repeat profile")
		}
		count, valid := scalarInt(countNode)
		if !valid || count < 1 || count > 10000 {
			return Action{}, c.fail(CodeInvalidValue, "count", countNode, "repeat count must be 1..10000")
		}
		sequence, diag := c.required(value, "sequence")
		if diag.Valid() {
			return Action{}, diag
		}
		children, diag := c.actionList(sequence, false)
		if diag.Valid() {
			return Action{}, diag
		}
		action.kind = ActionRepeat
		action.count = int(count)
		action.children = children
	case "parallel":
		if d := c.mapping(node, "parallel"); d.Valid() {
			return Action{}, d
		}
		value, _ := node.Lookup("parallel")
		children, d := c.actionList(value, true)
		if d.Valid() {
			return Action{}, d
		}
		action.kind = ActionParallel
		action.children = children
	case "wait_for_trigger":
		if d := c.mapping(node, "wait_for_trigger", "timeout", "continue_on_timeout"); d.Valid() {
			return Action{}, d
		}
		value, _ := node.Lookup("wait_for_trigger")
		triggers, d := c.triggerList(value, true, false)
		if d.Valid() {
			return Action{}, d
		}
		action.kind = ActionWait
		action.triggers = triggers
		if timeout, ok := node.Lookup("timeout"); ok {
			milliseconds, diag := c.timeout(timeout)
			if diag.Valid() {
				return Action{}, diag
			}
			action.timeoutMillis = milliseconds
			action.hasTimeout = true
		}
		if continuation, ok := node.Lookup("continue_on_timeout"); ok {
			enabled, valid := scalarBool(continuation)
			if !valid {
				return Action{}, c.fail(CodeInvalidValue, "continue_on_timeout", continuation, "boolean required")
			}
			action.continueOnTimeout = enabled
			action.hasContinueOnTimeout = true
		}
	case "stop":
		if d := c.mapping(node, "stop"); d.Valid() {
			return Action{}, d
		}
		value, _ := node.Lookup("stop")
		reason, valid := plain(value)
		if !valid {
			return Action{}, c.fail(CodeInvalidValue, "stop", value, "stop reason must be a string")
		}
		action.kind = ActionStop
		action.stop = reason
	case "action":
		if d := c.mapping(node, "action", "metadata", "data", "target"); d.Valid() {
			return Action{}, d
		}
		call, d := c.call(node)
		if d.Valid() {
			return Action{}, d
		}
		action.kind = ActionCall
		action.call = call
	}
	return action, Diagnostic{}
}

func (c *semanticCompiler) variables(node parser.Node) ([]Variable, Diagnostic) {
	if node.Kind() != parser.KindMapping {
		return nil, c.fail(CodeUnsupportedForm, "variables", node, "variable mapping required")
	}
	result := make([]Variable, 0, len(node.Entries()))
	for _, entry := range node.Entries() {
		if !validName(entry.Key) {
			return nil, c.failKey(CodeInvalidValue, entry, "invalid variable name")
		}
		value, d := c.variableValue(entry.Key, entry.Value)
		if d.Valid() {
			return nil, d
		}
		result = append(result, Variable{name: entry.Key, value: value, source: c.source(entry.Value)})
	}
	return result, Diagnostic{}
}
func (c *semanticCompiler) variableValue(name string, node parser.Node) (Value, Diagnostic) {
	scalar, ok := node.Scalar()
	if !ok {
		return Value{}, c.fail(CodeUnsupportedForm, "variables", node, "only scalar variables are admitted")
	}
	value := Value{source: c.source(node), text: scalar.Text()}
	switch scalar.Kind() {
	case parser.ScalarString:
		if containsTemplateSyntax(scalar.Text()) {
			return Value{}, c.fail(CodeUnsupportedTemplate, name, node, "templated variable is outside literal-variable profile")
		}
		value.kind = ValueString
	case parser.ScalarBool:
		value.kind = ValueBool
		value.boolean = strings.EqualFold(scalar.Text(), "true")
	case parser.ScalarInteger:
		integer, valid := scalarInt(node)
		if !valid {
			return Value{}, c.fail(CodeInvalidValue, "variables", node, "invalid integer")
		}
		value.kind = ValueInteger
		value.integer = integer
	case parser.ScalarFloat:
		decimal, valid := scalarNumber(node)
		if !valid {
			return Value{}, c.fail(CodeInvalidValue, "variables", node, "invalid decimal")
		}
		value.kind = ValueDecimal
		value.decimal = decimal
	case parser.ScalarNull:
		value.kind = ValueNull
	default:
		return Value{}, c.fail(CodeUnsupportedForm, "variables", node, "scalar kind is outside profile")
	}
	return value, Diagnostic{}
}

func (c *semanticCompiler) timeout(node parser.Node) (int64, Diagnostic) {
	if literal, valid := plain(node); valid && containsTemplateSyntax(literal) {
		return 0, c.fail(CodeUnsupportedTemplate, "timeout", node, "templated timeout is outside static wait profile")
	}
	if d := c.mapping(node, "milliseconds", "seconds", "minutes"); d.Valid() {
		return 0, d
	}
	if len(node.Entries()) == 0 {
		return 0, c.fail(CodeInvalidValue, "timeout", node, "timeout component required")
	}
	var total int64
	for _, entry := range node.Entries() {
		if literal, valid := plain(entry.Value); valid && containsTemplateSyntax(literal) {
			return 0, c.fail(CodeUnsupportedTemplate, entry.Key, entry.Value, "templated timeout component is outside static wait profile")
		}
		value, valid := scalarNumber(entry.Value)
		if !valid || value < 0 {
			return 0, c.fail(CodeInvalidValue, entry.Key, entry.Value, "nonnegative duration component required")
		}
		multiplier := float64(1)
		switch entry.Key {
		case "seconds":
			multiplier = 1000
		case "minutes":
			multiplier = 60000
		}
		milliseconds := value * multiplier
		if milliseconds > 86400000 || milliseconds != math.Trunc(milliseconds) || float64(total)+milliseconds > 86400000 {
			return 0, c.fail(CodeInvalidValue, entry.Key, entry.Value, "timeout must be an exact millisecond within 24 hours")
		}
		total += int64(milliseconds)
	}
	return total, Diagnostic{}
}

func (c *semanticCompiler) call(node parser.Node) (Call, Diagnostic) {
	selection, d := c.stringField(node, "action", true)
	if d.Valid() {
		return Call{}, d
	}
	serviceNode, _ := node.Lookup("action")
	if containsTemplateSyntax(selection) {
		return Call{}, c.fail(CodeUnsupportedTemplate, "action", serviceNode, "templated service name is outside static service profile")
	}
	call := Call{}
	var domain string
	var support DeclaredSupport
	switch selection {
	case "climate.set_temperature":
		call.kind = CallClimateSetTemperature
		domain = "climate"
		support = SupportClimateSetpoint
	case "switch.turn_on":
		call.kind = CallSwitchTurnOn
		domain = "switch"
		support = SupportSwitchTurnOn
	case "light.turn_on":
		call.kind = CallLightTurnOn
		domain = "light"
		support = SupportLightTurnOn
	case "light.turn_off":
		call.kind = CallLightTurnOff
		domain = "light"
		support = SupportLightTurnOff
	case "scene.turn_on":
		call.kind = CallSceneTurnOn
		domain = "scene"
		support = SupportSceneTurnOn
		c.need(RequirementNHA046Scene)
	default:
		return Call{}, c.fail(CodeUnknownService, "action", serviceNode, fmt.Sprintf("service %q is outside selected lot", selection))
	}
	if metadata, ok := node.Lookup("metadata"); ok {
		if metadata.Kind() != parser.KindMapping {
			return Call{}, c.fail(CodeUnsupportedForm, "metadata", metadata, "metadata mapping required")
		}
		if len(metadata.Entries()) != 0 {
			return Call{}, c.fail(CodeUnknownField, "metadata", metadata, "nonempty metadata is not admitted")
		}
	}
	target, diag := c.required(node, "target")
	if diag.Valid() {
		return Call{}, diag
	}
	call.targets, diag = c.targets(target, domain, support)
	if diag.Valid() {
		return Call{}, diag
	}
	data, hasData := node.Lookup("data")
	if !hasData {
		return Call{}, c.fail(CodeInvalidValue, "data", node, "explicit service data mapping required")
	}
	if data.Kind() != parser.KindMapping {
		return Call{}, c.fail(CodeUnsupportedForm, "data", data, "service data mapping required")
	}
	allowed := []string{}
	switch call.kind {
	case CallClimateSetTemperature:
		allowed = []string{"temperature"}
	case CallLightTurnOn:
		allowed = []string{"brightness_pct", "brightness_step_pct", "transition"}
	case CallLightTurnOff, CallSceneTurnOn:
		allowed = []string{"transition"}
	}
	if d = c.mapping(data, allowed...); d.Valid() {
		return Call{}, d
	}
	switch call.kind {
	case CallClimateSetTemperature:
		temperature, ok := data.Lookup("temperature")
		if !ok {
			return Call{}, c.fail(CodeInvalidValue, "temperature", data, "setpoint required")
		}
		call.temperature, diag = c.number(temperature, "temperature", -math.MaxFloat64, math.MaxFloat64)
		if diag.Valid() {
			return Call{}, diag
		}
	case CallLightTurnOn:
		absolute, hasAbsolute := data.Lookup("brightness_pct")
		relative, hasRelative := data.Lookup("brightness_step_pct")
		if hasAbsolute && hasRelative {
			return Call{}, c.fail(CodeUnsupportedForm, "brightness_step_pct", data, "absolute and relative brightness are exclusive")
		}
		call.brightness.kind = BrightnessUnchanged
		if hasAbsolute {
			call.brightness.kind = BrightnessAbsolutePercent
			call.brightness.percent, diag = c.number(absolute, "brightness_pct", 0, 100)
			if diag.Valid() {
				return Call{}, diag
			}
			for _, target := range call.targets {
				if d = c.requireTargetCapability(target, SupportLightBrightnessAbsolute, "brightness_pct"); d.Valid() {
					return Call{}, d
				}
			}
		}
		if hasRelative {
			call.brightness.kind = BrightnessRelativePercent
			call.brightness.percent, diag = c.number(relative, "brightness_step_pct", -100, 100)
			if diag.Valid() {
				return Call{}, diag
			}
			for _, target := range call.targets {
				if d = c.requireTargetCapability(target, SupportLightBrightnessRelative, "brightness_step_pct"); d.Valid() {
					return Call{}, d
				}
			}
			c.need(RequirementObservedBrightnessAndRevision)
		}
	}
	if value, ok := data.Lookup("transition"); ok {
		call.transitionSeconds, diag = c.number(value, "transition", 0, 6553)
		if diag.Valid() {
			return Call{}, diag
		}
		needed := SupportLightTransition
		if call.kind == CallSceneTurnOn {
			needed = SupportSceneTransition
			c.need(RequirementNHA046Scene)
		}
		for _, target := range call.targets {
			if d = c.requireTargetCapability(target, needed, "transition"); d.Valid() {
				return Call{}, d
			}
		}
		c.need(RequirementNHA046Transition)
	}
	return call, Diagnostic{}
}

func (c *semanticCompiler) number(node parser.Node, field string, min, max float64) (Number, Diagnostic) {
	if literal, valid := plain(node); valid && containsTemplateSyntax(literal) {
		return Number{}, c.fail(CodeUnsupportedTemplate, field, node, "templated service data is outside static data profile")
	}
	value, valid := scalarNumber(node)
	if !valid || value < min || value > max {
		return Number{}, c.fail(CodeInvalidValue, field, node, fmt.Sprintf("number must be within %g..%g", min, max))
	}
	return Number{present: true, value: value, source: c.source(node)}, Diagnostic{}
}

func (c *semanticCompiler) targets(node parser.Node, domain string, support DeclaredSupport) ([]Target, Diagnostic) {
	if d := c.mapping(node, "entity_id"); d.Valid() {
		return nil, d
	}
	value, diag := c.required(node, "entity_id")
	if diag.Valid() {
		return nil, diag
	}
	var nodes []parser.Node
	if value.Kind() == parser.KindSequence {
		nodes = value.Items()
	} else {
		nodes = []parser.Node{value}
	}
	if len(nodes) == 0 {
		return nil, c.fail(CodeInvalidValue, "entity_id", value, "target list cannot be empty")
	}
	result := make([]Target, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for _, item := range nodes {
		id, valid := plain(item)
		if valid && containsTemplateSyntax(id) {
			return nil, c.fail(CodeUnsupportedTemplate, "entity_id", item, "templated service target is outside static target profile")
		}
		if !valid || !strings.HasPrefix(id, domain+".") {
			return nil, c.fail(CodeInvalidValue, "entity_id", item, "target entity domain or identity is invalid")
		}
		if seen[id] {
			return nil, c.fail(CodeInvalidValue, "entity_id", item, "duplicate target")
		}
		seen[id] = true
		alias, diag := c.requireEntity(id, support, item)
		if diag.Valid() {
			return nil, diag
		}
		result = append(result, Target{alias: alias, source: c.source(item)})
	}
	return result, Diagnostic{}
}
