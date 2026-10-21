package jobs

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// FenceRestoredJobs belongs to the installation restore transaction. It never
// claims that work absent from an old archive did not execute after that cut.
// Every archived nonterminal occurrence is closed before a scheduler starts.
func FenceRestoredJobs(ctx context.Context, tx pgx.Tx) error {
	if ctx == nil || tx == nil {
		return failure(Invalid)
	}
	_, err := tx.Exec(ctx, `UPDATE public.scheduled_jobs SET state=CASE WHEN state=$1 THEN $2::smallint ELSE $3::smallint END,lease_until=NULL,updated_at=clock_timestamp() WHERE state IN ($4,$5,$1)`, int16(Dispatching), int16(Indeterminate), int16(Skipped), int16(Pending), int16(Claimed))
	if err != nil {
		return failure(Unavailable)
	}
	return nil
}
