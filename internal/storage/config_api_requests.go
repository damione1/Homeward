package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/jackc/pgx/v5"
)

const ConfigurationAPIRequestLimit = 512
const ConfigurationAPIAccountRequestLimit = 64
const ConfigurationAPIFailureBytes = 64 << 10

type ConfigurationAPIMethod uint8

const (
	ConfigurationPublishDocument ConfigurationAPIMethod = iota + 1
	ConfigurationRollbackDocument
	ConfigurationApplyPlan
	ConfigurationRollback
	ConfigurationCreateIntegration
	ConfigurationUpdateIntegration
	ConfigurationDeleteIntegration
)

func (m ConfigurationAPIMethod) Valid() bool {
	switch m {
	case ConfigurationPublishDocument, ConfigurationRollbackDocument, ConfigurationApplyPlan, ConfigurationRollback,
		ConfigurationCreateIntegration, ConfigurationUpdateIntegration, ConfigurationDeleteIntegration:
		return true
	default:
		return false
	}
}

type ConfigurationRequestErrorCode uint8

const (
	ConfigurationRequestInvalid ConfigurationRequestErrorCode = iota + 1
	ConfigurationRequestReused
	ConfigurationRequestCapacity
	ConfigurationRequestIntegrity
	ConfigurationRequestUnavailable
	ConfigurationRequestReplayed
)

type ConfigurationRequestError struct {
	code   ConfigurationRequestErrorCode
	record ConfigurationAPIRequestRecord
}

func (e *ConfigurationRequestError) Error() string {
	return "configuration request journal unavailable [redacted]"
}
func (e *ConfigurationRequestError) Code() ConfigurationRequestErrorCode {
	if e == nil {
		return ConfigurationRequestIntegrity
	}
	return e.code
}
func (e *ConfigurationRequestError) Existing() (ConfigurationAPIRequestRecord, bool) {
	if e == nil || e.code != ConfigurationRequestReplayed || !e.record.Valid() {
		return ConfigurationAPIRequestRecord{}, false
	}
	return e.record.clone(), true
}
func configRequestError(code ConfigurationRequestErrorCode) error {
	return &ConfigurationRequestError{code: code}
}
func configRequestSQLFailure(err error) error {
	var own *ConfigurationRequestError
	if errors.As(err, &own) || errors.Is(err, authorization.ErrDenied) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return configRequestError(ConfigurationRequestUnavailable)
}

// ConfigurationAPIRequest is an admitted identity plus the original native
// global-configuration rights evidence. It contains only a digest of input.
type ConfigurationAPIRequest struct {
	valid       bool
	account     contracts.AccountID
	method      ConfigurationAPIMethod
	id          apitypes.RequestID
	fingerprint [32]byte
	evidence    configurationRequestEvidence
	target      ConfigurationIntegrationTarget
	credential  configurationIntegrationCredential
}

func NewConfigurationAPIRequest(method ConfigurationAPIMethod, id apitypes.RequestID, normalized []byte, evidence authorization.MetadataEvidence) (ConfigurationAPIRequest, error) {
	return newConfigurationAPIRequest(method, id, normalized, configurationRequestEvidence{kind: configurationRequestGlobal, configuration: evidence}, ConfigurationIntegrationTarget{})
}

// NewIntegrationAPIRequest binds native integration authority and the exact
// native target before reservation. Configuration evidence is not accepted.
func NewIntegrationAPIRequest(target ConfigurationIntegrationTarget, id IntegrationRequestID, normalized []byte, evidence authorization.IntegrationAdministrationEvidence) (ConfigurationAPIRequest, error) {
	if _, err := evidence.Rights(); err != nil {
		return ConfigurationAPIRequest{}, authorization.ErrDenied
	}
	if !id.Valid() {
		return ConfigurationAPIRequest{}, configRequestError(ConfigurationRequestInvalid)
	}
	return newConfigurationAPIRequest(target.Method(), id.id, normalized, configurationRequestEvidence{kind: configurationRequestIntegration, integration: evidence}, target)
}

func newConfigurationAPIRequest(method ConfigurationAPIMethod, id apitypes.RequestID, normalized []byte, evidence configurationRequestEvidence, target ConfigurationIntegrationTarget) (ConfigurationAPIRequest, error) {
	rights, err := evidence.rights(method)
	if err != nil {
		return ConfigurationAPIRequest{}, authorization.ErrDenied
	}
	account, err := rights.Account()
	if err != nil {
		return ConfigurationAPIRequest{}, authorization.ErrDenied
	}
	if !method.Valid() || (!id.Valid() && !integrationAPIMethod(method)) || len(normalized) == 0 || len(normalized) > 2<<20 {
		return ConfigurationAPIRequest{}, configRequestError(ConfigurationRequestInvalid)
	}
	if evidence.kind == configurationRequestIntegration && (!target.Valid() || target.Method() != method) {
		return ConfigurationAPIRequest{}, configRequestError(ConfigurationRequestInvalid)
	}
	var credential configurationIntegrationCredential
	if integrationAPIMethod(method) {
		credential, err = integrationCredential(evidence.integration)
		if err != nil {
			return ConfigurationAPIRequest{}, authorization.ErrDenied
		}
	}
	fingerprint := sha256.Sum256(normalized)
	if target.Valid() {
		key, _ := target.Instance().Value()
		targetDigest := sha256.Sum256([]byte(key))
		var bound [65]byte
		bound[0] = byte(method)
		copy(bound[1:33], targetDigest[:])
		copy(bound[33:], fingerprint[:])
		fingerprint = sha256.Sum256(bound[:])
	}
	return ConfigurationAPIRequest{valid: true, account: account, method: method, id: id, fingerprint: fingerprint, evidence: evidence, target: target, credential: credential}, nil
}

