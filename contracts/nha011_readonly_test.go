package contracts

import (
	"errors"
	"testing"

	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func TestNHA011ReadOnlyAdmissionCoversEveryCoreActionBranch(t *testing.T) {
	catalog, err := NewCapabilityCatalog(domain.NewUnitCatalog())
	if err != nil {
		t.Fatal(err)
	}
	base := func(schema string, setConstraints func(*registry.CapabilityDescription)) *registry.CapabilityDescription {
		description := &registry.CapabilityDescription{SchemaId: schema, Major: 1, DisplayName: "Fixture", Endpoint: "fixture"}
		setConstraints(description)
		return description
	}
	// The closed core catalogue has two observation-only families and read-only
	// variants of switch, fan and climate. Light is always actionable today.
	readOnly := []*registry.CapabilityDescription{
		base(SwitchSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{}}
		}),
		base(FanSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{}}
		}),
		base(ClimateSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{}}
		}),
		base(MeasurementSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Measurement{Measurement: &registry.MeasurementConstraints{Unit: "W", Semantic: registry.MeasurementSemantic_MEASUREMENT_SEMANTIC_MEASUREMENT, Aggregation: registry.MeasurementAggregation_MEASUREMENT_AGGREGATION_CURRENT}}
		}),
		base(BinarySchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Binary{Binary: &registry.BinaryConstraints{}}
		}),
	}
	for _, description := range readOnly {
		if err := catalog.AdmitReadOnlyDriverCapability(description); err != nil {
			t.Fatalf("read-only %s rejected: %v", description.SchemaId, err)
		}
	}
	mode := registry.ClimateMode_CLIMATE_MODE_HEAT
	actionable := []*registry.CapabilityDescription{
		base(SwitchSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Switch{Switch: &registry.SwitchConstraints{SetOn: true}}
		}),
		base(LightSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: false}}
		}),
		base(FanSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{SetOn: true}}
		}),
		base(FanSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{SpeedOptions: []string{"high"}}}
		}),
		base(ClimateSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{SetpointCelsius: &registry.NumberBounds{Minimum: 5, Maximum: 30}}}
		}),
		base(ClimateSchema, func(d *registry.CapabilityDescription) {
			d.Constraints = &registry.CapabilityDescription_Climate{Climate: &registry.ClimateConstraints{ModeOptions: []registry.ClimateMode{mode}}}
		}),
	}
	for _, description := range actionable {
		if err := catalog.AdmitReadOnlyDriverCapability(description); !errors.Is(err, ErrActionableCapability) {
			t.Fatalf("actionable %s admitted: %v", description.SchemaId, err)
		}
	}
	extension := new(registry.CapabilityDescription)
	roundtrip(t, "extension-description", extension)
	extension.GetExtension().ActionMessage = nil
	if err := catalog.AdmitReadOnlyDriverCapability(extension); !errors.Is(err, ErrActionableCapability) {
		t.Fatalf("unknown extension admitted as Hilo observation: %v", err)
	}
	if err := catalog.AdmitReadOnlyDriverCapability(&registry.CapabilityDescription{SchemaId: ClimateSchema, Major: 1, DisplayName: "Fixture", Endpoint: "fixture", Constraints: (*registry.CapabilityDescription_Climate)(nil)}); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("typed-nil climate branch admitted: %v", err)
	}
}
