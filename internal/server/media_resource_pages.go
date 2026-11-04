package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"slices"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
	"google.golang.org/protobuf/proto"
)

func (h *mediaResourceHandler) queryBinding(ctx context.Context, method, parent string) ([32]byte, error) {
	principal, err := h.principal(ctx)
	if err != nil {
		return [32]byte{}, err
	}
	current, err := h.authority.Current(principal)
	if err != nil {
		return [32]byte{}, authorization.ErrDenied
	}
	_, binding, err := current.Binding()
	if err != nil {
		return [32]byte{}, authorization.ErrDenied
	}
	hash := sha256.New()
	hash.Write([]byte("homeward-media-resources/2"))
	hash.Write(binding[:])
	var length [8]byte
	for _, value := range []string{method, parent} {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	var out [32]byte
	copy(out[:], hash.Sum(nil))
	return out, nil
}

type mediaCaptureCursor struct {
	Capture [32]byte
	Offset  int
}

func (h *mediaResourceHandler) pageOffset(token string, binding, capture [32]byte, total int) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := h.cursors.Open(token, binding)
	if err != nil {
		return 0, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
	}
	var cursor mediaCaptureCursor
	if json.Unmarshal(raw, &cursor) != nil || cursor.Capture != capture || cursor.Offset < 1 || cursor.Offset >= total {
		return 0, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
	}
	return cursor.Offset, nil
}
func (h *mediaResourceHandler) pageToken(offset, total int, binding, capture [32]byte) (string, error) {
	if offset >= total {
		return "", nil
	}
	raw, err := json.Marshal(mediaCaptureCursor{capture, offset})
	if err != nil {
		return "", err
	}
	return h.cursors.Seal(raw, binding)
}
func mediaCaptureHash(message proto.Message) ([32]byte, error) {
	if proto.Size(message) > 1<<20 {
		return [32]byte{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}
func (h *mediaResourceHandler) ListMediaSources(ctx context.Context, request *connect.Request[api.ListMediaSourcesRequest]) (*connect.Response[api.ListMediaSourcesResponse], error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	raw := taskRequest(request)
	home, err := apitypes.ParseHomeName(raw.GetParent())
	if err != nil {
		return nil, taskDenied()
	}
	if err := h.home(ctx, home); err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	if !calendarKnown(raw) {
		return nil, taskInvalid()
	}
	limit, err := taskPageSize(raw.PageSize)
	if err != nil {
		return nil, err
	}
	binding, err := h.queryBinding(ctx, apiv2connect.MediaServiceListMediaSourcesProcedure, raw.Parent)
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	capture := &api.ListMediaSourcesResponse{}
	for _, registration := range h.sources {
		admission, err := h.sessions.AdmitSource(ctx, registration.entity, registration.slot)
		if mediasessions.IsCode(err, mediasessions.Denied) {
			continue
		}
		if err != nil {
			return nil, mediaResourceError(ctx, err)
		}
		source, readErr := mediaCapture(admission.Context(), registration)
		if readErr == nil {
			var wire *api.MediaSource
			wire, readErr = mediaSourceWire(home, source)
			if readErr == nil {
				capture.MediaSources = append(capture.MediaSources, wire)
			}
		}
		if readErr == nil {
			readErr = mediaFinalSource(admission.Context(), admission, registration, source)
		}
		admission.Release()
		if readErr != nil {
			return nil, mediaResourceError(ctx, readErr)
		}
	}
	slices.SortFunc(capture.MediaSources, func(a, b *api.MediaSource) int { return stringsCompare(a.Name, b.Name) })
	digest, err := mediaCaptureHash(capture)
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	offset, err := h.pageOffset(raw.PageToken, binding, digest, len(capture.MediaSources))
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	end := min(offset+limit, len(capture.MediaSources))
	token, err := h.pageToken(end, len(capture.MediaSources), binding, digest)
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	final, err := h.queryBinding(ctx, apiv2connect.MediaServiceListMediaSourcesProcedure, raw.Parent)
	if err != nil || final != binding {
		return nil, taskDenied()
	}
	return connect.NewResponse(&api.ListMediaSourcesResponse{MediaSources: capture.MediaSources[offset:end], NextPageToken: token}), nil
}
func stringsCompare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
func (h *mediaResourceHandler) ListMediaSessions(ctx context.Context, request *connect.Request[api.ListMediaSessionsRequest]) (*connect.Response[api.ListMediaSessionsResponse], error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	raw := taskRequest(request)
	home, err := apitypes.ParseHomeName(raw.GetParent())
	if err != nil {
		return nil, taskDenied()
	}
	if err := h.home(ctx, home); err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	if !calendarKnown(raw) {
		return nil, taskInvalid()
	}
	limit, err := taskPageSize(raw.PageSize)
	if err != nil {
		return nil, err
	}
	binding, err := h.queryBinding(ctx, apiv2connect.MediaServiceListMediaSessionsProcedure, raw.Parent)
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	locators, err := h.sessions.OwnedLocators(ctx)
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	capture := &api.ListMediaSessionsResponse{}
	for _, locator := range locators {
		name, err := mediasessions.ResourceName(home, locator.ID())
		if err != nil {
			return nil, mediaResourceError(ctx, err)
		}
		response, err := h.GetMediaSession(ctx, connect.NewRequest(&api.GetMediaSessionRequest{Name: name.String()}))
		if connect.CodeOf(err) == connect.CodeNotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		capture.MediaSessions = append(capture.MediaSessions, response.Msg)
	}
	slices.SortFunc(capture.MediaSessions, func(a, b *api.MediaSession) int { return stringsCompare(a.Name, b.Name) })
	digest, err := mediaCaptureHash(capture)
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	offset, err := h.pageOffset(raw.PageToken, binding, digest, len(capture.MediaSessions))
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	end := min(offset+limit, len(capture.MediaSessions))
	token, err := h.pageToken(end, len(capture.MediaSessions), binding, digest)
	if err != nil {
		return nil, mediaResourceError(ctx, err)
	}
	final, err := h.queryBinding(ctx, apiv2connect.MediaServiceListMediaSessionsProcedure, raw.Parent)
	if err != nil || final != binding {
		return nil, taskDenied()
	}
	return connect.NewResponse(&api.ListMediaSessionsResponse{MediaSessions: capture.MediaSessions[offset:end], NextPageToken: token}), nil
}

// mediaEvents binds one current native collection capture to an already
// authenticated live SQL lease. The native event owner remains authoritative;
// changes never rewrite or renew session timestamps.
func mediaEvents(ctx context.Context, registration mediaSourceRegistration, source mediacontent.Source) (mediacontent.EventCollection, error) {
	if source.Binding().Kind() != mediasessions.Camera {
		return mediacontent.EventCollection{}, mediacontent.ErrUnsupported
	}
	events, err := registration.owner.Events(ctx, source.Binding())
	if err != nil {
		return mediacontent.EventCollection{}, err
	}
	if !events.Valid() || !events.Matches(source.Binding()) {
		return mediacontent.EventCollection{}, mediacontent.ErrInvalid
	}
	return events, nil
}

func mediaRevisionDigest(capture [32]byte, revision int64) [32]byte {
	var raw [40]byte
	copy(raw[:32], capture[:])
	binary.BigEndian.PutUint64(raw[32:], uint64(revision))
	return sha256.Sum256(raw[:])
}
