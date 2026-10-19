package storage

import (
	"context"
	"time"
)

// currentSessionPredicate is the shared 141 authority for both a bearer
// admission and an explicitly delegated durable job's true session reference.
// The aliases s/c refer to auth_sessions/auth_accounts in both queries.
const currentSessionPredicate = `s.revoked_at IS NULL AND c.status=1 AND c.revocation_revision=s.account_revision`

// AdmitSessionGrant admits only a real, still-current session reference. The
// caller holds the native account coordinator gate, then refreshes the current
// authorization policy and registers a revocable lease before releasing it.
// This port creates neither a session nor a bearer/automation credential.
func (s *Store) AdmitSessionGrant(ctx context.Context, owner SessionOwner, now time.Time) (SessionRecord, error) {
	account, err := sqlAccountID(owner.Account)
	if err != nil {
		return SessionRecord{}, ErrSessionInvalid
	}
	session, err := owner.Session.Value()
	if err != nil || now.IsZero() || s == nil || s.pool == nil || accountContextError(ctx) != nil {
		return SessionRecord{}, ErrSessionInvalid
	}
	record, err := scanSession(s.pool.QueryRow(ctx, `SELECT `+sessionColumns+`
        FROM public.auth_sessions s JOIN public.auth_accounts c ON c.id=s.account_id
        WHERE s.id=$1 AND s.account_id=$2 AND `+currentSessionPredicate, session, account))
	if err != nil {
		return SessionRecord{}, err
	}
	if !now.Before(record.expiresAt) {
		return SessionRecord{}, ErrSessionNotFound
	}
	if record.account != owner.Account || record.id != owner.Session || record.revokedAt != nil || record.expiresAt.IsZero() || record.createdAt.IsZero() {
		return SessionRecord{}, ErrSessionUnavailable
	}
	return record, nil
}
