package ir

import (
	"errors"
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
)

// ScriptID is an open identity set in its own HA namespace. Neither an
// automation name nor a bare slug is an admitted script reference.
type ScriptID struct{ value string }

func ParseScriptID(value string) (ScriptID, error) {
	if !strings.HasPrefix(value, "script.") || len(value) <= len("script.") || len(value) > 256 {
		return ScriptID{}, ErrInvalidInput
	}
	// HA components/script/config.py:71-79 reserves the component services.
	switch value {
	case "script.reload", "script.toggle", "script.turn_off", "script.turn_on":
		return ScriptID{}, ErrInvalidInput
	}
	for _, char := range value[len("script."):] {
		if char != '_' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return ScriptID{}, ErrInvalidInput
		}
	}
	return ScriptID{value: value}, nil
}

func (id ScriptID) Value() (string, error) {
	if _, err := ParseScriptID(id.value); err != nil {
		return "", err
	}
	return id.value, nil
}

// ScriptCallMode governs waiting at a script call, independently of the called
// script's ir.AutomationMode, which remains the only run-mode declaration.
type ScriptCallMode uint8

const (
	ScriptCallInvalid ScriptCallMode = iota
	ScriptCallDirect
	ScriptCallDetached
	ScriptCallStop
)

type Script struct {
	admitted           bool
	id                 ScriptID
	alias, description string
	mode               AutomationMode
	maxExceeded        MaxExceededMode
	max                int
	hasMax             bool
	source             Source
	variables          []Variable
	actions            []Action
}

func (s Script) Valid() bool                  { return s.admitted }
func (s Script) ID() ScriptID                 { return s.id }
func (s Script) Alias() string                { return s.alias }
func (s Script) Description() string          { return s.description }
func (s Script) Mode() AutomationMode         { return s.mode }
func (s Script) MaxExceeded() MaxExceededMode { return s.maxExceeded }
func (s Script) Max() (int, bool)             { return s.max, s.hasMax }
func (s Script) Source() Source               { return s.source.clone() }
func (s Script) Actions() []Action            { return cloneActions(s.actions) }
func (s Script) VariableCount() int           { return len(s.variables) }
func (s Script) ExecutionSize() (nodes, depth, bytes int) {
	nodes, depth, bytes = actionSize(s.actions, 1)
	bytes += s.source.planBytes() + 2*(len(s.id.value)+len(s.alias)+len(s.description))
	for _, variable := range s.variables {
		bytes += variableSize(variable)
	}
	return
}
func (s Script) Variables() []Variable {
	result := make([]Variable, len(s.variables))
	for i, item := range s.variables {
		result[i] = item.clone()
	}
	return result
}
func (s Script) clone() Script {
	s.source, s.variables, s.actions = s.Source(), s.Variables(), s.Actions()
	return s
}

type ScriptResult struct {
	accepted []Script
	rejected []Diagnostic
}

func (r ScriptResult) Accepted() []Script {
	result := make([]Script, len(r.accepted))
	for i, script := range r.accepted {
		result[i] = script.clone()
	}
	return result
}
func (r ScriptResult) Rejected() []Diagnostic {
	result := make([]Diagnostic, len(r.rejected))
	for i, item := range r.rejected {
		result[i] = item.clone()
	}
	return result
}

// CompileScriptSources uses the same confined loader and additive logic
// grammar as automations. It never fabricates an automation or a trigger.
func CompileScriptSources(object, entry string, files []parser.SourceFile, catalogue Catalogue) (ScriptResult, error) {
	id, err := ParseScriptID(object)
	if err != nil || !catalogue.Valid() || !catalogue.hasScript(id) {
		return ScriptResult{}, ErrInvalidInput
	}
	document, err := parser.LoadSources(entry, files, parser.SyntaxProfileLogic)
	if err != nil {
		var diagnostic *parser.Diagnostic
		if !errors.As(err, &diagnostic) || diagnostic == nil {
			return ScriptResult{}, err
		}
		code := CodeInvalidInput
		switch diagnostic.Code {
		case parser.CodeUnknownField:
			code = CodeUnknownField
		case parser.CodeUnsupportedForm:
			code = CodeUnsupportedForm
		}
		return ScriptResult{rejected: []Diagnostic{{code: code, object: object, field: diagnostic.Path,
			source: Source{object: object, at: position(diagnostic.At), path: diagnostic.Path},
			reason: diagnostic.Message, syntaxCode: diagnostic.Code.String()}}}, nil
	}
	root := document.Root()
	if root.Kind() != parser.KindMapping {
		return ScriptResult{}, ErrInvalidInput
	}
	if scripts, wrapped := root.Lookup("script"); wrapped {
		if len(root.Entries()) != 1 || scripts.Kind() != parser.KindMapping {
			return ScriptResult{}, ErrInvalidInput
		}
		var found bool
		root, found = scripts.Lookup(strings.TrimPrefix(object, "script."))
		if !found || len(scripts.Entries()) != 1 {
			return ScriptResult{}, ErrInvalidInput
		}
	}
	compiler := semanticCompiler{object: object, catalogue: catalogue, triggerIDs: make(map[string]bool)}
	script, diagnostic := compiler.script(id, root)
	if diagnostic.Valid() {
		return ScriptResult{rejected: []Diagnostic{diagnostic}}, nil
	}
	return ScriptResult{accepted: []Script{script}}, nil
}

