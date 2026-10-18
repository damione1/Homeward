package storage

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/accounts"
	"github.com/jackc/pgx/v5"
)

const sessionAccessLifetime = 15 * time.Minute
const sessionAbsoluteLifetime = 30 * 24 * time.Hour
const sessionLimitPerAccount = 16
const sessionGenerationLimit = 4096
const sessionExpiredPruneBatch = 64

var ErrSessionInvalid = errors.New("sessions: invalid input")
var ErrSessionUnavailable = errors.New("sessions: unavailable")
var ErrSessionNotFound = errors.New("sessions: credential or session unavailable")
var ErrSessionPermission = errors.New("sessions: permission denied")
var ErrSessionCommitUnknown = errors.New("sessions: commit outcome unknown")

type SessionRecord struct {
	id        contracts.SessionID
	account   contracts.AccountID
	createdAt time.Time
	expiresAt time.Time
	revokedAt *time.Time
	lastUsed  *time.Time
}

func (r SessionRecord) ID() (contracts.SessionID, error) {
	_, err := r.id.Value()
	return r.id, err
}
func (r SessionRecord) AccountID() (contracts.AccountID, error) {
	_, err := r.account.Value()
	return r.account, err
}
func (r SessionRecord) CreatedAt() time.Time { return r.createdAt }
func (r SessionRecord) ExpiresAt() time.Time { return r.expiresAt }
func (r SessionRecord) RevokedAt() *time.Time {
	if r.revokedAt == nil {
		return nil
	}
	v := *r.revokedAt
	return &v
}
func (r SessionRecord) LastUsedAt() *time.Time {
	if r.lastUsed == nil {
		return nil
	}
	v := *r.lastUsed
	return &v
}

type SessionOwner struct {
	Account contracts.AccountID
	Session contracts.SessionID
}

type AccessAdmission struct {
	Owner            SessionOwner
	AccessExpiresAt  time.Time
	SessionExpiresAt time.Time
	Role             authv1.AccountRole
}

type SessionCreation struct {
	ID            contracts.SessionID
	Account       contracts.AccountID
	Revision      RevocationRevision
	ChallengeID   *contracts.ChallengeID
	AccessDigest  accounts.AccessDigest
	RefreshDigest accounts.RefreshDigest
	Now           time.Time
}

type CreatedSession struct {
	Session         SessionRecord
	AccessExpiresAt time.Time
	Evicted         *contracts.SessionID
}

type RefreshRotation struct {
	Owner           SessionOwner
	OldDigest       accounts.RefreshDigest
	NewDigest       accounts.RefreshDigest
	NewAccessDigest accounts.AccessDigest
	Now             time.Time
}

type refreshRotationKind uint8

const (
	refreshRotationUnspecified refreshRotationKind = iota
	refreshRotationSucceeded
	refreshRotationReplayRevoked
	refreshRotationGenerationExhausted
)

// RotatedSession is a closed result: only a successful rotation carries a
// usable session and access expiry. Zero is never a successful outcome.
type RotatedSession struct {
	kind            refreshRotationKind
	session         SessionRecord
	accessExpiresAt time.Time
}

func (r RotatedSession) Rotated() (SessionRecord, time.Time, bool) {
	if r.kind != refreshRotationSucceeded || r.accessExpiresAt.IsZero() {
		return SessionRecord{}, time.Time{}, false
	}
	if _, err := r.session.ID(); err != nil {
		return SessionRecord{}, time.Time{}, false
	}
	return r.session, r.accessExpiresAt, true
}
func (r RotatedSession) ReplayRevoked() bool { return r.kind == refreshRotationReplayRevoked }
func (r RotatedSession) GenerationExhausted() bool {
	return r.kind == refreshRotationGenerationExhausted
}

func sessionTransaction[T any](s *Store, ctx context.Context, fn func(pgx.Tx) (T, error)) (T, error) {
	var zero T
	if err := accountContextError(ctx); err != nil || s == nil || s.pool == nil || fn == nil {
		return zero, ErrSessionInvalid
	}
	var value T
	var decision error
	outcome, err := s.InTransactionOutcome(ctx, func(tx pgx.Tx) error {
		value, decision = fn(tx)
		return decision
	})
	if outcome == TransactionIndeterminate {
		return zero, ErrSessionCommitUnknown
	}
	if decision != nil {
		return zero, decision
	}
	if err != nil || outcome != TransactionCommitted {
		return zero, ErrSessionUnavailable
	}
	return value, nil
}

