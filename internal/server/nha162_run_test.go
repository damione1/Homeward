package server

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	listsv1 "github.com/damione1/Homeward/gen/home/lists/v1"
	"github.com/damione1/Homeward/gen/home/lists/v1/listsv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/emptypb"
)

type nha162ObservedRun struct {
	lists    *listComposition
	sessions *sessionCoordinator
}

type nha162Running struct {
	observed nha162ObservedRun
	address  net.Addr
	stop     context.CancelFunc
	done     chan error
	joined   bool
}

// This uses the same default assembly as Run: no factory, pilot, binding,
// external source or device action. The observer only retains actual owners.
func nha162StartDefaultRun(t *testing.T, settings config.Config) *nha162Running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &nha162Running{stop: cancel, done: make(chan error, 1)}
	ready := make(chan net.Addr, 1)
	observed := make(chan nha162ObservedRun, 1)
	dependencies := runDependencies{observeLists: func(lists *listComposition, sessions *sessionCoordinator) {
		observed <- nha162ObservedRun{lists, sessions}
	}}
	go func() {
		run.done <- runWithDependencies(ctx, settings, (&net.ListenConfig{}).Listen, func(address net.Addr) { ready <- address }, nil, dependencies)
	}()
	t.Cleanup(func() {
		if !run.joined {
			if err := run.join(t); err != nil {
				t.Error("default Run cleanup:", err)
			}
		}
	})
	select {
	case run.address = <-ready:
	case err := <-run.done:
		run.joined = true
		t.Fatal("default Run exited before ready:", err)
	case <-time.After(12 * time.Second):
		t.Fatal("default Run did not become ready")
	}
	select {
	case run.observed = <-observed:
	default:
		t.Fatal("Run became ready without its list composition")
	}
	return run
}

func (r *nha162Running) join(t *testing.T) error {
	t.Helper()
	r.stop()
	select {
	case err := <-r.done:
		r.joined = true
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return from its shutdown budget")
		return nil
	}
}

func nha162RunClient(t *testing.T, roots *x509.CertPool, address net.Addr) *http.Client {
	t.Helper()
	client := nha181Client(roots)
	// A Watch must outlive the server's shutdown budget in the retention test.
	client.Timeout = 60 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func nha162RunLogin(t *testing.T, ctx context.Context, auth authv1connect.AuthServiceClient, name, password string) *authv1.SessionTokens {
	t.Helper()
	response, err := auth.StartLogin(ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: name, Password: password}))
	if err != nil || response == nil || response.Msg.Decision.GetAuthenticated() == nil {
		t.Fatal("Run TLS native login:", err)
	}
	return response.Msg.Decision.GetAuthenticated()
}

func nha162RunBootstrap(t *testing.T, ctx context.Context, dataDir, name, password string) *authv1.AccountId {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dataDir, operatorSocketName))
	}}
	t.Cleanup(transport.CloseIdleConnections)
	operator := authv1connect.NewOperatorAccountServiceClient(&http.Client{Transport: transport, Timeout: 10 * time.Second}, "http://operator")
	response, err := operator.BootstrapAdmin(ctx, connect.NewRequest(&authv1.BootstrapAdminRequest{LoginName: name, DisplayName: name, Password: password}))
	if err != nil || response == nil || response.Msg.Account == nil || response.Msg.Account.Id == nil {
		t.Fatal("Run Unix bootstrap:", err)
	}
	return response.Msg.Account.Id
}

func nha162RunPolicy(t *testing.T, ctx context.Context, client authv1connect.AuthorizationPolicyServiceClient, account *authv1.AccountId, administrator, grants string) {
	t.Helper()
	current, err := client.GetPolicy(ctx, nha162Bearer(&authv1.GetPolicyRequest{AccountId: account}, administrator))
	if err != nil || current == nil || current.Msg.GetPresent() == nil {
		t.Fatal("Run native GetPolicy:", err)
	}
	source := []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n%s", account.Value, grants))
	_, err = client.ReplacePolicy(ctx, nha162Bearer(&authv1.ReplacePolicyRequest{
		AccountId: account, YamlSource: source,
		Precondition: &authv1.ReplacePolicyRequest_Expected{Expected: current.Msg.GetPresent().Version},
	}, administrator))
	if err != nil {
		t.Fatal("Run native ReplacePolicy:", err)
	}
}

