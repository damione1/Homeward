package dashboard

import (
	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/domain"
	"os"
	"strings"
	"testing"
)

func TestDashboardUXSourceRetainsCompactRoomsAndExactDetailDestinations(t *testing.T) {
	source, err := os.ReadFile("../../../examples/dashboards/demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "todo.to_do_secondary") {
		t.Fatal("private HA list exposed without account mapping")
	}
	compiled := compileWidgets(t, string(source))
	if compiled.Status() != StatusSupported {
		t.Fatalf("UX source not admitted: status=%v diagnostics=%v", compiled.Status(), compiled.Diagnostics())
	}
	projected, err := ProjectNativeLayout(compiled, LayoutProjection{Support: widgetsSupport(), Entity: func(alias domain.HAAlias) (domain.EntityID, bool) {
		id, e := domain.ParseEntityID("ux:" + func() string { value, _ := alias.Value(); return value }())
		return id, e == nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	rooms, details, popups := 0, 0, 0
	for _, node := range projected.Nodes {
		if node.Kind == wire.LayoutKind_LAYOUT_KIND_UNSUPPORTED {
			t.Fatalf("inert UX node %s: %s", node.Id, node.Diagnostic)
		}
		if node.Kind == wire.LayoutKind_LAYOUT_KIND_POPUP {
			popups++
		}
		if widget := node.GetWidget(); widget != nil {
			if widget.GetRoom() != nil {
				rooms++
				if widget.Action == nil || widget.Action.Presentation != wire.NativePresentation_NATIVE_PRESENTATION_POPUP {
					t.Fatal("room lost its configured popup")
				}
			}
			if thermostat := widget.GetThermostat(); thermostat != nil && thermostat.Presentation == wire.NativeThermostatPresentation_NATIVE_THERMOSTAT_PRESENTATION_DETAIL {
				details++
				if thermostat.CurrentTemperature.EntityId != thermostat.EntityId || thermostat.TargetTemperature.EntityId != thermostat.EntityId {
					t.Fatal("detail retargeted an exact field")
				}
			}
		}
	}
	// Responsive copies are configuration, with exactly one visible room entry
	// per window class. The compact first screen starts with actionable devices.
	nodes := map[string]*wire.LayoutNode{}
	for _, node := range projected.Nodes {
		nodes[node.Id] = node
	}
	for _, width := range []uint32{360, 800, 1200} {
		visibleRooms := map[string]bool{}
		var devices []string
		var walk func(string)
		walk = func(id string) {
			n := nodes[id]
			if n == nil || n.Kind == wire.LayoutKind_LAYOUT_KIND_POPUP {
				return
			}
			if screen := n.ScreenVisibility; screen != nil && ((screen.MinWidth > 0 && width < screen.MinWidth) || (screen.MaxWidth > 0 && width > screen.MaxWidth)) {
				return
			}
			if room := n.GetWidget().GetRoom(); room != nil {
				if visibleRooms[room.RoomId] {
					t.Fatalf("width %d duplicated room %s", width, room.RoomId)
				}
				visibleRooms[room.RoomId] = true
			}
			if n.EntityId != "" {
				devices = append(devices, n.EntityId)
			}
			for _, child := range n.Children {
				walk(child)
			}
		}
		walk(projected.Views[0])
		if len(visibleRooms) != 9 {
			t.Fatalf("width %d has %d visible rooms", width, len(visibleRooms))
		}
		if width < 1024 && (len(devices) < 2 || devices[0] != "ux:alarm_control_panel.demo_alarm" || devices[1] != "ux:cover.demo_garage_door") {
			t.Fatalf("width %d lost immediate alarm/garage entries: %v", width, devices)
		}
	}
	if rooms != 18 || details != 9 || popups != 22 {
		t.Fatalf("rooms=%d details=%d popups=%d", rooms, details, popups)
	}
}

func TestThermostatPresentationAndConfiguredIconsRemainData(t *testing.T) {
	source := strings.Replace(widgetSource, "type: custom:homeward-thermostat", "type: custom:homeward-thermostat\n            presentation: detail\n            icon: mdi:thermostat", 1)
	compiled := compileWidgets(t, source)
	if compiled.Status() != StatusSupported {
		t.Fatal(compiled.Diagnostics())
	}
	layout, err := ProjectNativeLayout(compiled, LayoutProjection{Support: widgetsSupport(), Entity: func(alias domain.HAAlias) (domain.EntityID, bool) {
		id, e := domain.ParseEntityID("ux:" + func() string { value, _ := alias.Value(); return value }())
		return id, e == nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, node := range layout.Nodes {
		if thermostat := node.GetWidget().GetThermostat(); thermostat != nil {
			found = true
			if thermostat.Presentation != wire.NativeThermostatPresentation_NATIVE_THERMOSTAT_PRESENTATION_DETAIL || node.Icon != wire.NativeIcon_NATIVE_ICON_THERMOSTAT {
				t.Fatal("server presentation was discarded")
			}
		}
	}
	if !found {
		t.Fatal("configured thermostat missing")
	}
	invalid := compileWidgets(t, strings.Replace(source, "presentation: detail", "presentation: downloaded-script", 1))
	if invalid.Status() == StatusSupported {
		t.Fatal("arbitrary renderer admitted")
	}
}

func TestSummaryAndShoppingPresentationsAreClosedServerChoices(t *testing.T) {
	source := strings.Replace(widgetSource, "type: custom:homeward-thermostat", "type: custom:homeward-thermostat\n            presentation: summary", 1)
	source = strings.Replace(source, "type: custom:homeward-tasks", "type: custom:homeward-tasks\n            presentation: shopping", 1)
	compiled := compileWidgets(t, source)
	if compiled.Status() != StatusSupported {
		t.Fatal(compiled.Diagnostics())
	}
	projected, err := ProjectNativeLayout(compiled, LayoutProjection{Support: widgetsSupport(), Entity: func(alias domain.HAAlias) (domain.EntityID, bool) {
		v, _ := alias.Value()
		id, e := domain.ParseEntityID("ux:" + v)
		return id, e == nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	thermostat, shopping := false, false
	for _, node := range projected.Nodes {
		if value := node.GetWidget().GetThermostat(); value != nil {
			thermostat = value.Presentation == wire.NativeThermostatPresentation_NATIVE_THERMOSTAT_PRESENTATION_SUMMARY
		}
		if value := node.GetWidget().GetTasks(); value != nil {
			shopping = value.Presentation == wire.NativeTasksPresentation_NATIVE_TASKS_PRESENTATION_SHOPPING
		}
	}
	if !thermostat || !shopping {
		t.Fatal("presentation discarded")
	}
	for _, invalid := range []string{strings.Replace(source, "presentation: summary", "presentation: inline-script", 1), strings.Replace(source, "presentation: shopping", "presentation: first-list", 1)} {
		if compileWidgets(t, invalid).Status() == StatusSupported {
			t.Fatal("open-ended presentation admitted")
		}
	}
}
