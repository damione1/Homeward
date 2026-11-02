package server

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	auth "github.com/damione1/Homeward/gen/home/auth/v1"
	"github.com/damione1/Homeward/gen/home/auth/v1/authv1connect"
	configuration "github.com/damione1/Homeward/gen/home/configuration/v1"
	applywire "github.com/damione1/Homeward/gen/home/configurationapply/v1"
	planwire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/gen/home/dashboard/v1/dashboardv1connect"
	statewire "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/gen/home/state/v1/statev1connect"
)

func dashboardWidgetsSupport() *wire.ClientLayoutSupport {
	return &wire.ClientLayoutSupport{Profile: imported.DashboardProfile, Version: 3, Kinds: []wire.LayoutKind{wire.LayoutKind_LAYOUT_KIND_VIEW, wire.LayoutKind_LAYOUT_KIND_SECTION, wire.LayoutKind_LAYOUT_KIND_WIDGET, wire.LayoutKind_LAYOUT_KIND_HEADING, wire.LayoutKind_LAYOUT_KIND_GRID, wire.LayoutKind_LAYOUT_KIND_HORIZONTAL_STACK, wire.LayoutKind_LAYOUT_KIND_VERTICAL_STACK, wire.LayoutKind_LAYOUT_KIND_TILE, wire.LayoutKind_LAYOUT_KIND_RECIPE, wire.LayoutKind_LAYOUT_KIND_POPUP, wire.LayoutKind_LAYOUT_KIND_CHIP_GROUP, wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED}}
}

