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

const accountListLimit = 10000

func clearExpiredInvitationClaims(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `DELETE FROM public.auth_login_claims c USING public.auth_invitations i
        WHERE c.owner_kind=2 AND c.owner_id=i.id AND
        (i.expires_at <= now() OR i.revoked_at IS NOT NULL)`)
	return err
}

func (s *Store) CreateAccount(ctx context.Context, actor contracts.AccountID, input NewAccount) (AccountRecord, error) {
	if _, err := sqlAccountID(actor); err != nil {
		return AccountRecord{}, ErrAccountPermission
	}
	return accountTransaction(s, ctx, accountAudit{actor: &actor, target: &input.ID, action: authActionCreateAccount}, func(tx pgx.Tx) (AccountRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return AccountRecord{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return AccountRecord{}, err
		}
		if err := clearExpiredInvitationClaims(ctx, tx); err != nil {
			return AccountRecord{}, err
		}
		if err := insertAccount(ctx, tx, input); err != nil {
			return AccountRecord{}, err
		}
		return accountByID(ctx, tx, input.ID)
	})
}

func (s *Store) GetAccount(ctx context.Context, actor, target contracts.AccountID) (AccountRecord, error) {
	if _, err := sqlAccountID(target); err != nil {
		return AccountRecord{}, err
	}
	return accountTransaction(s, ctx, accountAudit{actor: &actor, target: &target, action: authActionGetAccount}, func(tx pgx.Tx) (AccountRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return AccountRecord{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return AccountRecord{}, err
		}
		return accountByID(ctx, tx, target)
	})
}

func (s *Store) ListAccounts(ctx context.Context, actor contracts.AccountID) ([]AccountRecord, error) {
	return accountTransaction(s, ctx, accountAudit{actor: &actor, action: authActionListAccounts}, func(tx pgx.Tx) ([]AccountRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return nil, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `SELECT `+accountColumns+` FROM public.auth_accounts ORDER BY created_at,id LIMIT $1`, accountListLimit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := make([]AccountRecord, 0)
		for rows.Next() {
			record, err := scanAccount(rows)
			if err != nil {
				return nil, err
			}
			result = append(result, record)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(result) > accountListLimit {
			return nil, ErrAccountListTooLarge
		}
		return result, nil
	})
}

func (s *Store) UpdateAccount(ctx context.Context, actor, target contracts.AccountID, changes AccountChanges) (AccountRecord, error) {
	targetValue, err := sqlAccountID(target)
	if err != nil {
		return AccountRecord{}, err
	}
	if changes.Display == nil && changes.Role == nil && changes.Status == nil {
		return AccountRecord{}, ErrAccountInvalid
	}
	return accountTransaction(s, ctx, accountAudit{actor: &actor, target: &target, action: authActionUpdateAccount}, func(tx pgx.Tx) (AccountRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return AccountRecord{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return AccountRecord{}, err
		}
		current, err := accountByID(ctx, tx, target)
		if err != nil {
			return AccountRecord{}, err
		}
		display, err := current.display.Value()
		if err != nil {
			return AccountRecord{}, err
		}
		if changes.Display != nil {
			display, err = changes.Display.Value()
			if err != nil {
				return AccountRecord{}, ErrAccountInvalid
			}
		}
		role := current.role
		if changes.Role != nil {
			role = *changes.Role
		}
		roleSQL, err := sqlRole(role)
		if err != nil {
			return AccountRecord{}, err
		}
		status := current.status
		if changes.Status != nil {
			status = *changes.Status
		}
		statusSQL, err := sqlStatus(status)
		if err != nil {
			return AccountRecord{}, err
		}
		if current.role == authv1.AccountRole_ACCOUNT_ROLE_ADMIN && current.status == authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE &&
			(role != current.role || status != current.status) {
			count, err := activeAdminCount(ctx, tx)
			if err != nil {
				return AccountRecord{}, err
			}
			if count <= 1 {
				return AccountRecord{}, ErrLastActiveAdmin
			}
		}
		// Display-only changes leave future sessions valid; only a real role or
		// status change invalidates the authorization revision.
		rightsChanged := role != current.role || status != current.status
		_, err = tx.Exec(ctx, `UPDATE public.auth_accounts SET display_name=$2,role=$3,status=$4,
            revocation_revision=revocation_revision+CASE WHEN $5 THEN 1 ELSE 0 END,updated_at=now() WHERE id=$1`,
			targetValue, display, roleSQL, statusSQL, rightsChanged)
		if err != nil {
			return AccountRecord{}, err
		}
		return accountByID(ctx, tx, target)
	})
}

func (s *Store) DeleteAccount(ctx context.Context, actor, target contracts.AccountID) error {
	targetValue, err := sqlAccountID(target)
	if err != nil {
		return err
	}
	_, err = accountTransaction(s, ctx, accountAudit{actor: &actor, target: &target, action: authActionDeleteAccount}, func(tx pgx.Tx) (struct{}, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return struct{}{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return struct{}{}, err
		}
		current, err := accountByID(ctx, tx, target)
		if err != nil {
			return struct{}{}, err
		}
		if current.role == authv1.AccountRole_ACCOUNT_ROLE_ADMIN && current.status == authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
			count, err := activeAdminCount(ctx, tx)
			if err != nil {
				return struct{}{}, err
			}
			if count <= 1 {
				return struct{}{}, ErrLastActiveAdmin
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM public.auth_login_claims WHERE owner_kind=1 AND owner_id=$1`, targetValue); err != nil {
			return struct{}{}, err
		}
		_, err = tx.Exec(ctx, `DELETE FROM public.auth_accounts WHERE id=$1`, targetValue)
		return struct{}{}, err
	})
	return err
}

func (s *Store) CreateInvitation(ctx context.Context, actor contracts.AccountID, input NewInvitation) (InvitationRecord, error) {
	id, err := sqlInvitationID(input.ID)
	if err != nil {
		return InvitationRecord{}, err
	}
	loginName, key, err := input.Login.Values()
	if err != nil {
		return InvitationRecord{}, ErrAccountInvalid
	}
	role, err := sqlRole(input.Role)
	if err != nil || input.ExpiresAt.IsZero() || input.Digest == [32]byte{} {
		return InvitationRecord{}, ErrAccountInvalid
	}
	return accountTransaction(s, ctx, accountAudit{actor: &actor, invitation: &input.ID, action: authActionCreateInvitation}, func(tx pgx.Tx) (InvitationRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return InvitationRecord{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return InvitationRecord{}, err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return InvitationRecord{}, err
		}
		if !input.ExpiresAt.After(now) {
			return InvitationRecord{}, ErrInvitationExpired
		}
		if err := clearExpiredInvitationClaims(ctx, tx); err != nil {
			return InvitationRecord{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_login_claims(login_key,owner_kind,owner_id) VALUES($1,2,$2)`, key, id); err != nil {
			return InvitationRecord{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.auth_invitations(id,login_name,login_key,role,token_digest,expires_at)
            VALUES($1,$2,$3,$4,$5,$6)`, id, loginName, key, role, input.Digest[:], input.ExpiresAt); err != nil {
			return InvitationRecord{}, err
		}
		return scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM public.auth_invitations WHERE id=$1`, id))
	})
}

func (s *Store) AcceptInvitation(ctx context.Context, digest [32]byte, accountID contracts.AccountID,
	display accounts.DisplayName, verifier accounts.Verifier) (AccountRecord, error) {
	if _, err := sqlAccountID(accountID); err != nil {
		return AccountRecord{}, err
	}
	return accountTransaction(s, ctx, accountAudit{target: &accountID, action: authActionAcceptInvitation}, func(tx pgx.Tx) (AccountRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return AccountRecord{}, err
		}
		invitation, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM public.auth_invitations
            WHERE token_digest=$1 FOR UPDATE`, digest[:]))
		if errors.Is(err, pgx.ErrNoRows) {
			return AccountRecord{}, ErrInvitationNotFound
		}
		if err != nil {
			return AccountRecord{}, err
		}
		if invitation.acceptedAt != nil || invitation.revokedAt != nil {
			return AccountRecord{}, ErrInvitationUnavailable
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return AccountRecord{}, err
		}
		if !invitation.expiresAt.After(now) {
			return AccountRecord{}, ErrInvitationExpired
		}
		account := NewAccount{ID: accountID, Login: invitation.login, Display: display, Role: invitation.role, Verifier: verifier}
		id, err := sqlAccountID(accountID)
		if err != nil {
			return AccountRecord{}, err
		}
		_, key, err := invitation.login.Values()
		if err != nil {
			return AccountRecord{}, ErrAccountUnavailable
		}
		command, err := tx.Exec(ctx, `UPDATE public.auth_login_claims SET owner_kind=1,owner_id=$2
			WHERE login_key=$1 AND owner_kind=2 AND owner_id=$3`, key, id, mustInvitationID(invitation.id))
		if err != nil {
			return AccountRecord{}, err
		}
		if command.RowsAffected() != 1 {
			return AccountRecord{}, ErrInvitationUnavailable
		}
		if err := insertAccountRow(ctx, tx, account); err != nil {
			return AccountRecord{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_invitations SET accepted_at=now() WHERE id=$1`, mustInvitationID(invitation.id)); err != nil {
			return AccountRecord{}, err
		}
		return accountByID(ctx, tx, accountID)
	})
}

