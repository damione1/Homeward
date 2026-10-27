package calendars

import (
	"context"
	"errors"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/jackc/pgx/v5"
)

type networkEvidenceKey struct{}

// WithNetworkEvidence carries only an opaque050 admission into the existing
// transaction. Internal automation calls carry no user/session/evidence.
func WithNetworkEvidence(ctx context.Context, evidence authorization.RightsEvidence) (context.Context, error) {
	if err := contextValid(ctx); err != nil {
		return nil, err
	}
	if _, err := evidence.Account(); err != nil {
		return nil, authorization.ErrDenied
	}
	return context.WithValue(ctx, networkEvidenceKey{}, evidence), nil
}
func networkDenied(err error) bool { return errors.Is(err, authorization.ErrDenied) }
func fenceNetwork(ctx context.Context, tx pgx.Tx) error {
	evidence, present := ctx.Value(networkEvidenceKey{}).(authorization.RightsEvidence)
	if !present {
		return nil
	}
	account, err := evidence.Account()
	if err != nil {
		return authorization.ErrDenied
	}
	id, err := account.Value()
	if err != nil {
		return authorization.ErrDenied
	}
	var role, status int16
	var revocation, rights int64
	var semantic []byte
	err = tx.QueryRow(ctx, `SELECT role,status,revocation_revision FROM public.auth_accounts WHERE id=$1 FOR SHARE`, id).Scan(&role, &status, &revocation)
	if errors.Is(err, pgx.ErrNoRows) {
		return authorization.ErrDenied
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `SELECT rights_revision,semantic_sha256 FROM public.authorization_policy_active WHERE account_id=$1 FOR SHARE`, id).Scan(&rights, &semantic)
	if errors.Is(err, pgx.ErrNoRows) {
		return authorization.ErrDenied
	}
	if err != nil {
		return err
	}
	if !evidence.MatchesSQL(role, status, revocation, rights, semantic) {
		return authorization.ErrDenied
	}
	return nil
}
