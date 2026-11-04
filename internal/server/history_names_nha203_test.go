package server

import (
	"context"

	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

// Explicit fixture owner only: production resolves through registry.Repository.
// It never attempts to reverse a public hash or accepts a native caller string.
type nha203HistoryNames struct {
	home     apitypes.HomeName
	entities []domain.EntityID
}

func (n nha203HistoryNames) HomeName(ctx context.Context) (apitypes.HomeName, error) {
	if err := ctx.Err(); err != nil {
		return apitypes.HomeName{}, err
	}
	if !n.home.Valid() {
		return apitypes.HomeName{}, authorization.ErrUnavailable
	}
	return n.home, nil
}
func (n nha203HistoryNames) ResolveEntityName(ctx context.Context, name apitypes.EntityName) (domain.EntityID, error) {
	if err := ctx.Err(); err != nil {
		return domain.EntityID{}, err
	}
	for _, entity := range n.entities {
		expected, err := apitypes.EntityNameForIdentity(n.home, entity)
		if err == nil && expected == name {
			return entity, nil
		}
	}
	return domain.EntityID{}, authorization.ErrDenied
}
