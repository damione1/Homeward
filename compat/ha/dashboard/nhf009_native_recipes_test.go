package dashboard

import (
	"fmt"
	"testing"

	wire "github.com/damione1/Homeward/gen/home/dashboard/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
)

func nhf009Resolver() RecipeResolver {
	return RecipeResolver{
		Entity: func(alias domain.HAAlias) (domain.EntityID, bool) {
			value, err := alias.Value()
			if err != nil || (value != "light.kitchen" && value != "binary_sensor.occupied" && value != "lock.synthetic") {
				return domain.EntityID{}, false
			}
			id, err := domain.ParseEntityID("synthetic-id:" + value)
			return id, err == nil
		},
		Destination: func(path string) (*wire.NativeDestination, bool) {
			if path != "/lovelace/main" {
				return nil, false
			}
			return &wire.NativeDestination{DashboardId: &wire.DashboardId{Value: "synthetic-dashboard"}, NodeId: "stable-source-main", Presentation: wire.NativePresentation_NATIVE_PRESENTATION_PAGE}, true
		},
	}
}

func TestNHF009ImporterRecipesProjectToDataWithoutExecutingOrDroppingSemantics(t *testing.T) {
	result := nha059ImportBytes(t, []byte(nha059Supported))
	definition, present := result.Definition()
	if !present {
		t.Fatal("missing admitted definition")
	}
	section := definition.Views()[0].Sections()[0]
	card := section.Cards()[1].Children()[0]
	recipes, err := ProjectNativeRecipes(card.Location(), card.Actions(), section.Visibility(), nhf009Resolver())
	if err != nil {
		t.Fatal(err)
	}
	if recipes.Version != 1 || len(recipes.Interactions) != 3 {
		t.Fatalf("recipes=%+v", recipes)
	}
	if recipes.Interactions[0].GetCommand().Kind != wire.NativeCommandKind_NATIVE_COMMAND_KIND_TOGGLE || recipes.Interactions[0].GetCommand().EntityId != "synthetic-id:light.kitchen" {
		t.Fatal("toggle target/semantics lost")
	}
	if recipes.Interactions[1].GetMoreInfo().EntityId != "synthetic-id:light.kitchen" {
		t.Fatal("more-info binding lost")
	}
	if recipes.Interactions[2].GetNavigate().NodeId != "stable-source-main" || recipes.Interactions[2].GetCommand() != nil {
		t.Fatal("navigation became a device command")
	}
	state := recipes.Visibility.GetAll().Conditions[0].GetState()
	if state.EntityId != "synthetic-id:binary_sensor.occupied" || state.State != "on" || state.Comparison != wire.NativeComparison_NATIVE_COMPARISON_EQUAL {
		t.Fatal("visibility semantics lost")
	}
	for _, interaction := range recipes.Interactions {
		if interaction.Source == nil || interaction.Source.File != "dashboard.yaml" || interaction.Source.Path == "" || interaction.Source.Line == 0 {
			t.Fatal("action source discarded")
		}
	}
	encoded, err := proto.Marshal(recipes)
	if err != nil {
		t.Fatal(err)
	}
	var restored wire.NativeRecipes
	if err := proto.Unmarshal(encoded, &restored); err != nil || !proto.Equal(recipes, &restored) {
		t.Fatal("wire recipe drift")
	}
	resolver := nhf009Resolver()
	resolver.Entity = func(domain.HAAlias) (domain.EntityID, bool) { return domain.EntityID{}, false }
	if got, err := ProjectNativeRecipes(card.Location(), card.Actions(), section.Visibility(), resolver); err == nil || got != nil {
		t.Fatal("unresolved entity admitted or partially projected")
	}
	resolver = nhf009Resolver()
	resolver.Destination = nil
	if got, err := ProjectNativeRecipes(card.Location(), card.Actions(), nil, resolver); err == nil || got != nil {
		t.Fatal("unresolved navigation replaced by default tap")
	}
}

func TestNHF009PerformProfileKeepsConfirmationAndRejectsUnknownOrCrossDomainServices(t *testing.T) {
	for _, tc := range []struct {
		service, target string
		admitted        bool
	}{
		{"lock.unlock", "lock.synthetic", true},
		{"light.turn_on", "lock.synthetic", false},
		{"lock.future_action", "lock.synthetic", false},
	} {
		t.Run(tc.service, func(t *testing.T) {
			result := nha059ImportBytes(t, []byte(fmt.Sprintf(`views:
- type: sections
  title: Main
  path: main
  sections:
  - type: grid
    cards:
    - type: tile
      entity: lock.synthetic
      tap_action:
        action: perform-action
        perform_action: %s
        target: {entity_id: %s}
        confirmation: {text: ''}
      hold_action: {action: none}
`, tc.service, tc.target)))
			if result.Status() != StatusSupported {
				t.Fatalf("unadmitted fixture: %+v", result.Diagnostics())
			}
			definition, present := result.Definition()
			if !present {
				t.Fatal("missing definition")
			}
			card := definition.Views()[0].Sections()[0].Cards()[0]
			projection, err := ProjectNativeRecipes(card.Location(), card.Actions(), nil, nhf009Resolver())
			if !tc.admitted {
				if err == nil || projection != nil {
					t.Fatal("service admitted without an exact native mapping")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(projection.Interactions) != 2 || projection.Interactions[0].Confirmation == nil || projection.Interactions[0].Confirmation.Text != "" {
				t.Fatal("empty confirmation presence lost")
			}
			if projection.Interactions[0].GetCommand().Kind != wire.NativeCommandKind_NATIVE_COMMAND_KIND_UNLOCK || projection.Interactions[1].GetNone() == nil {
				t.Fatal("command or explicit disabled hold lost")
			}
		})
	}
}
