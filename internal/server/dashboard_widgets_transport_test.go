package server

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	applywire "github.com/damione1/Homeward/gen/home/api/v2"
	configuration "github.com/damione1/Homeward/gen/home/api/v2"
	planwire "github.com/damione1/Homeward/gen/home/api/v2"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	dashboardv1connect "github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
)

func TestDashboardWidgetsProductionPublicationWatchAndSourcePrivacy(t *testing.T) {
	f := nha055PublicationStart(t)
	httpClient := nha162RunClient(t, f.roots, f.run.address)
	client := dashboardv1connect.NewDashboardServiceClient(httpClient, "https://"+f.run.address.String())
	accounts := apiv2connect.NewAccountServiceClient(httpClient, "https://"+f.run.address.String())
	nha162RunPolicy(t, f.ctx, f.policies, nha203SessionAccount(t, f.tokens.Session).String(), f.token, "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
	if _, err := client.ListDashboards(f.ctx, connect.NewRequest(&wire.ListDashboardsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("dashboard route lacks native authentication", err)
	}
	empty, err := client.ListDashboards(f.ctx, nha056SessionRequest(&wire.ListDashboardsRequest{Parent: f.home}, f.token))
	if err != nil || len(empty.Msg.Dashboards) != 0 || empty.Msg.ResumeToken == "" {
		t.Fatal("an unconfigured dashboard must remain an empty catalogue", err)
	}
	var previous *configuration.DocumentVersion
	var previousRaw []byte
	apply := func(label string) {
		t.Helper()
		raw := []byte(fmt.Sprintf("homeward: {profile: homeward-dashboard-3, owner: %q, url_path: configured}\nviews:\n- type: sections\n  title: %s\n  path: home\n  sections:\n  - type: grid\n    cards:\n    - type: custom:homeward-tasks\n      title: %s\n      list_id: isolated-list\n      limit: 2\n", nha203AccountText(t, nha203NativeAccount(t, f.ctx, f.run.observed.sessions.store, nha203SessionAccount(t, f.tokens.Session).String())), label, label))
		candidate := &planwire.ConfigurationPlanCandidate{Family: planwire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.production-transport", Entry: "dashboard.yaml", Files: []*planwire.ConfigurationPlanSourceFile{{LogicalPath: "dashboard.yaml", ExactSource: raw}}}
		plan := f.build(t, candidate)
		request := nha055Request(f.read(t), candidate, plan)
		request.Sources = []*applywire.SourcePrecondition{nha055PublicationExpected("dashboard.production-transport", "dashboard.yaml", previous, previousRaw)}
		response, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
		if err != nil || !response.Msg.Done {
			t.Fatal("production source publication", err)
		}
		read, err := f.api.GetConfigurationDocument(f.ctx, nha056SessionRequest(&configuration.GetConfigurationDocumentRequest{Name: nha203DocumentName(t, f.ctx, f.run, "dashboard.production-transport")}, f.token))
		if err != nil {
			t.Fatal(err)
		}
		previous, previousRaw = read.Msg.Version, raw
	}
	apply("Configuration A")
	list, err := client.ListDashboards(f.ctx, nha056SessionRequest(&wire.ListDashboardsRequest{Parent: f.home}, f.token))
	if err != nil || len(list.Msg.Dashboards) != 1 {
		t.Fatal("production List", err)
	}
	first := list.Msg
	get, err := client.GetDashboard(f.ctx, nha056SessionRequest(&wire.GetDashboardRequest{Name: first.Dashboards[0].Name}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	if get.Msg.Etag == "" || get.Msg.Layout == nil {
		t.Fatal("missing canonical dashboard representation")
	}
	if get.Msg.UpdateTime == nil || get.Msg.UpdateTime.AsTime().Before(time.Now().Add(-time.Hour)) {
		t.Fatal("invented publication timestamp")
	}
	if len(get.Msg.Layout.Nodes) == 0 {
		t.Fatal("configured layout missing")
	}
	homeAPI := apiv2connect.NewHomeServiceClient(httpClient, "https://"+f.run.address.String())
	viewAPI := apiv2connect.NewEntityViewServiceClient(httpClient, "https://"+f.run.address.String())
	homes, err := homeAPI.ListHomes(f.ctx, nha056SessionRequest(&api.ListHomesRequest{PageSize: 10}, f.token))
	if err != nil || len(homes.Msg.GetHomes()) != 1 {
		t.Fatal("authorized home discovery before publication loop", err)
	}
	homeName := homes.Msg.GetHomes()[0].GetName()
	latest := get.Msg
	resume := first.ResumeToken
	for _, label := range []string{"Configuration A", "Configuration B"} {
		before := latest
		beforeVersion := previous.Revision
		beforeRaw := bytes.Clone(previousRaw)
		beforeResume := resume
		watchCtx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		changed := make(chan *wire.WatchDashboardsResponse, 1)
		watchError := make(chan error, 1)
		go func() {
			stream, err := client.WatchDashboards(watchCtx, nha056SessionRequest(&wire.WatchDashboardsRequest{Parent: f.home, ResumeToken: beforeResume}, f.token))
			if err != nil {
				watchError <- err
				return
			}
			defer stream.Close()
			if !stream.Receive() {
				watchError <- stream.Err()
				return
			}
			changed <- stream.Msg()
		}()
		apply(label)
		select {
		case frame := <-changed:
			if !frame.Invalidated {
				t.Fatal("publication did not invalidate the catalogue")
			}
			listed, err := client.ListDashboards(f.ctx, nha056SessionRequest(&wire.ListDashboardsRequest{Parent: f.home}, f.token))
			if err != nil || len(listed.Msg.Dashboards) != 1 || listed.Msg.NextPageToken != "" {
				t.Fatal("List after publication invalidation", err)
			}
			next := listed.Msg
			response, err := client.GetDashboard(f.ctx, nha056SessionRequest(&wire.GetDashboardRequest{Name: next.Dashboards[0].Name}, f.token))
			if err != nil {
				t.Fatal("Get after publication", err)
			}
			latest, resume = response.Msg, next.ResumeToken
			if resume == "" || resume == beforeResume || latest.Etag == before.Etag || previous.Revision <= beforeVersion {
				t.Fatal("same-source activation reused an earlier dashboard cohort", label)
			}
			if bytes.Equal(previousRaw, beforeRaw) != (label == "Configuration A") {
				t.Fatal("source byte identity differs from repeated-A/replacement-B publication", label)
			}
			createdView, err := viewAPI.CreateEntityView(f.ctx, nha056SessionRequest(&api.CreateEntityViewRequest{Parent: homeName, EntityView: &api.EntityView{}}, f.token))
			if err != nil {
				t.Fatal("authorized entity view capture after publication", err)
			}
			page, err := viewAPI.QueryEntityView(f.ctx, nha056SessionRequest(&api.QueryEntityViewRequest{Name: createdView.Msg.GetName(), PageSize: 16}, f.token))
			if err != nil || page.Msg.GetResumeToken() == "" {
				t.Fatal("authorized entity view query after publication", err)
			}
			if _, err = viewAPI.DeleteEntityView(f.ctx, nha056SessionRequest(&api.DeleteEntityViewRequest{Name: createdView.Msg.GetName()}, f.token)); err != nil {
				t.Fatal(err)
			}
		case err := <-watchError:
			cancel()
			t.Fatal(err)
		case <-watchCtx.Done():
			cancel()
			t.Fatal("watch missed publication", label)
		}
		cancel()
	}

	// A user with an own-dashboard policy must not become the owner merely
	// by asking for an administrator-authored dashboard.
	created, err := accounts.CreateAccount(f.ctx, nha056SessionRequest(&api.CreateAccountRequest{Parent: nha203SessionAccount(t, f.tokens.Session).Home().String(), Account: &api.Account{LoginName: "dashboard unrelated user", DisplayName: "Unrelated user", InitialPassword: "Dashboard isolated unrelated password", Role: api.AccountRole_ACCOUNT_ROLE_USER}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	other := nha162RunLogin(t, f.ctx, f.authentication, "dashboard unrelated user", "Dashboard isolated unrelated password")
	current, err := f.policies.GetAuthorizationPolicy(f.ctx, nha056SessionRequest(&api.GetAuthorizationPolicyRequest{Name: created.Msg.Name + "/authorizationPolicy"}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	policy := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n- scope: {family: dashboards, operation: read}\n  objects: {dashboards: {own: {}}}\n", nha203AccountText(t, nha203PolicyAccount(t, current.Msg.YamlSource)))
	_, err = f.policies.ReplaceAuthorizationPolicy(f.ctx, nha056SessionRequest(&api.ReplaceAuthorizationPolicyRequest{Name: created.Msg.Name + "/authorizationPolicy", YamlSource: []byte(policy), Precondition: &api.ReplaceAuthorizationPolicyRequest_ExpectedEtag{ExpectedEtag: current.Msg.Etag}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	denied, err := client.ListDashboards(f.ctx, nha056SessionRequest(&wire.ListDashboardsRequest{Parent: f.home}, other.AccessToken))
	if err != nil || len(denied.Msg.Dashboards) != 0 {
		t.Fatal("request principal changed trusted ownership", err)
	}
	_, err = client.GetDashboard(f.ctx, nha056SessionRequest(&wire.GetDashboardRequest{Name: first.Dashboards[0].Name}, other.AccessToken))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal("unauthorized dashboard existence disclosed", err)
	}
}
