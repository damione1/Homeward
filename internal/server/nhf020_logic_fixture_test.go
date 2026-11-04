package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	cfg "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
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
	nha162RunBootstrap(t, ctx, data, login, password)
	authentication := apiv2connect.NewAuthServiceClient(client, base)
	accounts := apiv2connect.NewAccountServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, login, password)
	_, err = accounts.CreateAccount(ctx, nha056SessionRequest(&cfg.CreateAccountRequest{Parent: nha203SessionAccount(t, tokens.Session).Home().String(), Account: &cfg.Account{
		LoginName: "NHF020 keeper", DisplayName: "NHF020 keeper", InitialPassword: "NHF020 synthetic keeper password", Role: cfg.AccountRole_ACCOUNT_ROLE_ADMIN,
	}}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	keeper := nha162RunLogin(t, ctx, authentication, "NHF020 keeper", "NHF020 synthetic keeper password")
	documents := configurationv1connect.NewConfigurationServiceClient(client, base)
	before, err := documents.GetConfigurationDocument(ctx, nha056SessionRequest(&cfg.GetConfigurationDocumentRequest{Name: nha203DocumentName(t, ctx, run, "operational.main")}, keeper.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	handler := nhf020NewLogic(t, out, documents, accounts, before.Msg.Name)
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
				current, err := accounts.GetAccount(ctx, nha056SessionRequest(&cfg.GetAccountRequest{Name: nha203SessionAccount(t, tokens.Session).String()}, keeper.AccessToken))
				if err != nil {
					t.Fatal(err)
				}
				_, err = accounts.UpdateAccount(ctx, nha056SessionRequest(&cfg.UpdateAccountRequest{Account: &cfg.Account{Name: current.Msg.Name, Etag: current.Msg.Etag, Role: cfg.AccountRole_ACCOUNT_ROLE_USER}}, keeper.AccessToken))
				if err != nil {
					t.Fatal("synthetic role withdrawal", err)
				}
				revoked = true
				nhf003WritePrivate(t, out, "admin-revoked.json", map[string]bool{"revoked": true})
			}
			if _, err := os.Stat(filepath.Join(out, "stop")); err != nil {
				continue
			}
			after, err := documents.GetConfigurationDocument(ctx, nha056SessionRequest(&cfg.GetConfigurationDocumentRequest{Name: before.Msg.Name}, keeper.AccessToken))
			if err != nil || (!proto.Equal(before.Msg, after.Msg) || !proto.Equal(before.Msg.ActiveGeneration, after.Msg.ActiveGeneration)) {
				t.Fatal("logic fixture changed configuration", err)
			}
			instances, err := configurationv1connect.NewIntegrationServiceClient(client, base).ListIntegrationInstances(ctx, nha056SessionRequest(&cfg.ListIntegrationInstancesRequest{Parent: nha203HomeName(t, ctx, run)}, keeper.AccessToken))
			if err != nil || len(instances.Msg.IntegrationInstances) != 0 {
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
