package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	applywire "github.com/damione1/Homeward/gen/home/api/v2"
	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	dashboardwire "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	taskwire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationapplyv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationplanv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	configurationv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	dashboardv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/tasknames"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type dashboardWidgetsDriver struct {
	mu       sync.Mutex
	requests []json.RawMessage
	source   *integrations.Session
	devices  map[string]*dashboardWidgetsSimulatedDevice
}

func (d *dashboardWidgetsDriver) Dispatch(ctx context.Context, command integrations.AdmittedCommand) error {
	if !command.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	request, err := command.Request()
	if err != nil {
		return err
	}
	raw, err := protojson.Marshal(request)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, raw)
	device := d.devices[request.GetTarget().GetEntityId()]
	if d.source == nil || device == nil || device.value == nil || device.definition.Endpoint != request.Endpoint {
		return integrations.ErrInvalidInput
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	next, err := dashboardWidgetsApply(*device.value, request.Arguments)
	if err != nil {
		return err
	}
	if err = command.Acknowledge(); err != nil {
		return err
	}
	cause, err := command.Causality()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	observation, err := integrations.NewStateObservation(device.definition.Endpoint, device.quality, &next, &now, now, nil)
	if err != nil {
		return err
	}
	observation, err = observation.WithCausality(cause)
	if err != nil {
		return err
	}
	if err = d.source.Publish(ctx, device.binding, observation); err != nil {
		return err
	}
	id, entity, err := command.RouteIdentity()
	if err != nil {
		return err
	}
	device.retain(request, id, entity, next, time.Now().UTC())
	if request.Arguments.GetSetClimate() != nil || request.Arguments.GetCover() != nil || request.Arguments.GetLock() != nil || request.Arguments.GetAlarm() != nil {
		// Read the retained simulator model only after its causal observation was
		// accepted. An ACK or the proposed next value alone supplies no proof.
		retained := d.devices[request.GetTarget().GetEntityId()]
		proof := dashboardWidgetsProtocolReadback{
			command: id, entity: entity, request: proto.Clone(request).(*actions.CommandRequest),
			value: *retained.value, quality: retained.quality, publishedAt: now, readAt: time.Now().UTC(),
			stopReceipt: retained.coverStopReceipt,
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if !command.ReadyForTransport() {
			return integrations.ErrCommandAuthorizationDenied
		}
		return command.ConfirmProtocol(proof)
	}
	return nil
}
func (d *dashboardWidgetsDriver) snapshot() []json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]json.RawMessage(nil), d.requests...)
}

