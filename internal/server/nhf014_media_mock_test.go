package server

// Opt-in qualification adapter only. It has no device driver, URL fetcher or
// production registration. The fixture supplies a fresh native authority and
// synthetic registry identities; every call still crosses native TLS/session
// admission and the current object policy. Writes affect this bounded mock only.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/gen/home/actions/v1/actionsv1connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	media "github.com/damione1/Homeward/gen/home/media/v1"
	"github.com/damione1/Homeward/gen/home/media/v1/mediav1connect"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const nhf014MediaSchema = "home.media.v1.MediaAction"

type nhf014ArtworkMode uint8

const (
	nhf014ArtworkAllowed nhf014ArtworkMode = iota + 1
	nhf014ArtworkDenied
	nhf014ArtworkExpired
)

type nhf014MediaRecord struct {
	actor       domain.ActorID
	entity      domain.EntityID
	fingerprint [32]byte
	response    *actions.CommandResponse
	progress    *actions.ActionProgress
	changed     chan struct{}
}

type nhf014MediaMock struct {
	mu          sync.Mutex
	authority   *authorization.Authority
	snapshots   map[domain.EntityID]*media.Snapshot
	records     map[domain.CommandID]*nhf014MediaRecord
	png         []byte
	artworkMode nhf014ArtworkMode
	loseReply   bool
	submitCalls uint64
	mutations   uint64
	now         func() time.Time
}

func nhf014MediaError(code connect.Code) error {
	return connect.NewError(code, errors.New("synthetic media request rejected"))
}

// fn runs memory-only under authority -> mock locks. No SQL, network or image
// decoding is permitted inside this fence.
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

