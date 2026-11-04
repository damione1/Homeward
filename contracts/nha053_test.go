package contracts

import (
	"bytes"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
)

func nha053ContractCandidate() *wire.ConfigurationPlanCandidate {
	return &wire.ConfigurationPlanCandidate{Family: wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.preview", Entry: "candidate.yaml", Files: []*wire.ConfigurationPlanSourceFile{{LogicalPath: "candidate.yaml", ExactSource: []byte("views: []\n")}}}
}
func TestNHA053CurrentUnknownInputIsNotAdmitted(t *testing.T) {
	valid := &wire.CreateConfigurationPlanRequest{Parent: "homes/synthetic", ConfigurationPlan: &wire.ConfigurationPlan{Candidate: nha053ContractCandidate()}}
	if e := AdmitConfigurationPlanRequest(valid); e != nil {
		t.Fatal(e)
	}
	future := proto.Clone(valid).(*wire.CreateConfigurationPlanRequest)
	future.ConfigurationPlan.Candidate.Family = wire.ConfigurationPlanSourceFamily(999)
	raw, e := proto.Marshal(future)
	if e != nil {
		t.Fatal(e)
	}
	decoded := new(wire.CreateConfigurationPlanRequest)
	if e := proto.Unmarshal(raw, decoded); e != nil {
		t.Fatal("current unknown wire field failed decoding", e)
	}
	if e := AdmitConfigurationPlanRequest(decoded); e == nil {
		t.Fatal("unknown input silently granted operational admission")
	}
	nested := proto.Clone(valid).(*wire.CreateConfigurationPlanRequest)
	nested.ConfigurationPlan.Candidate.Files[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if e := AdmitConfigurationPlanRequest(nested); e == nil {
		t.Fatal("nested future source field admitted")
	}
	for _, request := range []proto.Message{(*wire.CreateConfigurationPlanRequest)(nil), &wire.CreateConfigurationPlanRequest{}, &wire.RevalidateConfigurationPlanRequest{Candidate: nha053ContractCandidate()}, &wire.ConfigurationPlan{}} {
		if e := AdmitConfigurationPlanRequest(request); e == nil {
			t.Fatal("zero/wrong request admitted")
		}
	}
}
func nha053ContractPlan() *wire.ConfigurationPlan {
	return &wire.ConfigurationPlan{Name: "homes/synthetic/configurationPlans/p-synthetic", CreateTime: timestamppb.New(time.Unix(1700000000, 0)), UpdateTime: timestamppb.New(time.Unix(1700000000, 0)), ExpireTime: timestamppb.New(time.Unix(1700000600, 0)), Etag: "synthetic-plan-etag", State: wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_ADMITTED, Author: "homes/synthetic/accounts/server-admitted-admin", Family: wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.preview", SchemaVersion: "homeward-dashboard-1", SourceSha256: bytes.Repeat([]byte{1}, 32), SemanticSha256: bytes.Repeat([]byte{2}, 32), Base: &wire.ConfigurationPlanBase{Kind: wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_NO_ACTIVE_DOCUMENT}, GenerationRevision: 1, ManifestSha256: bytes.Repeat([]byte{3}, 32), ObservedWorkspaceSha256: bytes.Repeat([]byte{4}, 32), Effect: wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD, Changes: []*wire.ConfigurationPlanChange{{Effect: wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD, Operation: wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_ADDED, Field: "dashboard_views"}}, Receipt: string(bytes.Repeat([]byte{'a'}, 80))}
}
func TestNHA053ResponseProvenanceAndEffectsAreClosed(t *testing.T) {
	positive := nha053ContractPlan()
	if e := AdmitConfigurationPlanResponse(positive); e != nil {
		t.Fatal(e)
	}
	mutations := []func(*wire.ConfigurationPlan){func(p *wire.ConfigurationPlan) { p.ApplicationAvailable = true }, func(p *wire.ConfigurationPlan) { p.State = wire.ConfigurationPlanState(999) }, func(p *wire.ConfigurationPlan) { p.Family = wire.ConfigurationPlanSourceFamily(999) }, func(p *wire.ConfigurationPlan) { p.SchemaVersion = "future/999" }, func(p *wire.ConfigurationPlan) {
		p.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_HUMAN_RIGHTS
	}, func(p *wire.ConfigurationPlan) { p.Changes[0].Field = "secret raw source value" }, func(p *wire.ConfigurationPlan) {
		p.Changes[0].Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_HUMAN_RIGHTS
	}, func(p *wire.ConfigurationPlan) { n := int64(0); p.Base.Revision = &n }, func(p *wire.ConfigurationPlan) {
		p.Base.Kind = wire.ConfigurationPlanBaseKind_CONFIGURATION_PLAN_BASE_KIND_ACTIVE_SQL
	}, func(p *wire.ConfigurationPlan) { p.Base.SourceSha256 = bytes.Repeat([]byte{1}, 32) }, func(p *wire.ConfigurationPlan) { p.SemanticSha256 = nil }, func(p *wire.ConfigurationPlan) { p.Receipt = "" }, func(p *wire.ConfigurationPlan) { p.Base.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) }}
	for i, mutate := range mutations {
		value := proto.Clone(positive).(*wire.ConfigurationPlan)
		mutate(value)
		if e := AdmitConfigurationPlanResponse(value); e == nil {
			t.Fatalf("malformed plan %d admitted", i)
		}
	}
	stale := proto.Clone(positive).(*wire.ConfigurationPlan)
	stale.State = wire.ConfigurationPlanState_CONFIGURATION_PLAN_STATE_STALE
	stale.Receipt = ""
	if e := AdmitConfigurationPlanResponse(stale); e != nil {
		t.Fatal("typed revalidation staleness rejected", e)
	}
}