func (c *semanticCompiler) script(id ScriptID, node parser.Node) (Script, Diagnostic) {
	if diagnostic := c.mapping(node, "alias", "description", "mode", "max", "max_exceeded", "variables", "sequence"); diagnostic.Valid() {
		return Script{}, diagnostic
	}
	result := Script{id: id, mode: AutomationSingle, maxExceeded: MaxExceededUnspecified, source: c.source(node)}
	var diagnostic Diagnostic
	result.alias, diagnostic = c.stringField(node, "alias", false)
	if diagnostic.Valid() {
		return Script{}, diagnostic
	}
	result.description, diagnostic = c.stringField(node, "description", false)
	if diagnostic.Valid() {
		return Script{}, diagnostic
	}
	if mode, present := node.Lookup("mode"); present {
		text, valid := plain(mode)
		result.mode = parseAutomationMode(text)
		if !valid || result.mode == AutomationModeInvalid {
			return Script{}, c.fail(CodeInvalidValue, "mode", mode, "mode outside admitted profile")
		}
	}
	if exceeded, present := node.Lookup("max_exceeded"); present {
		text, valid := plain(exceeded)
		result.maxExceeded = parseMaxExceededMode(text)
		if !valid || text == "" || result.maxExceeded == MaxExceededInvalid {
			return Script{}, c.fail(CodeInvalidValue, "max_exceeded", exceeded, "max_exceeded outside admitted profile")
		}
	}
	if maximum, present := node.Lookup("max"); present {
		count, valid := scalarInt(maximum)
		if !valid || count < 1 || count > 10000 {
			return Script{}, c.fail(CodeInvalidValue, "max", maximum, "positive max of at most 10000 required")
		}
		result.max, result.hasMax = int(count), true
	}
	if variables, present := node.Lookup("variables"); present {
		result.variables, diagnostic = c.variables(variables)
		if diagnostic.Valid() {
			return Script{}, diagnostic
		}
	}
	sequence, diagnostic := c.required(node, "sequence")
	if diagnostic.Valid() {
		return Script{}, diagnostic
	}
	result.actions, diagnostic = c.actionList(sequence, true)
	if diagnostic.Valid() {
		return Script{}, diagnostic
	}
	result.admitted = true
	return result, Diagnostic{}
}

