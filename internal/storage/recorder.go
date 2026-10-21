package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strconv"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5"
)

type RecorderGapReason uint8

const (
	RecorderGapUnspecified RecorderGapReason = iota
	RecorderGapOverflow
	RecorderGapReconciliation
	RecorderGapAbandoned
)

type RecorderGap struct {
	From, Through     uint64
	KnownLost         *uint64
	Reason            RecorderGapReason
	UncertainEnvelope bool
}

func (g RecorderGap) valid() bool {
	if g.From == 0 || g.Through < g.From || g.Reason < RecorderGapOverflow || g.Reason > RecorderGapAbandoned {
		return false
	}
	return g.KnownLost == nil || *g.KnownLost > 0
}

type RecorderImage struct {
	Boundary uint64
	States   []RecorderSnapshotRecord
}

type RecorderBatch struct {
	epoch          RecorderEpoch
	from, through  uint64 // from is exclusive; through is inclusive.
	states         []RecorderRecord
	gaps           []RecorderGap
	policyExcluded uint64
	image          *RecorderImage
	digest         [32]byte
}

// NewRecorderBatch freezes the single in-flight value. Gaps describe coverage,
// never a fabricated number of state transitions. Image states have no entry
// sequence and are stored separately from state_history.
func NewRecorderBatch(epoch RecorderEpoch, from, through uint64, states []RecorderRecord, gaps []RecorderGap, excluded uint64, image *RecorderImage) (RecorderBatch, error) {
	if !epoch.valid() || through < from || (through == from && image == nil) || len(states) > 500 || len(gaps) > 32 || (image != nil && (image.Boundary != through || len(image.States) > 4096)) {
		return RecorderBatch{}, ErrRecorderInvalid
	}
	// Account for the entire frozen batch before allocating owned copies. The
	// mutable caller's backing stores can coexist until this call returns.
	remaining := 4<<20 - int(unsafe.Sizeof(RecorderBatch{})) - 1024 - len(gaps)*(int(unsafe.Sizeof(RecorderGap{}))+64)
	if remaining < 0 {
		return RecorderBatch{}, ErrRecorderTooLarge
	}
	for _, row := range states {
		charge := row.RetainedBytes()
		if charge > remaining {
			return RecorderBatch{}, ErrRecorderTooLarge
		}
		remaining -= charge
	}
	if image != nil {
		remaining -= int(unsafe.Sizeof(RecorderImage{})) + 256
		if remaining < 0 {
			return RecorderBatch{}, ErrRecorderTooLarge
		}
		for _, row := range image.States {
			charge := row.RetainedBytes()
			if charge > remaining {
				return RecorderBatch{}, ErrRecorderTooLarge
			}
			remaining -= charge
		}
	}
	copyStates := make([]RecorderRecord, len(states))
	for i, row := range states {
		copyStates[i] = cloneRecorderRecord(row)
	}
	var last uint64
	for i, state := range copyStates {
		if state.sequence <= from || state.sequence > through || (i > 0 && state.sequence <= last) || state.admittedAt.IsZero() || !state.observed.validDigest() || digestRecorderRecord(state) != state.digest {
			return RecorderBatch{}, ErrRecorderInvalid
		}
		last = state.sequence
	}
	copyGaps := make([]RecorderGap, len(gaps))
	for i, gap := range gaps {
		copyGaps[i] = gap
		if gap.KnownLost != nil {
			count := *gap.KnownLost
			copyGaps[i].KnownLost = &count
		}
	}
	covered := uint64(len(copyStates))
	if covered > through-from {
		return RecorderBatch{}, ErrRecorderInvalid
	}
	if excluded > through-from-covered {
		return RecorderBatch{}, ErrRecorderInvalid
	}
	covered += excluded
	unknownEnvelope := false
	var previousExactThrough uint64
	for _, gap := range copyGaps {
		if !gap.valid() || gap.From <= from || gap.Through > through || (gap.KnownLost != nil && *gap.KnownLost > gap.Through-gap.From+1) {
			return RecorderBatch{}, ErrRecorderInvalid
		}
		if gap.KnownLost != nil && *gap.KnownLost > through-from-covered {
			return RecorderBatch{}, ErrRecorderInvalid
		}
		if gap.UncertainEnvelope {
			// One explicit uncertain envelope covers the entire treated prefix.
			// Rows within it remain evidence, never proof that other positions
			// in the envelope were observed. KnownLost is only a lower bound.
			if len(copyGaps) != 1 || gap.From != from+1 || gap.Through != through {
				return RecorderBatch{}, ErrRecorderInvalid
			}
			unknownEnvelope = true
		} else {
			if gap.KnownLost == nil || *gap.KnownLost != gap.Through-gap.From+1 || (previousExactThrough != 0 && gap.From <= previousExactThrough) {
				return RecorderBatch{}, ErrRecorderInvalid
			}
			for _, row := range copyStates {
				if row.sequence >= gap.From && row.sequence <= gap.Through {
					return RecorderBatch{}, ErrRecorderInvalid
				}
			}
			previousExactThrough = gap.Through
			if *gap.KnownLost > gap.Through-gap.From+1 || *gap.KnownLost > through-from-covered {
				return RecorderBatch{}, ErrRecorderInvalid
			}
			covered += *gap.KnownLost
		}
	}
	if !unknownEnvelope && covered != through-from {
		return RecorderBatch{}, ErrRecorderInvalid
	}
	var copyImage *RecorderImage
	if image != nil {
		if image.Boundary != through || len(image.States) > 4096 {
			return RecorderBatch{}, ErrRecorderInvalid
		}
		copyImage = &RecorderImage{Boundary: image.Boundary, States: make([]RecorderSnapshotRecord, len(image.States))}
		for i, row := range image.States {
			copyImage.States[i] = cloneRecorderSnapshot(row)
		}
		lastEntity := ""
		for _, state := range copyImage.States {
			if state.observed.entity <= lastEntity || !state.observed.validDigest() || digestRecorderSnapshot(state) != state.digest {
				return RecorderBatch{}, ErrRecorderInvalid
			}
			lastEntity = state.observed.entity
		}
	}
	b := RecorderBatch{epoch: epoch, from: from, through: through, states: copyStates, gaps: copyGaps, policyExcluded: excluded, image: copyImage}
	b.digest = digestRecorderBatch(b)
	return b, nil
}

