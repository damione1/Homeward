package parser

// Rule references (original Go implementation, no upstream code vendored):
// HA Core 2026.10.1, Apache-2.0, abe1fdd7cd6b4de1715c87b6fd87d92699514ef9,
// components/blueprint/{models,schemas}.py: flat sections, key-presence defaults,
// declared references, and invocation overlay. annotatedyaml 1.0.2, Apache-2.0,
// 1b70ae7b069b10da27ad46f1ba20c870d8f0d772, src/annotatedyaml/input.py:
// structural single-pass substitution. Homeward deliberately admits stricter
// selector values and rejects references inside supplied/default input values.

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExpandBlueprint opens only the requested invocation and its local definition
// through the common confined loader. Other blueprints are never enumerated.
func ExpandBlueprint(bundleRoot, invocationPath string) (*ExpandedBlueprint, error) {
	if !safeName(invocationPath) {
		return nil, diagnostic(CodeInvalidEntry, Position{File: invocationPath, Line: 1, Column: 1}, "$", "entry must be a local YAML file")
	}
	root, err := os.OpenRoot(bundleRoot)
	if err != nil {
		return nil, diagnostic(CodeReadFile, Position{File: invocationPath, Line: 1, Column: 1}, "$", "bundle root cannot be opened")
	}
	defer root.Close()
	l := &loader{root: root, sources: make(map[string]SourceFile), profile: SyntaxProfileBlueprintInstance, active: make(map[string]bool)}
	return l.expandBlueprint(invocationPath)
}

// ExpandBlueprintSources restores from detached exact bytes, without any
// filesystem access. Unused valid snapshots are inert and not retained.
func ExpandBlueprintSources(invocationPath string, files []SourceFile) (*ExpandedBlueprint, error) {
	l, err := snapshotLoader(invocationPath, files, SyntaxProfileBlueprintInstance)
	if err != nil {
		return nil, err
	}
	// Even unused snapshots occupy memory until expansion returns. Charge all
	// of them once, so inert files cannot subsidize a second output budget.
	for _, file := range files {
		l.retained += int64(len(file.data) + len(file.path) + 128)
	}
	l.prechargedSources = true
	return l.expandBlueprint(invocationPath)
}

type inputDefinition struct {
	name         InputName
	selector     SelectorKind
	declaration  Node
	defaultValue Node
	hasDefault   bool
	numberMin    float64
	numberMax    float64
	hasMin       bool
	hasMax       bool
}

