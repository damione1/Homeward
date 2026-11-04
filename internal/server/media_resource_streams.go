package server

import (
	"context"
	"io"
	"math"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/mediacontent"
	"github.com/damione1/Homeward/internal/mediasessions"
	"google.golang.org/protobuf/types/known/durationpb"
)

func (h *mediaResourceHandler) WatchMediaSession(ctx context.Context, request *connect.Request[api.WatchMediaSessionRequest], stream *connect.ServerStream[api.WatchMediaSessionResponse]) error {
	original := ctx
	raw := taskRequest(request)
	_, err := withMediaLocator(h, ctx, raw.GetName(), func(ctx context.Context, name mediasessions.Name, registration mediaSourceRegistration, source mediacontent.Source, admission *mediasessions.Admission) (struct{}, error) {
		if !calendarKnown(raw) || stream == nil {
			return struct{}{}, taskInvalid()
		}
		operation, err := h.sessions.BeginOperation(ctx, name.ID())
		if err != nil {
			return struct{}{}, err
		}
		record, err := h.sessions.Get(operation.Context(), admission, name.ID(), source.Binding())
		if err != nil {
			operation.Close()
			return struct{}{}, err
		}
		// Stop/join this execution before the native lifecycle cleanup joins the
		// session's other work; retirement never waits on itself.
		defer func() { operation.Close(); _ = h.sessions.RetireVerified(record) }()
		bounded, cancel := context.WithDeadline(operation.Context(), record.ExpireTime())
		defer cancel()
		guarded, stop := mediaSourceLifetime(bounded, admission, registration, source)
		defer stop()
		bounded = guarded
		defer interruptCoreRPCWriteOnCancel(original, bounded)()
		revision := record.EventRevision()
		var changes <-chan struct{}
		if source.Binding().Kind() == mediasessions.Camera {
			// Subscribe before capture so an event change cannot fall between ready and
			// the first waiting notification. Provider queues remain native-owned.
			subscription, err := registration.owner.Subscribe(bounded, source.Binding())
			if err != nil {
				return struct{}{}, err
			}
			defer subscription.Close()
			changes = subscription.Changes()
			if changes == nil {
				return struct{}{}, mediacontent.ErrInvalid
			}
			events, err := mediaEvents(bounded, registration, source)
			if err != nil {
				return struct{}{}, err
			}
			revision = events.Revision()
		}
		wire, err := mediaSessionWireRevision(name.Home(), record, revision)
		if err != nil {
			return struct{}{}, err
		}
		if err := mediaFinalSource(bounded, admission, registration, source); err != nil {
			return struct{}{}, err
		}
		if _, err = h.sessions.Get(bounded, admission, name.ID(), source.Binding()); err != nil {
			return struct{}{}, err
		}
		if err := stream.Send(&api.WatchMediaSessionResponse{Sequence: 1, Frame: &api.WatchMediaSessionResponse_Ready{Ready: wire}}); err != nil {
			return struct{}{}, err
		}
		sequence := int64(1)
		// A bounded source-fence tick interrupts blocked writes when native
		// configuration/epoch changes without an event collection notification.
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-bounded.Done():
				return struct{}{}, context.Cause(bounded)
			case _, open := <-changes:
				if !open {
					return struct{}{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
				}
			case <-ticker.C:
			}
			if err := mediaFinalSource(bounded, admission, registration, source); err != nil {
				return struct{}{}, err
			}
			if _, err = h.sessions.Get(bounded, admission, name.ID(), source.Binding()); err != nil {
				return struct{}{}, err
			}
			if source.Binding().Kind() != mediasessions.Camera {
				continue
			}
			events, err := mediaEvents(bounded, registration, source)
			if err != nil {
				return struct{}{}, err
			}
			if events.Revision() < revision {
				return struct{}{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED)
			}
			if events.Revision() == revision {
				continue
			}
			revision = events.Revision()
			current, err := mediaSessionWireRevision(name.Home(), record, revision)
			if err != nil {
				return struct{}{}, err
			}
			if sequence == math.MaxInt64 {
				return struct{}{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
			}
			sequence++
			if !admission.Ready() {
				return struct{}{}, taskDenied()
			}
			if err := stream.Send(&api.WatchMediaSessionResponse{Sequence: sequence, Frame: &api.WatchMediaSessionResponse_Changed{Changed: &api.MediaEventsChanged{EventsEtag: current.EventsEtag}}}); err != nil {
				return struct{}{}, err
			}
		}
	})
	return err
}
func (h *mediaResourceHandler) StreamMediaSession(ctx context.Context, request *connect.Request[api.StreamMediaSessionRequest], stream *connect.ServerStream[api.StreamMediaSessionResponse]) error {
	original := ctx
	raw := taskRequest(request)
	_, err := withMediaLocator(h, ctx, raw.GetName(), func(ctx context.Context, name mediasessions.Name, registration mediaSourceRegistration, source mediacontent.Source, admission *mediasessions.Admission) (struct{}, error) {
		if !calendarKnown(raw) || stream == nil {
			return struct{}{}, taskInvalid()
		}
		if source.Binding().Kind() != mediasessions.Camera {
			return struct{}{}, mediacontent.ErrUnsupported
		}
		operation, err := h.sessions.BeginOperation(ctx, name.ID())
		if err != nil {
			return struct{}{}, err
		}
		record, err := h.sessions.Get(operation.Context(), admission, name.ID(), source.Binding())
		if err != nil {
			operation.Close()
			return struct{}{}, err
		}
		if err := h.sessions.ClaimVideo(operation, record); err != nil {
			operation.Close()
			return struct{}{}, err
		}
		defer func() { operation.Close(); _ = h.sessions.RetireVerified(record) }()
		bounded, cancel := context.WithDeadline(operation.Context(), record.ExpireTime())
		defer cancel()
		guarded, stop := mediaSourceLifetime(bounded, admission, registration, source)
		defer stop()
		bounded = guarded
		defer interruptCoreRPCWriteOnCancel(original, bounded)()
		video, err := registration.owner.Video(bounded, source.Binding())
		if err != nil {
			return struct{}{}, err
		}
		if nilReactiveInterface(video) {
			return struct{}{}, mediacontent.ErrInvalid
		}
		defer video.Close()
		header := video.Header()
		if !header.Valid() || !header.Matches(source.Binding()) {
			return struct{}{}, mediacontent.ErrInvalid
		}
		session, err := mediaSessionWire(name.Home(), record)
		if err != nil {
			return struct{}{}, err
		}
		if err := mediaFinalSource(bounded, admission, registration, source); err != nil {
			return struct{}{}, err
		}
		if _, err = h.sessions.Get(bounded, admission, name.ID(), source.Binding()); err != nil {
			return struct{}{}, err
		}
		if err := stream.Send(&api.StreamMediaSessionResponse{Sequence: 1, Frame: &api.StreamMediaSessionResponse_Header{Header: &api.MediaVideoHeader{MediaSession: session, Codec: api.MediaVideoCodec_MEDIA_VIDEO_CODEC_H264_BASELINE, Sps: header.SPS(), Pps: header.PPS(), Width: header.Width(), Height: header.Height(), FramesPerSecond: header.FramesPerSecond()}}}); err != nil {
			return struct{}{}, err
		}
		sequence := int64(1)
		last := time.Duration(-1)
		for {
			// Native Next owns pacing/backpressure; no detached frame queue is created.
			sample, err := video.Next(bounded)
			if err == io.EOF {
				return struct{}{}, nil
			}
			if err != nil {
				return struct{}{}, err
			}
			if !sample.Valid() || !sample.Matches(source.Binding()) || sample.Offset() <= last || last < 0 && !sample.Keyframe() || last >= 0 && sample.Offset()-last < time.Duration(1_000_000/int64(header.FramesPerSecond()))*time.Microsecond {
				return struct{}{}, mediacontent.ErrInvalid
			}
			if err := mediaFinalSource(bounded, admission, registration, source); err != nil {
				return struct{}{}, err
			}
			if _, err = h.sessions.Get(bounded, admission, name.ID(), source.Binding()); err != nil {
				return struct{}{}, err
			}
			if sequence == math.MaxInt64 {
				return struct{}{}, apierrors.New(api.ApiErrorReason_API_ERROR_REASON_QUERY_BUDGET_EXCEEDED)
			}
			sequence++
			last = sample.Offset()
			if !admission.Ready() {
				return struct{}{}, taskDenied()
			}
			if err := stream.Send(&api.StreamMediaSessionResponse{Sequence: sequence, Frame: &api.StreamMediaSessionResponse_Sample{Sample: &api.MediaVideoSample{MediaSession: name.String(), PresentationDuration: durationpb.New(sample.Offset()), Avcc: sample.AVCC()}}}); err != nil {
				return struct{}{}, err
			}
		}
	})
	return err
}

// The producer loop cannot check configuration while HTTP/2 Send is blocked.
// This bounded metadata guard owns no lease or frame queue; cancellation stops
// the native source and interrupts the protected writer on a generation change.
func mediaSourceLifetime(ctx context.Context, admission *mediasessions.Admission, registration mediaSourceRegistration, source mediacontent.Source) (context.Context, func()) {
	guarded, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-guarded.Done():
				return
			case <-ticker.C:
				if err := mediaFinalSource(guarded, admission, registration, source); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return guarded, func() { cancel(context.Canceled); <-done }
}
