package server

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"google.golang.org/protobuf/proto"
)

func TestNHA053ActualRunTLSRemoteCalendarPreviewIsPrivateInertAndRevalidatable(t *testing.T) {
	f := nha053StartTLS(t)
	before := f.read(t)
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer provider.Close()
	remote := func(path string) []byte {
		return append(bytes.Clone(raw), []byte(fmt.Sprintf("remote_calendar:\n  label: NHA053_PRIVATE_CALENDAR_LABEL\n  url: %q\n  auth: {bearer_secret: nha053-absent-bearer}\n  view_timezone: America/Toronto\n  network:\n    origin: %q\n    prefixes: ['127.0.0.1/32']\n", provider.URL+path+"?private=NHA053_PRIVATE_QUERY", provider.URL))...)
	}
	first := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", remote("/a.ics"))
	a := f.build(t, first)
	second := nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", remote("/b.ics"))
	b := f.build(t, second)
	for _, plan := range []*wire.Plan{a, b} {
		if plan.State != wire.PlanState_PLAN_STATE_ADMITTED || plan.Effect != wire.Effect_EFFECT_CONFIGURATION || len(plan.Changes) != 1 || plan.Changes[0].Field != "remote_calendar" || plan.Changes[0].ReferencesAdded != 1 || plan.ApplicationAvailable {
			t.Fatal("real remote preview lost operational effect, fixed reference or inert outcome")
		}
		encoded, err := proto.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range []string{"NHA053_PRIVATE_CALENDAR_LABEL", "NHA053_PRIVATE_QUERY", "nha053-absent-bearer", provider.URL} {
			if bytes.Contains(encoded, []byte(canary)) {
				t.Fatal("public plan/receipt exposed remote private settings")
			}
		}
	}
	if bytes.Equal(a.SourceSha256, b.SourceSha256) || bytes.Equal(a.SemanticSha256, b.SemanticSha256) {
		t.Fatal("actual resource variation did not change both exact and semantic hashes")
	}
	fresh, err := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: first, Receipt: a.Receipt}, f.token))
	if err != nil || fresh.Msg.Plan.State != wire.PlanState_PLAN_STATE_ADMITTED {
		t.Fatal("inert remote plan could not revalidate", err)
	}
	if _, err := f.plans.RevalidatePlan(f.ctx, nha056SessionRequest(&wire.RevalidatePlanRequest{Candidate: second, Receipt: a.Receipt}, f.token)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal("remote receipt authenticated another resource", err)
	}
	secret := append(bytes.Clone(raw), []byte("remote_calendar:\n  label: NHA053_PRIVATE_OPAQUE_LABEL\n  secret_url: nha053-absent-url\n  auth: {bearer_secret: nha053-absent-bearer}\n  view_timezone: America/Toronto\n")...)
	opaque := f.build(t, nha053WireCandidate(wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL, "operational.main", secret))
	if opaque.State != wire.PlanState_PLAN_STATE_ADMITTED {
		t.Fatal("preview tried to resolve an absent opaque secret instead of parsing its handle")
	}
	encodedOpaque, err := proto.Marshal(opaque)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"NHA053_PRIVATE_OPAQUE_LABEL", "nha053-absent-url", "nha053-absent-bearer"} {
		if bytes.Contains(encodedOpaque, []byte(canary)) {
			t.Fatal("opaque source handle or label leaked in public preview")
		}
	}

	f.unchanged(t, before, raw, info)
	if requests.Load() != 0 {
		t.Fatal("Run preview/revalidation started HTTP/runtime source activity")
	}
}
