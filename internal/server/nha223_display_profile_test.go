package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/configactivation"
	"github.com/damione1/Homeward/internal/view"
)

// Real default Run, TLS, sessions, publication CAS and SQL replay. No calendar
// provider or physical driver is configured; all labels and zones are synthetic.
func TestNHA223ProductionDisplayProfilePublicationRemovalRollbackAndRestart(t *testing.T) {
	f := nha055PublicationStart(t)
	nha162RunPolicy(t, f.ctx, f.policies, nha203SessionAccount(t, f.tokens.Session).String(), f.token, "  - scope: {family: dashboards, operation: read}\n    objects: {dashboards: {own: {}}}\n  - scope: {family: state_registry, operation: read}\n    objects: {entities: {all: {capabilities: {all: {}}}}}\n")
	actor, err := view.ParseActorID(nha203AccountText(t, nha203NativeAccount(t, f.ctx, f.run.observed.sessions.store, nha203SessionAccount(t, f.tokens.Session).String())))
	if err != nil {
		t.Fatal(err)
	}
	rawDashboard := []byte(fmt.Sprintf("homeward: {profile: homeward-dashboard-3, owner: %q, url_path: configured}\nviews:\n- type: sections\n  title: Synthetic calendar\n  path: home\n  sections: []\n", nha203AccountText(t, nha203NativeAccount(t, f.ctx, f.run.observed.sessions.store, nha203SessionAccount(t, f.tokens.Session).String()))))
	candidate := &api.ConfigurationPlanCandidate{Family: api.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.nha223", Entry: "dashboard.yaml", Files: []*api.ConfigurationPlanSourceFile{{LogicalPath: "dashboard.yaml", ExactSource: rawDashboard}}}
	plan := f.build(t, candidate)
	request := nha055Request(f.read(t), candidate, plan)
	request.Sources = []*api.SourcePrecondition{nha055PublicationExpected("dashboard.nha223", "dashboard.yaml", nil, nil)}
	result, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !result.Msg.Done || result.Msg.GetError() != nil {
		t.Fatal("dashboard publication", err)
	}

	read := func(want string) *api.ListDashboardsResponse {
		t.Helper()
		client := apiv2connect.NewDashboardServiceClient(f.http, f.origin)
		response, err := client.ListDashboards(f.ctx, nha056SessionRequest(&api.ListDashboardsRequest{Parent: f.home}, f.token))
		if err != nil || len(response.Msg.Dashboards) != 1 {
			t.Fatal("production profile catalogue", err)
		}
		dashboard := response.Msg.Dashboards[0]
		if dashboard.GetDisplayProfile().GetTimeZone() != want || (dashboard.DisplayProfile == nil) != (want == "") {
			t.Fatal("profile absence/value changed", dashboard.DisplayProfile, want)
		}
		fetched, err := client.GetDashboard(f.ctx, nha056SessionRequest(&api.GetDashboardRequest{Name: dashboard.Name}, f.token))
		if err != nil || fetched.Msg.GetDisplayProfile().GetTimeZone() != want || fetched.Msg.Etag != dashboard.Etag {
			t.Fatal("List/Get profile disagreement", err)
		}
		zone, err := f.owner.DisplayTimeZone(f.ctx, actor)
		if err != nil || zone != want {
			t.Fatal("active owner profile", zone, err)
		}
		calendars := apiv2connect.NewCalendarServiceClient(f.http, f.origin)
		sources, err := calendars.QueryCalendarSources(f.ctx, nha056SessionRequest(&api.QueryCalendarSourcesRequest{Parent: f.home}, f.token))
		if err != nil || len(sources.Msg.CalendarSources) != 0 {
			t.Fatal("profile invented calendar source", err)
		}
		return response.Msg
	}
	original := read("")
	apply := func(zone string) *api.ConfigurationDocument {
		t.Helper()
		before := f.read(t)
		raw := bytes.Clone(f.raw)
		if zone != "" {
			raw = append(raw, []byte("display_profile: {time_zone: "+zone+"}\n")...)
		}
		candidate := nha053WireCandidate(api.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", raw)
		plan := f.build(t, candidate)
		changed, release, err := f.owner.SubscribeDisplayProfileChanges(f.ctx, actor)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		request := nha055Request(before, candidate, plan)
		result, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
		if err != nil || !result.Msg.Done || result.Msg.GetError() != nil {
			t.Fatal("profile publication", err)
		}
		select {
		case <-changed:
		case <-time.After(3 * time.Second):
			t.Fatal("profile subscriber missed native activation")
		}
		if _, err := f.apply.ApplyConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token)); connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal("stale profile CAS admitted", err)
		}
		return f.read(t)
	}
	first := apply("Europe/Paris")
	current := read("Europe/Paris")
	if current.Dashboards[0].Etag == original.Dashboards[0].Etag || current.ResumeToken == original.ResumeToken {
		t.Fatal("profile change retained previous dashboard capture")
	}
	apply("America/New_York")
	read("America/New_York")
	removed := apply("")
	read("")
	restored, err := f.api.RollbackConfigurationDocument(f.ctx, nha056SessionRequest(&api.RollbackConfigurationDocumentRequest{Name: removed.Name, Expected: removed.Version, ExpectedWorkspaceSha256: removed.GetWorkspace().SourceSha256, Historical: first.ActiveGeneration}, f.token))
	if err != nil || !restored.Msg.Done || restored.Msg.GetError() != nil {
		t.Fatal("profile rollback", err)
	}
	after := f.read(t)
	if after.Version.Revision <= removed.Version.Revision || !bytes.Equal(after.Version.SourceSha256, first.Version.SourceSha256) {
		t.Fatal("rollback lost exact source or rewound version")
	}
	read("Europe/Paris")
	old := f.owner
	changed, release, err := old.SubscribeDisplayProfileChanges(f.ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := f.run.join(t); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("profile subscriber missed shutdown")
	}
	if _, err := old.DisplayTimeZone(f.ctx, actor); !errors.Is(err, configactivation.ErrActivationUnavailable) {
		t.Fatal("closed owner served stale profile", err)
	}
	f.startRun(t)
	f.login(t)
	read("Europe/Paris")
}