func scanSession(row pgx.Row) (SessionRecord, error) {
	var id, account string
	var created, expires time.Time
	var revoked, last *time.Time
	if err := row.Scan(&id, &account, &created, &expires, &revoked, &last); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionRecord{}, ErrSessionNotFound
		}
		return SessionRecord{}, ErrSessionUnavailable
	}
	sessionID, err := contracts.ParseSessionID(id)
	if err != nil {
		return SessionRecord{}, ErrSessionUnavailable
	}
	accountID, err := contracts.ParseAccountID(account)
	if err != nil {
		return SessionRecord{}, ErrSessionUnavailable
	}
	return SessionRecord{id: sessionID, account: accountID, createdAt: created, expiresAt: expires, revokedAt: revoked, lastUsed: last}, nil
}

const sessionColumns = `s.id,s.account_id,s.created_at,s.expires_at,s.revoked_at,s.last_used_at`

func (s *Store) LookupAccessOwner(ctx context.Context, digest accounts.AccessDigest) (SessionOwner, error) {
	raw, err := digest.Bytes()
	if err != nil {
		return SessionOwner{}, ErrSessionInvalid
	}
	return s.lookupSessionOwner(ctx, `SELECT s.account_id,s.id FROM public.auth_access_credentials a JOIN public.auth_sessions s ON s.id=a.session_id WHERE a.token_digest=$1`, raw)
}

func (s *Store) LookupRefreshOwner(ctx context.Context, digest accounts.RefreshDigest) (SessionOwner, error) {
	raw, err := digest.Bytes()
	if err != nil {
		return SessionOwner{}, ErrSessionInvalid
	}
	return s.lookupSessionOwner(ctx, `SELECT s.account_id,s.id FROM public.auth_refresh_generations g JOIN public.auth_sessions s ON s.id=g.session_id WHERE g.token_digest=$1`, raw)
}

func (s *Store) LookupSessionOwner(ctx context.Context, id contracts.SessionID) (SessionOwner, error) {
	value, err := id.Value()
	if err != nil {
		return SessionOwner{}, ErrSessionInvalid
	}
	if err := accountContextError(ctx); err != nil || s == nil || s.pool == nil {
		return SessionOwner{}, ErrSessionInvalid
	}
	return scanSessionOwner(s.pool.QueryRow(ctx, `SELECT account_id,id FROM public.auth_sessions WHERE id=$1`, value))
}

