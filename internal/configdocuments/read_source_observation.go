package configdocuments

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"hash"
	"sort"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/storage"
)

// SourceObservationSet is one bounded SQL/catalogue/physical observation cut.
// It is never a publisher, permission, compiled program or filesystem locator
// supplied by a client. Bytes are retained only for the lifetime of this value.
type SourceObservationSet struct {
	valid      bool
	generation storage.ConfigGenerationRevision
	manifest   [32]byte
	catalog    storage.ConfigDocumentCut
	targets    []sourceWatchTarget
	global     [32]byte
	hasGlobal  bool
}

func (s SourceObservationSet) Valid() bool {
	return s.valid && s.generation.Valid() && s.manifest != ([32]byte{}) && s.catalog.Valid()
}
func (s SourceObservationSet) Generation() storage.ConfigGenerationRevision { return s.generation }
func (s SourceObservationSet) ManifestSHA256() [32]byte                     { return s.manifest }
func (s SourceObservationSet) CatalogCut() storage.ConfigDocumentCut        { return s.catalog }
func (s SourceObservationSet) GlobalSHA256() ([32]byte, bool) {
	return s.global, s.Valid() && s.hasGlobal
}
func (SourceObservationSet) String() string                 { return "[REDACTED SOURCE OBSERVATION SET]" }
func (s SourceObservationSet) GoString() string             { return s.String() }
func (s SourceObservationSet) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// CaptureSourceObservations has no Service.Read recursion. Its caller must
// retain the surrounding Owner/journal bookends and compare CatalogCut with
// the document metadata cut before signing any ID-independent resume binding.
func (s *Service) CaptureSourceObservations(ctx context.Context, active storage.ConfigGenerationSnapshot) (SourceObservationSet, error) {
	if s == nil || !s.admitted || !usableContext(ctx) || !active.Revision().Valid() || active.ManifestSHA256() == ([32]byte{}) || active.Availability() != storage.ConfigGenerationComplete {
		return SourceObservationSet{}, &Error{kind: FailureInvalid}
	}
	before, err := s.store.ReadConfigDocumentState(ctx, operationalID(), operationalID())
	if err != nil || before.Generation() != active.Revision() || before.ManifestSHA256() != active.ManifestSHA256() {
		return SourceObservationSet{}, &Error{kind: Conflict}
	}
	targets, err := s.captureSourceTargets(ctx, active)
	if err != nil {
		return SourceObservationSet{}, err
	}
	after, err := s.store.ReadConfigDocumentState(ctx, operationalID(), operationalID())
	if err != nil || after.Generation() != active.Revision() || after.ManifestSHA256() != active.ManifestSHA256() || before.Cut() != after.Cut() {
		return SourceObservationSet{}, &Error{kind: Conflict}
	}
	global, present := sourceObservationGlobalDigest(targets)
	return SourceObservationSet{valid: true, generation: active.Revision(), manifest: active.ManifestSHA256(), catalog: after.Cut(), targets: targets, global: global, hasGlobal: present}, nil
}

