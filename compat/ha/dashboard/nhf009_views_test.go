package dashboard

import (
	"fmt"
	"testing"
)

func TestNHF009PathlessNativeRoutesPreserveExplicitHASourcePaths(t *testing.T) {
	result := nhf009ImportRecipe(t, `views:
- title: Repeated title
  type: sections
  sections: []
- title: Repeated title
  path: explicit
  type: sections
  sections: []
- title: Repeated title
  type: sections
  sections: []
`)
	definition, _ := result.Definition()
	layout, err := ProjectNativeLayout(result, LayoutProjection{Support: nhf009LayoutSupport(), Entity: nhf009ResolveEverySyntheticAlias})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"$.views[0]", "explicit", "$.views[2]"}
	for index, id := range layout.Views {
		for _, node := range layout.Nodes {
			if node.Id == id && node.Route != want[index] {
				t.Fatalf("source view %d has no distinct native route: %q", index, node.Route)
			}
		}
	}
	if definition.Views()[0].Path() != "" || definition.Views()[1].Path() != "explicit" || definition.Views()[2].Path() != "" {
		t.Fatal("native routing rewrote the captured HA paths")
	}
}

func TestNHF009KnownViewPresentationIsAdaptedWithDiagnosticAndMalformedValuesRemainInert(t *testing.T) {
	for _, property := range []string{"subview", "dense_section_placement", "top_margin"} {
		for _, value := range []struct {
			input  string
			status Status
		}{
			{"true", StatusSupported},
			{"false", StatusSupported},
			{"'true'", StatusUnsupported},
			{"{}", StatusUnsupported},
			{"[]", StatusUnsupported},
		} {
			t.Run(property+"/"+value.input, func(t *testing.T) {
				result := nhf009ImportRecipe(t, fmt.Sprintf("views:\n- title: Synthetic\n  type: sections\n  sections: []\n  %s: %s\n", property, value.input))
				definition, _ := result.Definition()
				if definition.Views()[0].Status() != value.status || len(result.Diagnostics()) == 0 {
					t.Fatal("presentation must adapt booleans with source diagnostics and refuse other shapes")
				}
			})
		}
	}
}
