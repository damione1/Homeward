package configdocuments

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

// ConfirmOperationalNoChange is trusted integration composition. The encoder
// captures only the previously admitted redacted definition; it performs no I/O
// or native lookups. Empty/same-mask detection must happen before this method,
// while this method independently rechecks all original native preconditions.
func (s *Service) ConfirmOperationalNoChange(ctx context.Context, expected ExpectedDocument, author storage.ConfigAuthor, encode func(Snapshot, storage.ConfigurationAPIRequestRecord, time.Time) ([]byte, error)) (configactivation.Result, error) {
	if !expected.valid() || !author.Valid() || encode == nil {
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
	defer s.workspace.mu.Unlock()
	if s.workspace.closed {
		return configactivation.Result{}, &Error{kind: Unavailable}
	}
	source, err := s.workspace.read(s.workspace.name, 1<<20)
	if err != nil {
		return configactivation.Result{}, err
	}
	if source.SHA256() != expected.workspace {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	return s.owner.ConfirmIntegrationNoChange(ctx, author, current.Generation(), current.ManifestSHA256(), current.Document().Ref(), current.Document().SourceSHA256(), func(record storage.ConfigurationAPIRequestRecord, at time.Time) ([]byte, error) {
		return encode(current, record, at)
	})
}

// ConfirmsDefinition compares typed native source identities. Workspace bytes
// retain their independent final CAS; this predicate does not admit a mutation.
func (e ExpectedDocument) ConfirmsDefinition(snapshot Snapshot) bool {
	return e.valid() && e.id == snapshot.Document().Ref().Document() && e.revision == snapshot.Document().Ref().Revision() && e.source == snapshot.Document().SourceSHA256()
}
