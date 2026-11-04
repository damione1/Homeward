package dashboard

import (
	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"testing"
)

// Fixtures declare their available native slots explicitly. An alias alone never
// manufactures an interactive binding in the v2 projector.
func apiFixtureProjection(t *testing.T) APIProjection {
	t.Helper()
	home, err := apitypes.ParseHomeName("homes/dashboard-fixture")
	if err != nil {
		t.Fatal(err)
	}
	identity := func(alias domain.HAAlias) domain.EntityID {
		text, err := alias.Value()
		if err != nil {
			t.Fatal(err)
		}
		id, err := domain.ParseEntityID("fixture:" + text)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	return APIProjection{Home: home, Entity: func(alias domain.HAAlias) (apitypes.EntityName, bool) {
		n, e := apiresources.EntityName(home, identity(alias))
		return n, e == nil
	}, Capabilities: func(alias domain.HAAlias) []apiresources.Capability {
		text, _ := alias.Value()
		if text != "light.room" && text != "light.synthetic" && text != "input_boolean.synthetic" {
			return nil
		}
		d := &registry.CapabilityDescription{SchemaId: contracts.SwitchSchema, Major: 1, Endpoint: "power", DisplayName: "Fixture power", Constraints: &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}}
		if text == "light.synthetic" {
			step := 1.0
			d = &registry.CapabilityDescription{SchemaId: contracts.LightSchema, Major: 1, Endpoint: "light", DisplayName: "Fixture dimmer", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true, BrightnessPercent: &registry.NumberBounds{Minimum: 0, Maximum: 100, Step: &step}}}}
		}
		caps, err := apiresources.ProjectCapabilities(home, identity(alias), []*registry.CapabilityDescription{d}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return caps
	}, Dashboard: func(id authorization.DashboardID) (apitypes.DashboardName, bool) {
		n, e := apiresources.DashboardNameForIdentity(home, id)
		return n, e == nil
	}}
}
func projectAPIFixture(t *testing.T, result Result) *api.DashboardLayout {
	t.Helper()
	definition, ok := result.Definition()
	if !ok {
		t.Fatal("missing admitted definition", result.Diagnostics())
	}
	layout, err := ProjectDashboardDefinition(definition, apiFixtureProjection(t))
	if err != nil {
		t.Fatal(err)
	}
	return layout
}
