package configdocuments

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/storage"
)

// DiagnosticKind is admitted through the private constructor below. These are
// public-safe classifications, never parser text or executable configuration.
type DiagnosticKind uint8

const (
	DiagnosticUnknown DiagnosticKind = iota
	CandidateRejected
	WorkspaceChanged
	WorkspaceReconciling
	ActivationUnavailable
)

type WorkspaceDiagnostic struct {
	document storage.ConfigDocumentID
	hash     [32]byte
	updated  time.Time
	kind     DiagnosticKind
}

func newWorkspaceDiagnostic(observation Observation, kind DiagnosticKind) (WorkspaceDiagnostic, error) {
	hash, present := observation.SHA256()
	updated, dated := observation.UpdatedAt()
	if !present || !dated || hash == ([32]byte{}) || updated.IsZero() {
		return WorkspaceDiagnostic{}, &Error{kind: FailureInvalid}
	}
	switch kind {
	case CandidateRejected, WorkspaceChanged, WorkspaceReconciling, ActivationUnavailable:
		return WorkspaceDiagnostic{document: operationalID(), hash: hash, updated: updated, kind: kind}, nil
	case DiagnosticUnknown:
		return WorkspaceDiagnostic{}, &Error{kind: FailureInvalid}
	default:
		return WorkspaceDiagnostic{}, &Error{kind: FailureInvalid}
	}
}
func (d WorkspaceDiagnostic) Document() storage.ConfigDocumentID { return d.document }
func (d WorkspaceDiagnostic) SourceSHA256() [32]byte             { return d.hash }
func (d WorkspaceDiagnostic) UpdatedAt() time.Time               { return d.updated }
func (d WorkspaceDiagnostic) Kind() DiagnosticKind               { return d.kind }
func (d WorkspaceDiagnostic) matches(observation Observation) bool {
	hash, present := observation.SHA256()
	updated, dated := observation.UpdatedAt()
	return present && dated && d.document == operationalID() && d.hash == hash && d.updated.Equal(updated) && d.kind != DiagnosticUnknown
}

func capturedRejectionDiagnostic(observation Observation, err error) DiagnosticKind {
	var rejection *config.WorkspaceRejection
	if !errors.As(err, &rejection) {
		return ActivationUnavailable
	}
	hash, admitted := rejection.SourceSHA256()
	if !admitted {
		return ActivationUnavailable
	}
	if !observation.present || hash != observation.hash {
		return WorkspaceChanged
	}
	return CandidateRejected
}

func activationDiagnostic(observation Observation, result configactivation.Result, err error) DiagnosticKind {
	if err == nil && result.Outcome() == configactivation.Published {
		return DiagnosticUnknown
	}
	switch {
	case errors.Is(err, config.ErrStartupSourceChanged), errors.Is(err, configactivation.ErrWorkspacePreconditionConflict), errors.Is(err, configactivation.ErrStartupConflict):
		return WorkspaceChanged
	case result.Outcome() == configactivation.CommitUnknown, result.Outcome() == configactivation.CommittedReconciling:
		return WorkspaceReconciling
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, configactivation.ErrActivationBusy), errors.Is(err, configactivation.ErrActivationUnavailable), errors.Is(err, configactivation.ErrStartupIntegrity):
		return ActivationUnavailable
	case result.Outcome() == configactivation.RejectedBeforeCommit:
		return capturedRejectionDiagnostic(observation, err)
	default:
		return ActivationUnavailable
	}
}

// recordObservedResult binds the diagnostic to the observation used for this
// attempt. Read compares BOTH digest and date, so a late rejection of A cannot
// become the diagnostic of an independently edited B. Clean always wins.
func (s *Service) recordObservedResult(observation Observation, result configactivation.Result, err error) {
	kind := activationDiagnostic(observation, result, err)
	diagnostic, admissionErr := newWorkspaceDiagnostic(observation, kind)
	s.dirtyMu.Lock()
	defer s.dirtyMu.Unlock()
	if kind == DiagnosticUnknown {
		s.diagnostic = WorkspaceDiagnostic{}
		s.rejected = [32]byte{}
		return
	}
	if admissionErr != nil {
		return
	}
	s.diagnostic = diagnostic
	if kind == CandidateRejected {
		s.rejected = diagnostic.hash
	} else {
		s.rejected = [32]byte{}
	}
}

// ReconcileObserved recovers the existing owner's unresolved publication before
// examining a persisted workspace intent. It never rewrites a workspace path.
// It deliberately cannot supply a missing trusted Logic1 program composition.
func (s *Service) ReconcileObserved(ctx context.Context) error {
	release, err := s.acquireMutation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if _, err := s.owner.CurrentConfig(ctx); err != nil {
		if _, reconcileErr := s.owner.Reconcile(ctx); reconcileErr != nil {
			return reconcileErr
		}
	}
	if reconciled, err := s.reconcileSourceSetLocked(ctx); reconciled || err != nil {
		return err
	}
	current, err := s.Read(ctx, operationalID())
	if err != nil {
		return err
	}
	_, err = s.workspace.Reconcile(current.document.SourceSHA256())
	return err
}
