package dashboard

import api "github.com/damione1/Homeward/gen/home/api/v2"

func controlRole(v string) api.ControlModuleRole {
	switch v {
	case "power":
		return api.ControlModuleRole_CONTROL_MODULE_ROLE_POWER
	case "range":
		return api.ControlModuleRole_CONTROL_MODULE_ROLE_RANGE
	case "steps":
		return api.ControlModuleRole_CONTROL_MODULE_ROLE_STEPS
	case "color":
		return api.ControlModuleRole_CONTROL_MODULE_ROLE_COLOR
	case "preset":
		return api.ControlModuleRole_CONTROL_MODULE_ROLE_PRESET
	case "motion":
		return api.ControlModuleRole_CONTROL_MODULE_ROLE_MOTION
	}
	return api.ControlModuleRole_CONTROL_MODULE_ROLE_UNSPECIFIED
}
func visualFamily(v string) api.ControlVisualFamily {
	switch v {
	case "binary":
		return api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_BINARY
	case "continuous_level":
		return api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_CONTINUOUS_LEVEL
	case "stepped_level":
		return api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_STEPPED_LEVEL
	case "blind":
		return api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_BLIND
	case "garage":
		return api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_GARAGE
	}
	return api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_UNSPECIFIED
}
func validVisualFamily(v string) bool {
	return visualFamily(v) != api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_UNSPECIFIED
}

func (p *apiProjector) controlComposition(card Card, primary string) (api.ControlVisualFamily, []*api.ControlModule, error) {
	family := visualFamily(card.visualFamily)
	var modules []*api.ControlModule
	var principal *api.Capability
	for _, b := range p.bindings(card.entity) {
		if b.resource.Name == primary {
			principal = b.resource
			break
		}
	}
	if principal != nil {
		modules = append(modules, principal.Modules...)
		if family == api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_UNSPECIFIED {
			switch {
			case principal.GetCover() != nil:
				family = api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_BLIND
			case principal.GetFanControl().GetLevel().GetSteps() != nil:
				family = api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_STEPPED_LEVEL
			case principal.GetLightControl().GetBrightnessPercent() != nil || principal.GetLevel() != nil || principal.GetFanControl().GetLevel().GetRange() != nil:
				family = api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_CONTINUOUS_LEVEL
			case principal.GetPower() != nil || principal.GetLightControl() != nil || principal.GetFanControl() != nil:
				family = api.ControlVisualFamily_CONTROL_VISUAL_FAMILY_BINARY
			}
		}
	}
	seen := map[string]bool{}
	for _, m := range modules {
		seen[m.Capability+":"+m.Role.String()] = true
	}
	for _, source := range card.controlModules {
		role := controlRole(source.role)
		var selected *api.ControlModule
		// Selection uses independently authorized exact capability bindings. A
		// family never supplies capabilities or borrows the principal's access.
		for _, b := range primaryBindings(p.bindings(source.entity)) {
			for _, m := range b.resource.Modules {
				if m.Role != role {
					continue
				}
				if selected != nil {
					return 0, nil, ErrUnprojectableRecipe
				}
				selected = m
			}
		}
		if selected == nil {
			return 0, nil, ErrUnprojectableRecipe
		}
		key := selected.Capability + ":" + selected.Role.String()
		if seen[key] {
			return 0, nil, ErrUnprojectableRecipe
		}
		seen[key] = true
		modules = append(modules, selected)
	}
	return family, modules, nil
}
