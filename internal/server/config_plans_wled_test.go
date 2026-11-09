package server

import (
	"bytes"
	"os"
	"testing"

	"github.com/damione1/Homeward/contracts"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
)

func TestNHA217WLEDPlanNativeTLSResponseAndReplay(t *testing.T) {
	f := nha053StartTLS(t)
	before := f.read(t)
	info, err := os.Stat(f.path)
	if err != nil {
		t.Fatal(err)
	}
	// This declaration is only previewed. No WLED driver or device is started.
	raw := append(bytes.Clone(f.raw), []byte("\nwled:\n  instance: synthetic-wled\n  controllers:\n    - address: 192.168.50.20\n      mac: aabbccddeeff\n      routes:\n        - {alias: light.basement_lights, label: Synthetic strip}\n")...)
	candidate := nha053WireCandidate(wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL, "operational.main", raw)
	request := &wire.CreateConfigurationPlanRequest{Parent: f.home, RequestId: "018e363a-8e16-44a4-9aa6-f623ecb02170", ConfigurationPlan: &wire.ConfigurationPlan{Candidate: candidate}}
	response, err := f.plans.CreateConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil {
		t.Fatal("native WLED preview failed", err)
	}
	plan := response.Msg
	if err := contracts.AdmitConfigurationPlanResponse(plan); err != nil {
		t.Fatal(err)
	}
	if plan.State != wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED || plan.Effect != wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION || len(plan.Changes) != 1 || plan.Changes[0].Field != "wled" || plan.Changes[0].Operation != wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_ADDED {
		t.Fatalf("WLED configuration delta lost: state=%v diagnostics=%v changes=%v", plan.State, plan.Diagnostics, plan.Changes)
	}
	replay, err := f.plans.CreateConfigurationPlan(f.ctx, nha056SessionRequest(request, f.token))
	if err != nil || !proto.Equal(plan, replay.Msg) {
		t.Fatal("persisted result was not replayed exactly", err)
	}
	read, err := f.plans.GetConfigurationPlan(f.ctx, nha056SessionRequest(&wire.GetConfigurationPlanRequest{Name: plan.Name}, f.token))
	if err != nil || !proto.Equal(plan, read.Msg) {
		t.Fatal("published plan cannot be read", err)
	}
	f.unchanged(t, before, f.raw, info)
	// A valid operational field must not become authority in a dashboard family.
	foreign := proto.Clone(plan).(*wire.ConfigurationPlan)
	foreign.Family = wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD
	foreign.SchemaVersion = "homeward-dashboard-1"
	foreign.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD
	foreign.Changes[0].Effect = foreign.Effect
	if contracts.AdmitConfigurationPlanMetadata(foreign) == nil {
		t.Fatal("WLED field admitted outside operational family")
	}
	foreign.Changes[0].Field = "dashboard_views"
	if contracts.AdmitConfigurationPlanMetadata(foreign) != nil {
		t.Fatal("negative family fixture was invalid independently of WLED")
	}
	unknown := proto.Clone(plan).(*wire.ConfigurationPlan)
	unknown.Changes[0].Field = "unregistered_integration"
	if contracts.AdmitConfigurationPlanMetadata(unknown) == nil {
		t.Fatal("unknown integration field admitted")
	}
}
