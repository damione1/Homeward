package server

// Explicit isolated fixture only. These handlers are never registered by Run.
// Native session admission is real; companion records/permissions are synthetic
// process-local state and never reach the registry, integration Host or devices.
import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	wire "github.com/damione1/Homeward/gen/home/companion/v1"
	"github.com/damione1/Homeward/gen/home/companion/v1/companionv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nhf021InstallationID string
type nhf021RequestID string
type nhf021Owner struct{ account, session string }
type nhf021Record struct {
	owner         nhf021Owner
	value         *wire.Installation
	lastPublished time.Time
}
type nhf021Counts struct {
	Registrations  uint64 `json:"registrations"`
	ConsentChanges uint64 `json:"consent_changes"`
	Publications   uint64 `json:"publications"`
	Revocations    uint64 `json:"revocations"`
	LostReplies    uint64 `json:"lost_replies"`
}
type nhf021Companion struct {
	companionv1connect.UnimplementedCompanionServiceHandler
	mu        sync.Mutex
	authority *authorization.Authority
	epoch     string
	records   map[nhf021InstallationID]*nhf021Record
	counts    nhf021Counts
	// Real time by default; deterministic server-unit tests may inject time.
	now func() time.Time
}

func nhf021Error(code connect.Code) error {
	return connect.NewError(code, errors.New("synthetic companion request rejected"))
}
func nhf021Opaque() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw[:])
}
func nhf021ID(raw string) bool {
	if len(raw) != 32 {
		return false
	}
	for _, c := range raw {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func nhf021Text(raw string) bool {
	return raw != "" && len(raw) <= 128 && utf8.ValidString(raw) && strings.TrimSpace(raw) == raw && !strings.ContainsFunc(raw, unicode.IsControl)
}
func nhf021Known(v proto.Message) bool {
	if v == nil || !v.ProtoReflect().IsValid() || proto.Size(v) > 32*1024 {
		return false
	}
	var known func(protoreflect.Message, int) bool
	known = func(m protoreflect.Message, depth int) bool {
		if depth > 12 || len(m.GetUnknown()) != 0 {
			return false
		}
		valid := true
		m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if f.IsMap() {
				valid = false
				return false
			}
			check := func(v protoreflect.Value) bool {
				if f.Message() != nil {
					return known(v.Message(), depth+1)
				}
				if f.Enum() != nil {
					return f.Enum().Values().ByNumber(v.Enum()) != nil
				}
				return true
			}
			if f.IsList() {
				for i := 0; i < v.List().Len(); i++ {
					if !check(v.List().Get(i)) {
						valid = false
						break
					}
				}
			} else {
				valid = check(v)
			}
			return valid
		})
		return valid
	}
	return known(v.ProtoReflect(), 0)
}

var nhf021Features = [...]wire.Feature{
	wire.Feature_FEATURE_BATTERY_LEVEL, wire.Feature_FEATURE_BATTERY_STATE, wire.Feature_FEATURE_CHARGING,
	wire.Feature_FEATURE_CONNECTIVITY, wire.Feature_FEATURE_ACTIVITY, wire.Feature_FEATURE_FOCUS, wire.Feature_FEATURE_DO_NOT_DISTURB,
	wire.Feature_FEATURE_KIOSK, wire.Feature_FEATURE_SCREEN_SAVER, wire.Feature_FEATURE_BRIGHTNESS, wire.Feature_FEATURE_VOLUME,
	wire.Feature_FEATURE_LOCATION, wire.Feature_FEATURE_CAMERA_MOTION, wire.Feature_FEATURE_NOTIFICATIONS,
}

