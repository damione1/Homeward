package configdocuments

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

const sourceWatchMaximumTargets = 256
const sourceWatchMaximumMembers = 256
const sourceWatchMaximumBytes = 4 << 20

type sourceWatchVariant uint8

const (
	sourceWatchInvalid sourceWatchVariant = iota
	sourceWatchNative
	sourceWatchPolicy
	sourceWatchDeclarative1
	sourceWatchDeclarative2
)

type sourceWatchMember struct {
	binding  storage.ConfigDocumentBinding
	revision storage.ConfigVersionRevision
	sql      [32]byte
	observed SourceObservation
}
type sourceWatchTarget struct {
	variant                             sourceWatchVariant
	entry                               string
	group                               storage.ConfigDocumentID
	descriptor                          storage.SourceDescriptor
	account                             contracts.AccountID
	policy                              authorization.Version
	members                             []sourceWatchMember
	observation                         Observation
	complete, materialized, unavailable bool
}
type sourceWatchCut struct {
	operational Snapshot
	targets     []sourceWatchTarget
}
type policyPhysicalObservation struct {
	present bool
	hash    [32]byte
}
type policyObservationProvenance struct {
	known    bool
	version  authorization.Version
	physical policyPhysicalObservation
	armed    bool
}

// observe never rebases an old physical intention onto a newly active policy.
// SQL/version changes and first captures establish a DISARMED baseline. Only
// a later bytes/presence change observed under that same base can arm apply;
// mtime, catalogue refresh or watcher restart alone can never restore a grant.
func (p *policyObservationProvenance) observe(target sourceWatchTarget) bool {
	_, _, versionErr := target.policy.Values()
	if target.variant != sourceWatchPolicy || len(target.members) != 1 || target.unavailable || target.observation.pending || versionErr != nil {
		*p = policyObservationProvenance{}
		return false
	}
	physical := policyPhysicalObservation{present: target.members[0].observed.Present(), hash: target.members[0].observed.observation.hash}
	if !p.known || p.version != target.policy {
		*p = policyObservationProvenance{known: true, version: target.policy, physical: physical}
		return false
	}
	if p.physical != physical {
		p.physical = physical
		p.armed = true
	}
	if !sourceTargetDirty(target) {
		p.armed = false
	}
	return p.armed
}

type sourceWatchWindow struct {
	policy                 policyObservationProvenance
	policyBlocked          bool
	policyEverMaterialized bool
	cut                    [32]byte
	stable                 stableObservation
	last, completed        Observation
	observed               bool
	retryAt                time.Time
}
type sourceWatchState struct {
	windows map[storage.ConfigDocumentID]*sourceWatchWindow
	retryAt time.Time
}

func nativeSourceKind(kind storage.ConfigDocumentKind) bool {
	switch kind {
	case storage.ConfigDocumentKindDashboardSource, storage.ConfigDocumentKindAutomationSource, storage.ConfigDocumentKindScriptSource, storage.ConfigDocumentKindSceneSource, storage.ConfigDocumentKindOwnershipSource:
		return true
	default:
		return false
	}
}

// Native kind+identity select the admitted group ID in the sole source writer.
// Declarative grouping is supplied only by its separate admitted generation
// locator; it is never inferred from a source ID or filesystem scan.
func nativeSourceWatchTargets(sources []storage.EditableSourceSnapshot) ([]sourceWatchTarget, error) {
	targets := make([]sourceWatchTarget, 0)
	indexes := map[storage.ConfigDocumentID]int{}
	for _, source := range sources {
		descriptor := source.Descriptor()
		binding := source.Binding()
		group, err := SourceWorkspaceGroup(descriptor)
		if err != nil || !nativeSourceKind(descriptor.Kind()) || !source.Ref().Valid() || source.Ref().Document() != binding.ID() {
			return nil, &Error{kind: Unavailable}
		}
		index, exists := indexes[group]
		if !exists {
			if len(targets) >= sourceWatchMaximumTargets {
				return nil, &Error{kind: Unavailable}
			}
			index = len(targets)
			indexes[group] = index
			targets = append(targets, sourceWatchTarget{variant: sourceWatchNative, group: group, descriptor: descriptor})
		}
		if targets[index].descriptor != descriptor || len(targets[index].members) >= 64 {
			return nil, &Error{kind: Unavailable}
		}
		targets[index].members = append(targets[index].members, sourceWatchMember{binding: binding, revision: source.Ref().Revision(), sql: source.File().SHA256()})
	}
	return targets, nil
}

