// Package configplans describes optional, inert configuration previews. It owns
// neither configuration publication nor a persistent plan or authorization store.
package configplans

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/storage"
)

var ErrInvalid = errors.New("configuration plan: invalid input")
var ErrUnavailable = errors.New("configuration plan: unavailable")
var ErrStale = errors.New("configuration plan: base changed; recalculate")

// Family names existing admission paths, not a free-form compiler registry.
type Family uint8

const (
	FamilyInvalid Family = iota
	Operational
	Declarative1
	Declarative2
	Authorization
	Ownership
	Dashboard
	Automation
	Script
	Scene
)

func (f Family) Valid() bool {
	switch f {
	case Operational, Declarative1, Declarative2, Authorization, Ownership, Dashboard, Automation, Script, Scene:
		return true
	case FamilyInvalid:
		return false
	default:
		return false
	}
}
func (f Family) Schema() string {
	switch f {
	case Operational:
		return "homeward.operational/1"
	case Declarative1:
		return "ha-declarative-1"
	case Declarative2:
		return "ha-declarative-2"
	case Authorization:
		return "homeward.authorization/1"
	case Ownership:
		return "homeward.heating-ownership/1"
	case Dashboard:
		return "homeward-dashboard-1"
	case Automation, Script:
		return "ha-logic-1"
	case Scene:
		return "ha-scene-1"
	case FamilyInvalid:
		return ""
	default:
		return ""
	}
}

type Effect uint8

const (
	EffectInvalid Effect = iota
	NoEffect
	DashboardEffect
	ConfigurationEffect
	RightsEffect
	OwnershipEffect
	RunTargetsEffect
)

func (e Effect) Valid() bool {
	switch e {
	case NoEffect, DashboardEffect, ConfigurationEffect, RightsEffect, OwnershipEffect, RunTargetsEffect:
		return true
	case EffectInvalid:
		return false
	default:
		return false
	}
}

type BaseKind uint8

const (
	BaseInvalid BaseKind = iota
	ActiveSQL
	ImmutableReference
	NoActiveDocument
)

type State uint8

const (
	StateInvalid State = iota
	Admitted
	Rejected
	Stale
)

type ChangeOperation uint8

const (
	ChangeInvalid ChangeOperation = iota
	Added
	Removed
	Replaced
)

type Diagnostic uint8

const (
	DiagnosticInvalid Diagnostic = iota
	InvalidSchema
	ExcludedReference
	UnsupportedSource
	Capacity
	BaseUnavailable
)

// Candidate owns source copies; formatting/JSON never exposes private sources.
type Candidate struct {
	family          Family
	identity, entry string
	files           []parser.SourceFile
	hash            [32]byte
	admitted        bool
}

func NewCandidate(family Family, identity, entry string, files []parser.SourceFile) (Candidate, error) {
	if !family.Valid() || !label(identity, 255) || len(files) < 1 || len(files) > 64 {
		return Candidate{}, ErrInvalid
	}
	total := 0
	seen := map[string]bool{}
	owned := make([]parser.SourceFile, 0, len(files))
	for _, file := range files {
		raw := file.Bytes()
		total += len(raw)
		if total > 1<<20 || len(raw) == 0 || seen[file.Path()] {
			return Candidate{}, ErrInvalid
		}
		copyFile, err := parser.NewSourceFile(file.Path(), raw)
		if err != nil || copyFile.SHA256() != file.SHA256() {
			return Candidate{}, ErrInvalid
		}
		seen[file.Path()] = true
		owned = append(owned, copyFile)
	}
	if !seen[entry] {
		return Candidate{}, ErrInvalid
	}
	// Source order is part of neither identity nor semantic admission. This exact
	// digest is a length-delimited sorted source manifest, not the semantic hash.
	hash := sourceDigest(owned)
	return Candidate{family: family, identity: identity, entry: entry, files: owned, hash: hash, admitted: true}, nil
}
func (c Candidate) Valid() bool                  { return c.admitted && c.family.Valid() && c.hash != ([32]byte{}) }
func (c Candidate) Family() Family               { return c.family }
func (c Candidate) Identity() string             { return c.identity }
func (c Candidate) SourceSHA256() [32]byte       { return c.hash }
func (c Candidate) Schema() string               { return c.family.Schema() }
func (Candidate) String() string                 { return "[REDACTED CONFIGURATION CANDIDATE]" }
func (c Candidate) GoString() string             { return c.String() }
func (c Candidate) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

type Base struct {
	kind     BaseKind
	revision int64
	source   [32]byte
}

func (b Base) Kind() BaseKind         { return b.kind }
func (b Base) Revision() int64        { return b.revision }
func (b Base) SourceSHA256() [32]byte { return b.source }

type Change struct {
	effect                             Effect
	operation                          ChangeOperation
	field                              string
	referencesAdded, referencesRemoved uint32
}

func (c Change) Effect() Effect             { return c.effect }
func (c Change) Operation() ChangeOperation { return c.operation }
func (c Change) Field() string              { return c.field }
func (c Change) ReferencesAdded() uint32    { return c.referencesAdded }
func (c Change) ReferencesRemoved() uint32  { return c.referencesRemoved }

// Plan contains public metadata only. Its receipt proves provenance, never
// grants permission; callers must still enter their existing 050/029 boundary.
type Plan struct {
	state                        State
	author                       string
	family                       Family
	identity, schema, entry      string
	source, semantic             [32]byte
	base                         Base
	generation                   int64
	manifest, workspace, catalog [32]byte
	effect                       Effect
	changes                      []Change
	diagnostics                  []Diagnostic
	receipt                      string
}

func (p Plan) State() State              { return p.state }
func (p Plan) Author() string            { return p.author }
func (p Plan) Family() Family            { return p.family }
func (p Plan) Identity() string          { return p.identity }
func (p Plan) Schema() string            { return p.schema }
func (p Plan) SourceSHA256() [32]byte    { return p.source }
func (p Plan) SemanticSHA256() [32]byte  { return p.semantic }
func (p Plan) Base() Base                { return p.base }
func (p Plan) Generation() int64         { return p.generation }
func (p Plan) ManifestSHA256() [32]byte  { return p.manifest }
func (p Plan) WorkspaceSHA256() [32]byte { return p.workspace }
func (p Plan) Effect() Effect            { return p.effect }
func (p Plan) Changes() []Change         { return append([]Change(nil), p.changes...) }
func (p Plan) Diagnostics() []Diagnostic { return append([]Diagnostic(nil), p.diagnostics...) }
func (p Plan) Receipt() string           { return p.receipt }

// ApplicationAvailable is deliberately false: preview is distinct from apply055.
func (Plan) ApplicationAvailable() bool { return false }
func label(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}
func usableAuthor(author storage.ConfigAuthor) (string, error) {
	value, ok := author.Value()
	if !ok {
		return "", ErrInvalid
	}
	return value, nil
}
func sameBytes(a, b []byte) bool { return bytes.Equal(a, b) }
