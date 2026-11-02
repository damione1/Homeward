package server

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/gen/home/media/v1/mediav1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sonos"
	"github.com/damione1/Homeward/internal/storage"
)

func addConfiguredSonos(plan *RuntimePlan, settings sonos.Settings) error {
	if plan == nil || plan.sonosRuntime == nil {
		return sonos.ErrSettings
	}
	repository, ok := plan.Identity.(*registry.Repository)
	if !ok {
		return ErrIdentityNotConfigured
	}
	instance, e := settings.Instance()
	if e != nil {
		return e
	}
	factory, e := plan.sonosRuntime.Factory(settings)
	if e != nil {
		return e
	}
	spec, e := integrations.NewPilotSpec(instance, factory)
	if e != nil {
		return e
	}
	previous := plan.Configure
	plan.Configure = func(ctx context.Context, host *integrations.Manager, store *storage.Store) error {
		for _, p := range settings.Players() {
			if _, e := repository.ConfigureBinding(ctx, instance, p.SourceKey(), p.Alias); e != nil {
				return e
			}
		}
		if previous != nil {
			return previous(ctx, host, store)
		}
		return nil
	}
	plan.Pilots = append(plan.Pilots, spec)
	return nil
}

type sonosHandler struct {
	runtime   *sonos.Runtime
	owner     *configactivation.Owner
	authority *authorization.Authority
}

func mediaReadError(code connect.Code) error {
	return connect.NewError(code, errors.New("media observation unavailable"))
}

func (h *sonosHandler) ReadSnapshot(ctx context.Context, request *connect.Request[media.ReadSnapshotRequest]) (*connect.Response[media.ReadSnapshotResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, mediaReadError(connect.CodeInvalidArgument)
	}
	entity, e := domain.ParseEntityID(request.Msg.EntityId)
	if e != nil {
		return nil, mediaReadError(connect.CodeInvalidArgument)
	}
	principal, e := authorization.PrincipalFromContext(ctx)
	if e != nil || !principal.Native() {
		return nil, mediaReadError(connect.CodePermissionDenied)
	}
	actor, e := principal.Actor()
	actual, present := ActorFromContext(ctx)
	if e != nil || !present || actor != actual {
		return nil, mediaReadError(connect.CodePermissionDenied)
	}
	stamp, e := h.owner.CurrentConfig(ctx)
	if e != nil {
		return nil, mediaReadError(connect.CodeUnavailable)
	}
	revision, _, e := stamp.Components()
	if e != nil {
		return nil, mediaReadError(connect.CodeUnavailable)
	}
	var snapshot *media.Snapshot
	e = h.authority.WithCurrent(ctx, principal, func(current authorization.Current) error {
		read := func(id domain.EntityID) error {
			slot, e := authorization.NewCapabilitySlot(sonos.Schema, sonos.Endpoint)
			if e != nil {
				return e
			}
			object, e := authorization.EntityObject(id, slot)
			if e != nil {
				return e
			}
			for _, family := range []auth.ScopeFamily{auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY} {
				permission, e := authorization.NewPermission(family, auth.ScopeOperation_SCOPE_OPERATION_READ)
				if e != nil {
					return e
				}
				if e = current.Authorize(permission, object); e != nil {
					return e
				}
			}
			return nil
		}
		if e := read(entity); e != nil {
			return e
		}
		value, ok := h.runtime.Snapshot(entity)
		if !ok {
			return sonos.ErrUnavailable
		}
		coordinator, e := domain.ParseEntityID(value.Route.CoordinatorEntityId)
		if e != nil {
			return sonos.ErrUnavailable
		}
		if e = read(coordinator); e != nil {
			return e
		}
		visible := value.Members[:0]
		for _, member := range value.Members {
			id, e := domain.ParseEntityID(member.EntityId)
			if e == nil && read(id) == nil {
				visible = append(visible, member)
			}
		}
		value.Members = visible
		value.Route.ConfigRevision = revision
		snapshot = value
		return nil
	})
	if e != nil {
		if errors.Is(e, authorization.ErrDenied) {
			return nil, mediaReadError(connect.CodePermissionDenied)
		}
		return nil, mediaReadError(connect.CodeUnavailable)
	}
	// Recheck configuration after the in-memory authorization fence. No network
	// work is performed inside that fence or on the public request path.
	current, e := h.owner.CurrentConfig(ctx)
	if e != nil || current != stamp {
		return nil, mediaReadError(connect.CodeUnavailable)
	}
	return connect.NewResponse(&media.ReadSnapshotResponse{Result: &media.ReadSnapshotResponse_Snapshot{Snapshot: snapshot}}), nil
}
func (h *sonosHandler) ReadArtwork(context.Context, *connect.Request[media.ReadArtworkRequest]) (*connect.Response[media.ReadArtworkResponse], error) {
	return nil, mediaReadError(connect.CodeUnimplemented)
}

func registerSonosRoutes(plan *PublicPlan, runtime *sonos.Runtime, owner *configactivation.Owner, authority *authorization.Authority) error {
	if runtime == nil {
		return nil
	}
	handler := &sonosHandler{runtime: runtime, owner: owner, authority: authority}
	paths := []string{mediav1connect.MediaServiceReadSnapshotProcedure, mediav1connect.MediaServiceReadArtworkProcedure}
	procedures := make([]ConnectProcedure, 0, len(paths))
	for _, path := range paths {
		procedures = append(procedures, ConnectProcedure{Path: path, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
	}
	if e := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return mediav1connect.NewMediaServiceHandler(handler, append(options, connect.WithReadMaxBytes(8192))...)
	}, procedures...); e != nil {
		return e
	}
	permission, e := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ)
	if e != nil {
		return e
	}
	for _, path := range paths {
		if e := plan.BindPermission(RouteKindConnect, RouteMethodPost, path, permission); e != nil {
			return e
		}
	}
	return nil
}
