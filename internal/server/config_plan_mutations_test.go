package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/gen/home/api/v2/apiv2connect"
	"github.com/damione1/Homeward/internal/apierrors"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
)

func TestNHA203PlanReplayRetainsCaptureWithoutResurrectingExpiredOrRestartedPlan(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	catalog, err := newConfigurationPlanCatalog(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	home, _ := apitypes.ParseHomeName("homes/synthetic")
	binding := nha203PlanBinding(t, 1)
	catalog.mu.Lock()
	record, err := catalog.prepareAddLocked(home, binding, nha203PlanMetadata(), now.Add(5*time.Minute))
	if err != nil {
		catalog.mu.Unlock()
		t.Fatal(err)
	}
	resource := configurationPlanResource(record)
	catalog.pending[record.name.String()] = record.clone()
	data, err := encodeConfigurationPlanMutation(configurationPlanMutationResult{resource: resource, binding: binding})
	if err != nil {
		catalog.mu.Unlock()
		t.Fatal(err)
	}
	replayed, err := catalog.admitPlanReplayLocked(data, binding)
	catalog.mu.Unlock()
	if err != nil || !proto.Equal(resource, replayed) {
		t.Fatal("SQL confirmation did not retain original capture", err)
	}
	if _, err := catalog.get(record.name, binding); err != nil {
		t.Fatal("duplicate returned before pending capture was visible", err)
	}
	now = now.Add(6 * time.Minute)
	catalog.mu.Lock()
	expired, err := catalog.admitPlanReplayLocked(data, binding)
	catalog.mu.Unlock()
	if err != nil || !proto.Equal(resource, expired) {
		t.Fatal("expiry changed deduplicated result", err)
	}
	if _, err := catalog.get(record.name, binding); err == nil {
		t.Fatal("replay resurrected expired native receipt")
	}
	restarted, err := newConfigurationPlanCatalog(func() time.Time { return now.Add(-6 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	restarted.mu.Lock()
	afterRestart, err := restarted.admitPlanReplayLocked(data, binding)
	restarted.mu.Unlock()
	if err != nil || !proto.Equal(resource, afterRestart) {
		t.Fatal("restart changed deduplicated result", err)
	}
	if _, err := restarted.get(record.name, binding); err == nil {
		t.Fatal("replay rebuilt a plan from a prior native receipt epoch")
	}
	if _, err := decodeConfigurationPlanMutation(data, nha203PlanBinding(t, 2)); err == nil {
		t.Fatal("another credential/rights capture read prior plan result")
	}
}

func TestNHA203ConfigurationPlanRequestReplaysOriginalCaptureTLS(t *testing.T) {
	f := nha149StartTLS(t)
	client := apiv2connect.NewConfigurationPlanServiceClient(f.http, f.origin)
	request := &api.CreateConfigurationPlanRequest{
		Parent: f.home, RequestId: "018e363a-8e16-44a4-9aa6-f623ecb0b505",
		ConfigurationPlan: &api.ConfigurationPlan{Candidate: &api.ConfigurationPlanCandidate{
			Family:   api.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL,
			Identity: "operational.main", Entry: "configuration.yaml",
			Files: []*api.ConfigurationPlanSourceFile{{LogicalPath: "configuration.yaml", ExactSource: f.raw}},
		}},
	}
	first, err := client.CreateConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil {
		t.Fatal("native diagnostic failed", err)
	}
	second, err := client.CreateConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !proto.Equal(first.Msg, second.Msg) {
		t.Fatal("plan request reallocated capture or extended native receipt", err)
	}
	changed := proto.Clone(request).(*api.CreateConfigurationPlanRequest)
	changed.ConfigurationPlan.Candidate.Files[0].ExactSource = append(changed.ConfigurationPlan.Candidate.Files[0].ExactSource, []byte("# changed diagnostic intent\n")...)
	if _, err := client.CreateConfigurationPlan(f.ctx, nha056SessionRequest(changed, f.token)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatal("plan request ignored changed fingerprint", err)
	}
	revalidate := &api.RevalidateConfigurationPlanRequest{Name: first.Msg.Name, Candidate: request.ConfigurationPlan.Candidate, Etag: first.Msg.Etag, RequestId: "018e363a-8e16-44a4-9aa6-f623ecb0b506"}
	refreshed, err := client.RevalidateConfigurationPlan(f.ctx, nha056SessionRequest(revalidate, f.token))
	if err != nil {
		t.Fatal("native revalidation failed", err)
	}
	again, err := client.RevalidateConfigurationPlan(f.ctx, nha056SessionRequest(revalidate, f.token))
	if err != nil || !proto.Equal(refreshed.Msg, again.Msg) {
		t.Fatal("revalidation replay depended on replaced catalogue etag", err)
	}
}

func TestNHA203PlanRequestIDRejectsMalformedIdentityBeforeDiagnostic(t *testing.T) {
	raw := &api.CreateConfigurationPlanRequest{RequestId: "not-a-uuid"}
	_, err := configurationPlanMutationContext(context.Background(), apiv2connect.ConfigurationPlanServiceCreateConfigurationPlanProcedure, raw.RequestId, raw)
	var fault apierrors.Fault
	if !errors.As(err, &fault) || fault.Reason() != api.ApiErrorReason_API_ERROR_REASON_VALIDATION_FAILED {
		t.Fatal("missing canonical request identity failure", err)
	}
}
