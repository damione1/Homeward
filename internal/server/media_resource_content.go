package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func withMediaRead[T any](h *mediaResourceHandler, ctx context.Context, raw string, fn func(context.Context, mediasessions.Name, mediaSourceRegistration, mediacontent.Source, *mediasessions.Admission, mediasessions.Record) (T, error)) (T, error) {
	return withMediaLocator(h, ctx, raw, func(ctx context.Context, name mediasessions.Name, registration mediaSourceRegistration, source mediacontent.Source, admission *mediasessions.Admission) (T, error) {
		var zero T
		// Register cancellation before authoritative SQL admission. A concurrent
		// retire can neither miss a just-started read nor return before it stops.
		operation, err := h.sessions.BeginOperation(ctx, name.ID())
		if err != nil {
			return zero, err
		}
		defer operation.Close()
		record, err := h.sessions.Get(operation.Context(), admission, name.ID(), source.Binding())
		if err != nil {
			return zero, err
		}
		bounded, cancel := context.WithDeadline(operation.Context(), record.ExpireTime())
		defer cancel()
		result, err := fn(bounded, name, registration, source, admission, record)
		if err != nil {
			return zero, err
		}
		if _, err = h.sessions.Get(bounded, admission, name.ID(), source.Binding()); err != nil {
			return zero, err
		}
		return result, nil
	})
}

func mediaEventWire(event mediacontent.Event) *api.MediaEvent {
	return &api.MediaEvent{EventId: event.ID().String(), DisplayName: event.Label(), Source: event.Source(), CaptureTime: timestamppb.New(event.CaptureTime()), HasImage: event.HasImage()}
}
func mediaTarget(raw *api.MediaImageTarget) (mediasessions.ImageTarget, error) {
	if !calendarKnown(raw) {
		return mediasessions.ImageTarget{}, taskInvalid()
	}
	switch value := raw.Value.(type) {
	case *api.MediaImageTarget_Snapshot:
		if value == nil || !calendarKnown(value.Snapshot) {
			return mediasessions.ImageTarget{}, taskInvalid()
		}
		return mediasessions.SnapshotTarget(), nil
	case *api.MediaImageTarget_EventId:
		if value == nil {
			return mediasessions.ImageTarget{}, taskInvalid()
		}
		id, err := mediasessions.ParseEventID(value.EventId)
		if err != nil {
			return mediasessions.ImageTarget{}, taskInvalid()
		}
		return mediasessions.EventTarget(id)
	default:
		return mediasessions.ImageTarget{}, taskInvalid()
	}
}
func mediaTargetWire(target mediasessions.ImageTarget) *api.MediaImageTarget {
	if id, event := target.Event(); event {
		return &api.MediaImageTarget{Value: &api.MediaImageTarget_EventId{EventId: id.String()}}
	}
	return &api.MediaImageTarget{Value: &api.MediaImageTarget_Snapshot{Snapshot: &emptypb.Empty{}}}
}
func (h *mediaResourceHandler) ReadMediaSession(ctx context.Context, request *connect.Request[api.ReadMediaSessionRequest]) (*connect.Response[api.ReadMediaSessionResponse], error) {
	raw := taskRequest(request)
	return withMediaRead(h, ctx, raw.GetName(), func(ctx context.Context, name mediasessions.Name, registration mediaSourceRegistration, source mediacontent.Source, admission *mediasessions.Admission, record mediasessions.Record) (*connect.Response[api.ReadMediaSessionResponse], error) {
		if !calendarKnown(raw) {
			return nil, taskInvalid()
		}
		target, err := mediaTarget(raw.Target)
		if err != nil {
			return nil, err
		}
		if source.Binding().Kind() == mediasessions.Artwork && target.Kind() != mediasessions.SnapshotImage {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESOURCE_NOT_FOUND)
		}
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		image, err := registration.owner.Image(bounded, source.Binding(), admission.CredentialBinding(), record.ArtworkReference(), target)
		if err != nil {
			return nil, err
		}
		if !image.Valid() || !image.Matches(record.Source(), target, record.ArtworkReference()) {
			return nil, mediacontent.ErrInvalid
		}
		out := &api.MediaImage{MediaSession: name.String(), Target: mediaTargetWire(target), CaptureTime: timestamppb.New(image.CaptureTime()), ExpireTime: timestamppb.New(record.ExpireTime()), Png: image.PNG()}
		if proto.Size(out) > 513<<10 {
			return nil, mediacontent.ErrInvalid
		}
		return connect.NewResponse(&api.ReadMediaSessionResponse{Image: out}), nil
	})
}

