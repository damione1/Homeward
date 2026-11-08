package contracts

import (
	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestNHA212NativeFanKeysRangesAndIndependentPower(t *testing.T) {
	catalog, _ := NewCapabilityCatalog(domain.NewUnitCatalog())
	entity := testNHA014Entity(t)
	description := &registry.CapabilityDescription{SchemaId: FanSchema, Major: 1, Endpoint: "drive", DisplayName: "Synthetic fan", Constraints: &registry.CapabilityDescription_Fan{Fan: &registry.FanConstraints{SetOn: true, ObservesPower: true, Speed: &registry.ControlChoices{Choices: []*registry.ControlChoice{{Key: "quiet/7"}, {Key: "boost:91"}, {Key: "night/13"}}}}}}
	if err := catalog.ReplaceDriverCapabilitiesBatch([]DriverCapabilityRegistration{{Entity: entity, Description: description}}); err != nil {
		t.Fatal(err)
	}
	value, err := domain.NewFanValue(domain.FanValueInput{On: proto.Bool(false), Speed: proto.String("quiet/7")})
	if err != nil {
		t.Fatal(err)
	}
	if err = catalog.AdmitDomainFan(entity, "drive", value); err != nil {
		t.Fatal(err)
	}
	wire, err := WireFanValue(value)
	if err != nil || wire.On == nil || *wire.On || wire.GetSpeed() != "quiet/7" {
		t.Fatal("power was inferred from a level", wire, err)
	}
	for _, key := range []string{"quiet/7", "boost:91", "night/13", "low", "7"} {
		cmd := testNHA014Command(FanSchema, "drive", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Speed: &key}}})
		err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1)
		if (err == nil) != (key == "quiet/7" || key == "boost:91" || key == "night/13") {
			t.Fatalf("exact key %q: %v", key, err)
		}
	}
	description.GetFan().Speed = nil
	description.GetFan().Level = &registry.NumberBounds{Minimum: 3, Maximum: 27, Step: proto.Float64(4)}
	if err = catalog.ReplaceDriverCapabilitiesBatch([]DriverCapabilityRegistration{{Entity: entity, Description: description}}); err != nil {
		t.Fatal(err)
	}
	for _, level := range []float64{3, 7, 27, 0, 8, 100} {
		cmd := testNHA014Command(FanSchema, "drive", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetFan{SetFan: &actions.SetFan{Level: &level}}})
		if got := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); (got == nil) != (level == 3 || level == 7 || level == 27) {
			t.Fatalf("native range %v: %v", level, got)
		}
	}
	if err = catalog.AdmitDomainFan(entity, "drive", value); err == nil {
		t.Fatal("old discrete observation admitted after definition replaced")
	}
}
func TestNHA212AtomicColorPresetAdmissionAndAbsence(t *testing.T) {
	catalog, _ := NewCapabilityCatalog(domain.NewUnitCatalog())
	entity := testNHA014Entity(t)
	d := &registry.CapabilityDescription{SchemaId: LightSchema, Major: 1, Endpoint: "light", DisplayName: "Synthetic lamp", Constraints: &registry.CapabilityDescription_Light{Light: &registry.LightConstraints{SetOn: true, Rgb: true, ColorTempMireds: &registry.NumberBounds{Minimum: 100, Maximum: 500}, Presets: &registry.ControlChoices{Choices: []*registry.ControlChoice{{Key: "scene:night/7"}}}}}}
	if err := catalog.ReplaceDriverCapabilitiesBatch([]DriverCapabilityRegistration{{Entity: entity, Description: d}}); err != nil {
		t.Fatal(err)
	}
	valid := &actions.SetLight{On: proto.Bool(true), Rgb: &registry.RgbColor{Red: proto.Uint32(0), Green: proto.Uint32(137), Blue: proto.Uint32(255)}, Preset: proto.String("scene:night/7")}
	cases := []struct {
		name   string
		mutate func(*actions.SetLight)
		want   bool
	}{
		{"atomic", func(*actions.SetLight) {}, true},
		{"exclusive", func(v *actions.SetLight) { v.ColorTempMireds = proto.Uint32(300) }, false},
		{"missing channel", func(v *actions.SetLight) { v.Rgb.Blue = nil }, false},
		{"channel bounds", func(v *actions.SetLight) { v.Rgb.Green = proto.Uint32(256) }, false},
		{"exact key", func(v *actions.SetLight) { v.Preset = proto.String("Scene:night/7") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := proto.CloneOf(valid)
			tc.mutate(v)
			cmd := testNHA014Command(LightSchema, "light", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: v}})
			if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); (err == nil) != tc.want {
				t.Fatal(err)
			}
		})
	}
	d.GetLight().Rgb = false
	d.GetLight().Presets = nil
	if err := catalog.ReplaceDriverCapabilitiesBatch([]DriverCapabilityRegistration{{Entity: entity, Description: d}}); err != nil {
		t.Fatal(err)
	}
	cmd := testNHA014Command(LightSchema, "light", &actions.ActionArgs{Arguments: &actions.ActionArgs_SetLight{SetLight: valid}})
	if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); err == nil {
		t.Fatal("withdrawn RGB/preset still actionable")
	}
}
