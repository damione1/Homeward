package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	"github.com/damione1/Homeward/gen/home/actions/v1/actionsv1connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	registryv1 "github.com/damione1/Homeward/gen/home/registry/v1"
	statev1 "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/scope"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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
	factory := func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
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
	operator := authv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	const password = "NHA147 synthetic native local password"
	admin, err := operator.BootstrapAdmin(ctx, connect.NewRequest(&authv1.BootstrapAdminRequest{LoginName: "NHA147 Admin", DisplayName: "NHA147 Admin", Password: password}))
	if err != nil {
		t.Fatal(err)
	}
	loginAccount := func(name string) *authv1.SessionTokens {
		t.Helper()
		response, err := authv1connect.NewAuthServiceClient(client, base).StartLogin(ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil || response.Msg.Decision.GetAuthenticated() == nil {
			t.Fatal("native login", err)
		}
		return response.Msg.Decision.GetAuthenticated()
	}
	login := func() *authv1.SessionTokens { return loginAccount("NHA147 Admin") }
	tokens := login()
	state := statev1connect.NewStateServiceClient(client, base)
	action := actionsv1connect.NewActionServiceClient(client, base)
	// Explicit fixture grants go through the real native policy CAS API.
	// Admin is an account-management role, never implicit equipment access.
	grantPolicy := func(account *authv1.AccountId, stateRead, actionRead, actionWrite bool) {
		t.Helper()
		policy := authv1connect.NewAuthorizationPolicyServiceClient(client, base)
		current, err := policy.GetPolicy(ctx, nha141BearerRequest(&authv1.GetPolicyRequest{AccountId: account}, tokens.AccessToken))
		if err != nil || current.Msg.GetPresent() == nil {
			t.Fatal("native policy read", err)
		}
		var source strings.Builder
		fmt.Fprintf(&source, "schema: homeward.authorization/1\naccount_id: %q\n", account.Value)
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
		_, err = policy.ReplacePolicy(ctx, nha141BearerRequest(&authv1.ReplacePolicyRequest{AccountId: account, YamlSource: []byte(source.String()), Precondition: &authv1.ReplacePolicyRequest_Expected{Expected: current.Msg.GetPresent().Version}}, tokens.AccessToken))
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
	open := func(size uint32) *statev1.OpenViewResponse {
		t.Helper()
		response, err := state.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{PageSize: size, Filter: &statev1.ViewFilter{}}, tokens.AccessToken))
		if err != nil {
			t.Fatal(err)
		}
		return response.Msg
	}
	publish(0, false)
	publish(1, false)
	if empty := open(1); len(empty.Entities) != 0 || len(empty.States) != 0 {
		t.Fatal("empty admin policy exposed synthetic equipment")
	}
	{
		epoch, err := sequence.CurrentAdmissionEpoch()
		if err != nil {
			t.Fatal(err)
		}
		stamp, err := owner.CurrentConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		revision, _, err := stamp.Components()
		if err != nil {
			t.Fatal(err)
		}
		denied := &actions.CommandRequest{CommandId: "nha147-empty-policy", AdmissionEpoch: epoch, ConfigRevision: revision, Target: &registryv1.EntityRef{Reference: &registryv1.EntityRef_HaAlias{HaAlias: aliases[0]}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay", Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
		if _, err := action.SubmitAction(ctx, nha141BearerRequest(&actions.SubmitActionRequest{Command: denied}, tokens.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied || driver.emitted.Load() != 0 {
			t.Fatal("empty admin policy dispatched a command", err)
		}
	}
	grantPolicy(admin.Msg.Account.Id, true, true, true)
	first := open(1)
	if len(first.Entities) != 1 || first.NextPageToken == "" || len(first.States) != 1 || first.States[0].GetSwitchOn() {
		t.Fatal("initial coherent page", first)
	}
	stamp, err := owner.CurrentConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revision, manifestHash, err := stamp.Components()
	if err != nil || first.Cursor.ConfigRevision != revision || !bytes.Equal(first.Cursor.ConfigSha256, manifestHash[:]) {
		t.Fatal("public cursor did not borrow actual Owner029 generation")
	}
	publish(0, true)
	pageRequest := &statev1.ReadPageRequest{SnapshotToken: first.SnapshotToken, PageToken: first.NextPageToken}
	second, err := state.ReadPage(ctx, nha141BearerRequest(pageRequest, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	again, err := state.ReadPage(ctx, nha141BearerRequest(pageRequest, tokens.AccessToken))
	if err != nil || !proto.Equal(second.Msg, again.Msg) || !proto.Equal(first.Cursor, second.Msg.Page.Cursor) {
		t.Fatal("page/cursor changed after concurrent publication", err)
	}
	watch, err := state.WatchView(ctx, nha141BearerRequest(&statev1.WatchViewRequest{Cursor: first.Cursor, Filter: &statev1.ViewFilter{}}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if !watch.Receive() || watch.Msg().GetDelta() == nil || len(watch.Msg().GetDelta().States) != 1 || !watch.Msg().GetDelta().States[0].GetSwitchOn() {
		t.Fatal("snapshot/live suffix mismatch", watch.Err())
	}
	watch.Close()
	for _, tc := range []struct {
		reason statev1.ResyncReason
		mutate func(*statev1.WatchViewRequest)
	}{
		{statev1.ResyncReason_RESYNC_REASON_EPOCH_CHANGED, func(r *statev1.WatchViewRequest) {
			if r.Cursor.Epoch[0] == '0' {
				r.Cursor.Epoch = "1" + r.Cursor.Epoch[1:]
			} else {
				r.Cursor.Epoch = "0" + r.Cursor.Epoch[1:]
			}
		}},
		{statev1.ResyncReason_RESYNC_REASON_CONFIGURATION_CHANGED, func(r *statev1.WatchViewRequest) { r.Cursor.ConfigRevision++ }},
		{statev1.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED, func(r *statev1.WatchViewRequest) { r.Cursor.AuthorizationRevision++ }},
		{statev1.ResyncReason_RESYNC_REASON_FILTER_CHANGED, func(r *statev1.WatchViewRequest) { r.Filter.EntityIds = []string{first.Entities[0].EntityId} }},
		{statev1.ResyncReason_RESYNC_REASON_INVALID_CURSOR, func(r *statev1.WatchViewRequest) { r.Cursor.AdmissionProof[39] ^= 1 }},
	} {
		request := &statev1.WatchViewRequest{Cursor: proto.Clone(first.Cursor).(*statev1.ViewCursor), Filter: &statev1.ViewFilter{}}
		tc.mutate(request)
		stream, err := state.WatchView(ctx, nha141BearerRequest(request, tokens.AccessToken))
		if err != nil || !stream.Receive() || stream.Msg().GetResyncRequired().GetReason() != tc.reason || stream.Receive() || stream.Err() != nil {
			t.Fatal("typed resync was collapsed or replay fabricated", tc.reason, err)
		}
		stream.Close()
	}
	// The dedicated x/net HTTP/2 client advertises a 4 MiB stream window.
	// Leave its response entirely unread while a second real client consumes
	// more than twice that wire volume from the same published state stream.
	slowAccount, err := authv1connect.NewAccountServiceClient(client, base).CreateAccount(ctx, nha141BearerRequest(&authv1.CreateAccountRequest{LoginName: "NHA147 Reader", DisplayName: "NHA147 Reader", Role: authv1.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	slowTokens := loginAccount("NHA147 Reader")
	if empty, err := state.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{PageSize: 1, Filter: &statev1.ViewFilter{}}, slowTokens.AccessToken)); err != nil || len(empty.Msg.Entities) != 0 {
		t.Fatal("empty user policy exposed equipment", err)
	}
	grantPolicy(slowAccount.Msg.Account.Id, true, true, true)
	if _, err := state.ReadPage(ctx, nha141BearerRequest(pageRequest, slowTokens.AccessToken)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("snapshot token crossed a credential binding", err)
	}
	slowPage, err := state.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{PageSize: 1, Filter: &statev1.ViewFilter{}}, slowTokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	fastPage := open(1)
	publish(0, false)
	slowTransport := &http2.Transport{}
	defer slowTransport.CloseIdleConnections()
	dialer := net.Dialer{Timeout: 3 * time.Second}
	tlsConnection, err := tls.DialWithDialer(&dialer, "tcp", strings.TrimPrefix(base, "https://"), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer tlsConnection.Close()
	if tlsConnection.ConnectionState().NegotiatedProtocol != "h2" {
		t.Fatal("slow TLS connection did not negotiate HTTP/2")
	}
	slowConnection, err := slowTransport.NewClientConn(tlsConnection)
	if err != nil {
		t.Fatal(err)
	}
	defer slowConnection.Close()
	slowCtx, stopSlow := context.WithCancel(ctx)
	defer stopSlow()
	watchMessage, err := proto.Marshal(&statev1.WatchViewRequest{Cursor: slowPage.Msg.Cursor, Filter: &statev1.ViewFilter{}})
	if err != nil {
		t.Fatal(err)
	}
	envelope := make([]byte, 5+len(watchMessage))
	binary.BigEndian.PutUint32(envelope[1:5], uint32(len(watchMessage)))
	copy(envelope[5:], watchMessage)
	slowRequest, err := http.NewRequestWithContext(slowCtx, http.MethodPost, base+statev1connect.StateServiceWatchViewProcedure, bytes.NewReader(envelope))
	if err != nil {
		t.Fatal(err)
	}
	slowRequest.Header.Set("Content-Type", "application/connect+proto")
	slowRequest.Header.Set("Connect-Protocol-Version", "1")
	slowRequest.Header.Set("Authorization", "Bearer "+slowTokens.AccessToken)
	slow, err := slowConnection.RoundTrip(slowRequest)
	if err != nil {
		t.Fatal(err)
	}
	if slow.StatusCode != http.StatusOK || slow.ProtoMajor != 2 {
		slow.Body.Close()
		t.Fatal("slow proof did not use a real HTTP/2 stream", slow.StatusCode, slow.Proto)
	}
	defer slow.Body.Close()
	fast, err := state.WatchView(ctx, nha141BearerRequest(&statev1.WatchViewRequest{Cursor: fastPage.Cursor, Filter: &statev1.ViewFilter{}}, tokens.AccessToken))
	if err != nil || !fast.Receive() || fast.Msg().GetDelta() == nil {
		t.Fatal("fast stream did not start", err)
	}
	var fastBytes int
	for count := 0; count <= 16384 || fastBytes <= 8<<20; count++ {
		publish(0, count%2 == 0)
		if !fast.Receive() || fast.Msg().GetDelta() == nil {
			t.Fatal("slow reader degraded active reader", fast.Err())
		}
		fastBytes += 5 + proto.Size(fast.Msg())
	}
	if active := slowConnection.State().StreamsActive; active != 1 {
		t.Fatal("unread slow stream ended before the blocked-send revocation proof", active)
	}
	// Withdraw rights while Send is blocked; the credential is not revoked.
	grantPolicy(slowAccount.Msg.Account.Id, false, true, true)
	until := time.Now().Add(3 * time.Second)
	for slowConnection.State().StreamsActive != 0 {
		if time.Now().After(until) {
			t.Fatal("policy withdrawal left an unread HTTP/2 Send active beyond its bounded error grace")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Completion was observed without reading or cancelling the client body.
	stopSlow()
	if err := slow.Body.Close(); err != nil {
		t.Fatal(err)
	}
	publish(0, false)
	if !fast.Receive() {
		t.Fatal("policy-invalidated slow reader stalled its fast witness", fast.Err())
	}
	if resync := fast.Msg().GetResyncRequired(); resync != nil {
		if resync.Reason != statev1.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED && resync.Reason != statev1.ResyncReason_RESYNC_REASON_EPOCH_CHANGED {
			t.Fatal("unexpected fast witness invalidation", resync)
		}
	} else if fast.Msg().GetDelta() == nil {
		t.Fatal("fast witness lost its typed frame")
	}
	fast.Close()
	freshFast := open(1)
	publish(0, true)
	fast, err = state.WatchView(ctx, nha141BearerRequest(&statev1.WatchViewRequest{Cursor: freshFast.Cursor, Filter: &statev1.ViewFilter{}}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if !fast.Receive() || fast.Msg().GetDelta() == nil {
		t.Fatal("fast witness could not reopen after policy invalidation", fast.Err())
	}
	fast.Close()
	if page := open(1); page.Cursor == nil {
		t.Fatal("slow stream retained public admissions")
	}
	if sessions, err := authv1connect.NewAuthServiceClient(client, base).ListSessions(ctx, nha141BearerRequest(&authv1.ListSessionsRequest{AccountId: slowAccount.Msg.Account.Id}, slowTokens.AccessToken)); err != nil || len(sessions.Msg.Sessions) == 0 {
		t.Fatal("policy withdrawal revoked the credential", err)
	}
	if empty, err := state.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{PageSize: 1, Filter: &statev1.ViewFilter{}}, slowTokens.AccessToken)); err != nil || len(empty.Msg.Entities) != 0 {
		t.Fatal("withdrawn view still exposed synthetic equipment", err)
	}
	// A normally consuming reader keeps the existing typed resync terminal.
	grantPolicy(slowAccount.Msg.Account.Id, true, true, true)
	readablePage, err := state.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{PageSize: 1, Filter: &statev1.ViewFilter{}}, slowTokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	publish(0, false)
	readable, err := state.WatchView(ctx, nha141BearerRequest(&statev1.WatchViewRequest{Cursor: readablePage.Msg.Cursor, Filter: &statev1.ViewFilter{}}, slowTokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	if !readable.Receive() || readable.Msg().GetDelta() == nil {
		t.Fatal("readable policy watch did not reach its idle point", readable.Err())
	}
	grantPolicy(slowAccount.Msg.Account.Id, false, true, true)
	if !readable.Receive() || readable.Msg().GetResyncRequired().GetReason() != statev1.ResyncReason_RESYNC_REASON_AUTHORIZATION_CHANGED || readable.Receive() || readable.Err() != nil {
		t.Fatal("policy withdrawal lost typed authorization resync", readable.Err())
	}
	readable.Close()
	grantPolicy(slowAccount.Msg.Account.Id, true, true, true)
	expired, err := state.WatchView(ctx, nha141BearerRequest(&statev1.WatchViewRequest{Cursor: first.Cursor, Filter: &statev1.ViewFilter{}}, tokens.AccessToken))
	if err != nil || !expired.Receive() || expired.Msg().GetResyncRequired().GetReason() != statev1.ResyncReason_RESYNC_REASON_REPLAY_EXPIRED || expired.Receive() || expired.Err() != nil {
		t.Fatal("bounded replay did not expire explicitly", err)
	}
	expired.Close()
	epoch, err := sequence.CurrentAdmissionEpoch()
	if err != nil {
		t.Fatal(err)
	}
	command := &actions.CommandRequest{CommandId: "nha147-real-ledger", AdmissionEpoch: epoch, ConfigRevision: first.Cursor.ConfigRevision,
		Target: &registryv1.EntityRef{Reference: &registryv1.EntityRef_HaAlias{HaAlias: aliases[0]}}, CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay",
		Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}, ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	submit := func(command *actions.CommandRequest) (*connect.Response[actions.SubmitActionResponse], error) {
		return action.SubmitAction(ctx, nha141BearerRequest(&actions.SubmitActionRequest{Command: command}, tokens.AccessToken))
	}
	observer := nha030ServerObserver(t, cluster.Port)
	if _, err := observer.Exec(ctx, `CREATE FUNCTION public.nha147_refuse_accept() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred acceptance failure'; END $$;
		CREATE CONSTRAINT TRIGGER nha147_refuse_accept AFTER INSERT ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha147_refuse_accept()`); err != nil {
		t.Fatal(err)
	}
	unknownCommit := proto.Clone(command).(*actions.CommandRequest)
	unknownCommit.CommandId += "-unknown-commit"
	if _, err := submit(unknownCommit); nha147FailureKind(t, err) != actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_COMMIT_OUTCOME_UNKNOWN || driver.emitted.Load() != 0 {
		t.Fatal("unknown acceptance commit lost its phase or reached driver", err)
	}
	if _, err := observer.Exec(ctx, `DROP TRIGGER nha147_refuse_accept ON public.command_intents; DROP FUNCTION public.nha147_refuse_accept()`); err != nil {
		t.Fatal(err)
	}
	result, err := submit(command)
	if err != nil || result.Msg.Result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED || driver.emitted.Load() != 1 {
		t.Fatal("one durable admission without a job", result, err)
	}
	if _, err := submit(command); err != nil || driver.emitted.Load() != 1 {
		t.Fatal("same ID was dispatched again", err)
	}
	conflict := proto.Clone(command).(*actions.CommandRequest)
	conflict.Arguments.GetSetSwitch().On = false
	if _, err := submit(conflict); connect.CodeOf(err) != connect.CodeAlreadyExists || driver.emitted.Load() != 1 {
		t.Fatal("conflicting ID changed the intention", err)
	}
	for _, mutate := range []func(*actions.CommandRequest){
		func(v *actions.CommandRequest) {
			if v.AdmissionEpoch[0] == '0' {
				v.AdmissionEpoch = "1" + v.AdmissionEpoch[1:]
			} else {
				v.AdmissionEpoch = "0" + v.AdmissionEpoch[1:]
			}
		},
		func(v *actions.CommandRequest) { v.ConfigRevision++ },
		func(v *actions.CommandRequest) {
			v.Target = &registryv1.EntityRef{Reference: &registryv1.EntityRef_HaAlias{HaAlias: "switch.nha147_absent"}}
		},
		func(v *actions.CommandRequest) { v.CapabilitySchemaId = "nha147.unregistered" },
		func(v *actions.CommandRequest) { v.Endpoint = "unavailable" },
		func(v *actions.CommandRequest) { v.ExpectedRevision = new(uint64); *v.ExpectedRevision = ^uint64(0) },
		func(v *actions.CommandRequest) { v.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01}) },
		func(v *actions.CommandRequest) {
			v.Arguments.Arguments = &actions.ActionArgs_Extension{Extension: &actions.ExtensionPayload{SchemaId: "nha147.unregistered", Major: 1, DescriptorSha256: make([]byte, 32), ProtobufPayload: []byte{1}}}
		},
	} {
		invalid := proto.Clone(command).(*actions.CommandRequest)
		invalid.CommandId += "-refused-" + time.Now().Format("150405.000000000")
		mutate(invalid)
		response, err := submit(invalid)
		if err == nil && response.Msg.Result.Status != actions.ActionStatus_ACTION_STATUS_FAILED {
			t.Fatal("invalid fence admitted", response)
		}
		if driver.emitted.Load() != 1 {
			t.Fatal("refused fence reached fake driver")
		}
	}
	stored, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: command.CommandId}, tokens.AccessToken))
	if err != nil || stored.Msg.GetProgress() == nil || stored.Msg.GetProgress().Status == actions.ActionStatus_ACTION_STATUS_CONFIRMED {
		t.Fatal("ACK/dispatch became physical confirmation", stored, err)
	}
	admitted := driver.captured()
	if err := admitted.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	publish(0, true)
	acked, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: command.CommandId}, tokens.AccessToken))
	if err != nil || acked.Msg.GetProgress().Status != actions.ActionStatus_ACTION_STATUS_ACKNOWLEDGED {
		t.Fatal("uncorrelated matching state confirmed an ACK", acked, err)
	}
	proof, err := admitted.Causality()
	if err != nil {
		t.Fatal(err)
	}
	publish(0, true, proof)
	confirmed, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: command.CommandId}, tokens.AccessToken))
	if err != nil || confirmed.Msg.GetProgress().Status != actions.ActionStatus_ACTION_STATUS_CONFIRMED || confirmed.Msg.Durable.Response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal("qualified live proof rewrote durable dispatch or failed to confirm", confirmed, err)
	}
	secondCommand := proto.Clone(command).(*actions.CommandRequest)
	secondCommand.CommandId += "-pending-revocation"
	if _, err := submit(secondCommand); err != nil || driver.emitted.Load() != 2 {
		t.Fatal("pending revocation fixture action", err)
	}
	if _, err := observer.Exec(ctx, `CREATE FUNCTION public.nha147_refuse_result() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic deferred result failure'; END $$;
		CREATE CONSTRAINT TRIGGER nha147_refuse_result AFTER UPDATE ON public.command_intents DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.state IN ('result', 'indeterminate')) EXECUTE FUNCTION public.nha147_refuse_result()`); err != nil {
		t.Fatal(err)
	}
	unknownResult := proto.Clone(command).(*actions.CommandRequest)
	unknownResult.CommandId += "-unknown-result"
	if _, err := submit(unknownResult); nha147FailureKind(t, err) != actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_OUTCOME_INDETERMINATE || driver.emitted.Load() != 3 {
		t.Fatal("post-effect persistence failure lost its uncertainty", err)
	}
	if _, err := observer.Exec(ctx, `DROP TRIGGER nha147_refuse_result ON public.command_intents; DROP FUNCTION public.nha147_refuse_result()`); err != nil {
		t.Fatal(err)
	}
	if _, err := submit(unknownResult); nha147FailureKind(t, err) != actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_OUTCOME_INDETERMINATE || driver.emitted.Load() != 3 {
		t.Fatal("unknown-result replay repeated an effect", err)
	}
	// AuthorizeActionRead retains the existing050 historical-actor boundary.
	if _, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: command.CommandId}, slowTokens.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("cross-authority action read admitted", err)
	}
	if _, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: "nha147-absent-intent"}, slowTokens.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("absent action was exposed", err)
	}
	readerCommand := proto.Clone(command).(*actions.CommandRequest)
	readerCommand.CommandId += "-reader-policy"
	if _, err := action.SubmitAction(ctx, nha141BearerRequest(&actions.SubmitActionRequest{Command: readerCommand}, slowTokens.AccessToken)); err != nil || driver.emitted.Load() != 4 {
		t.Fatal("reader policy fixture command", err)
	}
	readableAction, err := action.WatchAction(ctx, nha141BearerRequest(&actions.WatchActionRequest{CommandId: readerCommand.CommandId}, slowTokens.AccessToken))
	if err != nil || !readableAction.Receive() || readableAction.Msg().GetProgress() == nil {
		t.Fatal("policy action reader did not start", err)
	}
	// One-byte stream credit forces the first real progression Send to block.
	blockedAction, _ := nha147UnreadH2(t, ctx, base, roots, actionsv1connect.ActionServiceWatchActionProcedure, &actions.WatchActionRequest{CommandId: readerCommand.CommandId}, slowTokens.AccessToken, 1)
	if blockedAction.State().StreamsActive != 1 {
		t.Fatal("policy action stream was not held by unread HTTP/2 flow control")
	}
	if witness, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: command.CommandId}, tokens.AccessToken)); err != nil || witness.Msg.Durable == nil {
		t.Fatal("blocked action stalled the independent authorized reader", err)
	}
	grantPolicy(slowAccount.Msg.Account.Id, true, false, false)
	until = time.Now().Add(3 * time.Second)
	for blockedAction.State().StreamsActive != 0 {
		if time.Now().After(until) {
			t.Fatal("policy withdrawal retained blocked action Send")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if readableAction.Receive() || connect.CodeOf(readableAction.Err()) != connect.CodePermissionDenied {
		t.Fatal("idle action read survived policy withdrawal", readableAction.Err())
	}
	readableAction.Close()
	if _, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: readerCommand.CommandId}, slowTokens.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("withdrawn action read admitted", err)
	}
	refusedWatch, err := action.WatchAction(ctx, nha141BearerRequest(&actions.WatchActionRequest{CommandId: readerCommand.CommandId}, slowTokens.AccessToken))
	if err == nil {
		if refusedWatch.Receive() {
			t.Fatal("withdrawn action watch emitted a point")
		}
		err = refusedWatch.Err()
		refusedWatch.Close()
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("withdrawn action watch admitted", err)
	}
	refusedWrite := proto.Clone(readerCommand).(*actions.CommandRequest)
	refusedWrite.CommandId += "-write-denied"
	if _, err := action.SubmitAction(ctx, nha141BearerRequest(&actions.SubmitActionRequest{Command: refusedWrite}, slowTokens.AccessToken)); connect.CodeOf(err) != connect.CodePermissionDenied || driver.emitted.Load() != 4 {
		t.Fatal("withdrawn action write reached driver", err)
	}
	if sessions, err := authv1connect.NewAuthServiceClient(client, base).ListSessions(ctx, nha141BearerRequest(&authv1.ListSessionsRequest{AccountId: slowAccount.Msg.Account.Id}, slowTokens.AccessToken)); err != nil || len(sessions.Msg.Sessions) == 0 {
		t.Fatal("action policy withdrawal revoked credential", err)
	}
	if response, err := state.OpenView(ctx, nha141BearerRequest(&statev1.OpenViewRequest{PageSize: 1, Filter: &statev1.ViewFilter{}}, slowTokens.AccessToken)); err != nil || len(response.Msg.Entities) != 1 {
		t.Fatal("action policy withdrawal removed remaining state read", err)
	}
	if witness := open(1); len(witness.Entities) != 1 {
		t.Fatal("action policy withdrawal blocked the independent fast account")
	}
	if witness, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: command.CommandId}, tokens.AccessToken)); err != nil || witness.Msg.Durable == nil {
		t.Fatal("action policy withdrawal broke the independent action reader", err)
	}
	blockedCredential, _ := nha147UnreadH2(t, ctx, base, roots, actionsv1connect.ActionServiceWatchActionProcedure, &actions.WatchActionRequest{CommandId: secondCommand.CommandId}, tokens.AccessToken, 1)
	if blockedCredential.State().StreamsActive != 1 {
		t.Fatal("credential revocation lacked an active blocked Send")
	}
	live, err := action.WatchAction(ctx, nha141BearerRequest(&actions.WatchActionRequest{CommandId: secondCommand.CommandId}, tokens.AccessToken))
	if err != nil || !live.Receive() || live.Msg().GetProgress() == nil {
		t.Fatal("live progression did not start", err)
	}
	if _, err := authv1connect.NewAuthServiceClient(client, base).RevokeSession(ctx, nha141BearerRequest(&authv1.RevokeSessionRequest{SessionId: tokens.Session.Id}, tokens.AccessToken)); err != nil {
		t.Fatal(err)
	}
	until = time.Now().Add(3 * time.Second)
	for blockedCredential.State().StreamsActive != 0 {
		if time.Now().After(until) {
			t.Fatal("credential revocation retained unread blocked action Send")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if live.Receive() {
		t.Fatal("progression emitted after revocation")
	}
	var ended *connect.Error
	if !errors.As(live.Err(), &ended) || ended.Code() != connect.CodeUnauthenticated {
		t.Fatal("revoked stream lost termination", live.Err())
	}
	foundTermination := false
	for _, detail := range ended.Details() {
		value, err := detail.Value()
		if err == nil {
			if reason, ok := value.(*authv1.AuthorizationTermination); ok && reason.Reason == authv1.AuthorizationTerminationReason_AUTHORIZATION_TERMINATION_REASON_CREDENTIAL_REVOKED {
				foundTermination = true
			}
		}
	}
	if !foundTermination {
		t.Fatal("revoked progression lost detail141")
	}
	live.Close()
	stop()
	start(true)
	tokens = login()
	state, action = statev1connect.NewStateServiceClient(client, base), actionsv1connect.NewActionServiceClient(client, base)
	stored, err = action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: command.CommandId}, tokens.AccessToken))
	if err != nil || stored.Msg.Durable == nil || stored.Msg.GetUnavailable() == nil || stored.Msg.Durable.Response.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
		t.Fatal("restart conflated durable result and missing live tracker", stored, err)
	}
	if _, err := submit(command); err != nil || driver.emitted.Load() != 4 {
		t.Fatal("restart replay emitted an effect", err)
	}
	recovered, err := action.GetAction(ctx, nha141BearerRequest(&actions.GetActionRequest{CommandId: unknownResult.CommandId}, tokens.AccessToken))
	if err != nil || recovered.Msg.GetUnavailable() == nil || recovered.Msg.Durable.State != actions.DurableActionState_DURABLE_ACTION_STATE_INDETERMINATE || recovered.Msg.Durable.Response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE {
		t.Fatal("restart erased an uncertain effect", recovered, err)
	}
	if _, err := submit(unknownResult); nha147FailureKind(t, err) != actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_OUTCOME_INDETERMINATE || driver.emitted.Load() != 4 {
		t.Fatal("recovered uncertainty emitted again", err)
	}
	missing, err := action.WatchAction(ctx, nha141BearerRequest(&actions.WatchActionRequest{CommandId: command.CommandId}, tokens.AccessToken))
	if err != nil || !missing.Receive() || missing.Msg().GetUnavailable() == nil || missing.Receive() || missing.Err() != nil {
		t.Fatal("missing live tracker was erased/treated as generic EOF", err)
	}
	missing.Close()
	stop()
	// The non-factory Run must publish the same StateService without configuring
	// a job, script, automation or external hardware pilot.
	start(false)
	tokens = login()
	state = statev1connect.NewStateServiceClient(client, base)
	if response := open(1); response.Cursor == nil {
		t.Fatal("default no-job Run omitted common RPCs")
	}
	if driver.emitted.Load() != 4 {
		t.Fatal("read/reconnect/default startup replayed a command")
	}
}

