package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecordIntegrationNoChange appends the original redacted response to the same
// native reservation. It never creates an activation attempt or changes the
// active generation. The caller is the native Owner under its execution guard
// and the document owner's exact workspace gate. encode is bounded, pure and
// memory-only; SQL fixes its capture time and verifies the exact source CAS
// before invoking it. A captured response cannot be rebuilt from later state.
func (s *Store) RecordIntegrationNoChange(ctx context.Context, execution ConfigurationAPIExecution, generation ConfigGenerationRevision, manifest [32]byte, ref ConfigVersionRef, source [32]byte, encode func(ConfigurationAPIRequestRecord, time.Time) ([]byte, error)) (ConfigurationAPIRequestRecord, TransactionOutcome, error) {
	if ctx == nil || s == nil || s.pool == nil || !execution.Valid() || execution.request.method != ConfigurationUpdateIntegration || !execution.request.target.Valid() || !generation.Valid() || manifest == ([32]byte{}) || !ref.Valid() || source == ([32]byte{}) || encode == nil {
		return ConfigurationAPIRequestRecord{}, TransactionNotCommitted, configRequestError(ConfigurationRequestInvalid)
	}
	var record ConfigurationAPIRequestRecord
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		var e error
		record, e = lockConfigurationExecution(ctx, tx, execution)
		if e != nil {
			decision = e
			return e
		}
		if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(481316,151)`); e != nil {
			decision = e
			return e
		}
		var committed bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.config_activation_attempts WHERE attempt_id=$1)`, record.attempt.value[:]).Scan(&committed); e != nil {
			decision = e
			return e
		}
		if committed || len(record.failure) > 0 {
			decision = configRequestError(ConfigurationRequestIntegrity)
			return decision
		}
		// A duplicate completion returns the original capture without calling an
		// encoder against current state or claiming a new validation/publication.
		if len(record.noChange) > 0 {
			return nil
		}
		var actualRevision int64
		var actualManifest, actualSource []byte
		var captured time.Time
		e = tx.QueryRow(ctx, `SELECT a.generation_revision,g.manifest_hash,e.source_hash,clock_timestamp()
   FROM public.config_active a JOIN public.config_generations g ON g.revision=a.generation_revision AND g.sealed
   JOIN public.config_generation_entries e ON e.generation_revision=g.revision
   WHERE a.singleton AND e.document_id=$1 AND e.version_revision=$2`, ref.Document().String(), ref.revision.Number()).Scan(&actualRevision, &actualManifest, &actualSource, &captured)
		if e == pgx.ErrNoRows || e == nil && (actualRevision != generation.Number() || !bytes.Equal(actualManifest, manifest[:]) || !bytes.Equal(actualSource, source[:])) {
			decision = &Error{kind: ConfigConflict}
			return decision
		}
		if e != nil {
			decision = e
			return e
		}
		data, e := encode(record.clone(), captured)
		if e != nil {
			decision = e
			return e
		}
		if len(data) < 1 || len(data) > ConfigOperationResultLimit {
			decision = configRequestError(ConfigurationRequestInvalid)
			return decision
		}
		incoming := bytes.Clone(data)
		digest := sha256.Sum256(incoming)
		record, e = scanConfigurationRequest(tx.QueryRow(ctx, `UPDATE public.config_api_requests SET no_change_result=$2,no_change_sha256=$3,no_change_at=$4 WHERE attempt_id=$1 AND failure_result IS NULL AND no_change_result IS NULL RETURNING `+configurationRequestColumns, record.attempt.value[:], incoming, digest[:], captured))
		if e != nil {
			decision = e
		}
		return e
	})
	if decision != nil {
		var native *Error
		if errors.As(decision, &native) {
			return ConfigurationAPIRequestRecord{}, outcome, native
		}
		return ConfigurationAPIRequestRecord{}, outcome, configRequestSQLFailure(decision)
	}
	if err != nil || outcome != TransactionCommitted {
		return ConfigurationAPIRequestRecord{}, outcome, configRequestError(ConfigurationRequestUnavailable)
	}
	return record.clone(), outcome, nil
}
