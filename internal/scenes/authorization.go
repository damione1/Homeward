package scenes

import (
	"context"
	"errors"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/sequencer"
)

func (s *Service) withSceneRead(ctx context.Context, fn func(*integrations.SceneReadAccess) error) error {
	if port, ok := s.commands.(interface {
		WithSceneRead(context.Context, func(integrations.SceneReadAccess) error) error
	}); ok {
		return port.WithSceneRead(ctx, func(access integrations.SceneReadAccess) error { return fn(&access) })
	}
	return fn(nil) // explicitly injected legacy command port; never a native Host bypass
}
func snapshotEntity(snapshot sequencer.Snapshot, alias domain.HAAlias) (domain.EntityID, error) {
	var found domain.EntityID
	for entity, record := range snapshot.Registry {
		if record.Retired() {
			continue
		}
		_, _, aliases, err := record.Aliases()
		if err != nil {
			return domain.EntityID{}, integrations.ErrCommandAuthorizationDenied
		}
		for _, candidate := range aliases {
			if candidate == alias {
				if found != (domain.EntityID{}) && found != entity {
					return domain.EntityID{}, integrations.ErrCommandAuthorizationDenied
				}
				found = entity
			}
		}
	}
	if found == (domain.EntityID{}) {
		return domain.EntityID{}, integrations.ErrCommandAuthorizationDenied
	}
	return found, nil
}
func (s *Service) authorizePreparedResult(ctx context.Context, plan *scenePlan) error {
	if s == nil || plan == nil || nilPort(s.commands) {
		return integrations.ErrCommandAuthorizationDenied
	}
	if plan.internal {
		if plan.service != s || s.internal == nil || !s.internal.SameOccurrence(plan.origin, ctx) || plan.actor != (domain.ActorID{}) {
			return integrations.ErrCommandAuthorizationDenied
		}
		return nil
	}
	bindingPort, hasBinding := s.commands.(interface {
		SceneAuthorizationBinding(context.Context) ([32]byte, error)
	})
	var before [32]byte
	if hasBinding {
		var err error
		before, err = bindingPort.SceneAuthorizationBinding(ctx)
		if err != nil {
			return integrations.ErrCommandAuthorizationDenied
		}
	}
	for _, member := range plan.members {
		if member.capturedEntity != (domain.EntityID{}) {
			if port, ok := s.commands.(interface {
				AuthorizeSceneEntity(context.Context, domain.EntityID) error
			}); ok {
				if err := port.AuthorizeSceneEntity(ctx, member.capturedEntity); err != nil {
					return integrations.ErrCommandAuthorizationDenied
				}
			}
		}
		var err error
		switch member.kind {
		case preparedLight:
			err = member.light.AuthorizeRead(ctx)
		case preparedFan:
			err = member.fan.AuthorizeRead(ctx)
		case preparedDirect:
			err = member.direct.AuthorizeRead(ctx)
		case preparedRefused:
			if errors.Is(member.cause, integrations.ErrCommandAuthorizationDenied) {
				return integrations.ErrCommandAuthorizationDenied
			}
			if port, ok := s.commands.(interface {
				AuthorizeSceneAlias(context.Context, domain.HAAlias) error
			}); ok {
				err = port.AuthorizeSceneAlias(ctx, member.alias)
			}
		default:
			err = integrations.ErrCommandAuthorizationDenied
		}
		if err != nil {
			return integrations.ErrCommandAuthorizationDenied
		}
	}
	if hasBinding {
		after, err := bindingPort.SceneAuthorizationBinding(ctx)
		if err != nil || before != after {
			return integrations.ErrCommandAuthorizationDenied
		}
	}
	return nil
}
