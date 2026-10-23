package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
)

type nha165RotationService struct {
	statev1connect.UnimplementedStateServiceHandler
	remote   error
	deadline chan bool
	done     chan struct{}
}

func (s *nha165RotationService) WatchView(ctx context.Context, _ *connect.Request[state.WatchViewRequest], stream *connect.ServerStream[state.WatchViewResponse]) error {
	defer close(s.done)
	_, hasDeadline := ctx.Deadline()
	s.deadline <- hasDeadline
	if err := stream.Send(&state.WatchViewResponse{StreamSequence: 1}); err != nil {
		return err
	}
	if s.remote != nil {
		return s.remote
	}
	<-ctx.Done()
	return ctx.Err()
}

func nha165RotationRPC(t *testing.T, remote error) (statev1connect.StateServiceClient, *nha165RotationService) {
	t.Helper()
	service := &nha165RotationService{remote: remote, deadline: make(chan bool, 1), done: make(chan struct{})}
	_, handler := statev1connect.NewStateServiceHandler(service)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client() // trusts only this disposable TLS server
	client.Timeout = 0        // same streaming transport policy as the real profile
	return statev1connect.NewStateServiceClient(client, server.URL), service
}

func nha165RotationJoined(t *testing.T, service *nha165RotationService) {
	t.Helper()
	select {
	case <-service.done:
	case <-time.After(2 * time.Second):
		t.Fatal("real Connect handler did not join after cancellation")
	}
}

func TestNHA165RotationOwnedTimerCancelsRealConnectWithoutRPCDeadline(t *testing.T) {
	api, service := nha165RotationRPC(t, nil)
	// Only this unit uses an accelerated timer; real integration keeps4min.
	rotation := nha165NewStreamRotation(context.Background(), 500*time.Millisecond)
	defer rotation.close()
	stream, err := api.WatchView(rotation.ctx, connect.NewRequest(&state.WatchViewRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().StreamSequence != 1 {
		t.Fatal("real TLS stream did not publish before rotation", stream.Err())
	}
	if <-service.deadline {
		t.Fatal("owned rotation leaked a deadline into the real RPC")
	}
	if stream.Receive() || connect.CodeOf(stream.Err()) != connect.CodeCanceled {
		t.Fatal("owned timer did not cancel Receive", stream.Err())
	}
	if !rotation.planned(stream.Err()) || !errors.Is(context.Cause(rotation.ctx), nha165RotateStream) {
		t.Fatal("real timer cancellation not identified by its owned cause")
	}
	stream.Close()
	nha165RotationJoined(t, service)
}

func TestNHA165RotationRemoteDeadlineAndParentCancellationStayFatal(t *testing.T) {
	for _, mode := range []string{"remote_deadline", "parent_cancel"} {
		t.Run(mode, func(t *testing.T) {
			var remote error
			if mode == "remote_deadline" {
				remote = connect.NewError(connect.CodeDeadlineExceeded, errors.New("early remote deadline"))
			}
			api, service := nha165RotationRPC(t, remote)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			rotation := nha165NewStreamRotation(parent, time.Hour)
			defer rotation.close()
			stream, err := api.WatchView(rotation.ctx, connect.NewRequest(&state.WatchViewRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if !stream.Receive() {
				t.Fatal("real TLS stream did not start", stream.Err())
			}
			if mode == "parent_cancel" {
				cancel()
			}
			if stream.Receive() {
				t.Fatal("unexpected extra frame")
			}
			want := connect.CodeDeadlineExceeded
			if mode == "parent_cancel" {
				want = connect.CodeCanceled
			}
			if connect.CodeOf(stream.Err()) != want || rotation.planned(stream.Err()) {
				t.Fatalf("real early failure was hidden: error=%v cause=%v", stream.Err(), context.Cause(rotation.ctx))
			}
			stream.Close()
			nha165RotationJoined(t, service)
		})
	}
}

func TestNHA165RotationDeadlineOrEOFNeverBecomePlannedAfterTimer(t *testing.T) {
	for _, terminal := range []error{context.DeadlineExceeded, connect.NewError(connect.CodeDeadlineExceeded, errors.New("remote")), nil} {
		rotation := nha165NewStreamRotation(context.Background(), time.Millisecond)
		select {
		case <-rotation.ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("actual owned timer did not fire")
		}
		if rotation.planned(terminal) {
			t.Fatalf("deadline/EOF treated as rotation: %v", terminal)
		}
		rotation.close() // proves Stop/Join is idempotent after classification
	}
}
