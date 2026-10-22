package contracts

import (
	"bytes"
	"testing"

	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"google.golang.org/protobuf/proto"
)

func nha053ContractCandidate() *wire.Candidate {
	return &wire.Candidate{Family: wire.SourceFamily_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.preview", Entry: "candidate.yaml", Files: []*wire.SourceFile{{LogicalPath: "candidate.yaml", ExactSource: []byte("views: []\n")}}}
}
func TestNHA053UnknownWireRemainsDecodableButNotAdmitted(t *testing.T) {
	valid := &wire.BuildPlanRequest{Candidate: nha053ContractCandidate()}
	if e := AdmitConfigurationPlanRequest(valid); e != nil {
		t.Fatal(e)
	}
	future := proto.Clone(valid).(*wire.BuildPlanRequest)
	future.Candidate.Family = wire.SourceFamily(999)
	raw, e := proto.Marshal(future)
	if e != nil {
		t.Fatal(e)
	}
	decoded := new(wire.BuildPlanRequest)
	if e := proto.Unmarshal(raw, decoded); e != nil {
		t.Fatal("compatible wire failed decoding", e)
	}
	if e := AdmitConfigurationPlanRequest(decoded); e == nil {
		t.Fatal("wire compatibility silently granted operational admission")
	}
	nested := proto.Clone(valid).(*wire.BuildPlanRequest)
	nested.Candidate.Files[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	if e := AdmitConfigurationPlanRequest(nested); e == nil {
		t.Fatal("nested future source field admitted")
	}
	for _, request := range []proto.Message{(*wire.BuildPlanRequest)(nil), &wire.BuildPlanRequest{}, &wire.RevalidatePlanRequest{Candidate: nha053ContractCandidate()}, &wire.BuildPlanResponse{}} {
		if e := AdmitConfigurationPlanRequest(request); e == nil {
			t.Fatal("zero/wrong request admitted")
		}
	}
}
func nha053ContractPlan() *wire.Plan {
	return &wire.Plan{State: wire.PlanState_PLAN_STATE_ADMITTED, Author: "server-admitted-admin", Family: wire.SourceFamily_SOURCE_FAMILY_DASHBOARD, Identity: "dashboard.preview", SchemaVersion: "homeward-dashboard-1", SourceSha256: bytes.Repeat([]byte{1}, 32), SemanticSha256: bytes.Repeat([]byte{2}, 32), Base: &wire.Base{Kind: wire.BaseKind_BASE_KIND_NO_ACTIVE_DOCUMENT}, GenerationRevision: 1, ManifestSha256: bytes.Repeat([]byte{3}, 32), ObservedWorkspaceSha256: bytes.Repeat([]byte{4}, 32), Effect: wire.Effect_EFFECT_DASHBOARD, Changes: []*wire.Change{{Effect: wire.Effect_EFFECT_DASHBOARD, Operation: wire.ChangeOperation_CHANGE_OPERATION_ADDED, Field: "dashboard_views"}}, Receipt: string(bytes.Repeat([]byte{'a'}, 80))}
}
func TestNHA053ResponseProvenanceAndEffectsAreClosed(t *testing.T) {
	positive := &wire.BuildPlanResponse{Plan: nha053ContractPlan()}
	if e := AdmitConfigurationPlanResponse(positive); e != nil {
		t.Fatal(e)
	}
	mutations := []func(*wire.Plan){func(p *wire.Plan) { p.ApplicationAvailable = true }, func(p *wire.Plan) { p.State = wire.PlanState(999) }, func(p *wire.Plan) { p.Family = wire.SourceFamily(999) }, func(p *wire.Plan) { p.SchemaVersion = "future/999" }, func(p *wire.Plan) { p.Effect = wire.Effect_EFFECT_HUMAN_RIGHTS }, func(p *wire.Plan) { p.Changes[0].Field = "secret raw source value" }, func(p *wire.Plan) { p.Changes[0].Effect = wire.Effect_EFFECT_HUMAN_RIGHTS }, func(p *wire.Plan) { n := int64(0); p.Base.Revision = &n }, func(p *wire.Plan) { p.Base.Kind = wire.BaseKind_BASE_KIND_ACTIVE_SQL }, func(p *wire.Plan) { p.Base.SourceSha256 = bytes.Repeat([]byte{1}, 32) }, func(p *wire.Plan) { p.SemanticSha256 = nil }, func(p *wire.Plan) { p.Receipt = "" }, func(p *wire.Plan) { p.Base.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01}) }}
	for i, mutate := range mutations {
		value := proto.Clone(positive).(*wire.BuildPlanResponse)
		mutate(value.Plan)
		if e := AdmitConfigurationPlanResponse(value); e == nil {
			t.Fatalf("malformed plan %d admitted", i)
		}
	}
	stale := proto.Clone(positive).(*wire.BuildPlanResponse)
	stale.Plan.State = wire.PlanState_PLAN_STATE_STALE
	stale.Plan.Receipt = ""
	if e := AdmitConfigurationPlanResponse(stale); e == nil {
		t.Fatal("Build invented stale plan")
	}
	if e := AdmitConfigurationPlanResponse(&wire.RevalidatePlanResponse{Plan: stale.Plan}); e != nil {
		t.Fatal("typed revalidation staleness rejected", e)
	}
}
