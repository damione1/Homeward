package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/damione1/Homeward/internal/apierrors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/operator/v1/operatorv1connect"
	registrywire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/damione1/Homeward/internal/view"
	"github.com/damione1/Homeward/scope"
	"github.com/jackc/pgx/v5"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Fixture delegates every public EntityView RPC to the production handler.
type nha050StateHandler struct {
	*homeResourceHandler
}

type nha050ViewConfig struct{ stamp view.ConfigStamp }

func (c nha050ViewConfig) CurrentConfig(context.Context) (view.ConfigStamp, error) {
	return c.stamp, nil
}
func (nha050ViewConfig) SubscribeConfigChanges(context.Context) (<-chan struct{}, func(), error) {
	return make(chan struct{}), func() {}, nil
}

type nha050RunFixture struct {
	store    *storage.Store
	identity *nha044Identity
	host     *integrations.Manager
	state    *nha050StateHandler
	bindings []integrations.Binding
	session  *integrations.Session
	entities []domain.EntityID
	commands *integrations.VolatileCommandService
	driver   *nha050RunDriver
}

type nha050RunDriver struct{ emitted atomic.Int32 }

func (d *nha050RunDriver) Dispatch(_ context.Context, command integrations.AdmittedCommand) error {
	if !command.ReadyForTransport() {
		return integrations.ErrCommandAuthorizationDenied
	}
	d.emitted.Add(1)
	return nil
}