func (b RecorderBatch) Digest() [32]byte     { return b.digest }
func (b RecorderBatch) Epoch() RecorderEpoch { return b.epoch }
func (b RecorderBatch) From() uint64         { return b.from }
func (b RecorderBatch) Through() uint64      { return b.through }
func (b RecorderBatch) StateCount() int      { return len(b.states) }
func (b RecorderBatch) HasImage() bool       { return b.image != nil }
func (b RecorderBatch) HasGaps() bool        { return len(b.gaps) != 0 }

// EncodedStateBytes is the existing payload-size threshold observed after
// admission, distinct from the conservative retained-memory charge.
func (b RecorderBatch) EncodedStateBytes() int {
	total := 0
	for _, state := range b.states {
		total += state.EncodedBytes()
	}
	return total
}

func cloneRecorderObserved(o recorderObserved) recorderObserved {
	o.payload = bytes.Clone(o.payload)
	if o.observedAt != nil {
		value := *o.observedAt
		o.observedAt = &value
	}
	if o.sourceSequence != nil {
		value := *o.sourceSequence
		o.sourceSequence = &value
	}
	if o.valueKind != nil {
		value := *o.valueKind
		o.valueKind = &value
	}
	if o.measurementValue != nil {
		value := *o.measurementValue
		o.measurementValue = &value
	}
	if o.measurementUnit != nil {
		value := *o.measurementUnit
		o.measurementUnit = &value
	}
	if o.measurementCanonical != nil {
		value := *o.measurementCanonical
		o.measurementCanonical = &value
	}
	if o.measurementScale != nil {
		value := *o.measurementScale
		o.measurementScale = &value
	}
	if o.measurementOffset != nil {
		value := *o.measurementOffset
		o.measurementOffset = &value
	}
	if o.measurementDimension != nil {
		value := *o.measurementDimension
		o.measurementDimension = &value
	}
	if o.measurementPrecision != nil {
		value := *o.measurementPrecision
		o.measurementPrecision = &value
	}
	if o.measurementSemantic != nil {
		value := *o.measurementSemantic
		o.measurementSemantic = &value
	}
	if o.measurementAggregation != nil {
		value := *o.measurementAggregation
		o.measurementAggregation = &value
	}
	return o
}

