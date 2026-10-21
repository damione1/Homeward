package storage

import (
	"context"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
)

func recordAnalyticalBreak(ctx context.Context, tx pgx.Tx, digest []byte, start, end time.Time, reason recordermaintenance.Reason) error {
	if end.Before(start) {
		start, end = end, start
	}
	if !reason.Valid() || reason == 0 {
		return recordermaintenance.ErrInvalid
	}
	_, err := tx.Exec(ctx, `INSERT INTO public.recorder_analytical_breaks(entity_digest,start_at,end_at,reasons) VALUES($1,$2,$3,$4) ON CONFLICT(entity_digest,start_at,end_at) DO UPDATE SET reasons=recorder_analytical_breaks.reasons | EXCLUDED.reasons`, digest, start, end, int32(reason))
	return err
}

// projectRecorderTails runs only after the exact admission barrier has reached
// SQL, and after every state preceding it was consumed. A persisted per-series
// covered_through excludes this suffix from the next observation's segment.
func projectRecorderTails(ctx context.Context, tx pgx.Tx, epoch RecorderEpoch, runID int64, high uint64, at time.Time, updates *int) (bool, error) {
	var after []byte
	if err := tx.QueryRow(ctx, `SELECT tail_entity_digest FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&after); err != nil {
		return false, err
	}
	text, _ := epoch.Hex()
	limit := recordermaintenance.MaxPrefixRows - *updates
	rows, err := tx.Query(ctx, `SELECT entity_id,entity_digest,point,baseline_pending,high_water_seconds,high_water_nanos,covered_through,analytical_refusal FROM public.recorder_numeric_continuations WHERE point->>'Epoch'=$1 AND ($2::bytea IS NULL OR entity_digest>$2) ORDER BY entity_digest LIMIT $3`, text, after, limit+1)
	if err != nil {
		return false, err
	}
	type tail struct {
		row                numericRow
		pending            bool
		highWater, covered time.Time
	}
	batch := make([]tail, 0, limit)
	more := false
	for rows.Next() {
		if len(batch) == limit {
			more = true
			break
		}
		var t tail
		var data []byte
		var entity *string
		var highSeconds int64
		var highNanos int32
		if err = rows.Scan(&entity, &t.row.digest, &data, &t.pending, &highSeconds, &highNanos, &t.covered, &t.row.refused); err != nil {
			rows.Close()
			return false, err
		}
		if t.row.refused && (entity != nil || t.pending) || !t.row.refused && entity == nil {
			rows.Close()
			return false, recordermaintenance.ErrIntegrity
		}
		if entity != nil {
			t.row.entity = *entity
		}
		t.covered = t.covered.UTC()
		t.highWater = time.Unix(highSeconds, int64(highNanos)).UTC()
		if highNanos < 0 || highNanos > 999999999 {
			rows.Close()
			return false, recordermaintenance.ErrIntegrity
		}
		t.row.point, err = decodePoint(data)
		if err != nil {
			rows.Close()
			return false, err
		}
		if t.row.refused && (t.row.point.Input().Kind != domain.ValueUnspecified || t.row.point.Input().Numeric != nil) {
			rows.Close()
			return false, recordermaintenance.ErrIntegrity
		}
		t.row.epoch = epoch
		t.row.runID = runID
		t.row.sequence = high
		batch = append(batch, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	last := after
	for _, t := range batch {
		if *updates >= recordermaintenance.MaxPrefixRows || maintenanceBudgetLow(ctx) {
			more = true
			break
		}
		previousKey := last
		last = t.row.digest
		in := t.row.point.Input()
		if t.row.refused {
			from, covered := t.covered, at
			if from.Before(in.At) {
				from = in.At
			}
			reason := recordermaintenance.UnsupportedNumeric
			if at.Before(from) {
				reason |= recordermaintenance.ClockRegression
				covered = from
			}
			if err = recordAnalyticalBreak(ctx, tx, t.row.digest, from, at, reason); err != nil {
				return false, err
			}
			nextHigh := covered
			if t.highWater.After(nextHigh) {
				nextHigh = t.highWater
			}
			if _, err = tx.Exec(ctx, `UPDATE public.recorder_numeric_continuations SET covered_through=$2,high_water_seconds=$3,high_water_nanos=$4 WHERE entity_digest=$1`, t.row.digest, covered, nextHigh.Unix(), nextHigh.Nanosecond()); err != nil {
				return false, err
			}
			continue
		}
		if t.pending || in.Numeric == nil || in.Numeric.Semantic != domain.SemanticMeasurement {
			continue
		}
		if in.Sequence > high {
			return false, recordermaintenance.ErrIntegrity
		}
		if at.Before(t.covered) {
			if err = recordAnalyticalBreak(ctx, tx, t.row.digest, at, t.covered, recordermaintenance.ClockRegression); err != nil {
				return false, err
			}
			continue
		}
		from := t.covered
		if from.Before(in.At) {
			from = in.At
		}
		if !from.Before(at) {
			continue
		}
		boundaryInput := in
		boundaryInput.Sequence = high
		boundaryInput.At = at
		boundaryPoint, e := recordermaintenance.NewPoint(boundaryInput)
		if e != nil {
			return false, e
		}
		issues, e := edgeCoverage(ctx, tx, t.row.point, boundaryPoint)
		if e != nil {
			return false, e
		}
		start := from.Truncate(time.Hour)
		covered := at
		partial := false
		if at.Sub(start) > time.Duration(recordermaintenance.MaxPrefixRows-*updates)*time.Hour {
			if err = recordAnalyticalBreak(ctx, tx, t.row.digest, from, at, recordermaintenance.BudgetLimit); err != nil {
				return false, err
			}
		} else {
			for start.Before(at) {
				if *updates >= recordermaintenance.MaxPrefixRows || maintenanceBudgetLow(ctx) {
					covered = from
					if start.After(covered) {
						covered = start
					}
					partial = true
					break
				}
				w, e := recordermaintenance.NewWindow(start)
				if e != nil {
					return false, e
				}
				v, e := recordermaintenance.ProjectTail(w, t.row.point, from, at, issues)
				if e != nil {
					return false, e
				}
				if v.Input().Reasons&recordermaintenance.UnsupportedNumeric != 0 {
					refusedFrom, refusedThrough := start, start.Add(time.Hour)
					if from.After(refusedFrom) {
						refusedFrom = from
					}
					if at.Before(refusedThrough) {
						refusedThrough = at
					}
					if e = recordAnalyticalBreak(ctx, tx, t.row.digest, refusedFrom, refusedThrough, recordermaintenance.UnsupportedNumeric); e != nil {
						return false, e
					}
				}
				if v.Input().HasMeasurement || v.Input().Reasons != 0 {
					if e = writeAggregate(ctx, tx, t.row, in, *in.Numeric, w, v, in, updates); e != nil {
						return false, e
					}
				}
				start = start.Add(time.Hour)
			}
		}
		nextHigh := covered
		if t.highWater.After(nextHigh) {
			nextHigh = t.highWater
		}
		if _, err = tx.Exec(ctx, `UPDATE public.recorder_numeric_continuations SET covered_through=$2,high_water_seconds=$3,high_water_nanos=$4 WHERE entity_digest=$1`, t.row.digest, covered, nextHigh.Unix(), nextHigh.Nanosecond()); err != nil {
			return false, err
		}
		if partial {
			last = previousKey
			more = true
			break
		}
	}
	if !more {
		last = nil
	}
	_, err = tx.Exec(ctx, `UPDATE public.recorder_maintenance_head SET tail_entity_digest=$1 WHERE singleton=true`, last)
	return more, err
}