func nha162RunStoreProbe(store *storage.Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return store.InTransaction(ctx, func(tx pgx.Tx) error {
		var one int
		return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
	})
}

func nha162RunClosed(t *testing.T, observed nha162ObservedRun) {
	t.Helper()
	select {
	case <-observed.lists.Done():
	default:
		t.Fatal("Run returned before its list owner drained")
	}
	if err := nha162RunStoreProbe(observed.lists.store); err == nil {
		t.Fatal("Run kept PostgreSQL open after every owner drained")
	}
}

func TestNHA162DefaultRunTLSCRUDWatchAndRestart(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha181Config(t, cluster.Port, dataDir, "127.0.0.1:0", nha181TLSYAML())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	run := nha162StartDefaultRun(t, settings)
	client := nha162RunClient(t, roots, run.address)
	url := "https://" + run.address.String()
	auth := authv1connect.NewAuthServiceClient(client, url)
	accounts := authv1connect.NewAccountServiceClient(client, url)
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, url)
	api := listsv1connect.NewListServiceClient(client, url)
	const adminName, adminPassword = "NHA162 Run Admin", "NHA162 synthetic Run admin password"
	const userName, userPassword = "NHA162 Run User", "NHA162 synthetic Run user password"
	adminID := nha162RunBootstrap(t, ctx, dataDir, adminName, adminPassword)
	admin := nha162RunLogin(t, ctx, auth, adminName, adminPassword).AccessToken
	_, err := api.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "No admin bypass"}, admin))
	nha162Code(t, err, connect.CodePermissionDenied)
	nha162RunPolicy(t, ctx, policies, adminID, admin, nha162All)
	createdUser, err := accounts.CreateAccount(ctx, nha162Bearer(&authv1.CreateAccountRequest{
		LoginName: userName, DisplayName: userName, Role: authv1.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: userPassword,
	}, admin))
	if err != nil {
		t.Fatal(err)
	}
	nha162RunPolicy(t, ctx, policies, createdUser.Msg.Account.Id, admin, nha162All)
	user := nha162RunLogin(t, ctx, auth, userName, userPassword).AccessToken
	created, err := api.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Run durable list"}, admin))
	if err != nil {
		t.Fatal("Run CreateList:", err)
	}
	id := created.Msg.Receipt.ListId
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	watch, err := api.WatchLists(watchCtx, nha162Bearer(&listsv1.WatchListsRequest{Scope: &listsv1.WatchListsRequest_Catalog{Catalog: &emptypb.Empty{}}}, user))
	if err != nil {
		t.Fatal("Run WatchLists:", err)
	}
	defer watch.Close()
	if !watch.Receive() || watch.Msg().GetReady() == nil {
		t.Fatal("Run Watch did not establish its subscription:", watch.Err())
	}
	page, err := api.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1}, user))
	if err != nil || len(page.Msg.Lists) != 1 || page.Msg.Lists[0].Id.Value != id.Value {
		t.Fatal("Run page after ready:", err)
	}
	renamed, err := api.RenameList(ctx, nha162Bearer(&listsv1.RenameListRequest{ListId: id, ExpectedRevision: created.Msg.Receipt.ListRevision, Name: "Run renamed list"}, user))
	if err != nil {
		t.Fatal("Run user RenameList:", err)
	}
	if !watch.Receive() || watch.Msg().GetChanged() == nil || watch.Msg().GetChanged().ListId.Value != id.Value || watch.Msg().GetChanged().Revision.Number != renamed.Msg.Receipt.ListRevision.Number {
		t.Fatal("Run Watch missed the post-ready mutation:", watch.Err())
	}
	completed := true
	added, err := api.AddItem(ctx, nha162Bearer(&listsv1.AddItemRequest{
		ListId: id, ExpectedListRevision: renamed.Msg.Receipt.ListRevision,
		Item: &listsv1.ItemPayload{Summary: "Run persisted item", Completed: &completed},
	}, user))
	if err != nil {
		t.Fatal("Run AddItem:", err)
	}
	completed = false
	unchecked, err := api.SetCompleted(ctx, nha162Bearer(&listsv1.SetCompletedRequest{
		ListId: id, ItemUid: added.Msg.Receipt.ItemUid, ExpectedListRevision: added.Msg.Receipt.ListRevision,
		ExpectedItemRevision: added.Msg.Receipt.ItemRevision, Completed: &completed,
	}, user))
	if err != nil {
		t.Fatal("Run SetCompleted(false):", err)
	}
	// Leave this real stream unread while Run cancels and drains the handler.
	if err := run.join(t); err != nil {
		t.Fatal("Run shutdown with Watch:", err)
	}
	nha162RunClosed(t, run.observed)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for watch.Receive() {
		}
	}()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("Run left its public Watch stream alive")
	}

	restarted := nha162StartDefaultRun(t, settings)
	restartedHTTP := nha162RunClient(t, roots, restarted.address)
	restartedURL := "https://" + restarted.address.String()
	auth = authv1connect.NewAuthServiceClient(restartedHTTP, restartedURL)
	api = listsv1connect.NewListServiceClient(restartedHTTP, restartedURL)
	user = nha162RunLogin(t, ctx, auth, userName, userPassword).AccessToken
	list, err := api.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: id}, user))
	if err != nil || list.Msg.List.Name != "Run renamed list" || list.Msg.List.Revision.Number != unchecked.Msg.Receipt.ListRevision.Number {
		t.Fatal("Run restart lost committed list identity/content/revision:", err)
	}
	items, err := api.ListItems(ctx, nha162Bearer(&listsv1.ListItemsRequest{ListId: id, PageSize: 1}, user))
	if err != nil || len(items.Msg.Items) != 1 || items.Msg.Items[0].Uid.Value != added.Msg.Receipt.ItemUid.Value || items.Msg.Items[0].Value.Summary != "Run persisted item" || items.Msg.Items[0].Value.Completed == nil || *items.Msg.Items[0].Value.Completed || items.Msg.Items[0].Revision.Number != unchecked.Msg.Receipt.ItemRevision.Number {
		t.Fatal("Run restart lost committed item identity/content/false/revision:", err)
	}
	if err := restarted.join(t); err != nil {
		t.Fatal("restarted Run shutdown:", err)
	}
	nha162RunClosed(t, restarted.observed)
}