func mustInvitationID(id contracts.InvitationID) string {
	value, _ := id.Value()
	return value // only used after scanInvitation has admitted the stored ID
}

func (s *Store) ListInvitations(ctx context.Context, actor contracts.AccountID) ([]InvitationRecord, error) {
	return accountTransaction(s, ctx, accountAudit{actor: &actor, action: authActionListInvitations}, func(tx pgx.Tx) ([]InvitationRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return nil, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `SELECT `+invitationColumns+` FROM public.auth_invitations ORDER BY created_at,id LIMIT $1`, accountListLimit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := make([]InvitationRecord, 0)
		for rows.Next() {
			invitation, err := scanInvitation(rows)
			if err != nil {
				return nil, err
			}
			result = append(result, invitation)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(result) > accountListLimit {
			return nil, ErrAccountListTooLarge
		}
		return result, nil
	})
}

func (s *Store) RevokeInvitation(ctx context.Context, actor contracts.AccountID, id contracts.InvitationID) error {
	value, err := sqlInvitationID(id)
	if err != nil {
		return err
	}
	_, err = accountTransaction(s, ctx, accountAudit{actor: &actor, invitation: &id, action: authActionRevokeInvitation}, func(tx pgx.Tx) (struct{}, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return struct{}{}, err
		}
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return struct{}{}, err
		}
		invitation, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM public.auth_invitations WHERE id=$1 FOR UPDATE`, value))
		if errors.Is(err, pgx.ErrNoRows) {
			return struct{}{}, ErrInvitationNotFound
		}
		if err != nil {
			return struct{}{}, err
		}
		if invitation.acceptedAt != nil || invitation.revokedAt != nil {
			return struct{}{}, ErrInvitationUnavailable
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_invitations SET revoked_at=now() WHERE id=$1`, value); err != nil {
			return struct{}{}, err
		}
		_, err = tx.Exec(ctx, `DELETE FROM public.auth_login_claims WHERE owner_kind=2 AND owner_id=$1`, value)
		return struct{}{}, err
	})
	return err
}