func (r ConfigurationAPIRequest) Valid() bool {
	_, err := r.evidence.rights(r.method)
	return err == nil && r.valid && r.method.Valid() && (r.id.Valid() || integrationAPIMethod(r.method)) && r.fingerprint != ([32]byte{}) &&
		(r.evidence.kind != configurationRequestIntegration || (r.target.Valid() && r.target.Method() == r.method && r.credential.valid()))
}
func (ConfigurationAPIRequest) String() string     { return "[REDACTED CONFIGURATION API REQUEST]" }
func (r ConfigurationAPIRequest) GoString() string { return r.String() }

type configurationAPIRequestKey struct{}
type configurationAPIExecutionKey struct{}

func WithConfigurationAPIRequest(ctx context.Context, request ConfigurationAPIRequest) (context.Context, error) {
	if ctx == nil || !request.Valid() {
		return nil, configRequestError(ConfigurationRequestInvalid)
	}
	if _, present := ctx.Value(configurationAPIRequestKey{}).(ConfigurationAPIRequest); present {
		return nil, configRequestError(ConfigurationRequestInvalid)
	}
	return context.WithValue(ctx, configurationAPIRequestKey{}, request), nil
}

type ConfigurationAPIRequestRecord struct {
	account           contracts.AccountID
	method            ConfigurationAPIMethod
	requestID         apitypes.RequestID
	attempt           ActivationAttemptID
	accepted, expires time.Time
	failure           []byte
	noChange          []byte
	noChangeAt        time.Time
	target            ConfigurationIntegrationTarget
	credential        configurationIntegrationCredential
}

func (r ConfigurationAPIRequestRecord) Valid() bool {
	_, err := r.account.Value()
	return err == nil && (integrationAPIMethod(r.method) && r.target.Valid() && r.target.Method() == r.method && r.credential.valid() || !integrationAPIMethod(r.method) && !r.target.Valid() && r.credential == (configurationIntegrationCredential{})) && r.method.Valid() && (r.requestID.Valid() || integrationAPIMethod(r.method)) && r.attempt.Valid() && !r.accepted.IsZero() && r.expires.Equal(r.accepted.Add(24*time.Hour)) && len(r.failure) <= ConfigurationAPIFailureBytes && len(r.noChange) <= ConfigOperationResultLimit &&
		(len(r.noChange) == 0 && r.noChangeAt.IsZero() || len(r.noChange) > 0 && !r.noChangeAt.IsZero() && r.method == ConfigurationUpdateIntegration && len(r.failure) == 0)
}
func (r ConfigurationAPIRequestRecord) ID() ActivationAttemptID        { return r.attempt }
func (r ConfigurationAPIRequestRecord) AcceptedAt() time.Time          { return r.accepted }
func (r ConfigurationAPIRequestRecord) RequestID() apitypes.RequestID  { return r.requestID }
func (r ConfigurationAPIRequestRecord) Method() ConfigurationAPIMethod { return r.method }
func (r ConfigurationAPIRequestRecord) IntegrationTarget() (ConfigurationIntegrationTarget, bool) {
	return r.target, r.target.Valid()
}
func (r ConfigurationAPIRequestRecord) Failure() ([]byte, bool) {
	return bytes.Clone(r.failure), len(r.failure) > 0
}

// NoChange returns only the immutable original redacted response. This is not
// a publication proof or a capability to reconstruct the current resource.
func (r ConfigurationAPIRequestRecord) NoChange() ([]byte, time.Time, bool) {
	return bytes.Clone(r.noChange), r.noChangeAt, len(r.noChange) > 0
}
func (r ConfigurationAPIRequestRecord) clone() ConfigurationAPIRequestRecord {
	r.failure = bytes.Clone(r.failure)
	r.noChange = bytes.Clone(r.noChange)
	return r
}
func (ConfigurationAPIRequestRecord) String() string     { return "[REDACTED CONFIGURATION API RECEIPT]" }
func (r ConfigurationAPIRequestRecord) GoString() string { return r.String() }

// Only a confirmed first reservation constructs an execution. Reads/restarts
// cannot reconstruct its random capability from the SQL digest.
type ConfigurationAPIExecution struct {
	valid      bool
	request    ConfigurationAPIRequest
	record     ConfigurationAPIRequestRecord
	capability [32]byte
}

