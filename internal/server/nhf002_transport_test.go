package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// TestNHF002TransportServer is an explicitly selected helper, never a serving
// mode in homed. It runs real SQL, auth/policy and StateService routes; only the
// source of observations is synthetic. Its private control files are test
// barriers, not product RPCs. No command driver or physical pilot is attached.
func TestNHF002TransportServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF002_FIXTURE")
	if out == "" {
		t.Skip("selected only by the native transport qualification harness")
	}
	if !filepath.IsAbs(out) {
		t.Fatal("fixture output must be absolute")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("requires a fresh private fixture directory:", err)
	}
	write := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		temporary := filepath.Join(out, name+".writing")
		file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(temporary, filepath.Join(out, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(temporary); err != nil {
			t.Fatal(err)
		}
	}
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixtureWith(t, data, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), func(cert *x509.Certificate) {
		if os.Getenv("HOMEWARD_NHF002_WRONG_NAME") == "1" {
			cert.DNSNames, cert.IPAddresses = []string{"wrong.invalid"}, nil
		}
	})
	settings := nha049Config(t, cluster.Port, data)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	instance, _ := domain.ParseIntegrationInstanceID("nhf002-synthetic-only")
	key, _ := integrations.NewSourceKey("switch", "relay")
	var host *integrations.Manager
	var owner *configactivation.Owner
	var applied uint64
	var network *nhf002Network
	futureJoined := false
	var futureStats *nhf002FutureWire
	factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.LoadManifest(scope.BaselineV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		// Reuse a fixture identity admitted by BaselineV1, as NHA-147 does.
		// This private SQL registry has no physical provider or command driver.
		if _, err := repository.ConfigureBinding(factoryCtx, instance, key, "switch.basement_relay"); err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: repository,
			Configure: func(_ context.Context, actual *integrations.Manager, _ *storage.Store) error {
				host = actual
				return nil
			},
			ConfigureActivation: func(_ context.Context, actual *configactivation.Owner) error {
				owner = actual
				return nil
			},
		}, nil
	})
	ready, done := make(chan net.Addr, 1), make(chan error, 1)
	go func() {
		defer close(done)
		done <- RunWithStartupFactory(ctx, settings.startup, func(ctx context.Context, kind, address string) (net.Listener, error) {
			listener, err := (&net.ListenConfig{}).Listen(ctx, kind, address)
			if err != nil {
				return nil, err
			}
			network = &nhf002Network{Listener: listener, connections: make(map[*nhf002Connection]struct{})}
			return network, nil
		}, func(address net.Addr) { ready <- address }, factory)
		cancel()
	}()
	defer func() {
		cancel()
		joined := false
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Error("fixture server shutdown:", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("fixture server did not join its readers")
		}
		if host != nil && network != nil {
			network.mu.Lock()
			remaining := len(network.connections)
			network.mu.Unlock()
			var authorityCalls uint64
			if futureStats != nil {
				authorityCalls = futureStats.authorityCalls.Load()
			}
			write("result.json", map[string]any{"published": applied, "command_driver_entries": host.CommandDriverEntries(), "server_joined": joined, "future_server_joined": futureJoined, "connections_remaining": remaining, "outage_rejected": network.rejected.Load(), "authority_calls": authorityCalls})
			if remaining != 0 {
				t.Error("fixture retained TCP connections")
			}
			if host.CommandDriverEntries() != 0 {
				t.Error("fixture entered a command driver")
			}
		}
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		t.Fatal("fixture exited before ready:", err)
	case <-ctx.Done():
		t.Fatal("fixture startup:", ctx.Err())
	}
	if host == nil || owner == nil {
		t.Fatal("real Host and activation owner not composed")
	}
	session, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := integrations.NewCandidate(key, []*registryv1.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "Synthetic transport relay", Constraints: &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := session.Discover(ctx, []integrations.Candidate{candidate})
	if err != nil || len(bindings) != 1 {
		t.Fatal("synthetic discovery:", err)
	}
	_, entity, err := bindings[0].IDs()
	if err != nil {
		t.Fatal(err)
	}
	entityID, err := entity.Value()
	if err != nil {
		t.Fatal(err)
	}
	const login, password = "NHF002 isolated account", "NHF002 synthetic local password"
	account := nha162RunBootstrap(t, ctx, data, login, password)
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	if os.Getenv("HOMEWARD_NHF002_WRONG_NAME") == "1" {
		client.Transport.(*http.Transport).TLSClientConfig.ServerName = "wrong.invalid"
	}
	base := "https://" + address.String()
	auth := authv1connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, auth, login, password)
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
	current, err := policies.GetPolicy(ctx, nha141BearerRequest(&authv1.GetPolicyRequest{AccountId: account}, tokens.AccessToken))
	if err != nil || current.Msg.GetPresent() == nil {
		t.Fatal("fixture policy:", err)
	}
	policy := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects:\n      entities:\n        explicit:\n          - entity_id: %q\n            capabilities: {explicit: [{schema_id: core.switch.v1, endpoint: relay}]}\n", account.Value, entityID)
	if _, err := policies.ReplacePolicy(ctx, nha141BearerRequest(&authv1.ReplacePolicyRequest{AccountId: account, YamlSource: []byte(policy), Precondition: &authv1.ReplacePolicyRequest_Expected{Expected: current.Msg.GetPresent().Version}}, tokens.AccessToken)); err != nil {
		t.Fatal("fixture policy CAS:", err)
	}
	publish := func(sequence uint64) {
		t.Helper()
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		value, at := domain.NewSwitchValue(sequence%2 != 0), time.Now().UTC()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Publish(ctx, bindings[0], observation); err != nil {
			t.Fatal("synthetic publication:", err)
		}
	}
	publish(0)
	api := statev1connect.NewStateServiceClient(client, base)
	check, err := api.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{Filter: &statev1.ViewFilter{}, PageSize: 16}, tokens.AccessToken))
	if err != nil || len(check.Msg.Entities) != 1 || check.Msg.Entities[0].State.GetSwitchOn() {
		t.Fatal("fixture StateService readiness:", err)
	}
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	untrustedData := shortPrivateDataDir(t)
	nha181CertificateFixtureWith(t, untrustedData, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), func(cert *x509.Certificate) {
		cert.Subject.CommonName = "Unrelated NHF002 trust anchor"
	})
	untrusted, err := os.ReadFile(filepath.Join(untrustedData, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	future, futureCertificate, stats := nhf002StartFutureWire(t)
	futureStats = stats
	defer func() { future.Close(); futureJoined = true }()
	write("ready.json", map[string]any{"mode": "synthetic-only", "origin": base, "ca_pem": string(certificate), "untrusted_ca_pem": string(untrusted), "access_token": tokens.AccessToken, "entity_id": entityID, "wire_origin": future.URL, "wire_ca_pem": futureCertificate})
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	revoked := false
	cut, restored := false, false
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				t.Error("fixture control deadline exceeded")
			}
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			if _, err := os.Stat(filepath.Join(out, "outage")); err == nil && !cut {
				network.cut()
				cut = true
			}
			if _, err := os.Stat(filepath.Join(out, "restore")); err == nil && cut && !restored {
				network.outage.Store(false)
				restored = true
			}
			if _, err := os.Stat(filepath.Join(out, "revoke")); err == nil && !revoked {
				if _, err := auth.RevokeSession(ctx, nha141BearerRequest(&authv1.RevokeSessionRequest{SessionId: tokens.Session.Id}, tokens.AccessToken)); err != nil {
					t.Fatal("synthetic session revocation:", err)
				}
				revoked = true
			}
			control, err := os.ReadFile(filepath.Join(out, "advance"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var sequence uint64
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(control))), &sequence); err != nil || sequence > 1003 {
				t.Fatal("invalid fixture barrier")
			}
			if sequence == applied+1 {
				publish(sequence)
				applied = sequence
			}
		}
	}
}
