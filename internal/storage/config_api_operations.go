package storage

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/jackc/pgx/v5"
)

const ConfigOperationResultLimit = 1 << 20

// ConfigOperationResult is an immutable, bounded terminal publication record.
// The API owner admits its allowed protobuf result/metadata types before writing
// and after reading. Storage never manufactures a publication result from SQL
// revision ordering, and never rewrites the original activation audit facts.
type ConfigOperationResult struct {
	data     []byte
	observed time.Time
}

func (v ConfigOperationResult) Valid() bool {
	return len(v.data) > 0 && len(v.data) <= ConfigOperationResultLimit && !v.observed.IsZero()
}
func (v ConfigOperationResult) Bytes() []byte                 { return bytes.Clone(v.data) }
func (v ConfigOperationResult) ObservedAt() (time.Time, bool) { return v.observed, v.Valid() }
func (ConfigOperationResult) String() string                  { return "[REDACTED CONFIGURATION OPERATION RESULT]" }
func (v ConfigOperationResult) GoString() string              { return v.String() }

func configOperationActor(evidence authorization.MetadataEvidence) (authorization.RightsEvidence, string, error) {
	rights, err := evidence.Rights()
	if err != nil {
		return authorization.RightsEvidence{}, "", authorization.ErrDenied
	}
	account, err := rights.Account()
	if err != nil {
		return authorization.RightsEvidence{}, "", authorization.ErrDenied
	}
	actor, err := account.Value()
	if err != nil {
		return authorization.RightsEvidence{}, "", authorization.ErrDenied
	}
	return rights, actor, nil
}
func configOperationStored(data []byte, observed *time.Time) (ConfigOperationResult, bool, error) {
	if data == nil && observed == nil {
		return ConfigOperationResult{}, false, nil
	}
	if len(data) < 1 || len(data) > ConfigOperationResultLimit || observed == nil || observed.IsZero() {
		return ConfigOperationResult{}, false, &Error{kind: ConfigIntegrity}
	}
	return ConfigOperationResult{data: bytes.Clone(data), observed: *observed}, true, nil
}

// ReadConfigurationOperationResult rechecks native administrator rights and the
// attempt author under the account/policy SQL fence. A result is returned only
// after that read transaction succeeds; absence never means publication failed.
func (s *Store) ReadConfigurationOperationResult(ctx context.Context, id ActivationAttemptID, evidence authorization.MetadataEvidence) (ConfigOperationResult, bool, error) {
	return s.readConfigurationOperationResult(ctx, id, configurationRequestEvidence{kind: configurationRequestGlobal, configuration: evidence})
}
func (s *Store) ReadIntegrationOperationResult(ctx context.Context, id ActivationAttemptID, evidence authorization.IntegrationAdministrationEvidence) (ConfigOperationResult, bool, error) {
	return s.readConfigurationOperationResult(ctx, id, configurationRequestEvidence{kind: configurationRequestIntegration, integration: evidence})
}
func (s *Store) readConfigurationOperationResult(ctx context.Context, id ActivationAttemptID, evidence configurationRequestEvidence) (ConfigOperationResult, bool, error) {
	if ctx == nil || s == nil || s.pool == nil || !id.Valid() {
		return ConfigOperationResult{}, false, &Error{kind: ConfigInvalid}
	}
	method := ConfigurationPublishDocument
	if evidence.kind == configurationRequestIntegration {
		method = ConfigurationUpdateIntegration
	}
	_, actor, err := evidence.actor(method)
	if err != nil {
		return ConfigOperationResult{}, false, err
	}
	var credential configurationIntegrationCredential
	if evidence.kind == configurationRequestIntegration {
		credential, err = integrationCredential(evidence.integration)
		if err != nil {
			return ConfigOperationResult{}, false, authorization.ErrDenied
		}
	}
	var result ConfigOperationResult
	var present bool
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if err := lockConfigSecurityPrefix(ctx, tx); err != nil {
			return err
		}
		if err := evidence.fenceSQL(ctx, tx, method); err != nil {
			return err
		}
		var data []byte
		var observed *time.Time
		err := tx.QueryRow(ctx, `SELECT api_operation_result,api_operation_done_at FROM public.config_activation_attempts WHERE attempt_id=$1 AND author=$2 AND api_integration_credential_kind IS NOT DISTINCT FROM $3::smallint AND api_integration_credential_id IS NOT DISTINCT FROM $4::text FOR SHARE`, id.value[:], actor, credential.sqlKind(), credential.sqlID()).Scan(&data, &observed)
		if errors.Is(err, pgx.ErrNoRows) {
			return authorization.ErrDenied
		}
		if err != nil {
			return &Error{kind: Unavailable}
		}
		var e error
		result, present, e = configOperationStored(data, observed)
		return e
	})
	if err != nil {
		return ConfigOperationResult{}, false, err
	}
	if outcome != TransactionCommitted {
		return ConfigOperationResult{}, false, &Error{kind: Unavailable}
	}
	return result, present, nil
}