func (e ConfigurationAPIExecution) Valid() bool {
	return e.valid && e.request.Valid() && e.record.Valid() && e.capability != ([32]byte{})
}
func (e ConfigurationAPIExecution) Record() ConfigurationAPIRequestRecord { return e.record.clone() }
func (ConfigurationAPIExecution) String() string                          { return "[REDACTED CONFIGURATION EXECUTION]" }
func (e ConfigurationAPIExecution) GoString() string                      { return e.String() }

func (e ConfigurationAPIExecution) RecordFailure(ctx context.Context, store *Store, data []byte) (ConfigurationAPIRequestRecord, TransactionOutcome, error) {
	if !e.Valid() || store == nil {
		return ConfigurationAPIRequestRecord{}, TransactionNotCommitted, configRequestError(ConfigurationRequestInvalid)
	}
	return store.recordConfigurationAPIFailure(ctx, e, e.request.evidence, data)
}

func ConfigurationAPIExecutionFromContext(ctx context.Context) (ConfigurationAPIExecution, bool) {
	if ctx == nil {
		return ConfigurationAPIExecution{}, false
	}
	execution, present := ctx.Value(configurationAPIExecutionKey{}).(ConfigurationAPIExecution)
	return execution, present && execution.Valid()
}
func ConfigurationAPIActivationID(ctx context.Context) (ActivationAttemptID, bool) {
	execution, present := ConfigurationAPIExecutionFromContext(ctx)
	if !present {
		return ActivationAttemptID{}, false
	}
	return execution.record.attempt, true
}