// NewClientConn uses the pinned x/net client implementation. ConfigureTransports
// carries net/http.HTTP2Config into its actual SETTINGS_INITIAL_WINDOW_SIZE.
// A one-byte window retains the first Connect envelope in Send without a
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
		t.Fatal("blocked action connection did not negotiate HTTP/2")
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
	envelope := make([]byte, 5+len(encoded))
	binary.BigEndian.PutUint32(envelope[1:5], uint32(len(encoded)))
	copy(envelope[5:], encoded)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+procedure, bytes.NewReader(envelope))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/connect+proto")
	request.Header.Set("Connect-Protocol-Version", "1")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := connection.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK || response.ProtoMajor != 2 {
		t.Fatal("blocked action did not reach real HTTP/2 Send", response.StatusCode, response.Proto)
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

func nha147FailureKind(t *testing.T, err error) actions.ActionOperationFailureKind {
	t.Helper()
	var failure *connect.Error
	if !errors.As(err, &failure) {
		t.Fatal("missing Connect failure", err)
	}
	for _, detail := range failure.Details() {
		message, decodeErr := detail.Value()
		if decodeErr != nil {
			continue
		}
		if typed, ok := message.(*actions.ActionOperationFailure); ok {
			if contracts.AdmitActionOperationFailure(typed) != nil {
				t.Fatal("invalid machine failure detail", typed)
			}
			if typed.Kind == actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_OUTCOME_INDETERMINATE && (typed.Response == nil || typed.Response.Status != actions.ActionStatus_ACTION_STATUS_INDETERMINATE) {
				t.Fatal("response+error lost the response", typed)
			}
			return typed.Kind
		}
	}
	t.Fatal("missing typed action failure", err)
	return actions.ActionOperationFailureKind_ACTION_OPERATION_FAILURE_KIND_UNSPECIFIED
}
