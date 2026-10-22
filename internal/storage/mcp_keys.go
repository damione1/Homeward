package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/mcpkeys"
	"github.com/jackc/pgx/v5"
)

const mcpKeyLimitPerAccount = 128

var ErrMCPKeyNotFound = errors.New("mcp keys: credential unavailable")

type MCPKeyOwner struct {
	Account contracts.AccountID
	Key     contracts.McpKeyID
}
type MCPKeyRecord struct {
	Owner                 MCPKeyOwner
	Client                mcpkeys.ClientName
	Scopes                []authorization.Permission
	CreatedAt, ExpiresAt  time.Time
	RevokedAt, LastUsedAt *time.Time
	revision, rights      int64
}
type MCPKeyCreation struct {
	ID       contracts.McpKeyID
	Account  contracts.AccountID
	Client   mcpkeys.ClientName
	Digest   mcpkeys.Digest
	Scopes   []authorization.Permission
	Lifetime time.Duration
	Now      time.Time
}
type mcpStoredScope struct {
	Family    authv1.ScopeFamily    `json:"family"`
	Operation authv1.ScopeOperation `json:"operation"`
}

func mcpEncodeScopes(scopes []authorization.Permission) ([]byte, error) {
	if _, err := authorization.NewScopeSet(scopes); err != nil {
		return nil, mcpkeys.ErrInvalid
	}
	values := make([]mcpStoredScope, 0, len(scopes))
	for _, scope := range scopes {
		family, operation, err := scope.Values()
		if err != nil {
			return nil, err
		}
		values = append(values, mcpStoredScope{family, operation})
	}
	return json.Marshal(values)
}
func mcpDecodeScopes(data []byte) ([]authorization.Permission, error) {
	var stored []mcpStoredScope
	if json.Unmarshal(data, &stored) != nil || stored == nil || len(stored) > 39 {
		return nil, ErrSessionUnavailable
	}
	scopes := make([]authorization.Permission, 0, len(stored))
	for _, value := range stored {
		permission, err := authorization.NewPermission(value.Family, value.Operation)
		if err != nil {
			return nil, ErrSessionUnavailable
		}
		scopes = append(scopes, permission)
	}
	if _, err := authorization.NewScopeSet(scopes); err != nil {
		return nil, ErrSessionUnavailable
	}
	return scopes, nil
}

const mcpKeyColumns = `k.id,k.account_id,k.client_name,k.scopes,k.created_at,k.expires_at,k.revoked_at,k.last_used_at,k.account_revision,k.rights_revision`

