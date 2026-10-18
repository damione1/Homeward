package ir

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

var ErrInvalidInput = errors.New("invalid semantic compiler input")

// CompileSources is the entry for one selected object. It reuses exactly the
// NHA-037 confined/snapshot loader and budgets. The caller can process the
// next object even when this one's syntax or semantics is refused.
func CompileSources(object, entry string, files []parser.SourceFile, catalogue Catalogue) (Result, error) {
	if !validName(object) || !strings.HasPrefix(object, "automation.") || !catalogue.Valid() {
		return Result{}, ErrInvalidInput
	}
	doc, err := parser.LoadSources(entry, files, parser.SyntaxProfileLogic)
	if err != nil {
		var syntax *parser.Diagnostic
		if !errors.As(err, &syntax) || syntax == nil {
			return Result{}, err
		}
		code := CodeInvalidInput
		switch syntax.Code {
		case parser.CodeUnknownField:
			code = CodeUnknownField
		case parser.CodeUnsupportedForm:
			code = CodeUnsupportedForm
		}
		field := syntax.Path
		if dot := strings.LastIndexByte(field, '.'); dot >= 0 {
			field = field[dot+1:]
		}
		return Result{rejected: []Diagnostic{{code: code, object: object, field: field, source: Source{object: object, at: position(syntax.At), path: syntax.Path}, reason: syntax.Message, syntaxCode: syntax.Code.String()}}}, nil
	}
	root := doc.Root()
	if root.Kind() != parser.KindMapping {
		return Result{}, ErrInvalidInput
	}
	if _, wrapped := root.Lookup("automation"); wrapped {
		return Result{}, ErrInvalidInput
	}
	compiler := semanticCompiler{object: object, catalogue: catalogue}
	automation, diagnostic := compiler.automation(root)
	if diagnostic.Valid() {
		return Result{rejected: []Diagnostic{diagnostic}}, nil
	}
	return Result{accepted: []Automation{automation}}, nil
}

// CompileDocument accepts an already validated additive logic document. Each
// semantic rejection is atomic. Use CompileSources per selected file when a
// syntax rejection must also be isolated from other objects.
func CompileDocument(doc *parser.Document, catalogue Catalogue) (Result, error) {
	if doc == nil || doc.Profile() != parser.LogicProfile || !catalogue.Valid() {
		return Result{}, ErrInvalidInput
	}
	root := doc.Root()
	if root.Kind() != parser.KindMapping {
		return Result{}, ErrInvalidInput
	}
	if automations, ok := root.Lookup("automation"); ok {
		if len(root.Entries()) != 1 || automations.Kind() != parser.KindSequence {
			return Result{}, ErrInvalidInput
		}
		return compileMany(automations.Items(), catalogue), nil
	}
	return compileMany([]parser.Node{root}, catalogue), nil
}

// CompileExpanded uses the exact, bounded NHA-038 expansion and its retained
// provenance. The third-party blueprint remains outside the product.
func CompileExpanded(expanded *parser.ExpandedBlueprint, catalogue Catalogue) (Result, error) {
	if expanded == nil || !expanded.Valid() || expanded.Profile() != parser.BlueprintExpandedProfile || !catalogue.Valid() {
		return Result{}, ErrInvalidInput
	}
	return compileMany([]parser.Node{expanded.Root()}, catalogue), nil
}

func compileMany(nodes []parser.Node, catalogue Catalogue) Result {
	result := Result{}
	seen := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		name := objectName(node)
		compiler := semanticCompiler{object: name, catalogue: catalogue}
		if seen[name] {
			result.rejected = append(result.rejected, compiler.fail(CodeInvalidValue, "id", node, "duplicate automation identity"))
			continue
		}
		seen[name] = true
		automation, diagnostic := compiler.automation(node)
		if diagnostic.Valid() {
			result.rejected = append(result.rejected, diagnostic)
		} else {
			result.accepted = append(result.accepted, automation)
		}
	}
	return result
}

