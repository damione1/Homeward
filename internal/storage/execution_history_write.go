package storage

import (
	"bytes"
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

type HistoryEpochOpening struct {
	Epoch                  HistoryEpoch
	PreviousEpoch          HistoryEpoch
	PreviousCheckpoint     uint64
	InterruptedPredecessor bool
}

// OpenExecutionHistoryEpoch records a crash/stop boundary without guessing a
// lost count or replaying any run. The caller supplies the process sequencer's
// epoch; hot publication continues using the already opened epoch.
func (s *Store) OpenExecutionHistoryEpoch(ctx context.Context, epoch HistoryEpoch) (HistoryEpochOpening, error) {
	if s == nil || s.pool == nil || !validHistoryContext(ctx) || !epoch.Valid() {
		return HistoryEpochOpening{}, ErrHistoryInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var result HistoryEpochOpening
	result.Epoch = epoch
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		var current *string
		if err := tx.QueryRow(ctx, `SELECT current_epoch FROM public.execution_history_head WHERE singleton=true FOR UPDATE`).Scan(&current); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.execution_history_epochs WHERE epoch=$1)`, epoch.String()).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if current == nil || *current != epoch.String() {
				return ErrHistoryIntegrity
			}
			var previous *string
			var checkpoint int64
			if e := tx.QueryRow(ctx, `SELECT previous_epoch,previous_checkpoint,interrupted_predecessor FROM public.execution_history_epochs WHERE epoch=$1`, epoch.String()).Scan(&previous, &checkpoint, &result.InterruptedPredecessor); e != nil {
				return e
			}
			if previous != nil {
				var e error
				result.PreviousEpoch, e = ParseHistoryEpoch(*previous)
				if e != nil {
					return ErrHistoryIntegrity
				}
			}
			result.PreviousCheckpoint = uint64(checkpoint)
			return nil
		}
		if current != nil {
			previous, e := ParseHistoryEpoch(*current)
			if e != nil {
				return ErrHistoryIntegrity
			}
			result.PreviousEpoch = previous
			result.InterruptedPredecessor = true
			var checkpoint int64
			if e := tx.QueryRow(ctx, `SELECT checkpoint FROM public.execution_history_epochs WHERE epoch=$1`, *current).Scan(&checkpoint); e != nil {
				return e
			}
			result.PreviousCheckpoint = uint64(checkpoint)
		}
		_, e := tx.Exec(ctx, `INSERT INTO public.execution_history_epochs(epoch,previous_epoch,previous_checkpoint,interrupted_predecessor) VALUES($1,$2,$3,$4)`, epoch.String(), current, result.PreviousCheckpoint, result.InterruptedPredecessor)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE public.execution_history_head SET current_epoch=$1 WHERE singleton=true`, epoch.String())
		return e
	})
	if err != nil {
		if outcome == TransactionIndeterminate {
			return HistoryEpochOpening{}, ErrHistoryCommitUnknown
		}
		return HistoryEpochOpening{}, historyFailure(ErrHistoryUnavailable, err)
	}
	return result, nil
}

type HistoryCommitResult uint8

const (
	HistoryCommitInvalid HistoryCommitResult = iota
	HistoryCommitted
	HistoryAlreadyCommitted
)

