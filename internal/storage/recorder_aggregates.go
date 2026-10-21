package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/recordermaintenance"
	"github.com/jackc/pgx/v5"
)

// AggregateRecorderPrefix processes a stable logical prefix. The SQL transaction
// owns the cursor and all derived values, so a lost acknowledgement cannot cause
// the next attempt to add a committed contribution a second time.
func (s *Store) AggregateRecorderPrefix(ctx context.Context, b recordermaintenance.Boundary) (int64, bool, error) {
	epochText, high, boundaryAt, _, err := b.Components()
	if err != nil {
		return 0, false, err
	}
	boundaryAt = boundaryAt.Truncate(time.Microsecond)
	epoch, err := ParseRecorderEpoch(epochText)
	if err != nil {
		return 0, false, recordermaintenance.ErrInvalid
	}
	var processed int64
	var more bool
	outcome, err := s.maintenanceTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var oldRun, revision, highRun int64
		var oldText, committedText string
		if e := tx.QueryRow(ctx, `SELECT analytic_run,analytic_sequence::text,revision FROM public.recorder_maintenance_head WHERE singleton=true FOR UPDATE`).Scan(&oldRun, &oldText, &revision); e != nil {
			return e
		}
		oldSequence, e := coverageUint(oldText)
		if e != nil {
			return e
		}
		if e = tx.QueryRow(ctx, `SELECT run_id,checkpoint::text FROM public.recorder_epochs WHERE epoch=$1`, epoch.data()).Scan(&highRun, &committedText); e != nil {
			return e
		}
		checkpoint, e := coverageUint(committedText)
		if e != nil {
			return e
		}
		barrierReached := checkpoint >= high
		if checkpoint < high {
			high = checkpoint
		}
		if oldRun > highRun || (oldRun == highRun && oldSequence > high) {
			return recordermaintenance.ErrIntegrity
		}
		rows, e := tx.Query(ctx, `SELECT h.epoch,h.sequence::text,octet_length(h.entity_id),COALESCE(octet_length(h.measurement_unit),0),COALESCE(octet_length(h.measurement_canonical_unit),0) FROM public.state_history h JOIN public.recorder_epochs e ON e.epoch=h.epoch WHERE (e.run_id>$1 OR (e.run_id=$1 AND h.sequence>$2::numeric)) AND (e.run_id<$3 OR (e.run_id=$3 AND h.sequence<=$4::numeric)) ORDER BY e.run_id,h.sequence LIMIT 501`, oldRun, decimalUint(oldSequence), highRun, decimalUint(high))
		if e != nil {
			return e
		}
		type numericKey struct {
			epoch    []byte
			sequence string
		}
		batch := make([]numericKey, 0, recordermaintenance.MaxPrefixRows)
		charge := 0
		for rows.Next() {
			if len(batch) == recordermaintenance.MaxPrefixRows {
				more = true
				break
			}
			var key numericKey
			var entityBytes, unitBytes, canonicalBytes int64
			if e = rows.Scan(&key.epoch, &key.sequence, &entityBytes, &unitBytes, &canonicalBytes); e != nil {
				rows.Close()
				return e
			}
			cost, e := numericSourceCharge(entityBytes, unitBytes, canonicalBytes)
			if e != nil {
				rows.Close()
				return e
			}
			if cost > recordermaintenance.MaxInputBytes-charge {
				more = true
				break
			}
			charge += cost
			key.epoch = bytes.Clone(key.epoch)
			batch = append(batch, key)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		updates := 0
		var lastProcessed numericRow
		for _, key := range batch {
			if updates >= recordermaintenance.MaxPrefixRows || (processed > 0 && maintenanceBudgetLow(ctx)) {
				more = true
				break
			}
			row, e := scanNumericRow(tx.QueryRow(ctx, `SELECT `+recorderNumericColumns+` FROM public.state_history h JOIN public.recorder_epochs e ON e.epoch=h.epoch WHERE h.epoch=$1 AND h.sequence=$2::numeric`, key.epoch, key.sequence))
			if e != nil {
				return e
			}
			continuation, e := loadContinuation(ctx, tx, row)
			if e != nil {
				return e
			}
			present, pending := continuation != nil, false
			var previous recordermaintenance.Point
			var highWater, coveredThrough time.Time
			if present {
				previous, pending, highWater, coveredThrough = continuation.point, continuation.pending, continuation.highWater, continuation.coveredThrough
			}
			if row.refused || present && continuation.refused {
				start, end := row.point.Input().At, row.point.Input().At
				if present {
					start = previous.Input().At
				}
				if coveredThrough.After(end) {
					end = coveredThrough
				}
				if e = recordAnalyticalBreak(ctx, tx, row.digest, start, end, recordermaintenance.UnsupportedNumeric); e != nil {
					return e
				}
				pending = false
			} else if present && row.point.Input().At.Before(highWater) {
				if e = recordAnalyticalBreak(ctx, tx, row.digest, row.point.Input().At, highWater, recordermaintenance.ClockRegression); e != nil {
					return e
				}
				if e = storePointIssue(ctx, tx, row, recordermaintenance.ClockRegression, &updates); e != nil {
					return e
				}
				pending = true
			} else if present && !pending {
				issues, e := edgeCoverage(ctx, tx, previous, row.point)
				if e != nil {
					return e
				}
				pending, e = projectAndStoreEdge(ctx, tx, row, previous, coveredThrough, issues, &updates)
				if e != nil {
					return e
				}
			} else {
				pending = false
				if row.point.Input().Numeric != nil && row.point.Input().Numeric.Semantic != domain.SemanticMeasurement {
					if e = storePointIssue(ctx, tx, row, recordermaintenance.UnknownBaseline, &updates); e != nil {
						return e
					}
				}
			}
			if e = saveContinuation(ctx, tx, row, pending, highWater, coveredThrough); e != nil {
				return e
			}
			processed++
			lastProcessed = row
		}
		prefixMore := more
		if !prefixMore && barrierReached && updates < recordermaintenance.MaxPrefixRows {
			tailMore, e := projectRecorderTails(ctx, tx, epoch, highRun, high, boundaryAt, &updates)
			if e != nil {
				return e
			}
			more = more || tailMore
		}
		newRun, newSequence := highRun, high
		if prefixMore {
			if len(batch) == 0 {
				return recordermaintenance.ErrBudget
			}
			newRun, newSequence = lastProcessed.runID, lastProcessed.sequence
		}
		tag, e := tx.Exec(ctx, `UPDATE public.recorder_maintenance_head SET analytic_run=$1,analytic_sequence=$2::numeric,revision=revision+1 WHERE singleton=true AND analytic_run=$3 AND analytic_sequence=$4::numeric AND revision=$5`, newRun, decimalUint(newSequence), oldRun, decimalUint(oldSequence), revision)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return recordermaintenance.ErrIntegrity
		}
		return nil
	})
	if err != nil {
		return 0, false, maintenanceFailure(outcome, err)
	}
	return processed, more, nil
}

