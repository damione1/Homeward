package contracts

import (
	"testing"

	wire "github.com/damione1/Homeward/gen/home/configurationplan/v1"
	"google.golang.org/protobuf/proto"
)

func TestNHA053RemoteCalendarDiffUsesClosedPublicOperationalMetadata(t *testing.T) {
	plan := nha053ContractPlan()
	plan.Family = wire.SourceFamily_SOURCE_FAMILY_OPERATIONAL
	plan.Identity = "operational.main"
	plan.SchemaVersion = "homeward.operational/1"
	plan.Effect = wire.Effect_EFFECT_CONFIGURATION
	plan.Changes = []*wire.Change{{Effect: wire.Effect_EFFECT_CONFIGURATION, Operation: wire.ChangeOperation_CHANGE_OPERATION_REPLACED, Field: "remote_calendar"}}
	if err := AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: plan}); err != nil {
		t.Fatal("delivered101 operational field refused at shared contract", err)
	}
	for name, mutate := range map[string]func(*wire.Plan){"raw-url-path": func(p *wire.Plan) { p.Changes[0].Field = "remote_calendar.url" }, "raw-handle-path": func(p *wire.Plan) { p.Changes[0].Field = "remote_calendar.auth.bearer_secret" }, "wrong-family-effect": func(p *wire.Plan) {
		p.Effect = wire.Effect_EFFECT_DASHBOARD
		p.Changes[0].Effect = wire.Effect_EFFECT_DASHBOARD
	}, "wrong-source-family": func(p *wire.Plan) {
		p.Family = wire.SourceFamily_SOURCE_FAMILY_DASHBOARD
		p.Identity = "dashboard.preview"
		p.SchemaVersion = "homeward-dashboard-1"
		p.Effect = wire.Effect_EFFECT_DASHBOARD
		p.Changes[0].Effect = wire.Effect_EFFECT_DASHBOARD
	}, "invented-apply": func(p *wire.Plan) { p.ApplicationAvailable = true }} {
		t.Run(name, func(t *testing.T) {
			copy := proto.Clone(plan).(*wire.Plan)
			mutate(copy)
			if err := AdmitConfigurationPlanResponse(&wire.BuildPlanResponse{Plan: copy}); err == nil {
				t.Fatal("private source path or invented effect/application admitted")
			}
		})
	}
}
