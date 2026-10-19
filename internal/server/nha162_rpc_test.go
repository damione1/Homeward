package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/testpg"
	"google.golang.org/protobuf/types/known/emptypb"
)

type nha162RPCRig struct {
	ctx         context.Context
	store       *storage.Store
	coordinator *sessionCoordinator
	lists       *listComposition
	server      *httptest.Server
	auth        authv1connect.AuthServiceClient
	accounts    authv1connect.AccountServiceClient
	client      listsv1connect.ListServiceClient
	admin       contracts.AccountID
	adminToken  string
}

const nha162All = `  - scope: {family: lists, operation: read}
    objects: {lists: {all: {}}}
  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`

func nha162SetPolicy(t *testing.T, rig *nha162RPCRig, target contracts.AccountID, grants string) {
	t.Helper()
	accountText, err := target.Value()
	if err != nil {
		t.Fatal(err)
	}
	source, err := authorization.AdmitPolicySource(target, []byte(fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n%s", accountText, grants)))
	if err != nil {
		t.Fatal(err)
	}
	current, err := rig.store.AuthorizationPolicy(rig.ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	version, err := current.Version()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := authorization.ExpectVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := rig.store.ReplaceAuthorizationPolicy(rig.ctx, rig.admin, target, source, expected)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := outcome.Result()
	if err != nil {
		t.Fatal(err)
	}
	state, err := snapshot.State()
	if err != nil {
		t.Fatal(err)
	}
	if rig.coordinator != nil {
		if err := rig.coordinator.authority.Publish(state); err != nil {
			t.Fatal(err)
		}
	}
}

func nha162NewRPCRig(t *testing.T) *nha162RPCRig {
	t.Helper()
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	configuration := nha049Config(t, cluster.Port, dataDir)
	postgres, err := configuration.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	store, err := storage.Open(ctx, postgres)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); store.Close() })
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapMFAPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	const password = "NHA162 synthetic admin password canary"
	admin, err := newAccountInput(ctx, "NHA162 Admin", "NHA162 Admin", password, authv1.AccountRole_ACCOUNT_ROLE_ADMIN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BootstrapAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	rig := &nha162RPCRig{ctx: ctx, store: store, admin: admin.ID}
	nha162SetPolicy(t, rig, admin.ID, nha162All)
	coordinator, err := prepareLocalAuthorization(ctx, store, realSessionClock{})
	if err != nil {
		t.Fatal(err)
	}
	rig.coordinator = coordinator
	lists, err := newListComposition(store, coordinator.authority)
	if err != nil {
		t.Fatal(err)
	}
	rig.lists = lists
	t.Cleanup(func() {
		if err := lists.Close(); err != nil {
			t.Error(err)
		}
	})
	plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: store})
	if err != nil {
		t.Fatal(err)
	}
	if err := lists.Register(plan); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, store, coordinator)
	if err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultPublicSettings()
	compiled, err := compilePublicPlan(plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(compiled.handler(settings, newPublicAdmissions()))
	server.EnableHTTP2 = true
	server.StartTLS()
	rig.server = server
	t.Cleanup(server.Close)
	httpClient := server.Client()
	httpClient.Timeout = 40 * time.Second
	rig.auth = authv1connect.NewAuthServiceClient(httpClient, server.URL)
	rig.accounts = authv1connect.NewAccountServiceClient(httpClient, server.URL)
	rig.client = listsv1connect.NewListServiceClient(httpClient, server.URL)
	rig.adminToken = rig.login(t, "NHA162 Admin", password)
	return rig
}

func (r *nha162RPCRig) login(t *testing.T, name, password string) string {
	t.Helper()
	response, err := r.auth.StartLogin(r.ctx, connect.NewRequest(&authv1.StartLoginRequest{LoginName: name, Password: password}))
	if err != nil || response.Msg.Decision.GetAuthenticated() == nil {
		t.Fatalf("real session login: %v", err)
	}
	return response.Msg.Decision.GetAuthenticated().AccessToken
}

func nha162Bearer[T any](message *T, access string) *connect.Request[T] {
	request := connect.NewRequest(message)
	request.Header().Set("Authorization", "Bearer "+access)
	return request
}

func nha162Code(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != want {
		t.Fatalf("Connect code %v, want %v: %v", connect.CodeOf(err), want, err)
	}
}

