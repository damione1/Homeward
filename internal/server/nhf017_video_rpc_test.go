package server

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	cv "github.com/damione1/Homeward/gen/home/cameras/v1"
	"google.golang.org/protobuf/proto"
)

func nhf017EnableVideo(t *testing.T, r *nhf016CameraRig) {
	t.Helper()
	root := os.Getenv("HOMEWARD_NHF017_VIDEO")
	if root == "" {
		root = "../../qualification/frontend/video"
	}
	source, err := nhf017LoadVideoSource(root)
	if err != nil {
		t.Fatal(err)
	}
	r.handler.mu.Lock()
	r.handler.videoSource = source
	r.handler.mu.Unlock()
}
func nhf017WaitVideoJoined(t *testing.T, r *nhf016CameraRig) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.handler.videoStreams.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("video stream did not join")
		}
		time.Sleep(time.Millisecond)
	}
	r.handler.mu.Lock()
	defer r.handler.mu.Unlock()
	if len(r.handler.videoOwners) != 0 {
		t.Fatal("video owner slot survived cancellation")
	}
}

func TestNHF017VideoTLSAuthorizedMovingSourceAndOwnedCancellation(t *testing.T) {
	r := nhf016NewCameraRig(t, 60*time.Second)
	nhf017EnableVideo(t, r)
	lease := r.open(t, "driveway")
	anonymous, err := r.api.ReadVideo(r.ctx, connect.NewRequest(&cv.ReadVideoRequest{Session: lease.Session}))
	if err == nil {
		anonymous.Receive()
		err = anonymous.Err()
		anonymous.Close()
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("anonymous video", err)
	}
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	stream, err := r.api.ReadVideo(ctx, nha162Bearer(&cv.ReadVideoRequest{Session: lease.Session}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().Sequence != 1 || stream.Msg().GetHeader() == nil {
		t.Fatal("video header", stream.Err())
	}
	header := stream.Msg().GetHeader()
	if !proto.Equal(header.Lease, lease) || header.Codec != cv.VideoCodec_VIDEO_CODEC_H264_BASELINE || header.Width != 320 || header.Height != 180 || header.FramesPerSecond != 10 {
		t.Fatal("video source/lease changed")
	}
	var previous []byte
	for index := uint64(0); index < 3; index++ {
		if !stream.Receive() {
			t.Fatal(stream.Err())
		}
		sample := stream.Msg().GetSample()
		if stream.Msg().Sequence != index+2 || sample == nil || !proto.Equal(sample.Session, lease.Session) || sample.PresentationMicros != index*100_000 || len(sample.Avcc) == 0 || len(sample.Avcc) > 512*1024 {
			t.Fatal("video access unit identity/order/bound")
		}
		if previous != nil && bytes.Equal(previous, sample.Avcc) {
			t.Fatal("synthetic source did not move")
		}
		previous = bytes.Clone(sample.Avcc)
	}
	secondLease := r.open(t, "_driveway")
	blocked, err := r.api.ReadVideo(r.ctx, nha162Bearer(&cv.ReadVideoRequest{Session: secondLease.Session}, r.adminToken))
	if err == nil {
		blocked.Receive()
		err = blocked.Err()
		blocked.Close()
	}
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatal("second simultaneous video admitted", err)
	}
	cancel()
	stream.Close()
	nhf017WaitVideoJoined(t, r)
	fresh := r.open(t, "driveway")
	next, err := r.api.ReadVideo(r.ctx, nha162Bearer(&cv.ReadVideoRequest{Session: fresh.Session}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if !next.Receive() || next.Msg().GetHeader() == nil {
		t.Fatal("video slot not reusable", next.Err())
	}
	next.Close()
	nhf017WaitVideoJoined(t, r)
}

func TestNHF017VideoTLSForeignReferenceAndExplicitFallback(t *testing.T) {
	r := nhf016NewCameraRig(t, 60*time.Second)
	nhf017EnableVideo(t, r)
	lease := r.open(t, "driveway")
	other := r.login(t, "NHA162 Admin", "NHA162 synthetic admin password canary")
	foreign, err := r.api.ReadVideo(r.ctx, nha162Bearer(&cv.ReadVideoRequest{Session: lease.Session}, other))
	if err != nil {
		t.Fatal(err)
	}
	if !foreign.Receive() || foreign.Msg().GetRevoked() == nil {
		t.Fatal("foreign login reused native video reference", foreign.Err())
	}
	foreign.Close()
	nhf017WaitVideoJoined(t, r)
	wrong := proto.Clone(lease.Session).(*cv.SessionRef)
	wrong.EntityId = "_driveway"
	foreign, err = r.api.ReadVideo(r.ctx, nha162Bearer(&cv.ReadVideoRequest{Session: wrong}, r.adminToken))
	if err == nil {
		foreign.Receive()
		err = foreign.Err()
		foreign.Close()
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("foreign camera accepted", err)
	}
	nhf017WaitVideoJoined(t, r)
	for _, mode := range []nhf017VideoMode{nhf017VideoUnsupported, nhf017VideoNegotiationFailure} {
		lease = r.open(t, "driveway")
		r.handler.mu.Lock()
		r.handler.videoMode = mode
		r.handler.mu.Unlock()
		stream, err := r.api.ReadVideo(r.ctx, nha162Bearer(&cv.ReadVideoRequest{Session: lease.Session}, r.adminToken))
		if err != nil {
			t.Fatal(err)
		}
		if !stream.Receive() {
			t.Fatal("fallback absent", stream.Err())
		}
		switch mode {
		case nhf017VideoUnsupported:
			if stream.Msg().GetUnsupported() == nil {
				t.Fatal("codec fallback")
			}
		case nhf017VideoNegotiationFailure:
			if stream.Msg().GetUnavailable() == nil {
				t.Fatal("negotiation fallback")
			}
		case nhf017VideoAvailable:
			t.Fatal("invalid test mode")
		default:
			t.Fatal("invalid test mode")
		}
		if stream.Receive() || stream.Err() != nil {
			t.Fatal("fallback continued reading", stream.Err())
		}
		stream.Close()
		nhf017WaitVideoJoined(t, r)
	}
}

func TestNHF017VideoTLSExpiryCloseAndReadableRightsChange(t *testing.T) {
	r := nhf016NewCameraRig(t, 60*time.Second)
	nhf017EnableVideo(t, r)
	lease := r.open(t, "driveway")
	stream, err := r.api.ReadVideo(r.ctx, nha162Bearer(&cv.ReadVideoRequest{Session: lease.Session}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetHeader() == nil {
		t.Fatal("header", stream.Err())
	}
	r.handler.mu.Lock()
	for _, value := range r.handler.leases {
		value.wire.ExpiresAt = lease.IssuedAt
		nhf016EndCameraLease(value, nhf016LeaseExpired)
	}
	r.handler.mu.Unlock()
	for stream.Receive() {
		if stream.Msg().GetExpired() != nil {
			break
		}
		if stream.Msg().GetSample() == nil {
			t.Fatal("unexpected expiry transition")
		}
	}
	if stream.Msg().GetExpired() == nil {
		t.Fatal("video expiry", stream.Err())
	}
	stream.Close()
	nhf017WaitVideoJoined(t, r)
	lease = r.open(t, "driveway")
	stream, err = r.api.ReadVideo(r.ctx, nha162Bearer(&cv.ReadVideoRequest{Session: lease.Session}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetHeader() == nil {
		t.Fatal("new header", stream.Err())
	}
	nha162SetPolicy(t, r.nha162RPCRig, r.admin, nhf016AllCameras+`  - scope: {family: history, operation: read}
    objects: {entities: {all: {capabilities: {all: {}}}}}
`)
	for stream.Receive() {
		if stream.Msg().GetRevoked() != nil {
			break
		}
		if stream.Msg().GetSample() == nil {
			t.Fatal("unexpected binding transition")
		}
	}
	if stream.Msg().GetRevoked() == nil {
		t.Fatal("readable rights epoch retained video", stream.Err())
	}
	stream.Close()
	nhf017WaitVideoJoined(t, r)
	lease = r.open(t, "driveway")
	stream, err = r.api.ReadVideo(r.ctx, nha162Bearer(&cv.ReadVideoRequest{Session: lease.Session}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetHeader() == nil {
		t.Fatal("fresh header", stream.Err())
	}
	if _, err = r.api.CloseSession(r.ctx, nha162Bearer(&cv.CloseSessionRequest{Session: lease.Session}, r.adminToken)); err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
		if stream.Msg().GetRevoked() != nil {
			break
		}
	}
	if stream.Msg().GetRevoked() == nil {
		t.Fatal("close retained video", stream.Err())
	}
	stream.Close()
	nhf017WaitVideoJoined(t, r)
}
