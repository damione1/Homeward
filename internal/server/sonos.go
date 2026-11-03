package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/gen/home/media/v1/mediav1connect"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/androidtv"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sonos"
	"github.com/damione1/Homeward/internal/storage"
	"google.golang.org/protobuf/proto"
)

func newNativeSonosRuntime(catalog *contracts.CapabilityCatalog) (*sonos.Runtime, error) {
	runtime, err := sonos.NewRuntime()
	if err == nil {
		err = runtime.RegisterCodec(catalog)
	}
	return runtime, err
}

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

type nativeMediaSource struct {
	schema   string
	endpoint string
	snapshot func(domain.EntityID) (*media.Snapshot, bool)
	commands bool
}
type nativeMediaHandler struct {
	sources   []nativeMediaSource
	owner     *configactivation.Owner
	authority *authorization.Authority
	epoch     integrations.CommandEpochAuthority
}

func mediaReadError(code connect.Code) error {
	return connect.NewError(code, errors.New("media observation unavailable"))
}

func (h *nativeMediaHandler) ReadSnapshot(ctx context.Context, request *connect.Request[media.ReadSnapshotRequest]) (*connect.Response[media.ReadSnapshotResponse], error) {
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
		var err error
		snapshot, err = projectNativeMediaCurrent(current, entity, h.sources, revision, h.epoch)
		return err
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

func projectNativeMediaCurrent(current authorization.Current, entity domain.EntityID, sources []nativeMediaSource, revision uint64, epochAuthority integrations.CommandEpochAuthority) (*media.Snapshot, error) {
	if epochAuthority == nil {
		return nil, sonos.ErrUnavailable
	}
	epoch, err := epochAuthority.CurrentAdmissionEpoch()
	if err != nil || epoch == "" {
		return nil, sonos.ErrUnavailable
	}
	authorize := func(id domain.EntityID, schema, endpoint string, families []auth.ScopeFamily, operation auth.ScopeOperation) error {
		slot, err := authorization.NewCapabilitySlot(schema, endpoint)
		if err != nil {
			return err
		}
		object, err := authorization.EntityObject(id, slot)
		if err != nil {
			return err
		}
		for _, family := range families {
			permission, err := authorization.NewPermission(family, operation)
			if err != nil {
				return err
			}
			if err = current.Authorize(permission, object); err != nil {
				return err
			}
		}
		return nil
	}
	return projectNativeMediaActions(entity, sources, revision, epoch,
		func(id domain.EntityID, schema, endpoint string) error {
			return authorize(id, schema, endpoint, []auth.ScopeFamily{auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeFamily_SCOPE_FAMILY_STATE_REGISTRY}, auth.ScopeOperation_SCOPE_OPERATION_READ)
		}, func(id domain.EntityID, schema, endpoint string) error {
			return authorize(id, schema, endpoint, []auth.ScopeFamily{auth.ScopeFamily_SCOPE_FAMILY_ACTIONS}, auth.ScopeOperation_SCOPE_OPERATION_WRITE)
		})
}

// projectNativeMedia runs entirely inside the current authorization fence.
// Sources return owned cache copies; it never invokes network/provider work.
func projectNativeMedia(entity domain.EntityID, sources []nativeMediaSource, revision uint64, authorize func(domain.EntityID, string, string) error) (*media.Snapshot, error) {
	return projectNativeMediaActions(entity, sources, revision, "", authorize, nil)
}

func projectNativeMediaActions(entity domain.EntityID, sources []nativeMediaSource, revision uint64, epoch string, authorize, authorizeWrite func(domain.EntityID, string, string) error) (*media.Snapshot, error) {
	identity, err := entity.Value()
	if err != nil || authorize == nil {
		return nil, sonos.ErrUnavailable
	}
	var snapshot *media.Snapshot
	readable := false
	for _, source := range sources {
		read := func(id domain.EntityID) error { return authorize(id, source.schema, source.endpoint) }
		if err := read(entity); err != nil {
			if !errors.Is(err, authorization.ErrDenied) {
				return nil, err
			}
			continue
		}
		readable = true
		if source.snapshot == nil {
			return nil, sonos.ErrUnavailable
		}
		value, ok := source.snapshot(entity)
		if !ok {
			continue
		}
		if snapshot != nil || value == nil || value.Route == nil || value.Route.EntityId != identity || value.Route.Endpoint != source.endpoint {
			return nil, sonos.ErrUnavailable
		}
		value = proto.Clone(value).(*media.Snapshot)
		value.ActionAccess = state.ActionAccess_ACTION_ACCESS_DENIED
		standalone := len(value.Members) == 1 && value.Members[0] != nil && value.Members[0].EntityId == identity && value.Route.CoordinatorEntityId == identity
		coordinator, err := domain.ParseEntityID(value.Route.CoordinatorEntityId)
		if err != nil {
			return nil, sonos.ErrUnavailable
		}
		if err = read(coordinator); err != nil {
			return nil, err
		}
		if len(value.Members) > 16 {
			return nil, sonos.ErrUnavailable
		}
		visible := value.Members[:0]
		seen := map[string]bool{}
		for _, member := range value.Members {
			if member == nil || seen[member.EntityId] {
				return nil, sonos.ErrUnavailable
			}
			seen[member.EntityId] = true
			id, err := domain.ParseEntityID(member.EntityId)
			if err != nil {
				return nil, sonos.ErrUnavailable
			}
			if err = read(id); err == nil {
				visible = append(visible, member)
			} else if !errors.Is(err, authorization.ErrDenied) {
				return nil, err
			}
		}
		value.Members = visible
		value.Route.ConfigRevision = revision
		value.Route.AdmissionEpoch = epoch
		if source.commands {
			value.Capabilities = supportedNativeMediaCommands(value.Capabilities)
			if !standalone {
				value.Capabilities = &media.Capabilities{}
			}
			if standalone && nativeMediaCommandsFresh(value) && epoch != "" && revision != 0 && authorizeWrite != nil {
				if err = authorizeWrite(entity, source.schema, source.endpoint); err == nil {
					value.ActionAccess = state.ActionAccess_ACTION_ACCESS_ALLOWED
				} else if !errors.Is(err, authorization.ErrDenied) {
					return nil, err
				}
			}
		}
		snapshot = value
	}
	if !readable {
		return nil, authorization.ErrDenied
	}
	if snapshot == nil {
		return nil, sonos.ErrUnavailable
	}
	return snapshot, nil
}

func supportedNativeMediaCommands(capabilities *media.Capabilities) *media.Capabilities {
	result := &media.Capabilities{}
	if capabilities == nil {
		return result
	}
	seen := map[media.Operation]bool{}
	for _, operation := range capabilities.Operations {
		if !seen[operation] && (operation == media.Operation_OPERATION_PLAY || operation == media.Operation_OPERATION_PAUSE || operation == media.Operation_OPERATION_STOP) {
			result.Operations = append(result.Operations, operation)
			seen[operation] = true
		}
	}
	if volume := capabilities.VolumeBasisPoints; volume != nil && volume.Minimum == 0 && volume.Maximum == 10000 && volume.Step == 100 {
		result.VolumeBasisPoints = proto.Clone(volume).(*media.QuantityBounds)
	}
	return result
}

func nativeMediaCommandsFresh(snapshot *media.Snapshot) bool {
	if snapshot.Quality == nil || snapshot.Quality.Availability != state.Availability_AVAILABILITY_AVAILABLE || snapshot.Quality.Freshness != state.Freshness_FRESHNESS_FRESH || snapshot.Quality.Provenance != state.Provenance_PROVENANCE_OBSERVED || snapshot.ReceivedAt == nil || snapshot.ReceivedAt.CheckValid() != nil || snapshot.Route.TopologyRevision == 0 || len(snapshot.DescriptorSha256) != 32 || snapshot.Capabilities == nil || len(snapshot.Capabilities.Operations) == 0 && snapshot.Capabilities.VolumeBasisPoints == nil {
		return false
	}
	age := time.Since(snapshot.ReceivedAt.AsTime())
	return age >= 0 && age <= 45*time.Second
}

func (h *nativeMediaHandler) ReadArtwork(context.Context, *connect.Request[media.ReadArtworkRequest]) (*connect.Response[media.ReadArtworkResponse], error) {
	return nil, mediaReadError(connect.CodeUnimplemented)
}

func registerNativeMediaRoutes(plan *PublicPlan, runtime *sonos.Runtime, tv *androidtv.Runtime, owner *configactivation.Owner, authority *authorization.Authority, epoch integrations.CommandEpochAuthority) error {
	if runtime == nil && tv == nil {
		return nil
	}
	if plan == nil || owner == nil || authority == nil || epoch == nil {
		return sonos.ErrSettings
	}
	handler := &nativeMediaHandler{owner: owner, authority: authority, epoch: epoch}
	if runtime != nil {
		handler.sources = append(handler.sources, nativeMediaSource{schema: sonos.Schema, endpoint: sonos.Endpoint, snapshot: runtime.Snapshot, commands: true})
	}
	if tv != nil {
		handler.sources = append(handler.sources, nativeMediaSource{schema: androidtv.Schema, endpoint: androidtv.Endpoint, snapshot: tv.Snapshot})
	}
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