func (m *nhf014MediaMock) ReadSnapshot(ctx context.Context, request *connect.Request[media.ReadSnapshotRequest]) (*connect.Response[media.ReadSnapshotResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, nhf014MediaError(connect.CodeInvalidArgument)
	}
	var snapshot *media.Snapshot
	err := m.withEntity(ctx, request.Msg.EntityId, auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ, func(current authorization.Current, _ domain.ActorID, entity domain.EntityID, value *media.Snapshot) error {
		snapshot = proto.Clone(value).(*media.Snapshot)
		slot, err := authorization.NewCapabilitySlot(nhf014MediaSchema, value.Route.Endpoint)
		if err != nil {
			return err
		}
		object, err := authorization.EntityObject(entity, slot)
		if err != nil {
			return err
		}
		write, err := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_WRITE)
		if err != nil {
			return err
		}
		snapshot.ActionAccess = state.ActionAccess_ACTION_ACCESS_DENIED
		if current.Authorize(write, object) == nil {
			snapshot.ActionAccess = state.ActionAccess_ACTION_ACCESS_ALLOWED
		}
		if proto.Size(snapshot) > 64<<10 {
			return nhf014MediaError(connect.CodeResourceExhausted)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&media.ReadSnapshotResponse{Result: &media.ReadSnapshotResponse_Snapshot{Snapshot: snapshot}}), nil
}

func (m *nhf014MediaMock) ReadArtwork(ctx context.Context, request *connect.Request[media.ReadArtworkRequest]) (*connect.Response[media.ReadArtworkResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.Reference) == 0 || len(request.Msg.Reference) > 512 || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, nhf014MediaError(connect.CodeInvalidArgument)
	}
	var reply *media.ReadArtworkResponse
	err := m.withEntity(ctx, request.Msg.EntityId, auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ, func(_ authorization.Current, _ domain.ActorID, _ domain.EntityID, snapshot *media.Snapshot) error {
		switch m.artworkMode {
		case nhf014ArtworkDenied:
			return nhf014MediaError(connect.CodePermissionDenied)
		case nhf014ArtworkExpired:
			return nhf014MediaError(connect.CodeFailedPrecondition)
		case nhf014ArtworkAllowed:
		default:
			return nhf014MediaError(connect.CodeUnavailable)
		}
		ref := snapshot.Artwork
		if ref == nil || ref.ExpiresAt == nil || ref.ExpiresAt.CheckValid() != nil || !ref.ExpiresAt.AsTime().After(m.now()) || !bytes.Equal(ref.Reference, request.Msg.Reference) {
			return nhf014MediaError(connect.CodeFailedPrecondition)
		}
		if len(m.png) == 0 || len(m.png) > 128<<10 {
			return nhf014MediaError(connect.CodeResourceExhausted)
		}
		reply = &media.ReadArtworkResponse{Png: bytes.Clone(m.png), ExpiresAt: proto.Clone(ref.ExpiresAt).(*timestamppb.Timestamp)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(reply), nil
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

func (m *nhf014MediaMock) SubmitAction(ctx context.Context, request *connect.Request[actions.SubmitActionRequest]) (*connect.Response[actions.SubmitActionResponse], error) {
	if request == nil || request.Msg == nil || request.Msg.Command == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, nhf014MediaError(connect.CodeInvalidArgument)
	}
	command := request.Msg.Command
	id, err := domain.ParseCommandID(command.CommandId)
	if err != nil {
		return nil, nhf014MediaError(connect.CodeInvalidArgument)
	}
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(command)
	if err != nil || len(encoded) > 8192 {
		return nil, nhf014MediaError(connect.CodeInvalidArgument)
	}
	fingerprint := sha256.Sum256(encoded)
	var response *actions.CommandResponse
	lost := false
	err = m.withEntity(ctx, command.GetTarget().GetEntityId(), auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_WRITE, func(_ authorization.Current, actor domain.ActorID, entity domain.EntityID, snapshot *media.Snapshot) error {
		m.submitCalls++
		if existing := m.records[id]; existing != nil {
			if existing.actor != actor || existing.fingerprint != fingerprint {
				return nhf014MediaError(connect.CodeAlreadyExists)
			}
			response = proto.Clone(existing.response).(*actions.CommandResponse)
			return nil
		}
		if len(m.records) >= 32 {
			return nhf014MediaError(connect.CodeResourceExhausted)
		}
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
			snapshot.GroupId = proto.String(action.GroupId)
		case nil:
			return nhf014MediaError(connect.CodeInvalidArgument)
		default:
			return nhf014MediaError(connect.CodeInvalidArgument)
		}
		snapshot.ReceivedAt = timestamppb.New(m.now())
		m.mutations++
		response = &actions.CommandResponse{CommandId: command.CommandId, Status: actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED}
		m.records[id] = &nhf014MediaRecord{actor: actor, entity: entity, fingerprint: fingerprint, response: proto.Clone(response).(*actions.CommandResponse), progress: &actions.ActionProgress{AdmissionEpoch: command.AdmissionEpoch, ConfigRevision: command.ConfigRevision, Sequence: 1, Status: actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED}, changed: make(chan struct{})}
		lost, m.loseReply = m.loseReply, false
		return nil
	})
	if err != nil {
		return nil, err
	}
	if lost {
		return nil, nhf014MediaError(connect.CodeUnavailable)
	}
	return connect.NewResponse(&actions.SubmitActionResponse{Result: proto.Clone(response).(*actions.CommandResponse)}), nil
}

func (m *nhf014MediaMock) record(ctx context.Context, name string) (*actions.GetActionResponse, <-chan struct{}, error) {
	id, err := domain.ParseCommandID(name)
	if err != nil {
		return nil, nil, nhf014MediaError(connect.CodeInvalidArgument)
	}
	m.mu.Lock()
	record := m.records[id]
	var entity string
	if record != nil {
		entity, _ = record.entity.Value()
	}
	m.mu.Unlock()
	var response *actions.GetActionResponse
	var changed <-chan struct{}
	err = m.withEntity(ctx, entity, auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_READ, func(_ authorization.Current, actor domain.ActorID, _ domain.EntityID, _ *media.Snapshot) error {
		record := m.records[id]
		if record == nil || record.actor != actor {
			return nhf014MediaError(connect.CodePermissionDenied)
		}
		response = &actions.GetActionResponse{Durable: &actions.DurableActionResult{State: actions.DurableActionState_DURABLE_ACTION_STATE_RESULT, Error: actions.DurableActionError_DURABLE_ACTION_ERROR_NONE, Response: proto.Clone(record.response).(*actions.CommandResponse)}, Live: &actions.GetActionResponse_Progress{Progress: proto.Clone(record.progress).(*actions.ActionProgress)}}
		changed = record.changed
		return nil
	})
	return response, changed, err
}

func (m *nhf014MediaMock) GetAction(ctx context.Context, request *connect.Request[actions.GetActionRequest]) (*connect.Response[actions.GetActionResponse], error) {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nil, nhf014MediaError(connect.CodeInvalidArgument)
	}
	response, _, err := m.record(ctx, request.Msg.CommandId)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}

func (m *nhf014MediaMock) WatchAction(ctx context.Context, request *connect.Request[actions.WatchActionRequest], stream *connect.ServerStream[actions.WatchActionResponse]) error {
	if request == nil || request.Msg == nil || len(request.Msg.ProtoReflect().GetUnknown()) != 0 {
		return nhf014MediaError(connect.CodeInvalidArgument)
	}
	for sequence := uint64(1); sequence <= 32; sequence++ {
		response, changed, err := m.record(ctx, request.Msg.CommandId)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = stream.Send(&actions.WatchActionResponse{StreamSequence: sequence, Frame: &actions.WatchActionResponse_Progress{Progress: response.GetProgress()}}); err != nil {
			return err
		}
		switch response.GetProgress().Status {
		case actions.ActionStatus_ACTION_STATUS_CONFIRMED, actions.ActionStatus_ACTION_STATUS_FAILED, actions.ActionStatus_ACTION_STATUS_CANCELLED, actions.ActionStatus_ACTION_STATUS_INDETERMINATE:
			return nil
		case actions.ActionStatus_ACTION_STATUS_ACCEPTED, actions.ActionStatus_ACTION_STATUS_DISPATCHED, actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED:
		case actions.ActionStatus_ACTION_STATUS_UNSPECIFIED:
			return nhf014MediaError(connect.CodeDataLoss)
		default:
			return nhf014MediaError(connect.CodeDataLoss)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
	return nhf014MediaError(connect.CodeResourceExhausted)
}

func (m *nhf014MediaMock) register(plan *PublicPlan) error {
	for _, registration := range []struct {
		build ConnectHandlerBuilder
		paths []struct {
			path      string
			family    auth.ScopeFamily
			operation auth.ScopeOperation
			flow      RouteFlow
		}
	}{
		{func(options ...connect.HandlerOption) (string, http.Handler) {
			return mediav1connect.NewMediaServiceHandler(m, append(options, connect.WithReadMaxBytes(8192))...)
		}, []struct {
			path      string
			family    auth.ScopeFamily
			operation auth.ScopeOperation
			flow      RouteFlow
		}{
			{mediav1connect.MediaServiceReadSnapshotProcedure, auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
			{mediav1connect.MediaServiceReadArtworkProcedure, auth.ScopeFamily_SCOPE_FAMILY_MEDIA, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
		}},
		{func(options ...connect.HandlerOption) (string, http.Handler) {
			return actionsv1connect.NewActionServiceHandler(m, append(options, connect.WithReadMaxBytes(8192))...)
		}, []struct {
			path      string
			family    auth.ScopeFamily
			operation auth.ScopeOperation
			flow      RouteFlow
		}{
			{actionsv1connect.ActionServiceSubmitActionProcedure, auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_WRITE, RouteFlowUnary},
			{actionsv1connect.ActionServiceGetActionProcedure, auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowUnary},
			{actionsv1connect.ActionServiceWatchActionProcedure, auth.ScopeFamily_SCOPE_FAMILY_ACTIONS, auth.ScopeOperation_SCOPE_OPERATION_READ, RouteFlowServerStream},
		}},
	} {
		procedures := make([]ConnectProcedure, 0, len(registration.paths))
		for _, route := range registration.paths {
			procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: route.flow, Transport: RouteTransportTLSRequired})
		}
		if err := plan.RegisterConnect(registration.build, procedures...); err != nil {
			return err
		}
		for _, route := range registration.paths {
			permission, err := authorization.NewPermission(route.family, route.operation)
			if err != nil {
				return err
			}
			if err = plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
				return err
			}
		}
	}
	return nil
}
