package configdocuments

import (
	"context"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

// ReadOperationalSource is trusted composition, never an RPC source response.
// The existing replay owner reads the exact sealed active member. No second
// store, locator, or public source representation is introduced.
func (s *Service) ReadOperationalSource(ctx context.Context) (Snapshot, storage.ConfigProtectedSource, error) {
	current, err := s.Read(ctx, operationalID())
	if err != nil {
		return Snapshot{}, storage.ConfigProtectedSource{}, err
	}
	source, err := s.replay.ReadActiveProtectedSource(ctx, current.Generation(), current.ManifestSHA256(), current.Document().Ref())
	if err != nil {
		return Snapshot{}, storage.ConfigProtectedSource{}, err
	}
	after, err := s.Read(ctx, operationalID())
	if err != nil {
		return Snapshot{}, storage.ConfigProtectedSource{}, err
	}
	if current.Generation() != after.Generation() || current.ManifestSHA256() != after.ManifestSHA256() || current.Document().Ref() != after.Document().Ref() || source.SHA256() != after.Document().SourceSHA256() {
		return Snapshot{}, storage.ConfigProtectedSource{}, &Error{kind: Conflict}
	}
	return after, source, nil
}

// ReadOperationalWorkspace prepares a source-bound transformation outside the
// mutation callback. Full operational admission may stat a declared executable;
// that I/O must not happen in the bounded callback under the149 mutation lock.
// TransformOperational rechecks the exact same three preconditions and bytes.
func (s *Service) ReadOperationalWorkspace(ctx context.Context, expected ExpectedDocument) (ExactSource, error) {
	if !expected.valid() {
		return ExactSource{}, &Error{kind: FailureInvalid}
	}
	// A149 publication temporarily moves SQL, owner and workspace stamps. Use
	// that same mutation gate while capturing preparation input so a concurrent
	// writer cannot exhaust Read's coherent-cut retries and mask a CAS conflict
	// as an unavailable service. Full candidate admission stays outside the gate.
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return ExactSource{}, err
	}
	defer release()
	current, err := s.Read(ctx, expected.id)
	if err != nil {
		return ExactSource{}, err
	}
	observed, present := current.Workspace().SHA256()
	if current.Document().Ref().Revision() != expected.revision || current.Document().SourceSHA256() != expected.source || (present && observed != expected.workspace) {
		return ExactSource{}, &Error{kind: Conflict}
	}
	if !present || current.Dirty() == DirtyUnavailable || current.Workspace().ReconciliationPending() {
		return ExactSource{}, &Error{kind: ReconciliationRequired}
	}
	s.workspace.mu.Lock()
	defer s.workspace.mu.Unlock()
	if s.workspace.closed {
		return ExactSource{}, &Error{kind: Unavailable}
	}
	source, err := s.workspace.read(s.workspace.name, 1<<20)
	if err != nil {
		return ExactSource{}, err
	}
	if source.SHA256() != expected.workspace {
		return ExactSource{}, &Error{kind: Conflict}
	}
	return source, nil
}

// TransformOperational holds the SAME149 mutation lock and delegates publication
// to writeLocked/029. transform is trusted, bounded, memory-only composition;
// its source is the exact observed workspace, including unrelated dirty edits.
func (s *Service) TransformOperational(ctx context.Context, expected ExpectedDocument, author storage.ConfigAuthor, transform func(ExactSource) ([]byte, error)) (configactivation.Result, error) {
	if !expected.valid() || !author.Valid() || transform == nil {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return configactivation.Result{}, err
	}
	defer release()
	current, err := s.Read(ctx, expected.id)
	if err != nil {
		return configactivation.Result{}, err
	}
	observed, present := current.Workspace().SHA256()
	if current.Document().Ref().Revision() != expected.revision || current.Document().SourceSHA256() != expected.source || (present && observed != expected.workspace) {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	if !present || current.Dirty() == DirtyUnavailable || current.Workspace().ReconciliationPending() {
		return configactivation.Result{}, &Error{kind: ReconciliationRequired}
	}
	s.workspace.mu.Lock()
	if s.workspace.closed {
		s.workspace.mu.Unlock()
		return configactivation.Result{}, &Error{kind: Unavailable}
	}
	source, err := s.workspace.read(s.workspace.name, 1<<20)
	s.workspace.mu.Unlock()
	if err != nil {
		return configactivation.Result{}, err
	}
	if source.SHA256() != expected.workspace {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	raw, err := transform(source)
	if err != nil {
		return configactivation.Result{}, err
	}
	if len(raw) < 1 || len(raw) > 1<<20 {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	return s.writeLocked(ctx, expected, raw, author)
}
