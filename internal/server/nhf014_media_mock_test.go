package server

// Synthetic native media bytes and command effects only. Public reads and LROs
// are served by the Run-owned Core and SQL session/command services.
import (
	"bytes"
	"connectrpc.com/connect"
	"context"
	"errors"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sync"
	"time"
)

const nhf014MediaSchema = "home.media.v1"

type nhf014ArtworkMode uint8

const (
	nhf014ArtworkAllowed nhf014ArtworkMode = iota
	nhf014ArtworkDenied
	nhf014ArtworkExpired
)

type nhf014MediaRecord struct {
	entity  domain.EntityID
	command integrations.AdmittedCommand
}
type nhf014GroupID string
type nhf014GroupRoute struct {
	coordinator domain.EntityID
	members     []*media.Member
}
type nhf014MediaMock struct {
	mu                     sync.Mutex
	authority              *authorization.Authority
	snapshots              map[domain.EntityID]*media.Snapshot
	records                map[domain.CommandID]*nhf014MediaRecord
	artwork                map[domain.EntityID]*nha202MediaOwner
	png                    []byte
	artworkMode            nhf014ArtworkMode
	loseReply              bool
	submitCalls, mutations uint64
	now                    func() time.Time
	unavailable            map[domain.EntityID]bool
	offline                bool
	groups                 map[domain.EntityID]map[nhf014GroupID]nhf014GroupRoute
}

func nhf014MediaError(code connect.Code) error {
	reason := api.ApiErrorReason_API_ERROR_REASON_INTERNAL_FAILURE
	switch code {
	case connect.CodePermissionDenied:
		reason = api.ApiErrorReason_API_ERROR_REASON_ACCESS_DENIED
	case connect.CodeInvalidArgument:
		reason = api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED
	case connect.CodeFailedPrecondition:
		reason = api.ApiErrorReason_API_ERROR_REASON_TOPOLOGY_CHANGED
	case connect.CodeResourceExhausted:
		reason = api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED
	case connect.CodeUnavailable:
		reason = api.ApiErrorReason_API_ERROR_REASON_SERVICE_UNAVAILABLE
	}
	return apierrors.New(reason).Connect()
}
func (m *nhf014MediaMock) withEntity(ctx context.Context, name string, family auth.ScopeFamily, operation auth.ScopeOperation, fn func(authorization.Current, domain.ActorID, domain.EntityID, *media.Snapshot) error) error {
	if m == nil || m.authority == nil || ctx == nil || ctx.Err() != nil || len(name) == 0 || len(name) > 256 {
		return nhf014MediaError(connect.CodePermissionDenied)
	}
	principal, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !principal.Native() {
		return nhf014MediaError(connect.CodePermissionDenied)
	}
	actor, err := principal.Actor()
	transportActor, ok := ActorFromContext(ctx)
	if err != nil || !ok || actor != transportActor {
		return nhf014MediaError(connect.CodePermissionDenied)
	}
	entity, err := domain.ParseEntityID(name)
	if err != nil {
		return nhf014MediaError(connect.CodeInvalidArgument)
	}
	permission, err := authorization.NewPermission(family, operation)
	if err != nil {
		return err
	}
	err = m.authority.WithCurrent(ctx, principal, func(current authorization.Current) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		snapshot := m.snapshots[entity]
		if snapshot == nil || snapshot.Route == nil {
			return authorization.ErrDenied
		}
		slot, err := authorization.NewCapabilitySlot(nhf014MediaSchema, snapshot.Route.Endpoint)
		if err != nil {
			return err
		}
		object, err := authorization.EntityObject(entity, slot)
		if err != nil {
			return err
		}
		if err := current.Authorize(permission, object); err != nil {
			return err
		}
		read, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ)
		if err != nil {
			return err
		}
		if err := current.Authorize(read, object); err != nil {
			return err
		}
		if m.offline || m.unavailable[entity] {
			return nhf014MediaError(connect.CodeUnavailable)
		}
		return fn(current, actor, entity, snapshot)
	})
	if errors.Is(err, authorization.ErrDenied) {
		return nhf014MediaError(connect.CodePermissionDenied)
	}
	if errors.Is(err, authorization.ErrUnavailable) {
		return nhf014MediaError(connect.CodeUnavailable)
	}
	return err
}

