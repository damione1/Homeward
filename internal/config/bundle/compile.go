package bundle

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

// Compile admits a complete declarative candidate. It reads only under the
// supplied bundle root and never changes an active generation.
func Compile(bundleRoot, entry string) (Candidate, error) {
	doc, err := parser.LoadProfile(bundleRoot, entry, parser.SyntaxProfileDeclarative)
	if err != nil {
		return Candidate{}, err
	}
	return admit(doc, filepath.Clean(entry))
}

// CompileSources reuses the same admission path at a persisted-source restore
// boundary. Operational reads of the returned Candidate never reparse YAML.
func CompileSources(entry string, sources []parser.SourceFile) (Candidate, error) {
	doc, err := parser.LoadSources(entry, sources, parser.SyntaxProfileDeclarative)
	if err != nil {
		return Candidate{}, err
	}
	return admit(doc, filepath.Clean(entry))
}

type groupRef struct {
	target domain.HAAlias
	at     parser.Node
}
type pendingGroup struct {
	group Group
	refs  []groupRef
}

func admit(doc *parser.Document, entry string) (Candidate, error) {
	if doc == nil || doc.Profile() != parser.DeclarativeProfile {
		return Candidate{}, ErrInvalidCandidate
	}
	root := doc.Root()
	if root.Kind() != parser.KindMapping {
		return Candidate{}, diagnostic(CodeInvalidValue, root, "configuration must be a mapping")
	}
	c := Candidate{profile: parser.SyntaxProfileDeclarative, schema: SchemaDeclarative1, entry: entry}
	var pending []pendingGroup
	for _, family := range root.Entries() {
		switch parser.FamilyFromKey(family.Key) {
		case parser.FamilyInputBoolean:
			for _, item := range family.Value.Entries() {
				h, err := admitBool(item)
				if err != nil {
					return Candidate{}, err
				}
				c.booleans = append(c.booleans, h)
			}
		case parser.FamilyInputNumber:
			for _, item := range family.Value.Entries() {
				h, err := admitNumber(item)
				if err != nil {
					return Candidate{}, err
				}
				c.numbers = append(c.numbers, h)
			}
		case parser.FamilyCounter:
			for _, item := range family.Value.Entries() {
				h, err := admitCounter(item)
				if err != nil {
					return Candidate{}, err
				}
				c.counters = append(c.counters, h)
			}
		case parser.FamilyGroup:
			for _, item := range family.Value.Entries() {
				g, err := admitGroup(item)
				if err != nil {
					return Candidate{}, err
				}
				pending = append(pending, g)
			}
		default:
			return Candidate{}, &Diagnostic{Code: CodeUnsupportedCategory, At: family.KeySource, Path: family.Value.Path(), Message: "category has no admitted business schema"}
		}
	}
	if err := resolveGroups(&c, pending); err != nil {
		return Candidate{}, err
	}
	c.sources = doc.Sources()
	c.hash = manifestHash(c.profile, c.schema, entry, c.sources)
	c.valid = true
	return c, nil
}

func alias(family, slug string, at parser.Node) (domain.HAAlias, error) {
	id, err := domain.ParseHAAlias(family + "." + slug)
	if err != nil {
		return domain.HAAlias{}, diagnostic(CodeInvalidValue, at, "invalid HA declaration alias")
	}
	return id, nil
}

func textValue(n parser.Node) (Text, error) {
	if key, ok := n.SecretKey(); ok {
		return Text{kind: TextSecret, secret: SecretRef{key: key}}, nil
	}
	s, ok := n.Scalar()
	if !ok || s.Kind() != parser.ScalarString {
		return Text{}, diagnostic(CodeInvalidValue, n, "expected literal text or opaque secret handle")
	}
	return Text{kind: TextLiteral, literal: s.Text()}, nil
}

func literalTextValue(n parser.Node) (Text, error) {
	value, err := textValue(n)
	if err != nil {
		return Text{}, err
	}
	if value.Kind() != TextLiteral {
		return Text{}, diagnostic(CodeInvalidValue, n, "field requires literal text")
	}
	return value, nil
}

