package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

type nhf003SessionMode string

const (
	nhf003Password   nhf003SessionMode = "password"
	nhf003Recovery   nhf003SessionMode = "recovery"
	nhf003Enrollment nhf003SessionMode = "enrollment"
	nhf003RateLimit  nhf003SessionMode = "rate-limit"
)

func nhf003ParseSessionMode(value string) (nhf003SessionMode, error) {
	mode := nhf003SessionMode(value)
	switch mode {
	case nhf003Password, nhf003Recovery, nhf003Enrollment, nhf003RateLimit:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown synthetic session mode %q", value)
	}
}

func nhf003WritePrivate(t *testing.T, directory, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(directory, name+".writing")
	f, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(temporary, filepath.Join(directory, name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(temporary); err != nil {
		t.Fatal(err)
	}
}

// Native SQL/auth/MFA routes, operator bootstrap and TLS; no pilots, physical
// credentials, configured identity source or action outputs. P256 software test
// authenticators prepare server state only, never qualify a client FIDO2 device.
func TestNHF003SessionServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF003_FIXTURE")
	if out == "" {
		t.Skip("explicitly selected by the native session qualification harness")
	}
	if !filepath.IsAbs(out) {
		t.Fatal("fresh absolute fixture directory required")
	}
	modeInput := os.Getenv("HOMEWARD_NHF003_MODE")
	if modeInput == "" {
		modeInput = string(nhf003Password)
	}
	mode, err := nhf003ParseSessionMode(modeInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixtureWith(t, data, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), func(cert *x509.Certificate) {
		cert.DNSNames = append(cert.DNSNames, "nhf003.example.test")
	})
	public := strings.Replace(nha181TLSYAML(), "public:\n", "public:\n  url: https://nhf003.example.test\n", 1)
	settings := nha181Config(t, cluster.Port, data, "127.0.0.1:0", public)
	ctx, signals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signals()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var host *integrations.Manager
	var activation *configactivation.Owner
	instance, _ := domain.ParseIntegrationInstanceID("nhf003-synthetic-only")
	sourceKey, _ := integrations.NewSourceKey("switch", "relay")
	factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.LoadManifest(scope.BaselineV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		if _, err := repository.ConfigureBinding(factoryCtx, instance, sourceKey, "switch.basement_relay"); err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: repository, ConfigureActivation: func(_ context.Context, actual *configactivation.Owner) error {
			activation = actual
			return nil
		}, Configure: func(_ context.Context, actual *integrations.Manager, _ *storage.Store) error {
			host = actual
			return nil
		}}, nil
	})
	ready, done := make(chan net.Addr, 1), make(chan error, 1)
	go func() {
		defer close(done)
		done <- RunWithStartupFactory(ctx, settings.startup, (&net.ListenConfig{}).Listen, func(a net.Addr) { ready <- a }, factory)
	}()
	var proxy *nhf003Proxy
	defer func() {
		if proxy != nil {
			proxy.close()
		}
		cancel()
		joined := false
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("native session fixture did not join")
		}
		if host == nil {
			return
		}
		result := map[string]any{"server_joined": joined, "command_driver_entries": host.CommandDriverEntries(), "mode": mode}
		if proxy != nil {
			result["start_login_calls"] = proxy.login.Load()
			result["complete_login_calls"] = proxy.complete.Load()
			result["refresh_calls"] = proxy.refresh.Load()
			result["revoke_calls"] = proxy.revoke.Load()
			result["lost_refresh_responses"] = proxy.lost.Load()
			result["shortened_access_responses"] = proxy.shortened.Load()
			result["login_observation"] = proxy.loginObservation()
			result["state_read_calls"] = proxy.stateReads.Load()
			result["state_read_ok"] = proxy.stateReadOK.Load()
			result["revoke_ok"] = proxy.revokeOK.Load()
		}
		nhf003WritePrivate(t, out, "result.json", result)
		if host.CommandDriverEntries() != 0 {
			t.Error("synthetic session fixture entered a command driver")
		}
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatal("native auth server exited before ready:", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if host == nil || activation == nil {
		t.Fatal("native runtime not composed")
	}
	const login, password = "NHF003 isolated account", "NHF003 synthetic local password"
	account := nha162RunBootstrap(t, ctx, data, login, password)
	client := nha181Client(roots)
	client.Timeout = 10 * time.Second
	defer client.CloseIdleConnections()
	base := "https://" + address.String()
	auth := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, auth, login, password)
	source, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(sourceKey, []*registryv1.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic session relay", Constraints: &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := source.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatal("synthetic session discovery:", err)
	}
	_, entity, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	entityID, err := entity.Value()
	if err != nil {
		t.Fatal(err)
	}
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	currentPolicy, err := policies.GetPolicy(ctx, nha141BearerRequest(&authv1.GetPolicyRequest{AccountId: account}, tokens.AccessToken))
	if err != nil || currentPolicy.Msg.GetPresent() == nil {
		t.Fatal("synthetic read policy:", err)
	}
	policy := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects:\n      entities:\n        explicit:\n          - entity_id: %q\n            capabilities: {explicit: [{schema_id: core.switch.v1, endpoint: relay}]}\n", account.Value, entityID)
	if _, err := policies.ReplacePolicy(ctx, nha141BearerRequest(&authv1.ReplacePolicyRequest{AccountId: account, YamlSource: []byte(policy), Precondition: &authv1.ReplacePolicyRequest_Expected{Expected: currentPolicy.Msg.GetPresent().Version}}, tokens.AccessToken)); err != nil {
		t.Fatal("synthetic policy CAS:", err)
	}
	quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
	value, at := domain.NewSwitchValue(false), time.Now().UTC()
	observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Publish(ctx, bindings[0], observation); err != nil {
		t.Fatal(err)
	}
	stateClient := statev1connect.NewStateServiceClient(client, base)
	if page, err := stateClient.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{Filter: &statev1.ViewFilter{}, PageSize: 16}, tokens.AccessToken)); err != nil || len(page.Msg.Entities) != 1 {
		t.Fatal("synthetic protected read readiness:", err)
	}
	var recoveryCode string
	switch mode {
	case nhf003Recovery:
		authenticator := nha142NewAuthenticator(t)
		begun, err := auth.BeginFactorEnrollment(ctx, nha141BearerRequest(&authv1.BeginFactorEnrollmentRequest{Kind: authv1.FactorKind_FACTOR_KIND_FIDO2}, tokens.AccessToken))
		if err != nil || begun.Msg.Challenge == nil {
			t.Fatal("synthetic factor setup:", err)
		}
		activated, err := auth.CompleteFactorEnrollment(ctx, nha141BearerRequest(&authv1.CompleteFactorEnrollmentRequest{ChallengeId: begun.Msg.Challenge.Id, Attestation: authenticator.attestation(t, begun.Msg.Challenge)}, tokens.AccessToken))
		if err != nil || len(activated.Msg.RecoveryCodes) == 0 {
			t.Fatal("synthetic recovery setup:", err)
		}
		recoveryCode = activated.Msg.RecoveryCodes[0]
	case nhf003Enrollment:
		accounts := authv1connect.NewAccountServiceClient(client, base)
		current, err := accounts.GetSecondFactorPolicy(ctx, nha141BearerRequest(&authv1.GetSecondFactorPolicyRequest{}, tokens.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := accounts.SetSecondFactorPolicy(ctx, nha141BearerRequest(&authv1.SetSecondFactorPolicyRequest{Expected: current.Msg.Policy.Version, RequireAdminFactor: true}, tokens.AccessToken)); err != nil {
			t.Fatal(err)
		}
	case nhf003RateLimit:
		limited := false
		for attempt := 0; attempt < 80; attempt++ {
			_, err := auth.StartLogin(ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: login, Password: "synthetic incorrect password"}))
			if connect.CodeOf(err) == connect.CodeResourceExhausted {
				limited = true
				break
			}
			if err == nil {
				t.Fatal("incorrect setup password accepted")
			}
		}
		if !limited {
			t.Fatal("native login limiter not reached within bound")
		}
	case nhf003Password:
	default:
		t.Fatalf("unhandled typed session mode %q", mode)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	proxy = nhf003StartProxy(t, base, roots, pair, out, nhf003LoginExpectation{name: login, password: password})
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "scenario": mode, "origin": proxy.server.URL, "ca_pem": string(certificate), "login_name": login, "password": password, "account_id": account.Value, "recovery_code": recoveryCode, "client_fido2_qualified": false})
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	observedLogin := false
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Error("synthetic control deadline exceeded")
			}
			return
		case <-ticker.C:
			if observation := proxy.loginObservation(); observation != nil && !observedLogin {
				nhf003WritePrivate(t, out, "login-observation.json", observation)
				observedLogin = true
			}
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
		}
	}
}