func (l *loader) expandBlueprint(entry string) (*ExpandedBlueprint, error) {
	invocation, err := l.load(entry)
	if err != nil {
		return nil, err
	}
	use, ok := invocation.root.Lookup("use_blueprint")
	if !ok {
		return nil, diagnostic(CodeBlueprintDefinition, invocation.root.source, "$", "blueprint invocation is required")
	}
	pathNode, ok := use.Lookup("path")
	if !ok {
		return nil, diagnostic(CodeBlueprintDefinition, use.source, use.path, "blueprint path is required")
	}
	path, ok := plainString(pathNode)
	if !ok || !safeName(path) || filepath.Clean(path) != path || filepath.Ext(path) != ".yaml" || strings.ContainsAny(path, ":\\") {
		return nil, diagnostic(CodeIncludeOutside, pathNode.source, pathNode.path, "blueprint path must be a canonical local .yaml path")
	}
	definitionPath := filepath.Join("blueprints", "automation", path)
	if len(definitionPath) > maxPathBytes {
		return nil, diagnostic(CodeSyntaxLimit, pathNode.source, pathNode.path, "blueprint path limit exceeded")
	}
	values, hasValues := use.Lookup("input")
	if !hasValues {
		values = Node{kind: KindMapping, source: use.source, path: use.path + ".input"}
	}
	l.profile = SyntaxProfileBlueprintDefinition
	var definitionFiles []string
	l.trace = &definitionFiles
	definition, err := l.load(definitionPath)
	l.trace = nil
	if err != nil {
		return nil, err
	}
	defs, d := readInputDefinitions(definition.root)
	if d != nil {
		return nil, d
	}
	byName := make(map[InputName]int, len(defs))
	for i, def := range defs {
		byName[def.name] = i
	}
	for _, value := range values.entries {
		if _, exists := byName[InputName{value: value.Key}]; !exists {
			return nil, inputError(CodeBlueprintInputUnknown, value.Value, value.Key, "input is not declared")
		}
	}
	if d = checkBodyReferences(definition.root, byName, true); d != nil {
		return nil, d
	}
	effective := make([]EffectiveInput, 0, len(defs))
	for _, def := range defs {
		value, present := values.Lookup(def.name.value)
		origin := InputOriginInvocation
		if !present {
			value, present = def.defaultValue, def.hasDefault
			origin = InputOriginDefault
		}
		if !present {
			return nil, inputError(CodeBlueprintInputMissing, def.declaration, def.name.value, "required input is missing")
		}
		if d = terminalInput(value, def.name); d != nil {
			return nil, d
		}
		if d = validateInputValue(value, def); d != nil {
			return nil, d
		}
		effective = append(effective, EffectiveInput{name: def.name, selector: def.selector, origin: origin, value: value, declaration: def.declaration.source})
	}
	expanded, d := l.copyExpanded(definition.root, "$", effective, byName, nil, 0, true)
	if d != nil {
		return nil, d
	}
	// HA overlays the invocation after substitution. Restrict its admitted keys
	// before this merge, so nothing is silently ignored or dropped.
	for _, entry := range invocation.root.entries {
		if entry.Key == "use_blueprint" {
			continue
		}
		value, diag := l.copyExpanded(entry.Value, "$."+entry.Key, effective, byName, nil, 1, false)
		if diag != nil {
			return nil, diag
		}
		replacement := Entry{Key: entry.Key, KeySource: entry.KeySource, Value: value}
		replaced := false
		for i := range expanded.entries {
			if expanded.entries[i].Key == entry.Key {
				expanded.entries[i] = replacement
				replaced = true
				break
			}
		}
		if !replaced {
			expanded.entries = append(expanded.entries, replacement)
		}
	}
	if d = validate(expanded, blueprintBodyRule(false)); d != nil {
		annotateSubstitutionError(d, expanded)
		return nil, d
	}
	sources := make([]SourceFile, 0, len(l.used))
	for _, name := range l.used {
		sources = append(sources, l.sources[name])
	}
	definitionHash := sha256.New()
	for _, name := range definitionFiles {
		writeHashString(definitionHash, name)
		sum := l.sources[name].hash
		_, _ = definitionHash.Write(sum[:])
	}
	var definitionSum [sha256.Size]byte
	copy(definitionSum[:], definitionHash.Sum(nil))
	return &ExpandedBlueprint{root: expanded, sources: sources, blueprintPath: definitionPath, blueprintHash: l.sources[definitionPath].hash, definitionSourcesHash: definitionSum, inputsHash: hashInputs(effective), inputs: effective}, nil
}

func plainString(n Node) (string, bool) {
	return n.scalar.text, n.kind == KindScalar && n.scalar.kind == ScalarString
}

func inputError(code Code, n Node, name, message string) *Diagnostic {
	return diagnostic(code, n.source, n.path, fmt.Sprintf("input %q: %s", name, message))
}

func annotateSubstitutionError(d *Diagnostic, n Node) bool {
	if n.path == d.Path && len(n.substitutions) != 0 {
		d.Message = fmt.Sprintf("input %q: %s", n.substitutions[0].name.value, d.Message)
		return true
	}
	for _, entry := range n.entries {
		if annotateSubstitutionError(d, entry.Value) {
			return true
		}
	}
	for _, item := range n.items {
		if annotateSubstitutionError(d, item) {
			return true
		}
	}
	return false
}