func lockConfigurationRequestGate(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1735553389,203)`)
	return err
}

const configurationRequestColumns = `account_id,method,request_id::text,attempt_id,accepted_at,expire_time,failure_result,failure_sha256,failure_at,no_change_result,no_change_sha256,no_change_at,integration_instance_key,integration_credential_kind,integration_credential_id`

func scanConfigurationRequest(row pgx.Row) (ConfigurationAPIRequestRecord, error) {
	var account string
	var requestID, integrationKey, credentialID *string
	var credentialKind *int16
	var method int16
	var id, failure, hash, noChange, noChangeHash []byte
	var accepted, expires time.Time
	var failedAt, noChangeAt *time.Time
	if err := row.Scan(&account, &method, &requestID, &id, &accepted, &expires, &failure, &hash, &failedAt, &noChange, &noChangeHash, &noChangeAt, &integrationKey, &credentialKind, &credentialID); err != nil {
		return ConfigurationAPIRequestRecord{}, err
	}
	if len(id) != 16 || method < 1 || method > 255 {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
	}
	var raw [16]byte
	copy(raw[:], id)
	attempt, err := NewActivationAttemptID(raw)
	if err != nil {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
	}
	actor, err := contracts.ParseAccountID(account)
	if err != nil {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
	}
	var parsed apitypes.RequestID
	if requestID != nil {
		parsed, err = apitypes.ParseRequestID(*requestID)
		if err != nil {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
	} else if !integrationAPIMethod(ConfigurationAPIMethod(method)) {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
	}
	if failure == nil {
		if hash != nil || failedAt != nil {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
	} else {
		digest := sha256.Sum256(failure)
		if len(failure) == 0 || len(failure) > ConfigurationAPIFailureBytes || len(hash) != 32 || !bytes.Equal(hash, digest[:]) || failedAt == nil || failedAt.IsZero() {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
	}
	var capturedAt time.Time
	if noChange == nil {
		if noChangeHash != nil || noChangeAt != nil {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
	} else {
		digest := sha256.Sum256(noChange)
		if len(noChange) == 0 || len(noChange) > ConfigOperationResultLimit || len(noChangeHash) != 32 || !bytes.Equal(noChangeHash, digest[:]) || noChangeAt == nil || noChangeAt.IsZero() || failure != nil || ConfigurationAPIMethod(method) != ConfigurationUpdateIntegration {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
		capturedAt = *noChangeAt
	}
	var target ConfigurationIntegrationTarget
	if integrationKey != nil {
		instance, e := domain.ParseIntegrationInstanceID(*integrationKey)
		if e != nil {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
		target, e = NewConfigurationIntegrationTarget(ConfigurationAPIMethod(method), instance)
		if e != nil {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
	}
	var credential configurationIntegrationCredential
	if credentialKind != nil || credentialID != nil {
		if credentialKind == nil || credentialID == nil {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
		credential = configurationIntegrationCredential{kind: authorization.IntegrationCredentialKind(*credentialKind), id: *credentialID}
		if !credential.valid() {
			return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
		}
	}
	record := ConfigurationAPIRequestRecord{account: actor, method: ConfigurationAPIMethod(method), requestID: parsed, attempt: attempt, accepted: accepted, expires: expires, failure: bytes.Clone(failure), noChange: bytes.Clone(noChange), noChangeAt: capturedAt, target: target, credential: credential}
	if !record.Valid() {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
	}
	return record, nil
}

func fenceConfigurationRequest(ctx context.Context, tx pgx.Tx, request ConfigurationAPIRequest) (string, error) {
	if !request.Valid() {
		return "", configRequestError(ConfigurationRequestInvalid)
	}
	if integrationAPIMethod(request.method) {
		credential, err := integrationCredential(request.evidence.integration)
		if err != nil || credential != request.credential {
			return "", authorization.ErrDenied
		}
	}
	if err := lockConfigSecurityPrefix(ctx, tx); err != nil {
		return "", err
	}
	_, actor, err := request.evidence.actor(request.method)
	if err != nil {
		return "", err
	}
	expected, err := request.account.Value()
	if err != nil || actor != expected {
		return "", authorization.ErrDenied
	}
	if err := request.evidence.fenceSQL(ctx, tx, request.method); err != nil {
		return "", err
	}
	if err := lockConfigurationRequestGate(ctx, tx); err != nil {
		return "", err
	}
	return actor, nil
}

// Probe precedes an ephemeral plan lookup. It is authorized and read-only: an
// expired/restarted plan cannot trigger recompilation of a prior request.
func (s *Store) ProbeConfigurationAPIRequest(ctx context.Context, request ConfigurationAPIRequest) (ConfigurationAPIRequestRecord, bool, error) {
	if ctx == nil || s == nil || s.pool == nil || !request.Valid() {
		return ConfigurationAPIRequestRecord{}, false, configRequestError(ConfigurationRequestInvalid)
	}
	var record ConfigurationAPIRequestRecord
	var present bool
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		actor, e := fenceConfigurationRequest(ctx, tx, request)
		if e != nil {
			decision = e
			return e
		}
		record, e = scanConfigurationRequest(tx.QueryRow(ctx, `SELECT `+configurationRequestColumns+` FROM public.config_api_requests WHERE account_id=$1 AND method=$2 AND request_id=$3::uuid AND expire_time>statement_timestamp() FOR SHARE`, actor, int16(request.method), configurationRequestUUID(request.id)))
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			decision = e
			return e
		}
		if record.credential != request.credential {
			decision = authorization.ErrDenied
			return decision
		}
		var fingerprint []byte
		if e = tx.QueryRow(ctx, `SELECT fingerprint FROM public.config_api_requests WHERE attempt_id=$1`, record.attempt.value[:]).Scan(&fingerprint); e != nil {
			decision = e
			return e
		}
		if !bytes.Equal(fingerprint, request.fingerprint[:]) {
			decision = configRequestError(ConfigurationRequestReused)
			return decision
		}
		present = true
		return nil
	})
	if decision != nil {
		return ConfigurationAPIRequestRecord{}, false, configRequestSQLFailure(decision)
	}
	if err != nil || outcome != TransactionCommitted {
		return ConfigurationAPIRequestRecord{}, false, configRequestError(ConfigurationRequestUnavailable)
	}
	return record.clone(), present, nil
}

func (s *Store) reserveConfigurationAPIRequest(ctx context.Context, request ConfigurationAPIRequest) (ConfigurationAPIExecution, error) {
	if ctx == nil || s == nil || s.pool == nil || !request.Valid() {
		return ConfigurationAPIExecution{}, configRequestError(ConfigurationRequestInvalid)
	}
	attempt, err := RandomActivationAttemptID()
	if err != nil {
		return ConfigurationAPIExecution{}, configRequestError(ConfigurationRequestUnavailable)
	}
	var capability [32]byte
	if _, err := rand.Read(capability[:]); err != nil {
		return ConfigurationAPIExecution{}, configRequestError(ConfigurationRequestUnavailable)
	}
	if capability == ([32]byte{}) {
		return ConfigurationAPIExecution{}, configRequestError(ConfigurationRequestUnavailable)
	}
	digest := sha256.Sum256(capability[:])
	var record ConfigurationAPIRequestRecord
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		actor, e := fenceConfigurationRequest(ctx, tx, request)
		if e != nil {
			decision = e
			return e
		}
		if _, e = tx.Exec(ctx, `DELETE FROM public.config_api_requests WHERE expire_time<=statement_timestamp()`); e != nil {
			decision = e
			return e
		}
		existing, e := scanConfigurationRequest(tx.QueryRow(ctx, `SELECT `+configurationRequestColumns+` FROM public.config_api_requests WHERE account_id=$1 AND method=$2 AND request_id=$3::uuid FOR UPDATE`, actor, int16(request.method), configurationRequestUUID(request.id)))
		if e == nil {
			if existing.credential != request.credential {
				decision = authorization.ErrDenied
				return decision
			}
			var fingerprint []byte
			if e = tx.QueryRow(ctx, `SELECT fingerprint FROM public.config_api_requests WHERE attempt_id=$1`, existing.attempt.value[:]).Scan(&fingerprint); e != nil {
				decision = e
				return e
			}
			if !bytes.Equal(fingerprint, request.fingerprint[:]) {
				decision = configRequestError(ConfigurationRequestReused)
				return decision
			}
			decision = &ConfigurationRequestError{code: ConfigurationRequestReplayed, record: existing}
			return decision
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			decision = e
			return e
		}
		var total, accountTotal int64
		if e = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE account_id=$1) FROM public.config_api_requests`, actor).Scan(&total, &accountTotal); e != nil {
			decision = e
			return e
		}
		if total >= ConfigurationAPIRequestLimit || accountTotal >= ConfigurationAPIAccountRequestLimit {
			decision = configRequestError(ConfigurationRequestCapacity)
			return decision
		}
		record, e = scanConfigurationRequest(tx.QueryRow(ctx, `INSERT INTO public.config_api_requests(account_id,method,request_id,fingerprint,attempt_id,execution_digest,integration_instance_key,integration_credential_kind,integration_credential_id) VALUES($1,$2,$3::uuid,$4,$5,$6,$7,$8,$9) RETURNING `+configurationRequestColumns, actor, int16(request.method), configurationRequestUUID(request.id), request.fingerprint[:], attempt.value[:], digest[:], configurationIntegrationKey(request.target), request.credential.sqlKind(), request.credential.sqlID()))
		if e != nil {
			decision = e
		}
		return e
	})
	if decision != nil {
		return ConfigurationAPIExecution{}, configRequestSQLFailure(decision)
	}
	// An uncertain reservation never yields an execution capability. Subsequent
	// callers may only probe/read an accepted row, even if no stage occurred.
	if err != nil || outcome != TransactionCommitted {
		return ConfigurationAPIExecution{}, configRequestError(ConfigurationRequestUnavailable)
	}
	return ConfigurationAPIExecution{valid: true, request: request, record: record.clone(), capability: capability}, nil
}

