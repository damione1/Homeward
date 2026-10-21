// Package report builds immutable, descriptive migration reports. It owns no
// runtime, store, dispatcher, configuration activation or household connection.
package report

import (
	"errors"
	"strings"

	"github.com/damione1/Homeward/compat/ha/nha047"
	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/logicselection"
)

const (
	Schema         = "homeward.compatibility-report/v1"
	MaxSourceBytes = 1 << 20
	MaxInputBytes  = 4 << 20
	MaxReportBytes = 8 << 20
	MaxNodes       = 20000
)

var (
	ErrInvalidInput  = errors.New("compatibility report: invalid input")
	ErrLimit         = errors.New("compatibility report: limit exceeded")
	ErrInvalidReport = errors.New("compatibility report: invalid report")
)

type Verdict uint8

const (
	VerdictInvalid Verdict = iota
	AcceptedUnchanged
	AcceptedAdapted
	Refused
)

func (v Verdict) String() string {
	switch v {
	case AcceptedUnchanged:
		return "accepted_unchanged"
	case AcceptedAdapted:
		return "accepted_adapted"
	case Refused:
		return "refused"
	case VerdictInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

type RefusalKind uint8

const (
	RefusalNone RefusalKind = iota
	InvalidInput
	SourceUnavailable
	Syntax
	Semantic
	ExcludedReference
	MissingDeclaredCapability
	UnsupportedConstruction
	CoverageMissing
	EvidenceInvalid
	AdaptationUnvalidated
)

func (r RefusalKind) String() string {
	switch r {
	case RefusalNone:
		return "none"
	case InvalidInput:
		return "invalid_input"
	case SourceUnavailable:
		return "source_unavailable"
	case Syntax:
		return "syntax"
	case Semantic:
		return "semantic"
	case ExcludedReference:
		return "excluded_reference"
	case MissingDeclaredCapability:
		return "missing_declared_capability"
	case UnsupportedConstruction:
		return "unsupported_construction"
	case CoverageMissing:
		return "coverage_missing"
	case EvidenceInvalid:
		return "evidence_invalid"
	case AdaptationUnvalidated:
		return "adaptation_unvalidated"
	default:
		return "invalid"
	}
}

func (r RefusalKind) Reason() string {
	switch r {
	case RefusalNone:
		return ""
	case InvalidInput:
		return "The input does not satisfy the declared profile."
	case SourceUnavailable:
		return "An authorized source snapshot is unavailable."
	case Syntax:
		return "The source is outside the admitted syntax profile."
	case Semantic:
		return "The compiler refused this object's semantics."
	case ExcludedReference:
		return "A referenced identity is outside the supplied scope."
	case MissingDeclaredCapability:
		return "A required capability is not declared in the supplied catalogue."
	case UnsupportedConstruction:
		return "An operative construction is not represented by this profile."
	case CoverageMissing:
		return "No applicable differential proof covers this construction and its parameters."
	case EvidenceInvalid:
		return "The supplied evidence is not admitted."
	case AdaptationUnvalidated:
		return "The explicit candidate's complete delta has not been validated."
	default:
		return "Invalid refusal."
	}
}

type CoverageState uint8

const (
	CoverageInvalid CoverageState = iota
	Covered
	Partial
	Absent
)

func (s CoverageState) String() string {
	switch s {
	case Covered:
		return "covered"
	case Partial:
		return "partial"
	case Absent:
		return "absent"
	case CoverageInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

type Activation uint8

const (
	ActivationInvalid Activation = iota
	BusinessPending
	DeferredAfterMVP
)

func (a Activation) String() string {
	switch a {
	case BusinessPending:
		return "business_pending"
	case DeferredAfterMVP:
		return "deferred_after_mvp"
	case ActivationInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

type ConfigurationRef struct {
	id       string
	revision uint64
	digest   nha047.Digest
}

func NewConfigurationRef(id string, revision uint64, digest nha047.Digest) (ConfigurationRef, error) {
	if len(id) == 0 || len(id) > 128 || revision == 0 || !digest.Valid() {
		return ConfigurationRef{}, ErrInvalidInput
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ConfigurationRef{}, ErrInvalidInput
		}
	}
	return ConfigurationRef{id, revision, digest}, nil
}
func (c ConfigurationRef) ID() string            { return c.id }
func (c ConfigurationRef) Revision() uint64      { return c.revision }
func (c ConfigurationRef) Digest() nha047.Digest { return c.digest }
func (c ConfigurationRef) valid() bool {
	_, err := NewConfigurationRef(c.id, c.revision, c.digest)
	return err == nil
}

type Location struct {
	File   string
	Line   int
	Column int
	Path   string
}

func location(position parser.Position, path string) Location {
	// Never copy values or raw compiler reason text. Unknown path text is not
	// a public diagnostic field; the retained line/column still locates it.
	for _, c := range path {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("$._[]-'", c)) {
			path = "$"
			break
		}
	}
	return Location{position.File, position.Line, position.Column, path}
}

type Construction struct {
	feature    nha047.Feature
	source     Location
	references []domain.HAAlias
	proofs     []nha047.Proof
	coverage   CoverageState
	refusal    RefusalKind
}

func (c Construction) Feature() nha047.Feature { return c.feature }
func (c Construction) Source() Location        { return c.source }
func (c Construction) References() []domain.HAAlias {
	return append([]domain.HAAlias(nil), c.references...)
}
func (c Construction) Proofs() []nha047.Proof  { return append([]nha047.Proof(nil), c.proofs...) }
func (c Construction) Coverage() CoverageState { return c.coverage }
func (c Construction) Refusal() RefusalKind    { return c.refusal }
func (c Construction) clone() Construction {
	c.references = c.References()
	c.proofs = c.Proofs()
	return c
}

type Object struct {
	definition      logicselection.Definition
	verdict         Verdict
	refusal         RefusalKind
	activation      Activation
	candidate       nha047.Digest
	originalSource  SourceProvenance
	candidateSource SourceProvenance
	nodes           []Construction
	compilerCode    string
}

func (o Object) ID() domain.HAAlias                     { return o.definition.ID() }
func (o Object) Definition() logicselection.Definition  { return o.definition }
func (o Object) Verdict() Verdict                       { return o.verdict }
func (o Object) Refusal() RefusalKind                   { return o.refusal }
func (o Object) Activation() Activation                 { return o.activation }
func (o Object) Activatable() bool                      { return false }
func (o Object) CandidateDigest() (nha047.Digest, bool) { return o.candidate, o.candidate.Valid() }
func (o Object) OriginalSource() SourceProvenance       { return o.originalSource }
func (o Object) CandidateSource() SourceProvenance      { return o.candidateSource }
func (o Object) CompilerCode() string                   { return o.compilerCode }
func (o Object) Constructions() []Construction {
	result := make([]Construction, len(o.nodes))
	for i, c := range o.nodes {
		result[i] = c.clone()
	}
	return result
}
func (o Object) clone() Object { o.nodes = o.Constructions(); return o }

type Report struct {
	scopeSummary  scopeSummary
	configuration ConfigurationRef
	baseline      nha047.Digest
	selection     nha047.Digest
	manifest      nha047.Digest
	objects       []Object
	deferred      []domain.HAAlias
	valid         bool
}

func (r Report) Valid() bool                     { return r.valid }
func (r Report) Configuration() ConfigurationRef { return r.configuration }
func (r Report) BaselineDigest() nha047.Digest   { return r.baseline }
func (r Report) SelectionDigest() nha047.Digest  { return r.selection }
func (r Report) ManifestDigest() nha047.Digest   { return r.manifest }
func (r Report) Objects() []Object {
	result := make([]Object, len(r.objects))
	for i, o := range r.objects {
		result[i] = o.clone()
	}
	return result
}
func (r Report) Deferred() []domain.HAAlias { return append([]domain.HAAlias(nil), r.deferred...) }

// SourceOrigin distinguishes the frozen documentary source from an original
// supplied for this analysis. Neither origin grants activation or provenance
// beyond what the builder validates against the selected definition.
type SourceOrigin uint8

const (
	SourceOriginInvalid SourceOrigin = iota
	Frozen158
	ExplicitImportPair
	ExplicitCandidate
)

func (o SourceOrigin) String() string {
	switch o {
	case Frozen158:
		return "frozen_158"
	case ExplicitImportPair:
		return "explicit_import_pair"
	case ExplicitCandidate:
		return "explicit_candidate"
	case SourceOriginInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

type SourceProvenance struct {
	origin SourceOrigin
	path   string
	digest nha047.Digest
}

func (s SourceProvenance) Origin() SourceOrigin  { return s.origin }
func (s SourceProvenance) Path() string          { return s.path }
func (s SourceProvenance) Digest() nha047.Digest { return s.digest }
func (s SourceProvenance) Valid() bool {
	return (s.origin == Frozen158 || s.origin == ExplicitImportPair || s.origin == ExplicitCandidate) && s.path != "" && s.digest.Valid()
}

// Candidate is explicitly supplied, never inferred, written or activated by
// the report. The initial admitted delta is the legacy service/action spelling
// of switch.turn_on only; other adaptations remain unvalidated.
type Candidate struct {
	id           domain.HAAlias
	original     nha047.Digest
	file         parser.SourceFile
	origin       SourceOrigin
	originalFile parser.SourceFile
}

func NewCandidate(id domain.HAAlias, original nha047.Digest, file parser.SourceFile) (Candidate, error) {
	name, err := id.Value()
	if err != nil || !strings.HasPrefix(name, "automation.") || !original.Valid() || file.Path() == "" || file.ByteLen() == 0 {
		return Candidate{}, ErrInvalidInput
	}
	return Candidate{id: id, original: original, file: file, origin: Frozen158}, nil
}

// NewCandidatePair admits two detached snapshots; technical delta validation
// belongs to Build. A supplied original is never represented as historical
// household data or as the documentary source158.
func NewCandidatePair(id domain.HAAlias, original, candidate parser.SourceFile) (Candidate, error) {
	if original.Path() == "" || original.ByteLen() == 0 || original.ByteLen() > MaxSourceBytes || candidate.ByteLen() > MaxSourceBytes || !nha047.Digest(original.SHA256()).Valid() {
		return Candidate{}, ErrInvalidInput
	}
	value, err := NewCandidate(id, nha047.Digest(original.SHA256()), candidate)
	if err != nil {
		return Candidate{}, err
	}
	value.origin = ExplicitImportPair
	value.originalFile = original
	return value, nil
}

func (c Candidate) OriginalSource() SourceProvenance {
	if !c.valid() {
		return SourceProvenance{}
	}
	path := c.file.Path()
	if c.origin == ExplicitImportPair {
		path = c.originalFile.Path()
	}
	return SourceProvenance{c.origin, path, c.original}
}

func (c Candidate) CandidateSource() SourceProvenance {
	if !c.valid() {
		return SourceProvenance{}
	}
	return SourceProvenance{ExplicitCandidate, c.file.Path(), nha047.Digest(c.file.SHA256())}
}

func (c Candidate) valid() bool {
	switch c.origin {
	case Frozen158:
		_, err := NewCandidate(c.id, c.original, c.file)
		return err == nil && c.originalFile.Path() == "" && c.originalFile.ByteLen() == 0
	case ExplicitImportPair:
		_, err := NewCandidatePair(c.id, c.originalFile, c.file)
		return err == nil && c.original == nha047.Digest(c.originalFile.SHA256())
	case SourceOriginInvalid, ExplicitCandidate:
		return false
	default:
		return false
	}
}