func cloneRecorderRecord(row RecorderRecord) RecorderRecord {
	row.observed = cloneRecorderObserved(row.observed)
	if row.causation != nil {
		value := *row.causation
		row.causation = &value
	}
	if row.correlation != nil {
		value := *row.correlation
		row.correlation = &value
	}
	return row
}

func cloneRecorderSnapshot(row RecorderSnapshotRecord) RecorderSnapshotRecord {
	row.observed = cloneRecorderObserved(row.observed)
	return row
}

func digestRecorderBatch(b RecorderBatch) [32]byte {
	h := sha256.New()
	writeRecorderBytes(h, b.epoch.data())
	writeRecorderString(h, decimalUint(b.from))
	writeRecorderString(h, decimalUint(b.through))
	writeRecorderString(h, decimalUint(b.policyExcluded))
	writeRecorderString(h, decimalUint(uint64(len(b.states))))
	for _, row := range b.states {
		writeRecorderBytes(h, row.digest[:])
	}
	writeRecorderString(h, decimalUint(uint64(len(b.gaps))))
	for _, gap := range b.gaps {
		writeRecorderString(h, decimalUint(gap.From))
		writeRecorderString(h, decimalUint(gap.Through))
		writeRecorderString(h, strconv.Itoa(int(gap.Reason)))
		if gap.KnownLost == nil {
			writeRecorderString(h, "absent")
		} else {
			writeRecorderString(h, decimalUint(*gap.KnownLost))
		}
		if gap.UncertainEnvelope {
			writeRecorderString(h, "uncertain")
		} else {
			writeRecorderString(h, "exact")
		}
	}
	if b.image == nil {
		writeRecorderString(h, "no-image")
	} else {
		writeRecorderString(h, "image")
		writeRecorderString(h, decimalUint(uint64(len(b.image.States))))
		for _, row := range b.image.States {
			writeRecorderBytes(h, row.digest[:])
		}
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

// RecorderPrevious describes the physical observation interval between runs.
// Its event count is deliberately absent, even after a clean prior close.
type RecorderPrevious struct {
	Epoch      RecorderEpoch
	Checkpoint uint64
	CleanClose bool
	Present    bool
}

type RecorderCheckpoint struct {
	Sequence               uint64
	LastFrom               uint64
	LastThrough            uint64
	LastDigest             [32]byte
	HasLastBatch           bool
	ReconciliationComplete bool
	CleanClose             bool
}

func (c RecorderCheckpoint) Confirms(batch RecorderBatch) bool {
	return c.HasLastBatch && c.Sequence == batch.through && c.LastFrom == batch.from && c.LastThrough == batch.through && c.LastDigest == batch.digest
}

func (s *Store) OpenRecorderEpoch(ctx context.Context, epoch RecorderEpoch, started time.Time) (RecorderPrevious, TransactionOutcome, error) {
	if !epoch.valid() || started.IsZero() {
		return RecorderPrevious{}, TransactionNotCommitted, ErrRecorderInvalid
	}
	var previous RecorderPrevious
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		// Serialize starts while preserving the epoch's opaque identity.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316, 30)`); err != nil {
			return err
		}
		var oldEpoch []byte
		var oldCheckpoint string
		var oldClean bool
		err := tx.QueryRow(ctx, `SELECT epoch, checkpoint::text, clean_close FROM public.recorder_epochs ORDER BY run_id DESC LIMIT 1`).Scan(&oldEpoch, &oldCheckpoint, &oldClean)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var oldEpochArg, oldCheckpointArg, oldCleanArg any
		if err == nil {
			if len(oldEpoch) != 16 {
				return ErrRecorderIntegrity
			}
			copy(previous.Epoch.bytes[:], oldEpoch)
			value, parseErr := strconv.ParseUint(oldCheckpoint, 10, 64)
			if parseErr != nil {
				return ErrRecorderIntegrity
			}
			previous.Checkpoint, previous.CleanClose, previous.Present = value, oldClean, true
			oldEpochArg, oldCheckpointArg, oldCleanArg = oldEpoch, oldCheckpoint, oldClean
		}
		_, err = tx.Exec(ctx, `INSERT INTO public.recorder_epochs(epoch, started_at, previous_epoch, previous_checkpoint, previous_clean_close) VALUES ($1,$2,$3,$4::numeric,$5)`, epoch.data(), started.UTC(), oldEpochArg, oldCheckpointArg, oldCleanArg)
		return err
	})
	if outcome != TransactionCommitted || err != nil {
		return RecorderPrevious{}, outcome, err
	}
	return previous, outcome, nil
}