// BeginConfigurationAPIContext is invoked by the native Owner before stage.
// Internal trusted activations without an API request need no human identity.
func (s *Store) BeginConfigurationAPIContext(ctx context.Context, author ConfigAuthor) (context.Context, error) {
	if ctx == nil {
		return nil, configRequestError(ConfigurationRequestInvalid)
	}
	request, present := ctx.Value(configurationAPIRequestKey{}).(ConfigurationAPIRequest)
	if !present {
		return ctx, nil
	}
	target, targeted := configurationIntegrationTargetFromContext(ctx)
	switch request.method {
	case ConfigurationCreateIntegration, ConfigurationUpdateIntegration, ConfigurationDeleteIntegration:
		if !targeted || target.method != request.method || target != request.target {
			return nil, configRequestError(ConfigurationRequestInvalid)
		}
	default:
		if targeted {
			return nil, configRequestError(ConfigurationRequestInvalid)
		}
	}
	actor, err := request.account.Value()
	actual, valid := author.Value()
	if err != nil || !valid || actor != actual {
		return nil, authorization.ErrDenied
	}
	if _, present := ConfigurationAPIExecutionFromContext(ctx); present {
		return nil, configRequestError(ConfigurationRequestInvalid)
	}
	execution, err := s.reserveConfigurationAPIRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, configurationAPIExecutionKey{}, execution), nil
}

// ProbeConfigurationAPIContext handles a concurrent duplicate that waited on
// an existing native service gate and consequently observed changed source
// CAS before entering Owner. It reads the first reservation and never issues
// an execution capability or invokes the native mutation again.
func (s *Store) ProbeConfigurationAPIContext(ctx context.Context) (ConfigurationAPIRequestRecord, bool, error) {
	if ctx == nil {
		return ConfigurationAPIRequestRecord{}, false, configRequestError(ConfigurationRequestInvalid)
	}
	request, present := ctx.Value(configurationAPIRequestKey{}).(ConfigurationAPIRequest)
	if !present {
		return ConfigurationAPIRequestRecord{}, false, nil
	}
	return s.ProbeConfigurationAPIRequest(ctx, request)
}

func lockConfigurationExecution(ctx context.Context, tx pgx.Tx, execution ConfigurationAPIExecution) (ConfigurationAPIRequestRecord, error) {
	if !execution.Valid() {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestInvalid)
	}
	actor, err := fenceConfigurationRequest(ctx, tx, execution.request)
	if err != nil {
		return ConfigurationAPIRequestRecord{}, err
	}
	record, err := scanConfigurationRequest(tx.QueryRow(ctx, `SELECT `+configurationRequestColumns+` FROM public.config_api_requests WHERE account_id=$1 AND method=$2 AND attempt_id=$3 FOR UPDATE`, actor, int16(execution.request.method), execution.record.attempt.value[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
	}
	if err != nil {
		return ConfigurationAPIRequestRecord{}, err
	}
	var fingerprint, storedDigest []byte
	if err := tx.QueryRow(ctx, `SELECT fingerprint,execution_digest FROM public.config_api_requests WHERE attempt_id=$1`, record.attempt.value[:]).Scan(&fingerprint, &storedDigest); err != nil {
		return ConfigurationAPIRequestRecord{}, err
	}
	digest := sha256.Sum256(execution.capability[:])
	if len(storedDigest) != 32 || subtle.ConstantTimeCompare(storedDigest, digest[:]) != 1 || !bytes.Equal(fingerprint, execution.request.fingerprint[:]) || record.attempt != execution.record.attempt || record.target != execution.request.target || record.credential != execution.request.credential {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestIntegrity)
	}
	var live bool
	if err := tx.QueryRow(ctx, `SELECT $1::timestamptz>statement_timestamp()`, record.expires).Scan(&live); err != nil {
		return ConfigurationAPIRequestRecord{}, err
	}
	if !live {
		return ConfigurationAPIRequestRecord{}, configRequestError(ConfigurationRequestInvalid)
	}
	return record, nil
}

