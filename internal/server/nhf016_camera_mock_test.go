package server

// Opt-in synthetic qualification only: no production registration, device
// driver, provider credential, URL fetcher or persistent image cache.
import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	av "github.com/damione1/Homeward/gen/home/auth/v1"
	cv "github.com/damione1/Homeward/gen/home/cameras/v1"
	"github.com/damione1/Homeward/gen/home/cameras/v1/camerasv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const nhf016CameraSchema = "home.cameras.v1.Camera"
const nhf016CameraEndpoint = "camera"
const nhf016LeaseLifetime = 60 * time.Second

type nhf016LeaseState uint8

const (
	nhf016LeaseActive nhf016LeaseState = iota + 1
	nhf016LeaseExpired
	nhf016LeaseRevoked
	nhf016LeaseClosed
)

type nhf016SessionStatus uint8

const (
	nhf016SessionValid nhf016SessionStatus = iota + 1
	nhf016SessionExpired
	nhf016SessionRevoked
	nhf016SessionWrongCamera
)

type nhf016CameraImageState uint8

const (
	nhf016ImageAvailable nhf016CameraImageState = iota
	nhf016ImageMissing
	nhf016ImageWrongCamera
)

type nhf016CameraRecord struct {
	metadata   *cv.Camera
	events     []*cv.Event
	png        []byte
	imageState nhf016CameraImageState
	revision   uint64
	changed    chan struct{}
}
type nhf016CameraLease struct {
	wire    *cv.Lease
	actor   domain.ActorID
	binding [32]byte
	state   nhf016LeaseState
	done    chan struct{}
}
type nhf016CameraMock struct {
	mu            sync.Mutex
	authority     *authorization.Authority
	records       map[domain.EntityID]*nhf016CameraRecord
	leases        map[[32]byte]*nhf016CameraLease
	cursorKey     [32]byte
	lifetime      time.Duration
	now           func() time.Time
	offline       bool
	imageGate     <-chan struct{}
	imageRequests atomic.Uint64
	activeReads   atomic.Int64
	activeStreams atomic.Int64
}

var _ camerasv1connect.CameraServiceHandler = (*nhf016CameraMock)(nil)

func nhf016CameraError(code connect.Code) error {
	return connect.NewError(code, errors.New("synthetic camera request rejected"))
}
func nhf016CameraID(raw string) (domain.EntityID, error) {
	if raw == "" || len(raw) > 256 || !utf8.ValidString(raw) {
		return domain.EntityID{}, nhf016CameraError(connect.CodeInvalidArgument)
	}
	for _, c := range raw {
		if unicode.IsControl(c) {
			return domain.EntityID{}, nhf016CameraError(connect.CodeInvalidArgument)
		}
	}
	return domain.ParseEntityID(raw)
}
func nhf016KnownCameraMessage(value proto.Message) bool {
	if value == nil || !value.ProtoReflect().IsValid() || proto.Size(value) > 8192 {
		return false
	}
	var known func(protoreflect.Message, int) bool
	known = func(message protoreflect.Message, depth int) bool {
		if depth > 8 || len(message.GetUnknown()) != 0 {
			return false
		}
		valid := true
		message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			if field.IsMap() {
				valid = false
				return false
			}
			if field.Message() != nil {
				if field.IsList() {
					list := value.List()
					for i := 0; i < list.Len(); i++ {
						if !known(list.Get(i).Message(), depth+1) {
							valid = false
							return false
						}
					}
				} else {
					valid = known(value.Message(), depth+1)
				}
			}
			return valid
		})
		return valid
	}
	return known(value.ProtoReflect(), 0)
}
func nhf016CameraObject(entity domain.EntityID) (authorization.Object, error) {
	slot, err := authorization.NewCapabilitySlot(nhf016CameraSchema, nhf016CameraEndpoint)
	if err != nil {
		return authorization.Object{}, err
	}
	return authorization.EntityObject(entity, slot)
}
func nhf016CameraPermission() (authorization.Permission, error) {
	return authorization.NewPermission(av.ScopeFamily_SCOPE_FAMILY_MEDIA, av.ScopeOperation_SCOPE_OPERATION_READ)
}
func nhf016CameraPrincipal(ctx context.Context) (authorization.Principal, domain.ActorID, error) {
	p, err := authorization.PrincipalFromContext(ctx)
	if err != nil || !p.Native() {
		return authorization.Principal{}, domain.ActorID{}, nhf016CameraError(connect.CodePermissionDenied)
	}
	actor, err := p.Actor()
	transport, ok := ActorFromContext(ctx)
	if err != nil || !ok || actor != transport {
		return authorization.Principal{}, domain.ActorID{}, nhf016CameraError(connect.CodePermissionDenied)
	}
	return p, actor, nil
}
func nhf016CameraRPCError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return nhf016CameraError(connect.CodeCanceled)
	case errors.Is(err, context.DeadlineExceeded):
		return nhf016CameraError(connect.CodeDeadlineExceeded)
	case errors.Is(err, authorization.ErrUnavailable):
		return nhf016CameraError(connect.CodeUnavailable)
	case errors.Is(err, authorization.ErrDenied):
		return nhf016CameraError(connect.CodePermissionDenied)
	default:
		return err
	}
}

