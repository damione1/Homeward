package storage

import (
	"context"
	"github.com/damione1/Homeward/internal/historyquery"
	"github.com/jackc/pgx/v5"
)

// Coverage borrows the point transaction; no nested reader gate or snapshot.
// All logical epochs through the frozen high-water are relevant because 031
// never measured a UTC placement for its positional gaps. Bounds are explicit.
func readHistoricalCoverage(ctx context.Context, tx pgx.Tx, c *RecorderQueryCursor) (RecorderCoverageSnapshot, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT epoch FROM public.recorder_epochs WHERE run_id<=$1 ORDER BY run_id LIMIT 1`, c.highRun).Scan(&raw); err != nil {
		return RecorderCoverageSnapshot{}, historyFailure(historyquery.ErrUnavailable, err)
	}
	first, err := coverageEpoch(raw)
	if err != nil {
		return RecorderCoverageSnapshot{}, historyquery.ErrIntegrity
	}
	request, err := NewRecorderCoverageRequest(first, 0, c.highEpoch, &c.highSequence)
	if err != nil {
		return RecorderCoverageSnapshot{}, historyquery.ErrIntegrity
	}
	limits := recorderCoverageProductionLimits
	limits.bytes = historyquery.CoverageBytes
	snapshot, err := readRecorderCoverageTx(ctx, ctx, tx, request, limits)
	if err != nil {
		return RecorderCoverageSnapshot{}, historyFailure(historyquery.ErrUnavailable, err)
	}
	if snapshot.Kind() == RecorderCoverageReadComplete && c.liveSequence > c.highSequence {
		// Preserve the originally captured durable prefix even if a later page
		// sees its queued suffix committed.031 then projects UncommittedTail;
		// neither points nor the stable pagination high-water are advanced.
		// Retention ranges are coalesced by the writer, including late
		// expired positions beyond this cursor. Project them onto the same
		// frozen durable prefix before031 validates the checkpoint.
		retention := snapshot.retention[:0]
		for _, range_ := range snapshot.retention {
			if range_.epoch == c.highEpoch {
				if range_.from > c.highSequence {
					continue
				}
				if range_.through > c.highSequence {
					range_.through = c.highSequence
				}
			}
			retention = append(retention, range_)
		}
		snapshot.retention = retention
		snapshot.through = c.liveSequence
		for i := range snapshot.epochs {
			if snapshot.epochs[i].epoch == c.highEpoch {
				snapshot.epochs[i].checkpoint = c.highSequence
				snapshot.epochs[i].policyExcluded = c.highPolicyExcluded
			}
		}
	}
	return snapshot, nil
}