func TestNHA050RunNativePolicyCASViewIdleRevocationAndRestart(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha049Config(t, cluster.Port, dataDir)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	instance, _ := domain.ParseIntegrationInstanceID("nha050-run-input")
	firstKey, _ := integrations.NewSourceKey("first", "relay")
	secondKey, _ := integrations.NewSourceKey("second", "relay")
	keys := []integrations.SourceKey{firstKey, secondKey}
	aliases := []string{"switch.basement_relay", "switch.living_room_relay"}
	// Pick two declared switch aliases from the pinned manifest rather than
	// admitting fixture-only registry names through a production parser.
	var err error
	fixture := new(nha050RunFixture)
	factory := func(factoryCtx context.Context, store *storage.Store) (RuntimePlan, error) {
		manifest, err := registry.ParseManifest(scope.ManifestV1())
		if err != nil {
			return RuntimePlan{}, err
		}
		repository, err := registry.NewRepository(store, manifest)
		if err != nil {
			return RuntimePlan{}, err
		}
		fixture.store = store
		fixture.identity = &nha044Identity{repo: repository}
		fixture.entities = nil
		for i, key := range keys {
			resolved, err := repository.ConfigureBinding(factoryCtx, instance, key, aliases[i])
			if err != nil {
				return RuntimePlan{}, err
			}
			fixture.entities = append(fixture.entities, resolved.Entity)
		}
		fixture.state = &nha050StateHandler{}
		plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: store})
		if err != nil {
			return RuntimePlan{}, err
		}
		err = plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
			return apiv2connect.NewEntityViewServiceHandler(fixture.state, options...)
		},
			ConnectProcedure{Path: apiv2connect.EntityViewServiceCreateEntityViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
			ConnectProcedure{Path: apiv2connect.EntityViewServiceDeleteEntityViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
			ConnectProcedure{Path: apiv2connect.EntityViewServiceQueryEntityViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired},
			ConnectProcedure{Path: apiv2connect.EntityViewServiceWatchEntityViewProcedure, Access: RouteAccessProtected, Flow: RouteFlowServerStream, Transport: RouteTransportTLSRequired})
		if err != nil {
			return RuntimePlan{}, err
		}
		// This finite fixture route exercises the existing command service with
		// Run's native principal and Host. No product command façade is added.
		err = plan.Register(RouteKindMedia, RouteMethodPost, "/nha050/command", RouteAccessProtected, RouteFlowUnary, RouteTransportTLSRequired,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				actor, ok := ActorFromContext(r.Context())
				if !ok {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
				request := new(actions.CommandRequest)
				if err != nil || proto.Unmarshal(body, request) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				response, err := fixture.commands.Dispatch(r.Context(), actor, request)
				if errors.Is(err, integrations.ErrCommandAuthorizationDenied) {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if err != nil || response == nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				data, err := proto.Marshal(response)
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/protobuf")
				_, _ = w.Write(data)
			}))
		if err != nil {
			return RuntimePlan{}, err
		}
		permission, err := authorization.NewPermission(authv1.ScopeFamily_SCOPE_FAMILY_ACTIONS, authv1.ScopeOperation_SCOPE_OPERATION_WRITE)
		if err != nil {
			return RuntimePlan{}, err
		}
		if err := plan.BindPermission(RouteKindMedia, RouteMethodPost, "/nha050/command", permission); err != nil {
			return RuntimePlan{}, err
		}
		return RuntimePlan{Identity: fixture.identity, Public: plan, Configure: func(_ context.Context, host *integrations.Manager, _ *storage.Store) error {
			fixture.host = host
			return nil
		}, ConfigureAuthorization: func(_ context.Context, composition AuthorizationComposition) error {
			stamp, err := view.NewConfigStamp(7, sha256.Sum256([]byte("nha050-run-config")))
			if err != nil {
				return err
			}
			native := composition.ViewAuthorizer().(*policyViewAuthorizer)
			core := &coreRPCHandler{epoch: native.source}
			views, err := composition.NewViewService(nha050ViewConfig{stamp}, sequencer.RealClock{}, core.capabilityProjector(repository, native.authority))
			if err != nil {
				return err
			}
			fixture.state.homeResourceHandler, err = newHomeResourceHandler(repository, native.authority, views)
			if err == nil {
				fixture.commands, err = integrations.NewVolatileCommandService(fixture.host, nha094Generation{}, integrations.DefaultVolatileCommandLimits())
			}
			return err
		}}, nil
	}
	var stop func()
	var address net.Addr
	start := func() {
		t.Helper()
		runCtx, runCancel := context.WithCancel(ctx)
		ready := make(chan net.Addr, 1)
		done := make(chan error, 1)
		var listener net.ListenConfig
		go func() {
			done <- RunWithStartupFactory(runCtx, settings.startup, listener.Listen, func(addr net.Addr) { ready <- addr }, factory)
		}()
		select {
		case address = <-ready:
		case err := <-done:
			t.Fatalf("Run before ready: %v", err)
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
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("Run did not drain")
				}
			})
		}
		t.Cleanup(stop)
		var err error
		fixture.session, err = fixture.host.OpenSession(instance)
		if err != nil {
			t.Fatal(err)
		}
		candidates := make([]integrations.Candidate, 0, 2)
		for _, key := range keys {
			candidate, err := integrations.NewCandidate(key, []*registrywire.CapabilityDescription{{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "relay", DisplayName: "NHA050 synthetic relay", Constraints: &registrywire.CapabilityDescription_Switch{Switch: &registrywire.SwitchConstraints{SetOn: true}}}})
			if err != nil {
				t.Fatal(err)
			}
			candidates = append(candidates, candidate)
		}
		fixture.bindings, err = fixture.session.Discover(ctx, candidates)
		if err != nil {
			t.Fatal(err)
		}
		fixture.driver = new(nha050RunDriver)
		if err := fixture.host.AttachCommandDriver(fixture.session, fixture.driver); err != nil {
			t.Fatal(err)
		}
	}
	start()
	client := nha181Client(roots)
	client.Timeout = 10 * time.Second
	defer client.CloseIdleConnections()
	url := func() string { return "https://" + address.String() }
	operatorHTTP := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", filepath.Join(dataDir, operatorSocketName))
	}}}
	defer operatorHTTP.CloseIdleConnections()
	operator := operatorv1connect.NewOperatorAccountServiceClient(operatorHTTP, "http://operator")
	const password = "NHA050 synthetic native policy password"
	_, err = operator.BootstrapAdmin(ctx, connect.NewRequest(&authv1.BootstrapAdminRequest{LoginName: "NHA050 Admin", DisplayName: "NHA050 Admin", Password: password}))
	if err != nil {
		t.Fatal("first admin after zero-account hydration", err)
	}
	login := func(name string) *api.SessionTokens {
		t.Helper()
		response, err := apiv2connect.NewAuthServiceClient(client, url()).StartLogin(ctx, connect.NewRequest(&api.StartLoginRequest{LoginName: name, Password: password}))
		if err != nil || response.Msg.Decision.GetAuthenticated() == nil {
			t.Fatal("native login", err)
		}
		return response.Msg.Decision.GetAuthenticated()
	}
	adminTokens := login("NHA050 Admin")
	policyClient := func() apiv2connect.AuthorizationPolicyServiceClient {
		return apiv2connect.NewAuthorizationPolicyServiceClient(client, url())
	}
	accountsClient := func() apiv2connect.AccountServiceClient { return apiv2connect.NewAccountServiceClient(client, url()) }
	stateClient := func() apiv2connect.EntityViewServiceClient {
		return apiv2connect.NewEntityViewServiceClient(client, url())
	}
	dispatch := func(access, id string, wantStatus int, wantEmitted int32) {
		t.Helper()
		epoch, err := fixture.identity.Sequence().CurrentAdmissionEpoch()
		if err != nil {
			t.Fatal(err)
		}
		request := &actions.CommandRequest{CommandId: id, AdmissionEpoch: epoch, ConfigRevision: 7,
			Target:             &registrywire.EntityRef{Reference: &registrywire.EntityRef_HaAlias{HaAlias: aliases[0]}},
			CapabilitySchemaId: contracts.SwitchSchema, Endpoint: "relay", ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}}}
		data, err := proto.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url()+"/nha050/command", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		httpRequest.Header.Set("Authorization", "Bearer "+access)
		httpRequest.Header.Set("Content-Type", "application/protobuf")
		response, err := client.Do(httpRequest)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != wantStatus {
			t.Fatalf("native Run command status %d, want %d: %v", response.StatusCode, wantStatus, err)
		}
		if wantStatus == http.StatusOK {
			result := new(actions.CommandResponse)
			if err := proto.Unmarshal(body, result); err != nil || result.Status != actions.ActionStatus_ACTION_STATUS_DISPATCHED {
				t.Fatal("native Run router did not dispatch", result, err)
			}
		}
		if got := fixture.driver.emitted.Load(); got != wantEmitted {
			t.Fatalf("native Run driver emitted %d commands, want %d", got, wantEmitted)
		}
	}
	user, err := accountsClient().CreateAccount(ctx, nha141BearerRequest(&api.CreateAccountRequest{Parent: nha203SessionAccount(t, adminTokens.Session).Home().String(), Account: &api.Account{LoginName: "NHA050 User", DisplayName: "NHA050 User", Role: api.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password}}, adminTokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	userTokens := login("NHA050 User")
	getPolicy := func() *api.AuthorizationPolicy {
		t.Helper()
		response, err := policyClient().GetAuthorizationPolicy(ctx, nha141BearerRequest(&api.GetAuthorizationPolicyRequest{Name: user.Msg.Name + "/authorizationPolicy"}, adminTokens.AccessToken))
		if err != nil || response == nil {
			t.Fatal("policy read", err)
		}
		return response.Msg
	}
	seed := getPolicy()
	storedRevision := func() int64 {
		t.Helper()
		var revision int64
		if err := fixture.store.InTransaction(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT d.last_revision FROM public.config_documents d
				JOIN public.authorization_policy_active p ON p.document_id=d.id WHERE p.account_id=$1`, nha203AccountText(t, nha203NativeAccount(t, ctx, fixture.store, user.Msg.Name))).Scan(&revision)
		}); err != nil {
			t.Fatal(err)
		}
		return revision
	}
	beforeRejected := storedRevision()
	for _, rejected := range []struct {
		name   string
		mutate func(*api.ReplaceAuthorizationPolicyRequest)
		path   string
	}{
		{"empty-source", func(r *api.ReplaceAuthorizationPolicyRequest) { r.YamlSource = nil }, "yaml_source"},
		{"non-utf8-source", func(r *api.ReplaceAuthorizationPolicyRequest) { r.YamlSource = []byte{0xff} }, "yaml_source"},
		{"source-N-plus-one", func(r *api.ReplaceAuthorizationPolicyRequest) {
			r.YamlSource = bytes.Repeat([]byte{'x'}, authorization.MaxSourceBytes+1)
		}, "yaml_source"},
		{"absent-precondition", func(r *api.ReplaceAuthorizationPolicyRequest) { r.Precondition = nil }, "precondition"},
		{"empty-etag", func(r *api.ReplaceAuthorizationPolicyRequest) {
			r.Precondition = &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{}
		}, "expected_etag"},
		{"malformed-etag", func(r *api.ReplaceAuthorizationPolicyRequest) {
			r.Precondition = &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: "invalid"}
		}, "expected_etag"},
	} {
		t.Run(rejected.name, func(t *testing.T) {
			request := &api.ReplaceAuthorizationPolicyRequest{Name: seed.Name, YamlSource: bytes.Clone(seed.YamlSource), Precondition: &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: seed.Etag}}
			rejected.mutate(request)
			_, err := policyClient().ReplaceAuthorizationPolicy(ctx, nha141BearerRequest(request, adminTokens.AccessToken))
			var ce *connect.Error
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !errors.As(err, &ce) {
				t.Fatal("wrong rejected policy class", err)
			}
			found := false
			for _, detail := range ce.Details() {
				value, decodeErr := detail.Value()
				if diagnostic, ok := value.(*errdetails.BadRequest); decodeErr == nil && ok {
					for _, violation := range diagnostic.FieldViolations {
						if violation.Field == rejected.path {
							found = true
						}
					}
				}
			}
			if !found {
				t.Fatal("missing canonical field diagnostic", err)
			}
			if current := getPolicy(); !proto.Equal(current, seed) || storedRevision() != beforeRejected {
				t.Fatal("rejected source/CAS mutated policy")
			}
		})
	}

	_, err = policyClient().GetAuthorizationPolicy(ctx, nha141BearerRequest(&api.GetAuthorizationPolicyRequest{Name: nha203SessionAccount(t, adminTokens.Session).String() + "/authorizationPolicy"}, userTokens.AccessToken))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatal("user policy administration admitted", err)
	}
	captureNames := make(map[*api.QueryEntityViewResponse]string)
	open := func(access string) *api.QueryEntityViewResponse {
		t.Helper()
		created, err := stateClient().CreateEntityView(ctx, nha141BearerRequest(&api.CreateEntityViewRequest{Parent: nha203SessionAccount(t, adminTokens.Session).Home().String(), EntityView: &api.EntityView{}}, access))
		if err != nil {
			t.Fatal("create view", err)
		}
		response, err := stateClient().QueryEntityView(ctx, nha141BearerRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageSize: 1}, access))
		if err != nil {
			t.Fatal("query view", err)
		}
		first := response.Msg
		captureNames[first] = created.Msg.Name
		for token := first.NextPageToken; token != ""; {
			next, err := stateClient().QueryEntityView(ctx, nha141BearerRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageSize: 1, PageToken: token}, access))
			if err != nil {
				t.Fatal("finish capture", err)
			}
			token = next.Msg.NextPageToken
			first.ResumeToken = next.Msg.ResumeToken
		}
		return first
	}
	if initial := open(userTokens.AccessToken); len(initial.Entries) != 0 {
		t.Fatal("empty user seed exposed equipment")
	}
	if initial := open(adminTokens.AccessToken); len(initial.Entries) != 0 {
		t.Fatal("intrinsic admin rules exposed equipment")
	}
	dispatch(userTokens.AccessToken, "nha050-run-empty-user", http.StatusForbidden, 0)
	dispatch(adminTokens.AccessToken, "nha050-run-empty-admin", http.StatusForbidden, 0)
	account, _ := contracts.ParseAccountID(nha203AccountText(t, nha203NativeAccount(t, ctx, fixture.store, user.Msg.Name)))
	allSource := []byte("schema: homeward.authorization/1\naccount_id: \"" + nha203AccountText(t, nha203NativeAccount(t, ctx, fixture.store, user.Msg.Name)) + "\"\ngrants:\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n  - scope: {family: actions, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n  - scope: {family: actions, operation: write}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
	if _, err := authorization.AdmitPolicySource(account, allSource); err != nil {
		t.Fatal(err)
	}
	replace := func(etag string, source []byte) (*connect.Response[api.ReplaceAuthorizationPolicyResponse], error) {
		return policyClient().ReplaceAuthorizationPolicy(ctx, nha141BearerRequest(&api.ReplaceAuthorizationPolicyRequest{Name: user.Msg.Name + "/authorizationPolicy", YamlSource: source, Precondition: &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: etag}}, adminTokens.AccessToken))
	}
	changed, err := replace(seed.Etag, allSource)
	if err != nil || changed.Msg.RightsChange != api.PolicyRightsChange_POLICY_RIGHTS_CHANGE_CHANGED {
		t.Fatal("CAS policy", err)
	}
	if _, err := replace(seed.Etag, allSource); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatal("stale CAS admitted", err)
	}
	publish := func(on bool) {
		t.Helper()
		quality, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessFresh, domain.ProvenanceObserved)
		value := domain.NewSwitchValue(on)
		at := time.Now().UTC()
		observation, err := integrations.NewStateObservation("relay", quality, &value, &at, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.session.Publish(ctx, fixture.bindings[0], observation); err != nil {
			t.Fatal(err)
		}
	}
	publish(false)
	dispatch(userTokens.AccessToken, "nha050-run-permitted", http.StatusOK, 1)
	page := open(userTokens.AccessToken)
	if page.NextPageToken == "" {
		t.Fatal("real pagination not available")
	}
	watchDone := make(chan error, 1)
	frames := make(chan *api.WatchEntityViewResponse, 4)
	watch := func(page *api.QueryEntityViewResponse) {
		t.Helper()
		go func() {
			stream, err := stateClient().WatchEntityView(ctx, nha141BearerRequest(&api.WatchEntityViewRequest{Name: captureNames[page], ResumeToken: page.ResumeToken}, userTokens.AccessToken))
			if err == nil {
				for stream.Receive() {
					frames <- proto.Clone(stream.Msg()).(*api.WatchEntityViewResponse)
				}
				err = stream.Err()
			}
			watchDone <- err
		}()
		// A replayable observation confirms that this exact production Watch
		// has admitted the capture before the policy transaction starts.
		publish(false)
		select {
		case frame := <-frames:
			if frame.GetUpsert() == nil || frame.ResumeToken == "" {
				t.Fatal("watch barrier", frame)
			}
		case err := <-watchDone:
			t.Fatal("watch admission", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	watch(page)
	noOp, err := replace(changed.Msg.Policy.Etag, append(append([]byte(nil), allSource...), []byte("# same effective rights\n")...))
	if err != nil || noOp.Msg.RightsChange != api.PolicyRightsChange_POLICY_RIGHTS_CHANGE_UNCHANGED {
		t.Fatal("source-only edit revoked policy", err)
	}
	publish(true)
	select {
	case frame := <-frames:
		if frame.GetUpsert() == nil || frame.ResumeToken == "" {
			t.Fatal("no-op terminated active view", frame)
		}
	case err := <-watchDone:
		t.Fatal("no-op closed lease", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	empty, _ := authorization.EmptySource(account)
	emptyBytes, _ := empty.Bytes()
	withdrawn, err := replace(noOp.Msg.Policy.Etag, emptyBytes)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-watchDone:
		if connect.CodeOf(err) != connect.CodeFailedPrecondition && connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatal("idle stream not invalidated", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err = stateClient().QueryEntityView(ctx, nha141BearerRequest(&api.QueryEntityViewRequest{Name: captureNames[page], PageToken: page.NextPageToken}, userTokens.AccessToken))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("old page survived rights withdrawal", err)
	}
	if len(open(userTokens.AccessToken).Entries) != 0 {
		t.Fatal("new empty view exposed retired rights")
	}
	dispatch(userTokens.AccessToken, "nha050-run-withdrawn", http.StatusForbidden, 1)
	restored, err := replace(withdrawn.Msg.Policy.Etag, allSource)
	if err != nil {
		t.Fatal(err)
	}
	watch(open(userTokens.AccessToken))
	if err := fixture.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION public.nha050_unknown_policy_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic commit failure'; RETURN NEW; END $$; CREATE CONSTRAINT TRIGGER nha050_unknown_policy_commit AFTER UPDATE ON public.authorization_policy_active DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.nha050_unknown_policy_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = replace(restored.Msg.Policy.Etag, emptyBytes)
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatal("unknown commit reported success", err)
	}
	terminal := <-watchDone
	if connect.CodeOf(terminal) != connect.CodeUnavailable {
		t.Fatal("quarantined stream terminal", terminal)
	}
	var connectErr *connect.Error
	if !errors.As(terminal, &connectErr) {
		t.Fatal("missing typed terminal")
	}
	purge := false
	for _, detail := range connectErr.Details() {
		value, err := detail.Value()
		if err == nil {
			if ended, ok := value.(*errdetails.ErrorInfo); ok && ended.Domain == apierrors.Domain() && ended.Reason == "AUTHORITY_UNAVAILABLE" {
				purge = true
			}
		}
	}
	if !purge {
		t.Fatal("terminal lacks mandatory purge detail", terminal)
	}
	if err := fixture.store.InTransaction(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DROP TRIGGER nha050_unknown_policy_commit ON public.authorization_policy_active; DROP FUNCTION public.nha050_unknown_policy_commit()`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A later admission reconciles the settled PG policy rather than retrying
	// the unknown write or keeping a stale projection permanently quarantined.
	if len(open(userTokens.AccessToken).Entries) != 1 {
		t.Fatal("settled policy not reconciled")
	}
	beforeRestart := getPolicy()
	stop()
	client.CloseIdleConnections()
	start()
	if got := getPolicy(); got.Etag != beforeRestart.Etag || string(got.YamlSource) != string(beforeRestart.YamlSource) {
		t.Fatal("restart changed persisted policy")
	}
	if len(open(userTokens.AccessToken).Entries) != 1 {
		t.Fatal("Run restart did not hydrate policy for persistent native access")
	}
	publish(false)
	dispatch(userTokens.AccessToken, "nha050-run-after-restart", http.StatusOK, 1)
}
