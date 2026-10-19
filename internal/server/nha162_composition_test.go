package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	listsv1 "github.com/damione1/Homeward/gen/home/lists/v1"
	"github.com/damione1/Homeward/gen/home/lists/v1/listsv1connect"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
	"github.com/damione1/Homeward/internal/lists"
	"github.com/damione1/Homeward/internal/registry"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/emptypb"
)

func nha162ImportContext(t *testing.T, r *nha162RPCRig) context.Context {
	t.Helper()
	response, err := r.auth.StartLogin(r.ctx, connect.NewRequest(&authv1.StartLoginRequest{
		LoginName: "NHA162 Admin", Password: "NHA162 synthetic admin password canary",
	}))
	if err != nil || response == nil || response.Msg.Decision.GetAuthenticated() == nil {
		t.Fatal("synthetic importer native login:", err)
	}
	tokens := response.Msg.Decision.GetAuthenticated()
	session, err := contracts.ParseSessionID(tokens.Session.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
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
				count, err := r.client.CountLists(r.ctx, nha162Bearer(&listsv1.CountListsRequest{}, r.adminToken))
				if err != nil || count.Msg.Count != 0 {
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
			page, err := r.client.ListItems(r.ctx, nha162Bearer(&listsv1.ListItemsRequest{ListId: &listsv1.ListId{Value: listText}, PageSize: 1}, r.adminToken))
			if err != nil || len(page.Msg.Items) != 1 || page.Msg.Items[0].Uid.Value != "nha162-synthetic-composed-item" {
				t.Fatal("composed import did not persist through the common ListService:", err)
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
		if request.URL.Path != listsv1connect.ListServiceWatchListsProcedure {
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
	api := listsv1connect.NewListServiceClient(client, server.URL)
	ctx, cancel := context.WithTimeout(r.ctx, 40*time.Second)
	defer cancel()
	scope := func() *listsv1.WatchListsRequest {
		return &listsv1.WatchListsRequest{Scope: &listsv1.WatchListsRequest_Catalog{Catalog: &emptypb.Empty{}}}
	}
	slow, err := api.WatchLists(ctx, nha162Bearer(scope(), r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	if !slow.Receive() || slow.Msg().GetReady() == nil {
		t.Fatal("blocked-writer Watch did not establish its subscription:", slow.Err())
	}
	page, err := r.client.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1}, r.adminToken))
	if err != nil || len(page.Msg.Lists) != 0 {
		t.Fatal("blocked-writer reload after ready:", err)
	}
	fast, err := r.client.WatchLists(ctx, nha162Bearer(scope(), r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close()
	if !fast.Receive() || fast.Msg().GetReady() == nil {
		t.Fatal("fast Watch did not establish its subscription:", fast.Err())
	}
	first, err := r.client.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Synthetic blocked frame"}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if !fast.Receive() || fast.Msg().GetChanged() == nil || fast.Msg().GetChanged().ListId.Value != first.Msg.Receipt.ListId.Value {
		t.Fatal("another Watch did not receive the first change:", fast.Err())
	}
	select {
	case <-gate.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("real Watch handler never reached its blocked response writer")
	}
	for i := 0; i <= lists.MaxNotificationQueue; i++ {
		created, err := r.client.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Synthetic queued invalidation"}, r.adminToken))
		if err != nil {
			t.Fatal("slow Watch blocked a writer:", err)
		}
		if !fast.Receive() || fast.Msg().GetChanged() == nil || fast.Msg().GetChanged().ListId.Value != created.Msg.Receipt.ListId.Value {
			t.Fatal("slow Watch blocked another Watch:", fast.Err())
		}
	}
	count, err := r.client.CountLists(ctx, nha162Bearer(&listsv1.CountListsRequest{}, r.adminToken))
	if err != nil || count.Msg.Count != uint64(lists.MaxNotificationQueue+2) {
		t.Fatal("writes did not finish while the real Watch send was blocked:", err)
	}
	gate.open()
	if !slow.Receive() || slow.Msg().GetChanged() == nil || slow.Msg().GetChanged().ListId.Value != first.Msg.Receipt.ListId.Value || slow.Msg().StreamSequence != 2 {
		t.Fatal("blocked first change did not finish after release:", slow.Err())
	}
	if !slow.Receive() || slow.Msg().GetResyncRequired() == nil || slow.Msg().GetChanged() != nil || slow.Msg().StreamSequence != 3 {
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

func TestNHA162AmbiguousCommitWireErrorsAreDistinctAndRedacted(t *testing.T) {
	for _, variant := range []struct {
		domain lists.Code
		wire   listsv1.ListErrorCode
	}{
		{lists.CommitUnknown, listsv1.ListErrorCode_LIST_ERROR_CODE_COMMIT_UNKNOWN},
		{lists.CommittedResponseWithheld, listsv1.ListErrorCode_LIST_ERROR_CODE_COMMITTED_RESPONSE_WITHHELD},
	} {
		err := listRPCCode(variant.domain)
		var structured *connect.Error
		if !errors.As(err, &structured) || structured.Code() != connect.CodeAborted || structured.Message() != "list request rejected" || len(structured.Details()) != 1 {
			t.Fatalf("ambiguous commit leaked data or lost its stable envelope: %v", err)
		}
		value, err := structured.Details()[0].Value()
		if err != nil {
			t.Fatal(err)
		}
		detail, ok := value.(*listsv1.ListError)
		if !ok || detail.Code != variant.wire || detail.FieldPath != nil {
			t.Fatalf("ambiguous commit lost its closed distinct detail: %v", value)
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
	user, err := r.accounts.CreateAccount(ctx, nha162Bearer(&authv1.CreateAccountRequest{
		LoginName: "NHA162 Withheld Writer", DisplayName: "NHA162 Withheld Writer",
		Role: authv1.AccountRole_ACCOUNT_ROLE_USER, InitialPassword: password,
	}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	account, err := contracts.ParseAccountID(user.Msg.Account.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
	nha162SetPolicy(t, r, account, `  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`)
	userToken := r.login(t, "NHA162 Withheld Writer", password)
	created, err := r.client.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Before withheld response"}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	id, initial := created.Msg.Receipt.ListId, created.Msg.Receipt.ListRevision
	policies := authv1connect.NewAuthorizationPolicyServiceClient(r.server.Client(), r.server.URL)
	policy, err := policies.GetPolicy(ctx, nha162Bearer(&authv1.GetPolicyRequest{AccountId: user.Msg.Account.Id}, r.adminToken))
	if err != nil || policy == nil || policy.Msg.GetPresent() == nil {
		t.Fatal("could not capture the real policy CAS:", err)
	}
	var renameCalls atomic.Int32
	mux := r.server.Config.Handler
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == listsv1connect.ListServiceRenameListProcedure {
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
	api := listsv1connect.NewListServiceClient(httpClient, server.URL)

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
			if err := tx.QueryRow(ctx, `SELECT 1 FROM public.homeward_lists WHERE list_id=$1 FOR UPDATE`, id.Value).Scan(&one); err != nil {
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
		response *connect.Response[listsv1.RenameListResponse]
		err      error
	}
	renamed := make(chan renameResult, 1)
	renameStarted = true
	go func() {
		defer close(renameDone)
		response, err := api.RenameList(ctx, nha162Bearer(&listsv1.RenameListRequest{
			ListId: id, ExpectedRevision: initial, Name: "Committed with response withheld",
		}, userToken))
		renamed <- renameResult{response, err}
	}()
	// The real domain call has passed its account/policy SQL share fences and
	// is now waiting for the fixture row. No domain hook is installed.
	renamePID := nha162WaitSQLBlockedBy(t, ctx, r.store, rowPID, "%FROM public.homeward_lists WHERE list_id=$1 FOR UPDATE%")
	type policyResult struct {
		response *connect.Response[authv1.ReplacePolicyResponse]
		err      error
	}
	withdrawn := make(chan policyResult, 1)
	policyStarted = true
	go func() {
		defer close(policyDone)
		response, err := policies.ReplacePolicy(ctx, nha162Bearer(&authv1.ReplacePolicyRequest{
			AccountId:    user.Msg.Account.Id,
			YamlSource:   []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants: []\n", user.Msg.Account.Id.Value)),
			Precondition: &authv1.ReplacePolicyRequest_Expected{Expected: policy.Msg.GetPresent().Version},
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
		if result.err != nil || result.response == nil || result.response.Msg.RightsChange != authv1.PolicyRightsChange_POLICY_RIGHTS_CHANGE_CHANGED {
			t.Fatal("native policy revocation did not commit/publish:", result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case result := <-renamed:
		var rejected *connect.Error
		if result.response != nil || !errors.As(result.err, &rejected) || rejected.Code() != connect.CodeAborted || rejected.Message() != "list request rejected" || len(rejected.Details()) != 1 {
			t.Fatal("committed RPC returned a success/receipt or lost its structured refusal:", result.err)
		}
		value, err := rejected.Details()[0].Value()
		if err != nil {
			t.Fatal(err)
		}
		detail, ok := value.(*listsv1.ListError)
		if !ok || detail.Code != listsv1.ListErrorCode_LIST_ERROR_CODE_COMMITTED_RESPONSE_WITHHELD || detail.FieldPath != nil {
			t.Fatalf("committed RPC was mislabeled or disclosed data: %v", value)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if renameCalls.Load() != 1 {
		t.Fatalf("client retried the ambiguous commit: Rename HTTP calls=%d", renameCalls.Load())
	}
	final, err := r.client.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: id}, r.adminToken))
	if err != nil || final.Msg.List.Name != "Committed with response withheld" || final.Msg.List.Revision.Number != initial.Number+1 {
		t.Fatal("withheld response did not leave exactly one durable mutation:", err)
	}
	_, err = api.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: id}, userToken))
	nha162Code(t, err, connect.CodePermissionDenied)
}
