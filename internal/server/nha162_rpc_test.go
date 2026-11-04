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
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	authv1 "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/storage"
	"github.com/damione1/Homeward/internal/tasknames"
	"github.com/damione1/Homeward/internal/testpg"
	datepb "google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type nha162RPCRig struct {
	ctx         context.Context
	store       *storage.Store
	coordinator *sessionCoordinator
	lists       *listComposition
	server      *httptest.Server
	auth        apiv2connect.AuthServiceClient
	accounts    apiv2connect.AccountServiceClient
	tasks       apiv2connect.TaskServiceClient
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
	return nha162NewRPCRigWithLifetime(t, 120*time.Second)
}

func nha162NewRPCRigWithLifetime(t *testing.T, lifetime time.Duration) *nha162RPCRig {
	t.Helper()
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	configuration := nha049Config(t, cluster.Port, dataDir)
	postgres, err := configuration.PostgresSettings()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
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
	if err = registerHomeDiscoveryRoutes(plan, lists.registry, coordinator.authority); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, store, coordinator, administrationNames{home: lists.registry, store: store})
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
	rig.auth = apiv2connect.NewAuthServiceClient(httpClient, server.URL)
	rig.accounts = apiv2connect.NewAccountServiceClient(httpClient, server.URL)
	rig.tasks = apiv2connect.NewTaskServiceClient(httpClient, server.URL)
	rig.adminToken = rig.login(t, "NHA162 Admin", password)
	return rig
}