// ConfirmAccountSettled takes the same database lock order as NHA-049
// mutations. After an indeterminate commit, waiting for these locks makes a
// subsequent admission observe its final authoritative account state. A
// deleted account is also a settled (denied) state.
func (s *Store) ConfirmAccountSettled(ctx context.Context, id contracts.AccountID) error {
	account, err := sqlAccountID(id)
	if err != nil {
		return ErrSessionInvalid
	}
	_, err = sessionTransaction(s, ctx, func(tx pgx.Tx) (struct{}, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return struct{}{}, ErrSessionUnavailable
		}
		var revision int64
		err := tx.QueryRow(ctx, `SELECT revocation_revision FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return struct{}{}, nil
		}
		if err != nil || revision <= 0 {
			return struct{}{}, ErrSessionUnavailable
		}
		return struct{}{}, nil
	})
	return err
}

func (s *Store) lookupSessionOwner(ctx context.Context, query string, digest [32]byte) (SessionOwner, error) {
	if err := accountContextError(ctx); err != nil || s == nil || s.pool == nil || digest == [32]byte{} {
		return SessionOwner{}, ErrSessionInvalid
	}
	return scanSessionOwner(s.pool.QueryRow(ctx, query, digest[:]))
}

func scanSessionOwner(row pgx.Row) (SessionOwner, error) {
	var account, session string
	if err := row.Scan(&account, &session); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SessionOwner{}, ErrSessionNotFound
		}
		return SessionOwner{}, ErrSessionUnavailable
	}
	a, err := contracts.ParseAccountID(account)
	if err != nil {
		return SessionOwner{}, ErrSessionUnavailable
	}
	id, err := contracts.ParseSessionID(session)
	if err != nil {
		return SessionOwner{}, ErrSessionUnavailable
	}
	return SessionOwner{Account: a, Session: id}, nil
}

// AdmitAccess is the authoritative read after the local account lock. A
// LookupAccessOwner result alone must never authorize a request.
func (s *Store) AdmitAccess(ctx context.Context, owner SessionOwner, digest accounts.AccessDigest, now time.Time) (AccessAdmission, error) {
	account, err := sqlAccountID(owner.Account)
	if err != nil {
		return AccessAdmission{}, ErrSessionInvalid
	}
	session, err := owner.Session.Value()
	raw, digestErr := digest.Bytes()
	if err != nil || digestErr != nil || now.IsZero() || s == nil || s.pool == nil || accountContextError(ctx) != nil {
		return AccessAdmission{}, ErrSessionInvalid
	}
	var foundAccount, foundSession string
	var accessExpiry, sessionExpiry time.Time
	var role int16
	err = s.pool.QueryRow(ctx, `SELECT s.account_id,s.id,a.expires_at,s.expires_at,c.role
        FROM public.auth_access_credentials a
        JOIN public.auth_sessions s ON s.id=a.session_id
        JOIN public.auth_accounts c ON c.id=s.account_id
        WHERE a.token_digest=$1 AND s.id=$2 AND s.account_id=$3 AND a.audience=1
        AND s.revoked_at IS NULL AND c.status=1 AND c.revocation_revision=s.account_revision`,
		raw[:], session, account).Scan(&foundAccount, &foundSession, &accessExpiry, &sessionExpiry, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessAdmission{}, ErrSessionNotFound
	}
	if err != nil {
		return AccessAdmission{}, ErrSessionUnavailable
	}
	if !now.Before(accessExpiry) || !now.Before(sessionExpiry) {
		return AccessAdmission{}, ErrSessionNotFound
	}
	if foundAccount != account || foundSession != session ||
		role != int16(authv1.AccountRole_ACCOUNT_ROLE_ADMIN) && role != int16(authv1.AccountRole_ACCOUNT_ROLE_USER) {
		return AccessAdmission{}, ErrSessionUnavailable
	}
	return AccessAdmission{Owner: owner, AccessExpiresAt: accessExpiry, SessionExpiresAt: sessionExpiry, Role: authv1.AccountRole(role)}, nil
}

func (s *Store) CreateSession(ctx context.Context, input SessionCreation) (CreatedSession, error) {
	id, err := input.ID.Value()
	accessDigest, accessErr := input.AccessDigest.Bytes()
	refreshDigest, refreshErr := input.RefreshDigest.Bytes()
	if err != nil || len(id) > 128 || !input.Revision.validFor(input.Account) ||
		accessErr != nil || refreshErr != nil || input.Now.IsZero() {
		return CreatedSession{}, ErrSessionInvalid
	}
	account, err := sqlAccountID(input.Account)
	if err != nil {
		return CreatedSession{}, ErrSessionInvalid
	}
	return sessionTransaction(s, ctx, func(tx pgx.Tx) (CreatedSession, error) {
		var revision int64
		var status int16
		var loginText string
		var factorEnabled bool
		if err := tx.QueryRow(ctx, `SELECT revocation_revision,status,login_name,second_factor_enabled FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&revision, &status, &loginText, &factorEnabled); err != nil {
			return CreatedSession{}, ErrSessionNotFound
		}
		if status != 1 || revision != input.Revision.number {
			return CreatedSession{}, ErrSessionNotFound
		}
		if input.ChallengeID == nil {
			if factorEnabled {
				return CreatedSession{}, ErrSessionNotFound
			}
		} else {
			challengeID, err := input.ChallengeID.Value()
			if err != nil {
				return CreatedSession{}, ErrSessionInvalid
			}
			command, err := tx.Exec(ctx, `UPDATE public.auth_login_challenges SET consumed_at=$4
                WHERE id=$1 AND account_id=$2 AND account_revision=$3 AND expires_at>$4
                AND consumed_at IS NULL AND attempts BETWEEN 1 AND 5`, challengeID, account, revision, input.Now)
			if err != nil || command.RowsAffected() != 1 {
				return CreatedSession{}, ErrSessionNotFound
			}
		}
		// Keep consumed refresh digests for the full absolute lifetime. A new
		// issuance prunes only this locked account's expired families in a
		// bounded batch; child digests disappear through the schema's cascades.
		if _, err := tx.Exec(ctx, `DELETE FROM public.auth_sessions WHERE id IN (
			SELECT id FROM public.auth_sessions WHERE account_id=$1 AND expires_at<=$2
			ORDER BY expires_at,id LIMIT $3 FOR UPDATE)`, account, input.Now, sessionExpiredPruneBatch); err != nil {
			return CreatedSession{}, ErrSessionUnavailable
		}
		// Expired rows no longer count, but their consumed digests stay until the
		// absolute deadline. New issuance deterministically evicts the oldest.
		var evicted *contracts.SessionID
		var oldest string
		err := tx.QueryRow(ctx, `SELECT id FROM public.auth_sessions WHERE account_id=$1 AND revoked_at IS NULL AND expires_at>$2
            ORDER BY created_at,id OFFSET $3 LIMIT 1`, account, input.Now, sessionLimitPerAccount-1).Scan(&oldest)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return CreatedSession{}, ErrSessionUnavailable
		}
		if err == nil {
			var victim string
			if err := tx.QueryRow(ctx, `SELECT id FROM public.auth_sessions WHERE account_id=$1 AND revoked_at IS NULL AND expires_at>$2
                ORDER BY created_at,id LIMIT 1`, account, input.Now).Scan(&victim); err != nil {
				return CreatedSession{}, ErrSessionUnavailable
			}
			if _, err := tx.Exec(ctx, `UPDATE public.auth_sessions SET revoked_at=$2 WHERE id=$1`, victim, input.Now); err != nil {
				return CreatedSession{}, ErrSessionUnavailable
			}
			parsed, err := contracts.ParseSessionID(victim)
			if err != nil {
				return CreatedSession{}, ErrSessionUnavailable
			}
			evicted = &parsed
		}
		expires := input.Now.Add(sessionAbsoluteLifetime)
		accessExpires := input.Now.Add(sessionAccessLifetime)
		if accessExpires.After(expires) {
			accessExpires = expires
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_sessions(id,account_id,account_revision,created_at,expires_at,current_generation)
            VALUES($1,$2,$3,$4,$5,1)`, id, account, revision, input.Now, expires); err != nil {
			return CreatedSession{}, ErrSessionUnavailable
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_refresh_generations(session_id,generation,token_digest,state,issued_at)
			VALUES($1,1,$2,1,$3)`, id, refreshDigest[:], input.Now); err != nil {
			return CreatedSession{}, ErrSessionUnavailable
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_access_credentials(session_id,token_digest,audience,expires_at)
			VALUES($1,$2,1,$3)`, id, accessDigest[:], accessExpires); err != nil {
			return CreatedSession{}, ErrSessionUnavailable
		}
		login, err := accounts.ParseLoginName(loginText)
		if err != nil {
			return CreatedSession{}, ErrSessionUnavailable
		}
		if err := clearLoginAttempts(ctx, tx, login); err != nil {
			return CreatedSession{}, err
		}
		if err := insertSessionAudit(ctx, tx, input.Account, authActionSessionCreate, authOutcomeAdmitted); err != nil {
			return CreatedSession{}, err
		}
		return CreatedSession{Session: SessionRecord{id: input.ID, account: input.Account, createdAt: input.Now, expiresAt: expires}, AccessExpiresAt: accessExpires, Evicted: evicted}, nil
	})
}

func (s *Store) RotateRefresh(ctx context.Context, input RefreshRotation) (RotatedSession, error) {
	account, err := sqlAccountID(input.Owner.Account)
	if err != nil {
		return RotatedSession{}, ErrSessionInvalid
	}
	session, err := input.Owner.Session.Value()
	oldDigest, oldErr := input.OldDigest.Bytes()
	newDigest, newErr := input.NewDigest.Bytes()
	newAccessDigest, accessErr := input.NewAccessDigest.Bytes()
	if err != nil || oldErr != nil || newErr != nil || accessErr != nil || input.Now.IsZero() {
		return RotatedSession{}, ErrSessionInvalid
	}
	return sessionTransaction(s, ctx, func(tx pgx.Tx) (RotatedSession, error) {
		var revision int64
		var status int16
		if err := tx.QueryRow(ctx, `SELECT revocation_revision,status FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&revision, &status); err != nil {
			return RotatedSession{}, ErrSessionNotFound
		}
		var recordedRevision int64
		var generation int32
		var expiry time.Time
		var revoked *time.Time
		if err := tx.QueryRow(ctx, `SELECT account_revision,current_generation,expires_at,revoked_at FROM public.auth_sessions
            WHERE id=$1 AND account_id=$2 FOR UPDATE`, session, account).Scan(&recordedRevision, &generation, &expiry, &revoked); err != nil {
			return RotatedSession{}, ErrSessionNotFound
		}
		if status != 1 || revision != recordedRevision || revoked != nil || !input.Now.Before(expiry) {
			return RotatedSession{}, ErrSessionNotFound
		}
		var foundGeneration int32
		var state int16
		if err := tx.QueryRow(ctx, `SELECT generation,state FROM public.auth_refresh_generations
			WHERE session_id=$1 AND token_digest=$2 FOR UPDATE`, session, oldDigest[:]).Scan(&foundGeneration, &state); err != nil {
			return RotatedSession{}, ErrSessionNotFound
		}
		if state == 2 {
			if _, err := tx.Exec(ctx, `UPDATE public.auth_sessions SET revoked_at=$2 WHERE id=$1`, session, input.Now); err != nil {
				return RotatedSession{}, ErrSessionUnavailable
			}
			if err := insertSessionAudit(ctx, tx, input.Owner.Account, authActionSessionReplay, authOutcomeDenied); err != nil {
				return RotatedSession{}, err
			}
			return RotatedSession{kind: refreshRotationReplayRevoked}, nil
		}
		if state != 1 || foundGeneration != generation {
			return RotatedSession{}, ErrSessionNotFound
		}
		if generation >= sessionGenerationLimit {
			if _, err := tx.Exec(ctx, `UPDATE public.auth_sessions SET revoked_at=$2 WHERE id=$1`, session, input.Now); err != nil {
				return RotatedSession{}, ErrSessionUnavailable
			}
			if err := insertSessionAudit(ctx, tx, input.Owner.Account, authActionSessionRevoke, authOutcomeAdmitted); err != nil {
				return RotatedSession{}, err
			}
			return RotatedSession{kind: refreshRotationGenerationExhausted}, nil
		}
		command, err := tx.Exec(ctx, `UPDATE public.auth_refresh_generations SET state=2,consumed_at=$3
            WHERE session_id=$1 AND generation=$2 AND state=1`, session, generation, input.Now)
		if err != nil || command.RowsAffected() != 1 {
			return RotatedSession{}, ErrSessionUnavailable
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_refresh_generations(session_id,generation,token_digest,state,issued_at)
			VALUES($1,$2,$3,1,$4)`, session, generation+1, newDigest[:], input.Now); err != nil {
			return RotatedSession{}, ErrSessionUnavailable
		}
		accessExpires := input.Now.Add(sessionAccessLifetime)
		if accessExpires.After(expiry) {
			accessExpires = expiry
		}
		command, err = tx.Exec(ctx, `UPDATE public.auth_access_credentials SET token_digest=$2,expires_at=$3
			WHERE session_id=$1`, session, newAccessDigest[:], accessExpires)
		if err != nil || command.RowsAffected() != 1 {
			return RotatedSession{}, ErrSessionUnavailable
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_sessions SET current_generation=$2,last_used_at=$3 WHERE id=$1`, session, generation+1, input.Now); err != nil {
			return RotatedSession{}, ErrSessionUnavailable
		}
		if err := insertSessionAudit(ctx, tx, input.Owner.Account, authActionSessionRefresh, authOutcomeAdmitted); err != nil {
			return RotatedSession{}, err
		}
		record, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM public.auth_sessions s WHERE s.id=$1`, session))
		return RotatedSession{kind: refreshRotationSucceeded, session: record, accessExpiresAt: accessExpires}, err
	})
}

func insertSessionAudit(ctx context.Context, tx pgx.Tx, account contracts.AccountID, action authAction, outcome int16) error {
	value, err := sqlAccountID(account)
	if err != nil {
		return ErrSessionInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.auth_audit(target_account_id,action,outcome) VALUES($1,$2,$3)`, value, int16(action), outcome)
	if err != nil {
		return ErrSessionUnavailable
	}
	return nil
}

