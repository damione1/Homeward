// Package bundle admits a small declarative Home Assistant configuration
// profile. It does not activate a generation or execute Home Assistant programs.
package bundle

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

// Schema is the closed set of admitted business schemas. Syntax profiles are
// separately owned by parser; a future schema must enter this admission path.
type Schema uint8

const (
	SchemaInvalid Schema = iota
	SchemaDeclarative1
	SchemaDeclarative2
)

func (s Schema) String() string {
	switch s {
	case SchemaDeclarative1:
		return "ha-declarative-1"
	case SchemaDeclarative2:
		return "ha-declarative-2"
	default:
		return ""
	}
}

// Candidate is a value snapshot. Operational consumers must require Valid()
// before using it; a Go zero value or overwritten value is never admitted.
// Its collections and source bytes are only observable through copies.
type Candidate struct {
	valid     bool
	profile   parser.SyntaxProfile
	schema    Schema
	entry     string
	hash      [sha256.Size]byte
	sources   []parser.SourceFile
	booleans  []BoolHelper
	numbers   []NumberHelper
	counters  []CounterHelper
	buttons   []ButtonHelper
	datetimes []DatetimeHelper
	selects   []SelectHelper
	texts     []TextHelper
	groups    []Group
}

func (c Candidate) Valid() bool {
	return c.valid && ((c.profile == parser.SyntaxProfileDeclarative && c.schema == SchemaDeclarative1) ||
		(c.profile == parser.SyntaxProfileDeclarative2 && c.schema == SchemaDeclarative2))
}
func (c Candidate) Profile() parser.SyntaxProfile {
	if !c.Valid() {
		return parser.SyntaxProfileInvalid
	}
	return c.profile
}
func (c Candidate) Schema() Schema {
	if !c.Valid() {
		return SchemaInvalid
	}
	return c.schema
}
func (c Candidate) Entry() string {
	if !c.Valid() {
		return ""
	}
	return c.entry
}
func (c Candidate) SHA256() [sha256.Size]byte {
	if !c.Valid() {
		return [sha256.Size]byte{}
	}
	return c.hash
}
func (c Candidate) Sources() []parser.SourceFile {
	if !c.Valid() {
		return nil
	}
	return append([]parser.SourceFile(nil), c.sources...)
}
func (c Candidate) Booleans() []BoolHelper {
	if !c.Valid() {
		return nil
	}
	return append([]BoolHelper(nil), c.booleans...)
}
func (c Candidate) Numbers() []NumberHelper {
	if !c.Valid() {
		return nil
	}
	return append([]NumberHelper(nil), c.numbers...)
}
func (c Candidate) Counters() []CounterHelper {
	if !c.Valid() {
		return nil
	}
	return append([]CounterHelper(nil), c.counters...)
}

func (c Candidate) Buttons() []ButtonHelper {
	if !c.Valid() {
		return nil
	}
	return append([]ButtonHelper(nil), c.buttons...)
}
func (c Candidate) Datetimes() []DatetimeHelper {
	if !c.Valid() {
		return nil
	}
	return append([]DatetimeHelper(nil), c.datetimes...)
}
func (c Candidate) Selects() []SelectHelper {
	if !c.Valid() {
		return nil
	}
	out := make([]SelectHelper, len(c.selects))
	for i, value := range c.selects {
		out[i] = value.copy()
	}
	return out
}
func (c Candidate) Texts() []TextHelper {
	if !c.Valid() {
		return nil
	}
	return append([]TextHelper(nil), c.texts...)
}
func (c Candidate) Groups() []Group {
	if !c.Valid() {
		return nil
	}
	return append([]Group(nil), c.groups...)
}

// SecretRef is an opaque handle, never a resolved credential.
type SecretRef struct{ key string }

func (s SecretRef) Valid() bool         { return s.key != "" }
func (s SecretRef) Key() (string, bool) { return s.key, s.key != "" }

type TextKind uint8

const (
	TextInvalid TextKind = iota
	TextLiteral
	TextSecret
)

type Text struct {
	kind    TextKind
	literal string
	secret  SecretRef
}

func (t Text) Valid() bool {
	switch t.kind {
	case TextLiteral:
		return true
	case TextSecret:
		return t.secret.Valid()
	default:
		return false
	}
}
func (t Text) Kind() TextKind            { return t.kind }
func (t Text) Literal() (string, bool)   { return t.literal, t.kind == TextLiteral }
func (t Text) Secret() (SecretRef, bool) { return t.secret, t.kind == TextSecret }

// Number is a finite admitted numeric value. Zero is a valid number only when
// Valid is true; absence is represented at the containing optional field.
type Number struct {
	value float64
	valid bool
}

func (n Number) Valid() bool              { return n.valid && !math.IsNaN(n.value) && !math.IsInf(n.value, 0) }
func (n Number) Float64() (float64, bool) { return n.value, n.Valid() }

