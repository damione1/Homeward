package jobs

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Local scheduler budgets, not provider quotas. Two workers keep at most one
// bounded command each. All SQL, including pool acquisition, has a deadline.
const (
	Workers          = 2
	MaxCreations     = 2
	MaintenanceBatch = 32
	SQLTimeout       = 2 * time.Second
	LeaseDuration    = 30 * time.Second
	ExecutionTimeout = 20 * time.Second
	PollInterval     = 250 * time.Millisecond
	MaxDBBackoff     = 4 * time.Second
)

type Repository struct {
	store *storage.Store
}

func NewRepository(store *storage.Store) (*Repository, error) {
	if store == nil {
		return nil, failure(Invalid)
	}
	return &Repository{store: store}, nil
}

// transact does not invent a successful outcome from a callback success. Even
// read results are withheld when their transaction's final outcome is unknown.
func (r *Repository) transact(ctx context.Context, fn func(context.Context, pgx.Tx) error) (storage.TransactionOutcome, error) {
	if r == nil || r.store == nil || nilValue(ctx) || fn == nil {
		return storage.TransactionNotCommitted, failure(Invalid)
	}
	sqlCtx, cancel := context.WithTimeout(ctx, SQLTimeout)
	defer cancel()
	outcome, err := r.store.InTransactionOutcome(sqlCtx, func(tx pgx.Tx) error {
		// The server-side timeout also bounds queries if a client loses its I/O.
		if _, err := tx.Exec(sqlCtx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='2s'`); err != nil {
			return err
		}
		return fn(sqlCtx, tx)
	})
	switch outcome {
	case storage.TransactionCommitted:
		if err == nil {
			return outcome, nil
		}
		return outcome, failure(Unavailable)
	case storage.TransactionIndeterminate:
		return outcome, failure(CommitUnknown)
	case storage.TransactionNotCommitted:
		var domainError *Error
		if errors.As(err, &domainError) {
			return outcome, domainError
		}
		var sqlError *pgconn.PgError
		if errors.As(err, &sqlError) {
			switch sqlError.Code {
			case "42P01", "42703", "42804", "23514", "23503":
				return outcome, failure(Corrupt)
			}
		}
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		return outcome, failure(Unavailable)
	case storage.TransactionOutcomeUnspecified:
		return outcome, failure(Unavailable)
	default:
		return outcome, failure(Unavailable)
	}
}

type CreateResult uint8

const (
	CreateUnspecified CreateResult = iota
	Created
	AlreadyPresent
)

// Schedule commits definition and occurrence together before acknowledging.
// A version has exactly one occurrence in this one-shot tranche. A conflicting
// version, occurrence or idempotency key is never reinterpreted as a new job.
func (r *Repository) Schedule(ctx context.Context, definition Definition, occurrence OccurrenceID, key IdempotencyKey) (CreateResult, storage.TransactionOutcome, error) {
	if !definition.valid() || !identity(occurrence.text) || !identity(key.text) {
		return CreateUnspecified, storage.TransactionNotCommitted, failure(Invalid)
	}
	var result CreateResult
	outcome, err := r.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var owner, session, backup *string
		var command []byte
		var commandText *string
		switch definition.work.kind {
		case SessionCommand:
			o, _ := definition.work.grant.owner.Value()
			s, _ := definition.work.grant.session.Value()
			owner, session, command = &o, &s, definition.work.command
			id, _ := commandID(occurrence).Value()
			commandText = &id
		case SystemBackup:
			b := definition.work.backup.text
			backup = &b
		case WorkUnspecified:
			return failure(Invalid)
		default:
			return failure(Invalid)
		}
		_, err := tx.Exec(ctx, `INSERT INTO public.scheduled_job_definitions
            (job_id,version,deadline_utc,timezone,expires_at_utc,late_policy,tolerance_us,work_kind,owner_id,session_id,command_template,backup_key)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT (job_id,version) DO NOTHING`,
			definition.id.text, definition.version.number, definition.deadline, definition.zone, definition.expires,
			int16(definition.late.kind), definition.late.tolerance.Microseconds(), int16(definition.work.kind), owner, session, command, backup)
		if err != nil {
			return err
		}
		stored, err := scanDefinition(tx.QueryRow(ctx, `SELECT `+definitionColumns+` FROM public.scheduled_job_definitions d WHERE d.job_id=$1 AND d.version=$2`, definition.id.text, definition.version.number))
		if err != nil {
			return err
		}
		if !stored.equal(definition) {
			return failure(Conflict)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO public.scheduled_jobs(occurrence_id,job_id,definition_version,idempotency_key,command_id,state,updated_at)
            VALUES ($1,$2,$3,$4,$5,$6,clock_timestamp()) ON CONFLICT DO NOTHING`,
			occurrence.text, definition.id.text, definition.version.number, key.text, commandText, int16(Pending))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			result = Created
			return nil
		}
		existing, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM public.scheduled_jobs j JOIN public.scheduled_job_definitions d ON d.job_id=j.job_id AND d.version=j.definition_version WHERE j.occurrence_id=$1`, occurrence.text))
		if errors.Is(err, pgx.ErrNoRows) {
			return failure(Conflict)
		}
		if err != nil {
			return err
		}
		if existing.key != key || !existing.definition.equal(definition) {
			return failure(Conflict)
		}
		result = AlreadyPresent
		return nil
	})
	if outcome != storage.TransactionCommitted || err != nil {
		return CreateUnspecified, outcome, err
	}
	return result, outcome, nil
}

const definitionColumns = `d.job_id,d.version,d.deadline_utc,d.timezone,d.expires_at_utc,d.late_policy,d.tolerance_us,d.work_kind,d.owner_id,d.session_id,d.command_template,d.backup_key`
const recordColumns = definitionColumns + `,j.occurrence_id,j.idempotency_key,j.state,j.claim_token,j.lease_until,j.command_id`

type definitionScan struct {
	id, zone               string
	version, tolerance     int64
	deadline, expires      time.Time
	late, kind             int16
	owner, session, backup *string
	command                []byte
}

func (s *definitionScan) destinations() []any {
	return []any{&s.id, &s.version, &s.deadline, &s.zone, &s.expires, &s.late, &s.tolerance, &s.kind, &s.owner, &s.session, &s.command, &s.backup}
}
func (s definitionScan) admitted() (Definition, error) {
	var work Work
	switch WorkKind(s.kind) {
	case SessionCommand:
		if s.kind != int16(SessionCommand) || s.owner == nil || s.session == nil || s.backup != nil {
			return Definition{}, failure(Corrupt)
		}
		owner, ownerErr := domain.ParseActorID(*s.owner)
		session, sessionErr := contracts.ParseSessionID(*s.session)
		grant, grantErr := NewSessionGrant(owner, session)
		if ownerErr != nil || sessionErr != nil || grantErr != nil {
			return Definition{}, failure(Corrupt)
		}
		work = Work{kind: SessionCommand, grant: grant, command: s.command}
	case SystemBackup:
		if s.kind != int16(SystemBackup) || s.backup == nil || s.owner != nil || s.session != nil || s.command != nil {
			return Definition{}, failure(Corrupt)
		}
		work = Work{kind: SystemBackup, backup: BackupKey{*s.backup}}
	case WorkUnspecified:
		return Definition{}, failure(Corrupt)
	default:
		return Definition{}, failure(Corrupt)
	}
	if s.late < 1 || s.late > 2 || s.tolerance < 0 || s.tolerance > (30*24*time.Hour).Microseconds() {
		return Definition{}, failure(Corrupt)
	}
	definition, err := AdmitDefinition(DefinitionInput{JobID{s.id}, Version{s.version}, s.deadline.UTC(), s.zone, s.expires.UTC(), LatePolicy{lateKind(s.late), time.Duration(s.tolerance) * time.Microsecond}, work})
	if err != nil {
		return Definition{}, failure(Corrupt)
	}
	return definition, nil
}
func scanDefinition(row pgx.Row) (Definition, error) {
	var data definitionScan
	if err := row.Scan(data.destinations()...); err != nil {
		return Definition{}, err
	}
	return data.admitted()
}

type Record struct {
	definition Definition
	occurrence OccurrenceID
	key        IdempotencyKey
	state      State
	token      int64
	until      *time.Time
}

func (r Record) Definition() Definition {
	definition, _ := AdmitDefinition(r.definition.Input())
	return definition
}
func (r Record) Occurrence() OccurrenceID       { return r.occurrence }
func (r Record) IdempotencyKey() IdempotencyKey { return r.key }
func (r Record) State() State                   { return r.state }
func (r Record) ClaimGeneration() uint64        { return uint64(r.token) }
func (r Record) LeaseUntil() (time.Time, bool) {
	if r.until == nil {
		return time.Time{}, false
	}
	return *r.until, true
}
func (r Record) CommandID() (domain.CommandID, bool) {
	if r.definition.work.kind != SessionCommand || !identity(r.occurrence.text) {
		return domain.CommandID{}, false
	}
	return commandID(r.occurrence), true
}
func scanRecord(row pgx.Row) (Record, error) {
	var data definitionScan
	var occurrence, key string
	var state int16
	var token int64
	var until *time.Time
	var command *string
	destinations := append(data.destinations(), &occurrence, &key, &state, &token, &until, &command)
	if err := row.Scan(destinations...); err != nil {
		return Record{}, err
	}
	definition, err := data.admitted()
	if err != nil {
		return Record{}, err
	}
	admittedState, stateErr := stateFromSQL(state)
	if stateErr != nil || !identity(occurrence) || !identity(key) || token < 0 {
		return Record{}, failure(Corrupt)
	}
	if (admittedState == Claimed || admittedState == Dispatching) != (until != nil) || (until != nil && token <= 0) {
		return Record{}, failure(Corrupt)
	}
	record := Record{definition, OccurrenceID{occurrence}, IdempotencyKey{key}, admittedState, token, until}
	id, present := record.CommandID()
	if present {
		text, _ := id.Value()
		if command == nil || *command != text {
			return Record{}, failure(Corrupt)
		}
	} else if command != nil {
		return Record{}, failure(Corrupt)
	}
	return record, nil
}

func (r *Repository) Get(ctx context.Context, occurrence OccurrenceID) (Record, bool, error) {
	if !identity(occurrence.text) {
		return Record{}, false, failure(Invalid)
	}
	var record Record
	var found bool
	_, err := r.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		record, err = scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM public.scheduled_jobs j JOIN public.scheduled_job_definitions d ON d.job_id=j.job_id AND d.version=j.definition_version WHERE j.occurrence_id=$1`, occurrence.text))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return Record{}, false, err
	}
	return record, found, nil
}