// ReadConfigurationAPIRequestByAttempt exposes only the original native
// account's unexpired receipt under fresh global-configuration evidence.
func (s *Store) ReadConfigurationAPIRequestByAttempt(ctx context.Context, id ActivationAttemptID, evidence authorization.MetadataEvidence) (ConfigurationAPIRequestRecord, bool, error) {
	return s.readConfigurationAPIRequestByAttempt(ctx, id, configurationRequestEvidence{kind: configurationRequestGlobal, configuration: evidence})
}
func (s *Store) ReadIntegrationAPIRequestByAttempt(ctx context.Context, id ActivationAttemptID, evidence authorization.IntegrationAdministrationEvidence) (ConfigurationAPIRequestRecord, bool, error) {
	return s.readConfigurationAPIRequestByAttempt(ctx, id, configurationRequestEvidence{kind: configurationRequestIntegration, integration: evidence})
}
func (s *Store) readConfigurationAPIRequestByAttempt(ctx context.Context, id ActivationAttemptID, evidence configurationRequestEvidence) (ConfigurationAPIRequestRecord, bool, error) {
	if ctx == nil || s == nil || s.pool == nil || !id.Valid() {
		return ConfigurationAPIRequestRecord{}, false, configRequestError(ConfigurationRequestInvalid)
	}
	method := ConfigurationPublishDocument
	low, high := int16(1), int16(4)
	if evidence.kind == configurationRequestIntegration {
		method = ConfigurationUpdateIntegration
		low, high = 5, 7
	}
	_, actor, err := evidence.actor(method)
	if err != nil {
		return ConfigurationAPIRequestRecord{}, false, err
	}
	var credential configurationIntegrationCredential
	if evidence.kind == configurationRequestIntegration {
		credential, err = integrationCredential(evidence.integration)
		if err != nil {
			return ConfigurationAPIRequestRecord{}, false, authorization.ErrDenied
		}
	}
	var record ConfigurationAPIRequestRecord
	var present bool
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if e := lockConfigSecurityPrefix(ctx, tx); e != nil {
			decision = e
			return e
		}
		if e := evidence.fenceSQL(ctx, tx, method); e != nil {
			decision = e
			return e
		}
		var e error
		record, e = scanConfigurationRequest(tx.QueryRow(ctx, `SELECT `+configurationRequestColumns+` FROM public.config_api_requests WHERE account_id=$1 AND attempt_id=$2 AND method BETWEEN $3 AND $4 AND integration_credential_kind IS NOT DISTINCT FROM $5::smallint AND integration_credential_id IS NOT DISTINCT FROM $6::text AND expire_time>statement_timestamp() FOR SHARE`, actor, id.value[:], low, high, credential.sqlKind(), credential.sqlID()))
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			decision = e
			return e
		}
		present = true
		return nil
	})
	if decision != nil {
		return ConfigurationAPIRequestRecord{}, false, configRequestSQLFailure(decision)
	}
	if err != nil || outcome != TransactionCommitted {
		return ConfigurationAPIRequestRecord{}, false, configRequestError(ConfigurationRequestUnavailable)
	}
	return record.clone(), present, nil
}

