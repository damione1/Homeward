package server

import (
	"context"
	"errors"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	nativeMedia "github.com/damione1/Homeward/gen/home/media/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/view"
)

type mediaCapabilityProjector interface {
	PlaybackCapability(context.Context, apitypes.HomeName, domain.EntityID, authorization.CapabilitySlot) (*api.MediaPlaybackCapability, *nativeMedia.Route, error)
}

// capabilityProjector materializes once, from the native authorized cut. It
// reads only bounded provider-owned media caches; page/stream rendering never
// calls this adapter. The view owner rechecks all capture fences afterwards.
func (h *coreRPCHandler) capabilityProjector(owner homeResourceOwner, authority *authorization.Authority) view.CapabilityProjector {
	return func(ctx context.Context, actor view.ActorID, item view.Item, cursor view.Cursor) ([]apiresources.Capability, error) {
		principal, err := authorization.PrincipalFromContext(ctx)
		if err != nil {
			return nil, authorization.ErrDenied
		}
		expected, err := principal.Actor()
		if err != nil || expected != actor {
			return nil, authorization.ErrDenied
		}
		current, err := authority.Current(principal)
		if err != nil {
			return nil, err
		}
		rightsRevision, binding, err := current.Binding()
		if err != nil {
			return nil, err
		}
		_, stamp, capturedRights, _, err := cursor.Components()
		if err != nil || capturedRights != rightsRevision {
			return nil, view.ErrViewChanged
		}
		revision, configHash, err := stamp.Components()
		if err != nil {
			return nil, err
		}
		catalogue, err := cursor.CataloguePosition()
		if err != nil {
			return nil, err
		}
		catalogueEpoch, cataloguePosition, err := catalogue.Components()
		if err != nil {
			return nil, err
		}
		admissionEpoch, err := h.epoch.CurrentAdmissionEpoch()
		if err != nil {
			return nil, err
		}
		fence, err := apiresources.NewDefinitionFence(revision, configHash, catalogueEpoch, cataloguePosition, admissionEpoch, binding)
		if err != nil {
			return nil, err
		}
		home, err := owner.HomeName(ctx)
		if err != nil {
			return nil, err
		}
		_, entity, err := item.Registry().IDs()
		if err != nil {
			return nil, err
		}
		observed, present := item.State()
		var reading *domain.ObservedState
		if present {
			reading = &observed
		}
		values, err := apiresources.ProjectCapabilities(home, entity, item.Capabilities(), reading)
		if err != nil {
			return nil, err
		}
		permissions := make(map[authorization.CapabilitySlot]api.ActionAccess)
		for _, permission := range item.ActionAccess() {
			if permission == nil {
				return nil, apiresources.ErrProjection
			}
			slot, err := authorization.NewCapabilitySlot(permission.SchemaId, permission.Endpoint)
			if err != nil {
				return nil, err
			}
			access := api.ActionAccess_ACTION_ACCESS_DENIED
			if permission.Access == state.ActionAccess_ACTION_ACCESS_ALLOWED {
				access = api.ActionAccess_ACTION_ACCESS_ALLOWED
			} else if permission.Access != state.ActionAccess_ACTION_ACCESS_DENIED {
				return nil, apiresources.ErrProjection
			}
			if _, duplicate := permissions[slot]; duplicate {
				return nil, apiresources.ErrProjection
			}
			permissions[slot] = access
		}
		for i, capability := range values {
			access, established := permissions[capability.Source()]
			if !established {
				access = api.ActionAccess_ACTION_ACCESS_DENIED
			}
			if capability.Trait() == apiresources.Opaque && !nilReactiveInterface(h.media) {
				schema, _, err := capability.Source().Values()
				if err != nil {
					return nil, err
				}
				if schema == "home.media.v1" || schema == "androidtv.media.v1" {
					playback, route, err := h.media.PlaybackCapability(ctx, home, entity, capability.Source())
					if err != nil {
						var fault apierrors.Fault
						// STATE_REGISTRY.READ does not grant private MEDIA.READ.
						// The already-readable opaque definition remains available.
						if !errors.As(err, &fault) || fault.Reason() != api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED {
							return nil, err
						}
					} else {
						capability, err = apiresources.WithMediaPlayback(capability, playback, route)
						if err != nil {
							return nil, err
						}
					}
				}
			}
			capability, err = capability.WithActionAccess(access)
			if err != nil {
				return nil, err
			}
			values[i], err = capability.BindDefinition(fence)
			if err != nil {
				return nil, err
			}
		}
		latest, err := h.epoch.CurrentAdmissionEpoch()
		if err != nil || latest != admissionEpoch {
			return nil, view.ErrViewChanged
		}
		return values, nil
	}
}
