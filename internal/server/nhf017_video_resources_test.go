package server

import (
	"bytes"
	"connectrpc.com/connect"
	"context"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
	"os"
	"testing"
	"time"
)

func nhf017EnableResourceVideo(t *testing.T, r *nhf016ResourceRig) {
	t.Helper()
	root := os.Getenv("HOMEWARD_NHF017_VIDEO")
	if root == "" {
		root = "../../qualification/frontend/video"
	}
	source, err := nha202LoadVideoSource(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range r.owners {
		owner.mu.Lock()
		owner.video = source
		owner.mu.Unlock()
	}
}
func nhf017WaitResourceVideoJoined(t *testing.T, r *nhf016ResourceRig) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		active := int64(0)
		for _, owner := range r.owners {
			active += owner.activeVideos.Load()
		}
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("native video owner did not join")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestNHF017ResourceTLSMovingVideoOwnedCancellation(t *testing.T) {
	r := nhf016NewResourceRig(t, 60*time.Second)
	nhf017EnableResourceVideo(t, r)
	sources := r.catalog(t)
	session := r.open(t, sources[0].Name)
	anonymous, err := r.api.StreamMediaSession(r.ctx, connect.NewRequest(&api.StreamMediaSessionRequest{Name: session.Name}))
	if err == nil {
		anonymous.Receive()
		err = anonymous.Err()
		anonymous.Close()
	}
	nha162Code(t, err, connect.CodeUnauthenticated)
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	stream, err := r.api.StreamMediaSession(ctx, nha162Bearer(&api.StreamMediaSessionRequest{Name: session.Name}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().Sequence != 1 || stream.Msg().GetHeader() == nil {
		t.Fatal("video header", stream.Err())
	}
	header := stream.Msg().GetHeader()
	if !proto.Equal(header.MediaSession, session) || header.Codec != api.MediaVideoCodec_MEDIA_VIDEO_CODEC_H264_BASELINE || header.Width != 320 || header.Height != 180 || header.FramesPerSecond != 10 {
		t.Fatal("native video identity or metadata changed")
	}
	var previous []byte
	for index := int64(0); index < 3; index++ {
		if !stream.Receive() {
			t.Fatal(stream.Err())
		}
		sample := stream.Msg().GetSample()
		if stream.Msg().Sequence != index+2 || sample == nil || sample.MediaSession != session.Name || sample.PresentationDuration.AsDuration() != time.Duration(index)*100*time.Millisecond || len(sample.Avcc) == 0 || len(sample.Avcc) > 512*1024 {
			t.Fatal("video identity/order/bounds")
		}
		if previous != nil && bytes.Equal(previous, sample.Avcc) {
			t.Fatal("synthetic video not moving")
		}
		previous = bytes.Clone(sample.Avcc)
	}
	second := r.open(t, sources[1].Name)
	blocked, err := r.api.StreamMediaSession(r.ctx, nha162Bearer(&api.StreamMediaSessionRequest{Name: second.Name}, r.adminToken))
	if err == nil {
		blocked.Receive()
		err = blocked.Err()
		blocked.Close()
	}
	nha162Code(t, err, connect.CodeResourceExhausted)
	cancel()
	stream.Close()
	nhf017WaitResourceVideoJoined(t, r)
	fresh := r.open(t, sources[0].Name)
	next, err := r.api.StreamMediaSession(r.ctx, nha162Bearer(&api.StreamMediaSessionRequest{Name: fresh.Name}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if !next.Receive() || next.Msg().GetHeader() == nil {
		t.Fatal("video slot not reusable", next.Err())
	}
	next.Close()
	nhf017WaitResourceVideoJoined(t, r)
}
func TestNHF017ResourceTLSCredentialAndExplicitFailure(t *testing.T) {
	r := nhf016NewResourceRig(t, 60*time.Second)
	nhf017EnableResourceVideo(t, r)
	source := r.catalog(t)[0].Name
	session := r.open(t, source)
	other := r.login(t, "NHA162 Admin", "NHA162 synthetic admin password canary")
	foreign, err := r.api.StreamMediaSession(r.ctx, nha162Bearer(&api.StreamMediaSessionRequest{Name: session.Name}, other))
	if err == nil {
		if foreign.Receive() {
			foreign.Close()
			t.Fatal("foreign credential received native video content")
		}
		err = foreign.Err()
		foreign.Close()
	}
	nha162Code(t, err, connect.CodeNotFound)
	nhf017WaitResourceVideoJoined(t, r)
	owner := r.owners[source]
	for _, unsupported := range []bool{true, false} {
		session = r.open(t, source)
		owner.mu.Lock()
		owner.videoUnsupported = unsupported
		owner.videoUnavailable = !unsupported
		owner.mu.Unlock()
		stream, e := r.api.StreamMediaSession(r.ctx, nha162Bearer(&api.StreamMediaSessionRequest{Name: session.Name}, r.adminToken))
		if e == nil {
			if stream.Receive() {
				t.Fatal("failed source emitted decodable frame")
			}
			e = stream.Err()
			stream.Close()
		}
		if e == nil {
			t.Fatal("native failure missing")
		}
		reason := nha200Info(t, e).Reason
		if unsupported && reason != "CAPABILITY_UNAVAILABLE" || !unsupported && reason != "SERVICE_UNAVAILABLE" {
			t.Fatal("wrong explicit video failure", reason)
		}
		nhf017WaitResourceVideoJoined(t, r)
	}
}
func TestNHF017ResourceTLSExpiryDeleteAndReadableRightsChange(t *testing.T) {
	r := nhf016NewResourceRig(t, 60*time.Second)
	nhf017EnableResourceVideo(t, r)
	source := r.catalog(t)[0].Name
	for _, reason := range []string{"expiry", "delete", "readable-epoch"} {
		var session *api.MediaSession
		if reason == "expiry" {
			session = r.openLifetime(t, source, 300*time.Millisecond)
		} else {
			session = r.open(t, source)
		}
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		stream, err := r.api.StreamMediaSession(ctx, nha162Bearer(&api.StreamMediaSessionRequest{Name: session.Name}, r.adminToken))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if !stream.Receive() || stream.Msg().GetHeader() == nil {
			t.Fatal("video header", stream.Err())
		}
		switch reason {
		case "expiry":
		case "delete":
			if err = r.closeSession(r.ctx, session.Name, r.adminToken); err != nil {
				t.Fatal(err)
			}
		case "readable-epoch":
			nha162SetPolicy(t, r.nha162RPCRig, r.admin, nhf016AllCameras+"  - scope: {family: history, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
		}
		for stream.Receive() {
			if stream.Msg().GetSample() == nil {
				t.Fatal("unexpected video frame")
			}
		}
		if stream.Err() == nil {
			t.Fatal("retired video lacks terminal refusal", reason)
		}
		stream.Close()
		cancel()
		nhf017WaitResourceVideoJoined(t, r)
	}
}
