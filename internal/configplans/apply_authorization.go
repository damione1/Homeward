package configplans

import (
	"context"

	"github.com/damione1/Homeward/compat/ha/parser"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

// ApplyAuthorization shares optional-preview expiry and149/029 mutation
// admission with other source edits. The receipt is audit provenance, never
// permission. The server must independently admit the current admin permit.
func (s *Service) ApplyAuthorization(ctx context.Context, candidate Candidate, receipt string, expectedOperational configdocuments.ExpectedDocument, expected []configdocuments.ExpectedSource, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !candidate.Valid() || candidate.family != Authorization || len(candidate.files) != 1 || len(expected) != 1 {
		return configactivation.Result{}, ErrInvalid
	}
	account, err := contracts.ParseAccountID(candidate.identity)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	binding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil || candidate.entry != binding.Path() || expected[0].Binding() != binding {
		return configactivation.Result{}, ErrInvalid
	}
	source, err := authorization.AdmitPolicySource(account, candidate.files[0].Bytes())
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	plan, basis, err := s.applyBasis(ctx, candidate, receipt, author)
	if err != nil {
		return configactivation.Result{}, err
	}
	if plan.base.kind != ActiveSQL || plan.base.revision <= 0 {
		return configactivation.Result{}, ErrUnavailable
	}
	version, err := authorization.NewVersion(uint64(plan.base.revision), plan.base.source)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	policyExpected, err := authorization.ExpectVersion(version)
	if err != nil || !expected[0].MatchesPolicyVersion(policyExpected) {
		return configactivation.Result{}, ErrStale
	}
	return s.documents.WritePolicy(ctx, source, policyExpected, expectedOperational, expected[0], author, basis)
}

// RollbackAuthorization restores the exact after-version audited by a retained
// activation. It creates a new policy/config revision through the same writer;
// an untracked generation or purged source has no inferred replacement.
func (s *Service) RollbackAuthorization(ctx context.Context, identity string, expectedOperational configdocuments.ExpectedDocument, expected []configdocuments.ExpectedSource, generation storage.ConfigGenerationRevision, manifest [32]byte, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !s.valid() || !usableContext(ctx) || len(expected) != 1 || !author.Valid() {
		return configactivation.Result{}, ErrInvalid
	}
	account, err := contracts.ParseAccountID(identity)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	binding, err := storage.AuthorizationPolicyBinding(account)
	if err != nil || expected[0].Binding() != binding {
		return configactivation.Result{}, ErrInvalid
	}
	source, err := s.documents.Store().ReadActivationAuthorizationSource(ctx, account, generation, manifest)
	if err != nil {
		return configactivation.Result{}, err
	}
	raw, err := source.Bytes()
	if err != nil {
		return configactivation.Result{}, ErrUnavailable
	}
	file, err := parser.NewSourceFile(binding.Path(), raw)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	candidate, err := NewCandidate(Authorization, identity, binding.Path(), []parser.SourceFile{file})
	if err != nil {
		return configactivation.Result{}, err
	}
	return s.ApplyAuthorization(ctx, candidate, "", expectedOperational, expected, author)
}
