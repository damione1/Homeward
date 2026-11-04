package dashboard

import (
	wire "github.com/damione1/Homeward/gen/home/api/v2"
	"strings"
	"testing"
)

func TestScreenMediaQueryClosedGrammar(t *testing.T) {
	for _, test := range []struct {
		query    string
		min, max uint32
	}{{"(max-width: 767px)", 0, 767}, {"(min-width:768px) and (max-width:1199px)", 768, 1199}, {" (max-width: 65535px) and (min-width: 0px) ", 0, 65535}, {"(min-width:1200px)", 1200, 0}, {"(min-width:0px)", 0, 0}} {
		min, max, ok := parseScreenMediaQuery(test.query)
		if !ok || min != test.min || max != test.max {
			t.Fatalf("%q: %d/%d/%v", test.query, min, max, ok)
		}
	}
	for _, query := range []string{"", "screen and (min-width:768px)", "(orientation: landscape)", "(max-width:0px)", "(min-width:65536px)", "(min-width:900px) and (max-width:600px)", "(min-width:1px) and (min-width:2px)", "(max-width:767px), (min-width:1200px)", "(min-width:10.5px)", "(max-width:40em)", "(max-width:767px) or (min-width:1200px)", "javascript:alert(1)"} {
		if _, _, ok := parseScreenMediaQuery(query); ok {
			t.Fatalf("unknown query admitted: %q", query)
		}
	}
}
func screenSource(query string) string {
	return strings.Replace(widgetSource, "            title: Exact clock", "            title: Exact clock\n            visibility:\n              - condition: screen\n                media_query: '"+query+"'", 1)
}
func screenProject(t *testing.T, source string) *wire.DashboardLayoutNode {
	t.Helper()
	source = strings.Replace(source, "            weather_entity: weather.configured\n", "", 1)
	for _, node := range projectAPIFixture(t, compileWidgets(t, source)).Nodes {
		if node.Source.GetVariant() == "custom:homeward-clock-weather" {
			return node
		}
	}
	t.Fatal("missing authored clock")
	return nil
}
func TestScreenVisibilityIsExplicitAndNeverBecomesADomainPredicate(t *testing.T) {
	source := screenSource("(min-width:768px) and (max-width:1199px)")
	result := compileWidgets(t, source)
	if result.Status() != StatusSupported {
		t.Fatalf("closed screen condition rejected: %v", result.Diagnostics())
	}
	node := screenProject(t, source)
	if node.GetWidget() == nil || node.ScreenVisibility == nil || node.ScreenVisibility.MinWidth != 768 || node.ScreenVisibility.MaxWidth != 1199 {
		t.Fatalf("screen projection lost bounds: %v", node)
	}
	if node.Visibility != nil {
		t.Fatal("screen entered domain predicate tree")
	}
	mixed := strings.Replace(source, "media_query: '(min-width:768px) and (max-width:1199px)'", "media_query: '(min-width:768px) and (max-width:1199px)'\n              - condition: state\n                entity: light.room\n                state: 'on'", 1)
	node = screenProject(t, mixed)
	if node.ScreenVisibility == nil || node.Visibility == nil || len(node.Visibility.GetAll().Conditions) != 1 || node.Visibility.GetAll().Conditions[0].GetState() == nil {
		t.Fatal("domain state predicate changed when paired with screen presentation")
	}
}
func TestScreenVisibilityUnknownNestedAndContradictoryRulesAreInert(t *testing.T) {
	for _, source := range []string{screenSource("(orientation:portrait)"), strings.Replace(screenSource("(max-width:767px)"), "              - condition: screen\n                media_query:", "              - condition: or\n                conditions:\n                  - condition: screen\n                    media_query:", 1), strings.Replace(screenSource("(max-width:767px)"), "media_query: '(max-width:767px)'", "media_query: '(max-width:767px)'\n              - condition: screen\n                media_query: '(min-width:1200px)'", 1)} {
		node := screenProject(t, source)
		if node.GetUnsupported() == nil || node.GetWidget() != nil || node.Visibility != nil {
			t.Fatalf("invalid viewport rule became active: %v", node)
		}
	}
}
