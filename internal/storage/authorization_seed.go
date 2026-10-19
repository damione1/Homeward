package storage

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/jackc/pgx/v5"
)

func seedAuthorizationPolicy(config *ConfigTx, account contracts.AccountID) error {
	if err := config.valid(); err != nil {
		return err
	}
	text, err := sqlAccountID(account)
	if err != nil {
		return err
	}
	var exists bool
	if err := config.tx.QueryRow(config.ctx, `SELECT EXISTS(SELECT 1 FROM public.authorization_policy_active WHERE account_id=$1)`, text).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	source, err := authorization.EmptySource(account)
	if err != nil {
		return err
	}
	author, err := NewConfigAuthor("homeward-account-bootstrap")
	if err != nil {
		return err
	}
	origin, err := NewConfigSource("account-initialization")
	if err != nil {
		return err
	}
	metadata, err := NewConfigVersionMetadata(author, origin)
	if err != nil {
		return err
	}
	ref, err := config.SaveAuthorizationPolicy(source, metadata)
	if err != nil {
		return err
	}
	_, err = config.ActivateAuthorizationPolicy(account, authorization.ExpectAbsent(), ref)
	return err
}

// EnsureAuthorizationPolicies seeds existing accounts after migration, before
// admissions open. New accounts seed in their own creation transaction. The
// batches are bounded; each seed takes 151 -> auth_bootstrap in one pgx.Tx.
func (s *Store) EnsureAuthorizationPolicies(ctx context.Context) error {
	if accountContextError(ctx) != nil || s == nil || s.pool == nil {
		return ErrAccountInvalid
	}
	for {
		rows, err := s.pool.Query(ctx, `SELECT a.id FROM public.auth_accounts a
			LEFT JOIN public.authorization_policy_active p ON p.account_id=a.id
			WHERE p.account_id IS NULL ORDER BY a.id COLLATE "C" LIMIT 16`)
		if err != nil {
			return ErrAccountUnavailable
		}
		var ids []contracts.AccountID
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				rows.Close()
				return ErrAccountUnavailable
			}
			id, err := contracts.ParseAccountID(text)
			if err != nil {
				rows.Close()
				return ErrAccountUnavailable
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ErrAccountUnavailable
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			_, err := accountConfigTransaction(s, ctx, accountAudit{target: &id, action: authActionPolicySeed}, func(tx pgx.Tx, config *ConfigTx) (struct{}, error) {
				if _, err := lockAuthSingleton(ctx, tx); err != nil {
					return struct{}{}, err
				}
				if _, err := accountByID(ctx, tx, id); errors.Is(err, ErrAccountNotFound) {
					return struct{}{}, nil
				} else if err != nil {
					return struct{}{}, err
				}
				return struct{}{}, seedAuthorizationPolicy(config, id)
			})
			if err != nil {
				return err
			}
		}
	}
}

// ForEachAuthorizationPolicy yields one repeatable-read snapshot in batches.
// The caller builds its bounded authority privately and publishes only after
// this transaction returns successfully. No Host lock may surround this call.
func (s *Store) ForEachAuthorizationPolicy(ctx context.Context, yield func(authorization.AccountState) error) error {
	if accountContextError(ctx) != nil || s == nil || s.pool == nil || yield == nil {
		return ErrAccountInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrAccountUnavailable
	}
	defer rollback(tx)
	last := ""
	count := 0
	for {
		rows, err := tx.Query(ctx, `SELECT id FROM public.auth_accounts WHERE id COLLATE "C">$1 COLLATE "C" ORDER BY id COLLATE "C" LIMIT 16`, last)
		if err != nil {
			return ErrAccountUnavailable
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return ErrAccountUnavailable
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ErrAccountUnavailable
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			count++
			if count > accountListLimit {
				return ErrAccountListTooLarge
			}
			snapshot, err := scanAuthorizationPolicy(tx.QueryRow(ctx, `SELECT `+authorizationPolicyColumns+authorizationPolicyJoin+` WHERE a.id=$1`, id))
			if err != nil {
				return err
			}
			state, err := snapshot.State()
			if err != nil {
				return err
			}
			if err := yield(state); err != nil {
				return err
			}
			last = id
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrAccountUnavailable
	}
	return nil
}
