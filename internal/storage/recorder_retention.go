package storage

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
)

func readRecorderRetentionTx(ctx context.Context, tx pgx.Tx, out *RecorderCoverageSnapshot, limits recorderCoverageLimits, remaining *int) error {
	rows, err := tx.Query(ctx, `SELECT r.epoch,r.from_sequence::text,r.through_sequence::text,r.snapshot FROM public.recorder_retention_ranges r JOIN public.recorder_epochs e ON e.epoch=r.epoch WHERE e.run_id BETWEEN (SELECT run_id FROM public.recorder_epochs WHERE epoch=$1) AND (SELECT run_id FROM public.recorder_epochs WHERE epoch=$2) AND (r.epoch<>$1 OR r.through_sequence>$3::numeric OR (r.snapshot AND r.through_sequence=$3::numeric)) AND (r.epoch<>$2 OR r.from_sequence<=$4::numeric) ORDER BY e.run_id,r.from_sequence,r.through_sequence,r.snapshot LIMIT $5`, out.fromEpoch.data(), out.throughEpoch.data(), decimalUint(out.from), decimalUint(out.through), limits.gaps+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		var from, through string
		var r RecorderRetentionRange
		if err = rows.Scan(&raw, &from, &through, &r.snapshot); err != nil {
			return err
		}
		if r.epoch, err = coverageEpoch(raw); err != nil {
			return err
		}
		if r.from, err = coverageUint(from); err != nil {
			return err
		}
		if r.through, err = coverageUint(through); err != nil {
			return err
		}
		if r.from > r.through || (!r.snapshot && r.from == 0) {
			return ErrRecorderCoverageIntegrity
		}
		if len(out.retention)+len(out.gaps) >= limits.gaps {
			*out = coverageTruncated(RecorderCoverageGapLimit)
			return nil
		}
		*remaining -= 96
		if *remaining < 0 {
			*out = coverageTruncated(RecorderCoverageByteLimit)
			return nil
		}
		out.retention = append(out.retention, r)
	}
	return rows.Err()
}

// recordRetentionRange requires recorderMaintenanceLock. Coalescing joins only
// overlapping/adjacent positions, never a retained observation in between.
func recordRetentionRange(ctx context.Context, tx pgx.Tx, epoch RecorderEpoch, from, through uint64, snapshot bool, cutoff time.Time) error {
	_, err := tx.Exec(ctx, `WITH merged AS (DELETE FROM public.recorder_retention_ranges WHERE epoch=$1 AND snapshot=$4 AND from_sequence<=$3::numeric+1 AND through_sequence+1>=$2::numeric RETURNING from_sequence,through_sequence), bounds AS (SELECT LEAST($2::numeric,COALESCE(min(from_sequence),$2::numeric)) AS f,GREATEST($3::numeric,COALESCE(max(through_sequence),$3::numeric)) AS t FROM merged) INSERT INTO public.recorder_retention_ranges(epoch,from_sequence,through_sequence,snapshot,cutoff) SELECT $1,f,t,$4,$5 FROM bounds ON CONFLICT(epoch,from_sequence,through_sequence,snapshot) DO UPDATE SET cutoff=GREATEST(recorder_retention_ranges.cutoff,EXCLUDED.cutoff)`, epoch.data(), decimalUint(from), decimalUint(through), snapshot, cutoff)
	return err
}

func (s *Store) PurgeRecorderRaw(ctx context.Context, cutoff time.Time) (int64, bool, error) {
	if cutoff.IsZero() || cutoff.Location() != time.UTC {
		return 0, false, recordermaintenance.ErrInvalid
	}
	cutoff = cutoff.Truncate(time.Microsecond)
	var deleted int64
	var more bool
	outcome, err := s.maintenanceTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `UPDATE public.recorder_maintenance_head SET raw_cutoff=GREATEST(raw_cutoff,$1) WHERE singleton=true RETURNING raw_cutoff`, cutoff).Scan(&cutoff); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT epoch,sequence::text,octet_length(entity_id),COALESCE(octet_length(measurement_unit),0),COALESCE(octet_length(measurement_canonical_unit),0) FROM public.state_history WHERE received_at<$1 ORDER BY received_at,epoch,sequence LIMIT 500`, cutoff)
		if err != nil {
			return err
		}
		type key struct {
			epoch    RecorderEpoch
			sequence uint64
		}
		keys := make([]key, 0, 500)
		charge := 0
		for rows.Next() {
			var raw []byte
			var seq string
			var entityBytes, unitBytes, canonicalBytes int64
			if err = rows.Scan(&raw, &seq, &entityBytes, &unitBytes, &canonicalBytes); err != nil {
				rows.Close()
				return err
			}
			cost, e := numericSourceCharge(entityBytes, unitBytes, canonicalBytes)
			if e != nil {
				rows.Close()
				return e
			}
			if cost > recordermaintenance.MaxInputBytes-charge {
				break
			}
			charge += cost
			epoch, e := coverageEpoch(raw)
			if e != nil {
				rows.Close()
				return e
			}
			n, e := coverageUint(seq)
			if e != nil {
				rows.Close()
				return e
			}
			keys = append(keys, key{epoch, n})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, k := range keys {
			if deleted > 0 && maintenanceBudgetLow(ctx) {
				break
			}
			// Preserve the historical descriptor/quality even if analytical
			// catch-up could not consume this row within its separate budget.
			if err = preserveExpiredNumeric(ctx, tx, k.epoch, k.sequence); err != nil {
				return err
			}
			if err = recordRetentionRange(ctx, tx, k.epoch, k.sequence, k.sequence, false, cutoff); err != nil {
				return err
			}
			tag, e := tx.Exec(ctx, `DELETE FROM public.state_history WHERE epoch=$1 AND sequence=$2::numeric`, k.epoch.data(), decimalUint(k.sequence))
			if e != nil {
				return e
			}
			deleted += tag.RowsAffected()
		}
		remaining := 500 - deleted
		if maintenanceBudgetLow(ctx) {
			remaining = 0
		}
		rows, err = tx.Query(ctx, `SELECT epoch,boundary_sequence::text,entity_digest FROM public.recorder_snapshots WHERE received_at<$1 ORDER BY received_at,epoch,boundary_sequence,entity_digest LIMIT $2`, cutoff, remaining)
		if err != nil {
			return err
		}
		type imageKey struct {
			k      key
			digest []byte
		}
		images := make([]imageKey, 0, remaining)
		for rows.Next() {
			var raw, digest []byte
			var seq string
			if err = rows.Scan(&raw, &seq, &digest); err != nil {
				rows.Close()
				return err
			}
			epoch, e := coverageEpoch(raw)
			if e != nil {
				rows.Close()
				return e
			}
			n, e := coverageUint(seq)
			if e != nil {
				rows.Close()
				return e
			}
			images = append(images, imageKey{key{epoch, n}, digest})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, im := range images {
			if deleted > 0 && maintenanceBudgetLow(ctx) {
				break
			}
			if err = recordRetentionRange(ctx, tx, im.k.epoch, im.k.sequence, im.k.sequence, true, cutoff); err != nil {
				return err
			}
			tag, e := tx.Exec(ctx, `DELETE FROM public.recorder_snapshots WHERE epoch=$1 AND boundary_sequence=$2::numeric AND entity_digest=$3`, im.k.epoch.data(), decimalUint(im.k.sequence), im.digest)
			if e != nil {
				return e
			}
			deleted += tag.RowsAffected()
		}
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.state_history WHERE received_at<$1 UNION ALL SELECT 1 FROM public.recorder_snapshots WHERE received_at<$1)`, cutoff).Scan(&more)
	})
	if err != nil {
		return 0, false, maintenanceFailure(outcome, err)
	}
	return deleted, more, nil
}