func edgeCoverage(ctx context.Context, tx pgx.Tx, a, b recordermaintenance.Point) (recordermaintenance.Reason, error) {
	left, right := a.Input(), b.Input()
	if left.Epoch != right.Epoch {
		return recordermaintenance.EpochBoundary, nil
	}
	epoch, err := ParseRecorderEpoch(left.Epoch)
	if err != nil {
		return 0, recordermaintenance.ErrIntegrity
	}
	through := right.Sequence
	request, err := NewRecorderCoverageRequest(epoch, left.Sequence, epoch, &through)
	if err != nil {
		return 0, err
	}
	proof, err := readRecorderCoverageTx(ctx, ctx, tx, request, recorderCoverageProductionLimits)
	if err != nil {
		return 0, err
	}
	if proof.Kind() == RecorderCoverageReadTruncated {
		return recordermaintenance.BudgetLimit, nil
	}
	var issues recordermaintenance.Reason
	if len(proof.Gaps()) != 0 {
		issues |= recordermaintenance.Gap
	}
	if len(proof.Retention()) != 0 {
		issues |= recordermaintenance.RetentionPruned
	}
	for _, e := range proof.Epochs() {
		if e.Checkpoint() < right.Sequence || (e.Checkpoint() == 0 && !e.ReconciliationComplete()) {
			issues |= recordermaintenance.UnprovenTail
		}
	}
	return issues, nil
}

