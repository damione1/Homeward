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

// RollbackAuthorizationVersion selects exact retained documentary bytes, then
// enters the ordinary policy apply path with the current document/workspace and
// active-policy CAS. Historical grants never authorize the rollback caller.
func (s *Service) RollbackAuthorizationVersion(ctx context.Context, identity string, expectedOperational configdocuments.ExpectedDocument, expected []configdocuments.ExpectedSource, historical authorization.Version, author storage.ConfigAuthor) (configactivation.Result, error) {
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
	source, err := s.documents.Store().ReadAuthorizationPolicyVersion(ctx, account, historical)
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
