package server

// Test-only. Actual native authentication and companion ownership; synthetic
// messages/resource permissions/provider outcomes, never registered by Run.
import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	companion "github.com/damione1/Homeward/gen/home/companion/v1"
	wire "github.com/damione1/Homeward/gen/home/notifications/v1"
	"github.com/damione1/Homeward/gen/home/notifications/v1/notificationsv1connect"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nhf022DestinationID string
type nhf022DeliveryID string
type nhf022RequestID string
type nhf022DestinationRecord struct {
	owner         nhf021Owner
	value         *wire.Destination
	entries       map[nhf022DeliveryID]*wire.Delivery
	inboxRevision uint64
}
type nhf022Counts struct {
	Registrations uint64 `json:"registrations"`
	Updates       uint64 `json:"updates"`
	Receipts      uint64 `json:"receipts"`
	Revocations   uint64 `json:"revocations"`
	Seeded        uint64 `json:"seeded"`
	Opens         uint64 `json:"opens"`
	LostReplies   uint64 `json:"lost_replies"`
}
type nhf022Notifications struct {
	notificationsv1connect.UnimplementedNotificationServiceHandler
	parent         *nhf021Companion
	mu             sync.Mutex
	records        map[nhf022DestinationID]*nhf022DestinationRecord
	counts         nhf022Counts
	targetReadable bool
	targetID       string
}

func nhf022NewNotifications(parent *nhf021Companion) *nhf022Notifications {
	h := &nhf022Notifications{parent: parent, records: make(map[nhf022DestinationID]*nhf022DestinationRecord), targetReadable: true, targetID: nhf021Opaque()}
	// Installed before serving, no SQL/network/OS calls in the subscriber.
	if parent.onRetire != nil {
		panic("notification retirement subscriber already installed")
	}
	parent.onRetire = func(id nhf021InstallationID, state companion.InstallationState) {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, record := range h.records {
			if nhf021InstallationID(record.value.InstallationId.Value) != id {
				continue
			}
			next := wire.DestinationState_DESTINATION_STATE_REVOKED
			if state == companion.InstallationState_INSTALLATION_STATE_EXPIRED {
				next = wire.DestinationState_DESTINATION_STATE_EXPIRED
			}
			h.retireLocked(record, next)
		}
	}
	return h
}

type nhf022Fault struct{}