// Callback is bounded memory only, under authority -> mock locks. No network,
// decoding, randomness, SQL or recursive authority access is allowed here.
func (m *nhf016CameraMock) withCamera(ctx context.Context, raw string, fn func(authorization.Current, domain.ActorID, *nhf016CameraRecord) error) error {
	if m == nil || m.authority == nil {
		return nhf016CameraError(connect.CodeUnavailable)
	}
	entity, err := nhf016CameraID(raw)
	if err != nil {
		return err
	}
	p, actor, err := nhf016CameraPrincipal(ctx)
	if err != nil {
		return err
	}
	object, err := nhf016CameraObject(entity)
	if err != nil {
		return err
	}
	permission, err := nhf016CameraPermission()
	if err != nil {
		return err
	}
	err = m.authority.WithCurrent(ctx, p, func(current authorization.Current) error {
		if err := current.Authorize(permission, object); err != nil {
			return err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		record := m.records[entity]
		if record == nil {
			return authorization.ErrDenied
		}
		if m.offline {
			return nhf016CameraError(connect.CodeUnavailable)
		}
		return fn(current, actor, record)
	})
	return nhf016CameraRPCError(err)
}
func (m *nhf016CameraMock) permit(ctx context.Context, raw string) (*authorization.Permit, error) {
	entity, err := nhf016CameraID(raw)
	if err != nil {
		return nil, err
	}
	p, _, err := nhf016CameraPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	object, err := nhf016CameraObject(entity)
	if err != nil {
		return nil, err
	}
	permission, err := nhf016CameraPermission()
	if err != nil {
		return nil, err
	}
	permit, err := m.authority.Permit(ctx, p, permission, object)
	return permit, nhf016CameraRPCError(err)
}
func nhf016EndCameraLease(lease *nhf016CameraLease, state nhf016LeaseState) {
	if lease.state == nhf016LeaseActive {
		lease.state = state
		close(lease.done)
	}
}

// Caller holds both locks; a binding includes native session, account and rights.
// A foreign login cannot close or invalidate somebody else's still-valid lease.
func (m *nhf016CameraMock) session(current authorization.Current, actor domain.ActorID, ref *cv.SessionRef) (*nhf016CameraLease, nhf016SessionStatus, error) {
	_, binding, err := current.Binding()
	if err != nil {
		return nil, 0, err
	}
	var key [32]byte
	copy(key[:], ref.Reference)
	lease := m.leases[key]
	if lease == nil || lease.actor != actor || lease.binding != binding {
		return nil, nhf016SessionRevoked, nil
	}
	if lease.wire.Session.EntityId != ref.EntityId {
		return nil, nhf016SessionWrongCamera, nil
	}
	if !m.now().Before(lease.wire.ExpiresAt.AsTime()) {
		nhf016EndCameraLease(lease, nhf016LeaseExpired)
	}
	switch lease.state {
	case nhf016LeaseActive:
		return lease, nhf016SessionValid, nil
	case nhf016LeaseExpired:
		return lease, nhf016SessionExpired, nil
	case nhf016LeaseRevoked, nhf016LeaseClosed:
		return lease, nhf016SessionRevoked, nil
	default:
		return nil, 0, nhf016CameraError(connect.CodeInternal)
	}
}
func nhf016ValidCameraRef(value *cv.SessionRef) bool {
	return value != nil && len(value.Reference) == 32 && nhf016KnownCameraMessage(value)
}

func (m *nhf016CameraMock) ListCameras(ctx context.Context, request *connect.Request[cv.ListCamerasRequest]) (*connect.Response[cv.ListCamerasResponse], error) {
	if !nhf016KnownCameraMessage(request.Msg) {
		return nil, nhf016CameraError(connect.CodeInvalidArgument)
	}
	p, _, err := nhf016CameraPrincipal(ctx)
	if err != nil {
		return nil, err
	}
	permission, err := nhf016CameraPermission()
	if err != nil {
		return nil, err
	}
	response := new(cv.ListCamerasResponse)
	err = m.authority.WithCurrent(ctx, p, func(current authorization.Current) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.offline {
			return nhf016CameraError(connect.CodeUnavailable)
		}
		for entity, record := range m.records {
			object, err := nhf016CameraObject(entity)
			if err != nil {
				return err
			}
			if current.Authorize(permission, object) == nil {
				response.Cameras = append(response.Cameras, proto.Clone(record.metadata).(*cv.Camera))
			}
		}
		if len(response.Cameras) > 128 || proto.Size(response) > 64*1024 {
			return nhf016CameraError(connect.CodeResourceExhausted)
		}
		sort.Slice(response.Cameras, func(i, j int) bool { return response.Cameras[i].EntityId < response.Cameras[j].EntityId })
		return nil
	})
	if err != nil {
		return nil, nhf016CameraRPCError(err)
	}
	return connect.NewResponse(response), nil
}
func (m *nhf016CameraMock) OpenSession(ctx context.Context, request *connect.Request[cv.OpenSessionRequest]) (*connect.Response[cv.OpenSessionResponse], error) {
	if !nhf016KnownCameraMessage(request.Msg) {
		return nil, nhf016CameraError(connect.CodeInvalidArgument)
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	response := new(cv.OpenSessionResponse)
	err := m.withCamera(ctx, request.Msg.EntityId, func(current authorization.Current, actor domain.ActorID, record *nhf016CameraRecord) error {
		if record.metadata.Availability != cv.Availability_AVAILABILITY_AVAILABLE {
			response.Result = &cv.OpenSessionResponse_Unavailable{Unavailable: &emptypb.Empty{}}
			return nil
		}
		now := m.now()
		active := 0
		for _, lease := range m.leases {
			if !now.Before(lease.wire.ExpiresAt.AsTime()) {
				nhf016EndCameraLease(lease, nhf016LeaseExpired)
			}
			if lease.actor == actor && lease.state == nhf016LeaseActive {
				active++
			}
		}
		if active >= 8 {
			return nhf016CameraError(connect.CodeResourceExhausted)
		}
		if len(m.leases) >= 64 {
			var oldest [32]byte
			var selected *nhf016CameraLease
			for candidate, lease := range m.leases {
				if lease.state != nhf016LeaseActive && (selected == nil || lease.wire.ExpiresAt.AsTime().Before(selected.wire.ExpiresAt.AsTime())) {
					oldest, selected = candidate, lease
				}
			}
			if selected == nil {
				return nhf016CameraError(connect.CodeResourceExhausted)
			}
			delete(m.leases, oldest)
		}
		if m.lifetime <= 0 || m.lifetime > nhf016LeaseLifetime {
			return nhf016CameraError(connect.CodeInternal)
		}
		if _, exists := m.leases[key]; exists {
			return nhf016CameraError(connect.CodeResourceExhausted)
		}
		_, binding, err := current.Binding()
		if err != nil {
			return err
		}
		value := &cv.Lease{Session: &cv.SessionRef{EntityId: request.Msg.EntityId, Reference: bytes.Clone(key[:])}, IssuedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(m.lifetime)), EventRevision: record.revision}
		m.leases[key] = &nhf016CameraLease{wire: proto.Clone(value).(*cv.Lease), actor: actor, binding: binding, state: nhf016LeaseActive, done: make(chan struct{})}
		response.Result = &cv.OpenSessionResponse_Lease{Lease: value}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}
func (m *nhf016CameraMock) CloseSession(ctx context.Context, request *connect.Request[cv.CloseSessionRequest]) (*connect.Response[cv.CloseSessionResponse], error) {
	if !nhf016KnownCameraMessage(request.Msg) || !nhf016ValidCameraRef(request.Msg.Session) {
		return nil, nhf016CameraError(connect.CodeInvalidArgument)
	}
	err := m.withCamera(ctx, request.Msg.Session.EntityId, func(current authorization.Current, actor domain.ActorID, _ *nhf016CameraRecord) error {
		lease, status, err := m.session(current, actor, request.Msg.Session)
		if err != nil {
			return err
		}
		switch status {
		case nhf016SessionValid:
			nhf016EndCameraLease(lease, nhf016LeaseClosed)
		case nhf016SessionExpired, nhf016SessionRevoked, nhf016SessionWrongCamera:
		default:
			return nhf016CameraError(connect.CodeInternal)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&cv.CloseSessionResponse{}), nil
}
func (m *nhf016CameraMock) ReadImage(ctx context.Context, request *connect.Request[cv.ReadImageRequest]) (*connect.Response[cv.ReadImageResponse], error) {
	if !nhf016KnownCameraMessage(request.Msg) || request.Msg.Key == nil || !nhf016ValidCameraRef(request.Msg.Key.Session) {
		return nil, nhf016CameraError(connect.CodeInvalidArgument)
	}
	m.imageRequests.Add(1)
	m.activeReads.Add(1)
	defer m.activeReads.Add(-1)
	permit, err := m.permit(ctx, request.Msg.Key.Session.EntityId)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	m.mu.Lock()
	gate := m.imageGate
	m.mu.Unlock()
	if gate != nil {
		var expires time.Time
		var done <-chan struct{}
		err = m.withCamera(permit.Context(), request.Msg.Key.Session.EntityId, func(current authorization.Current, actor domain.ActorID, _ *nhf016CameraRecord) error {
			lease, status, err := m.session(current, actor, request.Msg.Key.Session)
			if err != nil {
				return err
			}
			if status == nhf016SessionValid {
				expires = lease.wire.ExpiresAt.AsTime()
				done = lease.done
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !expires.IsZero() {
			timer := time.NewTimer(time.Until(expires))
			select {
			case <-gate:
			case <-done:
			case <-timer.C:
			case <-permit.Context().Done():
				timer.Stop()
				return nil, nhf016CameraRPCError(context.Cause(permit.Context()))
			}
			timer.Stop()
		}
	}
	response := new(cv.ReadImageResponse)
	err = m.withCamera(permit.Context(), request.Msg.Key.Session.EntityId, func(current authorization.Current, actor domain.ActorID, record *nhf016CameraRecord) error {
		lease, status, err := m.session(current, actor, request.Msg.Key.Session)
		if err != nil {
			return err
		}
		switch status {
		case nhf016SessionExpired:
			response.Result = &cv.ReadImageResponse_Expired{Expired: &emptypb.Empty{}}
			return nil
		case nhf016SessionRevoked:
			response.Result = &cv.ReadImageResponse_Revoked{Revoked: &emptypb.Empty{}}
			return nil
		case nhf016SessionWrongCamera:
			response.Result = &cv.ReadImageResponse_WrongCamera{WrongCamera: &emptypb.Empty{}}
			return nil
		case nhf016SessionValid:
		default:
			return nhf016CameraError(connect.CodeInternal)
		}
		captured := m.now().Add(-time.Second)
		switch target := request.Msg.Key.Target.(type) {
		case *cv.ImageKey_Snapshot:
			if target.Snapshot == nil {
				return nhf016CameraError(connect.CodeInvalidArgument)
			}
		case *cv.ImageKey_EventId:
			if _, err := nhf016CameraID(target.EventId); err != nil {
				return err
			}
			var event *cv.Event
			for _, candidate := range record.events {
				if candidate.EventId == target.EventId {
					event = candidate
					break
				}
			}
			if event == nil || !event.HasImage {
				response.Result = &cv.ReadImageResponse_Missing{Missing: &emptypb.Empty{}}
				return nil
			}
			captured = event.CapturedAt.AsTime()
		default:
			return nhf016CameraError(connect.CodeInvalidArgument)
		}
		switch record.imageState {
		case nhf016ImageAvailable:
		case nhf016ImageMissing:
			response.Result = &cv.ReadImageResponse_Missing{Missing: &emptypb.Empty{}}
			return nil
		case nhf016ImageWrongCamera:
			response.Result = &cv.ReadImageResponse_WrongCamera{WrongCamera: &emptypb.Empty{}}
			return nil
		default:
			return nhf016CameraError(connect.CodeInternal)
		}
		response.Result = &cv.ReadImageResponse_Image{Image: &cv.Image{Key: proto.Clone(request.Msg.Key).(*cv.ImageKey), CapturedAt: timestamppb.New(captured), ExpiresAt: proto.Clone(lease.wire.ExpiresAt).(*timestamppb.Timestamp), Png: bytes.Clone(record.png)}}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}
func (m *nhf016CameraMock) cursor(ref *cv.SessionRef, revision uint64, index uint64) []byte {
	value := make([]byte, 8, 40)
	binary.BigEndian.PutUint64(value, index)
	mac := hmac.New(sha256.New, m.cursorKey[:])
	mac.Write(ref.Reference)
	mac.Write([]byte(ref.EntityId))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], revision)
	mac.Write(number[:])
	mac.Write(value)
	return append(value, mac.Sum(nil)...)
}
func (m *nhf016CameraMock) ListEvents(ctx context.Context, request *connect.Request[cv.ListEventsRequest]) (*connect.Response[cv.ListEventsResponse], error) {
	if !nhf016KnownCameraMessage(request.Msg) || !nhf016ValidCameraRef(request.Msg.Session) || request.Msg.PageSize == 0 || request.Msg.PageSize > 64 || len(request.Msg.Cursor) > 64 {
		return nil, nhf016CameraError(connect.CodeInvalidArgument)
	}
	response := new(cv.ListEventsResponse)
	err := m.withCamera(ctx, request.Msg.Session.EntityId, func(current authorization.Current, actor domain.ActorID, record *nhf016CameraRecord) error {
		_, status, err := m.session(current, actor, request.Msg.Session)
		if err != nil {
			return err
		}
		switch status {
		case nhf016SessionExpired:
			response.Result = &cv.ListEventsResponse_Expired{Expired: &emptypb.Empty{}}
			return nil
		case nhf016SessionRevoked:
			response.Result = &cv.ListEventsResponse_Revoked{Revoked: &emptypb.Empty{}}
			return nil
		case nhf016SessionWrongCamera:
			response.Result = &cv.ListEventsResponse_WrongCamera{WrongCamera: &emptypb.Empty{}}
			return nil
		case nhf016SessionValid:
		default:
			return nhf016CameraError(connect.CodeInternal)
		}
		index := uint64(0)
		if len(request.Msg.Cursor) != 0 {
			if len(request.Msg.Cursor) != 40 {
				response.Result = &cv.ListEventsResponse_ResyncRequired{ResyncRequired: &emptypb.Empty{}}
				return nil
			}
			index = binary.BigEndian.Uint64(request.Msg.Cursor[:8])
			if !hmac.Equal(request.Msg.Cursor, m.cursor(request.Msg.Session, record.revision, index)) || index >= uint64(len(record.events)) {
				response.Result = &cv.ListEventsResponse_ResyncRequired{ResyncRequired: &emptypb.Empty{}}
				return nil
			}
		}
		end := min(index+uint64(request.Msg.PageSize), uint64(len(record.events)))
		page := &cv.EventsPage{Session: proto.Clone(request.Msg.Session).(*cv.SessionRef), Revision: record.revision, Total: uint32(len(record.events))}
		for _, event := range record.events[index:end] {
			page.Events = append(page.Events, proto.Clone(event).(*cv.Event))
			if proto.Size(page) > 64*1024-64 {
				page.Events = page.Events[:len(page.Events)-1]
				break
			}
		}
		end = index + uint64(len(page.Events))
		if end == index && index < uint64(len(record.events)) {
			return nhf016CameraError(connect.CodeResourceExhausted)
		}
		if end < uint64(len(record.events)) {
			page.NextCursor = m.cursor(request.Msg.Session, record.revision, end)
		}
		if len(record.events) > 10000 || proto.Size(page) > 64*1024 {
			return nhf016CameraError(connect.CodeResourceExhausted)
		}
		response.Result = &cv.ListEventsResponse_Page{Page: page}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(response), nil
}
func (m *nhf016CameraMock) WatchSession(ctx context.Context, request *connect.Request[cv.WatchSessionRequest], stream *connect.ServerStream[cv.WatchSessionResponse]) error {
	if !nhf016KnownCameraMessage(request.Msg) || !nhf016ValidCameraRef(request.Msg.Session) {
		return nhf016CameraError(connect.CodeInvalidArgument)
	}
	permit, err := m.permit(ctx, request.Msg.Session.EntityId)
	if err != nil {
		return err
	}
	defer permit.Release()
	principal, _, err := nhf016CameraPrincipal(ctx)
	if err != nil {
		return err
	}
	subscription, err := m.authority.Subscribe(ctx, principal)
	if err != nil {
		return nhf016CameraRPCError(err)
	}
	defer subscription.Close()
	m.activeStreams.Add(1)
	defer m.activeStreams.Add(-1)
	var ownedLease *nhf016CameraLease
	defer func() {
		if ownedLease != nil {
			m.mu.Lock()
			nhf016EndCameraLease(ownedLease, nhf016LeaseClosed)
			m.mu.Unlock()
		}
	}()
	var sequence, lastRevision uint64
	for {
		var lease *cv.Lease
		var done, changed <-chan struct{}
		var status nhf016SessionStatus
		err = m.withCamera(permit.Context(), request.Msg.Session.EntityId, func(current authorization.Current, actor domain.ActorID, record *nhf016CameraRecord) error {
			currentLease, readStatus, err := m.session(current, actor, request.Msg.Session)
			if err != nil {
				return err
			}
			status = readStatus
			if readStatus == nhf016SessionValid && ownedLease == nil {
				ownedLease = currentLease
			}
			if currentLease != nil {
				lease = proto.Clone(currentLease.wire).(*cv.Lease)
				lease.EventRevision = record.revision
				done = currentLease.done
			}
			changed = record.changed
			return nil
		})
		if err != nil {
			return err
		}
		frame := &cv.WatchSessionResponse{Sequence: sequence + 1}
		switch status {
		case nhf016SessionExpired:
			frame.Frame = &cv.WatchSessionResponse_Expired{Expired: &emptypb.Empty{}}
		case nhf016SessionRevoked:
			frame.Frame = &cv.WatchSessionResponse_Revoked{Revoked: &emptypb.Empty{}}
		case nhf016SessionWrongCamera:
			return nhf016CameraError(connect.CodePermissionDenied)
		case nhf016SessionValid:
			if sequence == 0 {
				frame.Frame = &cv.WatchSessionResponse_Ready{Ready: lease}
			} else if lease.EventRevision > lastRevision {
				frame.Frame = &cv.WatchSessionResponse_Changed{Changed: &cv.EventsChanged{Session: proto.Clone(lease.Session).(*cv.SessionRef), Revision: lease.EventRevision}}
			}
		default:
			return nhf016CameraError(connect.CodeInternal)
		}
		if frame.Frame != nil {
			// Authorization cancellation is observed again before transport emission.
			if !permit.Ready() {
				return nhf016CameraError(connect.CodePermissionDenied)
			}
			if err = stream.Send(frame); err != nil {
				return err
			}
			sequence++
		}
		if status != nhf016SessionValid {
			return nil
		}
		lastRevision = lease.EventRevision
		timer := time.NewTimer(time.Until(lease.ExpiresAt.AsTime()))
		select {
		case <-permit.Context().Done():
			timer.Stop()
			return nhf016CameraRPCError(context.Cause(permit.Context()))
		case <-done:
			timer.Stop()
		case <-subscription.Changes():
			timer.Stop()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (m *nhf016CameraMock) register(plan *PublicPlan) error {
	routes := []struct {
		path string
		flow RouteFlow
	}{
		{camerasv1connect.CameraServiceListCamerasProcedure, RouteFlowUnary},
		{camerasv1connect.CameraServiceOpenSessionProcedure, RouteFlowUnary},
		{camerasv1connect.CameraServiceCloseSessionProcedure, RouteFlowUnary},
		{camerasv1connect.CameraServiceReadImageProcedure, RouteFlowUnary},
		{camerasv1connect.CameraServiceListEventsProcedure, RouteFlowUnary},
		{camerasv1connect.CameraServiceWatchSessionProcedure, RouteFlowServerStream},
	}
	procedures := make([]ConnectProcedure, 0, len(routes))
	for _, route := range routes {
		procedures = append(procedures, ConnectProcedure{Path: route.path, Access: RouteAccessProtected, Flow: route.flow, Transport: RouteTransportTLSRequired})
	}
	if err := plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
		return camerasv1connect.NewCameraServiceHandler(m, append(options, connect.WithReadMaxBytes(8192))...)
	}, procedures...); err != nil {
		return err
	}
	permission, err := nhf016CameraPermission()
	if err != nil {
		return err
	}
	for _, route := range routes {
		if err = plan.BindPermission(RouteKindConnect, RouteMethodPost, route.path, permission); err != nil {
			return err
		}
	}
	return nil
}