// FindConfigurationAPIRequests searches the native owner's immutable receipt
// identities, never a separate activation index or candidate catalogue. The
// current author/admin fence precedes both sources. At most 100 operations may
// be returned, including UUID reuse after its documented 24-hour dedup window.
func (s *Store) FindConfigurationAPIRequests(ctx context.Context, id apitypes.RequestID, cutoff time.Time, evidence authorization.MetadataEvidence) ([]ActivationAttemptID, error) {
	return s.findConfigurationAPIRequests(ctx, id, cutoff, configurationRequestEvidence{kind: configurationRequestGlobal, configuration: evidence})
}
func (s *Store) FindIntegrationAPIRequests(ctx context.Context, id apitypes.RequestID, cutoff time.Time, evidence authorization.IntegrationAdministrationEvidence) ([]ActivationAttemptID, error) {
	return s.findConfigurationAPIRequests(ctx, id, cutoff, configurationRequestEvidence{kind: configurationRequestIntegration, integration: evidence})
}
func (s *Store) findConfigurationAPIRequests(ctx context.Context, id apitypes.RequestID, cutoff time.Time, evidence configurationRequestEvidence) ([]ActivationAttemptID, error) {
	if ctx == nil || s == nil || s.pool == nil || !id.Valid() || cutoff.IsZero() {
		return nil, configRequestError(ConfigurationRequestInvalid)
	}
	method := ConfigurationPublishDocument
	if evidence.kind == configurationRequestIntegration {
		method = ConfigurationUpdateIntegration
	}
	_, actor, err := evidence.actor(method)
	if err != nil {
		return nil, err
	}
	var credential configurationIntegrationCredential
	if evidence.kind == configurationRequestIntegration {
		credential, err = integrationCredential(evidence.integration)
		if err != nil {
			return nil, authorization.ErrDenied
		}
	}
	var found []ActivationAttemptID
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if e := lockConfigSecurityPrefix(ctx, tx); e != nil {
			decision = e
			return e
		}
		if e := evidence.fenceSQL(ctx, tx, method); e != nil {
			decision = e
			return e
		}
		requestOrigin, attemptOrigin := "method BETWEEN 1 AND 4", "api_integration_method IS NULL"
		args := []any{actor, id.String(), cutoff.UTC()}
		if credential.valid() {
			requestOrigin = "integration_credential_kind=$4 AND integration_credential_id=$5"
			attemptOrigin = "api_integration_credential_kind=$4 AND api_integration_credential_id=$5"
			args = append(args, int16(credential.kind), credential.id)
		}
		rows, e := tx.Query(ctx, `SELECT attempt_id FROM (
			SELECT attempt_id FROM public.config_api_requests WHERE account_id=$1 AND `+requestOrigin+` AND request_id=$2::uuid AND expire_time>statement_timestamp() AND accepted_at<=$3
			UNION SELECT attempt_id FROM public.config_activation_attempts WHERE author=$1 AND `+attemptOrigin+` AND api_request_id=$2::uuid AND COALESCE(api_request_create_time,committed_at)<=$3
		) AS requests ORDER BY attempt_id LIMIT 101`, args...)
		if e != nil {
			decision = e
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var value []byte
			if e = rows.Scan(&value); e != nil {
				decision = e
				return e
			}
			if len(value) != 16 {
				decision = configRequestError(ConfigurationRequestIntegrity)
				return decision
			}
			var bytes [16]byte
			copy(bytes[:], value)
			attempt, e := NewActivationAttemptID(bytes)
			if e != nil {
				decision = configRequestError(ConfigurationRequestIntegrity)
				return decision
			}
			found = append(found, attempt)
			if len(found) > 100 {
				decision = configRequestError(ConfigurationRequestCapacity)
				return decision
			}
		}
		decision = rows.Err()
		return decision
	})
	if decision != nil {
		return nil, configRequestSQLFailure(decision)
	}
	if err != nil || outcome != TransactionCommitted {
		return nil, configRequestError(ConfigurationRequestUnavailable)
	}
	return found, nil
}