func TestNHA162RunCompositionFailuresCloseListOwner(t *testing.T) {
	for _, stage := range []string{"cancel-after-create", "route-register", "route-compile", "listener"} {
		t.Run(stage, func(t *testing.T) {
			cluster := testpg.Start(t)
			dataDir := shortPrivateDataDir(t)
			nha181CertificateFixture(t, dataDir)
			settings := nha181Config(t, cluster.Port, dataDir, "127.0.0.1:0", nha181TLSYAML())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			listenerFailure := errors.New("NHA162 synthetic listener failure")
			var observed nha162ObservedRun
			dependencies := runDependencies{observeLists: func(lists *listComposition, sessions *sessionCoordinator) {
				observed = nha162ObservedRun{lists, sessions}
				if stage == "cancel-after-create" {
					cancel()
				}
			}}
			factory := func(_ context.Context, store *storage.Store) (RuntimePlan, error) {
				plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: store})
				if err != nil {
					return RuntimePlan{}, err
				}
				switch stage {
				case "route-register":
					err = plan.RegisterConnect(func(options ...connect.HandlerOption) (string, http.Handler) {
						return listsv1connect.NewListServiceHandler(&listHandler{}, options...)
					}, ConnectProcedure{Path: listsv1connect.ListServiceCreateListProcedure, Access: RouteAccessProtected, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
				case "route-compile":
					err = plan.RegisterConnect(func(...connect.HandlerOption) (string, http.Handler) {
						return "invalid-service-prefix", http.NotFoundHandler()
					}, ConnectProcedure{Path: "/nha162.fixture/Failure", Access: RouteAccessAnonymous, Flow: RouteFlowUnary, Transport: RouteTransportTLSRequired})
				}
				return RuntimePlan{Identity: unconfiguredIdentity{}, Public: plan}, err
			}
			ready, listened := false, false
			listen := func(context.Context, string, string) (net.Listener, error) {
				listened = true
				return nil, listenerFailure
			}
			err := runWithDependencies(ctx, settings, listen, func(net.Addr) { ready = true }, factory, dependencies)
			want := listenerFailure
			switch stage {
			case "cancel-after-create":
				want = context.Canceled
			case "route-register":
				want = ErrPublicRouteConflict
			case "route-compile":
				want = ErrInvalidPublicPlan
			}
			if !errors.Is(err, want) || ready || observed.lists == nil || listened != (stage == "listener") {
				t.Fatalf("Run failure boundary: err=%v ready=%t observed=%t listened=%t", err, ready, observed.lists != nil, listened)
			}
			nha162RunClosed(t, observed)
		})
	}
}