func objectName(node parser.Node) string {
	if id, ok := node.Lookup("id"); ok {
		if value, valid := plain(id); valid && value != "" {
			return "automation." + value
		}
	}
	file := filepath.Base(node.Source().File)
	if strings.HasPrefix(file, "automation.") && (strings.HasSuffix(file, ".yaml") || strings.HasSuffix(file, ".yml")) {
		return strings.TrimSuffix(strings.TrimSuffix(file, ".yaml"), ".yml")
	}
	return "automation@" + node.Path()
}

type semanticCompiler struct {
	object       string
	catalogue    Catalogue
	requirements []Requirement
	triggerIDs   map[string]bool
}

func (c *semanticCompiler) fail(code Code, field string, node parser.Node, reason string) Diagnostic {
	return Diagnostic{code: code, object: c.object, field: field, source: c.source(node), reason: reason}
}
func (c *semanticCompiler) failKey(code Code, entry parser.Entry, reason string) Diagnostic {
	source := c.source(entry.Value)
	source.at = position(entry.KeySource)
	return Diagnostic{code: code, object: c.object, field: entry.Key, source: source, reason: reason}
}
func position(p parser.Position) Position {
	return Position{File: p.File, Line: p.Line, Column: p.Column}
}
func (c *semanticCompiler) source(node parser.Node) Source {
	source := sourceOf(node)
	source.object = c.object
	return source
}
func sourceOf(node parser.Node) Source {
	source := Source{at: position(node.Source()), path: node.Path()}
	for _, ref := range node.References() {
		kind := ReferenceInvalid
		switch ref.Kind {
		case parser.ReferenceInclude:
			kind = ReferenceInclude
		case parser.ReferenceAlias:
			kind = ReferenceAlias
		}
		source.references = append(source.references, Reference{Kind: kind, At: position(ref.At)})
	}
	for _, sub := range node.Substitutions() {
		origin := InputOriginInvalid
		switch sub.Origin() {
		case parser.InputOriginInvocation:
			origin = InputOriginInvocation
		case parser.InputOriginDefault:
			origin = InputOriginDefault
		}
		use := InputUse{name: sub.Input().String(), at: position(sub.At()), declaration: position(sub.Declaration()), origin: origin, valueSource: position(sub.ValueSource())}
		for _, ref := range sub.ReferenceChain() {
			kind := ReferenceInvalid
			switch ref.Kind {
			case parser.ReferenceInclude:
				kind = ReferenceInclude
			case parser.ReferenceAlias:
				kind = ReferenceAlias
			}
			use.references = append(use.references, Reference{Kind: kind, At: position(ref.At)})
		}
		source.inputs = append(source.inputs, use)
	}
	return source
}

