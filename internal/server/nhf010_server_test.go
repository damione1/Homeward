package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
)

// Explicit test executable, fresh database and loopback TLS. Real session,
// policy, registry, Host, state-view and dashboard composition; synthetic input
// observations and profiles only. No final device driver is attached at all.
func TestNHF010EnvironmentServer(t *testing.T) {
	out := os.Getenv("HOMEWARD_NHF010_FIXTURE")
	if out == "" {
		t.Skip("selected only by isolated NHF010 qualification")
	}
	root := os.Getenv("HOMEWARD_NHF010_SOURCES")
	if !filepath.IsAbs(out) || !filepath.IsAbs(root) {
		t.Fatal("fresh absolute fixture/source roots required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	devices, err := nhf010ReadDevices(filepath.Join(root, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := registry.LoadManifest(scope.BaselineV1())
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := nhf005DashboardFixture()
	if err != nil || dashboard == nil || dashboard.catalogue == nil {
		t.Fatal("explicit synthetic dashboard catalogue required", err)
	}
	dashboard.compilation = imported.CompilationRecipes
	profiles := new(nhf010Profiles)
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	ctx, signals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer signals()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	instance, _ := domain.ParseIntegrationInstanceID("nhf010-synthetic-only")
	var host *integrations.Manager
	factory := RuntimeFactory(func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		identities, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		for _, device := range devices {
			alias, _ := device.alias.Value()
			if _, err := identities.ConfigureBinding(factoryCtx, instance, device.key, alias); err != nil {
				return RuntimePlan{}, err
			}
		}
		public, err := NewPublicPlan(nil, nil, accountTransportAudit{store: store})
		if err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: identities, Public: public, DisplayProfiles: profiles,
			ConfigureAuthorization: func(_ context.Context, composition AuthorizationComposition) error {
				dashboard.authority = composition.ViewAuthorizer().(*policyViewAuthorizer).authority
				return nil
			},
			ConfigureActivation: func(_ context.Context, owner *configactivation.Owner) error {
				if owner == nil {
					return errors.New("actual configuration owner required")
				}
				dashboard.activation = owner
				return nil
			},
			Configure: func(_ context.Context, actual *integrations.Manager, _ *storage.Store) error {
				host = actual
				return nil
			},
		}, nil
	})
	done, ready := make(chan error, 1), make(chan net.Addr, 1)
	serverCtx, serverCancel := context.WithCancel(ctx)
	go func() {
		done <- RunWithStartupFactory(serverCtx, settings.startup, func(listenCtx context.Context, kind, _ string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(listenCtx, kind, "127.0.0.1:0")
		}, func(address net.Addr) { ready <- address }, factory)
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		serverCancel()
		t.Fatal("isolated startup", err)
	case <-ctx.Done():
		serverCancel()
		t.Fatal(ctx.Err())
	}
	calls := new(nhf006Calls)
	defer func() {
		entries := host.CommandDriverEntries()
		serverCancel()
		joined := false
		select {
		case err := <-done:
			joined = true
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("isolated server did not join")
		}
		calls.mu.Lock()
		defer calls.mu.Unlock()
		nhf003WritePrivate(t, out, "result.json", map[string]any{"server_joined": joined, "command_driver_entries": entries, "fake_driver_calls": 0, "real_device_writes": 0, "entity_count": len(devices), "execute_request_ids": calls.execute, "native_login_calls": calls.logins.Load(), "native_revokes": calls.revokes.Load()})
		if entries != 0 {
			t.Error("read-only environment fixture entered a command driver", entries)
		}
	}()
	source, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]integrations.Candidate, 0, len(devices))
	for _, device := range devices {
		candidate, err := integrations.NewCandidate(device.key, []*registrywire.CapabilityDescription{device.capability()})
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	bindings, err := source.Discover(ctx, candidates)
	if err != nil || len(bindings) != len(devices) {
		t.Fatal("synthetic discovery", err)
	}
	publish := func(sample nhf010Sample) {
		t.Helper()
		at := time.Now().UTC()
		for i, device := range devices {
			observation, err := device.observation(sample, at)
			if err != nil {
				t.Fatal(err)
			}
			if err := source.Publish(ctx, bindings[i], observation); err != nil {
				t.Fatal("synthetic publication", device.kind, err)
			}
		}
	}
	publish(nhf010Nominal)
	resolved := make(map[string]domain.EntityID)
	for i, device := range devices {
		_, id, err := bindings[i].IDs()
		if err != nil {
			t.Fatal(err)
		}
		alias, _ := device.alias.Value()
		resolved[alias] = id
	}
	const adminName, adminPassword = "NHF010 isolated admin", "NHF010 isolated admin password"
	adminID := nha162RunBootstrap(t, ctx, data, adminName, adminPassword)
	if err := dashboard.install(adminID.Value, resolved); err != nil {
		t.Fatal(err)
	}
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address.String()
	authAPI := apiv2connect.NewAuthServiceClient(client, base)
	admin := nha162RunLogin(t, ctx, authAPI, adminName, adminPassword)
	accounts := apiv2connect.NewAccountServiceClient(client, base)
	policies := apiv2connect.NewAuthorizationPolicyServiceClient(client, base)
	type reader struct {
		id                    contracts.AccountID
		publicName            string
		actor                 domain.ActorID
		name, password, token string
	}
	create := func(name string) reader {
		t.Helper()
		password := "NHF010 synthetic local password"
		created, err := accounts.CreateAccount(ctx, nha141BearerRequest(&api.CreateAccountRequest{Parent: nha203SessionAccount(t, admin.Session).Home().String(), Account: &api.Account{LoginName: name, DisplayName: name, Role: api.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}}, admin.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		policy, err := policies.GetAuthorizationPolicy(ctx, nha141BearerRequest(&api.GetAuthorizationPolicyRequest{Name: created.Msg.Name + "/authorizationPolicy"}, admin.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		id := nha203PolicyAccount(t, policy.Msg.YamlSource)
		actor, err := domain.ParseActorID(nha203AccountText(t, id))
		if err != nil {
			t.Fatal(err)
		}
		profiles.set(actor, "America/Toronto")
		tokens := nha162RunLogin(t, ctx, authAPI, name, password)
		return reader{id, created.Msg.Name, actor, name, password, tokens.AccessToken}
	}
	member, guest := create("NHF010 Member"), create("NHF010 Guest")
	dashboard.mu.Lock()
	dashboard.owners = make(map[contracts.AccountID]contracts.AccountID)
	for _, user := range []reader{member, guest} {
		account, err := contracts.ParseAccountID(nha203AccountText(t, user.id))
		if err != nil {
			dashboard.mu.Unlock()
			t.Fatal(err)
		}
		dashboard.owners[account] = account
	}
	dashboard.mu.Unlock()
	dashboard.start(t, ctx, client, base, admin.AccessToken)
	setPolicy := func(user reader, private bool) {
		t.Helper()
		current, err := policies.GetAuthorizationPolicy(ctx, nha141BearerRequest(&api.GetAuthorizationPolicyRequest{Name: user.publicName + "/authorizationPolicy"}, admin.AccessToken))
		if err != nil || current.Msg == nil {
			t.Fatal("read policy", err)
		}
		yaml := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects:\n      entities:\n        explicit:\n", nha203AccountText(t, user.id))
		for _, device := range devices {
			if device.kind == nhf010Details && !private {
				continue
			}
			alias, _ := device.alias.Value()
			id, _ := resolved[alias].Value()
			yaml += fmt.Sprintf("          - entity_id: %q\n            capabilities: {all: {}}\n", id)
		}
		yaml += "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {all: {}}}\n"
		_, err = policies.ReplaceAuthorizationPolicy(ctx, nha141BearerRequest(&api.ReplaceAuthorizationPolicyRequest{Name: user.publicName + "/authorizationPolicy", YamlSource: []byte(yaml), Precondition: &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: current.Msg.Etag}}, admin.AccessToken))
		if err != nil {
			t.Fatal("replace isolated policy", err)
		}
	}
	setPolicy(member, true)
	setPolicy(guest, false)
	// Actual authenticated TLS preflight includes pagination for both rights
	// profiles. This is a Go/network proof, not a Rust or native UI proof.
	homeAPI := apiv2connect.NewHomeServiceClient(client, base)
	entityViews := apiv2connect.NewEntityViewServiceClient(client, base)
	for i, user := range []reader{member, guest} {
		homeResp, err := homeAPI.ListHomes(ctx, nha141BearerRequest(&api.ListHomesRequest{PageSize: 1}, user.token))
		if err != nil || len(homeResp.Msg.GetHomes()) != 1 {
			t.Fatal(err)
		}
		createdView, err := entityViews.CreateEntityView(ctx, nha141BearerRequest(&api.CreateEntityViewRequest{
			Parent:     homeResp.Msg.GetHomes()[0].Name,
			EntityView: &api.EntityView{Filter: ""},
		}, user.token))
		if err != nil {
			t.Fatal(err)
		}
		first, err := entityViews.QueryEntityView(ctx, nha141BearerRequest(&api.QueryEntityViewRequest{
			Name:     createdView.Msg.GetName(),
			PageSize: 1,
		}, user.token))
		if err != nil {
			t.Fatal(err)
		}
		page := first.Msg
		count := 0
		for {
			// Native profile remains authoritative; Dashboard v2 owns its public projection.
			if zone, err := profiles.DisplayTimeZone(ctx, user.actor); err != nil || zone != "America/Toronto" {
				t.Fatal("missing authorized profile")
			}
			for _, entry := range page.Entries {
				observed := false
				for _, capability := range entry.Capabilities {
					if i == 1 && capability.GetPresenceDetails() != nil {
						t.Fatal("guest received private coordinates")
					}
					observed = observed || capability.GetWeather().GetObservation().GetValue() != nil || capability.GetPresence().GetObservation().GetValue() != nil || capability.GetPresenceDetails().GetObservation().GetValue() != nil || capability.GetSolarEvent().GetObservation().GetValue() != nil
				}
				if !observed {
					t.Fatal("missing typed synthetic observation")
				}
				count++
			}
			if page.NextPageToken == "" {
				break
			}
			next, err := entityViews.QueryEntityView(ctx, nha141BearerRequest(&api.QueryEntityViewRequest{
				Name:      createdView.Msg.GetName(),
				PageSize:  1,
				PageToken: page.NextPageToken,
			}, user.token))
			if err != nil {
				t.Fatal(err)
			}
			page = next.Msg
		}
		if _, err := entityViews.DeleteEntityView(ctx, nha141BearerRequest(&api.DeleteEntityViewRequest{Name: createdView.Msg.GetName()}, user.token)); err != nil {
			t.Fatal(err)
		}
		want := 4
		if i == 1 {
			want = 3
		}
		if count != want {
			t.Fatal("incorrect authorized entity count", count, want)
		}
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(data, nha181CertificateName), filepath.Join(data, nha181PrivateKeyName))
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	proxy := calls.proxy(t, base, transport, pair, nhf006Ack, true)
	defer func() { proxy.CloseClientConnections(); proxy.Close() }()
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	nhf003WritePrivate(t, out, "ready.json", map[string]any{"mode": "synthetic-only", "origin": proxy.URL, "ca_pem": string(certificate), "entity_count": len(devices), "member": map[string]string{"login_name": member.name, "password": member.password}, "guest": map[string]string{"login_name": guest.name, "password": guest.password}})
	if smoke := os.Getenv("HOMEWARD_NHF010_SMOKE"); smoke != "" {
		if smoke != "1" {
			t.Fatal("unknown smoke selection")
		}
		return
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			command, err := nhf010ReadControl(out, sequence)
			if err != nil {
				t.Fatal(err)
			}
			if command == nil {
				continue
			}
			switch command.action {
			case nhf010ControlNominal:
				publish(nhf010Nominal)
			case nhf010ControlStale:
				publish(nhf010Stale)
			case nhf010ControlUnknown:
				publish(nhf010Unknown)
			case nhf010ControlAbsent:
				publish(nhf010Absent)
			case nhf010ControlMissing:
				publish(nhf010Missing)
			case nhf010ControlRevoke:
				setPolicy(member, false)
			case nhf010ControlAllow:
				setPolicy(member, true)
			case nhf010ControlParis:
				profiles.set(member.actor, "Europe/Paris")
			case nhf010ControlToronto:
				profiles.set(member.actor, "America/Toronto")
			case nhf010ControlOutage:
				calls.network.cut()
			case nhf010ControlNetwork:
				calls.network.outage.Store(false)
			default:
				t.Fatal("unparsed environment control")
			}
			sequence = command.sequence
			nhf003WritePrivate(t, out, fmt.Sprintf("ack-%02d.json", sequence), map[string]any{"sequence": sequence, "action": command.action})
		}
	}
}