func TestNHA162RealMuxCRUDPagesCASAndPermissionSeparation(t *testing.T) {
	r := nha162NewRPCRig(t)
	ctx, admin := r.ctx, r.adminToken
	first, err := r.client.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Synthetic tasks"}, admin))
	if err != nil {
		t.Fatal(err)
	}
	listID, revision := first.Msg.Receipt.ListId, first.Msg.Receipt.ListRevision
	second, err := r.client.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Synthetic shopping"}, admin))
	if err != nil {
		t.Fatal(err)
	}
	firstPage, err := r.client.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1}, admin))
	if err != nil || len(firstPage.Msg.Lists) != 1 || firstPage.Msg.NextPageToken == "" {
		t.Fatalf("first list page: %v", err)
	}
	lastPage, err := r.client.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1, PageToken: firstPage.Msg.NextPageToken}, admin))
	if err != nil || len(lastPage.Msg.Lists) != 1 || lastPage.Msg.NextPageToken != "" || lastPage.Msg.Lists[0].Id.Value == firstPage.Msg.Lists[0].Id.Value {
		t.Fatalf("second list page: %v", err)
	}
	count, err := r.client.CountLists(ctx, nha162Bearer(&listsv1.CountListsRequest{}, admin))
	if err != nil || count.Msg.Count != 2 {
		t.Fatalf("list count: %v", err)
	}
	_, err = r.client.RenameList(ctx, nha162Bearer(&listsv1.RenameListRequest{ListId: listID, ExpectedRevision: &listsv1.ListRevision{ListId: second.Msg.Receipt.ListId, Number: revision.Number}, Name: "Wrong linked revision"}, admin))
	nha162Code(t, err, connect.CodeInvalidArgument)
	rename, err := r.client.RenameList(ctx, nha162Bearer(&listsv1.RenameListRequest{ListId: listID, ExpectedRevision: revision, Name: "Renamed tasks"}, admin))
	if err != nil {
		t.Fatal(err)
	}
	revision = rename.Msg.Receipt.ListRevision
	_, err = r.client.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1, PageToken: firstPage.Msg.NextPageToken}, admin))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	_, err = r.client.RenameList(ctx, nha162Bearer(&listsv1.RenameListRequest{ListId: listID, ExpectedRevision: first.Msg.Receipt.ListRevision, Name: "Stale writer"}, admin))
	nha162Code(t, err, connect.CodeAborted)

	_, err = r.client.AddItem(ctx, nha162Bearer(&listsv1.AddItemRequest{ListId: listID, ExpectedListRevision: revision, Item: &listsv1.ItemPayload{Summary: "Missing presence"}}, admin))
	nha162Code(t, err, connect.CodeInvalidArgument)
	completed := false
	_, err = r.client.AddItem(ctx, nha162Bearer(&listsv1.AddItemRequest{ListId: listID, ExpectedListRevision: revision, Item: &listsv1.ItemPayload{Summary: "Impossible civil date", Completed: &completed, Due: &listsv1.ItemPayload_DueDate{DueDate: "2026-02-29"}}}, admin))
	nha162Code(t, err, connect.CodeInvalidArgument)
	add, err := r.client.AddItem(ctx, nha162Bearer(&listsv1.AddItemRequest{ListId: listID, ExpectedListRevision: revision, Item: &listsv1.ItemPayload{Summary: "First", Completed: &completed}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	firstUID := add.Msg.Receipt.ItemUid
	revision = add.Msg.Receipt.ListRevision
	firstItemRevision := add.Msg.Receipt.ItemRevision
	addSecond, err := r.client.AddItem(ctx, nha162Bearer(&listsv1.AddItemRequest{ListId: listID, ExpectedListRevision: revision, Item: &listsv1.ItemPayload{Summary: "Second", Completed: &completed}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	secondUID := addSecond.Msg.Receipt.ItemUid
	revision = addSecond.Msg.Receipt.ListRevision
	// Typed-nil oneofs cannot be sent by a normal protobuf client, but the
	// generated handler must still fail closed if one reaches its Go boundary.
	httpRequest := httptest.NewRequest(http.MethodPost, listsv1connect.ListServiceMoveItemProcedure, nil)
	httpRequest.TLS = &tls.ConnectionState{}
	httpRequest.Header.Set("Authorization", "Bearer "+admin)
	actor, admittedCtx, release, err := (&sessionAuth{store: r.store, coordinator: r.coordinator}).AuthenticateWithLease(ctx, httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	admittedCtx = context.WithValue(admittedCtx, actorContextKey{}, actor)
	_, err = r.lists.handler.MoveItem(admittedCtx, connect.NewRequest(&listsv1.MoveItemRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: revision, ExpectedItemRevision: firstItemRevision, Destination: (*listsv1.MoveItemRequest_AfterUid)(nil)}))
	nha162Code(t, err, connect.CodeInvalidArgument)
	_, err = r.lists.handler.AddItem(admittedCtx, connect.NewRequest(&listsv1.AddItemRequest{ListId: listID, ExpectedListRevision: revision, Item: &listsv1.ItemPayload{Summary: "Typed nil due", Completed: &completed, Due: (*listsv1.ItemPayload_DueInstant)(nil)}}))
	nha162Code(t, err, connect.CodeInvalidArgument)
	release()
	items, err := r.client.ListItems(ctx, nha162Bearer(&listsv1.ListItemsRequest{ListId: listID, PageSize: 1}, admin))
	if err != nil || len(items.Msg.Items) != 1 || items.Msg.NextPageToken == "" {
		t.Fatalf("first item page: %v", err)
	}
	itemsNext, err := r.client.ListItems(ctx, nha162Bearer(&listsv1.ListItemsRequest{ListId: listID, PageSize: 1, PageToken: items.Msg.NextPageToken}, admin))
	if err != nil || len(itemsNext.Msg.Items) != 1 || itemsNext.Msg.NextPageToken != "" || itemsNext.Msg.Items[0].Uid.Value == items.Msg.Items[0].Uid.Value {
		t.Fatalf("second item page: %v", err)
	}
	itemCount, err := r.client.CountItems(ctx, nha162Bearer(&listsv1.CountItemsRequest{ListId: listID}, admin))
	if err != nil || itemCount.Msg.Count != 2 {
		t.Fatalf("item count: %v", err)
	}

	const userPassword = "NHA162 synthetic user password canary"
	created, err := r.accounts.CreateAccount(ctx, nha162Bearer(&authv1.CreateAccountRequest{LoginName: "NHA162 User", DisplayName: "NHA162 User", InitialPassword: userPassword, Role: authv1.AccountRole_ACCOUNT_ROLE_USER}, admin))
	if err != nil {
		t.Fatal(err)
	}
	userID, err := contracts.ParseAccountID(created.Msg.Account.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := fmt.Sprintf("  - scope: {family: lists, operation: read}\n    objects: {lists: {explicit: [%q]}}\n", listID.Value)
	nha162SetPolicy(t, r, userID, readOnly)
	user := r.login(t, "NHA162 User", userPassword)
	if _, err := r.client.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: listID}, user)); err != nil {
		t.Fatalf("READ-granted list: %v", err)
	}
	_, err = r.client.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: second.Msg.Receipt.ListId}, user))
	nha162Code(t, err, connect.CodePermissionDenied)
	filtered, err := r.client.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1}, user))
	if err != nil || len(filtered.Msg.Lists) != 1 || filtered.Msg.Lists[0].Id.Value != listID.Value || filtered.Msg.NextPageToken != "" {
		t.Fatalf("filtered page: %v", err)
	}
	filteredCount, err := r.client.CountLists(ctx, nha162Bearer(&listsv1.CountListsRequest{}, user))
	if err != nil || filteredCount.Msg.Count != 1 {
		t.Fatalf("filtered count: %v", err)
	}
	_, err = r.client.AddItem(ctx, nha162Bearer(&listsv1.AddItemRequest{ListId: listID, ExpectedListRevision: revision, Item: &listsv1.ItemPayload{Summary: "Forbidden", Completed: &completed}}, user))
	nha162Code(t, err, connect.CodePermissionDenied)
	writeOnly := `  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`
	nha162SetPolicy(t, r, userID, writeOnly)
	_, err = r.client.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: listID}, user))
	nha162Code(t, err, connect.CodePermissionDenied)
	description := "Present synthetic description"
	edit, err := r.client.EditItem(ctx, nha162Bearer(&listsv1.EditItemRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: revision, ExpectedItemRevision: firstItemRevision, Item: &listsv1.ItemPayload{Summary: "User edit", Completed: &completed, Description: &description, Due: &listsv1.ItemPayload_DueDate{DueDate: "2028-02-29"}}}, user))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.client.ListItems(ctx, nha162Bearer(&listsv1.ListItemsRequest{ListId: listID, PageSize: 1, PageToken: items.Msg.NextPageToken}, admin))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	changedItem, err := r.client.GetItem(ctx, nha162Bearer(&listsv1.GetItemRequest{ListId: listID, ItemUid: firstUID}, admin))
	if err != nil || changedItem.Msg.Item.Value.Description == nil || *changedItem.Msg.Item.Value.Description != description || changedItem.Msg.Item.Value.GetDueDate() != "2028-02-29" {
		t.Fatalf("edit lost description or civil due date: %v", err)
	}
	_, err = r.client.EditItem(ctx, nha162Bearer(&listsv1.EditItemRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: revision, ExpectedItemRevision: firstItemRevision, Item: &listsv1.ItemPayload{Summary: "Admin stale edit", Completed: &completed}}, admin))
	nha162Code(t, err, connect.CodeAborted)
	revision = edit.Msg.Receipt.ListRevision
	firstItemRevision = edit.Msg.Receipt.ItemRevision
	_, err = r.client.SetCompleted(ctx, nha162Bearer(&listsv1.SetCompletedRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: revision, ExpectedItemRevision: firstItemRevision}, admin))
	nha162Code(t, err, connect.CodeInvalidArgument)
	setTrue := true
	marked, err := r.client.SetCompleted(ctx, nha162Bearer(&listsv1.SetCompletedRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: revision, ExpectedItemRevision: firstItemRevision, Completed: &setTrue}, admin))
	if err != nil {
		t.Fatal(err)
	}
	revision, firstItemRevision = marked.Msg.Receipt.ListRevision, marked.Msg.Receipt.ItemRevision
	unmarked, err := r.client.SetCompleted(ctx, nha162Bearer(&listsv1.SetCompletedRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: revision, ExpectedItemRevision: firstItemRevision, Completed: &completed}, admin))
	if err != nil {
		t.Fatal(err)
	}
	revision, firstItemRevision = unmarked.Msg.Receipt.ListRevision, unmarked.Msg.Receipt.ItemRevision
	got, err := r.client.GetItem(ctx, nha162Bearer(&listsv1.GetItemRequest{ListId: listID, ItemUid: firstUID}, admin))
	if err != nil || got.Msg.Item.Value.Completed == nil || *got.Msg.Item.Value.Completed || got.Msg.Item.Value.CompletedAt != nil {
		t.Fatalf("present false did not decomplete: %v", err)
	}
	moved, err := r.client.MoveItem(ctx, nha162Bearer(&listsv1.MoveItemRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: revision, ExpectedItemRevision: firstItemRevision, Destination: &listsv1.MoveItemRequest_AfterUid{AfterUid: secondUID}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	revision, firstItemRevision = moved.Msg.Receipt.ListRevision, moved.Msg.Receipt.ItemRevision
	ordered, err := r.client.ListItems(ctx, nha162Bearer(&listsv1.ListItemsRequest{ListId: listID, PageSize: 2}, admin))
	if err != nil || len(ordered.Msg.Items) != 2 || ordered.Msg.Items[0].Uid.Value != secondUID.Value || ordered.Msg.Items[1].Uid.Value != firstUID.Value {
		t.Fatalf("move after UID did not change order: %v", err)
	}
	_, err = r.client.MoveItem(ctx, nha162Bearer(&listsv1.MoveItemRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: revision, ExpectedItemRevision: firstItemRevision, Destination: &listsv1.MoveItemRequest_AtStart{AtStart: &emptypb.Empty{}}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	ordered, err = r.client.ListItems(ctx, nha162Bearer(&listsv1.ListItemsRequest{ListId: listID, PageSize: 2}, admin))
	if err != nil || len(ordered.Msg.Items) != 2 || ordered.Msg.Items[0].Uid.Value != firstUID.Value {
		t.Fatalf("move to start did not restore order: %v", err)
	}
	// Refresh the linked revisions after the second move, then prove deletion.
	current, err := r.client.GetItem(ctx, nha162Bearer(&listsv1.GetItemRequest{ListId: listID, ItemUid: firstUID}, admin))
	if err != nil {
		t.Fatal(err)
	}
	currentList, err := r.client.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: listID}, admin))
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := r.client.DeleteItem(ctx, nha162Bearer(&listsv1.DeleteItemRequest{ListId: listID, ItemUid: firstUID, ExpectedListRevision: currentList.Msg.List.Revision, ExpectedItemRevision: current.Msg.Item.Revision}, admin))
	if err != nil || deleted.Msg.Receipt.ItemUid.Value != firstUID.Value {
		t.Fatalf("delete item: %v", err)
	}
	_, err = r.client.GetItem(ctx, nha162Bearer(&listsv1.GetItemRequest{ListId: listID, ItemUid: firstUID}, admin))
	nha162Code(t, err, connect.CodeNotFound)
	_, err = r.client.DeleteList(ctx, nha162Bearer(&listsv1.DeleteListRequest{ListId: listID, ExpectedRevision: deleted.Msg.Receipt.ListRevision}, admin))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.client.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: listID}, admin))
	nha162Code(t, err, connect.CodeNotFound)
}