func (s *Store) ListSessions(ctx context.Context, actor, target contracts.AccountID, now time.Time) ([]SessionRecord, error) {
	actorValue, err := sqlAccountID(actor)
	if err != nil {
		return nil, ErrSessionInvalid
	}
	targetValue, err := sqlAccountID(target)
	if err != nil || now.IsZero() {
		return nil, ErrSessionInvalid
	}
	return sessionTransaction(s, ctx, func(tx pgx.Tx) ([]SessionRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return nil, ErrSessionUnavailable
		}
		if actorValue != targetValue {
			if err := requireAdmin(ctx, tx, actor); err != nil {
				return nil, ErrSessionPermission
			}
		} else {
			var status int16
			if err := tx.QueryRow(ctx, `SELECT status FROM public.auth_accounts WHERE id=$1`, actorValue).Scan(&status); err != nil || status != 1 {
				return nil, ErrSessionPermission
			}
		}
		rows, err := tx.Query(ctx, `SELECT `+sessionColumns+` FROM public.auth_sessions s
            WHERE s.account_id=$1 AND s.revoked_at IS NULL AND s.expires_at>$2 ORDER BY s.created_at DESC,s.id DESC LIMIT $3`,
			targetValue, now, sessionLimitPerAccount+1)
		if err != nil {
			return nil, ErrSessionUnavailable
		}
		defer rows.Close()
		result := make([]SessionRecord, 0, sessionLimitPerAccount)
		for rows.Next() {
			record, err := scanSession(rows)
			if err != nil {
				return nil, err
			}
			result = append(result, record)
		}
		if rows.Err() != nil || len(result) > sessionLimitPerAccount {
			return nil, ErrSessionUnavailable
		}
		return result, nil
	})
}

