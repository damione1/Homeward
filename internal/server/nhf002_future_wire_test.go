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
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apierrors"
	"google.golang.org/protobuf/encoding/protowire"
)

// Future schema compatibility is a separate, explicitly synthetic wire fixture.
// It uses generated Go handlers/codecs and TLS, never a replacement core route.
// Native SQL/auth/core behavior is proved by TestNHF002TransportServer itself.
type nhf002FutureWire struct {
	apiv2connect.UnimplementedEntityViewServiceHandler
	entries        []*api.EntityViewEntry
	authorityCalls atomic.Uint64
}

func nhf002StartFutureWire(t *testing.T) (*httptest.Server, string, *nhf002FutureWire) {
	t.Helper()
	future := &api.EntityViewEntry{Entity: &api.Entity{Name: "homes/synthetic/entities/future"}}
	future.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), []byte("future-value")))
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fixture source unavailable")
	}
	if _, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "contracts", "golden", "unknown-quality.bin")); err != nil {
		t.Fatal(err)
	}
	fixture := &nhf002FutureWire{entries: []*api.EntityViewEntry{
		{
			Entity: &api.Entity{Name: "homes/synthetic/entities/ent_1"},
			Capabilities: []*api.Capability{
				{
					Name:  "homes/synthetic/entities/ent_1/capabilities/power-relay",
					Trait: &api.Capability_Power{Power: &api.PowerCapability{Observation: &api.PowerObservation{Value: api.PowerValue_POWER_VALUE_OFF.Enum()}}},
				},
			},
		},
		future,
	}}
	path, handler := apiv2connect.NewEntityViewServiceHandler(fixture)
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

func (f *nhf002FutureWire) QueryEntityView(ctx context.Context, request *connect.Request[api.QueryEntityViewRequest]) (*connect.Response[api.QueryEntityViewResponse], error) {
	if request.Msg.PageToken == "future-wire-deadline" {
		<-ctx.Done()
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("synthetic unary deadline"))
	}
	if request.Msg.PageToken != "future-wire-only" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("synthetic snapshot required"))
	}
	return connect.NewResponse(&api.QueryEntityViewResponse{Entries: f.entries, ResumeToken: "future-wire-resume"}), nil
}

func (f *nhf002FutureWire) WatchEntityView(_ context.Context, request *connect.Request[api.WatchEntityViewRequest], stream *connect.ServerStream[api.WatchEntityViewResponse]) error {
	if request.Msg.GetResumeToken() == "authority-unavailable" {
		f.authorityCalls.Add(1)
		return apierrors.New(api.ApiErrorReason_API_ERROR_REASON_AUTHORITY_UNAVAILABLE).Connect()
	}
	if err := stream.Send(&api.WatchEntityViewResponse{ResumeToken: "1", Change: &api.WatchEntityViewResponse_Upsert{Upsert: f.entries[0]}}); err != nil {
		return err
	}
	future := &api.WatchEntityViewResponse{ResumeToken: "2"}
	future.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), []byte("future-frame")))
	return stream.Send(future)
}