func sourceObservationGlobalDigest(targets []sourceWatchTarget) ([32]byte, bool) {
	ordered := append([]sourceWatchTarget(nil), targets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].group.String() < ordered[j].group.String() })
	h := sha256.New()
	_, _ = h.Write([]byte("homeward.source-workspace-observation/1\x00"))
	count := 0
	for _, target := range ordered {
		if target.variant == sourceWatchPolicy && !target.materialized && !target.unavailable {
			continue
		}
		count++
		_, _ = h.Write([]byte{byte(target.variant)})
		sourceObservationPart(h, []byte(target.group.String()))
		sourceObservationPart(h, []byte(target.entry))
		sourceObservationPart(h, []byte(target.descriptor.Kind().String()))
		sourceObservationPart(h, []byte(target.descriptor.Identity()))
		sourceObservationPart(h, []byte(target.descriptor.Entry()))
		_, _ = h.Write(target.observation.hash[:])
		if target.observation.pending {
			_, _ = h.Write([]byte{1})
		} else {
			_, _ = h.Write([]byte{0})
		}
		var memberCount [8]byte
		binary.BigEndian.PutUint64(memberCount[:], uint64(len(target.members)))
		_, _ = h.Write(memberCount[:])
		members := append([]sourceWatchMember(nil), target.members...)
		sort.Slice(members, func(i, j int) bool { return members[i].binding.ID().String() < members[j].binding.ID().String() })
		for _, member := range members {
			sourceObservationPart(h, []byte(member.binding.ID().String()))
			sourceObservationPart(h, []byte(member.binding.Path()))
			var number [8]byte
			binary.BigEndian.PutUint64(number[:], uint64(member.revision.Number()))
			_, _ = h.Write(number[:])
			_, _ = h.Write(member.sql[:])
		}
	}
	if count == 0 {
		return [32]byte{}, false
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result, true
}
func sourceObservationPart(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

// SourceDocumentObservation distinguishes an admitted physical locator from
// explicit unobserved legacy/inactive/security metadata. Located does not mean
// a file exists: a known missing member has DirtyUnavailable, never Clean.
type SourceDocumentObservation struct {
	valid, located bool
	observation    Observation
	dirty          DirtyState
	diagnostic     WorkspaceDiagnostic
}

func (s SourceDocumentObservation) Valid() bool {
	return s.valid && (!s.located && s.dirty == DirtyUnknown || s.located && validDirty(s.dirty))
}
func (s SourceDocumentObservation) Located() bool          { return s.Valid() && s.located }
func (s SourceDocumentObservation) Workspace() Observation { return s.observation }
func (s SourceDocumentObservation) Dirty() DirtyState      { return s.dirty }
func (s SourceDocumentObservation) Diagnostic() (WorkspaceDiagnostic, bool) {
	return s.diagnostic, s.diagnostic.kind != DiagnosticUnknown
}

// ReadSourceObservation projects a selected exact SQL reference from the SAME
// global set used in the signed149 cursor. It does not read, mutate or emit a
// second observation. The independent policy pointer is already represented
// by the captured policy version, including when absent from generation refs.
func (s *Service) ReadSourceObservation(document storage.ConfigDocumentMetadata, captured SourceObservationSet) (SourceDocumentObservation, error) {
	unobserved := SourceDocumentObservation{valid: true, dirty: DirtyUnknown}
	if s == nil || !s.admitted || !captured.Valid() || !document.Ref().Valid() {
		return SourceDocumentObservation{}, &Error{kind: FailureInvalid}
	}
	if !document.Retained() {
		return SourceDocumentObservation{}, &Error{kind: IncompleteHistory}
	}
	for _, target := range captured.targets {
		for _, member := range target.members {
			if member.binding.ID() != document.Ref().Document() {
				continue
			}
			if member.revision != document.Ref().Revision() || member.sql != document.SourceSHA256() {
				return unobserved, nil
			}
			if !sourceTargetMatchesDocument(target, document) {
				return unobserved, nil
			}
			if target.variant == sourceWatchPolicy && !target.materialized && !target.unavailable {
				return unobserved, nil
			}
			return s.projectSourceObservation(member, target), nil
		}
	}
	return unobserved, nil
}
func sourceTargetMatchesDocument(target sourceWatchTarget, document storage.ConfigDocumentMetadata) bool {
	switch target.variant {
	case sourceWatchNative:
		return target.descriptor.Valid() && target.descriptor.Kind() == document.Kind()
	case sourceWatchPolicy:
		return document.Kind() == storage.ConfigDocumentKindAuthorizationPolicy
	case sourceWatchDeclarative1:
		return document.Kind() == storage.ConfigDocumentKindYAMLSource && document.SchemaVersion() == bundle.SchemaDeclarative1.String() && document.SyntaxProfile() == parser.DeclarativeProfile
	case sourceWatchDeclarative2:
		return document.Kind() == storage.ConfigDocumentKindYAMLSource && document.SchemaVersion() == bundle.SchemaDeclarative2.String() && document.SyntaxProfile() == parser.DeclarativeProfile2
	default:
		return false
	}
}
func (s *Service) projectSourceObservation(member sourceWatchMember, target sourceWatchTarget) SourceDocumentObservation {
	observed := member.observed.observation
	result := SourceDocumentObservation{valid: true, located: true, observation: observed, dirty: DirtyUnvalidated}
	if target.unavailable || !member.observed.Present() {
		result.dirty = DirtyUnavailable
		return result
	}
	if observed.pending {
		result.dirty = DirtyReconciling
		result.diagnostic = sourceObservationDiagnostic(member, WorkspaceReconciling)
		return result
	}
	if observed.hash == member.sql {
		result.dirty = Clean
		return result
	}
	if diagnostic, present := s.sourceJournalDiagnostic(member); present {
		result.diagnostic = diagnostic
		if diagnostic.kind == CandidateRejected {
			result.dirty = DirtyRejected
		}
	}
	return result
}
func sourceObservationDiagnostic(member sourceWatchMember, kind DiagnosticKind) WorkspaceDiagnostic {
	observation := member.observed.observation
	if !observation.present || observation.hash == ([32]byte{}) || observation.updated.IsZero() {
		return WorkspaceDiagnostic{}
	}
	switch kind {
	case CandidateRejected, WorkspaceChanged, WorkspaceReconciling, ActivationUnavailable:
		return WorkspaceDiagnostic{document: member.binding.ID(), hash: observation.hash, updated: observation.updated, kind: kind}
	default:
		return WorkspaceDiagnostic{}
	}
}
func (s *Service) sourceJournalDiagnostic(member sourceWatchMember) (WorkspaceDiagnostic, bool) {
	if s.journal == nil {
		return WorkspaceDiagnostic{}, false
	}
	s.journal.mu.Lock()
	defer s.journal.mu.Unlock()
	for index := len(s.journal.ring) - 1; index >= 0; index-- {
		for _, change := range s.journal.ring[index].changes {
			if change.id != member.binding.ID() {
				continue
			}
			observation := member.observed.observation
			if change.revision != member.revision || change.hash != member.sql || change.workspace != observation.hash || !change.observedAt.Equal(observation.updated) {
				return WorkspaceDiagnostic{}, false
			}
			kind := change.diagnostic
			if kind == CandidateRejected && change.dirty != DirtyRejected {
				return WorkspaceDiagnostic{}, false
			}
			// A resolved intent must not inherit an old reconciliation diagnostic.
			if kind == WorkspaceReconciling && !observation.pending {
				return WorkspaceDiagnostic{}, false
			}
			diagnostic := sourceObservationDiagnostic(member, kind)
			return diagnostic, diagnostic.kind != DiagnosticUnknown
		}
	}
	return WorkspaceDiagnostic{}, false
}
