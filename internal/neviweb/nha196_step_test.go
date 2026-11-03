package neviweb

import (
	"fmt"
	"testing"

	"github.com/damione1/Homeward/contracts"
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	wire "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA196ModelCommandStepsKeepObservationBoundsContinuous(t *testing.T) {
	for _, tc := range []struct {
		model int
		step  float64 // zero means the existing unspecified Wi-Fi command step
	}{
		{1123, 0.5}, {1124, 0.5}, {300, 0.5}, {737, 0.5}, {7372, 0.5}, {7373, 0.5},
		{1510, 0}, {336, 0}, {348, 0}, {343, 0}, {350, 0}, {354, 0}, {738, 0}, {739, 0}, {742, 0},
	} {
		t.Run(fmt.Sprintf("model-%d", tc.model), func(t *testing.T) {
			// Observations may be finer than the user command grid. In
			// particular, do not infer a command step from roomTemperature.
			frame, err := frameFor(BindingClimate, []byte(fmt.Sprintf(`{"signature":{"model":%d},"roomTemperature":{"value":17.22},"roomSetpoint":16.25,"roomSetpointMin":5,"roomSetpointMax":30,"systemMode":"heat","setpointMode":"manual"}`, tc.model)))
			if err != nil {
				t.Fatal(err)
			}
			pilot := &Pilot{}
			descriptions := pilot.descriptions(Binding{alias: "synthetic-neviweb-step", kind: BindingClimate}, frame.model)
			if len(descriptions) != 1 {
				t.Fatal("climate description absent")
			}
			constraints := descriptions[0].GetClimate()
			if constraints == nil || constraints.SetpointCelsius == nil || constraints.ObservedSetpointCelsius == nil || constraints.ObservedSetpointCelsius.Bounds == nil {
				t.Fatal("command or observation bounds absent")
			}
			bounds := constraints.SetpointCelsius
			observed := constraints.ObservedSetpointCelsius.Bounds
			if bounds.Minimum != 5 || bounds.Maximum != 30 || observed.Minimum != 5 || observed.Maximum != 30 || observed.Step != nil {
				t.Fatal("physical bounds changed or command grid constrained observations")
			}
			if tc.step == 0 {
				if bounds.Step != nil {
					t.Fatal("Wi-Fi acquired an unqualified command step")
				}
			} else if bounds.Step == nil || *bounds.Step != tc.step {
				t.Fatal("qualified Zigbee command step absent or incorrect")
			}
			catalog, err := contracts.NewCapabilityCatalog(domain.NewUnitCatalog())
			if err != nil {
				t.Fatal(err)
			}
			entity, err := domain.ParseEntityID("Host:synthetic/nha196-climate")
			if err != nil {
				t.Fatal(err)
			}
			if err := catalog.RegisterDriverCapability(entity, descriptions[0]); err != nil {
				t.Fatal(err)
			}
			entityID, err := entity.Value()
			if err != nil {
				t.Fatal(err)
			}
			for _, command := range []struct {
				target float64
				admit  bool
			}{{5, true}, {16.5, true}, {18, true}, {30, true}, {16.25, tc.step == 0}, {4.5, false}, {30.5, false}} {
				request := &actions.CommandRequest{
					CommandId: "nha196-step", AdmissionEpoch: "nha196-epoch", ConfigRevision: 1,
					Target:             &wire.EntityRef{Reference: &wire.EntityRef_EntityId{EntityId: entityID}},
					CapabilitySchemaId: contracts.ClimateSchema, Endpoint: "climate",
					Arguments: &actions.ActionArgs{Arguments: &actions.ActionArgs_SetClimate{SetClimate: &actions.SetClimate{SetpointCelsius: &command.target}}},
				}
				if err := catalog.AdmitCapabilityCommand(request, "nha196-epoch", 1); (err == nil) != command.admit {
					t.Fatalf("target %g admission=%v, expected %v", command.target, err, command.admit)
				}
			}
			value, err := climateStateValue(frame)
			if err != nil || value == nil {
				t.Fatalf("off-grid observation construction: %v", err)
			}
			climate, ok := value.Climate()
			if !ok {
				t.Fatal("observation lost its climate type")
			}
			if err := catalog.AdmitDomainClimate(entity, "climate", climate); err != nil {
				t.Fatalf("actual off-grid setpoint or precise measured temperature rejected: %v", err)
			}
		})
	}
}
