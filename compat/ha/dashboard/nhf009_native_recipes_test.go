package dashboard

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apitypes"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestNHF009RecipesBindAdmittedCapabilityWithoutExecuting(t *testing.T) {
	source := `views:
- type: sections
  title: Home
  path: main
  sections:
  - type: grid
    cards:
    - type: tile
      entity: light.synthetic
      tap_action: {action: toggle}
      hold_action: {action: none}
`
	result := nhf009ImportRecipe(t, source)
	layout := projectAPIFixture(t, result)
	tile := layout.Nodes[2].GetTile()
	if tile == nil {
		t.Fatal("configured tile missing")
	}
	interactions := tile.GetInteractions().GetInteractions()
	if len(interactions) != 2 || interactions[0].GetCommand() == nil || interactions[1].GetNone() == nil {
		t.Fatal("explicit gestures were dropped or defaulted")
	}
	command := interactions[0].GetCommand()
	if _, err := apitypes.ParseCapabilityName(command.Capability); err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(layout)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(api.DashboardLayout)
	if err = proto.Unmarshal(encoded, decoded); err != nil || !proto.Equal(layout, decoded) {
		t.Fatal("closed recipe roundtrip", err)
	}
	definition, _ := result.Definition()
	options := apiFixtureProjection(t)
	options.Capabilities = nil
	if _, err = ProjectDashboardDefinition(definition, options); err == nil {
		t.Fatal("absent authoritative capability resolver accepted")
	}
}