// Claim is a capability: callers cannot fabricate its token, deadline or data.
type Claim struct{ record Record }

func (c Claim) Record() Record { return c.record }
func (c Claim) valid() bool {
	return c.record.state == Claimed && c.record.token > 0 && c.record.until != nil && c.record.definition.valid() && identity(c.record.occurrence.text)
}

type ClaimResult uint8

const (
	ClaimUnspecified ClaimResult = iota
	NoWork
	ClaimAcquired
	OccurrenceSettled
)

func dueState(definition Definition, now time.Time) State {
	if !now.Before(definition.expires) {
		return Expired
	}
	if definition.late.kind == lateSkip && now.After(definition.deadline.Add(definition.late.tolerance)) {
		return Skipped
	}
	return Claimed
}

func (r *Repository) Claim(ctx context.Context, now time.Time) (Claim, ClaimResult, storage.TransactionOutcome, error) {
	now, err := utcInstant(now)
	if err != nil {
		return Claim{}, ClaimUnspecified, storage.TransactionNotCommitted, err
	}
	var claim Claim
	result := NoWork
	outcome, err := r.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Dispatching is the irreversible uncertainty point. Expired owners are
		// settled in bounded pages; this path never makes them claimable again.
		_, err := tx.Exec(ctx, `WITH uncertain AS (
            SELECT occurrence_id FROM public.scheduled_jobs WHERE state=$1 AND lease_until<=$2
            ORDER BY lease_until,occurrence_id FOR UPDATE SKIP LOCKED LIMIT $3)
            UPDATE public.scheduled_jobs j SET state=$4,lease_until=NULL,updated_at=$2
            FROM uncertain u WHERE j.occurrence_id=u.occurrence_id`, int16(Dispatching), now, MaintenanceBatch, int16(Indeterminate))
		if err != nil {
			return err
		}
		record, err := scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+`
            FROM public.scheduled_jobs j JOIN public.scheduled_job_definitions d ON d.job_id=j.job_id AND d.version=j.definition_version
            WHERE d.deadline_utc<=$1 AND (j.state=$2 OR (j.state=$3 AND j.lease_until<=$1))
            ORDER BY d.deadline_utc,j.occurrence_id FOR UPDATE OF j SKIP LOCKED LIMIT 1`, now, int16(Pending), int16(Claimed)))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		state := dueState(record.definition, now)
		if state != Claimed {
			_, err = tx.Exec(ctx, `UPDATE public.scheduled_jobs SET state=$2,lease_until=NULL,updated_at=$3 WHERE occurrence_id=$1`, record.occurrence.text, int16(state), now)
			result = OccurrenceSettled
			return err
		}
		if record.token == math.MaxInt64 {
			return failure(Unavailable)
		}
		token := record.token + 1
		until := now.Add(LeaseDuration)
		_, err = tx.Exec(ctx, `UPDATE public.scheduled_jobs SET state=$2,claim_token=$3,lease_until=$4,updated_at=$5 WHERE occurrence_id=$1`, record.occurrence.text, int16(Claimed), token, until, now)
		if err != nil {
			return err
		}
		record.state, record.token, record.until = Claimed, token, &until
		claim, result = Claim{record}, ClaimAcquired
		return nil
	})
	if outcome != storage.TransactionCommitted || err != nil {
		return Claim{}, ClaimUnspecified, outcome, err
	}
	return claim, result, outcome, nil
}

