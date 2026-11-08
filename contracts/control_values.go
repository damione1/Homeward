package contracts

import (
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"github.com/damione1/Homeward/internal/domain"
)

func AdmitRGB(v *registry.RgbColor) (*domain.RGB, error) {
	if v == nil {
		return nil, nil
	}
	if !knownFieldsOnly(v.ProtoReflect()) || v.Red == nil || v.Green == nil || v.Blue == nil || *v.Red > 255 || *v.Green > 255 || *v.Blue > 255 {
		return nil, ErrInvalidWireState
	}
	return &domain.RGB{Red: uint8(*v.Red), Green: uint8(*v.Green), Blue: uint8(*v.Blue)}, nil
}
func WireRGB(v *domain.RGB) *registry.RgbColor {
	if v == nil {
		return nil
	}
	r, g, b := uint32(v.Red), uint32(v.Green), uint32(v.Blue)
	return &registry.RgbColor{Red: &r, Green: &g, Blue: &b}
}
func validControlChoices(v *registry.ControlChoices) bool {
	if v == nil {
		return true
	}
	if !knownFieldsOnly(v.ProtoReflect()) || len(v.Choices) > 256 {
		return false
	}
	seen := map[string]bool{}
	for _, c := range v.Choices {
		if c == nil || !knownFieldsOnly(c.ProtoReflect()) || !domain.ValidControlKey(c.Key) || seen[c.Key] {
			return false
		}
		seen[c.Key] = true
	}
	return true
}
func ContainsControlChoice(v *registry.ControlChoices, key string) bool {
	if v == nil {
		return false
	}
	for _, c := range v.Choices {
		if c != nil && c.Key == key {
			return true
		}
	}
	return false
}
func AdmitLightValue(v *state.LightValue) (domain.LightValue, error) {
	if v == nil || v.On == nil || !knownFieldsOnly(v.ProtoReflect()) {
		return domain.LightValue{}, ErrInvalidWireState
	}
	rgb, err := AdmitRGB(v.Rgb)
	if err != nil {
		return domain.LightValue{}, err
	}
	return domain.NewLightValue(domain.LightValueInput{On: *v.On, Brightness: v.BrightnessPercent, ColorTemperatureMireds: v.ColorTempMireds, RGB: rgb, Preset: v.Preset})
}
