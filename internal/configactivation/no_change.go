package configactivation

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/storage"
)

// NoChangeEvidence is native validation, never publication. Its constructor is
// private to Owner and requires both an exact coherent source and the confirmed
// append of its original redacted result under the SQL rights/CAS fence.
type NoChangeEvidence struct {
	record     storage.ConfigurationAPIRequestRecord
	generation storage.ConfigGenerationRevision
	manifest   [32]byte
	ref        storage.ConfigVersionRef
	source     [32]byte
	valid      bool
}

func (e NoChangeEvidence) Valid() bool {
	_, at, done := e.record.NoChange()
	return e.valid && e.record.Valid() && done && !at.IsZero() && e.generation.Valid() && e.manifest != ([32]byte{}) && e.ref.Valid() && e.source != ([32]byte{})
}
func (e NoChangeEvidence) Record() storage.ConfigurationAPIRequestRecord { return e.record }
func (r Result) NoChangeEvidence() (NoChangeEvidence, bool) {
	return r.noChange, r.outcome == NoChange && r.noChange.Valid()
}

// ConfirmIntegrationNoChange uses the same native execution guard as Activate.
// The document owner holds its exact-source mutation gate throughout this call.
// There is no staging, compiler, ConfigTx, publication cut or revision creation.
func (o *Owner) ConfirmIntegrationNoChange(ctx context.Context, author storage.ConfigAuthor, generation storage.ConfigGenerationRevision, manifest [32]byte, ref storage.ConfigVersionRef, source [32]byte, encode func(storage.ConfigurationAPIRequestRecord, time.Time) ([]byte, error)) (Result, error) {
	if !author.Valid() || !generation.Valid() || manifest == ([32]byte{}) || !ref.Valid() || source == ([32]byte{}) || encode == nil {
		return Result{outcome: RejectedBeforeCommit}, ErrInvalidStartup
	}
	bounded, finish, err := o.operation(ctx, false, author)
	if err != nil {
		return withAPIExecution(bounded, Result{outcome: RejectedBeforeCommit}), err
	}
	defer finish()
	execution, present := storage.ConfigurationAPIExecutionFromContext(bounded)
	if !present {
		return Result{outcome: RejectedBeforeCommit}, ErrInvalidStartup
	}
	if err := o.input.Startup.ConfirmBootstrapSource(); err != nil {
		return o.finalizeAPIContext(bounded, Result{outcome: RestartRequired}, err), err
	}
	o.mu.RLock()
	coherent := o.phase == ownerOpen && o.current.Revision() == generation && o.current.ManifestSHA256() == manifest
	o.mu.RUnlock()
	if !coherent {
		return o.finalizeAPIContext(bounded, Result{outcome: RejectedBeforeCommit}, ErrActivationUnavailable), ErrActivationUnavailable
	}
	record, outcome, err := o.input.Store.RecordIntegrationNoChange(bounded, execution, generation, manifest, ref, source, encode)
	if err != nil || outcome != storage.TransactionCommitted {
		result := Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}
		if outcome == storage.TransactionIndeterminate {
			result.outcome = CommitUnknown
			result.transaction = storage.TransactionIndeterminate
		}
		return o.finalizeAPIContext(bounded, result, err), err
	}
	proof := NoChangeEvidence{record: record, generation: generation, manifest: manifest, ref: ref, source: source, valid: true}
	if !proof.Valid() {
		return withAPIExecution(bounded, Result{outcome: CommitUnknown, transaction: storage.TransactionIndeterminate}), ErrInvalidStartup
	}
	return Result{outcome: NoChange, transaction: storage.TransactionNotCommitted, apiExecution: execution, noChange: proof}, nil
}