func nhf021Profile(platform wire.Platform, c *wire.Capability) bool {
	availability, source := wire.Availability_AVAILABILITY_UNSUPPORTED, wire.Source_SOURCE_NONE
	switch platform {
	case wire.Platform_PLATFORM_MAC_OS:
		switch c.Feature {
		case wire.Feature_FEATURE_BATTERY_LEVEL, wire.Feature_FEATURE_BATTERY_STATE, wire.Feature_FEATURE_CHARGING:
			availability, source = wire.Availability_AVAILABILITY_AVAILABLE, wire.Source_SOURCE_MAC_OS_POWER_SOURCES
		case wire.Feature_FEATURE_CONNECTIVITY:
			availability, source = wire.Availability_AVAILABILITY_PARTIAL, wire.Source_SOURCE_MAC_OS_NETWORK_CONFIGURATION
		case wire.Feature_FEATURE_ACTIVITY:
			availability, source = wire.Availability_AVAILABILITY_PARTIAL, wire.Source_SOURCE_NATIVE_LIFECYCLE
		case wire.Feature_FEATURE_FOCUS, wire.Feature_FEATURE_DO_NOT_DISTURB, wire.Feature_FEATURE_KIOSK,
			wire.Feature_FEATURE_SCREEN_SAVER, wire.Feature_FEATURE_BRIGHTNESS, wire.Feature_FEATURE_VOLUME,
			wire.Feature_FEATURE_LOCATION, wire.Feature_FEATURE_CAMERA_MOTION, wire.Feature_FEATURE_NOTIFICATIONS:
		default:
			return false
		}
	case wire.Platform_PLATFORM_ANDROID:
		if c.Feature != wire.Feature_FEATURE_FOCUS {
			availability = wire.Availability_AVAILABILITY_PENDING_QUALIFICATION
		}
	default:
		return false
	}
	if c.Availability != availability || c.Source != source {
		return false
	}
	switch availability {
	case wire.Availability_AVAILABILITY_UNSUPPORTED:
		return c.Permission == wire.Permission_PERMISSION_UNSUPPORTED
	case wire.Availability_AVAILABILITY_PENDING_QUALIFICATION:
		return c.Permission == wire.Permission_PERMISSION_NOT_REQUESTED
	case wire.Availability_AVAILABILITY_AVAILABLE, wire.Availability_AVAILABILITY_PARTIAL:
		switch c.Permission {
		case wire.Permission_PERMISSION_NOT_REQUIRED, wire.Permission_PERMISSION_GRANTED, wire.Permission_PERMISSION_DENIED, wire.Permission_PERMISSION_RESTRICTED:
			return true
		}
	}
	return false
}
func nhf021Device(v *wire.DeviceDescription) bool {
	if !nhf021Known(v) || !nhf021Text(v.Model) || !nhf021Text(v.OsVersion) || len(v.Capabilities) != len(nhf021Features) {
		return false
	}
	seen := make(map[wire.Feature]bool)
	for _, c := range v.Capabilities {
		if c == nil || c.Feature == wire.Feature_FEATURE_UNSPECIFIED || seen[c.Feature] || !nhf021Profile(v.Platform, c) {
			return false
		}
		seen[c.Feature] = true
	}
	for _, f := range nhf021Features {
		if !seen[f] {
			return false
		}
	}
	return true
}
func nhf021Cadence(c wire.Cadence) time.Duration {
	switch c {
	case wire.Cadence_CADENCE_SECONDS_30:
		return 30 * time.Second
	case wire.Cadence_CADENCE_MINUTE_1:
		return time.Minute
	case wire.Cadence_CADENCE_MINUTES_5:
		return 5 * time.Minute
	}
	return 0
}
func nhf021CanCollect(d *wire.DeviceDescription, f wire.Feature) bool {
	for _, c := range d.Capabilities {
		if c.Feature == f {
			return (c.Availability == wire.Availability_AVAILABILITY_AVAILABLE || c.Availability == wire.Availability_AVAILABILITY_PARTIAL) &&
				(c.Permission == wire.Permission_PERMISSION_NOT_REQUIRED || c.Permission == wire.Permission_PERMISSION_GRANTED) && c.Source != wire.Source_SOURCE_NONE
		}
	}
	return false
}
func nhf021Consent(c *wire.Consent, d *wire.DeviceDescription) bool {
	return c != nil && nhf021Cadence(c.Cadence) > 0 && (!c.Battery || (nhf021CanCollect(d, wire.Feature_FEATURE_BATTERY_LEVEL) && nhf021CanCollect(d, wire.Feature_FEATURE_BATTERY_STATE) && nhf021CanCollect(d, wire.Feature_FEATURE_CHARGING))) &&
		(!c.Connectivity || nhf021CanCollect(d, wire.Feature_FEATURE_CONNECTIVITY)) && (!c.Activity || nhf021CanCollect(d, wire.Feature_FEATURE_ACTIVITY))
}
func nhf021NoData(n wire.NoData, enabled, battery bool) bool {
	if !enabled {
		return n == wire.NoData_NO_DATA_DISABLED
	}
	switch n {
	case wire.NoData_NO_DATA_UNSUPPORTED, wire.NoData_NO_DATA_UNAVAILABLE, wire.NoData_NO_DATA_PERMISSION_DENIED, wire.NoData_NO_DATA_STALE:
		return true
	case wire.NoData_NO_DATA_NO_HARDWARE:
		return battery
	case wire.NoData_NO_DATA_UNSPECIFIED, wire.NoData_NO_DATA_DISABLED:
		return false
	}
	return false
}
func nhf021Snapshot(s *wire.Snapshot, c *wire.Consent) bool {
	if s == nil || s.CapturedAt == nil || s.CapturedAt.CheckValid() != nil || s.CapturedAt.Seconds <= 0 || s.Battery == nil || s.Connectivity == nil || s.Activity == nil {
		return false
	}
	battery, connectivity, activity := false, false, false
	switch b := s.Battery.Observation.(type) {
	case *wire.BatteryObservation_Value:
		if b.Value != nil && c.Battery && b.Value.Percent <= 100 {
			switch b.Value.State {
			case wire.PowerState_POWER_STATE_BATTERY, wire.PowerState_POWER_STATE_CHARGING, wire.PowerState_POWER_STATE_EXTERNAL_POWER, wire.PowerState_POWER_STATE_FULL:
				battery = true
			}
		}
	case *wire.BatteryObservation_NoData:
		battery = nhf021NoData(b.NoData, c.Battery, true)
	}
	switch v := s.Connectivity.Observation.(type) {
	case *wire.ConnectivityObservation_Value:
		if c.Connectivity {
			switch v.Value {
			case wire.Connectivity_CONNECTIVITY_LOCALLY_REACHABLE, wire.Connectivity_CONNECTIVITY_CONNECTION_REQUIRED, wire.Connectivity_CONNECTIVITY_NO_LOCAL_ROUTE:
				connectivity = true
			}
		}
	case *wire.ConnectivityObservation_NoData:
		connectivity = nhf021NoData(v.NoData, c.Connectivity, false)
	}
	switch v := s.Activity.Observation.(type) {
	case *wire.ActivityObservation_Value:
		activity = c.Activity && v.Value == wire.AppActivity_APP_ACTIVITY_FOREGROUND
	case *wire.ActivityObservation_NoData:
		activity = nhf021NoData(v.NoData, c.Activity, false)
	}
	return battery && connectivity && activity
}

