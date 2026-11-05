package dashboard

import (
	"fmt"
	"github.com/damione1/Homeward/compat/ha/parser"
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/authorization"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

const widgetSource = `homeward: {profile: homeward-dashboard-3, owner: dashboard-owner, url_path: configured}
views:
  - type: sections
    path: home
    title: Configured home
    max_columns: 4
    minimum_column_width: 280
    gap: 0
    sections:
      - type: grid
        cards:
          - type: custom:homeward-clock-weather
            title: Exact clock
            timezone: America/Toronto
            weather_entity: weather.configured
            grid_options:
              responsive_spans: [{columns: 4, span: 2}, {columns: 3, span: 1}]
          - type: custom:homeward-room
            room_id: configured-room
            temperature: {entity: sensor.room_temperature, field: measurement}
            activity_entities: [light.room]
            active_states: ['on']
            action: /configured/rooms
            action_label: Exact rooms
          - type: custom:homeward-thermostat
            entity: climate.configured
            current_temperature: {entity: climate.configured, field: current_temperature_celsius}
            target_temperature: {entity: climate.configured, field: setpoint_celsius}
          - type: custom:homeward-agenda
            calendar_id: calendar-id:configured
            timezone: America/Toronto
            window_seconds: 86400
            limit: 4
          - type: custom:homeward-tasks
            list_id: configured-list
            limit: 3
            empty_label: Exact empty label
          - type: custom:homeward-security
            entities: [lock.configured, alarm_control_panel.configured]
          - type: custom:homeward-observations
            items:
              - field: {entity: binary_sensor.configured, field: state}
                label: Exact observation
                active_states: ['on']
          - type: custom:homeward-navigation
            title: Exact navigation
            destination: /configured/rooms
  - type: sections
    path: rooms
    title: Exact rooms
    sections: []
`

func compileWidgets(t *testing.T, source string) Result {
	t.Helper()
	id, _ := authorization.ParseDashboardID("configured-dashboard")
	descriptor, _ := NewSource(id, "unrelated", "dashboard.yaml")
	file, err := parser.NewSourceFile("dashboard.yaml", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	result, err := RestoreConfigured(descriptor, []parser.SourceFile{file})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func widgetNodes(layout *wire.DashboardLayout) []*wire.DashboardLayoutNode {
	var out []*wire.DashboardLayoutNode
	for _, node := range layout.Nodes {
		if node.Source != nil && strings.HasPrefix(node.Source.Variant, "custom:homeward-") {
			out = append(out, node)
		}
	}
	return out
}
func TestDashboardWidgetsExactSourceAndClosedProjection(t *testing.T) {
	source := strings.Replace(widgetSource, "            weather_entity: weather.configured\n", "", 1)
	result := compileWidgets(t, source)
	if result.Status() != StatusSupported {
		t.Fatal(result.Diagnostics())
	}
	layout := projectAPIFixture(t, result)
	nodes := widgetNodes(layout)
	if len(nodes) != 8 || layout.Nodes[0].GetView().GetChildren().GetGap() != 0 || layout.Nodes[0].GetView().MinimumColumnWidth != 280 {
		t.Fatal("authored forest or explicit zero lost")
	}
	clock := nodes[0].GetWidget().GetClockWeather()
	if clock == nil || clock.TimeZone != "America/Toronto" || nodes[0].DisplayName != "Exact clock" || len(nodes[0].GetPlacement().GetResponsiveSpans()) != 2 {
		t.Fatal("authored clock presentation lost")
	}
	// Without an admitted source slot, names alone cannot activate these widgets.
	for _, i := range []int{2, 5, 6} {
		if nodes[i].GetUnsupported() == nil {
			t.Fatalf("widget %d inferred capability from alias", i)
		}
	}
	room := nodes[1].GetWidget().GetRoom()
	if room == nil || room.Temperature != nil || len(room.Activity) != 1 || nodes[1].GetWidget().Action == nil {
		t.Fatal("optional missing temperature erased room navigation or fabricated an observation")
	}
	navigation := nodes[7].GetWidget().GetNavigation()
	if navigation == nil || navigation.Destination.NodeId != "$.views[1]" || nodes[7].GetWidget().ActionLabel != "Exact navigation" {
		t.Fatal("exact local navigation lost")
	}
	clock.TimeZone = "UTC"
	if widgetNodes(projectAPIFixture(t, result))[0].GetWidget().GetClockWeather().TimeZone != "America/Toronto" {
		t.Fatal("projection mutated captured source")
	}
	bytes, err := proto.Marshal(layout)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(wire.DashboardLayout)
	if err = proto.Unmarshal(bytes, decoded); err != nil || !proto.Equal(layout, decoded) {
		t.Fatal("v2 closed representation did not roundtrip", err)
	}
}
func TestDashboardWidgetsMissingBindingNeverFallsBack(t *testing.T) {
	layout := projectAPIFixture(t, compileWidgets(t, widgetSource))
	nodes := widgetNodes(layout)
	if nodes[0].GetUnsupported() == nil || nodes[2].GetUnsupported() == nil || nodes[7].GetWidget().GetNavigation() == nil {
		t.Fatal("missing capability fell back or hid unrelated navigation")
	}
}
func TestEmptyConfiguredDashboardHasNoImplicitWidgets(t *testing.T) {
	layout := projectAPIFixture(t, compileWidgets(t, "homeward: {profile: homeward-dashboard-3, owner: dashboard-owner, url_path: configured}\nviews: []\n"))
	if len(layout.Nodes) != 0 || len(layout.Views) != 0 {
		t.Fatal("empty definition invented content")
	}
}
func TestDashboardWidgetMalformedOptionsRemainInert(t *testing.T) {
	for name, replacement := range map[string][2]string{
		"conflicting-widget-tap":        {"action: /configured/rooms", "action: /configured/rooms\n            tap_action: {action: navigate, navigation_path: /configured/home}"},
		"conflicting-navigation-action": {"destination: /configured/rooms", "destination: /configured/rooms\n            action: /configured/home"},
		"missing-navigation-label":      {"title: Exact navigation", "title: ''"},
		"gap-budget":                    {"gap: 0", "gap: 257"},
		"control-label":                 {"label: Exact observation", "label: \"invalid\\nlabel\""},
		"missing-url":                   {"owner: dashboard-owner, url_path: configured", "owner: dashboard-owner"},
		"unknown-field":                 {"field: measurement", "field: invented"},
		"wrong-current-field":           {"field: current_temperature_celsius", "field: value"},
		"cross-device-current":          {"entity: climate.configured, field: current_temperature_celsius", "entity: climate.other, field: current_temperature_celsius"},
		"cross-device-target":           {"entity: climate.configured, field: setpoint_celsius", "entity: climate.other, field: setpoint_celsius"},
		"invalid-zone":                  {"timezone: America/Toronto", "timezone: Invalid/Missing"},
		"missing-limit":                 {"limit: 4", "unknown_limit: 4"},
		"excess-limit":                  {"limit: 4", "limit: 1001"},
		"excess-window":                 {"window_seconds: 86400", "window_seconds: 31622401"},
		"missing-active-states":         {"active_states: ['on']", "active_states: []"},
		"duplicate-span":                {"{columns: 3, span: 1}", "{columns: 4, span: 1}"},
		"overflow-span":                 {"{columns: 3, span: 1}", "{columns: 3, span: 4}"},
		"unknown-option":                {"title: Exact clock", "title: Exact clock\n            executable: run()"},
	} {
		t.Run(name, func(t *testing.T) {
			result := compileWidgets(t, strings.Replace(widgetSource, replacement[0], replacement[1], 1))
			if result.Status() == StatusSupported {
				t.Fatal("invalid source admitted")
			}
			if def, ok := result.Definition(); ok {
				layout, err := ProjectDashboardDefinition(def, apiFixtureProjection(t))
				if err != nil {
					t.Fatal(err)
				}
				for _, node := range layout.Nodes {
					if node.GetUnsupported() != nil && node.Visibility != nil {
						t.Fatal("inert node carried domain predicate")
					}
				}
			}
		})
	}
}

func widgetBudgetSource(cards string) string {
	return "homeward: {profile: homeward-dashboard-3, owner: dashboard-owner, url_path: configured}\nviews:\n - type: sections\n   path: home\n   title: Configured\n   sections:\n    - type: grid\n      cards:\n" + cards
}
func TestDashboardWidgetAggregateBudgetsMatchNativeClientAdmission(t *testing.T) {
	resource := "       - type: custom:homeward-tasks\n         list_id: exact-list\n         limit: 1\n"
	if result := compileWidgets(t, widgetBudgetSource(strings.Repeat(resource, 32))); result.Status() != StatusSupported {
		t.Fatal("32 resource widgets refused", result.Diagnostics())
	}
	if result := compileWidgets(t, widgetBudgetSource(strings.Repeat(resource, 33))); result.Status() != StatusFatal {
		t.Fatal("resource widget budget escaped", result.Status())
	}
	aliases := make([]string, 64)
	for i := range aliases {
		aliases[i] = fmt.Sprintf("light.budget_%d", i)
	}
	security := "       - type: custom:homeward-security\n         entities: [" + strings.Join(aliases, ",") + "]\n"
	cards := strings.Repeat(security, 64)
	if result := compileWidgets(t, widgetBudgetSource(cards)); result.Status() != StatusSupported {
		t.Fatal("4096 bindings refused", result.Diagnostics())
	}
	clock := "       - type: custom:homeward-clock-weather\n         timezone: UTC\n         weather_entity: weather.extra\n"
	if result := compileWidgets(t, widgetBudgetSource(cards+clock)); result.Status() != StatusFatal {
		t.Fatal("aggregate entity binding budget escaped", result.Status())
	}
}
