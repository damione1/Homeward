package configdocuments

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

// MatchesPolicyVersion binds the immutable source CAS to the independently
// active authorization policy version. Neither retained history nor revision
// zero silently supplies a different base.
func (e ExpectedSource) MatchesPolicyVersion(expected authorization.ExpectedVersion) bool {
	absent, version, err := expected.Values()
	if err != nil || !e.sql.valid() || e.sql.absent != absent {
		return false
	}
	if absent {
		return !e.revision.Valid() && e.sql.hash == ([32]byte{})
	}
	revision, hash, err := version.Values()
	return err == nil && e.revision.Number() == int64(revision) && e.sql.hash == hash
}

// WritePolicy uses precisely the existing149 set intent and029 owner. The
// owner reserves the existing policy boundary and ConfigTx performs the active
// policy CAS, source save and audited generation publication in one commit.
func (s *Service) WritePolicy(ctx context.Context, source authorization.PolicySource, policyExpected authorization.ExpectedVersion, expectedOperational ExpectedDocument, expected ExpectedSource, author storage.ConfigAuthor, basis WriteBasis) (configactivation.Result, error) {
	account, err := source.Account()
	if err != nil || !expected.MatchesPolicyVersion(policyExpected) {
		return configactivation.Result{}, &Error{kind: Conflict}
	}
	binding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil || binding != expected.binding {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	raw, err := source.Bytes()
	if err != nil {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	file, err := parser.NewSourceFile(binding.Path(), raw)
	if err != nil {
		return configactivation.Result{}, &Error{kind: FailureInvalid}
	}
	change, err := configactivation.NewPolicyChange(source, policyExpected)
	if err != nil {
		return configactivation.Result{}, err
	}
	return s.writeSourceSet(ctx, binding.ID(), []parser.SourceFile{file}, configactivation.RetainDefinitions(), nil, configactivation.SourceCompilation{}, storage.ConfigDocumentKindAuthorizationPolicy, expectedOperational, []ExpectedSource{expected}, author, basis, []configactivation.PolicyChange{change})
}

// selectedSourceAuthorities uses the existing active policy pointer when a
// policy is outside the active generation's Versions. Other retained documents
// never become an admitted SQL base merely because their latest row exists.
func (s *Service) selectedSourceAuthorities(ctx context.Context, active storage.ConfigGenerationSnapshot, expected []ExpectedSource) (map[storage.ConfigDocumentID]MemberExpectation, error) {
	ids := make([]storage.ConfigDocumentID, 0, len(expected))
	for _, selected := range expected {
		ids = append(ids, selected.binding.ID())
	}
	return s.selectedDocumentAuthorities(ctx, active, ids)
}

func (s *Service) selectedDocumentAuthorities(ctx context.Context, active storage.ConfigGenerationSnapshot, ids []storage.ConfigDocumentID) (map[storage.ConfigDocumentID]MemberExpectation, error) {
	admitted := make(map[storage.ConfigDocumentID]MemberExpectation, len(ids))
	for _, id := range ids {
		admitted[id] = ExpectAbsentMember()
		metadata, err := s.store.ReadConfigDocument(ctx, id)
		if err != nil {
			var failure *storage.Error
			if errors.As(err, &failure) && failure.Kind() == storage.ConfigNotFound {
				continue
			}
			return nil, err
		}
		if metadata.Kind() == storage.ConfigDocumentKindAuthorizationPolicy {
			version, present, err := s.store.ReadActiveAuthorizationVersion(ctx, id)
			if err != nil {
				return nil, err
			}
			if present {
				_, hash, err := version.Values()
				if err != nil {
					return nil, err
				}
				admitted[id], err = ExpectExistingMember(hash)
				if err != nil {
					return nil, err
				}
			}
			continue
		}
		for _, version := range active.Versions() {
			if version.Ref().Document() == id {
				admitted[id], _ = ExpectExistingMember(version.SHA256())
				break
			}
		}
	}
	return admitted, nil
}