func (s *Store) RecorderCheckpoint(ctx context.Context, epoch RecorderEpoch) (RecorderCheckpoint, error) {
	if s == nil || s.pool == nil || ctx == nil || !epoch.valid() {
		return RecorderCheckpoint{}, ErrRecorderInvalid
	}
	var checkpointText string
	var fromText, throughText sql.NullString
	var digest []byte
	var complete, clean bool
	err := s.pool.QueryRow(ctx, `SELECT checkpoint::text,last_batch_from::text,last_batch_to::text,last_batch_digest,reconciliation_complete,clean_close FROM public.recorder_epochs WHERE epoch=$1`, epoch.data()).Scan(&checkpointText, &fromText, &throughText, &digest, &complete, &clean)
	if err != nil {
		return RecorderCheckpoint{}, err
	}
	checkpoint, err := strconv.ParseUint(checkpointText, 10, 64)
	if err != nil {
		return RecorderCheckpoint{}, ErrRecorderIntegrity
	}
	result := RecorderCheckpoint{Sequence: checkpoint, ReconciliationComplete: complete, CleanClose: clean}
	if fromText.Valid || throughText.Valid || digest != nil {
		if !fromText.Valid || !throughText.Valid || len(digest) != 32 {
			return RecorderCheckpoint{}, ErrRecorderIntegrity
		}
		result.LastFrom, err = strconv.ParseUint(fromText.String, 10, 64)
		if err != nil {
			return RecorderCheckpoint{}, ErrRecorderIntegrity
		}
		result.LastThrough, err = strconv.ParseUint(throughText.String, 10, 64)
		if err != nil {
			return RecorderCheckpoint{}, ErrRecorderIntegrity
		}
		copy(result.LastDigest[:], digest)
		result.HasLastBatch = true
	}
	return result, nil
}

