package storage

import (
	"context"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authmfa"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"github.com/jackc/pgx/v5"
)

// Individual metadata reads use the collection owner's current SQL admission,
// but select one indexed native identity. Credential secret columns are absent.
func (s *Store) GetInvitation(ctx context.Context, actor contracts.AccountID, id contracts.InvitationID) (InvitationRecord, error) {
	key, err := sqlInvitationID(id)
	if err != nil {
		return InvitationRecord{}, err
	}
	return accountTransaction(s, ctx, accountAudit{actor: &actor, invitation: &id, action: authActionListInvitations}, func(tx pgx.Tx) (InvitationRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return InvitationRecord{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return InvitationRecord{}, err
		}
		value, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM public.auth_invitations WHERE id=$1`, key))
		if errors.Is(err, pgx.ErrNoRows) {
			return InvitationRecord{}, ErrInvitationNotFound
		}
		return value, err
	})
}
func (s *Store) GetSession(ctx context.Context, actor contracts.AccountID, owner SessionOwner, now time.Time) (SessionRecord, error) {
	actorKey, err := sqlAccountID(actor)
	if err != nil {
		return SessionRecord{}, ErrSessionInvalid
	}
	target, err := sqlAccountID(owner.Account)
	id, se := owner.Session.Value()
	if err != nil || se != nil || now.IsZero() {
		return SessionRecord{}, ErrSessionInvalid
	}
	return sessionTransaction(s, ctx, func(tx pgx.Tx) (SessionRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return SessionRecord{}, ErrSessionUnavailable
		}
		if actorKey != target {
			if err := requireAdmin(ctx, tx, actor); err != nil {
				return SessionRecord{}, ErrSessionPermission
			}
		} else {
			var status int16
			if err := tx.QueryRow(ctx, `SELECT status FROM public.auth_accounts WHERE id=$1`, actorKey).Scan(&status); err != nil || status != sqlStatusActive {
				return SessionRecord{}, ErrSessionPermission
			}
		}
		return scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM public.auth_sessions s WHERE s.id=$1 AND s.account_id=$2 AND s.revoked_at IS NULL AND s.expires_at>$3`, id, target, now))
	})
}
func (s *Store) GetMCPKey(ctx context.Context, actor contracts.AccountID, owner MCPKeyOwner) (MCPKeyRecord, error) {
	account, err := sqlAccountID(owner.Account)
	id, ie := owner.Key.Value()
	if err != nil || ie != nil {
		return MCPKeyRecord{}, mcpkeys.ErrInvalid
	}
	return sessionTransaction(s, ctx, func(tx pgx.Tx) (MCPKeyRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return MCPKeyRecord{}, err
		}
		if err := requireMCPKeyOwner(ctx, tx, actor, owner.Account); err != nil {
			return MCPKeyRecord{}, err
		}
		return scanMCPKey(tx.QueryRow(ctx, `SELECT `+mcpKeyColumns+` FROM public.auth_mcp_keys k WHERE k.id=$1 AND k.account_id=$2`, id, account))
	})
}
func (r *MFARepository) GetFactor(ctx context.Context, owner SessionOwner, factor contracts.FactorCredentialID, now time.Time) (MFAFactorMetadata, bool, error) {
	id, err := factor.Value()
	if err != nil {
		return MFAFactorMetadata{}, false, mfaError(authmfa.ErrorInvalidInput)
	}
	type result struct {
		value MFAFactorMetadata
		found bool
	}
	read, err := mfaTransaction(r, ctx, func(tx pgx.Tx) (result, error) {
		if err := requireCurrentMFASessionTx(ctx, tx, owner, now); err != nil {
			return result{}, err
		}
		account, _ := owner.Account.Value()
		value := MFAFactorMetadata{account: owner.Account, factor: factor}
		err := tx.QueryRow(ctx, `SELECT created_at,last_used_at FROM public.auth_mfa_credentials WHERE account_id=$1 AND id=$2 AND removed_at IS NULL`, account, id).Scan(&value.created, &value.used)
		if errors.Is(err, pgx.ErrNoRows) {
			return result{}, nil
		}
		if err != nil {
			return result{}, mfaError(authmfa.ErrorUnavailable)
		}
		if _, _, _, _, err := value.Values(); err != nil {
			return result{}, err
		}
		return result{value, true}, nil
	})
	return read.value, read.found, err
}