// mediaEventCapture sorts only admitted native values and hashes them without
// building or encoding the entire public collection. At most one 64KiB page is
// projected, preserving the existing ten-thousand-event source limit.
func mediaEventCapture(events mediacontent.EventCollection) ([]mediacontent.Event, [32]byte) {
	rows := events.Events()
	slices.SortFunc(rows, func(a, b mediacontent.Event) int { return stringsCompare(a.ID().String(), b.ID().String()) })
	hash := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(events.Revision()))
	hash.Write(length[:])
	for _, event := range rows {
		for _, value := range []string{event.ID().String(), event.Label(), event.Source(), event.CaptureTime().UTC().Format(time.RFC3339Nano)} {
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			hash.Write(length[:])
			hash.Write([]byte(value))
		}
		if event.HasImage() {
			hash.Write([]byte{1})
		} else {
			hash.Write([]byte{0})
		}
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return rows, digest
}
func (h *mediaResourceHandler) QueryMediaSessionEvents(ctx context.Context, request *connect.Request[api.QueryMediaSessionEventsRequest]) (*connect.Response[api.QueryMediaSessionEventsResponse], error) {
	raw := taskRequest(request)
	return withMediaRead(h, ctx, raw.GetName(), func(ctx context.Context, name mediasessions.Name, registration mediaSourceRegistration, source mediacontent.Source, _ *mediasessions.Admission, record mediasessions.Record) (*connect.Response[api.QueryMediaSessionEventsResponse], error) {
		if !calendarKnown(raw) {
			return nil, taskInvalid()
		}
		limit, err := taskPageSize(raw.PageSize)
		if err != nil {
			return nil, err
		}
		events, err := mediaEvents(ctx, registration, source)
		if err != nil {
			return nil, err
		}
		rows, digest := mediaEventCapture(events)
		binding, err := h.queryBinding(ctx, apiv2connect.MediaServiceQueryMediaSessionEventsProcedure, name.String())
		if err != nil {
			return nil, err
		}
		offset, err := h.pageOffset(raw.PageToken, binding, digest, len(rows))
		if err != nil {
			return nil, err
		}
		session, err := mediaSessionWireRevision(name.Home(), record, events.Revision())
		if err != nil {
			return nil, err
		}
		out := &api.QueryMediaSessionEventsResponse{MediaSession: name.String(), EventsEtag: session.EventsEtag, TotalSize: int32(len(rows))}
		end := offset
		for end < len(rows) && len(out.MediaEvents) < limit {
			event := mediaEventWire(rows[end])
			out.MediaEvents = append(out.MediaEvents, event)
			previousToken := out.NextPageToken
			out.NextPageToken, err = h.pageToken(end+1, len(rows), binding, digest)
			if err != nil {
				return nil, err
			}
			// The cursor is part of the response byte budget, including on
			// a page shortened by this check.
			if proto.Size(out) > 64<<10 {
				out.MediaEvents = out.MediaEvents[:len(out.MediaEvents)-1]
				out.NextPageToken = previousToken
				break
			}
			end++
		}
		if end == offset && offset < len(rows) {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
		}
		current, err := mediaEvents(ctx, registration, source)
		if err != nil {
			return nil, err
		}
		if current.Revision() != events.Revision() {
			return nil, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
		}
		return connect.NewResponse(out), nil
	})
}