// Explicit qualification only: production registration, SQL generation,
// authentication, state and command services remain unchanged. The final
// synthetic device driver is the only replacement. No provider URL is loaded.
func TestDashboardWidgetsConfigurationServer(t *testing.T) {
	out, root := os.Getenv("HOMEWARD_DASHBOARD_WIDGETS_FIXTURE"), os.Getenv("HOMEWARD_DASHBOARD_WIDGETS_SOURCES")
	if out == "" {
		t.Skip("explicit isolated native qualification")
	}
	if os.Getenv("HOMEWARD_DASHBOARD_WIDGETS_SYNTHETIC_ONLY") != "1" || !filepath.IsAbs(out) || !filepath.IsAbs(root) {
		t.Fatal("absolute fresh synthetic fixture paths required")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	devices, err := dashboardWidgetsDevices(filepath.Join(root, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The opt-in test fixture alone may live longer for a physical native run.
	// Production server startup and transport deadlines are unchanged.
	lifetimeMinutes := 20
	if raw := os.Getenv("HOMEWARD_DASHBOARD_WIDGETS_LIFETIME_MINUTES"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 720 {
			t.Fatal("synthetic fixture lifetime must be 1..720 minutes")
		}
		lifetimeMinutes = value
	}
	reviewDeadline := time.Now().Add(time.Duration(lifetimeMinutes) * time.Minute)
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := dashboardWidgetsReviewCertificate(t, data, reviewDeadline)
	settings := nha181Config(t, cluster.Port, data, "127.0.0.1:0", nha181TLSYAML()+"scope_revision: ecobee_mvp_v1\n")
	workspace, err := settings.startup.CaptureWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	operational := filepath.Join(t.TempDir(), "configuration.yaml")
	if err = os.WriteFile(operational, workspace.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	startup, err := config.LoadStartupPaths(operational, config.DefaultBootstrapChoice(), nha029TestEnv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), reviewDeadline)
	defer cancel()
	instance, _ := domain.ParseIntegrationInstanceID("dashboard-widgets-final-fake")
	var actualRuntime *observationRuntime
	var actualLists *listComposition
	serverCtx, stopServer := context.WithCancel(ctx)
	done, ready := make(chan error, 1), make(chan net.Addr, 1)
	go func() {
		done <- runWithDependencies(serverCtx, startup, func(listenCtx context.Context, kind, _ string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(listenCtx, kind, "127.0.0.1:0")
		}, func(address net.Addr) { ready <- address }, nil, runDependencies{observeLists: func(composition *listComposition, _ *sessionCoordinator) { actualLists = composition }, observeRuntime: func(runtime *observationRuntime) { actualRuntime = runtime }})
	}()
	var address net.Addr
	select {
	case address = <-ready:
	case err := <-done:
		stopServer()
		t.Fatal("startup", err)
	case <-ctx.Done():
		stopServer()
		t.Fatal(ctx.Err())
	}
	if actualRuntime == nil || actualRuntime.host == nil {
		stopServer()
		t.Fatal("default production runtime required")
	}
	host := actualRuntime.host
	identities, present := actualRuntime.identity.(*registry.Repository)
	if !present || identities == nil {
		stopServer()
		t.Fatal("default runtime registry required")
	}
	var admitted []dashboardWidgetsDevice
	var unresolved []string
	for _, device := range devices {
		if _, err = identities.ConfigureBinding(ctx, instance, device.key, device.Alias); err != nil {
			if errors.Is(err, registry.ErrOutsideManifest) {
				unresolved = append(unresolved, device.Alias)
				continue
			}
			stopServer()
			t.Fatal(err)
		}
		admitted = append(admitted, device)
	}
	devices = admitted
	driver := new(dashboardWidgetsDriver)
	defer func() {
		stopServer()
		joined := false
		select {
		case err := <-done:
			joined = true
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			t.Error("server failed to join")
		}
		dashboardWidgetsWritePrivate(t, out, "result.json", map[string]any{"production_dashboard_service": true, "production_config_apply": true, "real_device_writes": 0, "device_requests": driver.snapshot(), "server_joined": joined})
	}()
	source, err := host.OpenSession(instance)
	if err != nil {
		t.Fatal(err)
	}
	var candidates []integrations.Candidate
	for _, device := range devices {
		candidate, err := integrations.NewCandidate(device.key, []*registrywire.CapabilityDescription{device.capability()})
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	bindings, err := source.Discover(ctx, candidates)
	if err != nil || len(bindings) != len(devices) {
		t.Fatal("discovery", err)
	}
	if err = driver.configure(source, devices, bindings); err != nil {
		t.Fatal(err)
	}
	if err = host.AttachCommandDriver(source, driver); err != nil {
		t.Fatal(err)
	}
	publish := func(condition *bool, stale bool) {
		t.Helper()
		if err = driver.publish(ctx, condition, stale); err != nil {
			t.Fatal(err)
		}
	}
	publish(nil, false)
	resolved := map[string]string{}
	for i, device := range devices {
		_, entity, err := bindings[i].IDs()
		if err != nil {
			t.Fatal(err)
		}
		resolved[device.Alias], err = entity.Value()
		if err != nil {
			t.Fatal(err)
		}
	}
	const login, password = "Dashboard configuration qualification", "Synthetic dashboard qualification password"
	account := nha162RunBootstrap(t, ctx, data, login, password)
	client := nha181Client(roots)
	defer client.CloseIdleConnections()
	base := "https://" + address.String()
	authAPI := apiv2connect.NewAuthServiceClient(client, base)
	tokens := nha162RunLogin(t, ctx, authAPI, login, password)
	// Control RPCs use the real rotating credential contract too. A long
	// physical review must not retain the initial short-lived access token.
	renewControlSession := func() {
		t.Helper()
		if tokens.AccessExpireTime != nil && tokens.AccessExpireTime.AsTime().After(time.Now().Add(time.Minute)) {
			return
		}
		response, e := authAPI.RefreshSession(ctx, connect.NewRequest(&applywire.RefreshSessionRequest{RefreshToken: tokens.RefreshToken, RenewalKey: []byte("0123456789abcdef0123456789abcdef")}))
		if e != nil || response == nil || response.Msg.Tokens == nil || response.Msg.Tokens.GetSession().GetName() != tokens.GetSession().GetName() {
			t.Fatal("owned control session refresh failed", e)
		}
		tokens = response.Msg.Tokens
	}
	policies := apiv2connect.NewAuthorizationPolicyServiceClient(client, base)
	nha162RunPolicy(t, ctx, policies, nha203SessionAccount(t, tokens.Session).String(), tokens.AccessToken, "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n  - scope: {family: actions, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n  - scope: {family: actions, operation: write}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n"+nha162All)
	documents := configurationv1connect.NewConfigurationServiceClient(client, base)
	plans := configurationplanv1connect.NewConfigurationPlanServiceClient(client, base)
	apply := configurationapplyv1connect.NewConfigurationApplyServiceClient(client, base)
	dashboards := dashboardv1connect.NewDashboardServiceClient(client, base)
	accountID, err := contracts.ParseAccountID(account.Value)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := nha203NativeSession(t, ctx, actualRuntime.store, tokens.Session.Name)
	principal, err := authorization.NativePrincipal(accountID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	importCtx, err := authorization.WithPrincipal(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	listIDs, err := dashboardWidgetsImportLists(importCtx, actualLists, filepath.Join(root, "lists.json"))
	if err != nil {
		t.Fatal("real list source import", err)
	}
	listAPI := apiv2connect.NewTaskServiceClient(client, base)
	home, err := actualLists.registry.HomeName(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshotLists := func(name string) {
		t.Helper()
		result := make(map[string]json.RawMessage)
		for slot, id := range listIDs {
			native, err := domain.ParseListID(id)
			if err != nil {
				t.Fatal(err)
			}
			name, err := tasknames.List(home, native)
			if err != nil {
				t.Fatal(err)
			}
			page, err := listAPI.ListTasks(ctx, nha056SessionRequest(&taskwire.ListTasksRequest{Parent: name.String(), PageSize: 16}, tokens.AccessToken))
			if err != nil {
				t.Fatal("public ListItems", err)
			}
			result[slot], err = protojson.Marshal(page.Msg)
			if err != nil {
				t.Fatal(err)
			}
		}
		dashboardWidgetsWritePrivate(t, out, name, result)
	}
	snapshotLists("lists-initial.json")
	var previous *configuration.DocumentVersion
	var previousRaw []byte
	publication := 0
	publishConfig := func(name string) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(raw, []byte("OWNER_ACCOUNT_ID")) != 1 {
			t.Fatal("one explicit owner placeholder required")
		}
		raw = bytes.ReplaceAll(raw, []byte("OWNER_ACCOUNT_ID"), []byte(account.Value))
		for slot, id := range listIDs {
			raw = bytes.ReplaceAll(raw, []byte("LIST_"+slot+"_ID"), []byte(id))
		}
		for alias, id := range resolved {
			key := "CALENDAR_" + strings.ToUpper(strings.TrimPrefix(alias, "calendar.")) + "_ID"
			if strings.HasPrefix(alias, "calendar.") {
				raw = bytes.ReplaceAll(raw, []byte(key), []byte(id))
			}
		}
		candidate := &planwire.ConfigurationPlanCandidate{Family: planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.configuration-qualification", Entry: "dashboard.yaml", Files: []*planwire.ConfigurationPlanSourceFile{{LogicalPath: "dashboard.yaml", ExactSource: raw}}}
		before, err := documents.GetConfigurationDocument(ctx, nha056SessionRequest(&configuration.GetConfigurationDocumentRequest{Name: nha203PublicDocumentName(t, ctx, client, base, tokens.AccessToken, "operational.main")}, tokens.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := plans.CreateConfigurationPlan(ctx, nha056SessionRequest(&planwire.CreateConfigurationPlanRequest{Parent: strings.Split(before.Msg.Name, "/configurationDocuments/")[0], ConfigurationPlan: &planwire.ConfigurationPlan{Candidate: candidate}}, tokens.AccessToken))
		if err != nil {
			t.Fatal("public plan", err)
		}
		request := nha055Request(before.Msg, candidate, plan.Msg)
		request.Sources = []*applywire.SourcePrecondition{nha055PublicationExpected("dashboard.configuration-qualification", "dashboard.yaml", previous, previousRaw)}
		response, err := apply.ApplyConfigurationPlan(ctx, nha056SessionRequest(request, tokens.AccessToken))
		if err != nil {
			t.Fatal("public Apply", err)
		}
		if !response.Msg.Done {
			t.Fatal("dashboard not published")
		}
		read, err := documents.GetConfigurationDocument(ctx, nha056SessionRequest(&configuration.GetConfigurationDocumentRequest{Name: nha203PublicDocumentName(t, ctx, client, base, tokens.AccessToken, "dashboard.configuration-qualification")}, tokens.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		previous, previousRaw = read.Msg.Version, raw
		publication++
		sum := sha256.Sum256(raw)
		dashboardWidgetsWritePrivate(t, out, "published.json", map[string]any{"name": name, "publication": publication, "generation": nha203PublishedMetadata(t, response.Msg).GenerationRevision, "source_revision": previous.Revision, "source_sha256": hex.EncodeToString(sum[:])})
		if err = os.WriteFile(filepath.Join(out, "published-"+name+".yaml"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		listed, err := dashboards.ListDashboards(ctx, nha056SessionRequest(&dashboardwire.ListDashboardsRequest{Parent: nha203DocumentParent(before.Msg.Name)}, tokens.AccessToken))
		if err != nil || len(listed.Msg.Dashboards) != 1 || listed.Msg.NextPageToken != "" {
			t.Fatal("production DashboardService List", err)
		}
		layout, err := dashboards.GetDashboard(ctx, nha056SessionRequest(&dashboardwire.GetDashboardRequest{Name: listed.Msg.Dashboards[0].Name}, tokens.AccessToken))
		if err != nil || layout.Msg.Layout == nil || layout.Msg.Etag == "" {
			t.Fatal("production DashboardService Get", err)
		}
		encoded, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(layout.Msg)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(out, "wire-"+name+".json"), encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	publishConfig("a")
	certificate, err := os.ReadFile(filepath.Join(data, nha181CertificateName))
	if err != nil {
		t.Fatal(err)
	}
	expiresAt, _ := ctx.Deadline()
	dashboardWidgetsWritePrivate(t, out, "ready.json", map[string]any{"expires_at_unix": expiresAt.Unix(), "certificate_expires_at_unix": reviewDeadline.Add(5 * time.Minute).Unix(), "mode": "synthetic-only", "origin": base, "ca_pem": string(certificate), "login_name": login, "password": password, "entity_bindings": resolved, "unresolved_aliases": unresolved, "list_bindings": listIDs, "production_dashboard_service": true, "production_config_apply": true, "no_physical_providers": true})
	var sequence uint64
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-heartbeat.C:
			if err = driver.heartbeat(ctx); err != nil {
				t.Fatal(err)
			}
		case <-ticker.C:
			if _, err = os.Stat(filepath.Join(out, "stop")); err == nil {
				return
			}
			raw, readErr := os.ReadFile(filepath.Join(out, "command.json"))
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			if readErr != nil || len(raw) > 4096 {
				t.Fatal("control unavailable", readErr)
			}
			var command struct {
				Sequence uint64 `json:"sequence"`
				Action   string `json:"action"`
			}
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.DisallowUnknownFields()
			if err = decoder.Decode(&command); err != nil {
				t.Fatal(err)
			}
			if decoder.Decode(new(any)) != io.EOF {
				t.Fatal("trailing control data")
			}
			if command.Sequence <= sequence {
				continue
			}
			if command.Sequence != sequence+1 {
				t.Fatal("control sequence gap")
			}
			renewControlSession()
			switch command.Action {
			case "apply-a":
				publishConfig("a")
			case "apply-b":
				publishConfig("b")
			case "apply-showcase":
				publishConfig("showcase")
			case "condition-on":
				publish(nhf008Pointer(true), false)
			case "condition-off":
				publish(nhf008Pointer(false), false)
			case "condition-stale":
				publish(nhf008Pointer(true), true)
			case "snapshot":
			default:
				t.Fatal("unknown control action")
			}
			sequence = command.Sequence
			snapshotLists("lists-current.json")
			dashboardWidgetsWritePrivate(t, out, "control-result.json", map[string]any{"sequence": sequence, "action": command.Action, "device_requests": driver.snapshot()})
		}
	}
}