// ListConfigurationOperationIDs pages actual author-owned attempts and unexpired
// reservations in bytewise ID order. UNION removes the committed reservation
// duplicate; one extra row proves continuation without counting all history.
func (s *Store) ListConfigurationOperationIDs(ctx context.Context, after *ActivationAttemptID, pageSize uint32, cutoff time.Time, evidence authorization.MetadataEvidence) ([]ActivationAttemptID, bool, error) {
	return s.listConfigurationOperationIDs(ctx, after, pageSize, cutoff, configurationRequestEvidence{kind: configurationRequestGlobal, configuration: evidence})
}
func (s *Store) ListIntegrationOperationIDs(ctx context.Context, after *ActivationAttemptID, pageSize uint32, cutoff time.Time, evidence authorization.IntegrationAdministrationEvidence) ([]ActivationAttemptID, bool, error) {
	return s.listConfigurationOperationIDs(ctx, after, pageSize, cutoff, configurationRequestEvidence{kind: configurationRequestIntegration, integration: evidence})
}
func (s *Store) listConfigurationOperationIDs(ctx context.Context, after *ActivationAttemptID, pageSize uint32, cutoff time.Time, evidence configurationRequestEvidence) ([]ActivationAttemptID, bool, error) {
	if ctx == nil || s == nil || s.pool == nil || (after != nil && !after.Valid()) || pageSize < 1 || pageSize > 500 || cutoff.IsZero() {
		return nil, false, configRequestError(ConfigurationRequestInvalid)
	}
	method := ConfigurationPublishDocument
	if evidence.kind == configurationRequestIntegration {
		method = ConfigurationUpdateIntegration
	}
	_, actor, err := evidence.actor(method)
	if err != nil {
		return nil, false, err
	}
	cursor := []byte{}
	if after != nil {
		cursor = after.value[:]
	}
	var credential configurationIntegrationCredential
	if evidence.kind == configurationRequestIntegration {
		credential, err = integrationCredential(evidence.integration)
		if err != nil {
			return nil, false, authorization.ErrDenied
		}
	}
	var found []ActivationAttemptID
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		if e := lockConfigSecurityPrefix(ctx, tx); e != nil {
			decision = e
			return e
		}
		if e := evidence.fenceSQL(ctx, tx, method); e != nil {
			decision = e
			return e
		}
		requestOrigin, attemptOrigin := "method BETWEEN 1 AND 4", "api_integration_method IS NULL"
		args := []any{actor, cursor, int(pageSize) + 1, cutoff.UTC()}
		if credential.valid() {
			requestOrigin = "integration_credential_kind=$5 AND integration_credential_id=$6"
			attemptOrigin = "api_integration_credential_kind=$5 AND api_integration_credential_id=$6"
			args = append(args, int16(credential.kind), credential.id)
		}
		rows, e := tx.Query(ctx, `SELECT attempt_id FROM (
			(SELECT attempt_id FROM public.config_api_requests WHERE account_id=$1 AND `+requestOrigin+` AND expire_time>statement_timestamp() AND accepted_at<=$4 AND attempt_id>$2::bytea ORDER BY attempt_id LIMIT $3)
			UNION (SELECT attempt_id FROM public.config_activation_attempts WHERE author=$1 AND `+attemptOrigin+` AND COALESCE(api_request_create_time,committed_at)<=$4 AND attempt_id>$2::bytea ORDER BY attempt_id LIMIT $3)
		) AS requests ORDER BY attempt_id LIMIT $3`, args...)
		if e != nil {
			decision = e
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var value []byte
			if e = rows.Scan(&value); e != nil {
				decision = e
				return e
			}
			if len(value) != 16 {
				decision = configRequestError(ConfigurationRequestIntegrity)
				return decision
			}
			var bytes [16]byte
			copy(bytes[:], value)
			attempt, e := NewActivationAttemptID(bytes)
			if e != nil {
				decision = configRequestError(ConfigurationRequestIntegrity)
				return decision
			}
			found = append(found, attempt)
		}
		decision = rows.Err()
		return decision
	})
	if decision != nil {
		return nil, false, configRequestSQLFailure(decision)
	}
	if err != nil || outcome != TransactionCommitted {
		return nil, false, configRequestError(ConfigurationRequestUnavailable)
	}
	more := len(found) > int(pageSize)
	if more {
		found = found[:pageSize]
	}
	return found, more, nil
}

// RecordConfigurationAPIFailure is append-only, including same-byte retries.
// The request row and 151 serialize it with the real activation transaction.
// Only the native result owner may supply the privately issued execution; the
// caller must separately admit the exact canonical Status/metadata payload.
func (s *Store) RecordConfigurationAPIFailure(ctx context.Context, execution ConfigurationAPIExecution, evidence authorization.MetadataEvidence, data []byte) (ConfigurationAPIRequestRecord, TransactionOutcome, error) {
	return s.recordConfigurationAPIFailure(ctx, execution, configurationRequestEvidence{kind: configurationRequestGlobal, configuration: evidence}, data)
}

func (s *Store) recordConfigurationAPIFailure(ctx context.Context, execution ConfigurationAPIExecution, evidence configurationRequestEvidence, data []byte) (ConfigurationAPIRequestRecord, TransactionOutcome, error) {
	if ctx == nil || s == nil || s.pool == nil || !execution.Valid() || len(data) < 1 || len(data) > ConfigurationAPIFailureBytes {
		return ConfigurationAPIRequestRecord{}, TransactionNotCommitted, configRequestError(ConfigurationRequestInvalid)
	}
	if _, err := evidence.rights(execution.request.method); err != nil {
		return ConfigurationAPIRequestRecord{}, TransactionNotCommitted, authorization.ErrDenied
	}
	current := execution
	current.request.evidence = evidence
	incoming := bytes.Clone(data)
	digest := sha256.Sum256(incoming)
	var record ConfigurationAPIRequestRecord
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		var e error
		record, e = lockConfigurationExecution(ctx, tx, current)
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
		if committed {
			decision = configRequestError(ConfigurationRequestIntegrity)
			return decision
		}
		if len(record.noChange) > 0 {
			decision = configRequestError(ConfigurationRequestIntegrity)
			return decision
		}
		if len(record.failure) > 0 {
			if !bytes.Equal(record.failure, incoming) {
				decision = configRequestError(ConfigurationRequestIntegrity)
				return decision
			}
			return nil
		}
		record, e = scanConfigurationRequest(tx.QueryRow(ctx, `UPDATE public.config_api_requests SET failure_result=$2,failure_sha256=$3,failure_at=clock_timestamp() WHERE attempt_id=$1 AND failure_result IS NULL RETURNING `+configurationRequestColumns, record.attempt.value[:], incoming, digest[:]))
		if e != nil {
			decision = e
		}
		return e
	})
	if decision != nil {
		return ConfigurationAPIRequestRecord{}, outcome, configRequestSQLFailure(decision)
	}
	if err != nil || outcome != TransactionCommitted {
		return ConfigurationAPIRequestRecord{}, outcome, configRequestError(ConfigurationRequestUnavailable)
	}
	return record.clone(), outcome, nil
}
