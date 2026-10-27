package server

import (
	"connectrpc.com/connect"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	integration "github.com/damione1/Homeward/gen/home/integrationadmin/v1"
	"github.com/damione1/Homeward/internal/testpg"
)

// An opt-in actual Run, with its own PostgreSQL cluster, TLS identity and source
// file. Only a disabled, local workday instance is configured. No installation
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
	create := func(name, password string, role auth.AccountRole) {
		t.Helper()
		_, err := f.accounts.CreateAccount(ctx, nha056SessionRequest(&auth.CreateAccountRequest{
			LoginName: name, DisplayName: name, InitialPassword: password, Role: role,
		}, f.token))
		if err != nil {
			t.Fatal("synthetic account creation", err)
		}
	}
	create("NHF018 keeper", "NHF018 synthetic keeper password", auth.AccountRole_ACCOUNT_ROLE_ADMIN)
	create("NHF018 ordinary", "NHF018 synthetic ordinary password", auth.AccountRole_ACCOUNT_ROLE_USER)
	f.createWorkday(t)
	// These two local cryptographic authenticators never use hardware or a device.
	enroll := func(name, password string, role auth.AccountRole) ([]string, *auth.SessionTokens) {
		t.Helper()
		create(name, password, role)
		plain := nha162RunLogin(t, ctx, f.authentication, name, password)
		authenticator := nha142NewAuthenticator(t)
		begun, err := f.authentication.BeginFactorEnrollment(ctx, nha056SessionRequest(&auth.BeginFactorEnrollmentRequest{Kind: auth.FactorKind_FACTOR_KIND_FIDO2}, plain.AccessToken))
		if err != nil {
			t.Fatal("synthetic enrollment begin", err)
		}
		activated, err := f.authentication.CompleteFactorEnrollment(ctx, nha056SessionRequest(&auth.CompleteFactorEnrollmentRequest{ChallengeId: begun.Msg.Challenge.Id, Attestation: authenticator.attestation(t, begun.Msg.Challenge)}, plain.AccessToken))
		if err != nil {
			t.Fatal("synthetic enrollment complete", err)
		}
		challenge, err := f.authentication.StartLogin(ctx, connect.NewRequest(&auth.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil || challenge.Msg.Decision.GetChallenge() == nil {
			t.Fatal("synthetic FIDO2 challenge", err)
		}
		completed, err := f.authentication.CompleteLogin(ctx, connect.NewRequest(&auth.CompleteLoginRequest{ChallengeId: challenge.Msg.Decision.GetChallenge().Id, Proof: authenticator.assertion(t, challenge.Msg.Decision.GetChallenge(), 0x05)}))
		if err != nil || completed.Msg.Decision.GetAuthenticated() == nil {
			t.Fatal("synthetic FIDO2 login", err)
		}
		return activated.Msg.RecoveryCodes, completed.Msg.Decision.GetAuthenticated()
	}
	factorCodes, _ := enroll("NHF018 factors", "NHF018 synthetic factors password", auth.AccountRole_ACCOUNT_ROLE_USER)
	witnessCodes, witness := enroll("NHF018 MFA witness", "NHF018 synthetic witness password", auth.AccountRole_ACCOUNT_ROLE_ADMIN)
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{
		"mode": "synthetic-only", "origin": "https://" + f.run.address.String(), "ca_pem": string(certificate),
		"admin":                    map[string]string{"login_name": "NHA148 admin", "password": "NHA148 synthetic isolated password"},
		"ordinary":                 map[string]string{"login_name": "NHF018 ordinary", "password": "NHF018 synthetic ordinary password"},
		"keeper":                   map[string]string{"login_name": "NHF018 keeper", "password": "NHF018 synthetic keeper password"},
		"factor":                   map[string]any{"login_name": "NHF018 factors", "password": "NHF018 synthetic factors password", "recovery_codes": factorCodes},
		"witness":                  map[string]any{"login_name": "NHF018 MFA witness", "password": "NHF018 synthetic witness password", "recovery_codes": witnessCodes},
		"production_configuration": false,
	})
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("administration fixture deadline", ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err != nil {
				continue
			}
			instances, err := f.api.ListInstances(ctx, nha056SessionRequest(&integration.ListInstancesRequest{}, witness.AccessToken))
			if err != nil || len(instances.Msg.Instances) != 1 || instances.Msg.Instances[0].TypeId != "workday" || instances.Msg.Instances[0].DesiredState != integration.DesiredState_DESIRED_STATE_DISABLED {
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
