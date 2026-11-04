package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	"github.com/damione1/Homeward/gen/google/longrunning/longrunningpbconnect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/operator/v1/operatorv1connect"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"golang.org/x/net/http2"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type nha147EntityCapture struct {
	Name, Resume string
	Page         *api.QueryEntityViewResponse
	Entries      []*api.EntityViewEntry
}

type nha147Driver struct {
	emitted atomic.Int32
	mu      sync.Mutex
	last    integrations.AdmittedCommand
}

func (d *nha147Driver) Dispatch(_ context.Context, command integrations.AdmittedCommand) error {
	if !command.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	d.mu.Lock()
	d.last = command
	d.mu.Unlock()
	d.emitted.Add(1)
	return nil
}
func (d *nha147Driver) captured() integrations.AdmittedCommand {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// The fixture uses the actual Run, SQL registry, Owner029, session provider,
// ledger and generated Connect routes. Only the final device driver is fake.
func TestNHA147RunNoJobsSnapshotDispatchRestartAndRevocation(t *testing.T) {
	cluster := testpg.Start(t)
	data := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, data)
	settings := nha049Config(t, cluster.Port, data)
	client := nha181Client(roots)
	// The shared context bounds the entire stream proof; no shorter client
	// timeout may stop its fast witness before the required wire volume.
	client.Timeout = 0
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	instance, _ := domain.ParseIntegrationInstanceID("nha147-fake-only")
	keys := make([]integrations.SourceKey, 2)
	keys[0], _ = integrations.NewSourceKey("first", "relay")
	keys[1], _ = integrations.NewSourceKey("second", "relay")
	aliases := []string{"switch.basement_relay", "switch.living_room_relay"}
	driver := new(nha147Driver)
	var host *integrations.Manager
	var sequence *sequencer.Sequencer
	var owner *configactivation.Owner
	var session *integrations.Session
	var bindings []integrations.Binding
	var base string
	var stop func()
	var nativeStore *storage.Store
	factory := func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		nativeStore = store
		manifest, err := registry.LoadManifest(scope.BaselineV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		for i, key := range keys {
			if _, err := repository.ConfigureBinding(factoryCtx, instance, key, aliases[i]); err != nil {
				return RuntimePlan{}, err
			}
		}
		return RuntimePlan{Identity: repository,
			Configure: func(_ context.Context, actual *integrations.Manager, _ *storage.Store) error {
				host = actual
				return nil
			},
			ConfigureAuthorization: func(_ context.Context, actual AuthorizationComposition) error {
				sequence = actual.(runtimeAuthorizationComposition).views.source
				return nil
			},
			ConfigureActivation: func(_ context.Context, actual *configactivation.Owner) error { owner = actual; return nil },
		}, nil
	}
	start := func(useFactory bool) {
		t.Helper()
		runCtx, runCancel := context.WithCancel(ctx)
		ready, done := make(chan net.Addr, 1), make(chan error, 1)
		var listener net.ListenConfig
		go func() {
			if useFactory {
				done <- RunWithStartupFactory(runCtx, settings.startup, listener.Listen, func(address net.Addr) { ready <- address }, factory)
			} else {
				done <- RunWithStartup(runCtx, settings.startup, listener.Listen, func(address net.Addr) { ready <- address })
			}
		}()
		select {
		case address := <-ready:
			base = "https://" + address.String()
		case err := <-done:
			t.Fatal("Run before ready", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		var once sync.Once
		stop = func() {
			once.Do(func() {
				runCancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error("Run drain", err)
					}
				case <-time.After(10 * time.Second):
					t.Error("Run retained a public reader or command owner")
				}
			})
		}
		t.Cleanup(stop)
		if !useFactory {
			return
		}
		if owner == nil || host == nil || sequence == nil {
			t.Fatal("real authorities were not composed")
		}
		var err error
		session, err = host.OpenSession(instance)
		if err != nil {
			t.Fatal(err)
		}
		candidates := make([]integrations.Candidate, 0, len(keys))
		for _, key := range keys {
			candidate, err := integrations.NewCandidate(key, []*registryv1.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "NHA147 simulated switch", Constraints: &registryv1.CapabilityDescription_Switch{Switch: &registryv1.SwitchConstraints{SetOn: true}}}})
			if err != nil {
				t.Fatal(err)
			}
			candidates = append(candidates, candidate)
		}
		bindings, err = session.Discover(ctx, candidates)
		if err != nil {
			t.Fatal(err)
		}
		if err := host.AttachCommandDriver(session, driver); err != nil {
			t.Fatal(err)
		}
	}
	start(true)
	operatorHTTP := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", filepath.Join(data, operatorSocketName))
	}}}
	defer operatorHTTP.CloseIdleConnections()
	operator := operatorv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	const password = "NHA147 synthetic native local password"
	_, err := operator.BootstrapAdmin(ctx, connect.NewRequest(&authv1.BootstrapAdminRequest{LoginName: "NHA147 Admin", DisplayName: "NHA147 Admin", Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	loginAccount := func(name string) *api.SessionTokens {
		t.Helper()
		response, err := apiv2connect.NewAuthServiceClient(client, base).StartLogin(ctx, connect.NewRequest(&api.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil || response.Msg.Decision.GetAuthenticated() == nil {
			t.Fatal("native login", err)
		}
		return response.Msg.Decision.GetAuthenticated()
	}
	login := func() *api.SessionTokens { return loginAccount("NHA147 Admin") }
	tokens := login()
	views := apiv2connect.NewEntityViewServiceClient(client, base)
	homes := apiv2connect.NewHomeServiceClient(client, base)
	capabilities := apiv2connect.NewCapabilityServiceClient(client, base)
	operations := longrunningpbconnect.NewOperationsClient(client, base)
	listedHomes, err := homes.ListHomes(ctx, nha141BearerRequest(&api.ListHomesRequest{}, tokens.AccessToken))
	if err != nil || len(listedHomes.Msg.GetHomes()) != 1 {
		t.Fatal("native installation discovery", err)
	}
	home, err := apitypes.ParseHomeName(listedHomes.Msg.Homes[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	entityName := func(index int) string {
		_, entity, err := bindings[index].IDs()
		if err != nil {
			t.Fatal(err)
		}
		name, err := apitypes.EntityNameForIdentity(home, entity)
		if err != nil {
			t.Fatal(err)
		}
		return name.String()
	}
	// Explicit fixture grants go through the real native policy CAS API.
	// Admin is an account-management role, never implicit equipment access.
	grantPolicy := func(account string, stateRead, actionRead, actionWrite bool) {
		t.Helper()
		policy := apiv2connect.NewAuthorizationPolicyServiceClient(client, base)
		current, err := policy.GetAuthorizationPolicy(ctx, nha141BearerRequest(&api.GetAuthorizationPolicyRequest{Name: account + "/authorizationPolicy"}, tokens.AccessToken))
		if err != nil || current == nil {
			t.Fatal("native policy read", err)
		}
		var source strings.Builder
		fmt.Fprintf(&source, "schema: homeward.authorization/1\naccount_id: %q\n", nha203AccountText(t, nha203PolicyAccount(t, current.Msg.YamlSource)))
		if !stateRead && !actionRead && !actionWrite {
			source.WriteString("grants: []\n")
		} else {
			source.WriteString("grants:\n")
		}
		for _, grant := range []struct {
			family, operation string
			enabled           bool
		}{
			{"state_registry", "read", stateRead}, {"actions", "read", actionRead}, {"actions", "write", actionWrite},
		} {
			if !grant.enabled {
				continue
			}
			fmt.Fprintf(&source, "  - scope: {family: %s, operation: %s}\n    objects:\n      entities:\n        explicit:\n", grant.family, grant.operation)
			for _, binding := range bindings {
				_, entity, err := binding.IDs()
				if err != nil {
					t.Fatal(err)
				}
				id, err := entity.Value()
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&source, "          - entity_id: %q\n            capabilities: {explicit: [{schema_id: core.switch.v1, endpoint: relay}]}\n", id)
			}
		}
		request := &api.ReplaceAuthorizationPolicyRequest{Name: current.Msg.Name, YamlSource: []byte(source.String()), Precondition: &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: current.Msg.Etag}}
		if current.Msg.Origin == api.AuthorizationPolicyOrigin_AUTHORIZATION_POLICY_ORIGIN_DEFAULT {
			request.Precondition = &api.ReplaceAuthorizationPolicyRequest_ExpectAbsent{ExpectAbsent: &emptypb.Empty{}}
		}
		_, err = policy.ReplaceAuthorizationPolicy(ctx, nha141BearerRequest(request, tokens.AccessToken))
		if err != nil {
			t.Fatal("native policy CAS", err)
		}
	}

	publish := func(index int, on bool, causes ...domain.Causality) {
		t.Helper()
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		value, at := domain.NewSwitchValue(on), time.Now().UTC()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(causes) > 1 {
			t.Fatal("multiple fake causes")
		}
		if len(causes) == 1 {
			observation, err = observation.WithCausality(causes[0])
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := session.Publish(ctx, bindings[index], observation); err != nil {
			t.Fatal(err)
		}
	}
	capture := func(token string, size int32, complete bool) *nha147EntityCapture {
		t.Helper()
		created, err := views.CreateEntityView(ctx, nha141BearerRequest(&api.CreateEntityViewRequest{Parent: home.String(), EntityView: &api.EntityView{}}, token))
		if err != nil {
			t.Fatal("authenticated entity capture", err)
		}
		page, err := views.QueryEntityView(ctx, nha141BearerRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageSize: size}, token))
		if err != nil {
			t.Fatal("entity snapshot first page", err)
		}
		result := &nha147EntityCapture{Name: created.Msg.Name, Page: page.Msg}
		result.Entries = append(result.Entries, page.Msg.Entries...)
		for complete && page.Msg.NextPageToken != "" {
			if page.Msg.ResumeToken != "" {
				t.Fatal("partial snapshot emitted checkpoint")
			}
			page, err = views.QueryEntityView(ctx, nha141BearerRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageSize: size, PageToken: page.Msg.NextPageToken}, token))
			if err != nil {
				t.Fatal("complete entity snapshot", err)
			}
			result.Entries = append(result.Entries, page.Msg.Entries...)
		}
		result.Resume = page.Msg.ResumeToken
		if complete && result.Resume == "" {
			t.Fatal("complete snapshot omitted checkpoint")
		}
		return result
	}
	open := func(size int32) *nha147EntityCapture { return capture(tokens.AccessToken, size, true) }
	watchRequest := func(c *nha147EntityCapture) *api.WatchEntityViewRequest {
		return &api.WatchEntityViewRequest{Name: c.Name, ResumeToken: c.Resume}
	}
	closeCapture := func(c *nha147EntityCapture, token string) {
		_, _ = views.DeleteEntityView(ctx, nha141BearerRequest(&api.DeleteEntityViewRequest{Name: c.Name}, token))
	}
	power := func(entry *api.EntityViewEntry) api.PowerValue {
		for _, capability := range entry.GetCapabilities() {
			if capability.GetPower().GetObservation() != nil {
				return capability.GetPower().GetObservation().GetValue()
			}
		}
		return api.PowerValue_POWER_VALUE_UNSPECIFIED
	}
	watchError := func(request *api.WatchEntityViewRequest, token string) error {
		stream, err := views.WatchEntityView(ctx, nha141BearerRequest(request, token))
		if err == nil {
			defer stream.Close()
			if stream.Receive() {
				t.Fatal("invalidated capture emitted state")
			}
			err = stream.Err()
		}
		return err
	}
	assertResync := func(err error) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || nha147FailureKind(t, err) != api.ApiErrorReason_API_ERROR_REASON_RESYNC_REQUIRED {
			t.Fatal("invalidation lost canonical resync", err)
		}
	}
	publish(0, false)
	publish(1, false)
	empty := open(1)
	if len(empty.Entries) != 0 {
		t.Fatal("empty admin policy exposed equipment")
	}
	closeCapture(empty, tokens.AccessToken)
	// Authorize the object before exposing command payload or stale-binding diagnostics.
	denied := &api.ExecuteCapabilityRequest{Name: entityName(0) + "/capabilities/denied", RequestId: nha114RequestID("nha147-empty-policy"), ExpireTime: timestamppb.New(time.Now().Add(time.Minute)), Command: &api.ExecuteCapabilityRequest_Power{Power: &api.SetPowerCommand{Value: api.PowerValue_POWER_VALUE_ON}}}
	if _, err := capabilities.ExecuteCapability(ctx, nha141BearerRequest(denied, tokens.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied || driver.emitted.Load() != 0 {
		t.Fatal("empty admin policy dispatched", err)
	}
	grantPolicy(nha203SessionAccount(t, tokens.Session).String(), true, true, true)
	first := capture(tokens.AccessToken, 1, false)
	if len(first.Entries) != 1 || first.Page.NextPageToken == "" || first.Resume != "" || power(first.Entries[0]) != api.PowerValue_POWER_VALUE_OFF {
		t.Fatal("initial coherent partial page", first)
	}
	publish(0, true)
	pageRequest := &api.QueryEntityViewRequest{Name: first.Name, PageSize: 1, PageToken: first.Page.NextPageToken}
	second, err := views.QueryEntityView(ctx, nha141BearerRequest(pageRequest, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	again, err := views.QueryEntityView(ctx, nha141BearerRequest(pageRequest, tokens.AccessToken))
	if err != nil || !proto.Equal(&api.QueryEntityViewResponse{Entries: second.Msg.Entries}, &api.QueryEntityViewResponse{Entries: again.Msg.Entries}) || second.Msg.NextPageToken != "" || again.Msg.NextPageToken != "" || second.Msg.ResumeToken == "" || again.Msg.ResumeToken == "" {
		t.Fatal("immutable page/checkpoint changed", err)
	}
	first.Resume = second.Msg.ResumeToken
	first.Entries = append(first.Entries, second.Msg.Entries...)
	for _, entry := range first.Entries {
		if power(entry) != api.PowerValue_POWER_VALUE_OFF {
			t.Fatal("concurrent observation changed captured page")
		}
	}
	watch, err := views.WatchEntityView(ctx, nha141BearerRequest(watchRequest(first), tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if !watch.Receive() || power(watch.Msg().GetUpsert()) != api.PowerValue_POWER_VALUE_ON || watch.Msg().ResumeToken == "" {
		t.Fatal("snapshot/live suffix mismatch", watch.Err())
	}
	watch.Close()
	// Epoch/configuration/rights/filter binding is sealed, not client-editable.
	for _, token := range []string{"invalid", first.Resume[:len(first.Resume)-1] + "!"} {
		request := watchRequest(first)
		request.ResumeToken = token
		assertResync(watchError(request, tokens.AccessToken))
	}
	other := open(1)
	wrongCapture := watchRequest(other)
	wrongCapture.ResumeToken = first.Resume
	assertResync(watchError(wrongCapture, tokens.AccessToken))
	closeCapture(other, tokens.AccessToken)
	slowAccount, err := apiv2connect.NewAccountServiceClient(client, base).CreateAccount(ctx, nha141BearerRequest(&api.CreateAccountRequest{Parent: home.String(), Account: &api.Account{LoginName: "NHA147 Reader", DisplayName: "NHA147 Reader", Role: api.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	slowTokens := loginAccount("NHA147 Reader")
	empty = capture(slowTokens.AccessToken, 1, true)
	if len(empty.Entries) != 0 {
		t.Fatal("empty user policy exposed equipment")
	}
	closeCapture(empty, slowTokens.AccessToken)
	grantPolicy(slowAccount.Msg.Name, true, true, true)
	if _, err := views.QueryEntityView(ctx, nha141BearerRequest(pageRequest, slowTokens.AccessToken)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("capture crossed credential binding", err)
	}
	slowPage := capture(slowTokens.AccessToken, 1, true)
	fastPage := open(1)
	publish(0, false)
	// Consume >8MiB while a separate real HTTP/2 reader leaves its 4MiB window unread.
	slowConnection, _ := nha147UnreadH2(t, ctx, base, roots, apiv2connect.EntityViewServiceWatchEntityViewProcedure, watchRequest(slowPage), slowTokens.AccessToken, 4<<20)
	fast, err := views.WatchEntityView(ctx, nha141BearerRequest(watchRequest(fastPage), tokens.AccessToken))
	if err != nil || !fast.Receive() || fast.Msg().GetUpsert() == nil {
		t.Fatal("fast stream did not start", err)
	}
	fastBytes := 0
	for count := 0; count <= 16384 || fastBytes <= 8<<20; count++ {
		publish(0, count%2 == 0)
		if !fast.Receive() || fast.Msg().GetUpsert() == nil {
			t.Fatal("slow reader degraded active reader", fast.Err())
		}
		fastBytes += 5 + proto.Size(fast.Msg())
	}
	if slowConnection.State().StreamsActive != 1 {
		t.Fatal("unread slow stream ended before revocation proof")
	}
	grantPolicy(slowAccount.Msg.Name, false, true, true)
	waitClosed := func(connection *http2.ClientConn, reason string) {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for connection.State().StreamsActive != 0 {
			if time.Now().After(until) {
				t.Fatal(reason)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitClosed(slowConnection, "policy withdrawal retained blocked entity Send")
	// A different account may also be told to recapture after publication; either
	// valid state or canonical resync is explicit, and neither stalls admission.
	publish(0, false)
	if fast.Receive() {
		if fast.Msg().GetUpsert() == nil {
			t.Fatal("fast witness lost typed membership")
		}
	} else {
		assertResync(fast.Err())
	}
	fast.Close()
	closeCapture(fastPage, tokens.AccessToken)
	freshFast := open(1)
	publish(0, true)
	fast, err = views.WatchEntityView(ctx, nha141BearerRequest(watchRequest(freshFast), tokens.AccessToken))
	if err != nil || !fast.Receive() || fast.Msg().GetUpsert() == nil {
		t.Fatal("fast witness could not recapture", err)
	}
	fast.Close()
	closeCapture(freshFast, tokens.AccessToken)
	if sessions, err := apiv2connect.NewAuthServiceClient(client, base).ListSessions(ctx, nha141BearerRequest(&api.ListSessionsRequest{Parent: slowAccount.Msg.Name}, slowTokens.AccessToken)); err != nil || len(sessions.Msg.Sessions) == 0 {
		t.Fatal("policy withdrawal revoked credential", err)
	}
	empty = capture(slowTokens.AccessToken, 1, true)
	if len(empty.Entries) != 0 {
		t.Fatal("withdrawn view exposed equipment")
	}
	closeCapture(empty, slowTokens.AccessToken)
	grantPolicy(slowAccount.Msg.Name, true, true, true)
	readablePage := capture(slowTokens.AccessToken, 1, true)
	publish(0, false)
	readable, err := views.WatchEntityView(ctx, nha141BearerRequest(watchRequest(readablePage), slowTokens.AccessToken))
	if err != nil || !readable.Receive() || readable.Msg().GetUpsert() == nil {
		t.Fatal("readable policy watch did not become idle", err)
	}
	grantPolicy(slowAccount.Msg.Name, false, true, true)
	if readable.Receive() {
		t.Fatal("withdrawn policy emitted a state")
	}
	assertResync(readable.Err())
	readable.Close()
	grantPolicy(slowAccount.Msg.Name, true, true, true)
	assertResync(watchError(watchRequest(first), tokens.AccessToken))
	// These captures become invalid after policy publication; the operation gets
	// its definition binding from a fresh authorized capability.
	current := open(2)
	var selected *api.Capability
	for _, entry := range current.Entries {
		if entry.Entity.GetName() == entityName(0) {
			for _, capability := range entry.Capabilities {
				if capability.GetPower() != nil {
					selected = capability
				}
			}
		}
	}
	if selected == nil {
		t.Fatal("current capability absent")
	}
	closeCapture(current, tokens.AccessToken)
	command := &api.ExecuteCapabilityRequest{Name: selected.Name, Etag: selected.Etag, RequestId: nha114RequestID("nha147-real-ledger"), ExpireTime: timestamppb.New(time.Now().Add(time.Minute)), Command: &api.ExecuteCapabilityRequest_Power{Power: &api.SetPowerCommand{Value: api.PowerValue_POWER_VALUE_ON}}}
	submit := func(command *api.ExecuteCapabilityRequest) (*connect.Response[longrunningpb.Operation], error) {
		return capabilities.ExecuteCapability(ctx, nha141BearerRequest(command, tokens.AccessToken))
	}
	operationName := func(requestID string, account string) string {
		actor, err := domain.ParseActorID(nha203AccountText(t, nha203NativeAccount(t, ctx, nativeStore, account)))
		if err != nil {
			t.Fatal(err)
		}
		key, err := apitypes.ParseRequestID(requestID)
		if err != nil {
			t.Fatal(err)
		}
		id, err := publicCommandID(actor, &key)
		if err != nil {
			t.Fatal(err)
		}
		text, _ := id.Value()
		return home.String() + "/operations/cmd-" + text
	}
	get := func(name, token string) (*connect.Response[longrunningpb.Operation], error) {
		return operations.GetOperation(ctx, nha141BearerRequest(&longrunningpb.GetOperationRequest{Name: name}, token))
	}
	durable := func(name string) storage.IntentRecord {
		parsed, err := apitypes.ParseOperationName(name)
		if err != nil {
			t.Fatal(err)
		}
		id, err := domain.ParseCommandID(strings.TrimPrefix(parsed.ID(), "cmd-"))
		if err != nil {
			t.Fatal(err)
		}
		record, found, err := nativeStore.CommandIntent(ctx, id)
		if err != nil || !found {
			t.Fatal("native durable occurrence absent", err)
		}
		return record
	}
	observer := nha030ServerObserver(t, cluster.Port)
	if _, err := observer.Exec(ctx, `CREATE FUNCTION public.nha147_refuse_accept() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred acceptance failure'; END $$; CREATE CONSTRAINT TRIGGER nha147_refuse_accept AFTER INSERT ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha147_refuse_accept()`); err != nil {
		t.Fatal(err)
	}
	unknownCommit := proto.Clone(command).(*api.ExecuteCapabilityRequest)
	unknownCommit.RequestId = nha114RequestID("nha147-unknown-commit")
	if _, err := submit(unknownCommit); nha147FailureKind(t, err) != api.ApiErrorReason_API_ERROR_REASON_COMMIT_OUTCOME_UNKNOWN || driver.emitted.Load() != 0 {
		t.Fatal("uncertain acceptance reached driver", err)
	}
	if _, err := observer.Exec(ctx, `DROP TRIGGER nha147_refuse_accept ON public.command_intents; DROP FUNCTION public.nha147_refuse_accept()`); err != nil {
		t.Fatal(err)
	}
	result, err := submit(command)
	if err != nil || result == nil || result.Msg.Name == "" {
		t.Fatal("durable admission without job", result, err)
	}
	name := result.Msg.Name
	nha114Eventually(t, "native dispatch result did not settle", func() bool {
		record := durable(name)
		return driver.emitted.Load() == 1 && record.State() == storage.IntentResult && record.ErrorKind() == storage.IntentNoError && len(record.Response()) > 0
	})
	original := durable(name).Response()
	if _, err := submit(command); err != nil || driver.emitted.Load() != 1 {
		t.Fatal("same key dispatched twice", err)
	}
	conflict := proto.Clone(command).(*api.ExecuteCapabilityRequest)
	conflict.GetPower().Value = api.PowerValue_POWER_VALUE_OFF
	if _, err := submit(conflict); connect.CodeOf(err) != connect.CodeAlreadyExists || driver.emitted.Load() != 1 {
		t.Fatal("key conflict changed intention", err)
	}
	for i, mutate := range []func(*api.ExecuteCapabilityRequest){
		func(v *api.ExecuteCapabilityRequest) { v.Etag = "stale-definition" },
		func(v *api.ExecuteCapabilityRequest) {
			v.Name = home.String() + "/entities/missing/capabilities/missing"
		},
		func(v *api.ExecuteCapabilityRequest) { v.Name = entityName(0) + "/capabilities/unavailable" },
		func(v *api.ExecuteCapabilityRequest) { v.ExpireTime = timestamppb.New(time.Now().Add(-time.Second)) },
		func(v *api.ExecuteCapabilityRequest) { v.RequestId = "not-a-uuid" },
		func(v *api.ExecuteCapabilityRequest) { v.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01}) },
		func(v *api.ExecuteCapabilityRequest) {
			v.Command = &api.ExecuteCapabilityRequest_Level{Level: &api.SetLevelCommand{Target: &api.SetLevelCommand_Percentage{Percentage: 50}}}
		},
	} {
		invalid := proto.Clone(command).(*api.ExecuteCapabilityRequest)
		invalid.RequestId = nha114RequestID(fmt.Sprintf("nha147-refused-%d", i))
		mutate(invalid)
		if response, err := submit(invalid); err == nil {
			t.Fatal("invalid binding admitted", response)
		}
		if driver.emitted.Load() != 1 {
			t.Fatal("refused binding reached driver")
		}
	}
	stored, err := get(name, tokens.AccessToken)
	if err != nil || nha114Metadata(t, stored.Msg).Phase == api.CommandPhase_COMMAND_PHASE_CONFIRMED {
		t.Fatal("dispatch became confirmation", err)
	}
	admitted := driver.captured()
	if err := admitted.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	publish(0, true)
	acked, err := get(name, tokens.AccessToken)
	if err != nil || nha114Metadata(t, acked.Msg).Phase != api.CommandPhase_COMMAND_PHASE_ACKNOWLEDGED {
		t.Fatal("uncorrelated equality confirmed ACK", err)
	}
	proof, err := admitted.Causality()
	if err != nil {
		t.Fatal(err)
	}
	publish(0, true, proof)
	nha114Eventually(t, "qualified native proof did not confirm operation", func() bool {
		confirmed, err := get(name, tokens.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(durable(name).Response(), original) {
			t.Fatal("qualified proof rewrote original durable dispatch")
		}
		return confirmed.Msg.Done && nha114Metadata(t, confirmed.Msg).Phase == api.CommandPhase_COMMAND_PHASE_CONFIRMED
	})
	secondCommand := proto.Clone(command).(*api.ExecuteCapabilityRequest)
	secondCommand.RequestId = nha114RequestID("nha147-pending-revocation")
	if _, err := submit(secondCommand); err != nil {
		t.Fatal("pending revocation fixture", err)
	}
	nha114Eventually(t, "second intention did not dispatch", func() bool { return driver.emitted.Load() == 2 })
	if _, err := observer.Exec(ctx, `CREATE FUNCTION public.nha147_refuse_result() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred result failure'; END $$; CREATE CONSTRAINT TRIGGER nha147_refuse_result AFTER UPDATE ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.state IN ('result','indeterminate')) EXECUTE FUNCTION public.nha147_refuse_result()`); err != nil {
		t.Fatal(err)
	}
	unknownResult := proto.Clone(command).(*api.ExecuteCapabilityRequest)
	unknownResult.RequestId = nha114RequestID("nha147-unknown-result")
	unknownName := operationName(unknownResult.RequestId, nha203SessionAccount(t, tokens.Session).String())
	rpcUnknown := func(err error) {
		t.Helper()
		var failure *connect.Error
		if !errors.As(err, &failure) {
			t.Fatal("uncertainty lost structured transport error", err)
		}
		raw := &statuspb.Status{Code: int32(failure.Code())}
		for _, detail := range failure.Details() {
			value, err := detail.Value()
			if err != nil {
				t.Fatal(err)
			}
			packed, err := anypb.New(value)
			if err != nil {
				t.Fatal(err)
			}
			raw.Details = append(raw.Details, packed)
		}
		fault, err := apierrors.AdmitStatus(raw)
		if err != nil || fault.Reason() != api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN {
			t.Fatal("uncertainty lost canonical status", err)
		}
		operation, present := fault.Operation()
		if !present || operation.String() != unknownName {
			t.Fatal("uncertainty lost authorized reconciliation handle")
		}
	}
	assertUnknown := func(result *connect.Response[longrunningpb.Operation], err error) {
		t.Helper()
		if err != nil {
			rpcUnknown(err)
			return
		}
		if result == nil || result.Msg.Name != unknownName {
			t.Fatal("admitted uncertain intention lost operation identity")
		}
		nha114Eventually(t, "post-effect persistence failure remained pending", func() bool {
			observed, err := get(unknownName, tokens.AccessToken)
			if err != nil {
				rpcUnknown(err)
				return true
			}
			if !observed.Msg.Done {
				return false
			}
			if nha114Metadata(t, observed.Msg).Phase != api.CommandPhase_COMMAND_PHASE_INDETERMINATE || observed.Msg.GetError() == nil {
				t.Fatal("uncertain effect became success", observed.Msg)
			}
			fault, err := apierrors.AdmitStatus(observed.Msg.GetError())
			if err != nil || fault.Reason() != api.ApiErrorReason_API_ERROR_REASON_EFFECT_OUTCOME_UNKNOWN {
				t.Fatal("uncertain operation lost canonical reason", err)
			}
			return true
		})
	}
	uncertain, err := submit(unknownResult)
	assertUnknown(uncertain, err)
	if driver.emitted.Load() != 3 {
		t.Fatal("uncertain intention did not dispatch exactly once")
	}
	if _, err := observer.Exec(ctx, `DROP TRIGGER nha147_refuse_result ON public.command_intents; DROP FUNCTION public.nha147_refuse_result()`); err != nil {
		t.Fatal(err)
	}
	uncertain, err = submit(unknownResult)
	assertUnknown(uncertain, err)
	if driver.emitted.Load() != 3 {
		t.Fatal("uncertain effect replayed")
	}
	if _, err := get(name, slowTokens.AccessToken); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("cross-authority operation read admitted", err)
	}
	if _, err := get(home.String()+"/operations/cmd-absent", slowTokens.AccessToken); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("absent operation exposed", err)
	}
	// Native rights changed: the user obtains its own current definition binding.
	readerCapture := capture(slowTokens.AccessToken, 2, true)
	var readerCapability *api.Capability
	for _, entry := range readerCapture.Entries {
		if entry.Entity.GetName() == entityName(0) {
			readerCapability = entry.Capabilities[0]
		}
	}
	if readerCapability == nil {
		t.Fatal("reader capability absent")
	}
	closeCapture(readerCapture, slowTokens.AccessToken)
	readerCommand := proto.Clone(command).(*api.ExecuteCapabilityRequest)
	readerCommand.Etag = readerCapability.Etag
	readerCommand.RequestId = nha114RequestID("nha147-reader-policy")
	readerResult, err := capabilities.ExecuteCapability(ctx, nha141BearerRequest(readerCommand, slowTokens.AccessToken))
	if err != nil {
		t.Fatal("reader fixture dispatch", err)
	}
	nha114Eventually(t, "reader intention did not dispatch", func() bool { return driver.emitted.Load() == 4 })
	readerName := readerResult.Msg.Name
	if readable, err := get(readerName, slowTokens.AccessToken); err != nil || nha114Metadata(t, readable.Msg).Phase != api.CommandPhase_COMMAND_PHASE_DISPATCHED {
		t.Fatal("reader operation absent", err)
	}
	// Unary protobuf, not a fabricated streaming RPC. One byte of flow credit
	// holds the real operation response body while policy publication proceeds.
	blockedAction, _ := nha147UnreadH2(t, ctx, base, roots, longrunningpbconnect.OperationsGetOperationProcedure, &longrunningpb.GetOperationRequest{Name: readerName}, slowTokens.AccessToken, 1)
	if blockedAction.State().StreamsActive != 1 {
		t.Fatal("operation response not held by flow control")
	}
	if witness, err := get(name, tokens.AccessToken); err != nil || witness.Msg.Name != name {
		t.Fatal("blocked operation stalled independent reader", err)
	}
	grantPolicy(slowAccount.Msg.Name, true, false, false)
	waitClosed(blockedAction, "policy withdrawal retained blocked operation Send")
	for _, method := range []string{"get", "wait"} {
		var err error
		if method == "get" {
			_, err = get(readerName, slowTokens.AccessToken)
		} else {
			_, err = operations.WaitOperation(ctx, nha141BearerRequest(&longrunningpb.WaitOperationRequest{Name: readerName}, slowTokens.AccessToken))
		}
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatal("withdrawn operation read admitted", method, err)
		}
	}
	refusedWrite := proto.Clone(readerCommand).(*api.ExecuteCapabilityRequest)
	refusedWrite.RequestId = nha114RequestID("nha147-write-denied")
	if _, err := capabilities.ExecuteCapability(ctx, nha141BearerRequest(refusedWrite, slowTokens.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied || driver.emitted.Load() != 4 {
		t.Fatal("withdrawn write reached driver", err)
	}
	if sessions, err := apiv2connect.NewAuthServiceClient(client, base).ListSessions(ctx, nha141BearerRequest(&api.ListSessionsRequest{Parent: slowAccount.Msg.Name}, slowTokens.AccessToken)); err != nil || len(sessions.Msg.Sessions) == 0 {
		t.Fatal("action withdrawal revoked credential", err)
	}
	remaining := capture(slowTokens.AccessToken, 1, true)
	if len(remaining.Entries) != 2 {
		t.Fatal("action withdrawal removed remaining state read")
	}
	closeCapture(remaining, slowTokens.AccessToken)
	witness := open(1)
	if len(witness.Entries) != 2 {
		t.Fatal("withdrawal blocked independent account")
	}
	closeCapture(witness, tokens.AccessToken)
	if witness, err := get(name, tokens.AccessToken); err != nil || witness.Msg.Name != name {
		t.Fatal("withdrawal broke independent operation reader", err)
	}
	secondName := operationName(secondCommand.RequestId, nha203SessionAccount(t, tokens.Session).String())
	blockedCredential, _ := nha147UnreadH2(t, ctx, base, roots, longrunningpbconnect.OperationsGetOperationProcedure, &longrunningpb.GetOperationRequest{Name: secondName}, tokens.AccessToken, 1)
	if blockedCredential.State().StreamsActive != 1 {
		t.Fatal("credential revocation lacked blocked response")
	}
	// Keep an actual streaming reader idle to verify its structured credential terminal too.
	liveCapture := open(2)
	publish(0, false)
	live, err := views.WatchEntityView(ctx, nha141BearerRequest(watchRequest(liveCapture), tokens.AccessToken))
	if err != nil || !live.Receive() {
		t.Fatal("credential entity stream absent", err)
	}
	if _, err := apiv2connect.NewAuthServiceClient(client, base).RevokeSession(ctx, nha141BearerRequest(&api.RevokeSessionRequest{Name: tokens.Session.Name}, tokens.AccessToken)); err != nil {
		t.Fatal(err)
	}
	waitClosed(blockedCredential, "credential revocation retained blocked operation Send")
	if live.Receive() {
		t.Fatal("entity frame emitted after credential revocation")
	}
	var ended *connect.Error
	if !errors.As(live.Err(), &ended) || ended.Code() != connect.CodeUnauthenticated {
		t.Fatal("revoked reader lost termination", live.Err())
	}
	foundTermination := false
	for _, detail := range ended.Details() {
		value, err := detail.Value()
		if err == nil {
			if reason, ok := value.(*errdetails.ErrorInfo); ok && reason.Domain == apierrors.Domain() && reason.Reason == "AUTHENTICATION_REQUIRED" {
				foundTermination = true
			}
		}
	}
	if !foundTermination {
		t.Fatal("revoked reader lost detail141")
	}
	live.Close()
	stop()
	start(true)
	tokens = login()
	views = apiv2connect.NewEntityViewServiceClient(client, base)
	homes = apiv2connect.NewHomeServiceClient(client, base)
	capabilities = apiv2connect.NewCapabilityServiceClient(client, base)
	operations = longrunningpbconnect.NewOperationsClient(client, base)
	stored, err = get(name, tokens.AccessToken)
	if err != nil || !stored.Msg.Done || nha114Metadata(t, stored.Msg).LiveProgressAvailable || !bytes.Equal(durable(name).Response(), original) {
		t.Fatal("restart lost durable terminal or invented live tracker", err)
	}
	if _, err := submit(command); err != nil || driver.emitted.Load() != 4 {
		t.Fatal("restart replayed effect", err)
	}
	recoveredName := operationName(unknownResult.RequestId, nha203SessionAccount(t, tokens.Session).String())
	recovered, err := get(recoveredName, tokens.AccessToken)
	if err != nil || !recovered.Msg.Done || nha114Metadata(t, recovered.Msg).LiveProgressAvailable || nha114Metadata(t, recovered.Msg).Phase != api.CommandPhase_COMMAND_PHASE_INDETERMINATE || durable(recoveredName).State() != storage.IntentIndeterminate {
		t.Fatal("restart erased uncertain effect", err)
	}
	uncertain, err = submit(unknownResult)
	assertUnknown(uncertain, err)
	if driver.emitted.Load() != 4 {
		t.Fatal("recovered uncertainty replayed effect")
	}
	// Missing live progress is explicit metadata, not a streaming EOF surrogate.
	missing, err := get(secondName, tokens.AccessToken)
	if err != nil || nha114Metadata(t, missing.Msg).LiveProgressAvailable {
		t.Fatal("restart invented pending live tracker", err)
	}
	stop()
	start(false)
	tokens = login()
	views = apiv2connect.NewEntityViewServiceClient(client, base)
	homes = apiv2connect.NewHomeServiceClient(client, base)
	if response := open(1); response.Name == "" || response.Resume == "" {
		t.Fatal("default no-job Run omitted common entity RPCs")
	}
	if driver.emitted.Load() != 4 {
		t.Fatal("read/reconnect/default startup replayed command")
	}

}

// NewClientConn uses the pinned x/net client implementation. ConfigureTransports
// carries net/http.HTTP2Config into its actual SETTINGS_INITIAL_WINDOW_SIZE.
// A one-byte window retains the first protobuf response in Send without a
// synthetic action tracker, extra action updates or a read from the body.
func nha147UnreadH2(t *testing.T, ctx context.Context, base string, roots *x509.CertPool, procedure string, message proto.Message, token string, window int) (*http2.ClientConn, *http.Response) {
	t.Helper()
	transport := &http.Transport{HTTP2: &http.HTTP2Config{MaxReceiveBufferPerStream: window}}
	h2, err := http2.ConfigureTransports(transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	dialer := net.Dialer{Timeout: 3 * time.Second}
	tlsConnection, err := tls.DialWithDialer(&dialer, "tcp", strings.TrimPrefix(base, "https://"), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tlsConnection.Close() })
	if tlsConnection.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("blocked response connection did not negotiate HTTP/2")
	}
	wire := &nha147H2SettingsConn{Conn: tlsConnection}
	connection, err := h2.NewClientConn(wire)
	if err != nil {
		t.Fatal(err)
	}
	if actual, present := wire.initialWindow(); !present || actual != uint32(window) {
		t.Fatal("HTTP/2 stream window was not actually advertised", actual, present)
	}
	t.Cleanup(func() { _ = connection.Close() })
	encoded, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	payload := encoded
	contentType := "application/proto"
	if strings.Contains(procedure, "/Watch") {
		contentType = "application/connect+proto"
		payload = make([]byte, 5+len(encoded))
		binary.BigEndian.PutUint32(payload[1:5], uint32(len(encoded)))
		copy(payload[5:], encoded)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+procedure, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := connection.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK || response.ProtoMajor != 2 {
		t.Fatal("blocked response did not reach real HTTP/2 Send", response.StatusCode, response.Proto)
	}
	return connection, response
}

// Capture only the bounded client handshake bytes actually written over TLS.
// The body is never read. Parsing SETTINGS needs no second HTTP/2 client loop.
type nha147H2SettingsConn struct {
	*tls.Conn
	mu      sync.Mutex
	written []byte
}

func (c *nha147H2SettingsConn) Write(data []byte) (int, error) {
	n, err := c.Conn.Write(data)
	c.mu.Lock()
	if len(c.written) < 64<<10 {
		c.written = append(c.written, data[:min(n, (64<<10)-len(c.written))]...)
	}
	c.mu.Unlock()
	return n, err
}
func (c *nha147H2SettingsConn) initialWindow() (uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !bytes.HasPrefix(c.written, []byte(http2.ClientPreface)) {
		return 0, false
	}
	data := c.written[len(http2.ClientPreface):]
	for len(data) >= 9 {
		size := int(data[0])<<16 | int(data[1])<<8 | int(data[2])
		if len(data) < 9+size {
			return 0, false
		}
		if data[3] == byte(http2.FrameSettings) && data[4]&1 == 0 {
			for payload := data[9 : 9+size]; len(payload) >= 6; payload = payload[6:] {
				if binary.BigEndian.Uint16(payload[:2]) == uint16(http2.SettingInitialWindowSize) {
					return binary.BigEndian.Uint32(payload[2:6]), true
				}
			}
		}
		data = data[9+size:]
	}
	return 0, false
}

func nha147FailureKind(t *testing.T, err error) api.ApiErrorReason {
	t.Helper()
	info := nha200Info(t, err)
	value, known := api.ApiErrorReason_value["API_ERROR_REASON_"+info.Reason]
	if !known || value == 0 {
		t.Fatal("unknown action failure reason", info.Reason)
	}
	return api.ApiErrorReason(value)
}
