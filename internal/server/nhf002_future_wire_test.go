package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Future schema compatibility is a separate, explicitly synthetic wire fixture.
// It uses generated Go handlers/codecs and TLS, never a replacement core route.
// Native SQL/auth/core behavior is proved by TestNHF002TransportServer itself.
type nhf002FutureWire struct {
	statev1connect.UnimplementedStateServiceHandler
	states         []*statev1.ObservedState
	authorityCalls atomic.Uint64
}

func nhf002StartFutureWire(t *testing.T) (*httptest.Server, string, *nhf002FutureWire) {
	t.Helper()
	zero := uint64(0)
	future := &statev1.ObservedState{}
	future.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), []byte("future-value")))
	golden := &statev1.ObservedState{}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fixture source unavailable")
	}
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "contracts", "golden", "unknown-quality.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(encoded, golden); err != nil {
		t.Fatal(err)
	}
	fixture := &nhf002FutureWire{states: []*statev1.ObservedState{
		{Value: &statev1.ObservedState_SwitchOn{SwitchOn: false}},
		{Value: &statev1.ObservedState_Measurement{Measurement: &statev1.Measurement{Value: 0, Unit: "kW"}}, SourceSeq: &zero},
		{Value: &statev1.ObservedState_Light{Light: &statev1.LightValue{}}},
		{}, future, golden,
	}}
	path, handler := statev1connect.NewStateServiceHandler(fixture)
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer nhf002-future-wire-only" {
			http.Error(w, "synthetic credential required", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	data := shortPrivateDataDir(t)
	nha181CertificateFixtureFor(t, data, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	server.StartTLS()
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	return server, string(certificate), fixture
}

func (f *nhf002FutureWire) ReadPage(ctx context.Context, request *connect.Request[statev1.ReadPageRequest]) (*connect.Response[statev1.ReadPageResponse], error) {
	if request.Msg.SnapshotToken == "future-wire-deadline" {
		<-ctx.Done()
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("synthetic unary deadline"))
	}
	if request.Msg.SnapshotToken != "future-wire-only" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("synthetic snapshot required"))
	}
	return connect.NewResponse(&statev1.ReadPageResponse{Page: &statev1.OpenViewResponse{States: f.states}}), nil
}

func (f *nhf002FutureWire) WatchView(_ context.Context, request *connect.Request[statev1.WatchViewRequest], stream *connect.ServerStream[statev1.WatchViewResponse]) error {
	if request.Msg.GetCursor().GetEpoch() == "authority-unavailable" {
		f.authorityCalls.Add(1)
		detail, err := connect.NewErrorDetail(&authv1.AuthorizationTermination{Reason: authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_AUTHORITY_UNAVAILABLE})
		if err != nil {
			return err
		}
		failure := connect.NewError(connect.CodeUnavailable, errors.New("synthetic authority unavailable"))
		failure.AddDetail(detail)
		return failure
	}
	if err := stream.Send(&statev1.WatchViewResponse{StreamSequence: 1, Frame: &statev1.WatchViewResponse_Delta{Delta: &statev1.StateDelta{States: f.states}}}); err != nil {
		return err
	}
	future := &statev1.WatchViewResponse{StreamSequence: 2}
	future.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), []byte("future-frame")))
	return stream.Send(future)
}
