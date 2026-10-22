package configplans

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/configdocuments"
	"github.com/damione1/Homeward/internal/storage"
)

// ErrAuthorMismatch identifies an audit receipt belonging to another human.
// Possession of a valid receipt never grants the current caller permission.
var ErrAuthorMismatch = errors.New("configuration apply: receipt author differs")

// Apply revalidates an optional preview before entering the existing writer.
// The server independently admits current human/API permissions. A caller
// without a preview retains the document/workspace CAS of ordinary YAML edits.
func (s *Service) Apply(ctx context.Context, candidate Candidate, receipt string, expected configdocuments.ExpectedDocument, author storage.ConfigAuthor) (configactivation.Result, error) {
	if !s.valid() || !usableContext(ctx) || !candidate.Valid() || candidate.family != Operational || candidate.identity != "operational.main" || len(candidate.files) != 1 {
		return configactivation.Result{}, ErrInvalid
	}
	name, err := usableAuthor(author)
	if err != nil {
		return configactivation.Result{}, err
	}
	if receipt == "" {
		return s.documents.Write(ctx, expected, candidate.files[0].Bytes(), author)
	}
	signed, err := s.verify(receipt, time.Now().UTC())
	if err != nil {
		return configactivation.Result{}, err
	}
	if signed.Author != name {
		return configactivation.Result{}, ErrAuthorMismatch
	}
	plan, err := s.Revalidate(ctx, candidate, receipt)
	if err != nil {
		return configactivation.Result{}, err
	}
	if plan.Author() != name {
		return configactivation.Result{}, ErrAuthorMismatch
	}
	if plan.State() == Stale {
		return configactivation.Result{}, ErrStale
	}
	if plan.State() != Admitted || plan.base.kind != ActiveSQL {
		return configactivation.Result{}, ErrInvalid
	}
	generation, err := storage.NewConfigGenerationRevision(plan.generation)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	basis, err := configdocuments.NewWriteBasis(generation, plan.manifest, plan.catalog, time.Unix(signed.Expires, 0))
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	// Derive the document CAS from the authenticated receipt, never a caller's
	// replacement base. This prevents supplying a fresh Expected with an old plan.
	id, _ := storage.NewConfigDocumentID(plan.identity)
	revision, err := storage.NewConfigVersionRevision(plan.base.revision)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	bound, err := configdocuments.NewExpectedDocument(id, revision, plan.base.source, plan.workspace)
	if err != nil {
		return configactivation.Result{}, ErrInvalid
	}
	if bound != expected {
		return configactivation.Result{}, ErrStale
	}
	return s.documents.WritePlanned(ctx, bound, candidate.files[0].Bytes(), author, basis)
}