// sourceWatchCapture reuses the single read composition and its exact
// Owner/catalogue/journal bookends. No second SQL/filesystem capture is made.
func (s *Service) sourceWatchCapture(ctx context.Context) (sourceWatchCut, error) {
	current, captured, err := s.readSourceSnapshot(ctx, operationalID())
	if err != nil {
		return sourceWatchCut{}, err
	}
	if !captured.Valid() || captured.Generation() != current.generation || captured.ManifestSHA256() != current.manifest || captured.CatalogCut() != current.catalog {
		return sourceWatchCut{}, &Error{kind: Conflict}
	}
	return sourceWatchCut{operational: current, targets: captured.targets}, nil
}

// captureSourceTargets is shared by watcher and public read composition. It
// contains no Service.Read call, publisher, watcher state or mutation lock.
func (s *Service) captureSourceTargets(ctx context.Context, active storage.ConfigGenerationSnapshot) ([]sourceWatchTarget, error) {
	hasNative := false
	for _, version := range active.Versions() {
		kind, present := version.Kind()
		if !present {
			return nil, &Error{kind: Unavailable}
		}
		hasNative = hasNative || nativeSourceKind(kind)
	}
	var targets []sourceWatchTarget
	if hasNative {
		sources, err := s.store.ReadEditableSources(ctx, active.Revision(), active.ManifestSHA256())
		if err != nil {
			return nil, err
		}
		targets, err = nativeSourceWatchTargets(sources)
		if err != nil {
			return nil, err
		}
	}
	declarative, hasDeclarative, err := declarativeSourceWatchTarget(active)
	if err != nil {
		return nil, err
	}
	if hasDeclarative {
		targets = append(targets, declarative)
	}
	err = s.store.ForEachAuthorizationPolicy(ctx, func(state authorization.AccountState) error {
		if len(targets) >= sourceWatchMaximumTargets {
			return &Error{kind: Unavailable}
		}
		account, err := state.Account()
		if err != nil {
			return err
		}
		version, err := state.Version()
		if err != nil {
			return err
		}
		revision, hash, err := version.Values()
		if err != nil || revision > uint64(^uint64(0)>>1) {
			return &Error{kind: Unavailable}
		}
		binding, err := storage.AuthorizationPolicyBinding(account)
		if err != nil {
			return err
		}
		sqlRevision, err := storage.NewConfigVersionRevision(int64(revision))
		if err != nil {
			return err
		}
		targets = append(targets, sourceWatchTarget{variant: sourceWatchPolicy, group: binding.ID(), account: account, policy: version, members: []sourceWatchMember{{binding: binding, revision: sqlRevision, sql: hash}}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].group.String() < targets[j].group.String() })
	totalMembers, totalBytes := 0, 0
	selected := targets[:0]
	for _, target := range targets {
		bindings := make([]storage.ConfigDocumentBinding, 0, len(target.members))
		for _, member := range target.members {
			bindings = append(bindings, member.binding)
		}
		if totalMembers+len(bindings) > sourceWatchMaximumMembers {
			return nil, &Error{kind: Unavailable}
		}
		observed, err := s.workspace.readSources(target.group, bindings, sourceWatchMaximumBytes-totalBytes)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			target.unavailable = true
			for index := range target.members {
				target.members[index].observed = SourceObservation{binding: target.members[index].binding}
			}
			target.observation = sourceTargetObservation(target)
			totalMembers += len(bindings)
			selected = append(selected, target)
			continue
		}
		target.complete = true
		for index, value := range observed {
			target.members[index].observed = value
			target.complete = target.complete && value.Present()
			target.materialized = target.materialized || value.Present()
			totalMembers++
			totalBytes += len(value.raw)
			if totalMembers > sourceWatchMaximumMembers || totalBytes > sourceWatchMaximumBytes {
				return nil, &Error{kind: Unavailable}
			}
		}
		target.observation = sourceTargetObservation(target)
		selected = append(selected, target)
	}
	return selected, nil
}