func (c *semanticCompiler) scriptCall(node parser.Node, name string) (Call, Diagnostic) {
	result := Call{kind: CallScript}
	if metadata, present := node.Lookup("metadata"); present {
		if diagnostic := c.mapping(metadata); diagnostic.Valid() {
			return Call{}, diagnostic
		}
	}
	switch name {
	case "script.turn_on":
		result.scriptMode = ScriptCallDetached
	case "script.turn_off":
		result.scriptMode = ScriptCallStop
	default:
		id, err := ParseScriptID(name)
		if err != nil {
			return Call{}, c.fail(CodeInvalidValue, "action", node, "invalid script identity")
		}
		result.scriptMode, result.scripts = ScriptCallDirect, []ScriptID{id}
	}
	if result.scriptMode == ScriptCallDirect {
		if target, present := node.Lookup("target"); present {
			return Call{}, c.fail(CodeUnknownField, "target", target, "direct script target is its service name")
		}
		if data, present := node.Lookup("data"); present {
			variables, diagnostic := c.variables(data)
			if diagnostic.Valid() {
				return Call{}, diagnostic
			}
			result.scriptVariables = variables
		}
	} else {
		target, diagnostic := c.required(node, "target")
		if diagnostic.Valid() {
			return Call{}, diagnostic
		}
		if diagnostic := c.mapping(target, "entity_id"); diagnostic.Valid() {
			return Call{}, diagnostic
		}
		entities, diagnostic := c.required(target, "entity_id")
		if diagnostic.Valid() {
			return Call{}, diagnostic
		}
		items := entities.Items()
		if entities.Kind() != parser.KindSequence {
			items = []parser.Node{entities}
		}
		if len(items) == 0 {
			return Call{}, c.fail(CodeInvalidValue, "target", entities, "script target required")
		}
		seen := make(map[ScriptID]bool, len(items))
		for _, item := range items {
			text, valid := plain(item)
			id, err := ParseScriptID(text)
			if !valid || err != nil || seen[id] {
				return Call{}, c.fail(CodeInvalidValue, "entity_id", item, "invalid or duplicate script target")
			}
			seen[id] = true
			result.scripts = append(result.scripts, id)
		}
		if data, present := node.Lookup("data"); present {
			allowed := []string{}
			if result.scriptMode == ScriptCallDetached {
				allowed = []string{"variables"}
			}
			if diagnostic := c.mapping(data, allowed...); diagnostic.Valid() {
				return Call{}, diagnostic
			}
			if variables, present := data.Lookup("variables"); present {
				result.scriptVariables, diagnostic = c.variables(variables)
				if diagnostic.Valid() {
					return Call{}, diagnostic
				}
			}
		}
	}
	for _, id := range result.scripts {
		if !c.catalogue.hasScript(id) {
			return Call{}, c.fail(CodeUnknownReference, "action", node, "script is not in the supplied generation catalogue")
		}
	}
	return result, Diagnostic{}
}

// WaitDefinition is the immutable selection needed by the one NHA-040
// trigger service. It is extracted without cloning unrelated action programs.
type WaitDefinition struct {
	source   Source
	triggers []Trigger
}

func (w WaitDefinition) Source() Source { return w.source.clone() }
func (w WaitDefinition) Triggers() []Trigger {
	result := make([]Trigger, len(w.triggers))
	for index, trigger := range w.triggers {
		result[index] = trigger.clone()
	}
	return result
}
func (a Automation) Waits() []WaitDefinition { return actionWaits(a.actions, nil) }
func (s Script) Waits() []WaitDefinition     { return actionWaits(s.actions, nil) }
func actionWaits(actions []Action, result []WaitDefinition) []WaitDefinition {
	for _, action := range actions {
		if action.kind == ActionWait {
			result = append(result, WaitDefinition{action.source.clone(), action.Triggers()})
		}
		result = actionWaits(action.children, result)
		result = actionWaits(action.otherwise, result)
		for _, branch := range action.branches {
			result = actionWaits(branch.actions, result)
		}
	}
	return result
}
func (a Automation) WaitPlanBytes() (int, int) { return waitPlanBytes(a.actions) }
func (s Script) WaitPlanBytes() (int, int)     { return waitPlanBytes(s.actions) }
func waitPlanBytes(actions []Action) (count, bytes int) {
	for _, action := range actions {
		if action.kind == ActionWait {
			n, charge := triggerPlanBytes(action.source.object, action.triggers)
			count += n
			bytes += charge + action.source.planBytes() + 256
		}
		n, charge := waitPlanBytes(action.children)
		count += n
		bytes += charge
		n, charge = waitPlanBytes(action.otherwise)
		count += n
		bytes += charge
		for _, branch := range action.branches {
			n, charge = waitPlanBytes(branch.actions)
			count += n
			bytes += charge
		}
	}
	return
}

func (a Automation) HasAction(kind ActionKind) bool { return hasAction(a.actions, kind) }
func (s Script) HasAction(kind ActionKind) bool     { return hasAction(s.actions, kind) }
func hasAction(actions []Action, kind ActionKind) bool {
	for _, action := range actions {
		if action.kind == kind || hasAction(action.children, kind) || hasAction(action.otherwise, kind) {
			return true
		}
		for _, branch := range action.branches {
			if hasAction(branch.actions, kind) {
				return true
			}
		}
	}
	return false
}
