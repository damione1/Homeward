package dashboard

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"time"
)

func (p *apiProjector) sourceField(source *sourceField) (*api.DashboardFieldBinding, projectedBinding, error) {
	if source == nil {
		return nil, projectedBinding{}, ErrUnprojectableRecipe
	}
	alias, err := domain.ParseHAAlias(source.EntityId)
	if err != nil {
		return nil, projectedBinding{}, err
	}
	return p.field(alias, source.FieldKey)
}
func activityValues(binding projectedBinding, field *api.DashboardFieldBinding, values []string) ([]*api.DashboardLiteral, error) {
	out := make([]*api.DashboardLiteral, 0, len(values))
	for _, value := range values {
		literal, err := typedLiteral(binding, field.Field, value)
		if err != nil {
			return nil, err
		}
		// Source spellings can denote the same typed value (for example
		// auto and heat_cool). Activity predicates are sets, and clients
		// require unique values after normalization, not only unique text.
		duplicate := false
		for _, existing := range out {
			if proto.Equal(existing, literal) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		out = append(out, literal)
	}
	return out, nil
}
func (p *apiProjector) sourceCapability(raw string, matches func(projectedBinding) bool) (projectedBinding, error) {
	alias, err := domain.ParseHAAlias(raw)
	if err != nil {
		return projectedBinding{}, err
	}
	return uniqueBinding(p.bindings(alias), matches)
}
func (p *apiProjector) widget(card Card, view Location) (*api.DashboardWidget, error) {
	source := card.widget
	if source == nil {
		return nil, ErrUnprojectableRecipe
	}
	result := &api.DashboardWidget{ActionLabel: source.ActionLabel, EmptyLabel: source.EmptyLabel}
	if card.widgetAction != "" {
		target, ok := p.destination(view, card.widgetAction)
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Action = target
	}
	switch value := source.Content.(type) {
	case *sourceClockWeather:
		widget := &api.DashboardClockWeatherWidget{TimeZone: value.Timezone}
		if value.WeatherEntityId != "" {
			binding, err := p.sourceCapability(value.WeatherEntityId, func(b projectedBinding) bool { return b.native.Trait() == apiresources.Weather })
			if err != nil {
				return nil, err
			}
			widget.WeatherCapability = binding.resource.Name
		}
		result.Content = &api.DashboardWidget_ClockWeather{ClockWeather: widget}
	case *sourceRoom:
		widget := &api.DashboardRoomWidget{RoomId: value.RoomId}
		if value.Temperature != nil {
			field, _, err := p.sourceField(value.Temperature)
			if err == nil {
				widget.Temperature = field
			}
		}
		for _, raw := range value.ActivityEntityIds {
			alias, err := domain.ParseHAAlias(raw)
			if err != nil {
				return nil, err
			}
			field, binding, err := p.field(alias, "state")
			if err != nil {
				// Room observations are independent optional enrichments. An
				// unavailable or unauthorized slot must not erase an authorized
				// navigation destination, nor manufacture a replacement binding.
				continue
			}
			values, err := activityValues(binding, field, value.ActiveStates)
			if err != nil {
				return nil, err
			}
			widget.Activity = append(widget.Activity, &api.DashboardActivityBinding{Field: field, ActiveValues: values})
		}
		result.Content = &api.DashboardWidget_Room{Room: widget}
	case *sourceThermostat:
		alias, err := domain.ParseHAAlias(value.EntityId)
		if err != nil {
			return nil, err
		}
		entity, err := nativeEntity(alias, p)
		if err != nil {
			return nil, err
		}
		current, _, err := p.sourceField(value.CurrentTemperature)
		if err != nil {
			return nil, err
		}
		target, binding, err := p.sourceField(value.TargetTemperature)
		if err != nil || binding.native.Trait() != apiresources.Thermal {
			return nil, ErrUnprojectableRecipe
		}
		presentation := api.DashboardThermostatPresentation_DASHBOARD_THERMOSTAT_PRESENTATION_COMPACT
		switch value.Presentation {
		case thermostatDetail:
			presentation = api.DashboardThermostatPresentation_DASHBOARD_THERMOSTAT_PRESENTATION_DETAIL
		case thermostatSummary:
			presentation = api.DashboardThermostatPresentation_DASHBOARD_THERMOSTAT_PRESENTATION_SUMMARY
		}
		result.Content = &api.DashboardWidget_Thermostat{Thermostat: &api.DashboardThermostatWidget{Entity: entity, CurrentTemperature: current, TargetTemperature: target, Presentation: presentation}}
	case *sourceAgenda:
		if value.Limit < 1 || value.Limit > 1000 {
			return nil, ErrUnprojectableRecipe
		}
		if p.options.Calendar == nil {
			return nil, ErrUnprojectableRecipe
		}
		id, err := domain.ParseEntityID(value.CalendarId)
		if err != nil {
			return nil, err
		}
		name, ok := p.options.Calendar(id)
		if !ok || name.Home() != p.options.Home || name.String() == "" {
			return nil, ErrUnprojectableRecipe
		}
		result.Content = &api.DashboardWidget_Agenda{Agenda: &api.DashboardAgendaWidget{Calendar: name.String(), TimeZone: value.Timezone, TimeWindow: durationpb.New(time.Duration(value.WindowSeconds) * time.Second), Limit: int32(value.Limit)}}
	case *sourceTasks:
		if value.Limit < 1 || value.Limit > 1000 {
			return nil, ErrUnprojectableRecipe
		}
		if p.options.TaskList == nil {
			return nil, ErrUnprojectableRecipe
		}
		id, err := domain.ParseListID(value.ListId)
		if err != nil {
			return nil, err
		}
		name, ok := p.options.TaskList(id)
		if !ok || name.Home() != p.options.Home || name.String() == "" {
			return nil, ErrUnprojectableRecipe
		}
		presentation := api.DashboardTasksPresentation_DASHBOARD_TASKS_PRESENTATION_PREVIEW
		if value.Presentation == tasksShopping {
			presentation = api.DashboardTasksPresentation_DASHBOARD_TASKS_PRESENTATION_SHOPPING
		}
		result.Content = &api.DashboardWidget_Tasks{Tasks: &api.DashboardTasksWidget{TaskList: name.String(), Limit: int32(value.Limit), Presentation: presentation}}
	case *sourceSecurity:
		widget := &api.DashboardSecurityWidget{}
		for _, raw := range value.EntityIds {
			binding, err := p.sourceCapability(raw, func(b projectedBinding) bool {
				return b.native.Trait() == apiresources.Lock || b.native.Trait() == apiresources.Alarm || b.native.Trait() == apiresources.Cover
			})
			if err != nil {
				return nil, err
			}
			widget.Capabilities = append(widget.Capabilities, binding.resource.Name)
		}
		result.Content = &api.DashboardWidget_Security{Security: widget}
	case *sourceObservations:
		widget := &api.DashboardObservationsWidget{}
		for _, item := range value.Items {
			field, binding, err := p.sourceField(item.Field)
			if err != nil {
				return nil, err
			}
			values, err := activityValues(binding, field, item.ActiveStates)
			if err != nil {
				return nil, err
			}
			widget.Items = append(widget.Items, &api.DashboardObservationBinding{Field: field, Label: item.Label, ActiveValues: values, Icon: nativeCardIcon(item.Icon)})
		}
		result.Content = &api.DashboardWidget_Observations{Observations: widget}
	case *sourceNavigation:
		target, ok := p.destination(view, card.widgetNavigation)
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Content = &api.DashboardWidget_Navigation{Navigation: &api.DashboardNavigationWidget{Destination: target}}
	default:
		return nil, ErrUnprojectableRecipe
	}
	return result, nil
}

// A multi-trait source declares its composite control explicitly. Only its own
// projected siblings are subsumed; multiple independent slots remain ambiguous.
func primaryBindings(values []projectedBinding) []projectedBinding {
	out := make([]projectedBinding, 0, len(values))
	for _, value := range values {
		subsumed := false
		for _, other := range values {
			if other.native.Source() != value.native.Source() {
				continue
			}
			if other.native.Trait() == apiresources.Light && (value.native.Trait() == apiresources.Power || value.native.Trait() == apiresources.Level) {
				subsumed = true
			}
			if other.native.Trait() == apiresources.Thermal && value.native.Trait() == apiresources.Temperature {
				subsumed = true
			}
		}
		if !subsumed {
			out = append(out, value)
		}
	}
	return out
}
func featureMatches(b projectedBinding, kind FeatureKind) bool {
	switch kind {
	case FeatureToggle:
		return b.native.Trait() == apiresources.Power || (b.resource.GetLightControl() != nil && b.resource.GetLightControl().SupportsPower) || b.resource.GetHelper().GetDefinition().GetKind() == api.HelperKind_HELPER_KIND_BOOLEAN
	case FeatureBrightness:
		return b.resource.GetLightControl().GetBrightnessPercent() != nil || b.native.Trait() == apiresources.Level
	case FeatureColorTemperature:
		return b.resource.GetLightControl().GetColorTemperatureMireds() != nil
	case FeatureTargetTemperature:
		return b.resource.GetTemperatureControl().GetSetpointCelsius() != nil
	case FeatureClimateModes:
		return len(b.resource.GetTemperatureControl().GetModeOptions()) > 0
	case FeatureFanSpeed:
		return b.native.Trait() == apiresources.Fan
	case FeatureNumericInput:
		return b.resource.GetHelper().GetDefinition().GetKind() == api.HelperKind_HELPER_KIND_NUMBER || b.resource.GetHelper().GetDefinition().GetKind() == api.HelperKind_HELPER_KIND_COUNTER
	case FeatureCoverOpenClose, FeatureCoverPosition:
		return b.native.Trait() == apiresources.Cover
	case FeatureAlarmModes:
		return b.native.Trait() == apiresources.Alarm
	}
	return false
}
func (p *apiProjector) cardBinding(card Card) (string, []api.DashboardFeatureKind, error) {
	alias, present := card.Entity()
	if !present {
		if card.Kind() == ElementRecipeCard && len(card.Features()) == 0 {
			return "", nil, nil
		}
		return "", nil, ErrUnprojectableRecipe
	}
	features := card.Features()
	values := p.bindings(alias)
	if len(features) == 0 {
		values = primaryBindings(values)
	} else {
		// Prefer the declared composite when all authored controls fit it, excluding
		// only siblings from that same source. Never resolve cross-slot ambiguity.
		values = primaryBindings(values)
	}
	selected, err := uniqueBinding(values, func(b projectedBinding) bool {
		for _, feature := range features {
			if !featureMatches(b, feature.Kind()) {
				return false
			}
		}
		return true
	})
	if err != nil {
		return "", nil, err
	}
	out := make([]api.DashboardFeatureKind, 0, len(features))
	for _, feature := range features {
		out = append(out, nativeFeatureKind(feature.Kind()))
	}
	return selected.resource.Name, out, nil
}