func sourceTargetObservation(target sourceWatchTarget) Observation {
	hash := sha256.New()
	updated := time.Time{}
	pending := false
	if target.unavailable {
		_, _ = hash.Write([]byte{1})
	} else {
		_, _ = hash.Write([]byte{0})
	}
	ordered := append([]sourceWatchMember(nil), target.members...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].binding.ID().String() < ordered[j].binding.ID().String() })
	for _, member := range ordered {
		_, _ = hash.Write([]byte(member.binding.ID().String()))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(member.binding.Path()))
		_, _ = hash.Write([]byte{0})
		observation := member.observed.observation
		if member.observed.Present() {
			_, _ = hash.Write([]byte{1})
		} else {
			_, _ = hash.Write([]byte{0})
		}
		_, _ = hash.Write(observation.hash[:])
		_, _ = hash.Write([]byte(observation.updated.Format(time.RFC3339Nano)))
		_, _ = hash.Write([]byte{0})
		if observation.updated.After(updated) {
			updated = observation.updated
		}
		pending = pending || observation.pending
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	// The synthetic aggregate is an observation identity, not a file source hash.
	// Its digest includes every member's presence, exact bytes and date.
	return Observation{hash: digest, updated: updated, present: true, pending: pending}
}
func sourceWatchCutHash(current Snapshot) [32]byte {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(current.generation.Number()))
	_, _ = h.Write(n[:])
	_, _ = h.Write(current.manifest[:])
	cut := current.catalog.SHA256()
	_, _ = h.Write(cut[:])
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func (s *Service) activateSourceTarget(ctx context.Context, current Snapshot, target sourceWatchTarget) (configactivation.Result, error) {
	if current.Dirty() != Clean || !target.complete || target.observation.pending {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	expectedOperational, err := NewExpectedDocument(operationalID(), current.document.Ref().Revision(), current.document.SourceSHA256(), current.observation.hash)
	if err != nil {
		return configactivation.Result{}, err
	}
	basis, err := NewWriteBasis(current.generation, current.manifest, current.catalog.SHA256(), time.Now().Add(10*time.Second))
	if err != nil {
		return configactivation.Result{}, err
	}
	expected := make([]ExpectedSource, 0, len(target.members))
	files := make([]parser.SourceFile, 0, len(target.members))
	bindings := make([]storage.ConfigDocumentBinding, 0, len(target.members))
	for _, member := range target.members {
		workspace, err := ExpectExistingMember(member.observed.observation.hash)
		if err != nil {
			return configactivation.Result{}, err
		}
		version, err := NewExpectedSource(member.binding, member.revision, member.sql, false, workspace)
		if err != nil {
			return configactivation.Result{}, err
		}
		file, err := parser.NewSourceFile(member.binding.Path(), member.observed.raw)
		if target.descriptor.Kind() == storage.ConfigDocumentKindOwnershipSource {
			file, err = parser.NewJSONSourceFile(member.binding.Path(), member.observed.raw)
		}
		if err != nil {
			return configactivation.Result{}, err
		}
		expected = append(expected, version)
		files = append(files, file)
		bindings = append(bindings, member.binding)
	}
	author, _ := storage.NewConfigAuthor("workspace.local")
	switch target.variant {
	case sourceWatchNative:
		if !target.descriptor.Valid() || !nativeSourceKind(target.descriptor.Kind()) {
			return configactivation.Result{}, &Error{kind: FailureInvalid}
		}
		group, err := configactivation.NewSourceGroup(target.descriptor, files, bindings)
		if err != nil {
			return configactivation.Result{}, err
		}
		return s.WriteSourceGroup(ctx, target.group, group, expectedOperational, expected, author, basis)
	case sourceWatchDeclarative1, sourceWatchDeclarative2:
		candidate, err := compileObservedDeclarative(target, files)
		if err != nil {
			return configactivation.Result{}, err
		}
		return s.WriteDefinitions(ctx, target.group, candidate, expectedOperational, expected, author, basis)
	case sourceWatchPolicy:
		if len(files) != 1 || target.descriptor.Valid() {
			return configactivation.Result{}, &Error{kind: FailureInvalid}
		}
		binding, err := storage.AuthorizationPolicyBinding(target.account)
		if err != nil || binding.ID() != target.group || binding != bindings[0] {
			return configactivation.Result{}, &Error{kind: FailureInvalid}
		}
		source, err := authorization.AdmitPolicySource(target.account, files[0].Bytes())
		if err != nil {
			return configactivation.Result{}, err
		}
		policyExpected, err := authorization.ExpectVersion(target.policy)
		if err != nil {
			return configactivation.Result{}, err
		}
		return s.WritePolicy(ctx, source, policyExpected, expectedOperational, expected[0], author, basis)
	default:
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
}

func sourceTargetDirty(target sourceWatchTarget) bool {
	for _, member := range target.members {
		if !member.observed.Present() || member.observed.observation.hash != member.sql {
			return true
		}
	}
	return false
}
func (s *Service) recordSourceTarget(target sourceWatchTarget, dirty DirtyState, diagnostic DiagnosticKind) {
	changes := make([]Change, 0, len(target.members))
	for _, member := range target.members {
		state := dirty
		if member.observed.Present() && member.observed.observation.hash == member.sql {
			state = Clean
		}
		change, err := NewChange(member.binding.ID(), member.revision, member.sql, member.observed.observation.hash, state)
		if err != nil {
			continue
		}
		change.observedAt = member.observed.observation.updated
		change.diagnostic = diagnostic
		if state == Clean {
			change.diagnostic = DiagnosticUnknown
		}
		changes = append(changes, change)
	}
	if len(changes) != 0 {
		_ = s.journal.Append(changes)
	}
}

// pollSourceSets is called by the existing150 joined goroutine only. Windows
// and terminal decisions contain fixed-size observations, never retained YAML.
func (w *Watcher) pollSourceSets(ctx context.Context, state *sourceWatchState, now time.Time) {
	captureCtx, cancelCapture := context.WithTimeout(ctx, 2*time.Second)
	capture, err := w.service.sourceWatchCapture(captureCtx)
	cancelCapture()
	if err != nil {
		if !now.Before(state.retryAt) {
			_ = w.service.ReconcileObserved(ctx)
			state.retryAt = now.Add(watcherRetry)
		}
		return
	}
	// Captures can consume the entire polling period. Stability starts AFTER
	// these SQL/filesystem reads, never at their stale invocation timestamp.
	now = w.clock.Now()
	state.retryAt = time.Time{}
	if state.windows == nil {
		state.windows = map[storage.ConfigDocumentID]*sourceWatchWindow{}
	}
	seen := map[storage.ConfigDocumentID]bool{}
	for _, target := range capture.targets {
		seen[target.group] = true
	}
	for group := range state.windows {
		if !seen[group] {
			delete(state.windows, group)
		}
	}
	cut := sourceWatchCutHash(capture.operational)
	for _, target := range capture.targets {
		seen[target.group] = true
		window := state.windows[target.group]
		if window == nil {
			window = &sourceWatchWindow{cut: cut}
			state.windows[target.group] = window
		} else if window.cut != cut {
			// Preserve policy provenance across unrelated global catalogue changes.
			window.cut = cut
			window.stable = stableObservation{}
			window.completed = Observation{}
		}
		policyReady := true
		policyBaseChanged := target.variant == sourceWatchPolicy && (!window.policy.known || window.policy.version != target.policy)
		observationChanged := !window.observed || window.last != target.observation
		if target.variant == sourceWatchPolicy {
			if target.materialized {
				window.policyEverMaterialized = true
			}
			policyReady = window.policy.observe(target)
			// An absent seeded policy establishes only a SQL/physical-absence base.
			// It emits no materialization/dirty event. A later real creation can arm;
			// first dirty capture after restart still lacks that earlier provenance.
			if !window.policyEverMaterialized && !target.unavailable {
				continue
			}
		}
		observation := target.observation
		if !window.observed || window.last != observation {
			window.last, window.observed = observation, true
			dirty, diagnostic := DirtyUnvalidated, DiagnosticUnknown
			if !sourceTargetDirty(target) {
				dirty = Clean
			} else if !target.complete {
				dirty, diagnostic = DirtyUnavailable, WorkspaceChanged
			}
			w.service.recordSourceTarget(target, dirty, diagnostic)
		}
		if observation.pending {
			window.stable = stableObservation{}
			if !now.Before(window.retryAt) {
				_ = w.service.ReconcileObserved(ctx)
				window.retryAt = now.Add(watcherRetry)
			}
			continue
		}
		if !target.complete {
			window.stable = stableObservation{}
			continue
		}
		if !sourceTargetDirty(target) {
			window.completed = observation
			window.policyBlocked = false
			continue
		}
		if !policyReady {
			window.stable = stableObservation{}
			if !window.policyBlocked || policyBaseChanged || observationChanged {
				w.service.recordSourceTarget(target, DirtyUnvalidated, WorkspaceChanged)
			}
			window.policyBlocked = true
			continue
		}
		window.policyBlocked = false
		if !window.stable.admit(observation, now) || window.completed == observation || now.Before(window.retryAt) || capture.operational.Dirty() != Clean {
			continue
		}
		result, activationErr := w.service.activateSourceTarget(ctx, capture.operational, target)
		switch result.Outcome() {
		case configactivation.Published, configactivation.RestartRequired:
			window.completed = observation
		case configactivation.CommitUnknown, configactivation.CommittedReconciling:
			w.service.recordSourceTarget(target, DirtyReconciling, WorkspaceReconciling)
			_ = w.service.ReconcileObserved(ctx)
			window.retryAt = w.clock.Now().Add(watcherRetry)
		default:
			if sourceCandidateRejected(activationErr) {
				w.service.recordSourceTarget(target, DirtyRejected, CandidateRejected)
				window.retryAt = w.clock.Now().Add(watcherRetry)
			} else {
				w.service.recordSourceTarget(target, DirtyUnvalidated, ActivationUnavailable)
				window.retryAt = w.clock.Now().Add(watcherRetry)
			}
		}
	}
	for group := range state.windows {
		if !seen[group] {
			delete(state.windows, group)
		}
	}
}
func sourceCandidateRejected(err error) bool {
	if errors.Is(err, configactivation.ErrInvalidStartup) || errors.Is(err, authorization.ErrInvalid) {
		return true
	}
	var failure *Error
	return errors.As(err, &failure) && failure.Kind() == FailureInvalid
}

// One global declarative bundle is selected by the generation's typed locator.
// Explicit legacy absence is unobserved; a zero locator or mixed profile is an
// integrity failure and can never select a guessed workspace root.
func declarativeSourceWatchTarget(active storage.ConfigGenerationSnapshot) (sourceWatchTarget, bool, error) {
	locator := active.DeclarativeWorkspace()
	if !locator.Valid() {
		return sourceWatchTarget{}, false, &Error{kind: Unavailable}
	}
	group, present := locator.Group()
	if !present {
		return sourceWatchTarget{}, false, nil
	}
	if !group.Valid() || reservedSourceWorkspaceGroup(group) {
		return sourceWatchTarget{}, false, &Error{kind: Unavailable}
	}
	target := sourceWatchTarget{group: group, entry: active.Entry()}
	entryPresent := false
	seenIDs, seenPaths := map[storage.ConfigDocumentID]bool{}, map[string]bool{}
	for _, version := range active.Versions() {
		kind, present := version.Kind()
		if !present {
			return sourceWatchTarget{}, false, &Error{kind: Unavailable}
		}
		if kind != storage.ConfigDocumentKindYAMLSource {
			continue
		}
		schema, hasSchema := version.Schema()
		profile, hasProfile := version.Profile()
		variant := sourceWatchInvalid
		if hasSchema && hasProfile && schema == bundle.SchemaDeclarative1 && profile == parser.SyntaxProfileDeclarative {
			variant = sourceWatchDeclarative1
		}
		if hasSchema && hasProfile && schema == bundle.SchemaDeclarative2 && profile == parser.SyntaxProfileDeclarative2 {
			variant = sourceWatchDeclarative2
		}
		if variant == sourceWatchInvalid || target.variant != sourceWatchInvalid && target.variant != variant || len(target.members) >= 64 {
			return sourceWatchTarget{}, false, &Error{kind: Unavailable}
		}
		binding, err := storage.NewConfigDocumentBinding(version.Ref().Document(), version.Path())
		if err != nil || seenIDs[binding.ID()] || seenPaths[binding.Path()] || version.SHA256() == ([32]byte{}) {
			return sourceWatchTarget{}, false, &Error{kind: Unavailable}
		}
		seenIDs[binding.ID()], seenPaths[binding.Path()] = true, true
		target.variant = variant
		entryPresent = entryPresent || binding.Path() == target.entry
		target.members = append(target.members, sourceWatchMember{binding: binding, revision: version.Ref().Revision(), sql: version.SHA256()})
	}
	if target.variant == sourceWatchInvalid || !entryPresent {
		return sourceWatchTarget{}, false, &Error{kind: Unavailable}
	}
	return target, true, nil
}

func compileObservedDeclarative(target sourceWatchTarget, files []parser.SourceFile) (bundle.Candidate, error) {
	if !target.group.Valid() || reservedSourceWorkspaceGroup(target.group) || target.entry == "" || len(files) != len(target.members) {
		return bundle.Candidate{}, &Error{kind: FailureInvalid}
	}
	switch target.variant {
	case sourceWatchDeclarative1:
		return bundle.CompileSources(target.entry, files)
	case sourceWatchDeclarative2:
		return bundle.CompileSourcesV2(target.entry, files)
	default:
		return bundle.Candidate{}, &Error{kind: FailureInvalid}
	}
}
