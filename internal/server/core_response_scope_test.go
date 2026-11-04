package server

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
	"google.golang.org/protobuf/proto"
)

func TestNHA204CoreUnaryPolicyWithdrawalInterruptsHTTP2Send(t *testing.T) {
	for _, entityCase := range []bool{false, true} {
		t.Run(map[bool]string{false: "Home", true: "Entity"}[entityCase], func(t *testing.T) {
			f := nha165GrantSetup(t)
			home, _ := apitypes.ParseHomeName("homes/synthetic-response")
			metadata, _ := apitypes.NewHomeMetadata(home, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			owner := nha201ProjectionOwner{home: metadata, record: f.record}
			core := &coreRPCHandler{epoch: f.a.source}
			stamp, _ := view.NewConfigStamp(1, [32]byte{1})
			views, err := view.NewService(f.a.source, f.a, nha050ViewConfig{stamp}, sequencer.RealClock{}, core.capabilityProjector(owner, f.a.authority))
			if err != nil {
				t.Fatal(err)
			}
			handler, err := newHomeResourceHandler(owner, f.a.authority, views)
			if err != nil {
				t.Fatal(err)
			}
			_, rpc := apiv2connect.NewHomeServiceHandler(handler)
			path := apiv2connect.HomeServiceGetHomeProcedure
			var message proto.Message = &api.GetHomeRequest{Name: home.String()}
			if entityCase {
				_, rpc = apiv2connect.NewHomeServiceHandler(handler)
				path = apiv2connect.HomeServiceGetEntityProcedure
				_, id, _ := f.record.IDs()
				name, _ := apitypes.EntityNameForIdentity(home, id)
				message = &api.GetEntityRequest{Name: name.String()}
			}
			route := publicRoute{key: RouteKey{kind: RouteKindConnect, method: RouteMethodPost, path: path}, access: RouteAccessAnonymous, flow: RouteFlowUnary, handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, err := authorization.WithPrincipal(r.Context(), f.principal)
				if err != nil {
					t.Error(err)
					return
				}
				rpc.ServeHTTP(w, r.WithContext(context.WithValue(ctx, actorContextKey{}, f.actor)))
			})}
			compiled := &compiledPublic{routes: map[routeLookup]publicRoute{{method: http.MethodPost, path: path}: route}, auditor: &nha139Auditor{}}
			server := httptest.NewUnstartedServer(compiled.handler(config.DefaultPublicSettings(), newPublicAdmissions()))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			blocked, _ := nha147UnreadH2(t, ctx, server.URL, roots, path, message, "synthetic", 1)
			if blocked.State().StreamsActive != 1 {
				t.Fatal("response not blocked in real HTTP2 Send")
			}
			empty, err := authorization.EmptySource(f.account)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.a.authority.Publish(nha164AuthorityState(t, empty, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for blocked.State().StreamsActive != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if blocked.State().StreamsActive != 0 {
				t.Fatal("policy withdrawal left unary response blocked")
			}
		})
	}
}
