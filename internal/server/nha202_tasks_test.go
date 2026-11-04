package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/config"
	"github.com/damione1/Homeward/internal/lists"
	datepb "google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type nha202Home struct{ name apitypes.HomeName }

func (h nha202Home) HomeName(context.Context) (apitypes.HomeName, error) { return h.name, nil }
func nha202TaskAPI(t *testing.T) (*nha162RPCRig, apiv2connect.TaskServiceClient, string) {
	t.Helper()
	r := nha162NewRPCRig(t)
	home, _ := apitypes.ParseHomeName("homes/synthetic-nha202")
	cursors, err := lists.NewCursorCodec()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newTaskHandler(r.lists.service, cursors, nha202Home{home})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPublicPlan(nil, nil, accountTransportAudit{store: r.store})
	if err != nil {
		t.Fatal(err)
	}
	if err = registerTaskRoutes(plan, handler); err != nil {
		t.Fatal(err)
	}
	plan, _, err = composeSessionsWithCoordinator(plan, r.store, r.coordinator)
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
	t.Cleanup(server.Close)
	return r, apiv2connect.NewTaskServiceClient(server.Client(), server.URL), home.String()
}
func TestNHA202TaskResourceCRUDMaskCASAndOrderingTLS(t *testing.T) {
	r, client, home := nha202TaskAPI(t)
	ctx, access := r.ctx, r.adminToken
	created, err := client.CreateTaskList(ctx, nha162Bearer(&api.CreateTaskListRequest{Parent: home, TaskList: &api.TaskList{DisplayName: "Synthetic tasks"}}, access))
	if err != nil {
		t.Fatal(err)
	}
	list := created.Msg
	for _, stale := range []string{"different-opaque-token", list.Etag[1 : len(list.Etag)-1]} {
		_, err := client.UpdateTaskList(ctx, nha162Bearer(&api.UpdateTaskListRequest{TaskList: &api.TaskList{Name: list.Name, Etag: stale, DisplayName: "Must not commit"}}, access))
		nha162Code(t, err, connect.CodeAborted)
	}
	response, err := client.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home}, access))
	if err != nil || len(response.Msg.TaskLists) != 1 || response.Msg.TotalSize != 1 {
		t.Fatalf("default page and actual count: %v", err)
	}
	_, err = client.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home, PageSize: -1}, access))
	nha162Code(t, err, connect.CodeInvalidArgument)
	first, err := client.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: list.Name, Etag: list.Etag, Task: &api.TaskContent{Summary: "First", Completed: true}}, access))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: list.Name, Etag: list.Etag, Task: &api.TaskContent{Summary: "Stale collection"}}, access))
	nha162Code(t, err, connect.CodeAborted)
	for _, stale := range []string{"different-opaque-token", first.Msg.Task.Etag[1 : len(first.Msg.Task.Etag)-1]} {
		_, err := client.UpdateTask(ctx, nha162Bearer(&api.UpdateTaskRequest{Task: &api.Task{Name: first.Msg.Task.Name, Etag: stale, Summary: "Must not commit"}}, access))
		nha162Code(t, err, connect.CodeAborted)
	}
	noop, err := client.UpdateTask(ctx, nha162Bearer(&api.UpdateTaskRequest{Task: &api.Task{Name: first.Msg.Task.Name, Etag: first.Msg.Task.Etag, Summary: "Ignored"}, UpdateMask: &fieldmaskpb.FieldMask{}}, access))
	if err != nil || noop.Msg.Etag != first.Msg.Task.Etag || noop.Msg.Summary != "First" {
		t.Fatalf("present empty mask is no-op: %v", err)
	}
	updated, err := client.UpdateTask(ctx, nha162Bearer(&api.UpdateTaskRequest{Task: &api.Task{Name: first.Msg.Task.Name, Etag: first.Msg.Task.Etag, Completed: false}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"completed"}}}, access))
	if err != nil || updated.Msg.Completed || updated.Msg.Summary != "First" || updated.Msg.CompletionTime != nil {
		t.Fatalf("explicit false patch: %v", err)
	}
	_, err = client.UpdateTask(ctx, nha162Bearer(&api.UpdateTaskRequest{Task: &api.Task{Name: first.Msg.Task.Name, Etag: first.Msg.Task.Etag, Summary: "Stale"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"summary"}}}, access))
	nha162Code(t, err, connect.CodeAborted)
	_, err = client.UpdateTask(ctx, nha162Bearer(&api.UpdateTaskRequest{Task: &api.Task{Name: first.Msg.Task.Name, Etag: updated.Msg.Etag}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"unknown"}}}, access))
	nha162Code(t, err, connect.CodeInvalidArgument)
	current, err := client.GetTaskList(ctx, nha162Bearer(&api.GetTaskListRequest{Name: list.Name}, access))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: list.Name, Etag: current.Msg.Etag, Task: &api.TaskContent{Summary: "Second"}}, access))
	if err != nil {
		t.Fatal(err)
	}
	moved, err := client.MoveTask(ctx, nha162Bearer(&api.MoveTaskRequest{Name: second.Msg.Task.Name, Etag: second.Msg.Task.Etag, Destination: &api.MoveTaskRequest_First{First: &emptypb.Empty{}}}, access))
	if err != nil || moved.Msg.Position != 0 {
		t.Fatalf("atomic task move: %v", err)
	}
	page, err := client.ListTasks(ctx, nha162Bearer(&api.ListTasksRequest{Parent: list.Name, PageSize: 1}, access))
	if err != nil || len(page.Msg.Tasks) != 1 || page.Msg.Tasks[0].Name != second.Msg.Task.Name || page.Msg.NextPageToken == "" || page.Msg.TotalSize != 2 {
		t.Fatalf("ordered page: %v", err)
	}
	last, err := client.ListTasks(ctx, nha162Bearer(&api.ListTasksRequest{Parent: list.Name, PageSize: 256, PageToken: page.Msg.NextPageToken}, access))
	if err != nil || len(last.Msg.Tasks) != 1 || last.Msg.NextPageToken != "" {
		t.Fatalf("page size can change: %v", err)
	}
	_, err = client.DeleteTask(ctx, nha162Bearer(&api.DeleteTaskRequest{Name: page.Msg.Tasks[0].Name, Etag: page.Msg.Tasks[0].Etag}, access))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetTask(ctx, nha162Bearer(&api.GetTaskRequest{Name: second.Msg.Task.Name}, access))
	nha162Code(t, err, connect.CodeNotFound)
	current, err = client.GetTaskList(ctx, nha162Bearer(&api.GetTaskListRequest{Name: list.Name}, access))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DeleteTaskList(ctx, nha162Bearer(&api.DeleteTaskListRequest{Name: list.Name, Etag: current.Msg.Etag}, access))
	nha162Code(t, err, connect.CodeFailedPrecondition)
	if _, err = client.GetTask(ctx, nha162Bearer(&api.GetTaskRequest{Name: first.Msg.Task.Name}, access)); err != nil {
		t.Fatal("default delete damaged children", err)
	}
	_, err = client.DeleteTaskList(ctx, nha162Bearer(&api.DeleteTaskListRequest{Name: list.Name, Etag: current.Msg.Etag, Force: true}, access))
	if err != nil {
		t.Fatal("explicit atomic cascade", err)
	}
	_, err = client.GetTask(ctx, nha162Bearer(&api.GetTaskRequest{Name: first.Msg.Task.Name}, access))
	nha162Code(t, err, connect.CodeNotFound)
}