func (s *Store) PurgeRecorderAggregates(ctx context.Context, cutoff time.Time) (int64, bool, error) {
	if cutoff.IsZero() || cutoff.Location() != time.UTC {
		return 0, false, recordermaintenance.ErrInvalid
	}
	var deleted int64
	var more bool
	outcome, err := s.maintenanceTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `WITH selected AS (SELECT entity_digest,bucket_start,group_digest FROM public.recorder_hourly_aggregates WHERE bucket_end<=$1 ORDER BY bucket_end,entity_digest,bucket_start,group_digest LIMIT 500) DELETE FROM public.recorder_hourly_aggregates a USING selected s WHERE a.entity_digest=s.entity_digest AND a.bucket_start=s.bucket_start AND a.group_digest=s.group_digest`, cutoff)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		tag, err = tx.Exec(ctx, `WITH selected AS (SELECT entity_digest FROM public.recorder_numeric_continuations WHERE last_observation_at<$1 ORDER BY last_observation_at,entity_digest LIMIT $2) DELETE FROM public.recorder_numeric_continuations c USING selected s WHERE c.entity_digest=s.entity_digest`, cutoff, 500-deleted)
		if err != nil {
			return err
		}
		deleted += tag.RowsAffected()
		// A point at the cutoff belongs to the first retained half-open
		// window. A nondegenerate interval ending there is fully expired.
		tag, err = tx.Exec(ctx, `WITH selected AS (SELECT entity_digest,start_at,end_at FROM public.recorder_analytical_breaks WHERE (end_at<$1 OR (end_at=$1 AND start_at<end_at)) ORDER BY end_at,entity_digest,start_at LIMIT $2) DELETE FROM public.recorder_analytical_breaks b USING selected s WHERE b.entity_digest=s.entity_digest AND b.start_at=s.start_at AND b.end_at=s.end_at`, cutoff, 500-deleted)
		if err != nil {
			return err
		}
		deleted += tag.RowsAffected()
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.recorder_hourly_aggregates WHERE bucket_end<=$1 UNION ALL SELECT 1 FROM public.recorder_numeric_continuations WHERE last_observation_at<$1 UNION ALL SELECT 1 FROM public.recorder_analytical_breaks WHERE (end_at<$1 OR (end_at=$1 AND start_at<end_at)))`, cutoff).Scan(&more)
	})
	if err != nil {
		return 0, false, maintenanceFailure(outcome, err)
	}
	return deleted, more, nil
}

func (s *Store) PurgeRecorderAudit(ctx context.Context, cutoff time.Time) (int64, bool, error) {
	if cutoff.IsZero() || cutoff.Location() != time.UTC {
		return 0, false, recordermaintenance.ErrInvalid
	}
	var deleted int64
	var more bool
	outcome, err := s.maintenanceTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `WITH selected AS (SELECT id FROM public.auth_audit WHERE occurred_at<$1 ORDER BY occurred_at,id LIMIT 500) DELETE FROM public.auth_audit a USING selected s WHERE a.id=s.id`, cutoff)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		tag, err = tx.Exec(ctx, `WITH selected AS (SELECT id FROM public.secret_access_audit WHERE recorded_at<$1 ORDER BY recorded_at,id LIMIT $2) DELETE FROM public.secret_access_audit a USING selected s WHERE a.id=s.id`, cutoff, 500-deleted)
		if err != nil {
			return err
		}
		deleted += tag.RowsAffected()
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.auth_audit WHERE occurred_at<$1 UNION ALL SELECT 1 FROM public.secret_access_audit WHERE recorded_at<$1)`, cutoff).Scan(&more)
	})
	if err != nil {
		return 0, false, maintenanceFailure(outcome, err)
	}
	return deleted, more, nil
}