// RecordConfigurationOperationResult is called only after the native Owner has
// proved Published, or an exact current generation/manifest publication. Rights
// are fenced first; the same existing attempt/author/target is then locked. The
// first terminal bytes are append-only, while an identical concurrent recording
// returns the original bytes/time without UPDATE or extending any retention.
// No compiler, runtime/authority callback or physical effect runs under SQL.
func (s *Store) RecordConfigurationOperationResult(ctx context.Context, id ActivationAttemptID, target ConfigGenerationRevision, manifest [32]byte, evidence authorization.MetadataEvidence, data []byte) (ConfigOperationResult, TransactionOutcome, error) {
	return s.recordConfigurationOperationResult(ctx, id, target, manifest, configurationRequestEvidence{kind: configurationRequestGlobal, configuration: evidence}, data)
}
func (s *Store) RecordIntegrationOperationResult(ctx context.Context, id ActivationAttemptID, target ConfigGenerationRevision, manifest [32]byte, evidence authorization.IntegrationAdministrationEvidence, data []byte) (ConfigOperationResult, TransactionOutcome, error) {
	return s.recordConfigurationOperationResult(ctx, id, target, manifest, configurationRequestEvidence{kind: configurationRequestIntegration, integration: evidence}, data)
}
func (s *Store) recordConfigurationOperationResult(ctx context.Context, id ActivationAttemptID, target ConfigGenerationRevision, manifest [32]byte, evidence configurationRequestEvidence, data []byte) (ConfigOperationResult, TransactionOutcome, error) {
	if ctx == nil || s == nil || s.pool == nil || !id.Valid() || !target.Valid() || manifest == ([32]byte{}) || len(data) < 1 || len(data) > ConfigOperationResultLimit {
		return ConfigOperationResult{}, TransactionNotCommitted, &Error{kind: ConfigInvalid}
	}
	method := ConfigurationPublishDocument
	if evidence.kind == configurationRequestIntegration {
		method = ConfigurationUpdateIntegration
	}
	_, actor, err := evidence.actor(method)
	if err != nil {
		return ConfigOperationResult{}, TransactionNotCommitted, err
	}
	incoming := bytes.Clone(data)
	var credential configurationIntegrationCredential
	if evidence.kind == configurationRequestIntegration {
		credential, err = integrationCredential(evidence.integration)
		if err != nil {
			return ConfigOperationResult{}, TransactionNotCommitted, authorization.ErrDenied
		}
	}
	var result ConfigOperationResult
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if err := lockConfigSecurityPrefix(ctx, tx); err != nil {
			return err
		}
		if err := evidence.fenceSQL(ctx, tx, method); err != nil {
			return err
		}
		var revision int64
		var actual, stored []byte
		var observed *time.Time
		err := tx.QueryRow(ctx, `SELECT target_generation,target_manifest_hash,api_operation_result,api_operation_done_at FROM public.config_activation_attempts WHERE attempt_id=$1 AND author=$2 AND api_integration_credential_kind IS NOT DISTINCT FROM $3::smallint AND api_integration_credential_id IS NOT DISTINCT FROM $4::text FOR UPDATE`, id.value[:], actor, credential.sqlKind(), credential.sqlID()).Scan(&revision, &actual, &stored, &observed)
		if errors.Is(err, pgx.ErrNoRows) {
			return authorization.ErrDenied
		}
		if err != nil {
			return &Error{kind: Unavailable}
		}
		if revision != target.Number() || len(actual) != 32 || !bytes.Equal(actual, manifest[:]) {
			return &Error{kind: ConfigIntegrity}
		}
		existing, present, err := configOperationStored(stored, observed)
		if err != nil {
			return err
		}
		if present {
			if !bytes.Equal(existing.data, incoming) {
				return &Error{kind: ConfigConflict}
			}
			result = existing
			return nil
		}
		var written []byte
		var done time.Time
		err = tx.QueryRow(ctx, `UPDATE public.config_activation_attempts SET api_operation_result=$3,api_operation_done_at=clock_timestamp() WHERE attempt_id=$1 AND author=$2 AND api_operation_result IS NULL AND api_operation_done_at IS NULL RETURNING api_operation_result,api_operation_done_at`, id.value[:], actor, incoming).Scan(&written, &done)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{kind: ConfigConflict}
		}
		if err != nil {
			return &Error{kind: Unavailable}
		}
		result = ConfigOperationResult{data: bytes.Clone(written), observed: done}
		if !result.Valid() {
			return &Error{kind: ConfigIntegrity}
		}
		return nil
	})
	if err != nil {
		return ConfigOperationResult{}, outcome, err
	}
	if outcome != TransactionCommitted {
		return ConfigOperationResult{}, outcome, &Error{kind: Unavailable}
	}
	return result, outcome, nil
}