func TestNHA202TaskMutationDedupReplaysExactResultAndRejectsDifferentIntention(t *testing.T) {
	r, client, home := nha202TaskAPI(t)
	ctx, access := r.ctx, r.adminToken
	request := &api.CreateTaskListRequest{Parent: home, TaskList: &api.TaskList{DisplayName: "Idempotent synthetic collection"}, RequestId: "69b5f006-c4ed-4bdf-a744-e4d8f20f34dc"}
	first, err := client.CreateTaskList(ctx, nha162Bearer(request, access))
	if err != nil {
		t.Fatal(err)
	}
	replay, err := client.CreateTaskList(ctx, nha162Bearer(request, access))
	if err != nil || !proto.Equal(first.Msg, replay.Msg) {
		t.Fatalf("same request did not replay exact original resource: %v", err)
	}
	changed := proto.Clone(request).(*api.CreateTaskListRequest)
	changed.TaskList.DisplayName = "Different intention"
	_, err = client.CreateTaskList(ctx, nha162Bearer(changed, access))
	nha162Code(t, err, connect.CodeAlreadyExists)
	ignored := proto.Clone(request).(*api.CreateTaskListRequest)
	ignored.TaskList.Origin = api.TaskListOrigin_TASK_LIST_ORIGIN_SHOPPING_LIST
	replay, err = client.CreateTaskList(ctx, nha162Bearer(ignored, access))
	if err != nil || !proto.Equal(first.Msg, replay.Msg) {
		t.Fatalf("output-only field changed normalized intention: %v", err)
	}
	page, err := client.ListTaskLists(ctx, nha162Bearer(&api.ListTaskListsRequest{Parent: home}, access))
	if err != nil || page.Msg.TotalSize != 1 {
		t.Fatalf("dedup created another collection: %v", err)
	}
	task, err := client.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: first.Msg.Name, Etag: first.Msg.Etag, Task: &api.TaskContent{Summary: "Original", Completed: true}}, access))
	if err != nil {
		t.Fatal(err)
	}
	patch := &api.UpdateTaskRequest{Task: &api.Task{Name: task.Msg.Task.Name, Etag: task.Msg.Task.Etag, Completed: false}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"completed"}}, RequestId: "e5cbafdb-63f6-4aeb-bfa5-6a53b32a5a8e"}
	updated, err := client.UpdateTask(ctx, nha162Bearer(patch, access))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := client.UpdateTask(ctx, nha162Bearer(patch, access))
	if err != nil || !proto.Equal(updated.Msg, repeated.Msg) {
		t.Fatalf("stale original etag was checked before known result replay: %v", err)
	}
	current, err := client.GetTask(ctx, nha162Bearer(&api.GetTaskRequest{Name: task.Msg.Task.Name}, access))
	if err != nil || !proto.Equal(updated.Msg, current.Msg) {
		t.Fatalf("replay mutated current data: %v", err)
	}
}