func (r *Repository) Renew(ctx context.Context, claim Claim, now time.Time) (Claim, storage.TransactionOutcome, error) {
	now, err := utcInstant(now)
	if err != nil || !claim.valid() {
		return Claim{}, storage.TransactionNotCommitted, failure(Invalid)
	}
	until := now.Add(LeaseDuration)
	outcome, err := r.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public.scheduled_jobs SET lease_until=GREATEST(lease_until,$4),updated_at=$3
            WHERE occurrence_id=$1 AND claim_token=$2 AND state IN ($5,$6) AND lease_until>$3`, claim.record.occurrence.text, claim.record.token, now, until, int16(Claimed), int16(Dispatching))
		if err == nil && tag.RowsAffected() != 1 {
			return failure(LeaseLost)
		}
		return err
	})
	if err != nil {
		return Claim{}, outcome, err
	}
	claim.record.until = &until
	return claim, outcome, nil
}

// BeginDispatch commits before calling any execution port, including backups.
// A crash after this commit may conservatively overstate uncertainty. It can
// never make an effect replayable. CommandID links this row to NHA-024's ledger.
func (r *Repository) BeginDispatch(ctx context.Context, claim Claim, now time.Time) (State, storage.TransactionOutcome, error) {
	now, err := utcInstant(now)
	if err != nil || !claim.valid() {
		return StateUnspecified, storage.TransactionNotCommitted, failure(Invalid)
	}
	state := dueState(claim.record.definition, now)
	if state == Claimed {
		state = Dispatching
	}
	outcome, err := r.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public.scheduled_jobs SET state=$4::smallint,lease_until=CASE WHEN $4::smallint=$6::smallint THEN lease_until ELSE NULL END,updated_at=$3
			WHERE occurrence_id=$1 AND claim_token=$2 AND state=$5 AND lease_until>$3`, claim.record.occurrence.text, claim.record.token, now, int16(state), int16(Claimed), int16(Dispatching))
		if err == nil && tag.RowsAffected() != 1 {
			return failure(LeaseLost)
		}
		return err
	})
	if err != nil {
		return StateUnspecified, outcome, err
	}
	return state, outcome, nil
}

func (r *Repository) Finish(ctx context.Context, claim Claim, state State, now time.Time) (storage.TransactionOutcome, error) {
	now, err := utcInstant(now)
	if err != nil || !claim.valid() {
		return storage.TransactionNotCommitted, failure(Invalid)
	}
	switch state {
	case Succeeded, Rejected, Indeterminate:
		// Only execution outcomes may finish a dispatching claim.
	case StateUnspecified, Pending, Claimed, Dispatching, Expired, Skipped:
		return storage.TransactionNotCommitted, failure(Invalid)
	default:
		return storage.TransactionNotCommitted, failure(Invalid)
	}
	return r.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE public.scheduled_jobs SET state=$4,lease_until=NULL,updated_at=$3
            WHERE occurrence_id=$1 AND claim_token=$2 AND state=$5 AND lease_until>$3`, claim.record.occurrence.text, claim.record.token, now, int16(state), int16(Dispatching))
		if err == nil && tag.RowsAffected() != 1 {
			return failure(LeaseLost)
		}
		return err
	})
}