func boolValue(n parser.Node) (bool, error) {
	s, ok := n.Scalar()
	if !ok || s.Kind() != parser.ScalarBool {
		return false, diagnostic(CodeInvalidValue, n, "expected boolean")
	}
	switch s.Text() {
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	default:
		return false, diagnostic(CodeInvalidValue, n, "invalid boolean lexeme")
	}
}

func intValue(n parser.Node) (int64, error) {
	s, ok := n.Scalar()
	if !ok || s.Kind() != parser.ScalarInteger {
		return 0, diagnostic(CodeInvalidValue, n, "expected decimal integer")
	}
	v, err := decimalInteger(s.Text())
	if err != nil {
		return 0, diagnostic(CodeInvalidValue, n, "invalid decimal integer")
	}
	return v, nil
}

func decimalInteger(text string) (int64, error) {
	digits := text
	if strings.HasPrefix(digits, "+") || strings.HasPrefix(digits, "-") {
		digits = digits[1:]
	}
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return 0, strconv.ErrSyntax
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	return strconv.ParseInt(text, 10, 64)
}

// This is the admitted decimal spelling for an explicitly tagged YAML float.
// strconv.ParseFloat alone also accepts Go hex floats and separators.
var decimalFloatPattern = regexp.MustCompile(`^[+-]?(?:(?:0|[1-9][0-9]*)(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func numberValue(n parser.Node) (Number, error) {
	s, ok := n.Scalar()
	if !ok || (s.Kind() != parser.ScalarInteger && s.Kind() != parser.ScalarFloat) {
		return Number{}, diagnostic(CodeInvalidValue, n, "expected decimal number")
	}
	var v float64
	var err error
	if s.Kind() == parser.ScalarInteger {
		var integer int64
		integer, err = decimalInteger(s.Text())
		if integer > 1<<53 || integer < -(1<<53) {
			return Number{}, diagnostic(CodeInvalidValue, n, "integer exceeds exact numeric range")
		}
		v = float64(integer)
	} else {
		if !decimalFloatPattern.MatchString(s.Text()) {
			return Number{}, diagnostic(CodeInvalidValue, n, "invalid decimal float")
		}
		v, err = strconv.ParseFloat(s.Text(), 64)
	}
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return Number{}, diagnostic(CodeInvalidValue, n, "invalid finite decimal number")
	}
	return Number{value: v, valid: true}, nil
}

func admitBool(item parser.Entry) (BoolHelper, error) {
	id, err := alias(parser.FamilyInputBoolean.Key(), item.Key, item.Value)
	if err != nil {
		return BoolHelper{}, err
	}
	h := BoolHelper{id: id}
	for _, field := range item.Value.Entries() {
		switch parser.DeclarativeFieldFromKey(field.Key) {
		case parser.FieldInitial:
			h.initial, err = boolValue(field.Value)
			h.hasInitial = err == nil
		case parser.FieldName:
			h.name, err = textValue(field.Value)
		case parser.FieldIcon:
			h.icon, err = textValue(field.Value)
		default:
			return BoolHelper{}, diagnostic(CodeInvalidValue, field.Value, "unsupported boolean helper field")
		}
		if err != nil {
			return BoolHelper{}, err
		}
	}
	h.valid = true
	return h, nil
}

func admitNumber(item parser.Entry) (NumberHelper, error) {
	id, err := alias(parser.FamilyInputNumber.Key(), item.Key, item.Value)
	if err != nil {
		return NumberHelper{}, err
	}
	h := NumberHelper{id: id, step: Number{value: 1, valid: true}, mode: NumberModeSlider}
	stepAt := item.Value
	for _, field := range item.Value.Entries() {
		switch parser.DeclarativeFieldFromKey(field.Key) {
		case parser.FieldMin:
			h.min, err = numberValue(field.Value)
		case parser.FieldMax:
			h.max, err = numberValue(field.Value)
		case parser.FieldStep:
			h.step, err = numberValue(field.Value)
			stepAt = field.Value
		case parser.FieldInitial:
			h.initial, err = numberValue(field.Value)
			h.hasInitial = err == nil
		case parser.FieldName:
			h.name, err = textValue(field.Value)
		case parser.FieldIcon:
			h.icon, err = textValue(field.Value)
		case parser.FieldUnitOfMeasurement:
			h.unit, err = literalTextValue(field.Value)
		case parser.FieldMode:
			var s parser.Scalar
			var ok bool
			s, ok = field.Value.Scalar()
			if !ok || s.Kind() != parser.ScalarString {
				return NumberHelper{}, diagnostic(CodeInvalidValue, field.Value, "invalid number mode")
			}
			switch s.Text() {
			case "slider":
				h.mode = NumberModeSlider
			case "box":
				h.mode = NumberModeBox
			default:
				return NumberHelper{}, diagnostic(CodeInvalidValue, field.Value, "unknown number mode")
			}
		default:
			return NumberHelper{}, diagnostic(CodeInvalidValue, field.Value, "unsupported number helper field")
		}
		if err != nil {
			return NumberHelper{}, err
		}
	}
	if !h.min.Valid() || !h.max.Valid() {
		return NumberHelper{}, &Diagnostic{Code: CodeMissingField, At: item.KeySource, Path: item.Value.Path(), Message: "number helper requires min and max"}
	}
	if h.min.value > h.max.value {
		return NumberHelper{}, diagnostic(CodeInvalidValue, item.Value, "number bounds are reversed")
	}
	if h.step.value < .001 {
		return NumberHelper{}, diagnostic(CodeInvalidValue, stepAt, "number step must be at least 0.001")
	}
	if h.hasInitial && (h.initial.value < h.min.value || h.initial.value > h.max.value) {
		return NumberHelper{}, diagnostic(CodeInvalidValue, item.Value, "number initial is outside bounds")
	}
	h.valid = true
	return h, nil
}

func admitCounter(item parser.Entry) (CounterHelper, error) {
	id, err := alias(parser.FamilyCounter.Key(), item.Key, item.Value)
	if err != nil {
		return CounterHelper{}, err
	}
	h := CounterHelper{id: id, step: 1}
	stepAt := item.Value
	for _, field := range item.Value.Entries() {
		switch parser.DeclarativeFieldFromKey(field.Key) {
		case parser.FieldInitial:
			h.initial, err = intValue(field.Value)
			h.hasInitial = err == nil
		case parser.FieldMinimum:
			h.minimum, err = intValue(field.Value)
			h.hasMinimum = err == nil
		case parser.FieldMaximum:
			h.maximum, err = intValue(field.Value)
			h.hasMaximum = err == nil
		case parser.FieldStep:
			h.step, err = intValue(field.Value)
			stepAt = field.Value
		case parser.FieldName:
			h.name, err = textValue(field.Value)
		case parser.FieldIcon:
			h.icon, err = textValue(field.Value)
		default:
			return CounterHelper{}, diagnostic(CodeInvalidValue, field.Value, "unsupported counter field")
		}
		if err != nil {
			return CounterHelper{}, err
		}
	}
	if h.step <= 0 {
		return CounterHelper{}, diagnostic(CodeInvalidValue, stepAt, "counter step must be positive")
	}
	if h.hasMinimum && h.hasMaximum && h.minimum > h.maximum {
		return CounterHelper{}, diagnostic(CodeInvalidValue, item.Value, "counter bounds are reversed")
	}
	if h.hasInitial && ((h.hasMinimum && h.initial < h.minimum) || (h.hasMaximum && h.initial > h.maximum)) {
		return CounterHelper{}, diagnostic(CodeInvalidValue, item.Value, "counter initial is outside bounds")
	}
	h.valid = true
	return h, nil
}

func admitGroup(item parser.Entry) (pendingGroup, error) {
	id, err := alias(parser.FamilyGroup.Key(), item.Key, item.Value)
	if err != nil {
		return pendingGroup{}, err
	}
	g := pendingGroup{group: Group{id: id}}
	var hasEntities bool
	for _, field := range item.Value.Entries() {
		switch parser.DeclarativeFieldFromKey(field.Key) {
		case parser.FieldName:
			g.group.name, err = textValue(field.Value)
		case parser.FieldIcon:
			g.group.icon, err = textValue(field.Value)
		case parser.FieldAll:
			g.group.all, err = boolValue(field.Value)
		case parser.FieldEntities:
			hasEntities = true
			for _, member := range field.Value.Items() {
				s, ok := member.Scalar()
				if !ok || s.Kind() != parser.ScalarString {
					return pendingGroup{}, diagnostic(CodeInvalidValue, member, "group member must be an HA alias")
				}
				ref, parseErr := domain.ParseHAAlias(s.Text())
				if parseErr != nil {
					return pendingGroup{}, diagnostic(CodeInvalidValue, member, "invalid group member alias")
				}
				g.refs = append(g.refs, groupRef{target: ref, at: member})
			}
		default:
			return pendingGroup{}, diagnostic(CodeInvalidValue, field.Value, "unsupported group field")
		}
		if err != nil {
			return pendingGroup{}, err
		}
	}
	if !hasEntities || len(g.refs) == 0 {
		return pendingGroup{}, diagnostic(CodeMissingField, item.Value, "group requires a nonempty entities list")
	}
	return g, nil
}

func resolveGroups(c *Candidate, pending []pendingGroup) error {
	declared := make(map[string]struct{}, len(c.booleans)+len(pending))
	groupIndex := make(map[string]int, len(pending))
	for _, h := range c.booleans {
		id, _ := h.id.Value()
		declared[id] = struct{}{}
	}
	for i, g := range pending {
		id, _ := g.group.id.Value()
		declared[id] = struct{}{}
		groupIndex[id] = i
	}
	for i := range pending {
		seen := make(map[string]bool, len(pending[i].refs))
		for _, ref := range pending[i].refs {
			id, _ := ref.target.Value()
			if _, ok := declared[id]; !ok {
				return diagnostic(CodeMissingReference, ref.at, "group member is absent from candidate")
			}
			if seen[id] {
				return diagnostic(CodeDuplicateReference, ref.at, "duplicate group member")
			}
			seen[id] = true
			pending[i].group.members = append(pending[i].group.members, ref.target)
		}
	}
	type visitState uint8
	const (
		visitUnseen visitState = iota
		visitActive
		visitDone
	)
	state := make([]visitState, len(pending))
	var visit func(int) error
	visit = func(i int) error {
		state[i] = visitActive
		for _, ref := range pending[i].refs {
			id, _ := ref.target.Value()
			j, ok := groupIndex[id]
			if !ok {
				continue
			}
			if state[j] == visitActive {
				return diagnostic(CodeReferenceCycle, ref.at, "group reference cycle")
			}
			if state[j] == visitUnseen {
				if err := visit(j); err != nil {
					return err
				}
			}
		}
		state[i] = visitDone
		return nil
	}
	for i := range pending {
		if state[i] == visitUnseen {
			if err := visit(i); err != nil {
				return err
			}
		}
	}
	for _, g := range pending {
		g.group.valid = true
		c.groups = append(c.groups, g.group)
	}
	return nil
}

func manifestHash(profile parser.SyntaxProfile, schema Schema, entry string, files []parser.SourceFile) [sha256.Size]byte {
	h := sha256.New()
	writePart(h, "homeward-candidate-v1")
	writePart(h, profile.String())
	writePart(h, schema.String())
	writePart(h, entry)
	sorted := append([]parser.SourceFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path() < sorted[j].Path() })
	for _, file := range sorted {
		writePart(h, file.Path())
		sum := file.SHA256()
		_, _ = h.Write(sum[:])
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func writePart(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write([]byte(value))
}
