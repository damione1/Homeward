package server

import (
	"context"
	"encoding/hex"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/view"
)

type coreRPCHandler struct {
	home     *homeResourceHandler
	media    mediaCapabilityProjector
	views    *view.Service
	grants   view.GrantChangeNotifier
	commands *integrations.DurableCommandService
	epoch    integrations.CommandEpochAuthority
}

func coreRPCError(ctx context.Context, err error) error {
	return apiRPCError(ctx, err)
}

func coreRPCActor(ctx context.Context) (domain.ActorID, error) {
	if nilReactiveInterface(ctx) {
		return domain.ActorID{}, coreRPCError(ctx, integrations.ErrCommandAuthorizationDenied)
	}
	actor, present := ActorFromContext(ctx)
	if !present || nilReactiveInterface(ctx) || ctx.Err() != nil {
		return domain.ActorID{}, coreRPCError(ctx, integrations.ErrCommandAuthorizationDenied)
	}
	if _, err := actor.Value(); err != nil {
		return domain.ActorID{}, coreRPCError(ctx, integrations.ErrCommandAuthorizationDenied)
	}
	return actor, nil
}

func corePageToken(text string) bool {
	if len(text) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return false
	}
	for _, value := range decoded {
		if value != 0 {
			return true
		}
	}
	return false
}
