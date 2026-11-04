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
	return authorization.FenceSQL(ctx, tx, evidence)
}