func nhf014Quantity(value uint64, bounds *media.QuantityBounds, ceiling uint64) bool {
	return bounds != nil && len(bounds.ProtoReflect().GetUnknown()) == 0 && bounds.Minimum <= bounds.Maximum && bounds.Maximum <= ceiling && bounds.Step > 0 && bounds.Step <= ceiling && value >= bounds.Minimum && value <= bounds.Maximum && (value-bounds.Minimum)%bounds.Step == 0
}

func nhf014AdmitMediaAction(command *actions.CommandRequest, snapshot *media.Snapshot, now time.Time) (*media.MediaAction, error) {
	fail := func() (*media.MediaAction, error) { return nil, nhf014MediaError(connect.CodeFailedPrecondition) }
	if command == nil || snapshot == nil || snapshot.Route == nil || snapshot.Capabilities == nil || len(command.ProtoReflect().GetUnknown()) != 0 || len(command.CommandId) == 0 || len(command.CommandId) > 256 || command.GetTarget().GetEntityId() != snapshot.Route.EntityId || command.CapabilitySchemaId != nhf014MediaSchema || command.Endpoint != snapshot.Route.Endpoint || command.AdmissionEpoch != snapshot.Route.AdmissionEpoch || command.ConfigRevision != snapshot.Route.ConfigRevision || command.ExpiresAt == nil || command.ExpiresAt.CheckValid() != nil || !command.ExpiresAt.AsTime().After(now) || command.ExpiresAt.AsTime().After(now.Add(31*time.Second)) {
		return fail()
	}
	extension := command.GetArguments().GetExtension()
	if extension == nil || extension.SchemaId != nhf014MediaSchema || extension.Major != 1 || len(extension.DescriptorSha256) != 32 || !bytes.Equal(extension.DescriptorSha256, snapshot.DescriptorSha256) || len(extension.ProtobufPayload) == 0 || len(extension.ProtobufPayload) > 4096 || len(extension.ProtoReflect().GetUnknown()) != 0 || len(command.GetArguments().ProtoReflect().GetUnknown()) != 0 {
		return fail()
	}
	var value media.MediaAction
	if proto.Unmarshal(extension.ProtobufPayload, &value) != nil || len(value.ProtoReflect().GetUnknown()) != 0 || value.Route == nil || !proto.Equal(value.Route, snapshot.Route) {
		return fail()
	}
	limits := snapshot.Capabilities
	switch action := value.Command.(type) {
	case *media.MediaAction_Operation:
		known := false
		switch action.Operation {
		case media.Operation_OPERATION_PLAY, media.Operation_OPERATION_PAUSE, media.Operation_OPERATION_STOP, media.Operation_OPERATION_PREVIOUS, media.Operation_OPERATION_NEXT:
			for _, allowed := range limits.Operations {
				if action.Operation == allowed {
					known = true
				}
			}
		case media.Operation_OPERATION_UNSPECIFIED:
		default:
		}
		if !known {
			return fail()
		}
	case *media.MediaAction_VolumeBasisPoints:
		if !nhf014Quantity(uint64(action.VolumeBasisPoints), limits.VolumeBasisPoints, 10_000) {
			return fail()
		}
	case *media.MediaAction_PositionMillis:
		if snapshot.DurationMillis == nil || action.PositionMillis > *snapshot.DurationMillis || !nhf014Quantity(action.PositionMillis, limits.PositionMillis, 7*24*60*60*1000) {
			return fail()
		}
	case *media.MediaAction_SourceId:
		found := false
		for _, option := range limits.Sources {
			if option != nil && option.Id == action.SourceId {
				found = true
			}
		}
		if !found {
			return fail()
		}
	case *media.MediaAction_GroupId:
		found := false
		for _, option := range limits.Groups {
			if option != nil && option.Id == action.GroupId {
				found = true
			}
		}
		if !found {
			return fail()
		}
	case nil:
		return fail()
	default:
		return fail()
	}
	return &value, nil
}