func scanMCPKey(row pgx.Row) (MCPKeyRecord, error) {
	var r MCPKeyRecord
	var id, account, client string
	var scopes []byte
	if err := row.Scan(&id, &account, &client, &scopes, &r.CreatedAt, &r.ExpiresAt, &r.RevokedAt, &r.LastUsedAt, &r.revision, &r.rights); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r, ErrMCPKeyNotFound
		}
		return r, ErrSessionUnavailable
	}
	var err error
	r.Owner.Key, err = contracts.ParseMcpKeyID(id)
	if err != nil {
		return MCPKeyRecord{}, ErrSessionUnavailable
	}
	r.Owner.Account, err = contracts.ParseAccountID(account)
	if err != nil {
		return MCPKeyRecord{}, ErrSessionUnavailable
	}
	r.Client, err = mcpkeys.ParseClientName(client)
	if err != nil {
		return MCPKeyRecord{}, ErrSessionUnavailable
	}
	r.Scopes, err = mcpDecodeScopes(scopes)
	if err != nil || r.revision <= 0 || r.rights <= 0 || r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) {
		return MCPKeyRecord{}, ErrSessionUnavailable
	}
	return r, nil
}
func (s *Store) CreateMCPKey(ctx context.Context, input MCPKeyCreation) (MCPKeyRecord, error) {
	id, e := input.ID.Value()
	if e != nil || len(id) != 32 || input.Now.IsZero() || input.Lifetime <= 0 || input.Lifetime > 365*24*time.Hour {
		return MCPKeyRecord{}, mcpkeys.ErrInvalid
	}
	account, e := sqlAccountID(input.Account)
	if e != nil {
		return MCPKeyRecord{}, mcpkeys.ErrInvalid
	}
	client, e := input.Client.Value()
	if e != nil {
		return MCPKeyRecord{}, e
	}
	digest, e := input.Digest.Bytes()
	if e != nil {
		return MCPKeyRecord{}, e
	}
	scopes, e := mcpEncodeScopes(input.Scopes)
	if e != nil {
		return MCPKeyRecord{}, e
	}
	// PostgreSQL timestamps have microsecond precision. Round the lifetime up
	// within the admitted ceiling so every positive duration remains representable.
	created := input.Now.UTC().Truncate(time.Microsecond)
	lifetime := input.Lifetime
	if remainder := lifetime % time.Microsecond; remainder != 0 {
		lifetime += time.Microsecond - remainder
	}
	expires := created.Add(lifetime)
	return sessionTransaction(s, ctx, func(tx pgx.Tx) (MCPKeyRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return MCPKeyRecord{}, err
		}
		snapshot, err := scanAuthorizationPolicy(tx.QueryRow(ctx, `SELECT `+authorizationPolicyColumns+authorizationPolicyJoin+` WHERE a.id=$1`, account))
		if err != nil {
			return MCPKeyRecord{}, err
		}
		state, err := snapshot.State()
		if err != nil {
			return MCPKeyRecord{}, err
		}
		if err := state.AdmitCredentialScopes(input.Scopes); err != nil {
			return MCPKeyRecord{}, err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM public.auth_mcp_keys WHERE account_id=$1 AND revoked_at IS NULL AND expires_at>$2`, account, input.Now).Scan(&count); err != nil {
			return MCPKeyRecord{}, err
		}
		if count >= mcpKeyLimitPerAccount {
			return MCPKeyRecord{}, authorization.ErrCapacity
		}
		// Revision binding freezes every right at issuance: later promotions or
		// object-policy expansions never silently grant this credential more power.
		_, err = tx.Exec(ctx, `INSERT INTO public.auth_mcp_keys(id,account_id,client_name,token_digest,account_revision,rights_revision,scopes,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, account, client, digest[:], snapshot.revocation, int64(snapshot.rightsRevision), scopes, created, expires)
		if err != nil {
			return MCPKeyRecord{}, err
		}
		return scanMCPKey(tx.QueryRow(ctx, `SELECT `+mcpKeyColumns+` FROM public.auth_mcp_keys k WHERE k.id=$1`, id))
	})
}
func (s *Store) LookupMCPKeyOwner(ctx context.Context, digest mcpkeys.Digest) (MCPKeyOwner, error) {
	raw, e := digest.Bytes()
	if e != nil || accountContextError(ctx) != nil || s == nil || s.pool == nil {
		return MCPKeyOwner{}, mcpkeys.ErrInvalid
	}
	var account, id string
	if err := s.pool.QueryRow(ctx, `SELECT account_id,id FROM public.auth_mcp_keys WHERE token_digest=$1`, raw[:]).Scan(&account, &id); err != nil {
		return MCPKeyOwner{}, ErrMCPKeyNotFound
	}
	a, e := contracts.ParseAccountID(account)
	if e != nil {
		return MCPKeyOwner{}, ErrSessionUnavailable
	}
	k, e := contracts.ParseMcpKeyID(id)
	if e != nil {
		return MCPKeyOwner{}, ErrSessionUnavailable
	}
	return MCPKeyOwner{a, k}, nil
}
func (s *Store) MCPKeyOwner(ctx context.Context, id contracts.McpKeyID) (MCPKeyOwner, error) {
	key, e := id.Value()
	if e != nil || accountContextError(ctx) != nil || s == nil || s.pool == nil {
		return MCPKeyOwner{}, mcpkeys.ErrInvalid
	}
	var account string
	if err := s.pool.QueryRow(ctx, `SELECT account_id FROM public.auth_mcp_keys WHERE id=$1`, key).Scan(&account); err != nil {
		return MCPKeyOwner{}, ErrMCPKeyNotFound
	}
	a, e := contracts.ParseAccountID(account)
	if e != nil {
		return MCPKeyOwner{}, ErrSessionUnavailable
	}
	return MCPKeyOwner{a, id}, nil
}
func (s *Store) AdmitMCPKey(ctx context.Context, owner MCPKeyOwner, digest mcpkeys.Digest, now time.Time) (MCPKeyRecord, error) {
	id, e := owner.Key.Value()
	if e != nil || now.IsZero() {
		return MCPKeyRecord{}, mcpkeys.ErrInvalid
	}
	account, e := sqlAccountID(owner.Account)
	if e != nil {
		return MCPKeyRecord{}, mcpkeys.ErrInvalid
	}
	raw, e := digest.Bytes()
	if e != nil {
		return MCPKeyRecord{}, e
	}
	return sessionTransaction(s, ctx, func(tx pgx.Tx) (MCPKeyRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return MCPKeyRecord{}, err
		}
		record, err := scanMCPKey(tx.QueryRow(ctx, `SELECT `+mcpKeyColumns+` FROM public.auth_mcp_keys k JOIN public.auth_accounts a ON a.id=k.account_id JOIN public.authorization_policy_active p ON p.account_id=a.id WHERE k.id=$1 AND k.account_id=$2 AND k.token_digest=$3 AND k.revoked_at IS NULL AND k.expires_at>$4 AND k.created_at<=$4 AND a.status=$5 AND a.revocation_revision=k.account_revision AND p.rights_revision=k.rights_revision FOR UPDATE OF k`, id, account, raw[:], now, sqlStatusActive))
		if err != nil {
			return MCPKeyRecord{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE public.auth_mcp_keys SET last_used_at=$2 WHERE id=$1`, id, now); err != nil {
			return MCPKeyRecord{}, err
		}
		record.LastUsedAt = &now
		return record, nil
	})
}
func requireMCPKeyOwner(ctx context.Context, tx pgx.Tx, actor, target contracts.AccountID) error {
	current, err := accountByID(ctx, tx, actor)
	if err != nil {
		return ErrSessionPermission
	}
	if current.status != authv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return ErrSessionPermission
	}
	if actor != target && current.role != authv1.AccountRole_ACCOUNT_ROLE_ADMIN {
		return ErrSessionPermission
	}
	return nil
}
func (s *Store) ListMCPKeys(ctx context.Context, actor, target contracts.AccountID) ([]MCPKeyRecord, error) {
	account, e := sqlAccountID(target)
	if e != nil {
		return nil, mcpkeys.ErrInvalid
	}
	return sessionTransaction(s, ctx, func(tx pgx.Tx) ([]MCPKeyRecord, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return nil, err
		}
		if err := requireMCPKeyOwner(ctx, tx, actor, target); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `SELECT `+mcpKeyColumns+` FROM public.auth_mcp_keys k WHERE k.account_id=$1 ORDER BY k.created_at,k.id LIMIT 10001`, account)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		result := make([]MCPKeyRecord, 0)
		for rows.Next() {
			r, err := scanMCPKey(rows)
			if err != nil {
				return nil, err
			}
			result = append(result, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(result) > 10000 {
			return nil, authorization.ErrCapacity
		}
		return result, nil
	})
}
func (s *Store) RevokeMCPKey(ctx context.Context, actor contracts.AccountID, owner MCPKeyOwner, now time.Time) error {
	id, e := owner.Key.Value()
	if e != nil || now.IsZero() {
		return mcpkeys.ErrInvalid
	}
	account, e := sqlAccountID(owner.Account)
	if e != nil {
		return mcpkeys.ErrInvalid
	}
	_, err := sessionTransaction(s, ctx, func(tx pgx.Tx) (struct{}, error) {
		if _, err := lockAuthSingleton(ctx, tx); err != nil {
			return struct{}{}, err
		}
		if err := requireMCPKeyOwner(ctx, tx, actor, owner.Account); err != nil {
			return struct{}{}, err
		}
		command, err := tx.Exec(ctx, `UPDATE public.auth_mcp_keys SET revoked_at=COALESCE(revoked_at,$3) WHERE id=$1 AND account_id=$2`, id, account, now)
		if err != nil {
			return struct{}{}, err
		}
		if command.RowsAffected() != 1 {
			return struct{}{}, ErrMCPKeyNotFound
		}
		return struct{}{}, nil
	})
	return err
}