func (r *nha162RPCRig) login(t *testing.T, name, password string) string {
	t.Helper()
	response, err := r.auth.StartLogin(r.ctx, connect.NewRequest(&api.StartLoginRequest{LoginName: name, Password: password}))
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

// Read fixture identities from the same SQL owner used by production routes.
func (r *nha162RPCRig) taskHome(t *testing.T) string {
	t.Helper()
	name, err := r.lists.registry.HomeName(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return name.String()
}
func (r *nha162RPCRig) taskList(t *testing.T, name string) *api.TaskList {
	t.Helper()
	v, err := r.tasks.GetTaskList(r.ctx, nha162Bearer(&api.GetTaskListRequest{Name: name}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return v.Msg
}
func (r *nha162RPCRig) task(t *testing.T, name string) *api.Task {
	t.Helper()
	v, err := r.tasks.GetTask(r.ctx, nha162Bearer(&api.GetTaskRequest{Name: name}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	return v.Msg
}
func TestNHA162RealMuxCRUDPagesCASAndPermissionSeparation(t *testing.T) {
	r := nha162NewRPCRig(t)
	ctx, admin := r.ctx, r.adminToken
	home := r.taskHome(t)
	create := func(label string) *api.TaskList {
		v, e := r.tasks.CreateTaskList(ctx, nha162Bearer(&api.CreateTaskListRequest{Parent: home, TaskList: &api.TaskList{DisplayName: label}}, admin))
		if e != nil {
			t.Fatal(e)
		}
		return v.Msg
	}
	first, second := create("Synthetic tasks"), create("Synthetic shopping")
	page, err := r.tasks.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home, PageSize: 1}, admin))
	if err != nil || len(page.Msg.TaskLists) != 1 || page.Msg.NextPageToken == "" || page.Msg.TotalSize != 2 {
		t.Fatalf("first page: %v", err)
	}
	next, err := r.tasks.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home, PageSize: 1, PageToken: page.Msg.NextPageToken}, admin))
	if err != nil || len(next.Msg.TaskLists) != 1 || next.Msg.NextPageToken != "" || next.Msg.TaskLists[0].Name == page.Msg.TaskLists[0].Name {
		t.Fatalf("next page: %v", err)
	}
	_, err = r.tasks.UpdateTaskList(ctx, nha162Bearer(&api.UpdateTaskListRequest{TaskList: &api.TaskList{Name: first.Name, Etag: second.Etag, DisplayName: "Foreign etag"}}, admin))
	nha162Code(t, err, connect.CodeAborted)
	renamed, err := r.tasks.UpdateTaskList(ctx, nha162Bearer(&api.UpdateTaskListRequest{TaskList: &api.TaskList{Name: first.Name, Etag: first.Etag, DisplayName: "Renamed tasks"}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.tasks.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home, PageToken: page.Msg.NextPageToken}, admin))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	_, err = r.tasks.UpdateTaskList(ctx, nha162Bearer(&api.UpdateTaskListRequest{TaskList: &api.TaskList{Name: first.Name, Etag: first.Etag, DisplayName: "Stale"}}, admin))
	nha162Code(t, err, connect.CodeAborted)
	_, err = r.tasks.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: first.Name, Etag: renamed.Msg.Etag, Task: &api.TaskContent{Summary: "Impossible date", Due: &api.TaskContent_DueDate{DueDate: &datepb.Date{Year: 2026, Month: 2, Day: 29}}}}, admin))
	nha162Code(t, err, connect.CodeInvalidArgument)
	appendTask := func(summary string) *api.Task {
		v, e := r.tasks.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: first.Name, Etag: r.taskList(t, first.Name).Etag, Task: &api.TaskContent{Summary: summary}}, admin))
		if e != nil {
			t.Fatal(e)
		}
		return v.Msg.Task
	}
	a, b := appendTask("First"), appendTask("Second")
	a = r.task(t, a.Name)
	// Typed-nil protobuf variants must fail closed even at the in-process boundary.
	req := httptest.NewRequest(http.MethodPost, apiv2connect.TaskServiceMoveTaskProcedure, nil)
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Authorization", "Bearer "+admin)
	actor, admitted, release, err := (&sessionAuth{store: r.store, coordinator: r.coordinator}).AuthenticateWithLease(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	admitted = context.WithValue(admitted, actorContextKey{}, actor)
	_, err = r.lists.tasks.MoveTask(admitted, connect.NewRequest(&api.MoveTaskRequest{Name: a.Name, Etag: a.Etag, Destination: (*api.MoveTaskRequest_Predecessor)(nil)}))
	nha162Code(t, err, connect.CodeInvalidArgument)
	_, err = r.lists.tasks.AppendTask(admitted, connect.NewRequest(&api.AppendTaskRequest{Name: first.Name, Etag: r.taskList(t, first.Name).Etag, Task: &api.TaskContent{Summary: "Typed nil due", Due: (*api.TaskContent_DueInstant)(nil)}}))
	nha162Code(t, err, connect.CodeInvalidArgument)
	release()
	items, err := r.tasks.ListTasks(ctx, nha162Bearer(&api.ListTasksRequest{Parent: first.Name, PageSize: 1}, admin))
	if err != nil || len(items.Msg.Tasks) != 1 || items.Msg.TotalSize != 2 || items.Msg.NextPageToken == "" {
		t.Fatalf("task page: %v", err)
	}
	more, err := r.tasks.ListTasks(ctx, nha162Bearer(&api.ListTasksRequest{Parent: first.Name, PageSize: 1, PageToken: items.Msg.NextPageToken}, admin))
	if err != nil || len(more.Msg.Tasks) != 1 || more.Msg.NextPageToken != "" || more.Msg.Tasks[0].Name == items.Msg.Tasks[0].Name {
		t.Fatalf("task next: %v", err)
	}
	const password = "NHA162 synthetic user password canary"
	account, err := r.accounts.CreateAccount(ctx, nha162Bearer(&api.CreateAccountRequest{Parent: r.taskHome(t), Account: &api.Account{LoginName: "NHA162 User", DisplayName: "NHA162 User", InitialPassword: password, Role: api.AccountRole_ACCOUNT_ROLE_USER}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	userID := nha203NativeAccount(t, ctx, r.store, account.Msg.Name)
	name, err := tasknames.ParseList(first.Name)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, err := name.ID().Value()
	if err != nil {
		t.Fatal(err)
	}
	nha162SetPolicy(t, r, userID, fmt.Sprintf("  - scope: {family: lists, operation: read}\n    objects: {lists: {explicit: [%q]}}\n", nativeID))
	user := r.login(t, "NHA162 User", password)
	if _, err = r.tasks.GetTaskList(ctx, nha162Bearer(&api.GetTaskListRequest{Name: first.Name}, user)); err != nil {
		t.Fatal(err)
	}
	_, err = r.tasks.GetTaskList(ctx, nha162Bearer(&api.GetTaskListRequest{Name: second.Name}, user))
	nha162Code(t, err, connect.CodePermissionDenied)
	filtered, err := r.tasks.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home, PageSize: 1}, user))
	if err != nil || len(filtered.Msg.TaskLists) != 1 || filtered.Msg.TaskLists[0].Name != first.Name || filtered.Msg.TotalSize != 1 || filtered.Msg.NextPageToken != "" {
		t.Fatalf("filtered page: %v", err)
	}
	_, err = r.tasks.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: first.Name, Etag: r.taskList(t, first.Name).Etag, Task: &api.TaskContent{Summary: "Forbidden"}}, user))
	nha162Code(t, err, connect.CodePermissionDenied)
	nha162SetPolicy(t, r, userID, "  - scope: {family: lists, operation: write}\n    objects: {lists: {all: {}}}\n")
	_, err = r.tasks.GetTaskList(ctx, nha162Bearer(&api.GetTaskListRequest{Name: first.Name}, user))
	nha162Code(t, err, connect.CodePermissionDenied)
	description := "Present synthetic description"
	edit, err := r.tasks.UpdateTask(ctx, nha162Bearer(&api.UpdateTaskRequest{Task: &api.Task{Name: a.Name, Etag: a.Etag, Summary: "User edit", Description: &description, Due: &api.Task_DueDate{DueDate: &datepb.Date{Year: 2028, Month: 2, Day: 29}}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"*"}}}, user))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.tasks.ListTasks(ctx, nha162Bearer(&api.ListTasksRequest{Parent: first.Name, PageToken: items.Msg.NextPageToken}, admin))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	changed := r.task(t, a.Name)
	if changed.GetDescription() != description || changed.GetDueDate().GetDay() != 29 {
		t.Fatal("edit lost description/date")
	}
	_, err = r.tasks.UpdateTask(ctx, nha162Bearer(&api.UpdateTaskRequest{Task: &api.Task{Name: a.Name, Etag: a.Etag, Summary: "Stale"}}, admin))
	nha162Code(t, err, connect.CodeAborted)
	current := edit.Msg
	for _, completed := range []bool{true, false} {
		v, e := r.tasks.UpdateTask(ctx, nha162Bearer(&api.UpdateTaskRequest{Task: &api.Task{Name: current.Name, Etag: current.Etag, Completed: completed}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"completed"}}}, admin))
		if e != nil {
			t.Fatal(e)
		}
		current = v.Msg
	}
	if current.Completed || current.CompletionTime != nil {
		t.Fatal("explicit false did not clear completion")
	}
	moved, err := r.tasks.MoveTask(ctx, nha162Bearer(&api.MoveTaskRequest{Name: a.Name, Etag: current.Etag, Destination: &api.MoveTaskRequest_Predecessor{Predecessor: b.Name}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := r.tasks.ListTasks(ctx, nha162Bearer(&api.ListTasksRequest{Parent: first.Name}, admin))
	if err != nil || len(ordered.Msg.Tasks) != 2 || ordered.Msg.Tasks[0].Name != b.Name || ordered.Msg.Tasks[1].Name != a.Name {
		t.Fatalf("move order: %v", err)
	}
	_, err = r.tasks.MoveTask(ctx, nha162Bearer(&api.MoveTaskRequest{Name: a.Name, Etag: moved.Msg.Etag, Destination: &api.MoveTaskRequest_First{First: &emptypb.Empty{}}}, admin))
	if err != nil {
		t.Fatal(err)
	}
	ordered, err = r.tasks.ListTasks(ctx, nha162Bearer(&api.ListTasksRequest{Parent: first.Name}, admin))
	if err != nil || ordered.Msg.Tasks[0].Name != a.Name {
		t.Fatalf("first move: %v", err)
	}
	_, err = r.tasks.DeleteTask(ctx, nha162Bearer(&api.DeleteTaskRequest{Name: a.Name, Etag: r.task(t, a.Name).Etag}, admin))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.tasks.GetTask(ctx, nha162Bearer(&api.GetTaskRequest{Name: a.Name}, admin))
	nha162Code(t, err, connect.CodeNotFound)
	_, err = r.tasks.DeleteTaskList(ctx, nha162Bearer(&api.DeleteTaskListRequest{Name: first.Name, Etag: r.taskList(t, first.Name).Etag, Force: true}, admin))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.tasks.GetTaskList(ctx, nha162Bearer(&api.GetTaskListRequest{Name: first.Name}, admin))
	nha162Code(t, err, connect.CodeNotFound)
}
func TestNHA162RealMuxWatchReadyPageChangeDeletionAndRevocation(t *testing.T) {
	r := nha162NewRPCRig(t)
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	home := r.taskHome(t)
	stream, err := r.tasks.WatchTaskLists(ctx, nha162Bearer(&api.WatchTaskListsRequest{Parent: home}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Receive() || stream.Msg().GetReady() == nil || stream.Msg().Sequence != 1 {
		t.Fatalf("ready: %v", stream.Err())
	}
	page, err := r.tasks.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home}, r.adminToken))
	if err != nil || len(page.Msg.TaskLists) != 0 {
		t.Fatalf("initial page: %v", err)
	}
	create := func(label string) *api.TaskList {
		v, e := r.tasks.CreateTaskList(ctx, nha162Bearer(&api.CreateTaskListRequest{Parent: home, TaskList: &api.TaskList{DisplayName: label}}, r.adminToken))
		if e != nil {
			t.Fatal(e)
		}
		return v.Msg
	}
	first := create("Synthetic watched list")
	if !stream.Receive() || stream.Msg().GetChanged().GetTaskList() != first.Name || stream.Msg().Sequence != 2 {
		t.Fatalf("create invalidation: %v", stream.Err())
	}
	page, err = r.tasks.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home}, r.adminToken))
	if err != nil || len(page.Msg.TaskLists) != 1 || page.Msg.TaskLists[0].Name != first.Name {
		t.Fatalf("reload created: %v", err)
	}
	_, err = r.tasks.DeleteTaskList(ctx, nha162Bearer(&api.DeleteTaskListRequest{Name: first.Name, Etag: first.Etag}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || stream.Msg().GetChanged().GetTaskList() != first.Name || stream.Msg().Sequence != 3 {
		t.Fatalf("delete invalidation: %v", stream.Err())
	}
	page, err = r.tasks.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home}, r.adminToken))
	if err != nil || len(page.Msg.TaskLists) != 0 {
		t.Fatalf("reload deleted: %v", err)
	}
	target := create("Synthetic revocation target")
	const password = "NHA162 synthetic watcher password canary"
	account, err := r.accounts.CreateAccount(ctx, nha162Bearer(&api.CreateAccountRequest{Parent: r.taskHome(t), Account: &api.Account{LoginName: "NHA162 Watcher", DisplayName: "NHA162 Watcher", InitialPassword: password, Role: api.AccountRole_ACCOUNT_ROLE_USER}}, r.adminToken))
	if err != nil {
		t.Fatal(err)
	}
	userID := nha203NativeAccount(t, ctx, r.store, account.Msg.Name)
	name, err := tasknames.ParseList(target.Name)
	if err != nil {
		t.Fatal(err)
	}
	nativeID, err := name.ID().Value()
	if err != nil {
		t.Fatal(err)
	}
	nha162SetPolicy(t, r, userID, fmt.Sprintf("  - scope: {family: lists, operation: read}\n    objects: {lists: {explicit: [%q]}}\n", nativeID))
	token := r.login(t, "NHA162 Watcher", password)
	watcher, err := r.tasks.WatchTaskLists(ctx, nha162Bearer(&api.WatchTaskListsRequest{Parent: home, TaskList: &target.Name}, token))
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if !watcher.Receive() || watcher.Msg().GetReady() == nil {
		t.Fatalf("granted watch: %v", watcher.Err())
	}
	if _, err = r.tasks.GetTaskList(ctx, nha162Bearer(&api.GetTaskListRequest{Name: target.Name}, token)); err != nil {
		t.Fatal(err)
	}
	nha162SetPolicy(t, r, userID, "  - scope: {family: lists, operation: write}\n    objects: {lists: {all: {}}}\n")
	if watcher.Receive() || watcher.Err() == nil {
		t.Fatalf("revoked watch remained open: %v", watcher.Err())
	}
	_, err = r.tasks.GetTaskList(ctx, nha162Bearer(&api.GetTaskListRequest{Name: target.Name}, token))
	nha162Code(t, err, connect.CodePermissionDenied)
}