func projectAndStoreEdge(ctx context.Context, tx pgx.Tx, row numericRow, previous recordermaintenance.Point, coveredThrough time.Time, issues recordermaintenance.Reason, updates *int) (bool, error) {
	current := row.point
	edge, err := recordermaintenance.NewEdge(previous, current, issues)
	if err != nil {
		return false, err
	}
	a, b := previous.Input(), current.Input()
	at := b.At.Truncate(time.Hour)
	window, err := recordermaintenance.NewWindow(at)
	if err != nil {
		return false, err
	}
	// Broken edges retain a typed issue at the observation on the right. They
	// do not create a value or spread a counter delta over guessed hours.
	v, err := recordermaintenance.ProjectFrom(window, edge, coveredThrough)
	if err != nil {
		return false, err
	}
	if v.Input().Reasons != 0 || a.Numeric == nil || b.Numeric == nil || a.Numeric.Semantic != domain.SemanticMeasurement {
		if b.Numeric != nil {
			if v.Input().Reasons != 0 {
				input := v.Input()
				input.Reset = recordermaintenance.ResetUnknown
				if b.Numeric.Semantic == domain.SemanticMeasurement {
					input.Reset = recordermaintenance.ResetNotApplicable
				}
				v, err = recordermaintenance.NewValues(input)
				if err != nil {
					return false, err
				}
			}
			if v.Input().HasContribution {
				v, err = recordermaintenance.ConvertValues(v, *a.Numeric, *b.Numeric)
				if err != nil {
					return false, err
				}
			}
			if v.Input().Reasons&recordermaintenance.UnsupportedNumeric != 0 {
				if err = recordAnalyticalBreak(ctx, tx, row.digest, a.At, b.At, recordermaintenance.UnsupportedNumeric); err != nil {
					return false, err
				}
			}
			if err = writeAggregate(ctx, tx, row, b, *b.Numeric, window, v, previous.Input(), updates); err != nil {
				return false, err
			}
		}
		return v.Input().Reasons&recordermaintenance.CounterDip != 0, nil
	}
	start := a.At.Truncate(time.Hour)
	if coveredThrough.After(a.At) {
		start = coveredThrough.Truncate(time.Hour)
	}
	end := b.At
	// A huge idle span is explicit incomplete evidence, never an unbounded
	// allocation or loop over thirteen months of empty buckets.
	if end.Sub(start) > time.Duration(500-*updates)*time.Hour {
		if err = recordAnalyticalBreak(ctx, tx, row.digest, start, end, recordermaintenance.BudgetLimit); err != nil {
			return false, err
		}
		return false, storePointIssue(ctx, tx, row, recordermaintenance.BudgetLimit, updates)
	}
	for start.Before(end) {
		if *updates >= recordermaintenance.MaxPrefixRows || maintenanceBudgetLow(ctx) {
			if err = recordAnalyticalBreak(ctx, tx, row.digest, start, end, recordermaintenance.BudgetLimit); err != nil {
				return false, err
			}
			return false, storePointIssue(ctx, tx, row, recordermaintenance.BudgetLimit, updates)
		}
		window, err = recordermaintenance.NewWindow(start)
		if err != nil {
			return false, err
		}
		v, err = recordermaintenance.ProjectFrom(window, edge, coveredThrough)
		if err != nil {
			return false, err
		}
		if v.Input().Reasons&recordermaintenance.UnsupportedNumeric != 0 {
			// The refused interval survives raw retention and remains visible
			// even when a later read has no numeric bucket for this edge.
			refusedFrom, refusedThrough := start, start.Add(time.Hour)
			if a.At.After(refusedFrom) {
				refusedFrom = a.At
			}
			if coveredThrough.After(refusedFrom) {
				refusedFrom = coveredThrough
			}
			if end.Before(refusedThrough) {
				refusedThrough = end
			}
			if err = recordAnalyticalBreak(ctx, tx, row.digest, refusedFrom, refusedThrough, recordermaintenance.UnsupportedNumeric); err != nil {
				return false, err
			}
		}
		if v.Input().HasMeasurement || v.Input().Reasons != 0 {
			if err = writeAggregate(ctx, tx, row, a, *a.Numeric, window, v, a, updates); err != nil {
				return false, err
			}
		}
		start = start.Add(time.Hour)
	}
	return false, nil
}

