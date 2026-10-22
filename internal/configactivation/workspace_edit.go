package configactivation

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/internal/storage"
)

// ActivateWorkspaceEdit serializes the existing editable-workspace writer with
// the same operation that owns configuration publication. The callback is
// trusted server composition, never an RPC-provided activation capability.
// Its request must already carry the exact SQL/source workspace precondition.
func (o *Owner) ActivateWorkspaceEdit(ctx context.Context, request Request, stage func(context.Context) error) (result Result, err error) {
	if !request.admitted || request.workspacePrecondition == nil || !request.workspacePrecondition.valid() || stage == nil {
		return Result{outcome: RejectedBeforeCommit}, ErrInvalidStartup
	}
	bounded, finish, err := o.operation(ctx, false)
	if err != nil {
		return Result{outcome: RejectedBeforeCommit}, err
	}
	defer finish()
	o.mu.RLock()
	current := o.current
	o.mu.RUnlock()
	p := request.workspacePrecondition
	if !current.Valid() || current.Revision() != p.generation || current.ManifestSHA256() != p.manifest {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, ErrWorkspacePreconditionConflict
	}
	if err := o.input.Startup.ConfirmBootstrapSource(); err != nil {
		return Result{outcome: RestartRequired, transaction: storage.TransactionNotCommitted}, err
	}
	if err := bounded.Err(); err != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
	}
	request, err = o.reserveSourceContext(bounded, request, current)
	if err != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
	}
	reserved := request.sourceReservations
	defer func() { err = errors.Join(err, o.releaseSourceContextReservations(reserved)) }()
	request, err = o.admitEditableSourceContext(bounded, request, current)
	if err != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
	}
	if err := o.confirmEditableSourceComposition(bounded, request, current); err != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
	}
	if err := stage(bounded); err != nil {
		return Result{outcome: RejectedBeforeCommit, transaction: storage.TransactionNotCommitted}, err
	}
	return o.activate(ctx, bounded, request)
}
