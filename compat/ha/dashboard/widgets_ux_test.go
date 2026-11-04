package dashboard

import (
	"github.com/damione1/Homeward/internal/apitypes"
	"os"
	"strings"
	"testing"
)

func TestDashboardUXConfiguredSourceProjectsWithoutInventedBindings(t *testing.T) {
	source, err := os.ReadFile("../../../examples/dashboards/demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "todo.to_do_secondary") {
		t.Fatal("private list source leaked")
	}
	result := compileWidgets(t, string(source))
	if result.Status() != StatusSupported {
		t.Fatal(result.Diagnostics())
	}
	layout := projectAPIFixture(t, result)
	if len(layout.Views) == 0 {
		t.Fatal("configured roots dropped")
	}
	unsupported := 0
	for _, node := range layout.Nodes {
		if node.GetUnsupported() != nil {
			unsupported++
			continue
		}
		if widget := node.GetWidget(); widget != nil && widget.GetNavigation() != nil {
			if _, err := apitypes.ParseDashboardName(widget.GetNavigation().GetDestination().GetDashboard()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if unsupported == 0 {
		t.Fatal("fixture without device slots inferred interactive controls")
	}
}
func TestThermostatPresentationAndConfiguredIconsRemainClosedSource(t *testing.T) {
	source := strings.Replace(widgetSource, "type: custom:homeward-thermostat", "type: custom:homeward-thermostat\n            presentation: detail\n            icon: mdi:thermostat", 1)
	if result := compileWidgets(t, source); result.Status() != StatusSupported {
		t.Fatal(result.Diagnostics())
	}
	if compileWidgets(t, strings.Replace(source, "presentation: detail", "presentation: downloaded-script", 1)).Status() == StatusSupported {
		t.Fatal("executable presentation admitted")
	}
}
func TestSummaryAndShoppingPresentationsAreClosedServerChoices(t *testing.T) {
	source := strings.Replace(widgetSource, "type: custom:homeward-thermostat", "type: custom:homeward-thermostat\n            presentation: summary", 1)
	source = strings.Replace(source, "type: custom:homeward-tasks", "type: custom:homeward-tasks\n            presentation: shopping", 1)
	if result := compileWidgets(t, source); result.Status() != StatusSupported {
		t.Fatal(result.Diagnostics())
	}
	for _, invalid := range []string{strings.Replace(source, "presentation: summary", "presentation: executable", 1), strings.Replace(source, "presentation: shopping", "presentation: executable", 1)} {
		if compileWidgets(t, invalid).Status() == StatusSupported {
			t.Fatal("unknown presentation admitted")
		}
	}
}
