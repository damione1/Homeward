package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/lists"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/tasknames"
)

func nha162ImportContext(t *testing.T, r *nha162RPCRig) context.Context {
	t.Helper()
	response, err := r.auth.StartLogin(r.ctx, connect.NewRequest(&wire.StartLoginRequest{
		LoginName: "NHA162 Admin", Password: "NHA162 synthetic admin password canary",
	}))
	if err != nil || response == nil || response.Msg.Decision.GetAuthenticated() == nil {
		t.Fatal("synthetic importer native login:", err)
	}
	tokens := response.Msg.Decision.GetAuthenticated()
	session := nha203NativeSession(t, r.ctx, r.store, tokens.Session.Name)
	principal, err := authorization.NativePrincipal(r.admin, session)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authorization.WithPrincipal(r.ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestNHA162ComposedImporterChecksMutableRegistryBeforeDomain(t *testing.T) {
	for _, stage := range []string{"unconfigured", "original", "renamed", "other-identity", "retired", "unavailable"} {
		t.Run(stage, func(t *testing.T) {
			r := nha162NewRPCRig(t)
			ctx := nha162ImportContext(t, r)
			alias, err := domain.ParseHAAlias("todo.brico")
			if err != nil {
				t.Fatal(err)
			}
			instance, err := domain.ParseIntegrationInstanceID("nha162-synthetic-import")
			if err != nil {
				t.Fatal(err)
			}
			key, err := integrations.NewSourceKey("synthetic-source", "list")
			if err != nil {
				t.Fatal(err)
			}
			var identity integrations.ResolvedIdentity
			if stage == "original" || stage == "renamed" || stage == "retired" || stage == "other-identity" {
				original := "todo.brico"
				if stage == "other-identity" {
					original = "todo.costco"
				}
				identity, err = r.lists.registry.ConfigureBinding(ctx, instance, key, original)
				if err != nil {
					t.Fatal(err)
				}
			}
			if stage == "renamed" {
				if err := r.lists.registry.Rename(ctx, identity.Entity, "todo.nha162_synthetic_renamed"); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "retired" {
				if err := r.lists.registry.Tombstone(ctx, identity.Entity); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "other-identity" {
				// The public registry API reserves manifest aliases. An adversarial
				// disposable SQL fixture models a historical alias on another ID.
				entity, err := identity.Entity.Value()
				if err != nil {
					t.Fatal(err)
				}
				if err := r.store.InTransaction(ctx, func(tx pgx.Tx) error {
					_, err := tx.Exec(ctx, `INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES ('todo.brico',$1,false)`, entity)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			payload, err := lists.NewLegacyListPayload("Synthetic composed import")
			if err != nil {
				t.Fatal(err)
			}
			if stage == "unavailable" {
				r.store.Close()
			}
			receipt, err := r.lists.ImportLegacyList(ctx, alias, payload)
			if stage == "unavailable" {
				if !errors.Is(err, registry.ErrStorage) || receipt.Outcome() != lists.ReceiptInvalid {
					t.Fatal("registry unavailability did not stop the importer before domain admission:", err)
				}
				return
			}
			if stage == "other-identity" || stage == "retired" {
				if !errors.Is(err, ErrListImportBinding) || receipt.Outcome() != lists.ReceiptInvalid {
					t.Fatal("conflicting registry identity admitted an import:", err)
				}
				count, err := r.tasks.ListTaskLists(r.ctx, nha162Bearer(&wire.ListTaskListsRequest{Parent: r.taskHome(t)}, r.adminToken))
				if err != nil || count.Msg.TotalSize != 0 {
					t.Fatal("rejected registry guard changed durable lists:", err)
				}
				return
			}
			if err != nil || receipt.Outcome() != lists.Committed {
				t.Fatal("manifest original rejected by composed importer:", err)
			}
			listText, err := receipt.ListID().Value()
			if err != nil {
				t.Fatal(err)
			}
			if stage != "unconfigured" {
				entityText, err := identity.Entity.Value()
				if err != nil || listText == entityText {
					t.Fatal("import confused registry entity identity and business list identity:", err)
				}
			}
			uid, err := lists.ParseItemUID("nha162-synthetic-composed-item")
			if err != nil {
				t.Fatal(err)
			}
			item, err := lists.NewItemPayload(lists.ItemInput{Summary: "Synthetic composed item", Due: lists.NoDue()})
			if err != nil {
				t.Fatal(err)
			}
			legacyItem, err := lists.NewLegacyItemPayload(uid, 0, item)
			if err != nil {
				t.Fatal(err)
			}
			expected, present := receipt.ListRevision()
			if !present {
				t.Fatal("committed import omitted its list revision")
			}
			if _, err := r.lists.ImportLegacyItem(ctx, receipt.ListID(), alias, legacyItem, expected); err != nil {
				t.Fatal("composed item importer rejected an unchanged original alias:", err)
			}
			home, err := r.lists.registry.HomeName(r.ctx)
			if err != nil {
				t.Fatal(err)
			}
			name, err := tasknames.List(home, receipt.ListID())
			if err != nil {
				t.Fatal(err)
			}
			page, err := r.tasks.ListTasks(r.ctx, nha162Bearer(&wire.ListTasksRequest{Parent: name.String(), PageSize: 1}, r.adminToken))
			if err != nil || len(page.Msg.Tasks) != 1 {
				t.Fatal("composed import did not persist through the common TaskService:", err)
			}
			imported, err := tasknames.ParseTask(page.Msg.Tasks[0].Name)
			if err != nil || imported.Parent() != name || imported.UID() != uid || page.Msg.Tasks[0].Summary != "Synthetic composed item" {
				t.Fatal("canonical task name/content lost imported native identity:", page.Msg.Tasks[0], err)
			}
		})
	}
}

type nha162WatchWriteGate struct {
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *nha162WatchWriteGate) open() { g.once.Do(func() { close(g.release) }) }

type nha162SlowWatchWriter struct {
	http.ResponseWriter
	ctx     context.Context
	gate    *nha162WatchWriteGate
	flushes int
}

func (w *nha162SlowWatchWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *nha162SlowWatchWriter) Flush()                      { _ = w.FlushError() }
func (w *nha162SlowWatchWriter) FlushError() error {
	w.flushes++
	if w.flushes == 2 {
		// Connect 1.21 flushes once per Send. The real handler is held after
		// ready and its first change, while the real subscription queue fills.
		close(w.gate.blocked)
		select {
		case <-w.gate.release:
		case <-w.ctx.Done():
			return w.ctx.Err()
		}
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func TestNHA162RealWatchRPCBlockedWriterResyncsWithoutBlockingOthers(t *testing.T) {
	r := nha162NewRPCRig(t)
	gate := &nha162WatchWriteGate{blocked: make(chan struct{}), release: make(chan struct{})}
	watchDone := make(chan struct{})
	compiledMux := r.server.Config.Handler
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != apiv2connect.TaskServiceWatchTaskListsProcedure {
			compiledMux.ServeHTTP(writer, request)
			return
		}
		defer close(watchDone)
		compiledMux.ServeHTTP(&nha162SlowWatchWriter{ResponseWriter: writer, ctx: request.Context(), gate: gate}, request)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	t.Cleanup(gate.open)
	client := server.Client()
	client.Timeout = 45 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	api := apiv2connect.NewTaskServiceClient(client, server.URL)
	ctx, cancel := context.WithTimeout(r.ctx, 40*time.Second)
	defer cancel()
	scope := func() *wire.WatchTaskListsRequest {
		return &wire.WatchTaskListsRequest{Parent: r.taskHome(t)}
	}
	slow, err := api.WatchTaskLists(ctx, nha162Bearer(scope(), r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	if !slow.Receive() || slow.Msg().GetReady() == nil {
		t.Fatal("blocked-writer Watch did not establish its subscription:", slow.Err())
	}
	page, err := r.tasks.ListTaskLists(ctx, nha162Bearer(&wire.ListTaskListsRequest{Parent: r.taskHome(t), PageSize: 1}, r.adminToken))
	if err != nil || len(page.Msg.TaskLists) != 0 {
		t.Fatal("blocked-writer reload after ready:", err)
	}
	fast, err := r.tasks.WatchTaskLists(ctx, nha162Bearer(scope(), r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	if !fast.Receive() || fast.Msg().GetReady() == nil {
		t.Fatal("fast Watch did not establish its subscription:", fast.Err())
	}
	first, err := r.tasks.CreateTaskList(ctx, nha162Bearer(&wire.CreateTaskListRequest{Parent: r.taskHome(t), TaskList: &wire.TaskList{DisplayName: "Synthetic blocked frame"}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if !fast.Receive() || fast.Msg().GetChanged() == nil || fast.Msg().GetChanged().TaskList != first.Msg.Name {
		t.Fatal("another Watch did not receive the first change:", fast.Err())
	}
	select {
	case <-gate.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("real Watch handler never reached its blocked response writer")
	}
	for i := 0; i <= lists.MaxNotificationQueue; i++ {
		created, err := r.tasks.CreateTaskList(ctx, nha162Bearer(&wire.CreateTaskListRequest{Parent: r.taskHome(t), TaskList: &wire.TaskList{DisplayName: "Synthetic queued invalidation"}}, r.adminToken))
		if err != nil {
			t.Fatal("slow Watch blocked a writer:", err)
		}
		if !fast.Receive() || fast.Msg().GetChanged() == nil || fast.Msg().GetChanged().TaskList != created.Msg.Name {
			t.Fatal("slow Watch blocked another Watch:", fast.Err())
		}
	}
	count, err := r.tasks.ListTaskLists(ctx, nha162Bearer(&wire.ListTaskListsRequest{Parent: r.taskHome(t)}, r.adminToken))
	if err != nil || count.Msg.TotalSize != int32(lists.MaxNotificationQueue+2) {
		t.Fatal("writes did not finish while the real Watch send was blocked:", err)
	}
	gate.open()
	if !slow.Receive() || slow.Msg().GetChanged() == nil || slow.Msg().GetChanged().TaskList != first.Msg.Name || slow.Msg().Sequence != 2 {
		t.Fatal("blocked first change did not finish after release:", slow.Err())
	}
	if !slow.Receive() || slow.Msg().GetResyncRequired() == nil || slow.Msg().GetChanged() != nil || slow.Msg().Sequence != 3 {
		t.Fatal("real overflow did not become an identity-free terminal resync:", slow.Err())
	}
	if slow.Receive() || slow.Err() != nil {
		t.Fatal("resync did not close its real RPC stream cleanly:", slow.Err())
	}
	select {
	case <-watchDone:
	case <-time.After(3 * time.Second):
		t.Fatal("resync left its handler/subscription running")
	}
}

func TestNHA162AmbiguousCommitWireErrorsAreRedacted(t *testing.T) {
	for _, variant := range []struct {
		domain lists.Code
		status connect.Code
		reason string
	}{
		{lists.CommitUnknown, connect.CodeUnknown, "COMMIT_OUTCOME_UNKNOWN"},
		{lists.CommittedResponseWithheld, connect.CodePermissionDenied, "ACCESS_DENIED"},
	} {
		err := listFault(variant.domain).Connect()
		if connect.CodeOf(err) != variant.status || nha200Info(t, err).Reason != variant.reason {
			t.Fatal(err)
		}
	}
}

func nha162WaitSQLBlockedBy(t *testing.T, ctx context.Context, store *storage.Store, blocker int, pattern string) int {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked int
		err := store.InTransaction(bounded, func(tx pgx.Tx) error {
			return tx.QueryRow(bounded, `SELECT pid FROM pg_stat_activity
				WHERE pid<>pg_backend_pid() AND wait_event_type='Lock'
				AND $1::integer=ANY(pg_blocking_pids(pid)) AND query LIKE $2
				ORDER BY pid LIMIT 1`, blocker, pattern).Scan(&blocked)
		})
		if err == nil {
			return blocked
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("could not observe the existing SQL fence:", err)
		}
		select {
		case <-tick.C:
		case <-bounded.Done():
			t.Fatal("SQL operation did not wait behind the expected owner:", bounded.Err())
		}
	}
}

func TestNHA162RealRPCKnownCommitWithholdsReceiptAfterPolicyRevocation(t *testing.T) {
	r := nha162NewRPCRig(t)
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	const password = "NHA162 synthetic withheld writer password"
	user, err := r.accounts.CreateAccount(ctx, nha162Bearer(&wire.CreateAccountRequest{Parent: r.taskHome(t), Account: &wire.Account{
		LoginName: "NHA162 Withheld Writer", DisplayName: "NHA162 Withheld Writer",
		Role: wire.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password,
	}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	account := nha203NativeAccount(t, ctx, r.store, user.Msg.Name)
	nha162SetPolicy(t, r, account, `  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`)
	userToken := r.login(t, "NHA162 Withheld Writer", password)
	created, err := r.tasks.CreateTaskList(ctx, nha162Bearer(&wire.CreateTaskListRequest{Parent: r.taskHome(t), TaskList: &wire.TaskList{DisplayName: "Before withheld response"}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	id, initial := created.Msg.Name, created.Msg.Etag
	parsed, err := tasknames.ParseList(id)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, err := parsed.ID().Value()
	if err != nil {
		t.Fatal(err)
	}
	var initialRevision int64
	if err = r.store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT revision FROM public.homeward_lists WHERE list_id=$1`, nativeID).Scan(&initialRevision)
	}); err != nil {
		t.Fatal(err)
	}
	policies := apiv2connect.NewAuthorizationPolicyServiceClient(r.server.Client(), r.server.URL)
	policy, err := policies.GetAuthorizationPolicy(ctx, nha162Bearer(&wire.GetAuthorizationPolicyRequest{Name: user.Msg.Name + "/authorizationPolicy"}, r.adminToken))
	if err != nil || policy == nil || policy.Msg == nil {
		t.Fatal("could not capture the real policy CAS:", err)
	}
	var renameCalls atomic.Int32
	mux := r.server.Config.Handler
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == apiv2connect.TaskServiceUpdateTaskListProcedure {
			renameCalls.Add(1)
		}
		mux.ServeHTTP(writer, request)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	httpClient := server.Client()
	httpClient.Timeout = 30 * time.Second
	t.Cleanup(httpClient.CloseIdleConnections)
	api := apiv2connect.NewTaskServiceClient(httpClient, server.URL)

	locked := make(chan int, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRow := func() { releaseOnce.Do(func() { close(release) }) }
	rowDone := make(chan struct{})
	rowResult := make(chan error, 1)
	renameDone, policyDone := make(chan struct{}), make(chan struct{})
	renameStarted, policyStarted := false, false
	// All three workers are joined on every exit, including a failed oracle.
	defer func() {
		releaseRow()
		cancel()
		owners := []<-chan struct{}{rowDone}
		if renameStarted {
			owners = append(owners, renameDone)
		}
		if policyStarted {
			owners = append(owners, policyDone)
		}
		for _, done := range owners {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("withheld-response fixture worker did not join")
			}
		}
	}()
	go func() {
		defer close(rowDone)
		rowResult <- r.store.InTransaction(ctx, func(tx pgx.Tx) error {
			var one, pid int
			if err := tx.QueryRow(ctx, `SELECT 1 FROM public.homeward_lists WHERE list_id=$1 FOR UPDATE`, nativeID).Scan(&one); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			locked <- pid
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var rowPID int
	select {
	case rowPID = <-locked:
	case err := <-rowResult:
		t.Fatal("fixture did not lock its disposable list row:", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	type renameResult struct {
		response *connect.Response[wire.TaskList]
		err      error
	}
	renamed := make(chan renameResult, 1)
	renameStarted = true
	go func() {
		defer close(renameDone)
		response, err := api.UpdateTaskList(ctx, nha162Bearer(&wire.UpdateTaskListRequest{TaskList: &wire.TaskList{Name: id, Etag: initial, DisplayName: "Committed with response withheld"}}, userToken))
		renamed <- renameResult{response, err}
	}()
	// The real domain call has passed its account/policy SQL share fences and
	// is now waiting for the fixture row. No domain hook is installed.
	renamePID := nha162WaitSQLBlockedBy(t, ctx, r.store, rowPID, "%FROM public.homeward_lists WHERE list_id=$1 FOR UPDATE%")
	type policyResult struct {
		response *connect.Response[wire.ReplaceAuthorizationPolicyResponse]
		err      error
	}
	withdrawn := make(chan policyResult, 1)
	policyStarted = true
	go func() {
		defer close(policyDone)
		response, err := policies.ReplaceAuthorizationPolicy(ctx, nha162Bearer(&wire.ReplaceAuthorizationPolicyRequest{
			Name:         user.Msg.Name + "/authorizationPolicy",
			YamlSource:   []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n", nha203AccountText(t, account))),
			Precondition: &wire.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: policy.Msg.Etag},
		}, r.adminToken))
		withdrawn <- policyResult{response, err}
	}()
	// Native ReplacePolicy holds its Authority Mutation until SQL+Publish end.
	// Its blocked policy-row lock proves that gate precedes Rename's COMMIT.
	_ = nha162WaitSQLBlockedBy(t, ctx, r.store, renamePID, "%FROM public.authorization_policy_active%FOR UPDATE OF p%")
	releaseRow()
	select {
	case err := <-rowResult:
		if err != nil {
			t.Fatal("fixture row release failed:", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case result := <-withdrawn:
		if result.err != nil || result.response == nil || result.response.Msg.RightsChange != wire.PolicyRightsChange_POLICY_RIGHTS_CHANGE_CHANGED {
			t.Fatal("native policy revocation did not commit/publish:", result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case result := <-renamed:
		var rejected *connect.Error
		if result.response != nil || !errors.As(result.err, &rejected) || rejected.Code() != connect.CodePermissionDenied || len(rejected.Details()) != 1 {
			t.Fatal("committed RPC returned a success/receipt or lost its structured refusal:", result.err)
		}
		if nha200Info(t, result.err).Reason != "ACCESS_DENIED" {
			t.Fatal("revocation disclosed commit details")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if renameCalls.Load() != 1 {
		t.Fatalf("client retried the ambiguous commit: Rename HTTP calls=%d", renameCalls.Load())
	}
	final, err := r.tasks.GetTaskList(ctx, nha162Bearer(&wire.GetTaskListRequest{Name: id}, r.adminToken))
	if err != nil || final.Msg.DisplayName != "Committed with response withheld" || final.Msg.Etag == initial {
		t.Fatal("withheld response did not leave exactly one durable mutation:", err)
	}
	var finalRevision int64
	if err = r.store.InTransaction(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT revision FROM public.homeward_lists WHERE list_id=$1`, nativeID).Scan(&finalRevision)
	}); err != nil || finalRevision != initialRevision+1 {
		t.Fatal("expected exactly one native SQL revision advance", err)
	}
	_, err = api.GetTaskList(ctx, nha162Bearer(&wire.GetTaskListRequest{Name: id}, userToken))
	nha162Code(t, err, connect.CodePermissionDenied)
}
