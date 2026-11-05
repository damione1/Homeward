package dashboard

import (
	"strings"
	"testing"

	"github.com/damione1/Homeward/contracts"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

const nha207RoomSource = `homeward: {profile: homeward-dashboard-3, owner: dashboard-owner, url_path: configured}
views:
- type: sections
  path: home
  title: Synthetic home
  sections:
  - type: grid
    cards:
    - type: custom:homeward-room
      title: Synthetic room
      room_id: synthetic-room
      temperature: {entity: climate.synthetic, field: current_temperature_celsius}
      activity_entities: [light.missing, light.synthetic]
      active_states: ['on']
      action: /configured/home#synthetic-room
    - type: custom:bubble-card
      card_type: pop-up
      hash: '#synthetic-room'
      name: Synthetic details
      cards: []
`

func nha207Projection(t *testing.T) APIProjection {
	t.Helper()
	p := apiFixtureProjection(t)
	base := p.Capabilities
	p.Capabilities = func(alias domain.HAAlias) []apiresources.Capability {
		text, _ := alias.Value()
		if text != "climate.synthetic" {
			return base(alias)
		}
		entity, _ := domain.ParseEntityID("fixture:" + text)
		d := &registry.CapabilityDescription{SchemaId: contracts.ClimateSchema, Major: 1, Endpoint: "climate", DisplayName: "Synthetic thermostat", Constraints: &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{
			ObservedTemperatureCelsius: &registry.ClimateTemperatureObservation{Bounds: &registry.NumberBounds{Minimum: -50, Maximum: 100}},
			ObservedModeOptions:        []registry.ClimateMode{registry.ClimateMode_CLIMATE_MODE_OFF, registry.ClimateMode_CLIMATE_MODE_HEAT, registry.ClimateMode_CLIMATE_MODE_AUTO},
		}}}
		caps, err := apiresources.ProjectCapabilities(p.Home, entity, []*registry.CapabilityDescription{d}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return caps
	}
	return p
}

func nha207Room(t *testing.T, source string, projection APIProjection) *api.DashboardLayoutNode {
	t.Helper()
	result := compileWidgets(t, source)
	definition, ok := result.Definition()
	if !ok {
		t.Fatal(result.Diagnostics())
	}
	layout, err := ProjectDashboardDefinition(definition, projection)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range layout.Nodes {
		if node.Source.GetVariant() == "custom:homeward-room" {
			return node
		}
	}
	t.Fatal("room node missing")
	return nil
}

func TestNHA207RoomPreservesIndependentObservationsAndPopup(t *testing.T) {
	p := nha207Projection(t)
	node := nha207Room(t, nha207RoomSource, p)
	w := node.GetWidget()
	room := w.GetRoom()
	if room == nil || room.Temperature == nil || len(room.Activity) != 1 || w.Action.GetPresentation() != api.DashboardPresentation_DASHBOARD_PRESENTATION_POPUP || w.Action.NodeId != "$.views[0].sections[0].cards[1]" {
		t.Fatal("missing optional activity erased independent content or popup", node)
	}
	if strings.Contains(room.Activity[0].Field.Capability, "missing") {
		t.Fatal("fabricated binding")
	}
	before := proto.Clone(node)
	room.Temperature.Capability = "changed-copy"
	if !proto.Equal(before, nha207Room(t, nha207RoomSource, p)) {
		t.Fatal("projection mutated admitted source")
	}
}

func TestNHA207RoomMissingOrDeniedObservationsNeverGrantBindings(t *testing.T) {
	for _, mode := range []string{"missing-slots", "denied-entities", "cross-home"} {
		t.Run(mode, func(t *testing.T) {
			p := nha207Projection(t)
			switch mode {
			case "missing-slots":
				p.Capabilities = func(domain.HAAlias) []apiresources.Capability { return nil }
			case "denied-entities":
				p.Entity = func(domain.HAAlias) (apitypes.EntityName, bool) { return apitypes.EntityName{}, false }
			case "cross-home":
				p.Entity = func(domain.HAAlias) (apitypes.EntityName, bool) {
					n, _ := apitypes.ParseEntityName("homes/other/entities/other")
					return n, true
				}
			}
			w := nha207Room(t, nha207RoomSource, p).GetWidget()
			if w.GetRoom() == nil || w.GetRoom().Temperature != nil || len(w.GetRoom().Activity) != 0 || w.Action == nil {
				t.Fatal("missing observations invented authority or erased authorized navigation")
			}
		})
	}
	p := nha207Projection(t)
	p.Dashboard = func(authorization.DashboardID) (apitypes.DashboardName, bool) { return apitypes.DashboardName{}, false }
	if nha207Room(t, nha207RoomSource, p).GetUnsupported() == nil {
		t.Fatal("denied destination retained")
	}
	if nha207Room(t, strings.Replace(nha207RoomSource, "action: /configured/home#synthetic-room", "action: /configured/home#missing", 1), nha207Projection(t)).GetUnsupported() == nil {
		t.Fatal("unknown destination retained")
	}
}

func TestNHA207RoomThermalSourceLiteralRemainsClosed(t *testing.T) {
	source := strings.Replace(nha207RoomSource, "activity_entities: [light.missing, light.synthetic]", "activity_entities: [climate.synthetic]", 1)
	source = strings.Replace(source, "active_states: ['on']", "active_states: ['heat_cool']", 1)
	room := nha207Room(t, source, nha207Projection(t)).GetWidget().GetRoom()
	if room == nil || len(room.Activity) != 1 || len(room.Activity[0].ActiveValues) != 1 || room.Activity[0].ActiveValues[0].GetTemperatureMode() != api.TemperatureMode_TEMPERATURE_MODE_AUTO {
		t.Fatal("HA heat/cool source spelling lost")
	}
	if nha207Room(t, strings.Replace(source, "heat_cool", "invented_mode", 1), nha207Projection(t)).GetUnsupported() == nil {
		t.Fatal("unknown literal inferred a mode")
	}
	if nha207Room(t, strings.Replace(nha207RoomSource, "['on']", "['heat_cool']", 1), nha207Projection(t)).GetUnsupported() == nil {
		t.Fatal("thermal spelling accepted for power trait")
	}
}

func TestNHA207ActivityAliasesNormalizeToUniqueTypedValues(t *testing.T) {
	for _, test := range []struct {
		name, entities, values string
		want                   []*api.DashboardLiteral
	}{
		{"thermal-aliases", "[climate.synthetic]", "['heat', 'cool', 'auto', 'heat_cool']", []*api.DashboardLiteral{
			{Value: &api.DashboardLiteral_TemperatureMode{TemperatureMode: api.TemperatureMode_TEMPERATURE_MODE_HEAT}},
			{Value: &api.DashboardLiteral_TemperatureMode{TemperatureMode: api.TemperatureMode_TEMPERATURE_MODE_COOL}},
			{Value: &api.DashboardLiteral_TemperatureMode{TemperatureMode: api.TemperatureMode_TEMPERATURE_MODE_AUTO}},
		}},
		{"power-aliases", "[light.synthetic]", "['on', 'true', 'off', 'false']", []*api.DashboardLiteral{
			{Value: &api.DashboardLiteral_PowerValue{PowerValue: api.PowerValue_POWER_VALUE_ON}},
			{Value: &api.DashboardLiteral_PowerValue{PowerValue: api.PowerValue_POWER_VALUE_OFF}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(nha207RoomSource, "activity_entities: [light.missing, light.synthetic]", "activity_entities: "+test.entities, 1)
			source = strings.Replace(source, "active_states: ['on']", "active_states: "+test.values, 1)
			room := nha207Room(t, source, nha207Projection(t)).GetWidget().GetRoom()
			if room == nil || len(room.Activity) != 1 || len(room.Activity[0].ActiveValues) != len(test.want) {
				t.Fatal("activity normalization did not produce a unique closed set", room)
			}
			for i, value := range room.Activity[0].ActiveValues {
				if !proto.Equal(value, test.want[i]) {
					t.Fatal("first semantic value order changed")
				}
			}
			invalid := strings.Replace(source, test.values, strings.TrimSuffix(test.values, "]")+", 'invented_value']", 1)
			if nha207Room(t, invalid, nha207Projection(t)).GetUnsupported() == nil {
				t.Fatal("normalization discarded an unknown value")
			}
		})
	}
}
