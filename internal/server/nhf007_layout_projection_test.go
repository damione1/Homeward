package server

import (
	imported "github.com/damione1/Homeward/compat/ha/dashboard"
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/authorization"
	"github.com/damione1/Homeward/internal/domain"
)

func projectCatalogueLayouts(results []imported.Result, resolve func(domain.HAAlias) (domain.EntityID, bool)) ([]*api.DashboardLayout, error) {
	definitions := make([]imported.Definition, 0, len(results))
	for _, result := range results {
		definition, ok := result.Definition()
		if !ok {
			return nil, ErrInvalidPublicPlan
		}
		definitions = append(definitions, definition)
	}
	navigation, err := imported.NewNavigationIndex(definitions)
	if err != nil {
		return nil, err
	}
	home, err := apitypes.ParseHomeName("homes/qualification-dashboard")
	if err != nil {
		return nil, err
	}
	options := imported.APIProjection{Home: home, Navigation: &navigation, Entity: func(alias domain.HAAlias) (apitypes.EntityName, bool) {
		native, ok := resolve(alias)
		if !ok {
			return apitypes.EntityName{}, false
		}
		name, err := apiresources.EntityName(home, native)
		return name, err == nil
	}, Dashboard: func(id authorization.DashboardID) (apitypes.DashboardName, bool) {
		name, err := apiresources.DashboardNameForIdentity(home, id)
		return name, err == nil
	}}
	// This corpus declares no native slots. Commands/observations therefore stay
	// inert; source navigation is projected only through this captured index.
	layouts := make([]*api.DashboardLayout, 0, len(results))
	for _, definition := range definitions {
		layout, err := imported.ProjectDashboardDefinition(definition, options)
		if err != nil {
			return nil, err
		}
		layouts = append(layouts, layout)
	}
	return layouts, nil
}
func projectDashboardLayout(result imported.Result, resolve func(domain.HAAlias) (domain.EntityID, bool)) (*api.DashboardLayout, error) {
	layouts, err := projectCatalogueLayouts([]imported.Result{result}, resolve)
	if err != nil {
		return nil, err
	}
	return layouts[0], nil
}
