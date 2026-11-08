package apiresources

import api "github.com/damione1/Homeward/gen/home/api/v2"

// Modules inherit only their own capability's current admission. Observations
// remain in that same typed capability; no sibling supplies a missing value.
func projectControlModules(c *api.Capability) []*api.ControlModule {
	var out []*api.ControlModule
	add := func(m *api.ControlModule) {
		m.Capability, m.Etag, m.ActionAccess = c.Name, c.Etag, c.ActionAccess
		out = append(out, m)
	}
	power := func(supported bool) {
		if supported {
			add(&api.ControlModule{Role: api.ControlModuleRole_CONTROL_MODULE_ROLE_POWER, Definition: &api.ControlModule_Power{Power: true}})
		}
	}
	numeric := func(bounds *api.NumberBounds) {
		if bounds != nil {
			add(&api.ControlModule{Role: api.ControlModuleRole_CONTROL_MODULE_ROLE_RANGE, Definition: &api.ControlModule_Range{Range: bounds}})
		}
	}
	if v := c.GetPower(); v != nil {
		power(v.SupportsSet)
	}
	if v := c.GetLevel(); v != nil && v.SupportsSet && v.Bounds != nil {
		numeric(&api.NumberBounds{Minimum: v.Bounds.Minimum, Maximum: v.Bounds.Maximum, Step: v.Bounds.Step})
	}
	if v := c.GetLightControl(); v != nil {
		power(v.SupportsPower)
		numeric(v.BrightnessPercent)
		if v.ColorTemperatureMireds != nil || v.Rgb {
			add(&api.ControlModule{Role: api.ControlModuleRole_CONTROL_MODULE_ROLE_COLOR, Definition: &api.ControlModule_Color{Color: &api.ColorDefinition{TemperatureMireds: v.ColorTemperatureMireds, Rgb: v.Rgb}}})
		}
		if len(v.GetPresets().GetChoices()) > 0 {
			add(&api.ControlModule{Role: api.ControlModuleRole_CONTROL_MODULE_ROLE_PRESET, Definition: &api.ControlModule_Preset{Preset: v.Presets}})
		}
	}
	if v := c.GetFanControl(); v != nil {
		power(v.SupportsPower)
		numeric(v.GetLevel().GetRange())
		if steps := v.GetLevel().GetSteps(); len(steps.GetChoices()) > 0 {
			add(&api.ControlModule{Role: api.ControlModuleRole_CONTROL_MODULE_ROLE_STEPS, Definition: &api.ControlModule_Steps{Steps: steps}})
		}
	}
	if v := c.GetCover(); v != nil && v.Definition != nil {
		if len(v.Definition.Operations) > 0 {
			add(&api.ControlModule{Role: api.ControlModuleRole_CONTROL_MODULE_ROLE_MOTION, Definition: &api.ControlModule_Motion{Motion: v.Definition}})
		}
		if b := v.Definition.SetPositionPercent; b != nil {
			numeric(&api.NumberBounds{Minimum: b.Minimum, Maximum: b.Maximum, Step: b.Step})
		}
	}
	return out
}
