package server

import (
	"connectrpc.com/connect"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cfg "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// An opt-in actual Run, with its own PostgreSQL cluster, TLS identity and source
// file. A local workday and a disabled opaque worker are configured. No installation
// configuration, physical provider, production account or device is accepted.
func TestNHF018AdministrationServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF018_FIXTURE")
	if out == "" {
		t.Skip("explicit isolated administration fixture only")
	}
	if os.Getenv("HOMEWARD_NHF018_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || filepath.Clean(out) != out {
		t.Fatal("fresh absolute synthetic-only administration fixture required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha181Config(t, cluster.Port, data, "127.0.0.1:0", strings.Replace(nha181TLSYAML(), "public:\n", "public:\n  url: https://localhost\n", 1))
	captured, err := settings.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "configuration.yaml")
	if err := os.WriteFile(path, captured.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	f := &nha148TLSFixture{ctx: ctx, data: data, path: path, roots: roots}
	f.start(t)
	nha162RunBootstrap(t, ctx, data, "NHA148 admin", "NHA148 synthetic isolated password")
	f.login(t)
	create := func(name, password string, role cfg.AccountRole) {
		t.Helper()
		_, err := f.accounts.CreateAccount(ctx, nha056SessionRequest(&cfg.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &cfg.Account{
			LoginName: name, DisplayName: name, InitialPassword: password, Role: role,
		}}, f.token))
		if err != nil {
			t.Fatal("synthetic account creation", err)
		}
	}
	create("NHF018 keeper", "NHF018 synthetic keeper password", cfg.AccountRole_ACCOUNT_ROLE_ADMIN)
	create("NHF018 ordinary", "NHF018 synthetic ordinary password", cfg.AccountRole_ACCOUNT_ROLE_USER)
	before := f.read(t)
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("external_workers:\n  - instance: nha148-opaque\n    executable: /usr/bin/true\n    args: [NHA148_PRIVATE_OPAQUE_ARGUMENT]\n    secret_names: [opaque-ref]\ndisabled_integrations: [nha148-opaque]\n")...)
	_, err = f.documents.PublishConfigurationDocument(ctx, nha056SessionRequest(&cfg.PublishConfigurationDocumentRequest{Name: before.Name, Expected: before.Version, ExpectedWorkspaceSha256: before.GetWorkspace().SourceSha256, ExactYamlSource: raw}, f.token))
	if err != nil {
		t.Fatal("synthetic disabled opaque source", err)
	}
	f.createWorkday(t)
	// Managed synthetic values exercise reference preservation without a broker.
	f.seed(t, "user-ref", "NHF018_PRIVATE_SYNTHETIC_USER")
	f.seed(t, "password-ref", "NHF018_PRIVATE_SYNTHETIC_PASSWORD")
	// These two local cryptographic authenticators never use hardware or a device.
	enroll := func(name, password string, role cfg.AccountRole) ([]string, *cfg.SessionTokens) {
		t.Helper()
		create(name, password, role)
		plain := nha162RunLogin(t, ctx, f.authentication, name, password)
		authenticator := nha142NewAuthenticator(t)
		begun, err := f.authentication.BeginFactorEnrollment(ctx, nha056SessionRequest(&cfg.BeginFactorEnrollmentRequest{Parent: nha203SessionAccount(t, plain.Session).String(), Kind: cfg.FactorKind_FACTOR_KIND_FIDO2}, plain.AccessToken))
		if err != nil {
			t.Fatal("synthetic enrollment begin", err)
		}
		activated, err := f.authentication.CompleteFactorEnrollment(ctx, nha056SessionRequest(&cfg.CompleteFactorEnrollmentRequest{Parent: nha203SessionAccount(t, plain.Session).String(), ChallengeId: begun.Msg.Challenge.ChallengeId, Attestation: authenticator.attestation(t, begun.Msg.Challenge)}, plain.AccessToken))
		if err != nil {
			t.Fatal("synthetic enrollment complete", err)
		}
		challenge, err := f.authentication.StartLogin(ctx, connect.NewRequest(&cfg.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil || challenge.Msg.Decision.GetChallenge() == nil {
			t.Fatal("synthetic FIDO2 challenge", err)
		}
		completed, err := f.authentication.CompleteLogin(ctx, connect.NewRequest(&cfg.CompleteLoginRequest{ChallengeId: challenge.Msg.Decision.GetChallenge().ChallengeId, Proof: authenticator.assertion(t, challenge.Msg.Decision.GetChallenge(), 0x05)}))
		if err != nil || completed.Msg.Decision.GetAuthenticated() == nil {
			t.Fatal("synthetic FIDO2 login", err)
		}
		return activated.Msg.RecoveryCodes, completed.Msg.Decision.GetAuthenticated()
	}
	factorCodes, _ := enroll("NHF018 factors", "NHF018 synthetic factors password", cfg.AccountRole_ACCOUNT_ROLE_USER)
	witnessCodes, witness := enroll("NHF018 MFA witness", "NHF018 synthetic witness password", cfg.AccountRole_ACCOUNT_ROLE_ADMIN)
	faultOrigin := nhf018ObservationFixture(t, f, out)
	nativeInvitation, err := f.accounts.IssueInvitation(ctx, nha056SessionRequest(&cfg.IssueInvitationRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), LoginName: "NHF018 native invited", Role: cfg.AccountRole_ACCOUNT_ROLE_USER, ExpireTime: timestamppb.New(time.Now().Add(time.Hour))}, f.token))
	if err != nil {
		t.Fatal("native synthetic invitation", err)
	}
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{
		"mode": "synthetic-only", "origin": "https://" + f.run.address.String(), "ca_pem": string(certificate),
		"fault_origin":             faultOrigin,
		"native_invitation":        map[string]string{"login_name": "NHF018 native invited", "token": nativeInvitation.Msg.InvitationToken},
		"admin":                    map[string]string{"login_name": "NHA148 admin", "password": "NHA148 synthetic isolated password"},
		"ordinary":                 map[string]string{"login_name": "NHF018 ordinary", "password": "NHF018 synthetic ordinary password"},
		"keeper":                   map[string]string{"login_name": "NHF018 keeper", "password": "NHF018 synthetic keeper password"},
		"factor":                   map[string]any{"login_name": "NHF018 factors", "password": "NHF018 synthetic factors password", "recovery_codes": factorCodes},
		"witness":                  map[string]any{"login_name": "NHF018 MFA witness", "password": "NHF018 synthetic witness password", "recovery_codes": witnessCodes},
		"production_configuration": false,
	})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	evicted := false
	for {
		select {
		case <-ctx.Done():
			t.Fatal("administration fixture deadline", ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "evict-logs")); err == nil && !evicted {
				nhf018SeedLogs(t, f, 1200)
				evicted = true
				if err := os.WriteFile(filepath.Join(out, "logs-evicted"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(out, "stop")); err != nil {
				continue
			}
			instances, err := f.api.ListIntegrationInstances(ctx, nha056SessionRequest(&cfg.ListIntegrationInstancesRequest{Parent: nha203HomeName(t, ctx, f.run)}, witness.AccessToken))
			if err != nil || len(instances.Msg.IntegrationInstances) != 1 || instances.Msg.IntegrationInstances[0].IntegrationType != f.typeName(t, "workday") || instances.Msg.IntegrationInstances[0].DesiredState != cfg.IntegrationDesiredState_INTEGRATION_DESIRED_STATE_DISABLED {
				t.Fatal("isolated configuration changed beyond disabled workday", err)
			}
			if err := f.run.join(t); err != nil {
				t.Fatal("actual Run shutdown", err)
			}
			nhf003WritePrivate(t, out, "result.json", map[string]any{
				"synthetic_only": true, "actual_run_joined": true,
				"only_disabled_workday": true, "production_configuration": false,
			})
			return
		}
	}
}
