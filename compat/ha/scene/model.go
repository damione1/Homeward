// Package scene admits Home Assistant scene definitions as immutable desired
// states. It does not observe devices, authorize commands, or activate scenes.
package scene

import (
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
)

var ErrInvalidInput = errors.New("invalid HA scene input")

// SourceRef is the already admitted NHA-158 identity/provenance, not an
// identity inferred from a YAML name or a source file's basename.
type SourceRef struct {
	id     domain.HAAlias
	path   string
	digest [sha256.Size]byte
}

func NewSourceRef(id domain.HAAlias, path string, digest [sha256.Size]byte) (SourceRef, error) {
	name, err := id.Value()
	if err != nil || !strings.HasPrefix(name, "scene.") || path == "" || len(path) > 4096 || digest == ([sha256.Size]byte{}) {
		return SourceRef{}, ErrInvalidInput
	}
	return SourceRef{id: id, path: path, digest: digest}, nil
}

func (r SourceRef) ID() (domain.HAAlias, error) {
	if _, err := NewSourceRef(r.id, r.path, r.digest); err != nil {
		return domain.HAAlias{}, err
	}
	return r.id, nil
}
func (r SourceRef) Path() string              { return r.path }
func (r SourceRef) Digest() [sha256.Size]byte { return r.digest }

type MemberKind uint8

const (
	MemberInvalid MemberKind = iota
	MemberLight
	MemberSwitch
	MemberFan
	MemberClimate
)

type Code uint8

const (
	CodeInvalid Code = iota
	CodeSyntax
	CodeInvalidValue
	CodeUnknownField
	CodeUnsupportedValue
	CodeMissingValue
	CodeUnqualifiedCapability
)

type Diagnostic struct {
	code   Code
	alias  domain.HAAlias
	field  string
	at     parser.Position
	path   string
	reason string
}

func (d Diagnostic) Code() Code                { return d.code }
func (d Diagnostic) Alias() domain.HAAlias     { return d.alias }
func (d Diagnostic) Field() string             { return d.field }
func (d Diagnostic) Position() parser.Position { return d.at }
func (d Diagnostic) Path() string              { return d.path }
func (d Diagnostic) Reason() string            { return d.reason }

// LightIntent keeps HA brightness in its original 0..255 unit. CT is already
// projected from Kelvin with HA's mired rule, but never into a native level.
type LightIntent struct {
	on         bool
	brightness *uint8
	ctMireds   *uint32
}

func (v LightIntent) Components() (bool, *uint8, *uint32) {
	var b *uint8
	if v.brightness != nil {
		value := *v.brightness
		b = &value
	}
	var ct *uint32
	if v.ctMireds != nil {
		value := *v.ctMireds
		ct = &value
	}
	return v.on, b, ct
}

type FanIntent struct {
	on         bool
	percentage uint8
}

func (v FanIntent) Percentage() uint8 { return v.percentage }
func (v FanIntent) On() bool          { return v.on }

// ClimateIntent retains the present HA service requirements even when this
// Homeward profile cannot command them. A refusal never drops those fields.
type ClimateIntent struct {
	mode        domain.ClimateMode
	temperature *float64
	humidity    *float64
	fanMode     *string
	presetMode  *string
}

func (v ClimateIntent) Components() (domain.ClimateMode, *float64, *float64, *string, *string) {
	return v.mode, cloneFloat(v.temperature), cloneFloat(v.humidity), cloneString(v.fanMode), cloneString(v.presetMode)
}

type Member struct {
	kind        MemberKind
	alias       domain.HAAlias
	source      parser.Position
	light       LightIntent
	switchOn    bool
	fan         FanIntent
	climate     ClimateIntent
	diagnostics []Diagnostic
}

func (m Member) Kind() MemberKind        { return m.kind }
func (m Member) Alias() domain.HAAlias   { return m.alias }
func (m Member) Source() parser.Position { return m.source }
func (m Member) Light() (LightIntent, bool) {
	on, brightness, ct := m.light.Components()
	return LightIntent{on: on, brightness: brightness, ctMireds: ct}, m.kind == MemberLight
}
func (m Member) Switch() (bool, bool)   { return m.switchOn, m.kind == MemberSwitch }
func (m Member) Fan() (FanIntent, bool) { return m.fan, m.kind == MemberFan }
func (m Member) Climate() (ClimateIntent, bool) {
	mode, temperature, humidity, fanMode, presetMode := m.climate.Components()
	return ClimateIntent{mode: mode, temperature: temperature, humidity: humidity, fanMode: fanMode, presetMode: presetMode}, m.kind == MemberClimate
}
func (m Member) Diagnostics() []Diagnostic { return append([]Diagnostic(nil), m.diagnostics...) }

type Definition struct {
	ref     SourceRef
	yamlID  string
	name    string
	members []Member
}

func (d Definition) Source() SourceRef  { return d.ref }
func (d Definition) ID() domain.HAAlias { return d.ref.id }
func (d Definition) YAMLID() string     { return d.yamlID }
func (d Definition) Name() string       { return d.name }
func (d Definition) Members() []Member {
	out := make([]Member, len(d.members))
	for i, member := range d.members {
		out[i] = member
		out[i].light, _ = member.Light()
		out[i].climate, _ = member.Climate()
		out[i].diagnostics = member.Diagnostics()
	}
	return out
}

type Result struct {
	accepted []Definition
	rejected []Diagnostic
}

func (r Result) Accepted() []Definition {
	out := make([]Definition, len(r.accepted))
	for i, definition := range r.accepted {
		out[i] = definition
		out[i].members = definition.Members()
	}
	return out
}
func (r Result) Rejected() []Diagnostic { return append([]Diagnostic(nil), r.rejected...) }

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