func readInputDefinitions(root Node) ([]inputDefinition, *Diagnostic) {
	meta, ok := root.Lookup("blueprint")
	if !ok {
		return nil, diagnostic(CodeBlueprintDefinition, root.source, root.path, "blueprint metadata is required")
	}
	for _, key := range []string{"name", "domain"} {
		value, present := meta.Lookup(key)
		text, isString := plainString(value)
		if !present || !isString || strings.TrimSpace(text) == "" {
			return nil, diagnostic(CodeBlueprintDefinition, meta.source, meta.path, "blueprint name and automation domain are required strings")
		}
	}
	inputs, present := meta.Lookup("input")
	if !present {
		return nil, nil
	}
	var result []inputDefinition
	seen := make(map[InputName]bool)
	add := func(entry Entry) *Diagnostic {
		name := InputName{value: entry.Key}
		if !name.Valid() {
			return inputError(CodeBlueprintDefinition, entry.Value, entry.Key, "invalid input name")
		}
		if seen[name] {
			return inputError(CodeBlueprintInputCollision, entry.Value, entry.Key, "input collides across sections")
		}
		if len(result) >= 256 {
			return inputError(CodeSyntaxLimit, entry.Value, entry.Key, "input declaration limit exceeded")
		}
		def, d := readInputDefinition(name, entry.Value)
		if d != nil {
			return d
		}
		seen[name] = true
		result = append(result, def)
		return nil
	}
	for _, entry := range inputs.entries {
		section, hasSection := entry.Value.Lookup("input")
		if hasSection {
			for _, field := range entry.Value.entries {
				switch field.Key {
				case "name", "description", "icon", "input":
				case "collapsed":
					if field.Value.kind != KindScalar || field.Value.scalar.kind != ScalarBool {
						return nil, inputError(CodeBlueprintDefinition, field.Value, entry.Key, "section collapsed must be a boolean")
					}
				default:
					return nil, inputError(CodeBlueprintDefinition, field.Value, entry.Key, "input section mixes leaf fields")
				}
			}
			for _, leaf := range section.entries {
				if d := add(leaf); d != nil {
					return nil, d
				}
			}
		} else {
			for _, field := range entry.Value.entries {
				if field.Key == "icon" || field.Key == "collapsed" {
					return nil, inputError(CodeBlueprintDefinition, field.Value, entry.Key, "section field on input")
				}
			}
			if d := add(entry); d != nil {
				return nil, d
			}
		}
	}
	return result, nil
}

func readInputDefinition(name InputName, n Node) (inputDefinition, *Diagnostic) {
	def := inputDefinition{name: name, declaration: n}
	def.defaultValue, def.hasDefault = n.Lookup("default")
	if def.hasDefault {
		if d := terminalInput(def.defaultValue, name); d != nil {
			return def, d
		}
	}
	selector, present := n.Lookup("selector")
	if !present || len(selector.entries) != 1 {
		return def, inputError(CodeBlueprintDefinition, n, name.value, "exactly one qualified selector is required")
	}
	selection := selector.entries[0]
	def.selector = selectorFromKey(selection.Key)
	switch def.selector {
	case SelectorDevice:
		if multiple, ok := selection.Value.Lookup("multiple"); ok {
			if multiple.kind != KindScalar || multiple.scalar.kind != ScalarBool || !strings.EqualFold(multiple.scalar.text, "false") {
				return def, inputError(CodeBlueprintDefinition, multiple, name.value, "only single-device selectors are qualified")
			}
		}
	case SelectorAction, SelectorBoolean:
	case SelectorNumber:
		for _, field := range selection.Value.entries {
			switch field.Key {
			case "min", "max", "step":
				number, ok := syntaxNumber(field.Value)
				if !ok || (field.Key == "step" && number < 0.001) {
					return def, inputError(CodeBlueprintDefinition, field.Value, name.value, "selector numeric constraint is invalid")
				}
				switch field.Key {
				case "min":
					def.numberMin, def.hasMin = number, true
				case "max":
					def.numberMax, def.hasMax = number, true
				case "step":
				}
			case "mode":
				mode, ok := plainString(field.Value)
				if !ok || (mode != "slider" && mode != "box") {
					return def, inputError(CodeBlueprintDefinition, field.Value, name.value, "selector mode is invalid")
				}
			case "unit_of_measurement":
				if _, ok := plainString(field.Value); !ok {
					return def, inputError(CodeBlueprintDefinition, field.Value, name.value, "selector unit must be a string")
				}
			default:
				return def, inputError(CodeBlueprintDefinition, field.Value, name.value, "selector field is not qualified")
			}
		}
		if def.hasMin && def.hasMax && def.numberMin > def.numberMax {
			return def, inputError(CodeBlueprintDefinition, n, name.value, "selector minimum exceeds maximum")
		}
		if mode, ok := selection.Value.Lookup("mode"); ok && mode.scalar.text == "slider" && (!def.hasMin || !def.hasMax) {
			return def, inputError(CodeBlueprintDefinition, mode, name.value, "slider requires minimum and maximum")
		}
	case SelectorInvalid:
		return def, inputError(CodeBlueprintDefinition, selector, name.value, "selector is not qualified")
	default:
		return def, inputError(CodeBlueprintDefinition, selector, name.value, "unknown selector kind")
	}
	return def, nil
}