// RevokeSession rechecks the target and current actor rights inside the same
// transaction. Its caller holds the target account's local coordinator lock.
func (s *Store) RevokeSession(ctx context.Context, actor contracts.AccountID, owner SessionOwner, now time.Time) error {
	actorValue, err := sqlAccountID(actor)
	if err != nil {
		return ErrSessionInvalid
	}
	targetValue, err := sqlAccountID(owner.Account)
	if err != nil || now.IsZero() {
		return ErrSessionInvalid
	}
	sessionID, err := owner.Session.Value()
	if err != nil {
		return ErrSessionInvalid
	}
	_, err = sessionTransaction(s, ctx, func(tx pgx.Tx) (struct{}, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return struct{}{}, ErrSessionUnavailable
		}
		if actorValue != targetValue {
			if err := requireAdmin(ctx, tx, actor); err != nil {
				return struct{}{}, ErrSessionPermission
			}
		}
		var status int16
		if err := tx.QueryRow(ctx, `SELECT status FROM public.auth_accounts WHERE id=$1 FOR UPDATE`, targetValue).Scan(&status); err != nil || status != 1 {
			return struct{}{}, ErrSessionNotFound
		}
		var revoked *time.Time
		if err := tx.QueryRow(ctx, `SELECT revoked_at FROM public.auth_sessions WHERE id=$1 AND account_id=$2 FOR UPDATE`, sessionID, targetValue).Scan(&revoked); err != nil {
			return struct{}{}, ErrSessionNotFound
		}
		if revoked != nil {
			return struct{}{}, ErrSessionNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_sessions SET revoked_at=$2 WHERE id=$1`, sessionID, now); err != nil {
			return struct{}{}, ErrSessionUnavailable
		}
		if err := insertSessionAudit(ctx, tx, owner.Account, authActionSessionRevoke, authOutcomeAdmitted); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, nil
	})
	return err
}
