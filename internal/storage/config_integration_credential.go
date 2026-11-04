package storage

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/internal/authorization"
	"github.com/jackc/pgx/v5"
)

// Configuration writers already hold the native security prefix before this
// fence. MCP revocation/authentication use that same prefix and account/policy
// ordering. No authority callback occurs while the credential row is locked.
func (e configurationRequestEvidence) fenceSQL(ctx context.Context, tx pgx.Tx, method ConfigurationAPIMethod) error {
	rights, err := e.rights(method)
	if err != nil {
		return authorization.ErrDenied
	}
	if err := authorization.FenceSQL(ctx, tx, rights); err != nil {
		return err
	}
	if e.kind == configurationRequestGlobal {
		return nil
	}
	switch e.integration.CredentialKind() {
	case authorization.IntegrationNativeSession:
		session, err := e.integration.Session()
		if err != nil {
			return authorization.ErrDenied
		}
		id, err := session.Value()
		if err != nil {
			return authorization.ErrDenied
		}
		var account string
		var revision int64
		err = tx.QueryRow(ctx, `SELECT s.account_id,s.account_revision FROM public.auth_sessions s JOIN public.auth_accounts c ON c.id=s.account_id CROSS JOIN public.auth_mfa_policy p
 WHERE p.singleton AND s.id=$1 AND s.created_at<=statement_timestamp() AND s.expires_at>statement_timestamp() AND `+mfaSessionPredicate+` FOR SHARE OF s`, id).Scan(&account, &revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return authorization.ErrDenied
		}
		if err != nil {
			return err
		}
		if !e.integration.MatchesNativeSession(account, revision) {
			return authorization.ErrDenied
		}
		return nil
	case authorization.IntegrationMCPKey:
		key, err := e.integration.MCPKey()
		if err != nil {
			return authorization.ErrDenied
		}
		id, err := key.Value()
		if err != nil {
			return authorization.ErrDenied
		}
		var account string
		var revision, rightsRevision int64
		var scopesJSON []byte
		err = tx.QueryRow(ctx, `SELECT account_id,account_revision,rights_revision,scopes FROM public.auth_mcp_keys
   WHERE id=$1 AND revoked_at IS NULL AND created_at<=statement_timestamp() AND expires_at>statement_timestamp() FOR SHARE`, id).Scan(&account, &revision, &rightsRevision, &scopesJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return authorization.ErrDenied
		}
		if err != nil {
			return err
		}
		scopes, err := mcpDecodeScopes(scopesJSON)
		if err != nil || !e.integration.MatchesMCPKey(account, revision, rightsRevision, scopes) {
			return authorization.ErrDenied
		}
		return nil
	default:
		return authorization.ErrDenied
	}
}