func syntaxNumber(n Node) (float64, bool) {
	if n.kind != KindScalar {
		return 0, false
	}
	text := strings.ReplaceAll(n.scalar.text, "_", "")
	var value float64
	var err error
	switch n.scalar.kind {
	case ScalarInteger:
		var integer int64
		integer, err = strconv.ParseInt(text, 0, 64)
		if err != nil {
			var unsigned uint64
			unsigned, err = strconv.ParseUint(text, 0, 64)
			value = float64(unsigned)
		} else {
			value = float64(integer)
		}
	case ScalarFloat:
		value, err = strconv.ParseFloat(text, 64)
	case ScalarInvalid, ScalarString, ScalarBool, ScalarNull:
		return 0, false
	default:
		return 0, false
	}
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateInputValue(n Node, def inputDefinition) *Diagnostic {
	valid := false
	switch def.selector {
	case SelectorDevice:
		text, ok := plainString(n)
		valid = ok && strings.TrimSpace(text) != "" && strings.TrimSpace(text) == text
	case SelectorBoolean:
		valid = n.kind == KindScalar && n.scalar.kind == ScalarBool && (strings.EqualFold(n.scalar.text, "true") || strings.EqualFold(n.scalar.text, "false"))
	case SelectorNumber:
		number, ok := syntaxNumber(n)
		valid = ok && (!def.hasMin || number >= def.numberMin) && (!def.hasMax || number <= def.numberMax)
	case SelectorAction:
		if n.kind != KindSequence {
			return inputError(CodeBlueprintInputType, n, def.name.value, "value does not match action selector")
		}
		body := blueprintBodyRule(false)
		if d := validate(n, body.fields["actions"]); d != nil {
			d.Message = fmt.Sprintf("input %q: %s", def.name.value, d.Message)
			return d
		}
		valid = true
	case SelectorInvalid:
		return inputError(CodeBlueprintDefinition, n, def.name.value, "uninitialized selector")
	default:
		return inputError(CodeBlueprintDefinition, n, def.name.value, "unknown selector")
	}
	if !valid {
		return inputError(CodeBlueprintInputType, n, def.name.value, "value does not match "+def.selector.String()+" selector")
	}
	return nil
}

func terminalInput(n Node, name InputName) *Diagnostic {
	if n.kind == KindInputRef {
		return inputError(CodeBlueprintInputReference, n, name.value, "references inside metadata, defaults or supplied values are outside the single-pass profile")
	}
	for _, entry := range n.entries {
		if d := terminalInput(entry.Value, name); d != nil {
			return d
		}
	}
	for _, item := range n.items {
		if d := terminalInput(item, name); d != nil {
			return d
		}
	}
	return nil
}

func checkBodyReferences(n Node, defs map[InputName]int, root bool) *Diagnostic {
	if n.kind == KindInputRef {
		name := InputName{value: n.scalar.text}
		if _, ok := defs[name]; !ok {
			return inputError(CodeBlueprintInputUnknown, n, name.value, "reference has no declaration")
		}
	}
	for _, entry := range n.entries {
		if root && entry.Key == "blueprint" {
			continue
		}
		if d := checkBodyReferences(entry.Value, defs, false); d != nil {
			return d
		}
	}
	for _, item := range n.items {
		if d := checkBodyReferences(item, defs, false); d != nil {
			return d
		}
	}
	return nil
}

func (l *loader) copyExpanded(n Node, path string, inputs []EffectiveInput, defs map[InputName]int, sub *Substitution, depth int, removeMetadata bool) (Node, *Diagnostic) {
	switch n.kind {
	case KindInputRef:
		index, exists := defs[InputName{value: n.scalar.text}]
		if !exists || index >= len(inputs) {
			return Node{}, inputError(CodeBlueprintInputUnknown, n, n.scalar.text, "reference has no effective input")
		}
		input := inputs[index]
		replacement := Substitution{name: input.name, at: n.source, declaration: input.declaration, origin: input.origin, valueSource: input.value.source, references: append([]Reference(nil), n.references...)}
		return l.copyExpanded(input.value, path, inputs, defs, &replacement, depth, false)
	case KindMapping, KindSequence, KindScalar, KindSecretRef:
	case KindInvalid:
		return Node{}, diagnostic(CodeUnsupportedForm, n.source, path, "uninitialized syntax node")
	default:
		return Node{}, diagnostic(CodeUnsupportedForm, n.source, path, "unknown syntax node kind")
	}
	extra := int64(192 + len(path) + len(n.references)*48)
	if sub != nil {
		extra += int64(192 + len(sub.name.value) + len(sub.references)*48)
	}
	if depth > maxSyntaxDepth || len(path) > maxPathBytes || l.nodes >= maxExpandedNodes || l.retained+extra > maxRetainedBytes {
		return Node{}, diagnostic(CodeSyntaxLimit, n.source, path, "blueprint expanded syntax budget exceeded")
	}
	l.nodes++
	l.retained += extra
	out := Node{kind: n.kind, scalar: n.scalar, secretKey: n.secretKey, source: n.source, path: path, references: append([]Reference(nil), n.references...)}
	if sub != nil {
		out.substitutions = []Substitution{*sub}
	}
	for _, entry := range n.entries {
		if removeMetadata && entry.Key == "blueprint" {
			continue
		}
		child, d := l.copyExpanded(entry.Value, path+"."+entry.Key, inputs, defs, sub, depth+1, false)
		if d != nil {
			return Node{}, d
		}
		out.entries = append(out.entries, Entry{Key: entry.Key, KeySource: entry.KeySource, Value: child})
	}
	for i, item := range n.items {
		child, d := l.copyExpanded(item, fmt.Sprintf("%s[%d]", path, i), inputs, defs, sub, depth+1, false)
		if d != nil {
			return Node{}, d
		}
		out.items = append(out.items, child)
	}
	return out, nil
}

func writeHashString(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}
func hashSyntax(h hash.Hash, n Node) {
	_, _ = h.Write([]byte{byte(n.kind), byte(n.scalar.kind)})
	writeHashString(h, n.scalar.text)
	writeHashString(h, n.secretKey)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(n.entries)))
	_, _ = h.Write(count[:])
	for _, entry := range n.entries {
		writeHashString(h, entry.Key)
		hashSyntax(h, entry.Value)
	}
	binary.BigEndian.PutUint64(count[:], uint64(len(n.items)))
	_, _ = h.Write(count[:])
	for _, item := range n.items {
		hashSyntax(h, item)
	}
}
func hashInputs(inputs []EffectiveInput) [sha256.Size]byte {
	h := sha256.New()
	writeHashString(h, BlueprintExpandedProfile)
	for _, input := range inputs {
		writeHashString(h, input.name.value)
		_, _ = h.Write([]byte{byte(input.selector), byte(input.origin)})
		hashSyntax(h, input.value)
	}
	var result [sha256.Size]byte
	copy(result[:], h.Sum(nil))
	return result
}