func (*nhf022Fault) Error() string         { return "synthetic notification request rejected" }
func nhf022Error(code connect.Code) error  { return connect.NewError(code, &nhf022Fault{}) }
func nhf022Known(value proto.Message) bool { return nhf021KnownLimit(value, 128*1024) }
func nhf022Revision(value uint64) bool     { return value > 0 && value < math.MaxUint64 }
func nhf022Stamp(value *timestamppb.Timestamp) bool {
	return value != nil && value.Seconds >= 1 && value.CheckValid() == nil
}
func nhf022Text(value string, maximum int, multiline bool) bool {
	return strings.TrimSpace(value) != "" && len(value) <= maximum && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, func(c rune) bool { return unicode.IsControl(c) && !(multiline && c == '\n') })
}
func nhf022Permission(v *wire.PermissionSnapshot) bool {
	if v == nil || !nhf022Known(v) || v.Permission == wire.Permission_PERMISSION_UNSPECIFIED ||
		v.Alerts == wire.Setting_SETTING_UNSPECIFIED || v.NotificationCenter == wire.Setting_SETTING_UNSPECIFIED {
		return false
	}
	noSurface := v.Alerts == wire.Setting_SETTING_UNSUPPORTED && v.NotificationCenter == wire.Setting_SETTING_UNSUPPORTED
	switch v.Source {
	case wire.PermissionSource_PERMISSION_SOURCE_MAC_OS_USER_NOTIFICATIONS:
		return true
	case wire.PermissionSource_PERMISSION_SOURCE_BUNDLE_REQUIRED, wire.PermissionSource_PERMISSION_SOURCE_UNSUPPORTED:
		return v.Permission == wire.Permission_PERMISSION_UNKNOWN && noSurface
	case wire.PermissionSource_PERMISSION_SOURCE_ANDROID_PENDING_QUALIFICATION:
		return (v.Permission == wire.Permission_PERMISSION_NOT_REQUESTED || v.Permission == wire.Permission_PERMISSION_UNKNOWN) && noSurface
	case wire.PermissionSource_PERMISSION_SOURCE_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func nhf022PermissionPlatform(platform companion.Platform, v *wire.PermissionSnapshot) bool {
	if !nhf022Permission(v) {
		return false
	}
	switch platform {
	case companion.Platform_PLATFORM_MAC_OS:
		return v.Source == wire.PermissionSource_PERMISSION_SOURCE_MAC_OS_USER_NOTIFICATIONS ||
			v.Source == wire.PermissionSource_PERMISSION_SOURCE_BUNDLE_REQUIRED || v.Source == wire.PermissionSource_PERMISSION_SOURCE_UNSUPPORTED
	case companion.Platform_PLATFORM_ANDROID:
		return v.Source == wire.PermissionSource_PERMISSION_SOURCE_ANDROID_PENDING_QUALIFICATION || v.Source == wire.PermissionSource_PERMISSION_SOURCE_UNSUPPORTED
	case companion.Platform_PLATFORM_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func nhf022CanPresent(v *wire.PermissionSnapshot) bool {
	if !nhf022Permission(v) || v.Source != wire.PermissionSource_PERMISSION_SOURCE_MAC_OS_USER_NOTIFICATIONS {
		return false
	}
	permitted := v.Permission == wire.Permission_PERMISSION_AUTHORIZED || v.Permission == wire.Permission_PERMISSION_PROVISIONAL || v.Permission == wire.Permission_PERMISSION_EPHEMERAL
	return permitted && (v.Alerts == wire.Setting_SETTING_ENABLED || v.NotificationCenter == wire.Setting_SETTING_ENABLED)
}
func (h *nhf022Notifications) retireLocked(record *nhf022DestinationRecord, state wire.DestinationState) {
	if record.value.State != wire.DestinationState_DESTINATION_STATE_ACTIVE {
		return
	}
	record.value.State = state
	record.value.NativeDelivery = false
	if record.value.Revision < math.MaxUint64-1 {
		record.value.Revision++
	}
	record.entries = make(map[nhf022DeliveryID]*wire.Delivery)
	if record.inboxRevision < math.MaxUint64-1 {
		record.inboxRevision++
	}
}
func (h *nhf022Notifications) collectLocked(now time.Time) {
	for id, record := range h.records {
		if !now.Before(record.value.ExpiresAt.AsTime()) {
			h.retireLocked(record, wire.DestinationState_DESTINATION_STATE_EXPIRED)
		}
		for id, entry := range record.entries {
			if !now.Before(entry.ExpiresAt.AsTime()) {
				delete(record.entries, id)
				if record.inboxRevision < math.MaxUint64-1 {
					record.inboxRevision++
				}
			}
		}
		if now.After(record.value.ExpiresAt.AsTime().Add(30 * time.Minute)) {
			delete(h.records, id)
		}
	}
}

// Actual authority -> companion -> notification lock order. Memory only.
func (h *nhf022Notifications) withNative(ctx context.Context, fn func(nhf021Owner, time.Time) error) error {
	return h.parent.withNative(ctx, func(owner nhf021Owner, now time.Time) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.collectLocked(now)
		return fn(owner, now)
	})
}
func (h *nhf022Notifications) own(owner nhf021Owner, id *wire.DestinationId) (*nhf022DestinationRecord, error) {
	if id == nil || !nhf021ID(id.Value) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	record := h.records[nhf022DestinationID(id.Value)]
	if record == nil || record.owner != owner {
		return nil, nhf022Error(connect.CodeNotFound)
	}
	return record, nil
}
func (h *nhf022Notifications) active(owner nhf021Owner, id *wire.DestinationId) (*nhf022DestinationRecord, error) {
	record, err := h.own(owner, id)
	if err != nil {
		return nil, err
	}
	installation, err := h.parent.own(owner, record.value.InstallationId)
	if err != nil {
		return nil, err
	}
	if record.value.State != wire.DestinationState_DESTINATION_STATE_ACTIVE ||
		installation.State != companion.InstallationState_INSTALLATION_STATE_ACTIVE {
		return nil, nhf022Error(connect.CodePermissionDenied)
	}
	return record, nil
}
func (h *nhf022Notifications) expected(value *companion.Context) error {
	if value == nil || !proto.Equal(value, h.parent.context()) {
		return nhf022Error(connect.CodeFailedPrecondition)
	}
	return nil
}
func (h *nhf022Notifications) DescribeNotifications(ctx context.Context, r *connect.Request[wire.DescribeNotificationsRequest]) (*connect.Response[wire.DescribeNotificationsResponse], error) {
	if !nhf022Known(r.Msg) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	var result *wire.DescribeNotificationsResponse
	err := h.withNative(ctx, func(_ nhf021Owner, _ time.Time) error {
		result = &wire.DescribeNotificationsResponse{Context: h.parent.context(), Foreground: wire.ForegroundTransport_FOREGROUND_TRANSPORT_NATIVE_SESSION_LAN_POLL,
			MacOsBackground:    wire.BackgroundTransport_BACKGROUND_TRANSPORT_UNAVAILABLE,
			AndroidBackground:  wire.BackgroundTransport_BACKGROUND_TRANSPORT_ANDROID_FOREGROUND_SERVICE_PENDING_QUALIFICATION,
			MinimumPollSeconds: 5, MaximumMessageLifetimeSeconds: 3600, MaximumInboxEntries: 64}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(result), nil
}
func (h *nhf022Notifications) RegisterDestination(ctx context.Context, r *connect.Request[wire.RegisterDestinationRequest]) (*connect.Response[wire.RegisterDestinationResponse], error) {
	if !nhf022Known(r.Msg) || !nhf021ID(r.Msg.RequestId) || !nhf022Permission(r.Msg.Permission) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	id := nhf022DestinationID(nhf021Opaque()) // Outside authority/memory locks.
	var result *wire.Destination
	err := h.withNative(ctx, func(owner nhf021Owner, now time.Time) error {
		if err := h.expected(r.Msg.ExpectedContext); err != nil {
			return err
		}
		installation, err := h.parent.own(owner, r.Msg.InstallationId)
		if err != nil {
			return err
		}
		if installation.State != companion.InstallationState_INSTALLATION_STATE_ACTIVE {
			return nhf022Error(connect.CodePermissionDenied)
		}
		if !nhf022PermissionPlatform(installation.Device.Platform, r.Msg.Permission) {
			return nhf022Error(connect.CodeInvalidArgument)
		}
		if len(h.records) >= 256 {
			return nhf022Error(connect.CodeResourceExhausted)
		}
		if _, exists := h.records[id]; exists {
			return nhf022Error(connect.CodeResourceExhausted)
		}
		for _, record := range h.records {
			if record.owner != owner {
				continue
			}
			if record.value.RegistrationRequestId == r.Msg.RequestId ||
				(record.value.State == wire.DestinationState_DESTINATION_STATE_ACTIVE && proto.Equal(record.value.InstallationId, r.Msg.InstallationId)) {
				return nhf022Error(connect.CodeAlreadyExists)
			}
		}
		deadline := now.Add(30 * time.Minute)
		if installation.ExpiresAt.AsTime().Before(deadline) {
			deadline = installation.ExpiresAt.AsTime()
		}
		value := &wire.Destination{Id: &wire.DestinationId{Value: string(id)}, InstallationId: proto.Clone(installation.Id).(*companion.InstallationId),
			DeviceId: proto.Clone(installation.DeviceId).(*companion.DeviceId), AccountId: &auth.AccountId{Value: owner.account},
			SessionId: &auth.SessionId{Value: owner.session}, Platform: installation.Device.Platform,
			State: wire.DestinationState_DESTINATION_STATE_ACTIVE, Revision: 1, NativeDelivery: false,
			Permission: proto.Clone(r.Msg.Permission).(*wire.PermissionSnapshot), ExpiresAt: timestamppb.New(deadline),
			LastRequestId: r.Msg.RequestId, RegistrationRequestId: r.Msg.RequestId}
		h.records[id] = &nhf022DestinationRecord{owner: owner, value: value, entries: make(map[nhf022DeliveryID]*wire.Delivery), inboxRevision: 1}
		h.counts.Registrations++
		result = proto.Clone(value).(*wire.Destination)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.RegisterDestinationResponse{Context: h.parent.context(), Destination: result}), nil
}
func (h *nhf022Notifications) GetDestination(ctx context.Context, r *connect.Request[wire.GetDestinationRequest]) (*connect.Response[wire.GetDestinationResponse], error) {
	if !nhf022Known(r.Msg) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	var result *wire.Destination
	err := h.withNative(ctx, func(owner nhf021Owner, _ time.Time) error {
		var record *nhf022DestinationRecord
		switch selector := r.Msg.Selector.(type) {
		case *wire.GetDestinationRequest_Id:
			var err error
			record, err = h.own(owner, selector.Id)
			if err != nil {
				return err
			}
		case *wire.GetDestinationRequest_RegistrationRequestId:
			if !nhf021ID(selector.RegistrationRequestId) {
				return nhf022Error(connect.CodeInvalidArgument)
			}
			for _, candidate := range h.records {
				if candidate.owner == owner && candidate.value.RegistrationRequestId == selector.RegistrationRequestId {
					record = candidate
					break
				}
			}
			if record == nil {
				return nhf022Error(connect.CodeNotFound)
			}
		default:
			return nhf022Error(connect.CodeInvalidArgument)
		}
		result = proto.Clone(record.value).(*wire.Destination)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.GetDestinationResponse{Context: h.parent.context(), Destination: result}), nil
}
func (h *nhf022Notifications) UpdateDestination(ctx context.Context, r *connect.Request[wire.UpdateDestinationRequest]) (*connect.Response[wire.UpdateDestinationResponse], error) {
	if !nhf022Known(r.Msg) || !nhf021ID(r.Msg.RequestId) || !nhf022Revision(r.Msg.ExpectedRevision) || !nhf022Permission(r.Msg.Permission) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	var result *wire.Destination
	err := h.withNative(ctx, func(owner nhf021Owner, _ time.Time) error {
		if err := h.expected(r.Msg.ExpectedContext); err != nil {
			return err
		}
		record, err := h.active(owner, r.Msg.Id)
		if err != nil {
			return err
		}
		if record.value.Revision != r.Msg.ExpectedRevision {
			return nhf022Error(connect.CodeAborted)
		}
		if record.value.Revision >= math.MaxUint64-1 {
			return nhf022Error(connect.CodeResourceExhausted)
		}
		if !nhf022PermissionPlatform(record.value.Platform, r.Msg.Permission) || (r.Msg.NativeDelivery && !nhf022CanPresent(r.Msg.Permission)) {
			return nhf022Error(connect.CodeInvalidArgument)
		}
		record.value.NativeDelivery = r.Msg.NativeDelivery
		record.value.Permission = proto.Clone(r.Msg.Permission).(*wire.PermissionSnapshot)
		record.value.LastRequestId = r.Msg.RequestId
		record.value.Revision++
		h.counts.Updates++
		result = proto.Clone(record.value).(*wire.Destination)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.UpdateDestinationResponse{Context: h.parent.context(), Destination: result}), nil
}
func (h *nhf022Notifications) ReadInbox(ctx context.Context, r *connect.Request[wire.ReadInboxRequest]) (*connect.Response[wire.ReadInboxResponse], error) {
	if !nhf022Known(r.Msg) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	var result *wire.ReadInboxResponse
	err := h.withNative(ctx, func(owner nhf021Owner, now time.Time) error {
		record, err := h.active(owner, r.Msg.Id)
		if err != nil {
			return err
		}
		result = &wire.ReadInboxResponse{Context: h.parent.context(), Destination: proto.Clone(record.value).(*wire.Destination), Revision: record.inboxRevision}
		for _, entry := range record.entries {
			if entry.ReadState == wire.ReadState_READ_STATE_DISMISSED || now.Before(entry.CreatedAt.AsTime()) {
				continue
			}
			result.Entries = append(result.Entries, proto.Clone(entry).(*wire.Delivery))
		}
		sort.Slice(result.Entries, func(a, b int) bool {
			x, y := result.Entries[a], result.Entries[b]
			if x.CreatedAt.AsTime().Equal(y.CreatedAt.AsTime()) {
				return x.Id.Value < y.Id.Value
			}
			return x.CreatedAt.AsTime().Before(y.CreatedAt.AsTime())
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(result), nil
}
func (h *nhf022Notifications) entry(owner nhf021Owner, destination *wire.DestinationId, id *wire.DeliveryId) (*nhf022DestinationRecord, *wire.Delivery, error) {
	record, err := h.active(owner, destination)
	if err != nil {
		return nil, nil, err
	}
	if id == nil || !nhf021ID(id.Value) {
		return nil, nil, nhf022Error(connect.CodeInvalidArgument)
	}
	entry := record.entries[nhf022DeliveryID(id.Value)]
	if entry == nil {
		return nil, nil, nhf022Error(connect.CodeNotFound)
	}
	return record, entry, nil
}
func (h *nhf022Notifications) GetDelivery(ctx context.Context, r *connect.Request[wire.GetDeliveryRequest]) (*connect.Response[wire.GetDeliveryResponse], error) {
	if !nhf022Known(r.Msg) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	var result *wire.Delivery
	err := h.withNative(ctx, func(owner nhf021Owner, _ time.Time) error {
		_, entry, err := h.entry(owner, r.Msg.DestinationId, r.Msg.DeliveryId)
		if err != nil {
			return err
		}
		result = proto.Clone(entry).(*wire.Delivery)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.GetDeliveryResponse{Context: h.parent.context(), Delivery: result}), nil
}
func (h *nhf022Notifications) ResolveOpen(ctx context.Context, r *connect.Request[wire.ResolveOpenRequest]) (*connect.Response[wire.ResolveOpenResponse], error) {
	if !nhf022Known(r.Msg) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	var result *wire.ResolveOpenResponse
	err := h.withNative(ctx, func(owner nhf021Owner, now time.Time) error {
		record, entry, err := h.entry(owner, r.Msg.DestinationId, r.Msg.DeliveryId)
		if err != nil {
			return err
		}
		if now.Before(entry.CreatedAt.AsTime()) || entry.ReadState == wire.ReadState_READ_STATE_DISMISSED ||
			entry.TransportAcceptance != wire.TransportAcceptance_TRANSPORT_ACCEPTANCE_ACCEPTED_FOREGROUND_LAN {
			return nhf022Error(connect.CodePermissionDenied)
		}
		switch target := entry.Target.Resource.(type) {
		case *wire.Target_Home:
			if target.Home == nil {
				return nhf022Error(connect.CodePermissionDenied)
			}
		case *wire.Target_Entity:
			if !h.targetReadable || target.Entity == nil || target.Entity.Value != h.targetID {
				return nhf022Error(connect.CodePermissionDenied)
			}
		default:
			return nhf022Error(connect.CodePermissionDenied)
		}
		deadline := now.Add(5 * time.Second)
		if record.value.ExpiresAt.AsTime().Before(deadline) {
			deadline = record.value.ExpiresAt.AsTime()
		}
		if entry.ExpiresAt.AsTime().Before(deadline) {
			deadline = entry.ExpiresAt.AsTime()
		}
		result = &wire.ResolveOpenResponse{Context: h.parent.context(), DestinationId: proto.Clone(record.value.Id).(*wire.DestinationId),
			DeliveryId: proto.Clone(entry.Id).(*wire.DeliveryId), AuthorizedTarget: proto.Clone(entry.Target).(*wire.Target), ValidUntil: timestamppb.New(deadline)}
		h.counts.Opens++
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(result), nil
}
func (h *nhf022Notifications) RevokeDestination(ctx context.Context, r *connect.Request[wire.RevokeDestinationRequest]) (*connect.Response[wire.RevokeDestinationResponse], error) {
	if !nhf022Known(r.Msg) || !nhf021ID(r.Msg.RequestId) || !nhf022Revision(r.Msg.ExpectedRevision) {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	var result *wire.Destination
	err := h.withNative(ctx, func(owner nhf021Owner, _ time.Time) error {
		if err := h.expected(r.Msg.ExpectedContext); err != nil {
			return err
		}
		record, err := h.active(owner, r.Msg.Id)
		if err != nil {
			return err
		}
		if record.value.Revision != r.Msg.ExpectedRevision {
			return nhf022Error(connect.CodeAborted)
		}
		if record.value.Revision >= math.MaxUint64-1 {
			return nhf022Error(connect.CodeResourceExhausted)
		}
		h.retireLocked(record, wire.DestinationState_DESTINATION_STATE_REVOKED)
		record.value.LastRequestId = r.Msg.RequestId
		h.counts.Revocations++
		result = proto.Clone(record.value).(*wire.Destination)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.RevokeDestinationResponse{Context: h.parent.context(), Destination: result}), nil
}
func nhf022PresentationAdvance(prior, next wire.Presentation) bool {
	switch prior {
	case wire.Presentation_PRESENTATION_NOT_ATTEMPTED:
		return true
	case wire.Presentation_PRESENTATION_OS_ACCEPTED:
		return next == wire.Presentation_PRESENTATION_OS_ACCEPTED || next == wire.Presentation_PRESENTATION_PRESENT_IN_OS_CENTER
	case wire.Presentation_PRESENTATION_OUTCOME_UNKNOWN:
		return next == wire.Presentation_PRESENTATION_OUTCOME_UNKNOWN || next == wire.Presentation_PRESENTATION_OS_ACCEPTED || next == wire.Presentation_PRESENTATION_PRESENT_IN_OS_CENTER
	case wire.Presentation_PRESENTATION_PRESENT_IN_OS_CENTER, wire.Presentation_PRESENTATION_PERMISSION_DENIED, wire.Presentation_PRESENTATION_UNAVAILABLE:
		return prior == next
	case wire.Presentation_PRESENTATION_UNSPECIFIED:
		return false
	default:
		return false
	}
}
func (h *nhf022Notifications) AcknowledgeDelivery(ctx context.Context, r *connect.Request[wire.AcknowledgeDeliveryRequest]) (*connect.Response[wire.AcknowledgeDeliveryResponse], error) {
	if !nhf022Known(r.Msg) || !nhf021ID(r.Msg.RequestId) || !nhf022Revision(r.Msg.ExpectedRevision) ||
		!nhf022Stamp(r.Msg.ObservedAt) || r.Msg.Kind == wire.ReceiptKind_RECEIPT_KIND_UNSPECIFIED {
		return nil, nhf022Error(connect.CodeInvalidArgument)
	}
	var result *wire.Delivery
	err := h.withNative(ctx, func(owner nhf021Owner, now time.Time) error {
		if err := h.expected(r.Msg.ExpectedContext); err != nil {
			return err
		}
		record, entry, err := h.entry(owner, r.Msg.DestinationId, r.Msg.DeliveryId)
		if err != nil {
			return err
		}
		if entry.Revision != r.Msg.ExpectedRevision {
			return nhf022Error(connect.CodeAborted)
		}
		if entry.Revision >= math.MaxUint64-1 || record.inboxRevision >= math.MaxUint64-1 {
			return nhf022Error(connect.CodeResourceExhausted)
		}
		observed := r.Msg.ObservedAt.AsTime()
		if observed.Before(entry.CreatedAt.AsTime()) || !observed.Before(entry.ExpiresAt.AsTime()) || observed.After(now.Add(5*time.Second)) {
			return nhf022Error(connect.CodeFailedPrecondition)
		}
		next := proto.Clone(entry).(*wire.Delivery)
		switch r.Msg.Kind {
		case wire.ReceiptKind_RECEIPT_KIND_RECEIVED:
			if entry.Reception != wire.Reception_RECEPTION_NOT_RECEIVED {
				return nhf022Error(connect.CodeFailedPrecondition)
			}
			next.Reception = wire.Reception_RECEPTION_RECEIVED
		case wire.ReceiptKind_RECEIPT_KIND_READ, wire.ReceiptKind_RECEIPT_KIND_DISMISSED:
			if entry.Reception != wire.Reception_RECEPTION_RECEIVED || entry.ReadState == wire.ReadState_READ_STATE_DISMISSED {
				return nhf022Error(connect.CodeFailedPrecondition)
			}
			if r.Msg.Kind == wire.ReceiptKind_RECEIPT_KIND_READ {
				if entry.ReadState == wire.ReadState_READ_STATE_READ {
					return nhf022Error(connect.CodeFailedPrecondition)
				}
				next.ReadState = wire.ReadState_READ_STATE_READ
			} else {
				next.ReadState = wire.ReadState_READ_STATE_DISMISSED
			}
		case wire.ReceiptKind_RECEIPT_KIND_OS_ACCEPTED, wire.ReceiptKind_RECEIPT_KIND_PRESENT_IN_OS_CENTER, wire.ReceiptKind_RECEIPT_KIND_OUTCOME_UNKNOWN,
			wire.ReceiptKind_RECEIPT_KIND_PERMISSION_DENIED, wire.ReceiptKind_RECEIPT_KIND_UNAVAILABLE:
			if entry.Reception != wire.Reception_RECEPTION_RECEIVED || entry.ReadState == wire.ReadState_READ_STATE_DISMISSED {
				return nhf022Error(connect.CodeFailedPrecondition)
			}
			switch r.Msg.Kind {
			case wire.ReceiptKind_RECEIPT_KIND_OS_ACCEPTED:
				next.Presentation = wire.Presentation_PRESENTATION_OS_ACCEPTED
			case wire.ReceiptKind_RECEIPT_KIND_PRESENT_IN_OS_CENTER:
				next.Presentation = wire.Presentation_PRESENTATION_PRESENT_IN_OS_CENTER
			case wire.ReceiptKind_RECEIPT_KIND_OUTCOME_UNKNOWN:
				next.Presentation = wire.Presentation_PRESENTATION_OUTCOME_UNKNOWN
			case wire.ReceiptKind_RECEIPT_KIND_PERMISSION_DENIED:
				next.Presentation = wire.Presentation_PRESENTATION_PERMISSION_DENIED
			case wire.ReceiptKind_RECEIPT_KIND_UNAVAILABLE:
				next.Presentation = wire.Presentation_PRESENTATION_UNAVAILABLE
			case wire.ReceiptKind_RECEIPT_KIND_UNSPECIFIED, wire.ReceiptKind_RECEIPT_KIND_RECEIVED, wire.ReceiptKind_RECEIPT_KIND_READ, wire.ReceiptKind_RECEIPT_KIND_DISMISSED:
				return nhf022Error(connect.CodeInvalidArgument)
			default:
				return nhf022Error(connect.CodeInvalidArgument)
			}
			if !nhf022PresentationAdvance(entry.Presentation, next.Presentation) {
				return nhf022Error(connect.CodeFailedPrecondition)
			}
			if next.Presentation == wire.Presentation_PRESENTATION_OS_ACCEPTED || next.Presentation == wire.Presentation_PRESENTATION_PRESENT_IN_OS_CENTER ||
				next.Presentation == wire.Presentation_PRESENTATION_OUTCOME_UNKNOWN {
				if !record.value.NativeDelivery || !nhf022CanPresent(record.value.Permission) ||
					entry.TransportAcceptance != wire.TransportAcceptance_TRANSPORT_ACCEPTANCE_ACCEPTED_FOREGROUND_LAN {
					return nhf022Error(connect.CodePermissionDenied)
				}
			}
		case wire.ReceiptKind_RECEIPT_KIND_UNSPECIFIED:
			return nhf022Error(connect.CodeInvalidArgument)
		default:
			return nhf022Error(connect.CodeInvalidArgument)
		}
		next.Revision++
		next.LastRequestId = r.Msg.RequestId
		record.entries[nhf022DeliveryID(next.Id.Value)] = next
		record.inboxRevision++
		h.counts.Receipts++
		result = proto.Clone(next).(*wire.Delivery)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.AcknowledgeDeliveryResponse{Context: h.parent.context(), Delivery: result}), nil
}

type nhf022SeedCase uint8

const (
	nhf022SeedNominal nhf022SeedCase = iota + 1
	nhf022SeedEntity
	nhf022SeedExpiring
)

// Private fixture controller only, not a public send API. Precompute identity
// entropy, then perform bounded memory updates under companion -> notification.
func (h *nhf022Notifications) seed(caseValue nhf022SeedCase) error {
	ids := []string{nhf021Opaque(), nhf021Opaque(), nhf021Opaque()}
	h.parent.mu.Lock()
	defer h.parent.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.parent.now()
	h.collectLocked(now)
	var recipient *nhf022DestinationRecord
	for _, record := range h.records {
		if record.value.State == wire.DestinationState_DESTINATION_STATE_ACTIVE {
			if recipient != nil {
				return nhf022Error(connect.CodeFailedPrecondition)
			}
			recipient = record
		}
	}
	if recipient == nil || len(recipient.entries) > 61 {
		return nhf022Error(connect.CodeResourceExhausted)
	}
	fresh := func(id, title, body string, target *wire.Target) *wire.Delivery {
		return &wire.Delivery{Id: &wire.DeliveryId{Value: id}, DestinationId: proto.Clone(recipient.value.Id).(*wire.DestinationId), Revision: 1,
			Title: title, Body: body, CreatedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(10 * time.Minute)), Target: target,
			TransportAcceptance: wire.TransportAcceptance_TRANSPORT_ACCEPTANCE_ACCEPTED_FOREGROUND_LAN, TransportReason: wire.TransportReason_TRANSPORT_REASON_NONE,
			Reception: wire.Reception_RECEPTION_NOT_RECEIVED, Presentation: wire.Presentation_PRESENTATION_NOT_ATTEMPTED, ReadState: wire.ReadState_READ_STATE_UNREAD}
	}
	home := func() *wire.Target { return &wire.Target{Resource: &wire.Target_Home{Home: &emptypb.Empty{}}} }
	entries := []*wire.Delivery{}
	switch caseValue {
	case nhf022SeedNominal:
		entries = append(entries, fresh(ids[0], "Notification synthétique", "Message privé de cette installation, via le serveur isolé.", home()))
		failed := fresh(ids[1], "Remise fournisseur refusée — synthétique", "Le refus reste distinct d’une acceptation ou d’un affichage OS.", home())
		failed.TransportAcceptance = wire.TransportAcceptance_TRANSPORT_ACCEPTANCE_PROVIDER_REJECTED
		failed.TransportReason = wire.TransportReason_TRANSPORT_REASON_PROVIDER_FAILURE
		entries = append(entries, failed)
		expired := fresh(ids[2], "Message déjà expiré", "Ne doit jamais être présenté.", home())
		expired.CreatedAt = timestamppb.New(now.Add(-2 * time.Minute))
		expired.ExpiresAt = timestamppb.New(now.Add(-time.Second))
		entries = append(entries, expired)
	case nhf022SeedEntity:
		entries = append(entries, fresh(ids[0], "Ressource synthétique", "L’ouverture demande une nouvelle admission de lecture.",
			&wire.Target{Resource: &wire.Target_Entity{Entity: &registry.EntityId{Value: h.targetID}}}))
	case nhf022SeedExpiring:
		entry := fresh(ids[0], "Message synthétique bref", "Expire après deux secondes ; aucun rattrapage.", home())
		entry.ExpiresAt = timestamppb.New(now.Add(2 * time.Second))
		entries = append(entries, entry)
	default:
		return nhf022Error(connect.CodeInvalidArgument)
	}
	if recipient.inboxRevision >= math.MaxUint64-1 {
		return nhf022Error(connect.CodeResourceExhausted)
	}
	for _, entry := range entries {
		if !nhf022Text(entry.Title, 128, false) || !nhf022Text(entry.Body, 1024, true) {
			return nhf022Error(connect.CodeInvalidArgument)
		}
		if _, exists := recipient.entries[nhf022DeliveryID(entry.Id.Value)]; exists {
			return nhf022Error(connect.CodeResourceExhausted)
		}
	}
	for _, entry := range entries {
		recipient.entries[nhf022DeliveryID(entry.Id.Value)] = entry
		h.counts.Seeded++
	}
	recipient.inboxRevision++
	h.collectLocked(now)
	return nil
}

var _ notificationsv1connect.NotificationServiceHandler = (*nhf022Notifications)(nil)

func TestNHF022PermissionAndReceiptAdmission(t *testing.T) {
	granted := &wire.PermissionSnapshot{Permission: wire.Permission_PERMISSION_AUTHORIZED, Alerts: wire.Setting_SETTING_ENABLED,
		NotificationCenter: wire.Setting_SETTING_ENABLED, Source: wire.PermissionSource_PERMISSION_SOURCE_MAC_OS_USER_NOTIFICATIONS}
	if !nhf022CanPresent(granted) {
		t.Fatal("valid explicit permission rejected")
	}
	android := proto.Clone(granted).(*wire.PermissionSnapshot)
	android.Source = wire.PermissionSource_PERMISSION_SOURCE_ANDROID_PENDING_QUALIFICATION
	if nhf022Permission(android) || nhf022CanPresent(android) {
		t.Fatal("unqualified Android permission became a grant")
	}
	android.Permission = wire.Permission_PERMISSION_NOT_REQUESTED
	android.Alerts = wire.Setting_SETTING_UNSUPPORTED
	android.NotificationCenter = wire.Setting_SETTING_UNSUPPORTED
	if !nhf022Permission(android) || nhf022CanPresent(android) {
		t.Fatal("pending capability not represented honestly")
	}
	granted.Permission = wire.Permission(999)
	if nhf022Permission(granted) {
		t.Fatal("future enum admitted")
	}
	if nhf022PresentationAdvance(wire.Presentation_PRESENTATION_PRESENT_IN_OS_CENTER, wire.Presentation_PRESENTATION_NOT_ATTEMPTED) {
		t.Fatal("OS delivery receipt regressed")
	}
	if nhf022PresentationAdvance(wire.Presentation_PRESENTATION_OUTCOME_UNKNOWN, wire.Presentation_PRESENTATION_PERMISSION_DENIED) {
		t.Fatal("permission refusal disproved an already uncertain submission")
	}
}
func TestNHF022CompanionRetirementPurgesDependentInbox(t *testing.T) {
	parent := nhf021NewCompanion(nil)
	h := nhf022NewNotifications(parent)
	now := time.Now()
	installation := &companion.Installation{Id: &companion.InstallationId{Value: strings.Repeat("a", 32)},
		Consent: &companion.Consent{Cadence: companion.Cadence_CADENCE_MINUTE_1}, State: companion.InstallationState_INSTALLATION_STATE_ACTIVE}
	record := &nhf022DestinationRecord{value: &wire.Destination{Id: &wire.DestinationId{Value: strings.Repeat("b", 32)},
		InstallationId: installation.Id, State: wire.DestinationState_DESTINATION_STATE_ACTIVE, NativeDelivery: true, Revision: 1, ExpiresAt: timestamppb.New(now.Add(10 * time.Minute))},
		inboxRevision: 1, entries: map[nhf022DeliveryID]*wire.Delivery{"private": {Body: "private synthetic text", ExpiresAt: timestamppb.New(now.Add(time.Minute))}}}
	h.records[nhf022DestinationID(record.value.Id.Value)] = record
	parent.mu.Lock()
	parent.retireLocked(installation, companion.InstallationState_INSTALLATION_STATE_REVOKED)
	parent.mu.Unlock()
	if record.value.State != wire.DestinationState_DESTINATION_STATE_REVOKED || record.value.NativeDelivery || len(record.entries) != 0 || record.value.Revision != 2 {
		t.Fatal("parent revocation retained a private destination or message")
	}
	h.mu.Lock()
	h.collectLocked(now.Add(11 * time.Minute))
	h.mu.Unlock()
	if record.value.State != wire.DestinationState_DESTINATION_STATE_REVOKED {
		t.Fatal("revoked tombstone changed state on expiry")
	}
}
func TestNHF022AnonymousCannotDescribePrivateNotificationDeployment(t *testing.T) {
	h := nhf022NewNotifications(nhf021NewCompanion(nil))
	_, err := h.DescribeNotifications(context.Background(), connect.NewRequest(&wire.DescribeNotificationsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous discovery: %v", err)
	}
	_, err = h.ReadInbox(context.Background(), connect.NewRequest(&wire.ReadInboxRequest{Id: &wire.DestinationId{Value: strings.Repeat("a", 32)}}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous inbox: %v", err)
	}
	if h.counts != (nhf022Counts{}) || len(h.records) != 0 {
		t.Fatal("anonymous read introduced state")
	}
}