func storePointIssue(ctx context.Context, tx pgx.Tx, row numericRow, reason recordermaintenance.Reason, updates *int) error {
	in := row.point.Input()
	if in.Numeric == nil {
		return nil
	}
	w, err := recordermaintenance.NewWindow(in.At.Truncate(time.Hour))
	if err != nil {
		return err
	}
	reset := recordermaintenance.ResetUnknown
	if in.Numeric.Semantic == domain.SemanticMeasurement {
		reset = recordermaintenance.ResetNotApplicable
	}
	v, e := recordermaintenance.NewValues(recordermaintenance.ValuesInput{Reset: reset, Reasons: reason})
	if e != nil {
		return e
	}
	return writeAggregate(ctx, tx, row, in, *in.Numeric, w, v, in, updates)
}

func writeAggregate(ctx context.Context, tx pgx.Tx, row numericRow, quality recordermaintenance.PointInput, n recordermaintenance.NumericInput, w recordermaintenance.Window, value recordermaintenance.Values, from recordermaintenance.PointInput, updates *int) error {
	if !value.Valid() {
		return recordermaintenance.ErrInvalid
	}
	if *updates >= recordermaintenance.MaxPrefixRows {
		return recordermaintenance.ErrBudget
	}
	*updates++
	n.Value = 0
	descriptor, err := json.Marshal(n)
	if err != nil || len(descriptor) > 2048 {
		return recordermaintenance.ErrBudget
	}
	keyBytes, err := json.Marshal(struct {
		N recordermaintenance.NumericInput
		A domain.Availability
		F domain.Freshness
		P domain.Provenance
	}{n, quality.Availability, quality.Freshness, quality.Provenance})
	if err != nil {
		return err
	}
	group := sha256.Sum256(keyBytes)
	start, end := w.Bounds()
	var oldDescriptor []byte
	var oldEntity string
	var duration int64
	var weighted float64
	var minimum, maximum, contribution *float64
	var reset int16
	var reasons int32
	var oldFromEpoch []byte
	var oldFromSequence string
	err = tx.QueryRow(ctx, `SELECT entity_id,descriptor,duration_ns,weighted,minimum,maximum,contribution,reset_kind,reasons,from_epoch,from_sequence::text FROM public.recorder_hourly_aggregates WHERE entity_digest=$1 AND bucket_start=$2 AND group_digest=$3 FOR UPDATE`, row.digest, start, group[:]).Scan(&oldEntity, &oldDescriptor, &duration, &weighted, &minimum, &maximum, &contribution, &reset, &reasons, &oldFromEpoch, &oldFromSequence)
	fromEpoch, e := ParseRecorderEpoch(from.Epoch)
	if e != nil {
		return e
	}
	fromSequence := from.Sequence
	if err == nil {
		oldN, decodeErr := decodeAggregateDescriptor(oldDescriptor)
		if decodeErr != nil || oldN != n || oldEntity != row.entity {
			return recordermaintenance.ErrIntegrity
		}
		old, e := aggregateValues(duration, weighted, minimum, maximum, contribution, reset, reasons)
		if e != nil {
			return e
		}
		value, err = recordermaintenance.Merge(old, value)
		if err != nil {
			return err
		}
		fromEpoch, e = coverageEpoch(oldFromEpoch)
		if e != nil {
			return e
		}
		fromSequence, e = coverageUint(oldFromSequence)
		if e != nil {
			return e
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	input := value.Input()
	var minValue, maxValue, delta any
	if input.HasMeasurement {
		minValue = input.Minimum
		maxValue = input.Maximum
	}
	if input.HasContribution {
		delta = input.Contribution
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.recorder_hourly_aggregates(entity_digest,entity_id,bucket_start,bucket_end,group_digest,descriptor,availability,freshness,provenance,duration_ns,weighted,minimum,maximum,contribution,reset_kind,reasons,from_epoch,from_sequence,through_epoch,through_sequence) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18::numeric,$19,$20::numeric) ON CONFLICT(entity_digest,bucket_start,group_digest) DO UPDATE SET duration_ns=EXCLUDED.duration_ns,weighted=EXCLUDED.weighted,minimum=EXCLUDED.minimum,maximum=EXCLUDED.maximum,contribution=EXCLUDED.contribution,reset_kind=EXCLUDED.reset_kind,reasons=EXCLUDED.reasons,through_epoch=EXCLUDED.through_epoch,through_sequence=EXCLUDED.through_sequence`, row.digest, row.entity, start, end, group[:], descriptor, int16(quality.Availability), int16(quality.Freshness), int16(quality.Provenance), int64(input.Duration), input.Weighted, minValue, maxValue, delta, int16(input.Reset), int32(input.Reasons), fromEpoch.data(), decimalUint(fromSequence), row.epoch.data(), decimalUint(row.sequence))
	return err
}

func preserveExpiredNumeric(ctx context.Context, tx pgx.Tx, epoch RecorderEpoch, sequence uint64) error {
	var run, analyticRun int64
	var analyticText string
	if err := tx.QueryRow(ctx, `SELECT e.run_id,m.analytic_run,m.analytic_sequence::text FROM public.recorder_epochs e CROSS JOIN public.recorder_maintenance_head m WHERE e.epoch=$1 AND m.singleton=true`, epoch.data()).Scan(&run, &analyticRun, &analyticText); err != nil {
		return err
	}
	analyticSequence, err := coverageUint(analyticText)
	if err != nil {
		return err
	}
	if run < analyticRun || (run == analyticRun && sequence <= analyticSequence) {
		return nil
	}
	row, err := scanNumericRow(tx.QueryRow(ctx, `SELECT `+recorderNumericColumns+` FROM public.state_history h JOIN public.recorder_epochs e ON e.epoch=h.epoch WHERE h.epoch=$1 AND h.sequence=$2::numeric`, epoch.data(), decimalUint(sequence)))
	if err != nil {
		return err
	}
	if row.refused {
		at := row.point.Input().At
		return recordAnalyticalBreak(ctx, tx, row.digest, at, at, recordermaintenance.RetentionPruned|recordermaintenance.UnsupportedNumeric)
	}
	updates := 0
	return storePointIssue(ctx, tx, row, recordermaintenance.RetentionPruned|recordermaintenance.BudgetLimit, &updates)
}

// ReadRecorderHourlyAggregate is an internal storage read, not public API164.
// It returns fresh immutable numeric values and preserves incomplete evidence.
type RecorderHourlyAggregate struct {
	Entity       string
	Start, End   time.Time
	Descriptor   recordermaintenance.NumericInput
	Availability domain.Availability
	Freshness    domain.Freshness
	Provenance   domain.Provenance
	Values       recordermaintenance.Values
}

func (s *Store) ReadRecorderHourlyAggregates(ctx context.Context, entity string, start, end time.Time) ([]RecorderHourlyAggregate, error) {
	if s == nil || s.pool == nil || !validHistoryContext(ctx) || entity == "" || !start.Before(end) || start.Location() != time.UTC || end.Location() != time.UTC || end.Sub(start) > 500*time.Hour {
		return nil, recordermaintenance.ErrInvalid
	}
	bounded, cancel := context.WithTimeout(ctx, recordermaintenance.AttemptTimeout)
	defer cancel()
	rows, err := s.pool.Query(bounded, `SELECT a.entity_id,a.bucket_start,a.bucket_end,a.descriptor,a.availability,a.freshness,a.provenance,a.duration_ns,a.weighted,a.minimum,a.maximum,a.contribution,a.reset_kind,(a.reasons | COALESCE((SELECT bit_or(b.reasons) FROM public.recorder_analytical_breaks b WHERE b.entity_digest=a.entity_digest AND b.start_at<a.bucket_end AND (b.end_at>a.bucket_start OR (b.start_at=b.end_at AND b.start_at>=a.bucket_start))),0)) FROM public.recorder_hourly_aggregates a WHERE entity_digest=$1 AND bucket_start>=$2 AND bucket_start<$3 ORDER BY bucket_start,group_digest LIMIT 501`, hashEntity(entity), start, end)
	if err != nil {
		return nil, recordermaintenance.ErrUnavailable
	}
	defer rows.Close()
	out := make([]RecorderHourlyAggregate, 0)
	for rows.Next() {
		if len(out) == 500 {
			return nil, recordermaintenance.ErrBudget
		}
		var a RecorderHourlyAggregate
		var descriptor []byte
		var av, fr, pr, reset int16
		var duration int64
		var reasons int32
		var min, max, delta *float64
		var weighted float64
		if err = rows.Scan(&a.Entity, &a.Start, &a.End, &descriptor, &av, &fr, &pr, &duration, &weighted, &min, &max, &delta, &reset, &reasons); err != nil {
			return nil, recordermaintenance.ErrUnavailable
		}
		if a.Entity != entity {
			return nil, recordermaintenance.ErrIntegrity
		}
		a.Start = a.Start.UTC()
		a.End = a.End.UTC()
		a.Descriptor, err = decodeAggregateDescriptor(descriptor)
		if err != nil {
			return nil, err
		}
		if av < 1 || av > 3 || fr < 1 || fr > 2 || pr < 1 || pr > 3 {
			return nil, recordermaintenance.ErrIntegrity
		}
		a.Availability = domain.Availability(av)
		a.Freshness = domain.Freshness(fr)
		a.Provenance = domain.Provenance(pr)
		if _, err = domain.NewQuality(a.Availability, a.Freshness, a.Provenance); err != nil {
			return nil, recordermaintenance.ErrIntegrity
		}
		if _, err = recordermaintenance.NewPoint(recordermaintenance.PointInput{Epoch: "00000000000000000000000000000001", Sequence: 1, At: a.Start, Availability: a.Availability, Freshness: a.Freshness, Provenance: a.Provenance, Kind: domain.ValueMeasurement, Numeric: &a.Descriptor}); err != nil {
			return nil, recordermaintenance.ErrIntegrity
		}
		a.Values, err = aggregateValues(duration, weighted, min, max, delta, reset, reasons)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if rows.Err() != nil {
		return nil, recordermaintenance.ErrUnavailable
	}
	return out, nil
}

// SQL reset codes are admitted explicitly; unknown and not-applicable never alias.
func aggregateValues(duration int64, weighted float64, min, max, delta *float64, reset int16, reasons int32) (recordermaintenance.Values, error) {
	in := recordermaintenance.ValuesInput{Duration: time.Duration(duration), Weighted: weighted}
	switch reset {
	case 1:
		in.Reset = recordermaintenance.ResetNotApplicable
	case 2:
		in.Reset = recordermaintenance.ResetUnknown
	case 3:
		in.Reset = recordermaintenance.ResetNotDetected
	case 4:
		in.Reset = recordermaintenance.ResetInferred
	default:
		return recordermaintenance.Values{}, recordermaintenance.ErrIntegrity
	}
	if reasons < 0 || reasons > 4095 {
		return recordermaintenance.Values{}, recordermaintenance.ErrIntegrity
	}
	in.Reasons = recordermaintenance.Reason(reasons)
	if min != nil && max != nil {
		in.HasMeasurement = true
		in.Minimum = *min
		in.Maximum = *max
	} else if min != nil || max != nil {
		return recordermaintenance.Values{}, recordermaintenance.ErrIntegrity
	}
	if delta != nil {
		in.HasContribution = true
		in.Contribution = *delta
	}
	v, err := recordermaintenance.NewValues(in)
	if err != nil {
		return recordermaintenance.Values{}, recordermaintenance.ErrIntegrity
	}
	return v, nil
}

func decodeAggregateDescriptor(data []byte) (recordermaintenance.NumericInput, error) {
	var n recordermaintenance.NumericInput
	if len(data) == 0 || len(data) > 2048 {
		return n, recordermaintenance.ErrIntegrity
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&n); err != nil {
		return n, recordermaintenance.ErrIntegrity
	}
	return n, nil
}