func TestNHA202TaskOneofMaskPreservesUnselectedCaseTLS(t *testing.T) {
	r, client, home := nha202TaskAPI(t)
	ctx, access := r.ctx, r.adminToken
	list, err := client.CreateTaskList(ctx, nha162Bearer(&api.CreateTaskListRequest{Parent: home, TaskList: &api.TaskList{DisplayName: "Oneof masks"}}, access))
	if err != nil {
		t.Fatal(err)
	}
	date := &datepb.Date{Year: 2026, Month: 11, Day: 3}
	appended, err := client.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: list.Msg.Name, Etag: list.Msg.Etag, Task: &api.TaskContent{Summary: "Preserve civil identity", Due: &api.TaskContent_DueDate{DueDate: date}}}, access))
	if err != nil {
		t.Fatal(err)
	}
	current := appended.Msg.Task
	patch := &api.UpdateTaskRequest{Task: &api.Task{Name: current.Name, Etag: current.Etag, Due: &api.Task_DueDate{DueDate: &datepb.Date{Year: 2026, Month: 11, Day: 4}}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"due_instant"}}}
	preserved, err := client.UpdateTask(ctx, nha162Bearer(patch, access))
	if err != nil || !proto.Equal(preserved.Msg.GetDueDate(), date) {
		t.Fatal("unselected oneof changed", err)
	}
	// A selected missing instant clears only that case, retaining a civil date.
	patch.Task = &api.Task{Name: current.Name, Etag: preserved.Msg.Etag}
	again, err := client.UpdateTask(ctx, nha162Bearer(patch, access))
	if err != nil || !proto.Equal(again.Msg.GetDueDate(), date) {
		t.Fatal("clearing inactive case damaged selected value", err)
	}
	// Selecting both paths replaces/clears the complete oneof atomically.
	patch.Task = &api.Task{Name: current.Name, Etag: again.Msg.Etag}
	patch.UpdateMask.Paths = []string{"due_date", "due_instant"}
	cleared, err := client.UpdateTask(ctx, nha162Bearer(patch, access))
	if err != nil || cleared.Msg.Due != nil {
		t.Fatal("whole oneof clear", err)
	}
}