// CommitExecutionHistoryBatch has one serialization point with purge. The
// head row is locked before any batch mutation; a later resolver locks that
// same row, so absence after a lost COMMIT ACK proves the earlier transaction
// has ended. A conflict aborts the entire batch, including checkpoint/loss.
func (s *Store) CommitExecutionHistoryBatch(ctx context.Context, batch HistoryBatch) (HistoryCommitResult, error) {
	if s == nil || s.pool == nil || !validHistoryContext(ctx) || !batch.Valid() {
		return HistoryCommitInvalid, ErrHistoryInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result := HistoryCommitted
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		var cutoff time.Time
		var commitSequence int64
		if e := tx.QueryRow(ctx, `SELECT retention_cutoff,commit_sequence FROM public.execution_history_head WHERE singleton=true FOR UPDATE`).Scan(&cutoff, &commitSequence); e != nil {
			return e
		}
		var existing []byte
		e := tx.QueryRow(ctx, `SELECT digest FROM public.execution_history_batches WHERE epoch=$1 AND batch_number=$2`, batch.epoch.String(), int64(batch.number)).Scan(&existing)
		if e == nil {
			if !bytes.Equal(existing, batch.digest[:]) {
				return ErrHistoryIntegrity
			}
			result = HistoryAlreadyCommitted
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		var currentEpoch *string
		if e := tx.QueryRow(ctx, `SELECT current_epoch FROM public.execution_history_head WHERE singleton=true`).Scan(&currentEpoch); e != nil {
			return e
		}
		if currentEpoch == nil || *currentEpoch != batch.epoch.String() {
			return ErrHistoryIntegrity
		}
		if commitSequence == math.MaxInt64 {
			return ErrHistoryIntegrity
		}
		commitSequence++
		if _, e := tx.Exec(ctx, `INSERT INTO public.execution_history_batches(epoch,batch_number,digest,retention_time) VALUES($1,$2,$3,$4)`, batch.epoch.String(), int64(batch.number), batch.digest[:], batchRetentionTime(batch)); e != nil {
			return e
		}
		for _, record := range batch.records {
			if record.input.EmissionTime.Before(cutoff) {
				continue
			}
			if e := insertHistoryRecord(ctx, tx, record, commitSequence); e != nil {
				return e
			}
		}
		for _, loss := range batch.losses {
			// A proof crossing the cutoff keeps its whole count. It cannot be
			// divided by elapsed time or claimed as fully retained history.
			if loss.Through.Before(cutoff) {
				continue
			}
			if _, e := tx.Exec(ctx, `INSERT INTO public.execution_history_loss(epoch,batch_number,reason,known_count,uncertain_envelope,start_time,emission_time,commit_sequence) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, batch.epoch.String(), int64(batch.number), int16(loss.Reason), int64(loss.KnownCount), loss.UncertainEnvelope || loss.From.Before(cutoff), loss.From, loss.Through, commitSequence); e != nil {
				return e
			}
		}
		if _, e := tx.Exec(ctx, `UPDATE public.execution_history_epochs SET checkpoint=GREATEST(checkpoint,$2) WHERE epoch=$1`, batch.epoch.String(), lastHistoryOrdinal(batch.records)); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE public.execution_history_head SET commit_sequence=$1 WHERE singleton=true`, commitSequence)
		return e
	})
	if err != nil {
		if outcome == TransactionIndeterminate {
			return HistoryCommitInvalid, ErrHistoryCommitUnknown
		}
		if errors.Is(err, ErrHistoryIntegrity) {
			return HistoryCommitInvalid, ErrHistoryIntegrity
		}
		return HistoryCommitInvalid, historyFailure(ErrHistoryUnavailable, err)
	}
	return result, nil
}

func lastHistoryOrdinal(records []HistoryRecord) int64 {
	if len(records) == 0 {
		return 0
	}
	return int64(records[len(records)-1].ID().Ordinal())
}

func batchRetentionTime(batch HistoryBatch) time.Time {
	latest := time.Time{}
	for _, record := range batch.records {
		if record.input.EmissionTime.After(latest) {
			latest = record.input.EmissionTime
		}
	}
	for _, loss := range batch.losses {
		if loss.Through.After(latest) {
			latest = loss.Through
		}
	}
	return latest
}

func historyOptionalInt(v uint8) any {
	if v == 0 {
		return nil
	}
	return int16(v)
}
func historyOptionalBigInt(v uint64) any {
	if v == 0 {
		return nil
	}
	return int64(v)
}
func historyOptionalRevision(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
func historyOptionalHash(v [32]byte) any {
	if v == [32]byte{} {
		return nil
	}
	return v[:]
}

func insertHistoryRecord(ctx context.Context, tx pgx.Tx, r HistoryRecord, sequence int64) error {
	in := r.input
	_, err := tx.Exec(ctx, `INSERT INTO public.execution_history_records(
		epoch,ordinal,emission_time,commit_sequence,kind,decision,node_stage,command_outcome,scene_outcome,scene_aggregate,script_mode,
		native_ordinal,node_visit,command_ordinal,action_kind,failure_kind,wait_exit,
		controller_generation,run_id,parent_run_id,provenance_kind,config_revision,manifest_sha256,
		object_id,source_file,source_path,source_line,source_column,trigger_key,command_id,scene_invocation,scene_member,
		branches,iterations,record_digest)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35)
		ON CONFLICT(epoch,ordinal) DO NOTHING`, in.ID.Epoch().String(), int64(in.ID.Ordinal()), in.EmissionTime, sequence, int16(in.Kind),
		historyOptionalInt(in.Decision), historyOptionalInt(in.NodeStage), historyOptionalInt(in.CommandOutcome), historyOptionalInt(in.SceneOutcome), historyOptionalInt(in.SceneAggregate), historyOptionalInt(in.ScriptMode),
		historyOptionalBigInt(in.NativeOrdinal), historyOptionalBigInt(in.NodeVisit), historyOptionalBigInt(in.CommandOrdinal), historyOptionalInt(in.ActionKind), historyOptionalInt(in.FailureKind), historyOptionalInt(in.WaitExit),
		historyOptionalBigInt(in.ControllerGeneration), historyOptionalBigInt(in.RunID), historyOptionalBigInt(in.ParentRunID), int16(in.Provenance), historyOptionalRevision(in.ConfigRevision), historyOptionalHash(in.ManifestSHA256),
		in.ObjectID, in.SourceFile, in.SourcePath, historyOptionalPositive(in.SourceLine), historyOptionalPositive(in.SourceColumn), in.TriggerKey, in.CommandID, in.SceneInvocation, historyOptionalMember(in),
		in.Branches, in.Iterations, r.digest[:])
	if err != nil {
		return err
	}
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT record_digest FROM public.execution_history_records WHERE epoch=$1 AND ordinal=$2`, in.ID.Epoch().String(), int64(in.ID.Ordinal())).Scan(&stored); err != nil {
		return err
	}
	if !bytes.Equal(stored, r.digest[:]) {
		return ErrHistoryIntegrity
	}
	return nil
}

func historyOptionalPositive(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
func historyOptionalMember(in HistoryRecordInput) any {
	if in.Kind != HistorySceneMember {
		return nil
	}
	return in.SceneMember
}

type HistoryBatchResolution uint8

const (
	HistoryResolutionInvalid HistoryBatchResolution = iota
	HistoryResolutionAbsent
	HistoryResolutionCommitted
)

func (s *Store) ResolveExecutionHistoryBatch(ctx context.Context, batch HistoryBatch) (HistoryBatchResolution, error) {
	if s == nil || s.pool == nil || !validHistoryContext(ctx) || !batch.Valid() {
		return HistoryResolutionInvalid, ErrHistoryInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return HistoryResolutionInvalid, historyFailure(ErrHistoryUnavailable, err)
	}
	defer rollback(tx)
	var cutoff time.Time
	if err := tx.QueryRow(ctx, `SELECT retention_cutoff FROM public.execution_history_head WHERE singleton=true FOR UPDATE`).Scan(&cutoff); err != nil {
		return HistoryResolutionInvalid, historyFailure(ErrHistoryUnavailable, err)
	}
	var digest []byte
	err = tx.QueryRow(ctx, `SELECT digest FROM public.execution_history_batches WHERE epoch=$1 AND batch_number=$2`, batch.epoch.String(), int64(batch.number)).Scan(&digest)
	if err == nil {
		if !bytes.Equal(digest, batch.digest[:]) {
			return HistoryResolutionInvalid, ErrHistoryIntegrity
		}
		return HistoryResolutionCommitted, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return HistoryResolutionAbsent, nil
	}
	return HistoryResolutionInvalid, historyFailure(ErrHistoryUnavailable, err)
}
