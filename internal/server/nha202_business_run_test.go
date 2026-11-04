package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	"github.com/damione1/Homeward/internal/testpg"
)

// This exercises Run's real constructors/registrars and durable SQL owners.
// No source factory, synthetic content owner or device is installed.
func TestNHA202DefaultRunServesTaskCalendarAndEmptyMediaResourcesTLS(t *testing.T) {
	cluster := testpg.Start(t)
	dataDir := shortPrivateDataDir(t)
	roots := nha181CertificateFixture(t, dataDir)
	settings := nha181Config(t, cluster.Port, dataDir, "127.0.0.1:0", nha181TLSYAML())
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	run := nha162StartDefaultRun(t, settings.startup)
	client := nha162RunClient(t, roots, run.address)
	url := "https://" + run.address.String()
	auth := authv1connect.NewAuthServiceClient(client, url)
	policies := authv1connect.NewAuthorizationPolicyServiceClient(client, url)
	homes := apiv2connect.NewHomeServiceClient(client, url)
	tasks := apiv2connect.NewTaskServiceClient(client, url)
	calendars := apiv2connect.NewCalendarServiceClient(client, url)
	media := apiv2connect.NewMediaServiceClient(client, url)
	const name, password = "NHA202 Run Admin", "NHA202 synthetic isolated admin password"
	account := nha162RunBootstrap(t, ctx, dataDir, name, password)
	token := nha162RunLogin(t, ctx, auth, name, password).AccessToken
	discovery, err := homes.ListHomes(ctx, nha162Bearer(&api.ListHomesRequest{PageSize: 2}, token))
	if err != nil || len(discovery.Msg.Homes) != 1 || discovery.Msg.NextPageToken != "" {
		t.Fatal("Run home discovery", err)
	}
	home := discovery.Msg.Homes[0].Name
	_, err = tasks.CreateTaskList(ctx, nha162Bearer(&api.CreateTaskListRequest{Parent: home, TaskList: &api.TaskList{DisplayName: "Must need list grant"}}, token))
	nha162Code(t, err, connect.CodePermissionDenied)
	nha162RunPolicy(t, ctx, policies, account, token, nha162All+nhf015AllCalendars+nhf016AllCameras)
	created, err := tasks.CreateTaskList(ctx, nha162Bearer(&api.CreateTaskListRequest{Parent: home, TaskList: &api.TaskList{DisplayName: "Run task resource"}}, token))
	if err != nil {
		t.Fatal("Run TaskService is not active", err)
	}
	watchCtx, stopWatch := context.WithCancel(ctx)
	watch, err := tasks.WatchTaskLists(watchCtx, nha162Bearer(&api.WatchTaskListsRequest{Parent: home}, token))
	if err != nil {
		stopWatch()
		t.Fatal(err)
	}
	if !watch.Receive() || watch.Msg().GetReady() == nil {
		stopWatch()
		watch.Close()
		t.Fatal("Run task ready", watch.Err())
	}
	added, err := tasks.AppendTask(ctx, nha162Bearer(&api.AppendTaskRequest{Name: created.Msg.Name, Etag: created.Msg.Etag, Task: &api.TaskContent{Summary: "Run SQL-owned task"}}, token))
	if err != nil {
		stopWatch()
		watch.Close()
		t.Fatal("Run append", err)
	}
	if !watch.Receive() || watch.Msg().GetChanged() == nil || watch.Msg().GetChanged().TaskList != created.Msg.Name {
		stopWatch()
		watch.Close()
		t.Fatal("Run task change", watch.Err())
	}
	stopWatch()
	watch.Close()
	loaded, err := tasks.GetTask(ctx, nha162Bearer(&api.GetTaskRequest{Name: added.Msg.Task.Name}, token))
	if err != nil || loaded.Msg.Etag != added.Msg.Task.Etag {
		t.Fatal("Run SQL task result", err)
	}
	sources, err := calendars.QueryCalendarSources(ctx, nha162Bearer(&api.QueryCalendarSourcesRequest{Parent: home}, token))
	if err != nil {
		t.Fatal("Run CalendarService is not active", err)
	}
	for _, source := range sources.Msg.CalendarSources {
		if source.Provenance != api.CalendarProvenance_CALENDAR_PROVENANCE_OBSERVED {
			t.Fatal("Run installed synthetic calendar data")
		}
	}
	content, err := media.ListMediaSources(ctx, nha162Bearer(&api.ListMediaSourcesRequest{Parent: home}, token))
	if err != nil || len(content.Msg.MediaSources) != 0 || content.Msg.NextPageToken != "" {
		t.Fatal("Run must declare an empty real content inventory", err)
	}
	if run.observed.lists.resources == nil || run.observed.lists.resources.sessions == nil || run.observed.lists.resources.ownedCalendar != nil {
		t.Fatal("Run did not retain unique real SQL owners or duplicated its remote calendar owner")
	}
	if err := run.join(t); err != nil {
		t.Fatal("Run business owner shutdown", err)
	}
	nha162RunClosed(t, run.observed)
}