func TestDashboardWidgetsProductionPublicationWatchAndSourcePrivacy(t *testing.T) {
	f := nha055PublicationStart(t)
	httpClient := nha162RunClient(t, f.roots, f.run.address)
	client := dashboardv1connect.NewDashboardServiceClient(httpClient, "https://"+f.run.address.String())
	accounts := authv1connect.NewAccountServiceClient(httpClient, "https://"+f.run.address.String())
	nha162RunPolicy(t, f.ctx, f.policies, f.tokens.Session.AccountId, f.token, "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
	if _, err := client.ListDashboards(f.ctx, connect.NewRequest(&wire.ListDashboardsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal("dashboard route lacks native authentication", err)
	}
	empty, err := client.ListDashboards(f.ctx, nha056SessionRequest(&wire.ListDashboardsRequest{Support: dashboardWidgetsSupport()}, f.token))
	if err != nil || len(empty.Msg.Dashboards) != 0 || empty.Msg.ResumeToken == "" {
		t.Fatal("an unconfigured dashboard must remain an empty catalogue", err)
	}
	var previous *configuration.DocumentVersion
	var previousRaw []byte
	apply := func(label string) {
		t.Helper()
		raw := []byte(fmt.Sprintf("homeward: {profile: homeward-dashboard-3, owner: %q, url_path: configured}\nviews:\n- type: sections\n  title: %s\n  path: home\n  sections:\n  - type: grid\n    cards:\n    - type: custom:homeward-tasks\n      title: %s\n      list_id: isolated-list\n      limit: 2\n", f.tokens.Session.AccountId.Value, label, label))
		candidate := &planwire.Candidate{Family: planwire.SourceFamily_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.production-transport", Entry: "dashboard.yaml", Files: []*planwire.SourceFile{{LogicalPath: "dashboard.yaml", ExactSource: raw}}}
		plan := f.build(t, candidate)
		request := nha055Request(f.read(t), candidate, &plan.Receipt)
		request.Sources = []*applywire.SourcePrecondition{nha055PublicationExpected("dashboard.production-transport", "dashboard.yaml", previous, previousRaw)}
		response, err := f.apply.Apply(f.ctx, nha056SessionRequest(request, f.token))
		if err != nil || response.Msg.Activation.Outcome != configuration.ActivationOutcome_ACTIVATION_OUTCOME_PUBLISHED {
			t.Fatal("production source publication", err)
		}
		read, err := f.api.ReadDocument(f.ctx, nha056SessionRequest(&configuration.ReadDocumentRequest{Id: &configuration.DocumentId{Value: "dashboard.production-transport"}}, f.token))
		if err != nil {
			t.Fatal(err)
		}
		previous, previousRaw = read.Msg.Document.Version, raw
	}
	apply("Configuration A")
	list, err := client.ListDashboards(f.ctx, nha056SessionRequest(&wire.ListDashboardsRequest{Support: dashboardWidgetsSupport()}, f.token))
	if err != nil || len(list.Msg.Dashboards) != 1 {
		t.Fatal("production List", err)
	}
	first := list.Msg
	get, err := client.GetDashboard(f.ctx, nha056SessionRequest(&wire.GetDashboardRequest{Id: first.Dashboards[0].Id, Support: dashboardWidgetsSupport()}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	if get.Msg.ResumeToken != first.ResumeToken || get.Msg.Dashboard.Definition != nil || get.Msg.Dashboard.NativeLayout.Version != 3 {
		t.Fatal("source exposed or incoherent tree")
	}
	if get.Msg.Dashboard.Metadata.Version.Revision != previous.Revision || !bytes.Equal(get.Msg.Dashboard.Metadata.Version.SourceSha256, previous.SourceSha256) {
		t.Fatal("dashboard metadata substituted the actual SQL source version")
	}
	if get.Msg.Dashboard.Metadata.UpdatedAt.AsTime().Before(time.Now().Add(-time.Hour)) {
		t.Fatal("invented publication timestamp")
	}
	var widget bool
	for _, node := range get.Msg.Dashboard.NativeLayout.Nodes {
		widget = widget || node.GetWidget().GetTasks() != nil
	}
	if !widget {
		t.Fatal("configured widget missing")
	}
	stateAPI := statev1connect.NewStateServiceClient(httpClient, "https://"+f.run.address.String())
	latest := get.Msg
	for _, label := range []string{"Configuration A", "Configuration B"} {
		before := latest
		watchCtx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		changed := make(chan *wire.WatchDashboardsResponse, 1)
		watchError := make(chan error, 1)
		go func() {
			stream, err := client.WatchDashboards(watchCtx, nha056SessionRequest(&wire.WatchDashboardsRequest{ResumeToken: before.ResumeToken}, f.token))
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
			next := frame.GetSnapshot()
			if frame.GetResyncRequired() != nil {
				listed, err := client.ListDashboards(f.ctx, nha056SessionRequest(&wire.ListDashboardsRequest{Support: dashboardWidgetsSupport()}, f.token))
				if err != nil {
					t.Fatal("List after explicit publication resync", err)
				}
				next = listed.Msg
			}
			if next == nil || len(next.Dashboards) != 1 || next.Dashboards[0].Title != label || next.ResumeToken == before.ResumeToken {
				t.Fatal("watch missed a publication, including identical source replacement", label)
			}
			response, err := client.GetDashboard(f.ctx, nha056SessionRequest(&wire.GetDashboardRequest{Id: next.Dashboards[0].Id, Support: dashboardWidgetsSupport()}, f.token))
			if err != nil {
				t.Fatal("Get after publication", err)
			}
			latest = response.Msg
			if latest.ResumeToken != next.ResumeToken || latest.Dashboard.NativeOverview.ConfigRevision <= before.Dashboard.NativeOverview.ConfigRevision || latest.Dashboard.Metadata.Version.Revision <= before.Dashboard.Metadata.Version.Revision {
				t.Fatal("same-source activation reused an earlier dashboard cohort", label)
			}
			sameSource := bytes.Equal(latest.Dashboard.Metadata.Version.SourceSha256, before.Dashboard.Metadata.Version.SourceSha256)
			if sameSource != (label == "Configuration A") {
				t.Fatal("source byte identity differs from repeated-A/replacement-B publication", label)
			}
			page, err := stateAPI.OpenView(f.ctx, nha056SessionRequest(&statewire.OpenViewRequest{Filter: &statewire.ViewFilter{}, PageSize: 16}, f.token))
			if err != nil {
				t.Fatal("authorized state snapshot after publication", err)
			}
			cursor, overview := page.Msg.Cursor, latest.Dashboard.NativeOverview
			if cursor == nil || cursor.ConfigRevision != overview.ConfigRevision || !bytes.Equal(cursor.ConfigSha256, overview.ConfigSha256) || cursor.AuthorizationRevision != overview.AuthorizationRevision {
				t.Fatal("dashboard and state snapshot cohorts diverged", label)
			}
			if _, err = stateAPI.ReleaseView(f.ctx, nha056SessionRequest(&statewire.ReleaseViewRequest{SnapshotToken: page.Msg.SnapshotToken}, f.token)); err != nil {
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
	created, err := accounts.CreateAccount(f.ctx, nha056SessionRequest(&auth.CreateAccountRequest{LoginName: "dashboard unrelated user", DisplayName: "Unrelated user", InitialPassword: "Dashboard isolated unrelated password", Role: auth.AccountRole_ACCOUNT_ROLE_USER}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	other := nha162RunLogin(t, f.ctx, f.authentication, "dashboard unrelated user", "Dashboard isolated unrelated password")
	current, err := f.policies.GetPolicy(f.ctx, nha056SessionRequest(&auth.GetPolicyRequest{AccountId: created.Msg.Account.Id}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	policy := fmt.Sprintf("schema: homeward.authorization/1\naccount_id: %q\ngrants:\n- scope: {family: dashboards, operation: read}\n  objects: {dashboards: {own: {}}}\n", created.Msg.Account.Id.Value)
	_, err = f.policies.ReplacePolicy(f.ctx, nha056SessionRequest(&auth.ReplacePolicyRequest{AccountId: created.Msg.Account.Id, YamlSource: []byte(policy), Precondition: &auth.ReplacePolicyRequest_Expected{Expected: current.Msg.GetPresent().Version}}, f.token))
	if err != nil {
		t.Fatal(err)
	}
	denied, err := client.ListDashboards(f.ctx, nha056SessionRequest(&wire.ListDashboardsRequest{Support: dashboardWidgetsSupport()}, other.AccessToken))
	if err != nil || len(denied.Msg.Dashboards) != 0 {
		t.Fatal("request principal changed trusted ownership", err)
	}
	_, err = client.GetDashboard(f.ctx, nha056SessionRequest(&wire.GetDashboardRequest{Id: first.Dashboards[0].Id, Support: dashboardWidgetsSupport()}, other.AccessToken))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal("unauthorized dashboard existence disclosed", err)
	}
}
