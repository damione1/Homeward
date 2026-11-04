package contracts

import (
	"testing"

	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"google.golang.org/protobuf/proto"
)

func TestNHA053RemoteCalendarDiffUsesClosedPublicOperationalMetadata(t *testing.T) {
	plan := nha053ContractPlan()
	plan.Family = wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_OPERATIONAL
	plan.Identity = "operational.main"
	plan.SchemaVersion = "homeward.operational/1"
	plan.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION
	plan.Changes = []*wire.ConfigurationPlanChange{{Effect: wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_CONFIGURATION, Operation: wire.ConfigurationPlanChangeOperation_CONFIGURATION_PLAN_CHANGE_OPERATION_REPLACED, Field: "remote_calendar"}}
	if err := AdmitConfigurationPlanResponse(plan); err != nil {
		t.Fatal("delivered101 operational field refused at shared contract", err)
	}
	for name, mutate := range map[string]func(*wire.ConfigurationPlan){"raw-url-path": func(p *wire.ConfigurationPlan) { p.Changes[0].Field = "remote_calendar.url" }, "raw-handle-path": func(p *wire.ConfigurationPlan) { p.Changes[0].Field = "remote_calendar.auth.bearer_secret" }, "wrong-family-effect": func(p *wire.ConfigurationPlan) {
		p.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD
		p.Changes[0].Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD
	}, "wrong-source-family": func(p *wire.ConfigurationPlan) {
		p.Family = wire.ConfigurationPlanSourceFamily_CONFIGURATION_PLAN_SOURCE_FAMILY_DASHBOARD
		p.Identity = "dashboard.preview"
		p.SchemaVersion = "homeward-dashboard-1"
		p.Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD
		p.Changes[0].Effect = wire.ConfigurationPlanEffect_CONFIGURATION_PLAN_EFFECT_DASHBOARD
	}, "invented-apply": func(p *wire.ConfigurationPlan) { p.ApplicationAvailable = true }} {
		t.Run(name, func(t *testing.T) {
			copy := proto.Clone(plan).(*wire.ConfigurationPlan)
			mutate(copy)
			if err := AdmitConfigurationPlanResponse(copy); err == nil {
				t.Fatal("private source path or invented effect/application admitted")
			}
		})
	}
}