func (m *nhf014MediaMock) PlaybackCapability(ctx context.Context, home apitypes.HomeName, entity domain.EntityID, slot authorization.CapabilitySlot) (*api.MediaPlaybackCapability, *media.Route, error) {
	raw, err := entity.Value()
	if err != nil {
		return nil, nil, err
	}
	schema, endpoint, err := slot.Values()
	if err != nil || schema != nhf014MediaSchema {
		return nil, nil, nhf014MediaError(connect.CodePermissionDenied)
	}
	var snapshot *media.Snapshot
	err = m.withEntity(ctx, raw, auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ, func(current authorization.Current, _ domain.ActorID, _ domain.EntityID, value *media.Snapshot) error {
		if value.Route.Endpoint != endpoint {
			return authorization.ErrDenied
		}
		snapshot = proto.Clone(value).(*media.Snapshot)
		object, err := authorization.EntityObject(entity, slot)
		if err != nil {
			return err
		}
		write, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_WRITE)
		snapshot.ActionAccess = state.ActionAccess_ACTION_ACCESS_DENIED
		if current.Authorize(write, object) == nil {
			snapshot.ActionAccess = state.ActionAccess_ACTION_ACCESS_ALLOWED
		}
		if snapshot.Artwork != nil {
			owner := m.artwork[entity]
			if owner == nil {
				return nhf014MediaError(connect.CodeUnavailable)
			}
			_, credential, err := current.Binding()
			if err != nil {
				return err
			}
			owner.mu.Lock()
			owner.png = bytes.Clone(m.png)
			owner.offline = m.offline
			owner.artworkDenied = m.artworkMode == nhf014ArtworkDenied
			owner.artworkExpiry = snapshot.Artwork.ExpiresAt.AsTime()
			owner.mu.Unlock()
			snapshot.Artwork.Reference = owner.reference(credential)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	value, err := apiresources.ProjectMediaPlayback(home, snapshot, api.MediaDataProvenance_MEDIA_DATA_PROVENANCE_SYNTHETIC)
	if err != nil {
		return nil, nil, err
	}
	if value.Observation != nil && value.Observation.Artwork != nil {
		name, err := mediaSourceName(home, entity, slot)
		if err != nil {
			return nil, nil, err
		}
		value.Observation.Artwork.MediaSource = name.String()
	}
	return value, proto.CloneOf(snapshot.Route), nil
}

// Descriptor validation precedes Host dispatch. Resource/route/current support
// are rechecked by Core and by the native source before its one synthetic effect.
type nhf014MediaCodec struct{}

func (nhf014MediaCodec) Validate(message proto.Message) error {
	raw, err := proto.Marshal(message)
	if err != nil || len(raw) == 0 || len(raw) > 4096 {
		return integrations.ErrInvalidInput
	}
	value := new(media.MediaAction)
	if proto.Unmarshal(raw, value) != nil || len(value.ProtoReflect().GetUnknown()) != 0 || value.Route == nil || len(value.Route.ProtoReflect().GetUnknown()) != 0 || value.Route.EntityId == "" || value.Route.CoordinatorEntityId == "" || value.Route.Endpoint != "media" || value.Route.TopologyRevision == 0 || value.Route.AdmissionEpoch == "" || value.Route.ConfigRevision == 0 {
		return integrations.ErrInvalidInput
	}
	switch command := value.Command.(type) {
	case *media.MediaAction_Operation:
		if command == nil || command.Operation < media.Operation_OPERATION_PLAY || command.Operation > media.Operation_OPERATION_NEXT {
			return integrations.ErrInvalidInput
		}
	case *media.MediaAction_VolumeBasisPoints:
		if command == nil || command.VolumeBasisPoints > 10000 {
			return integrations.ErrInvalidInput
		}
	case *media.MediaAction_PositionMillis:
		if command == nil || command.PositionMillis > 7*24*60*60*1000 {
			return integrations.ErrInvalidInput
		}
	case *media.MediaAction_SourceId:
		if command == nil || command.SourceId == "" {
			return integrations.ErrInvalidInput
		}
	case *media.MediaAction_GroupId:
		if command == nil || command.GroupId == "" {
			return integrations.ErrInvalidInput
		}
	default:
		return integrations.ErrInvalidInput
	}
	return nil
}
func (m *nhf014MediaMock) Dispatch(ctx context.Context, admitted integrations.AdmittedCommand) error {
	if ctx.Err() != nil || !admitted.ReadyForTransport() {
		return authorization.ErrDenied
	}
	_, entity, err := admitted.RouteIdentity()
	if err != nil {
		return err
	}
	command, err := admitted.Request()
	if err != nil {
		return err
	}
	id, err := domain.ParseCommandID(command.CommandId)
	if err != nil {
		return err
	}
	err = func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.offline || m.unavailable[entity] {
			return nhf014MediaError(connect.CodeUnavailable)
		}
		if m.records[id] != nil {
			return errors.New("native owner dispatched same occurrence twice")
		}
		snapshot := m.snapshots[entity]
		value, err := nhf014AdmitMediaAction(command, snapshot, m.now())
		if err != nil {
			return err
		}
		// Explicit synthetic observations, never an application optimistic value.
		switch action := value.Command.(type) {
		case *media.MediaAction_Operation:
			switch action.Operation {
			case media.Operation_OPERATION_PLAY:
				snapshot.Playback = media.PlaybackState_PLAYBACK_STATE_PLAYING
			case media.Operation_OPERATION_PAUSE:
				snapshot.Playback = media.PlaybackState_PLAYBACK_STATE_PAUSED
			case media.Operation_OPERATION_STOP:
				snapshot.Playback = media.PlaybackState_PLAYBACK_STATE_STOPPED
			case media.Operation_OPERATION_PREVIOUS, media.Operation_OPERATION_NEXT:
			case media.Operation_OPERATION_UNSPECIFIED:
				return nhf014MediaError(connect.CodeInvalidArgument)
			default:
				return nhf014MediaError(connect.CodeInvalidArgument)
			}
		case *media.MediaAction_VolumeBasisPoints:
			snapshot.VolumeBasisPoints = proto.Uint32(action.VolumeBasisPoints)
		case *media.MediaAction_PositionMillis:
			snapshot.PositionMillis = proto.Uint64(action.PositionMillis)
		case *media.MediaAction_SourceId:
			snapshot.SourceId = proto.String(action.SourceId)
		case *media.MediaAction_GroupId:
			preset, ok := m.groups[entity][nhf014GroupID(action.GroupId)]
			if !ok || snapshot.Route.TopologyRevision == ^uint64(0) {
				return nhf014MediaError(connect.CodeFailedPrecondition)
			}
			coordinator, err := preset.coordinator.Value()
			if err != nil {
				return err
			}
			snapshot.GroupId = proto.String(action.GroupId)
			snapshot.Route.CoordinatorEntityId = coordinator
			snapshot.Route.TopologyRevision++
			snapshot.Members = make([]*media.Member, 0, len(preset.members))
			for _, member := range preset.members {
				snapshot.Members = append(snapshot.Members, proto.Clone(member).(*media.Member))
			}
		case nil:
			return nhf014MediaError(connect.CodeInvalidArgument)
		default:
			return nhf014MediaError(connect.CodeInvalidArgument)
		}

		snapshot.ReceivedAt = timestamppb.New(m.now())
		m.mutations++
		m.records[id] = &nhf014MediaRecord{entity, admitted}
		return nil
	}()
	if err != nil {
		return err
	}
	return admitted.Acknowledge()
}

// An explicit fixture control confirms only the exact effect already recorded
// by this synthetic driver, through the native tracker API.
type nhf014SyntheticProof struct {
	id     domain.CommandID
	entity domain.EntityID
}

func (p nhf014SyntheticProof) QualifyProtocolConfirmation(command integrations.AdmittedCommand) error {
	id, entity, err := command.RouteIdentity()
	if err != nil || id != p.id || entity != p.entity {
		return integrations.ErrInvalidInput
	}
	return nil
}