// PersistRecorderBatch commits rows, gaps, an optional image, and the covered
// checkpoint together. An indeterminate outcome must be read back by the
// caller before retry or acknowledgement.
func (s *Store) PersistRecorderBatch(ctx context.Context, batch RecorderBatch) (TransactionOutcome, error) {
	verified, validationErr := NewRecorderBatch(batch.epoch, batch.from, batch.through, batch.states, batch.gaps, batch.policyExcluded, batch.image)
	if validationErr != nil || verified.digest != batch.digest {
		return TransactionNotCommitted, ErrRecorderInvalid
	}
	for _, row := range batch.states {
		if !row.observed.validDigest() || digestRecorderRecord(row) != row.digest {
			return TransactionNotCommitted, ErrRecorderInvalid
		}
	}
	if batch.image != nil {
		for _, row := range batch.image.States {
			if !row.observed.validDigest() || digestRecorderSnapshot(row) != row.digest {
				return TransactionNotCommitted, ErrRecorderInvalid
			}
		}
	}
	return s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		// Retention takes this same lock before selecting/deleting raw rows.
		// The lock order is always maintenance then epoch, including late batches.
		if _, err := tx.Exec(ctx, recorderMaintenanceLock); err != nil {
			return err
		}
		var retentionCutoff time.Time
		if err := tx.QueryRow(ctx, `SELECT raw_cutoff FROM public.recorder_maintenance_head WHERE singleton=true`).Scan(&retentionCutoff); err != nil {
			return err
		}
		var currentText string
		var oldFrom, oldThrough sql.NullString
		var oldDigest []byte
		err := tx.QueryRow(ctx, `SELECT checkpoint::text,last_batch_from::text,last_batch_to::text,last_batch_digest FROM public.recorder_epochs WHERE epoch=$1 FOR UPDATE`, batch.epoch.data()).Scan(&currentText, &oldFrom, &oldThrough, &oldDigest)
		if err != nil {
			return err
		}
		current, err := strconv.ParseUint(currentText, 10, 64)
		if err != nil {
			return ErrRecorderIntegrity
		}
		if current == batch.through && oldFrom.Valid && oldThrough.Valid && oldFrom.String == decimalUint(batch.from) && oldThrough.String == decimalUint(batch.through) {
			if bytes.Equal(oldDigest, batch.digest[:]) {
				return nil
			}
			return ErrRecorderIntegrity
		}
		if current != batch.from {
			return ErrRecorderIntegrity
		}
		for _, row := range batch.states {
			if row.observed.receivedAt.Before(retentionCutoff) {
				if err := recordRetentionRange(ctx, tx, batch.epoch, row.sequence, row.sequence, false, retentionCutoff); err != nil {
					return err
				}
				continue
			}
			if err := insertRecorderState(ctx, tx, batch.epoch, row); err != nil {
				return err
			}
		}
		for _, gap := range batch.gaps {
			if err := insertRecorderGap(ctx, tx, batch.epoch, gap); err != nil {
				return err
			}
		}
		if batch.image != nil {
			for _, row := range batch.image.States {
				if row.observed.receivedAt.Before(retentionCutoff) {
					if err := recordRetentionRange(ctx, tx, batch.epoch, batch.image.Boundary, batch.image.Boundary, true, retentionCutoff); err != nil {
						return err
					}
					continue
				}
				if err := insertRecorderSnapshot(ctx, tx, batch.epoch, batch.image.Boundary, row); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE public.recorder_epochs SET checkpoint=$2::numeric,last_batch_from=$3::numeric,last_batch_to=$2::numeric,last_batch_digest=$4,policy_excluded=policy_excluded+$5::numeric,reconciliation_complete=CASE WHEN $6 THEN true WHEN $7 THEN false ELSE reconciliation_complete END WHERE epoch=$1`, batch.epoch.data(), decimalUint(batch.through), decimalUint(batch.from), batch.digest[:], decimalUint(batch.policyExcluded), batch.image != nil, len(batch.gaps) > 0)
		return err
	})
}

func (s *Store) CloseRecorderEpoch(ctx context.Context, epoch RecorderEpoch, checkpoint uint64) (TransactionOutcome, error) {
	if !epoch.valid() {
		return TransactionNotCommitted, ErrRecorderInvalid
	}
	return s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public.recorder_epochs SET closed_at=now(),clean_close=true WHERE epoch=$1 AND checkpoint=$2::numeric AND clean_close=false`, epoch.data(), decimalUint(checkpoint))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrRecorderIntegrity
		}
		return nil
	})
}

