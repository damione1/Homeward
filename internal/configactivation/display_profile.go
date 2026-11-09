package configactivation

import (
	"context"

	"github.com/damione1/Homeward/internal/view"
)

// DisplayTimeZone projects the active house preference through the existing
// profile port. All actors share this configured preference; it grants no
// authority and is read only after the caller's normal admission.
func (o *Owner) DisplayTimeZone(ctx context.Context, actor view.ActorID) (string, error) {
	if o == nil || absentPort(ctx) {
		return "", ErrInvalidStartup
	}
	if _, err := actor.Value(); err != nil {
		return "", ErrInvalidStartup
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.phase != ownerOpen {
		return "", ErrActivationUnavailable
	}
	return o.current.operational.DisplayTimeZone(), nil
}

// The generation owner already invalidates on commit, rollback, suspension and
// closure. Reuse its bounded subscriptions rather than a second profile cache.
func (o *Owner) SubscribeDisplayProfileChanges(ctx context.Context, actor view.ActorID) (<-chan struct{}, func(), error) {
	if _, err := actor.Value(); err != nil {
		return nil, nil, ErrInvalidStartup
	}
	return o.SubscribeConfigChanges(ctx)
}

var _ view.DisplayProfileSource = (*Owner)(nil)
