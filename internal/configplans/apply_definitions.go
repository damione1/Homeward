package configplans

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/config/bundle"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

// applyBasis binds optional-preview admission to the original receipt's
// expiry. An absent preview is compiled here without introducing UI approval.
func (s *Service) applyBasis(ctx context.Context, candidate Candidate, receipt string, author storage.ConfigAuthor) (Plan, configdocuments.WriteBasis, error) {
	if !s.valid() || !usableContext(ctx) || !candidate.Valid() {
		return Plan{}, configdocuments.WriteBasis{}, ErrInvalid
	}
	name, err := usableAuthor(author)
	if err != nil {
		return Plan{}, configdocuments.WriteBasis{}, err
	}
	var plan Plan
	var expires time.Time
	if receipt == "" {
		plan, err = s.Build(ctx, candidate, author)
		expires = time.Now().Add(30 * time.Minute)
	} else {
		signed, verifyErr := s.verify(receipt, time.Now().UTC())
		if verifyErr != nil {
			return Plan{}, configdocuments.WriteBasis{}, verifyErr
		}
		if signed.Author != name {
			return Plan{}, configdocuments.WriteBasis{}, ErrAuthorMismatch
		}
		expires = time.Unix(signed.Expires, 0)
		plan, err = s.Revalidate(ctx, candidate, receipt)
	}
	if err != nil {
		return Plan{}, configdocuments.WriteBasis{}, err
	}
	if plan.State() == Stale {
		return Plan{}, configdocuments.WriteBasis{}, ErrStale
	}
	if plan.State() != Admitted || plan.author != name {
		return Plan{}, configdocuments.WriteBasis{}, ErrInvalid
	}
	generation, err := storage.NewConfigGenerationRevision(plan.generation)
	if err != nil {
		return Plan{}, configdocuments.WriteBasis{}, ErrInvalid
	}
	basis, err := configdocuments.NewWriteBasis(generation, plan.manifest, plan.catalog, expires)
	if err != nil {
		return Plan{}, configdocuments.WriteBasis{}, ErrInvalid
	}
	return plan, basis, nil
}

func (s *Service) ApplyDefinitions(ctx context.Context, candidate Candidate, receipt string, expectedOperational configdocuments.ExpectedDocument, expected []configdocuments.ExpectedSource, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !candidate.Valid() || candidate.family != Declarative1 && candidate.family != Declarative2 {
		return configactivation.Result{}, ErrInvalid
	}
	_, basis, err := s.applyBasis(ctx, candidate, receipt, author)
	if err != nil {
		return configactivation.Result{}, err
	}
	var admitted bundle.Candidate
	if candidate.family == Declarative1 {
		admitted, err = bundle.CompileSources(candidate.entry, candidate.files)
	} else {
		admitted, err = bundle.CompileSourcesV2(candidate.entry, candidate.files)
	}
	if err != nil || !admitted.Valid() {
		return configactivation.Result{}, ErrInvalid
	}
	group, err := storage.NewConfigDocumentID(candidate.identity)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	return s.documents.WriteDefinitions(ctx, group, admitted, expectedOperational, expected, author, basis)
}