type NumberMode uint8

const (
	NumberModeInvalid NumberMode = iota
	NumberModeSlider
	NumberModeBox
)

type BoolHelper struct {
	valid      bool
	id         domain.HAAlias
	name       Text
	icon       Text
	initial    bool
	hasInitial bool
}

func (h BoolHelper) Valid() bool           { return h.valid }
func (h BoolHelper) ID() domain.HAAlias    { return h.id }
func (h BoolHelper) Name() (Text, bool)    { return h.name, h.name.kind != TextInvalid }
func (h BoolHelper) Icon() (Text, bool)    { return h.icon, h.icon.kind != TextInvalid }
func (h BoolHelper) Initial() (bool, bool) { return h.initial, h.hasInitial }

type NumberHelper struct {
	valid      bool
	id         domain.HAAlias
	name       Text
	icon       Text
	unit       Text
	initial    Number
	hasInitial bool
	min        Number
	max        Number
	step       Number
	mode       NumberMode
}

func (h NumberHelper) Valid() bool             { return h.valid }
func (h NumberHelper) ID() domain.HAAlias      { return h.id }
func (h NumberHelper) Name() (Text, bool)      { return h.name, h.name.kind != TextInvalid }
func (h NumberHelper) Icon() (Text, bool)      { return h.icon, h.icon.kind != TextInvalid }
func (h NumberHelper) Unit() (Text, bool)      { return h.unit, h.unit.kind != TextInvalid }
func (h NumberHelper) Initial() (Number, bool) { return h.initial, h.hasInitial }
func (h NumberHelper) Min() Number             { return h.min }
func (h NumberHelper) Max() Number             { return h.max }
func (h NumberHelper) Step() Number            { return h.step }
func (h NumberHelper) Mode() NumberMode        { return h.mode }

type CounterHelper struct {
	valid      bool
	id         domain.HAAlias
	name       Text
	icon       Text
	initial    int64
	hasInitial bool
	minimum    int64
	hasMinimum bool
	maximum    int64
	hasMaximum bool
	step       int64
	restore    bool
}

func (h CounterHelper) Valid() bool            { return h.valid }
func (h CounterHelper) ID() domain.HAAlias     { return h.id }
func (h CounterHelper) Name() (Text, bool)     { return h.name, h.name.kind != TextInvalid }
func (h CounterHelper) Icon() (Text, bool)     { return h.icon, h.icon.kind != TextInvalid }
func (h CounterHelper) Initial() (int64, bool) { return h.initial, h.hasInitial }
func (h CounterHelper) Minimum() (int64, bool) { return h.minimum, h.hasMinimum }
func (h CounterHelper) Maximum() (int64, bool) { return h.maximum, h.hasMaximum }
func (h CounterHelper) Step() int64            { return h.step }
func (h CounterHelper) Restore() bool          { return h.restore }

type Group struct {
	valid   bool
	id      domain.HAAlias
	name    Text
	icon    Text
	all     bool
	members []domain.HAAlias
}

func (g Group) Valid() bool               { return g.valid }
func (g Group) ID() domain.HAAlias        { return g.id }
func (g Group) Name() (Text, bool)        { return g.name, g.name.kind != TextInvalid }
func (g Group) Icon() (Text, bool)        { return g.icon, g.icon.kind != TextInvalid }
func (g Group) All() bool                 { return g.all }
func (g Group) Members() []domain.HAAlias { return append([]domain.HAAlias(nil), g.members...) }

type Code uint8

const (
	CodeInvalid Code = iota
	CodeUnsupportedCategory
	CodeInvalidValue
	CodeMissingField
	CodeMissingReference
	CodeReferenceCycle
	CodeDuplicateReference
)

func (c Code) String() string {
	switch c {
	case CodeUnsupportedCategory:
		return "BUNDLE_UNSUPPORTED_CATEGORY"
	case CodeInvalidValue:
		return "BUNDLE_INVALID_VALUE"
	case CodeMissingField:
		return "BUNDLE_MISSING_FIELD"
	case CodeMissingReference:
		return "BUNDLE_MISSING_REFERENCE"
	case CodeReferenceCycle:
		return "BUNDLE_REFERENCE_CYCLE"
	case CodeDuplicateReference:
		return "BUNDLE_DUPLICATE_REFERENCE"
	default:
		return "BUNDLE_INVALID"
	}
}

var ErrInvalidCandidate = errors.New("bundle candidate is not admitted")

type Diagnostic struct {
	Code    Code
	At      parser.Position
	Path    string
	Message string
}

func (d *Diagnostic) Error() string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d %s at %s: %s", d.At.File, d.At.Line, d.At.Column, d.Code.String(), d.Path, d.Message)
}
func diagnostic(code Code, n parser.Node, message string) *Diagnostic {
	return &Diagnostic{Code: code, At: n.Source(), Path: n.Path(), Message: message}
}