func insertRecorderState(ctx context.Context, tx pgx.Tx, epoch RecorderEpoch, row RecorderRecord) error {
	o := row.observed
	tag, err := tx.Exec(ctx, `INSERT INTO public.state_history(epoch,sequence,entity_id,instance_id,admitted_at,admitted_unix_seconds,admitted_nanos,revision,observed_at,received_at,last_changed,last_updated,last_reported,source_session,source_sequence,causation_id,correlation_id,availability,freshness,provenance,value_kind,measurement_value,measurement_unit,measurement_canonical_unit,measurement_scale,measurement_offset,measurement_dimension,measurement_precision,measurement_semantic,measurement_aggregation,payload,payload_digest,record_digest,entity_digest)
	VALUES ($1,$2::numeric,$3,$4,$5,$6,$7,$8::numeric,$9,$10,$11,$12,$13,$14,$15::numeric,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34) ON CONFLICT (epoch,sequence) DO NOTHING`,
		epoch.data(), decimalUint(row.sequence), o.entity, row.instance, row.admittedAt, row.admittedAt.Unix(), row.admittedAt.Nanosecond(), decimalUint(o.revision), o.observedAt, o.receivedAt, o.lastChanged, o.lastUpdated, o.lastReported,
		o.sourceSession, optionalDecimal(o.sourceSequence), row.causation, row.correlation, o.availability, o.freshness, o.provenance, o.valueKind,
		o.measurementValue, o.measurementUnit, o.measurementCanonical, o.measurementScale, o.measurementOffset, o.measurementDimension, o.measurementPrecision, o.measurementSemantic, o.measurementAggregation,
		o.payload, o.payloadDigest[:], row.digest[:], o.entityDigest[:])
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT record_digest FROM public.state_history WHERE epoch=$1 AND sequence=$2::numeric`, epoch.data(), decimalUint(row.sequence)).Scan(&stored); err != nil {
		return err
	}
	if !bytes.Equal(stored, row.digest[:]) {
		return ErrRecorderIntegrity
	}
	return nil
}

func insertRecorderGap(ctx context.Context, tx pgx.Tx, epoch RecorderEpoch, gap RecorderGap) error {
	tag, err := tx.Exec(ctx, `INSERT INTO public.history_gaps(epoch,from_sequence,through_sequence,reason,known_lost_count,uncertain_envelope) VALUES ($1,$2::numeric,$3::numeric,$4,$5::numeric,$6) ON CONFLICT (epoch,from_sequence,through_sequence,reason) DO NOTHING`, epoch.data(), decimalUint(gap.From), decimalUint(gap.Through), int16(gap.Reason), optionalDecimal(gap.KnownLost), gap.UncertainEnvelope)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var known sql.NullString
	var uncertain bool
	if err := tx.QueryRow(ctx, `SELECT known_lost_count::text,uncertain_envelope FROM public.history_gaps WHERE epoch=$1 AND from_sequence=$2::numeric AND through_sequence=$3::numeric AND reason=$4`, epoch.data(), decimalUint(gap.From), decimalUint(gap.Through), int16(gap.Reason)).Scan(&known, &uncertain); err != nil {
		return err
	}
	if uncertain != gap.UncertainEnvelope || known.Valid != (gap.KnownLost != nil) || (known.Valid && known.String != decimalUint(*gap.KnownLost)) {
		return ErrRecorderIntegrity
	}
	return nil
}

func insertRecorderSnapshot(ctx context.Context, tx pgx.Tx, epoch RecorderEpoch, boundary uint64, row RecorderSnapshotRecord) error {
	o := row.observed
	tag, err := tx.Exec(ctx, `INSERT INTO public.recorder_snapshots(epoch,boundary_sequence,entity_id,revision,observed_at,received_at,source_session,source_sequence,payload,payload_digest,record_digest,entity_digest)
	VALUES ($1,$2::numeric,$3,$4::numeric,$5,$6,$7,$8::numeric,$9,$10,$11,$12) ON CONFLICT (epoch,boundary_sequence,entity_digest) DO NOTHING`, epoch.data(), decimalUint(boundary), o.entity, decimalUint(o.revision), o.observedAt, o.receivedAt, o.sourceSession, optionalDecimal(o.sourceSequence), o.payload, o.payloadDigest[:], row.digest[:], o.entityDigest[:])
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var stored []byte
	var storedEntity string
	if err := tx.QueryRow(ctx, `SELECT entity_id,record_digest FROM public.recorder_snapshots WHERE epoch=$1 AND boundary_sequence=$2::numeric AND entity_digest=$3`, epoch.data(), decimalUint(boundary), o.entityDigest[:]).Scan(&storedEntity, &stored); err != nil {
		return err
	}
	if storedEntity != o.entity || !bytes.Equal(stored, row.digest[:]) {
		return ErrRecorderIntegrity
	}
	return nil
}
