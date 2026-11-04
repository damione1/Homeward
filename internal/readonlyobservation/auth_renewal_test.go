package readonlyobservation

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type renewalProbe struct {
	apiv2connect.UnimplementedAuthServiceHandler
	mu        sync.Mutex
	keys      [][]byte
	starts    int
	malformed bool
	refused   bool
}

func (p *renewalProbe) StartLogin(context.Context, *connect.Request[api.StartLoginRequest]) (*connect.Response[api.StartLoginResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	return nil, connect.NewError(connect.CodeInternal, nil)
}

func (p *renewalProbe) RefreshSession(_ context.Context, r *connect.Request[api.RefreshSessionRequest]) (*connect.Response[api.RefreshSessionResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Msg.RefreshToken != "old-family-token" || len(r.Msg.RenewalKey) != 32 || bytes.Equal(r.Msg.RenewalKey, make([]byte, 32)) {
		return nil, connect.NewError(connect.CodeInvalidArgument, nil)
	}
	p.keys = append(p.keys, bytes.Clone(r.Msg.RenewalKey))
	if p.refused {
		return nil, connect.NewError(connect.CodeUnauthenticated, nil)
	}
	if len(p.keys) == 1 {
		if p.malformed {
			return connect.NewResponse(&api.RefreshSessionResponse{Tokens: &api.SessionTokens{AccessToken: "incomplete"}}), nil
		}
		return nil, connect.NewError(connect.CodeUnavailable, nil)
	}
	now := time.Now().UTC()
	return connect.NewResponse(&api.RefreshSessionResponse{Tokens: &api.SessionTokens{
		Session:  &api.Session{Name: "homes/synthetic/accounts/reader/sessions/current", CreateTime: timestamppb.New(now.Add(-time.Minute)), ExpireTime: timestamppb.New(now.Add(time.Hour)), AuthenticationSatisfaction: api.AuthenticationSatisfaction_AUTHENTICATION_SATISFACTION_NOT_RECORDED},
		Audience: api.CredentialAudience_CREDENTIAL_AUDIENCE_INTERACTIVE, AccessToken: "new-access", RefreshToken: "new-family-token", AccessExpireTime: timestamppb.New(now.Add(10 * time.Minute)),
	}}), nil
}

func TestAPIObservationRenewalRetainsExactKeyAfterUnknownReply(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "malformed"}[malformed], func(t *testing.T) {
			probe := &renewalProbe{malformed: malformed}
			_, handler := apiv2connect.NewAuthServiceHandler(probe)
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			source := &NativeSource{auth: apiv2connect.NewAuthServiceClient(server.Client(), server.URL), access: "old-access", refresh: "old-family-token", expires: time.Now().Add(-time.Second)}
			if err := source.session(t.Context()); err == nil {
				t.Fatal("ambiguous renewal reported success")
			}
			pending := bytes.Clone(source.renewalKey)
			if len(pending) != 32 || source.refresh != "old-family-token" || source.access != "old-access" {
				t.Fatal("uncertain renewal erased its reconciliation state")
			}
			// Even a still usable access token cannot bypass pending reconciliation.
			source.expires = time.Now().Add(10 * time.Minute)
			if err := source.session(t.Context()); err != nil {
				t.Fatal(err)
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if probe.starts != 0 || len(probe.keys) != 2 || !bytes.Equal(probe.keys[0], pending) || !bytes.Equal(probe.keys[1], pending) {
				t.Fatal("renewal changed key or fell back to password login")
			}
			if source.refresh != "new-family-token" || source.access != "new-access" || len(source.renewalKey) != 0 {
				t.Fatal("admitted successor did not replace pending state")
			}
		})
	}
}

func TestAPIObservationRenewalRefusalNeverStartsPasswordLogin(t *testing.T) {
	probe := &renewalProbe{refused: true}
	_, handler := apiv2connect.NewAuthServiceHandler(probe)
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	source := &NativeSource{auth: apiv2connect.NewAuthServiceClient(server.Client(), server.URL), refresh: "old-family-token"}
	for range 2 {
		if err := source.session(t.Context()); err == nil {
			t.Fatal("refused renewal reported success")
		}
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.starts != 0 || len(probe.keys) != 2 || !bytes.Equal(probe.keys[0], probe.keys[1]) {
		t.Fatal("credential refusal silently started a different session")
	}
}
