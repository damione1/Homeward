package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	cfg "github.com/damione1/Homeward/gen/home/configuration/v1"
	"github.com/damione1/Homeward/gen/home/configuration/v1/configurationv1connect"
	integration "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/gen/home/integrationadmin/v1/integrationadminv1connect"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/proto"
)

// Explicit opt-in only. A fresh private PostgreSQL cluster, real Run, native
// sessions and verified TLS surround the synthetic logic surface. No installed
// configuration, Home Assistant endpoint or physical provider is admitted.
func TestNHF020LogicServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF020_FIXTURE")
	if out == "" {
		t.Skip("explicit isolated logic fixture only")
	}
	if os.Getenv("HOMEWARD_NHF020_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out {
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
	const login = "NHF020 isolated administrator"
	const password = "NHF020 synthetic isolated password"
	adminID := nha162RunBootstrap(t, ctx, data, login, password)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	accounts := authv1connect.NewAccountServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, login, password)
	_, err = accounts.CreateAccount(ctx, nha056SessionRequest(&auth.CreateAccountRequest{
		LoginName: "NHF020 keeper", DisplayName: "NHF020 keeper", InitialPassword: "NHF020 synthetic keeper password", Role: auth.AccountRole_ACCOUNT_ROLE_ADMIN,
	}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	keeper := nha162RunLogin(t, ctx, authentication, "NHF020 keeper", "NHF020 synthetic keeper password")
	documents := configurationv1connect.NewConfigurationServiceClient(client, base)
	before, err := documents.ReadDocument(ctx, nha056SessionRequest(&cfg.ReadDocumentRequest{Id: &cfg.DocumentId{Value: "operational.main"}}, keeper.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	handler := nhf020NewLogic(t, out, documents, accounts)
	origin := nhf020StartProxy(t, out, data, base, roots, handler)
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": origin, "ca_pem": string(certificate),
		"admin": map[string]string{"login_name": login, "password": password}, "production_configuration": false, "no_physical_providers": true})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	revoked, restarted := false, false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("logic fixture deadline", ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "restart")); err == nil && !restarted {
				handler.mu.Lock()
				handler.reset(before.Msg.ActiveGeneration)
				handler.counts.Restarts++
				handler.mu.Unlock()
				restarted = true
				nhf003WritePrivate(t, out, "restarted.json", map[string]bool{"synthetic_restart_injected": true})
			}
			if _, err := os.Stat(filepath.Join(out, "revoke-admin")); err == nil && !revoked {
				_, err := accounts.UpdateAccount(ctx, nha056SessionRequest(&auth.UpdateAccountRequest{AccountId: adminID, Role: auth.AccountRole_ACCOUNT_ROLE_USER.Enum()}, keeper.AccessToken))
				if err != nil {
					t.Fatal("synthetic role withdrawal", err)
				}
				revoked = true
				nhf003WritePrivate(t, out, "admin-revoked.json", map[string]bool{"revoked": true})
			}
			if _, err := os.Stat(filepath.Join(out, "stop")); err != nil {
				continue
			}
			after, err := documents.ReadDocument(ctx, nha056SessionRequest(&cfg.ReadDocumentRequest{Id: before.Msg.Document.Id}, keeper.AccessToken))
			if err != nil || (!proto.Equal(before.Msg.Document, after.Msg.Document) || !proto.Equal(before.Msg.ActiveGeneration, after.Msg.ActiveGeneration)) {
				t.Fatal("logic fixture changed configuration", err)
			}
			instances, err := integrationadminv1connect.NewIntegrationServiceClient(client, base).ListInstances(ctx, nha056SessionRequest(&integration.ListInstancesRequest{}, keeper.AccessToken))
			if err != nil || len(instances.Msg.Instances) != 0 {
				t.Fatal("physical provider introduced", err)
			}
			if err := run.join(t); err != nil {
				t.Fatal(err)
			}
			handler.mu.Lock()
			counts := handler.counts
			handler.mu.Unlock()
			nhf003WritePrivate(t, out, "result.json", map[string]any{"synthetic_only": true, "actual_run_joined": true, "no_physical_providers": true,
				"production_configuration": false, "configuration_unchanged": true, "counts": counts, "admin_role_withdrawn": revoked, "synthetic_restart_injected": restarted})
			return
		}
	}
}