// Done is first consulted by execute's context.WithCancel after Service.enter.
// It retains the parent's exact cancellation semantics and only signals entry.
type nha162RunEntryContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *nha162RunEntryContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestNHA162RunRetainsStoreThroughListAuthorityBarrier(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha181Config(t, cluster.Port, dataDir, "127.0.0.1:0", nha181TLSYAML())
	run := nha162StartDefaultRun(t, settings)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := nha162RunClient(t, roots, run.address)
	url := "https://" + run.address.String()
	auth := authv1connect.NewAuthServiceClient(client, url)
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, url)
	api := listsv1connect.NewListServiceClient(client, url)
	const name, password = "NHA162 Barrier Admin", "NHA162 synthetic barrier admin password"
	accountID := nha162RunBootstrap(t, ctx, dataDir, name, password)
	tokens := nha162RunLogin(t, ctx, auth, name, password)
	nha162RunPolicy(t, ctx, policies, accountID, tokens.AccessToken, nha162All)
	created, err := api.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Run authority barrier"}, tokens.AccessToken))
	if err != nil {
		t.Fatal(err)
	}
	account, err := contracts.ParseAccountID(accountID.Value)
	if err != nil {
		t.Fatal(err)
	}
	session, err := contracts.ParseSessionID(tokens.Session.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := authorization.NativePrincipal(account, session)
	if err != nil {
		t.Fatal(err)
	}
	principalCtx, err := authorization.WithPrincipal(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	id, err := domain.ParseListID(created.Msg.Receipt.ListId.Value)
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := run.observed.sessions.authority.BeginMutation(account)
	if err != nil {
		t.Fatal(err)
	}
	defer mutation.Release()
	entered := &nha162RunEntryContext{Context: principalCtx, entered: make(chan struct{})}
	callDone := make(chan error, 1)
	// This is the actual Run-owned domain service. The preceding RPC establishes
	// its native-session/Authority/Store wiring before testing its inherited gate.
	go func() { _, err := run.observed.lists.service.GetList(entered, id); callDone <- err }()
	select {
	case <-entered.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Run-owned list call did not enter the service")
	}
	select {
	case err := <-callDone:
		t.Fatal("list call passed the held Authority barrier:", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := run.join(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Run did not report its undrained list owner:", err)
	}
	select {
	case <-run.observed.lists.Done():
		t.Fatal("Run declared its list owner drained while Authority was held")
	default:
	}
	if err := nha162RunStoreProbe(run.observed.lists.store); err != nil {
		t.Fatal("Run closed PostgreSQL under its list owner:", err)
	}
	mutation.Release()
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("shutdown-canceled Authority waiter reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Authority waiter did not join after release")
	}
	select {
	case <-run.observed.lists.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Run-owned lists did not finish after Authority release")
	}
	deadline := time.Now().Add(3 * time.Second)
	for nha162RunStoreProbe(run.observed.lists.store) == nil {
		if time.Now().After(deadline) {
			t.Fatal("retained PostgreSQL did not close after the true list Done")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
