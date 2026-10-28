package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/gen/home/companion/v1/companionv1connect"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	integration "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/gen/home/integrationadmin/v1/integrationadminv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
)

// Additional test-only services reuse exactly the same actual native guard.
type nhf021TestRoute struct {
	path        string
	handler     http.Handler
	isRead      func(string) bool
	onLostReply func()
}

// Same actual TLS/native-session guard as Run, on a fresh test-only endpoint.
// Identity cannot be supplied in a JSON claim or synthetic privilege header.
func nhf021StartProxy(t *testing.T, out, data, origin string, roots *x509.CertPool, run *nha162Running, h *nhf021Companion, extra ...nhf021TestRoute) string {
	t.Helper()
	target, err := url.Parse(origin)
	if err != nil || target.Scheme != "https" || target.Hostname() != "127.0.0.1" {
		t.Fatal("isolated TLS upstream required", err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	authenticator := &sessionAuth{store: run.observed.lists.store, coordinator: run.observed.sessions}
	path, handler := companionv1connect.NewCompanionServiceHandler(h, connect.WithReadMaxBytes(32*1024), connect.WithSendMaxBytes(32*1024))
	mux := http.NewServeMux()
	errorWriter := connect.NewErrorWriter()
	routes := append([]nhf021TestRoute{{path: path, handler: handler,
		isRead: func(procedure string) bool {
			return procedure == companionv1connect.CompanionServiceGetInstallationProcedure
		},
		onLostReply: func() { h.mu.Lock(); h.counts.LostReplies++; h.mu.Unlock() },
	}}, extra...)
	for _, route := range routes {
		if route.handler == nil || route.isRead == nil || route.onLostReply == nil {
			t.Fatal("complete synthetic route required")
		}
		mux.Handle(route.path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ctx, release, err := authenticator.AuthenticateWithLease(r.Context(), r)
			if err != nil {
				_ = errorWriter.Write(w, r, nhf021Error(connect.CodeUnauthenticated))
				return
			}
			defer release()
			ctx = context.WithValue(ctx, actorContextKey{}, actor)
			r = r.WithContext(ctx)
			if route.isRead(r.URL.Path) {
				marker := filepath.Join(out, "reject-next-read")
				if _, err := os.Stat(marker); err == nil {
					if err := os.Remove(marker); err != nil {
						_ = errorWriter.Write(w, r, nhf021Error(connect.CodeUnavailable))
						return
					}
					_ = errorWriter.Write(w, r, nhf021Error(connect.CodeUnavailable))
					return
				}
			}
			// Fault injection only removes an actual successful response. It never
			// causes a second call. The owner must resolve the original operation.
			marker := filepath.Join(out, "lose-next-reply")
			raw, err := os.ReadFile(marker)
			if err != nil && !os.IsNotExist(err) {
				_ = errorWriter.Write(w, r, nhf021Error(connect.CodeUnavailable))
				return
			}
			if string(raw) == r.URL.Path && !route.isRead(r.URL.Path) {
				if err := os.Remove(marker); err != nil {
					_ = errorWriter.Write(w, r, nhf021Error(connect.CodeUnavailable))
					return
				}
				recorded := httptest.NewRecorder()
				route.handler.ServeHTTP(recorded, r)
				if recorded.Code == http.StatusOK {
					route.onLostReply()
					panic(http.ErrAbortHandler)
				}
				for key, values := range recorded.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorded.Code)
				_, _ = io.Copy(w, recorded.Body)
				return
			}
			route.handler.ServeHTTP(w, r)
		}))
	}
	mux.Handle("/", proxy)
	server := httptest.NewUnstartedServer(mux)
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(func() { server.CloseClientConnections(); server.Close() })
	return server.URL
}

func TestNHF021CompanionServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF021_FIXTURE")
	if out == "" {
		t.Skip("explicit isolated companion fixture only")
	}
	if os.Getenv("HOMEWARD_NHF021_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("fresh absolute synthetic fixture required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML())
	captured, err := settings.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(source, captured.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(source, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	run := nha162StartDefaultRun(t, startup)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const adminLogin = "NHF021 isolated administrator"
	const adminPassword = "NHF021 isolated administrator password"
	nha162RunBootstrap(t, ctx, data, adminLogin, adminPassword)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	accounts := authv1connect.NewAccountServiceClient(client, base)
	admin := nha162RunLogin(t, ctx, authentication, adminLogin, adminPassword)
	const login = "NHF021 synthetic ordinary user"
	const password = "NHF021 synthetic ordinary password"
	user, err := accounts.CreateAccount(ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: login, DisplayName: login, InitialPassword: password, Role: auth.AccountRole_ACCOUNT_ROLE_USER}, admin.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	documents := configurationv1connect.NewConfigurationServiceClient(client, base)
	before, err := documents.ReadDocument(ctx, nha056SessionRequest(&cfg.ReadDocumentRequest{Id: &cfg.DocumentId{Value: "operational.main"}}, admin.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	handler := nhf021NewCompanion(run.observed.sessions.authority)
	origin := nhf021StartProxy(t, out, data, base, roots, run, handler)
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": origin, "ca_pem": string(certificate),
		"user": map[string]string{"login_name": login, "password": password}, "admin": map[string]string{"login_name": adminLogin, "password": adminPassword},
		"production_configuration": false, "no_physical_providers": true})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	revoked := false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("companion fixture deadline", ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "revoke-account")); err == nil && !revoked {
				_, err := accounts.UpdateAccount(ctx, nha056SessionRequest(&auth.UpdateAccountRequest{AccountId: user.Msg.Account.Id, Status: auth.AccountStatus_ACCOUNT_STATUS_DISABLED.Enum()}, admin.AccessToken))
				if err != nil {
					t.Fatal(err)
				}
				revoked = true
				nhf003WritePrivate(t, out, "account-revoked.json", map[string]bool{"revoked": true})
			}
			if _, err := os.Stat(filepath.Join(out, "stop")); err != nil {
				continue
			}
			after, err := documents.ReadDocument(ctx, nha056SessionRequest(&cfg.ReadDocumentRequest{Id: before.Msg.Document.Id}, admin.AccessToken))
			if err != nil || !proto.Equal(before.Msg.Document, after.Msg.Document) || !proto.Equal(before.Msg.ActiveGeneration, after.Msg.ActiveGeneration) {
				t.Fatal("companion changed configuration", err)
			}
			instances, err := integrationadminv1connect.NewIntegrationServiceClient(client, base).ListInstances(ctx, nha056SessionRequest(&integration.ListInstancesRequest{}, admin.AccessToken))
			if err != nil || len(instances.Msg.Instances) != 0 {
				t.Fatal("physical provider introduced", err)
			}
			if err := run.join(t); err != nil {
				t.Fatal(err)
			}
			handler.mu.Lock()
			counts := handler.counts
			handler.mu.Unlock()
			nhf003WritePrivate(t, out, "result.json", map[string]any{"synthetic_only": true, "actual_run_joined": true, "production_configuration": false, "configuration_unchanged": true, "no_physical_providers": true, "account_revoked": revoked, "counts": counts})
			return
		}
	}
}

var _ companionv1connect.CompanionServiceHandler = (*nhf021Companion)(nil)