func TestNHA162RealMuxWatchReadyPageChangeDeletionAndRevocation(t *testing.T) {
	r := nha162NewRPCRig(t)
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	stream, err := r.client.WatchLists(ctx, nha162Bearer(&listsv1.WatchListsRequest{Scope: &listsv1.WatchListsRequest_Catalog{Catalog: &emptypb.Empty{}}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil || stream.Msg().StreamSequence != 1 {
		t.Fatalf("watch did not first send ready: %v", stream.Err())
	}
	page, err := r.client.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1}, r.adminToken))
	if err != nil || len(page.Msg.Lists) != 0 {
		t.Fatalf("ready did not permit initial page: %v", err)
	}
	created, err := r.client.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Synthetic watched list"}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	listID := created.Msg.Receipt.ListId
	if !stream.Receive() || stream.Msg().GetChanged() == nil || stream.Msg().GetChanged().ListId.Value != listID.Value || stream.Msg().StreamSequence != 2 {
		t.Fatalf("watch did not deliver create invalidation: %v", stream.Err())
	}
	page, err = r.client.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1}, r.adminToken))
	if err != nil || len(page.Msg.Lists) != 1 || page.Msg.Lists[0].Id.Value != listID.Value {
		t.Fatalf("watch reload did not see created list: %v", err)
	}
	_, err = r.client.DeleteList(ctx, nha162Bearer(&listsv1.DeleteListRequest{ListId: listID, ExpectedRevision: created.Msg.Receipt.ListRevision}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || stream.Msg().GetChanged() == nil || stream.Msg().GetChanged().ListId.Value != listID.Value || stream.Msg().StreamSequence != 3 {
		t.Fatalf("watch did not deliver deletion invalidation: %v", stream.Err())
	}
	page, err = r.client.ListLists(ctx, nha162Bearer(&listsv1.ListListsRequest{PageSize: 1}, r.adminToken))
	if err != nil || len(page.Msg.Lists) != 0 {
		t.Fatalf("watch reload did not see deletion: %v", err)
	}

	created, err = r.client.CreateList(ctx, nha162Bearer(&listsv1.CreateListRequest{Name: "Synthetic revocation target"}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	listID = created.Msg.Receipt.ListId
	const password = "NHA162 synthetic watcher password canary"
	user, err := r.accounts.CreateAccount(ctx, nha162Bearer(&authv1.CreateAccountRequest{LoginName: "NHA162 Watcher", DisplayName: "NHA162 Watcher", InitialPassword: password, Role: authv1.AccountRole_ACCOUNT_ROLE_USER}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	userID, err := contracts.ParseAccountID(user.Msg.Account.Id.Value)
	if err != nil {
		t.Fatal(err)
	}
	readOne := fmt.Sprintf("  - scope: {family: lists, operation: read}\n    objects: {lists: {explicit: [%q]}}\n", listID.Value)
	nha162SetPolicy(t, r, userID, readOne)
	userToken := r.login(t, "NHA162 Watcher", password)
	userWatch, err := r.client.WatchLists(ctx, nha162Bearer(&listsv1.WatchListsRequest{Scope: &listsv1.WatchListsRequest_ListId{ListId: listID}}, userToken))
	if err != nil {
		t.Fatal(err)
	}
	defer userWatch.Close()
	if !userWatch.Receive() || userWatch.Msg().GetReady() == nil {
		t.Fatalf("granted watcher did not reach ready: %v", userWatch.Err())
	}
	if _, err := r.client.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: listID}, userToken)); err != nil {
		t.Fatalf("watch reload denied prior to revocation: %v", err)
	}
	// The SQL policy edit and Authority publication cancel the real stream.
	nha162SetPolicy(t, r, userID, `  - scope: {family: lists, operation: write}
    objects: {lists: {all: {}}}
`)
	if userWatch.Receive() || userWatch.Err() == nil {
		t.Fatalf("revoked watcher received data or stayed open: %v", userWatch.Err())
	}
	_, err = r.client.GetList(ctx, nha162Bearer(&listsv1.GetListRequest{ListId: listID}, userToken))
	nha162Code(t, err, connect.CodePermissionDenied)
}