func plain(node parser.Node) (string, bool) {
	scalar, ok := node.Scalar()
	return scalar.Text(), ok && scalar.Kind() == parser.ScalarString
}
func containsTemplateSyntax(value string) bool {
	return strings.Contains(value, "{{") || strings.Contains(value, "{%") || strings.Contains(value, "{#")
}
func scalarBool(node parser.Node) (bool, bool) {
	scalar, ok := node.Scalar()
	if !ok || scalar.Kind() != parser.ScalarBool {
		return false, false
	}
	return strings.EqualFold(scalar.Text(), "true"), true
}
func scalarInt(node parser.Node) (int64, bool) {
	scalar, ok := node.Scalar()
	if !ok || scalar.Kind() != parser.ScalarInteger {
		return 0, false
	}
	value, err := strconv.ParseInt(scalar.Text(), 10, 64)
	return value, err == nil
}
func scalarNumber(node parser.Node) (float64, bool) {
	scalar, ok := node.Scalar()
	if !ok {
		return 0, false
	}
	if scalar.Kind() != parser.ScalarInteger && scalar.Kind() != parser.ScalarFloat {
		return 0, false
	}
	value, err := strconv.ParseFloat(scalar.Text(), 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}
func (c *semanticCompiler) mapping(node parser.Node, allowed ...string) Diagnostic {
	if node.Kind() != parser.KindMapping {
		return c.fail(CodeUnsupportedForm, "", node, "mapping required")
	}
	for _, entry := range node.Entries() {
		found := false
		for _, key := range allowed {
			if entry.Key == key {
				found = true
				break
			}
		}
		if !found {
			return c.failKey(CodeUnknownField, entry, "field is outside the semantic profile")
		}
	}
	return Diagnostic{}
}
func (c *semanticCompiler) required(node parser.Node, key string) (parser.Node, Diagnostic) {
	value, ok := node.Lookup(key)
	if !ok {
		return parser.Node{}, c.fail(CodeInvalidValue, key, node, "required field is absent")
	}
	return value, Diagnostic{}
}
func (c *semanticCompiler) stringField(node parser.Node, key string, required bool) (string, Diagnostic) {
	value, ok := node.Lookup(key)
	if !ok {
		if required {
			return "", c.fail(CodeInvalidValue, key, node, "required field is absent")
		}
		return "", Diagnostic{}
	}
	text, valid := plain(value)
	if !valid || (required && !validName(text)) {
		return "", c.fail(CodeInvalidValue, key, value, "string value is invalid")
	}
	return text, Diagnostic{}
}
func (c *semanticCompiler) need(requirement Requirement) {
	for _, v := range c.requirements {
		if v == requirement {
			return
		}
	}
	c.requirements = append(c.requirements, requirement)
}
func (c *semanticCompiler) requireEntity(id string, support DeclaredSupport, node parser.Node) (domain.HAAlias, Diagnostic) {
	alias, err := domain.ParseHAAlias(id)
	if err != nil {
		return domain.HAAlias{}, c.fail(CodeInvalidValue, "entity_id", node, "invalid HA entity alias")
	}
	entity, ok := c.catalogue.entity(alias)
	if !ok {
		return domain.HAAlias{}, c.fail(CodeUnknownReference, "entity_id", node, fmt.Sprintf("entity %q is not in the supplied catalogue", id))
	}
	if entity.status == ReferenceExcluded {
		return domain.HAAlias{}, c.fail(CodeExcludedReference, "entity_id", node, fmt.Sprintf("entity %q is excluded", id))
	}
	if !entity.has(support) {
		return domain.HAAlias{}, c.fail(CodeMissingCapability, "entity_id", node, fmt.Sprintf("entity %q lacks declared support %d", id, support))
	}
	return alias, Diagnostic{}
}
func (c *semanticCompiler) requireTargetCapability(target Target, support DeclaredSupport, field string) Diagnostic {
	entity, ok := c.catalogue.entity(target.alias)
	if !ok || entity.status != ReferencePresent {
		return Diagnostic{code: CodeUnknownReference, object: c.object, field: field, source: target.source.clone(), reason: "target reference is not present"}
	}
	if !entity.has(support) {
		id, _ := target.alias.Value()
		return Diagnostic{code: CodeMissingCapability, object: c.object, field: field, source: target.source.clone(), reason: fmt.Sprintf("entity %q lacks declared support %d", id, support)}
	}
	return Diagnostic{}
}
func (c *semanticCompiler) requireDevice(id string, node parser.Node) (HADeviceRef, Diagnostic) {
	ref, err := ParseHADeviceRef(id)
	if err != nil {
		return HADeviceRef{}, c.fail(CodeInvalidValue, "device_id", node, "invalid HA device reference")
	}
	device, ok := c.catalogue.device(ref)
	if !ok {
		return HADeviceRef{}, c.fail(CodeUnknownReference, "device_id", node, fmt.Sprintf("device %q is not in the supplied catalogue", id))
	}
	if device.status == ReferenceExcluded {
		return HADeviceRef{}, c.fail(CodeExcludedReference, "device_id", node, fmt.Sprintf("device %q is excluded", id))
	}
	return ref, Diagnostic{}
}
func (c *semanticCompiler) requireEvent(name string, node parser.Node) (EventType, Diagnostic) {
	eventType, err := ParseEventType(name)
	if err != nil {
		return EventType{}, c.fail(CodeInvalidValue, "event_type", node, "invalid HA event type")
	}
	event, ok := c.catalogue.event(eventType)
	if !ok {
		return EventType{}, c.fail(CodeUnknownReference, "event_type", node, fmt.Sprintf("event %q is not in the supplied catalogue", name))
	}
	if event.status == ReferenceExcluded {
		return EventType{}, c.fail(CodeExcludedReference, "event_type", node, fmt.Sprintf("event %q is excluded", name))
	}
	return eventType, Diagnostic{}
}
