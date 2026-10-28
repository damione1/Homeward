package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

// Opt-in real Run with a fresh private PostgreSQL cluster/source/TLS identity.
// The supplied source has no physical provider; the native probe only adds
// comments to it. Simulation targets exist only in its explicit fixtures.
func TestNHF019ConfigurationServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF019_FIXTURE")
	if out == "" {
		t.Skip("explicit isolated configuration fixture only")
	}
	if os.Getenv("HOMEWARD_NHF019_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out {
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
	raw := captured.Bytes()
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "supplied-source.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(path, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	run := nha162StartDefaultRun(t, startup)
	client := nha162RunClient(t, roots, run.address)
	base := "https://" + run.address.String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const login = "NHF019 isolated administrator"
	const password = "NHF019 synthetic isolated password"
	adminID := nha162RunBootstrap(t, ctx, data, login, password)
	authentication := authv1connect.NewAuthServiceClient(client, base)
	accounts := authv1connect.NewAccountServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authentication, login, password)
	_, err = accounts.CreateAccount(ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "NHF019 keeper", DisplayName: "NHF019 keeper", InitialPassword: "NHF019 synthetic keeper password", Role: auth.AccountRole_ACCOUNT_ROLE_ADMIN}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	keeper := nha162RunLogin(t, ctx, authentication, "NHF019 keeper", "NHF019 synthetic keeper password")
	documents := configurationv1connect.NewConfigurationServiceClient(client, base)
	before, err := documents.ReadDocument(ctx, nha056SessionRequest(&cfg.ReadDocumentRequest{Id: &cfg.DocumentId{Value: "operational.main"}}, keeper.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	faultProxy := nhf019StartFaultProxy(t, out, data, base, roots, documents)
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": faultProxy.url, "ca_pem": string(certificate), "admin": map[string]string{"login_name": login, "password": password}, "production_configuration": false, "no_physical_providers": true})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	revoked := false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("configuration fixture deadline", ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "revoke-admin")); err == nil && !revoked {
				_, err = accounts.UpdateAccount(ctx, nha056SessionRequest(&auth.UpdateAccountRequest{AccountId: adminID, Role: auth.AccountRole_ACCOUNT_ROLE_USER.Enum()}, keeper.AccessToken))
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
			if err != nil {
				t.Fatal(err)
			}
			current, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(current)
			if !bytes.Equal(current, raw) || !bytes.Equal(after.Msg.Document.Version.SourceSha256, sum[:]) || after.Msg.Document.Version.Revision <= before.Msg.Document.Version.Revision || after.Msg.ActiveGeneration.Revision <= before.Msg.ActiveGeneration.Revision {
				t.Fatal("source rollback/counter evidence incomplete")
			}
			instances, err := integrationadminv1connect.NewIntegrationServiceClient(client, base).ListInstances(ctx, nha056SessionRequest(&integration.ListInstancesRequest{}, keeper.AccessToken))
			if err != nil || len(instances.Msg.Instances) != 0 {
				t.Fatal("physical provider introduced into synthetic fixture", err)
			}
			if err := run.join(t); err != nil {
				t.Fatal(err)
			}
			nhf003WritePrivate(t, out, "result.json", map[string]any{"synthetic_only": true, "actual_run_joined": true, "production_configuration": false, "no_physical_providers": true, "root_source_restored": true, "source_sha256": hex.EncodeToString(sum[:]), "before_revision": before.Msg.Document.Version.Revision, "after_revision": after.Msg.Document.Version.Revision, "admin_role_withdrawn": revoked, "fault_counts": faultProxy.snapshot()})
			return
		}
	}
}