func nhf021NewCompanion(authority *authorization.Authority) *nhf021Companion {
	return &nhf021Companion{authority: authority, epoch: nhf021Opaque(), records: make(map[nhf021InstallationID]*nhf021Record), now: time.Now}
}
func (h *nhf021Companion) context() *wire.Context {
	return &wire.Context{Deployment: wire.Deployment_DEPLOYMENT_SYNTHETIC_ONLY, Epoch: h.epoch}
}
func nhf021Purge(v *wire.Installation, state wire.InstallationState) {
	v.State = state
	v.Latest = nil
	v.AcceptedAt = nil
	v.Consent = &wire.Consent{Cadence: v.Consent.Cadence}
}

// Account authority -> mock lock order. No SQL/network/randomness/OS work in
// the callback. The principal and session owner came from actual native auth.
func (h *nhf021Companion) withNative(ctx context.Context, fn func(nhf021Owner, time.Time) error) error {
	p, err := authorization.PrincipalFromContext(ctx)
	lease, ok := ctx.Value(sessionLeaseContextKey{}).(*sessionLease)
	if err != nil || !p.Native() || !ok || lease == nil {
		return nhf021Error(connect.CodeUnauthenticated)
	}
	account, e1 := lease.owner.Account.Value()
	session, e2 := lease.owner.Session.Value()
	principalAccount, e3 := p.Account()
	accountValue, e4 := principalAccount.Value()
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || account != accountValue {
		return nhf021Error(connect.CodeUnauthenticated)
	}
	err = h.authority.WithCurrent(ctx, p, func(_ authorization.Current) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		now := h.now()
		for id, r := range h.records {
			if !now.Before(r.value.ExpiresAt.AsTime()) {
				nhf021Purge(r.value, wire.InstallationState_INSTALLATION_STATE_EXPIRED)
			}
			if now.After(r.value.ExpiresAt.AsTime().Add(30 * time.Minute)) {
				delete(h.records, id)
			}
		}
		return fn(nhf021Owner{account: account, session: session}, now)
	})
	if errors.Is(err, authorization.ErrDenied) {
		return nhf021Error(connect.CodePermissionDenied)
	}
	if errors.Is(err, authorization.ErrUnavailable) {
		return nhf021Error(connect.CodeUnavailable)
	}
	if errors.Is(err, context.Canceled) {
		return nhf021Error(connect.CodeCanceled)
	}
	return err
}
func (h *nhf021Companion) own(owner nhf021Owner, id *wire.InstallationId) (*wire.Installation, error) {
	if id == nil || !nhf021ID(id.Value) {
		return nil, nhf021Error(connect.CodeInvalidArgument)
	}
	r := h.records[nhf021InstallationID(id.Value)]
	if r == nil || r.owner != owner {
		return nil, nhf021Error(connect.CodeNotFound)
	}
	return r.value, nil
}
func (h *nhf021Companion) current(owner nhf021Owner, expected *wire.Context, id *wire.InstallationId, revision uint64) (*wire.Installation, error) {
	if expected == nil || !proto.Equal(expected, h.context()) {
		return nil, nhf021Error(connect.CodeFailedPrecondition)
	}
	v, err := h.own(owner, id)
	if err != nil {
		return nil, err
	}
	if v.State != wire.InstallationState_INSTALLATION_STATE_ACTIVE {
		return nil, nhf021Error(connect.CodePermissionDenied)
	}
	if revision == 0 || revision == math.MaxUint64 || v.Revision != revision {
		return nil, nhf021Error(connect.CodeAborted)
	}
	return v, nil
}
func (h *nhf021Companion) RegisterInstallation(ctx context.Context, r *connect.Request[wire.RegisterInstallationRequest]) (*connect.Response[wire.RegisterInstallationResponse], error) {
	if !nhf021Known(r.Msg) || !nhf021ID(r.Msg.RequestId) || !nhf021Device(r.Msg.Device) {
		return nil, nhf021Error(connect.CodeInvalidArgument)
	}
	if r.Msg.Expected == nil || !proto.Equal(r.Msg.Expected, h.context()) {
		return nil, nhf021Error(connect.CodeFailedPrecondition)
	}
	// Randomness happens before entering the authority callback.
	installationID, deviceID := nhf021Opaque(), nhf021Opaque()
	var result *wire.Installation
	err := h.withNative(ctx, func(owner nhf021Owner, now time.Time) error {
		active := 0
		for _, r2 := range h.records {
			if r2.owner == owner {
				if r2.value.RegistrationRequestId == r.Msg.RequestId {
					return nhf021Error(connect.CodeAlreadyExists)
				}
				if r2.value.State == wire.InstallationState_INSTALLATION_STATE_ACTIVE {
					active++
				}
			}
		}
		if active >= 8 || len(h.records) >= 256 {
			return nhf021Error(connect.CodeResourceExhausted)
		}
		result = &wire.Installation{Id: &wire.InstallationId{Value: installationID}, DeviceId: &wire.DeviceId{Value: deviceID},
			AccountId: &auth.AccountId{Value: owner.account}, SessionId: &auth.SessionId{Value: owner.session}, Device: proto.Clone(r.Msg.Device).(*wire.DeviceDescription),
			Consent: &wire.Consent{Cadence: wire.Cadence_CADENCE_MINUTE_1}, Revision: 1, State: wire.InstallationState_INSTALLATION_STATE_ACTIVE,
			ExpiresAt: timestamppb.New(now.Add(30 * time.Minute)), RegistrationRequestId: r.Msg.RequestId}
		h.records[nhf021InstallationID(installationID)] = &nhf021Record{owner: owner, value: result}
		h.counts.Registrations++
		result = proto.Clone(result).(*wire.Installation)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.RegisterInstallationResponse{Context: h.context(), Installation: result}), nil
}

func (h *nhf021Companion) DescribeCompanion(ctx context.Context, r *connect.Request[wire.DescribeCompanionRequest]) (*connect.Response[wire.DescribeCompanionResponse], error) {
	if !nhf021Known(r.Msg) {
		return nil, nhf021Error(connect.CodeInvalidArgument)
	}
	if err := h.withNative(ctx, func(nhf021Owner, time.Time) error { return nil }); err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.DescribeCompanionResponse{Context: h.context()}), nil
}
func (h *nhf021Companion) GetInstallation(ctx context.Context, r *connect.Request[wire.GetInstallationRequest]) (*connect.Response[wire.GetInstallationResponse], error) {
	if !nhf021Known(r.Msg) {
		return nil, nhf021Error(connect.CodeInvalidArgument)
	}
	var result *wire.Installation
	err := h.withNative(ctx, func(owner nhf021Owner, _ time.Time) error {
		var value *wire.Installation
		switch selector := r.Msg.Selector.(type) {
		case *wire.GetInstallationRequest_Id:
			var err error
			value, err = h.own(owner, selector.Id)
			if err != nil {
				return err
			}
		case *wire.GetInstallationRequest_RegistrationRequestId:
			if !nhf021ID(selector.RegistrationRequestId) {
				return nhf021Error(connect.CodeInvalidArgument)
			}
			for _, record := range h.records {
				if record.owner == owner && record.value.RegistrationRequestId == selector.RegistrationRequestId {
					value = record.value
					break
				}
			}
			if value == nil {
				return nhf021Error(connect.CodeNotFound)
			}
		default:
			return nhf021Error(connect.CodeInvalidArgument)
		}
		result = proto.Clone(value).(*wire.Installation)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.GetInstallationResponse{Context: h.context(), Installation: result}), nil
}
func (h *nhf021Companion) UpdateConsent(ctx context.Context, r *connect.Request[wire.UpdateConsentRequest]) (*connect.Response[wire.UpdateConsentResponse], error) {
	if !nhf021Known(r.Msg) {
		return nil, nhf021Error(connect.CodeInvalidArgument)
	}
	var result *wire.Installation
	err := h.withNative(ctx, func(owner nhf021Owner, _ time.Time) error {
		v, err := h.current(owner, r.Msg.Expected, r.Msg.Id, r.Msg.ExpectedRevision)
		if err != nil {
			return err
		}
		if v.Revision >= math.MaxUint64-1 || !nhf021Consent(r.Msg.Consent, v.Device) {
			return nhf021Error(connect.CodeInvalidArgument)
		}
		v.Consent = proto.Clone(r.Msg.Consent).(*wire.Consent)
		v.Revision++
		v.Latest = nil
		v.AcceptedAt = nil
		h.counts.ConsentChanges++
		result = proto.Clone(v).(*wire.Installation)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.UpdateConsentResponse{Context: h.context(), Installation: result}), nil
}
func (h *nhf021Companion) PublishTelemetry(ctx context.Context, r *connect.Request[wire.PublishTelemetryRequest]) (*connect.Response[wire.PublishTelemetryResponse], error) {
	if !nhf021Known(r.Msg) {
		return nil, nhf021Error(connect.CodeInvalidArgument)
	}
	var result *wire.Installation
	err := h.withNative(ctx, func(owner nhf021Owner, now time.Time) error {
		v, err := h.current(owner, r.Msg.Expected, r.Msg.Id, r.Msg.ExpectedRevision)
		if err != nil {
			return err
		}
		if !v.Consent.Battery && !v.Consent.Connectivity && !v.Consent.Activity {
			return nhf021Error(connect.CodePermissionDenied)
		}
		if r.Msg.Sequence == 0 || r.Msg.Sequence == math.MaxUint64 || r.Msg.Sequence != v.LastSequence+1 || !nhf021Snapshot(r.Msg.Snapshot, v.Consent) {
			return nhf021Error(connect.CodeInvalidArgument)
		}
		age := now.Sub(r.Msg.Snapshot.CapturedAt.AsTime())
		cadence := nhf021Cadence(v.Consent.Cadence)
		if age < -30*time.Second || age >= 2*cadence {
			return nhf021Error(connect.CodeFailedPrecondition)
		}
		record := h.records[nhf021InstallationID(v.Id.Value)]
		if !record.lastPublished.IsZero() && now.Sub(record.lastPublished) < cadence {
			return nhf021Error(connect.CodeResourceExhausted)
		}
		v.LastSequence = r.Msg.Sequence
		v.Latest = proto.Clone(r.Msg.Snapshot).(*wire.Snapshot)
		v.AcceptedAt = timestamppb.New(now)
		record.lastPublished = now
		h.counts.Publications++
		result = proto.Clone(v).(*wire.Installation)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.PublishTelemetryResponse{Context: h.context(), Installation: result}), nil
}
func (h *nhf021Companion) RevokeInstallation(ctx context.Context, r *connect.Request[wire.RevokeInstallationRequest]) (*connect.Response[wire.RevokeInstallationResponse], error) {
	if !nhf021Known(r.Msg) {
		return nil, nhf021Error(connect.CodeInvalidArgument)
	}
	var result *wire.Installation
	err := h.withNative(ctx, func(owner nhf021Owner, _ time.Time) error {
		v, err := h.current(owner, r.Msg.Expected, r.Msg.Id, r.Msg.ExpectedRevision)
		if err != nil {
			return err
		}
		if v.Revision >= math.MaxUint64-1 {
			return nhf021Error(connect.CodeResourceExhausted)
		}
		nhf021Purge(v, wire.InstallationState_INSTALLATION_STATE_REVOKED)
		v.Revision++
		h.counts.Revocations++
		result = proto.Clone(v).(*wire.Installation)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wire.RevokeInstallationResponse{Context: h.context(), Installation: result}), nil
}
