package dashboard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/damione1/Homeward/compat/ha/parser"
	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
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
func widgetsSupport() *wire.ClientLayoutSupport {
	support := nhf009LayoutSupport()
	support.Profile = DashboardProfile
	support.Version = 3
	support.ScreenVisibility = true
	support.Kinds = append(support.Kinds, wire.LayoutKind_LAYOUT_KIND_WIDGET)
	return support
}
func widgetNodes(layout *wire.NativeLayout) []*wire.LayoutNode {
	var result []*wire.LayoutNode
	for _, node := range layout.Nodes {
		if node.Source != nil && strings.HasPrefix(node.Source.Variant, "custom:homeward-") {
			result = append(result, node)
		}
	}
	return result
}
func TestDashboardWidgetsPreserveExactConfigurationAndBoundedForest(t *testing.T) {
	result := compileWidgets(t, widgetSource)
	if result.Status() != StatusSupported {
		t.Fatalf("compile %v: %v", result.Status(), result.Diagnostics())
	}
	definition, _ := result.Definition()
	owner, known := definition.Owner()
	ownerText, _ := owner.Value()
	if !known || ownerText != "dashboard-owner" || definition.Profile() != DashboardProfile || definition.Source().URLPath() != "configured" {
		t.Fatal("trusted metadata lost")
	}
	layout, err := ProjectNativeDefinition(definition, LayoutProjection{Support: widgetsSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	widgets := widgetNodes(layout)
	if layout.Version != 3 || len(widgets) != 8 || layout.Nodes[0].Gap != 0 || layout.Nodes[0].MinimumColumnWidth != 280 || layout.Nodes[1].Gap != 20 {
		t.Fatal("configured widget order or explicit/default metrics lost")
	}
	for _, node := range widgets {
		if node.Kind != wire.LayoutKind_LAYOUT_KIND_WIDGET || node.Widget == nil {
			t.Fatalf("widget not projected: %v", node)
		}
	}
	if widgets[0].Widget.GetClockWeather().WeatherEntityId != "synthetic-id:weather.configured" || widgets[0].Widget.GetClockWeather().Timezone != "America/Toronto" || widgets[0].Title != "Exact clock" || len(widgets[0].ResponsiveSpans) != 2 || widgets[0].ResponsiveSpans[1].Span != 1 {
		t.Fatal("clock exact binding or responsive spans lost")
	}
	room := widgets[1].Widget.GetRoom()
	if room.Temperature.EntityId != "synthetic-id:sensor.room_temperature" || room.ActivityEntityIds[0] != "synthetic-id:light.room" || room.ActiveStates[0] != "on" || widgets[1].Widget.Action.NodeId != "$.views[1]" {
		t.Fatal("room binding or configured action lost")
	}
	thermostat := widgets[2].Widget.GetThermostat()
	if thermostat.EntityId != "synthetic-id:climate.configured" || thermostat.TargetTemperature.FieldKey != "setpoint_celsius" {
		t.Fatal("thermostat binding lost")
	}
	agenda := widgets[3].Widget.GetAgenda()
	if agenda.CalendarId != "calendar-id:configured" || agenda.Limit != 4 || agenda.WindowSeconds != 86400 {
		t.Fatal("agenda binding lost")
	}
	if widgets[4].Widget.GetTasks().ListId != "configured-list" || widgets[4].Widget.GetTasks().Limit != 3 || widgets[4].Widget.EmptyLabel != "Exact empty label" || widgets[5].Widget.GetSecurity().EntityIds[0] != "synthetic-id:lock.configured" || widgets[6].Widget.GetObservations().Items[0].Label != "Exact observation" || widgets[7].Widget.GetNavigation().Destination.NodeId != "$.views[1]" || widgets[7].Widget.ActionLabel != "Exact navigation" {
		t.Fatal("explicit data or navigation changed")
	}
	// Projection output and exported model clones cannot mutate the source.
	widgets[0].Widget.GetClockWeather().Timezone = "UTC"
	copy, _ := result.Definition()
	again, err := ProjectNativeDefinition(copy, LayoutProjection{Support: widgetsSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil || widgetNodes(again)[0].Widget.GetClockWeather().Timezone != "America/Toronto" {
		t.Fatal("projection mutated source")
	}
}
func TestDashboardWidgetsNeverFallbackAfterMissingOrUnnegotiatedBinding(t *testing.T) {
	result := compileWidgets(t, widgetSource)
	for _, support := range []*wire.ClientLayoutSupport{nil, nhf009LayoutSupport(), {Profile: DashboardProfile, Version: 99}} {
		layout, err := ProjectNativeLayout(result, LayoutProjection{Support: support, Entity: nhf009ResolveEverySyntheticAlias})
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range layout.Nodes {
			if node.Kind != wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED || node.Widget != nil || node.Recipes != nil {
				t.Fatal("unnegotiated payload escaped")
			}
		}
	}
	layout, err := ProjectNativeLayout(result, LayoutProjection{Support: widgetsSupport(), Entity: func(alias domain.HAAlias) (domain.EntityID, bool) {
		text, _ := alias.Value()
		if text == "sensor.room_temperature" {
			return domain.EntityID{}, false
		}
		return nhf009ResolveEverySyntheticAlias(alias)
	}})
	if err != nil {
		t.Fatal(err)
	}
	nodes := widgetNodes(layout)
	if nodes[1].Kind != wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED || nodes[1].Widget != nil || nodes[2].Kind != wire.LayoutKind_LAYOUT_KIND_WIDGET {
		t.Fatal("missing room temperature substituted unrelated entity or hid unrelated widget")
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
				layout, err := ProjectNativeDefinition(def, LayoutProjection{Support: widgetsSupport(), Entity: nhf009ResolveEverySyntheticAlias})
				if err != nil {
					t.Fatal(err)
				}
				for _, node := range layout.Nodes {
					if node.Kind == wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED && node.Widget != nil {
						t.Fatal("invalid widget still executable")
					}
				}
			}
		})
	}
}
func TestEmptyConfiguredDashboardHasNoImplicitWidgets(t *testing.T) {
	result := compileWidgets(t, "homeward: {profile: homeward-dashboard-3, owner: dashboard-owner, url_path: configured}\nviews: []\n")
	if result.Status() != StatusSupported {
		t.Fatal(result.Diagnostics())
	}
	definition, _ := result.Definition()
	layout, err := ProjectNativeDefinition(definition, LayoutProjection{Support: widgetsSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil || layout.Version != 3 || len(layout.Nodes) != 0 || len(layout.Views) != 0 {
		t.Fatal("empty configuration invented content")
	}
}
func TestWidgetProjectionRoundTripAndBasicCompatibility(t *testing.T) {
	result := compileWidgets(t, widgetSource)
	layout, err := ProjectNativeLayout(result, LayoutProjection{Support: widgetsSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := proto.Marshal(layout)
	if err != nil {
		t.Fatal(err)
	}
	decoded := &wire.NativeLayout{}
	if err = proto.Unmarshal(bytes, decoded); err != nil || !proto.Equal(layout, decoded) {
		t.Fatal("widget protocol roundtrip failed")
	}
	id, _ := authorization.ParseDashboardID("basic")
	source, _ := NewSource(id, "basic", "basic.yaml")
	file, _ := parser.NewSourceFile("basic.yaml", []byte("views: [{type: sections, path: home, title: Home, sections: []}]\n"))
	basic, err := RestoreConfigured(source, []parser.SourceFile{file})
	definition, _ := basic.Definition()
	if err != nil || basic.Status() != StatusSupported || definition.Profile() != Profile {
		t.Fatal("configured importer changed legacy profile")
	}
	if _, err = ProjectNativeDefinition(definition, LayoutProjection{Support: widgetsSupport(), Entity: nhf009ResolveEverySyntheticAlias}); err != nil {
		t.Fatal("v3 client cannot consume Basic")
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

func TestNativeWidgetEquivalentExplicitTapKeepsItsRecipeAndConfiguredLabel(t *testing.T) {
	source := strings.Replace(widgetSource, "action: /configured/rooms", "action: /configured/rooms\n            tap_action: {action: navigate, navigation_path: /configured/rooms}", 1)
	result := compileWidgets(t, source)
	if result.Status() != StatusSupported {
		t.Fatal("equivalent configured intentions refused", result.Diagnostics())
	}
	layout, err := ProjectNativeLayout(result, LayoutProjection{Support: widgetsSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	room := widgetNodes(layout)[1]
	if room.Kind != wire.LayoutKind_LAYOUT_KIND_WIDGET || room.Widget.Action.NodeId != "$.views[1]" || room.Recipes.Interactions[0].GetNavigate().NodeId != "$.views[1]" {
		t.Fatal("equivalent configured actions lost")
	}
}
