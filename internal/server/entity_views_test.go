package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/sequencer"
	"github.com/damione1/Homeward/internal/view"
)

func TestNHA201EntityViewsTLSBindsResumeAndCurrentRights(t *testing.T) {
	f := nha165GrantSetup(t)
	home, _ := apitypes.ParseHomeName("homes/synthetic-views")
	metadata, _ := apitypes.NewHomeMetadata(home, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	owner := nha201ProjectionOwner{home: metadata, record: f.record}
	core := &coreRPCHandler{epoch: f.a.source}
	stamp, _ := view.NewConfigStamp(7, [32]byte{1})
	views, err := view.NewService(f.a.source, f.a, nha050ViewConfig{stamp}, sequencer.RealClock{}, core.capabilityProjector(owner, f.a.authority))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHomeResourceHandler(owner, f.a.authority, views)
	if err != nil {
		t.Fatal(err)
	}
	path, rpc := apiv2connect.NewEntityViewServiceHandler(handler)
	mux := http.NewServeMux()
	mux.Handle(path, rpc)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := authorization.WithPrincipal(r.Context(), f.principal)
		if err != nil {
			http.Error(w, "fixture admission", 500)
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(ctx, actorContextKey{}, f.actor)))
	}))
	defer server.Close()
	client := apiv2connect.NewEntityViewServiceClient(server.Client(), server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	created, err := client.CreateEntityView(ctx, connect.NewRequest(&api.CreateEntityViewRequest{Parent: home.String(), EntityViewId: "caller-view", EntityView: &api.EntityView{}}))
	if err != nil {
		t.Fatal(err)
	}
	if created.Msg.Name != home.String()+"/entityViews/caller-view" {
		t.Fatal("client ID ignored", created.Msg.Name)
	}
	_, duplicateErr := client.CreateEntityView(ctx, connect.NewRequest(&api.CreateEntityViewRequest{Parent: home.String(), EntityViewId: "caller-view", EntityView: &api.EntityView{}}))
	if connect.CodeOf(duplicateErr) != connect.CodeAlreadyExists {
		t.Fatal("duplicate changed capture", duplicateErr)
	}
	read, err := client.QueryEntityView(ctx, connect.NewRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageSize: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Msg.Entries) != 1 || read.Msg.NextPageToken != "" || read.Msg.ResumeToken == "" {
		t.Fatal("snapshot incomplete", read.Msg)
	}
	other, err := client.CreateEntityView(ctx, connect.NewRequest(&api.CreateEntityViewRequest{Parent: home.String(), EntityView: &api.EntityView{}}))
	if err != nil {
		t.Fatal(err)
	}
	// A valid signed checkpoint from another capture cannot be transplanted,
	// including two captures with identical selection and credentials.
	stream, err := client.WatchEntityView(ctx, connect.NewRequest(&api.WatchEntityViewRequest{Name: other.Msg.Name, ResumeToken: read.Msg.ResumeToken}))
	if err == nil {
		if stream.Receive() {
			t.Fatal("transferred checkpoint emitted data")
		}
		err = stream.Err()
		stream.Close()
	}
	if err == nil {
		t.Fatal("transferred checkpoint accepted")
	}
	listed, err := client.ListEntityViews(ctx, connect.NewRequest(&api.ListEntityViewsRequest{Parent: home.String(), PageSize: 1}))
	if err != nil || len(listed.Msg.EntityViews) != 1 || listed.Msg.NextPageToken == "" {
		t.Fatal("list first", err)
	}
	tail, err := client.ListEntityViews(ctx, connect.NewRequest(&api.ListEntityViewsRequest{Parent: home.String(), PageSize: 500, PageToken: listed.Msg.NextPageToken}))
	if err != nil || len(tail.Msg.EntityViews) != 1 || tail.Msg.NextPageToken != "" {
		t.Fatal("list tail", err)
	}
	if _, err := client.DeleteEntityView(ctx, connect.NewRequest(&api.DeleteEntityViewRequest{Name: created.Msg.Name})); err != nil {
		t.Fatal(err)
	}
	recreated, err := client.CreateEntityView(ctx, connect.NewRequest(&api.CreateEntityViewRequest{Parent: home.String(), EntityViewId: "caller-view", EntityView: &api.EntityView{}}))
	if err != nil {
		t.Fatal(err)
	}
	if recreated.Msg.Name != created.Msg.Name {
		t.Fatal("recreation did not preserve requested identity")
	}
	stale, err := client.WatchEntityView(ctx, connect.NewRequest(&api.WatchEntityViewRequest{Name: recreated.Msg.Name, ResumeToken: read.Msg.ResumeToken}))
	if err == nil {
		if stale.Receive() {
			t.Fatal("old incarnation checkpoint emitted data")
		}
		err = stale.Err()
		stale.Close()
	}
	if err == nil {
		t.Fatal("old incarnation checkpoint accepted")
	}
	empty, err := authorization.EmptySource(f.account)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.a.authority.Publish(nha164AuthorityState(t, empty, auth.AccountRole_ACCOUNT_ROLE_USER, 2)); err != nil {
		t.Fatal(err)
	}
	_, err = client.QueryEntityView(ctx, connect.NewRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageToken: "malformed", PageSize: -1}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || nha200Info(t, err).Reason != "RESYNC_REQUIRED" {
		t.Fatal("changed capture rights must resync before payload diagnostics", err)
	}
	permission, _ := authorization.NewPermission(auth.ScopeFamily_SCOPE_FAMILY_HISTORY, auth.ScopeOperation_SCOPE_OPERATION_READ)
	scopes, _ := authorization.NewScopeSet([]authorization.Permission{permission})
	key, _ := contracts.ParseMcpKeyID("entity-view-no-read-ceiling")
	scoped, _ := authorization.ScopedPrincipal(f.account, key, scopes)
	deniedContext, _ := authorization.WithPrincipal(context.Background(), scoped)
	deniedContext = context.WithValue(deniedContext, actorContextKey{}, f.actor)
	_, err = handler.QueryEntityView(deniedContext, connect.NewRequest(&api.QueryEntityViewRequest{Name: created.Msg.Name, PageToken: "malformed", PageSize: -1}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || nha200Info(t, err).Reason != "ACCESS_DENIED" {
		t.Fatal("credential ceiling must precede payload diagnostics", err)
	}
}
