package dashboard

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	api "github.com/damione1/Homeward/gen/home/api/v2"
	"github.com/damione1/Homeward/internal/apiresources"
	"github.com/damione1/Homeward/internal/apitypes"
	"github.com/damione1/Homeward/internal/domain"
)

type apiProjector struct {
	options    APIProjection
	definition Definition
}
type projectedBinding struct {
	native   apiresources.Capability
	resource *api.Capability
}

func (p *apiProjector) bindings(alias domain.HAAlias) []projectedBinding {
	if p.options.Entity == nil || p.options.Capabilities == nil {
		return nil
	}
	entity, ok := p.options.Entity(alias)
	if !ok || !entity.Valid() || entity.Home() != p.options.Home {
		return nil
	}
	var out []projectedBinding
	for _, capability := range p.options.Capabilities(alias) {
		if capability.Trait() == apiresources.Opaque || capability.Trait() == apiresources.Media {
			continue
		}
		resource := capability.Resource()
		if resource == nil {
			return nil
		}
		name, err := apitypes.ParseCapabilityName(resource.Name)
		if err != nil || name.Entity() != entity {
			return nil
		}
		out = append(out, projectedBinding{capability, resource})
	}
	return out
}
func (p *apiProjector) destination(view Location, path string) (*api.DashboardDestination, bool) {
	if p.options.Navigation == nil || p.options.Dashboard == nil {
		return nil, false
	}
	target, ok := p.options.Navigation.Resolve(p.definition.Source().ID(), view, path)
	if !ok || target == nil {
		return nil, false
	}
	name, ok := p.options.Dashboard(target.dashboard)
	if !ok || !name.Valid() || name.Home() != p.options.Home {
		return nil, false
	}
	style := api.DashboardPresentation_DASHBOARD_PRESENTATION_PAGE
	if target.presentation == navigationPopup {
		style = api.DashboardPresentation_DASHBOARD_PRESENTATION_POPUP
	}
	return &api.DashboardDestination{Dashboard: name.String(), NodeId: target.node, Presentation: style}, true
}
func uniqueBinding(values []projectedBinding, matches func(projectedBinding) bool) (projectedBinding, error) {
	var selected projectedBinding
	for _, value := range values {
		if matches(value) {
			if selected.resource != nil {
				return projectedBinding{}, ErrUnprojectableRecipe
			}
			selected = value
		}
	}
	if selected.resource == nil {
		return projectedBinding{}, ErrUnprojectableRecipe
	}
	return selected, nil
}
func (p *apiProjector) command(alias domain.HAAlias, kind api.DashboardCommandKind) (string, error) {
	selected, err := uniqueBinding(p.bindings(alias), func(b projectedBinding) bool {
		switch kind {
		case api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_TOGGLE, api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_TURN_ON, api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_TURN_OFF:
			return b.native.Trait() == apiresources.Power || (b.resource.GetHelper() != nil && b.resource.GetHelper().GetDefinition().GetKind() == api.HelperKind_HELPER_KIND_BOOLEAN)
		case api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ACTIVATE:
			return b.resource.GetHelper() != nil && b.resource.GetHelper().GetDefinition().GetKind() == api.HelperKind_HELPER_KIND_BUTTON_PRESS
		case api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_COVER_OPEN, api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_COVER_CLOSE, api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_COVER_STOP:
			return b.native.Trait() == apiresources.Cover
		case api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_LOCK, api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_UNLOCK:
			return b.native.Trait() == apiresources.Lock
		case api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ALARM_ARM_HOME, api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ALARM_ARM_AWAY, api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ALARM_DISARM, api.DashboardCommandKind_DASHBOARD_COMMAND_KIND_ALARM_ARM_NIGHT:
			return b.native.Trait() == apiresources.Alarm
		default:
			return false
		}
	})
	if err != nil {
		return "", err
	}
	return selected.resource.Name, nil
}
func fieldKey(value string) (api.DashboardFieldKey, bool) {
	switch value {
	case "state":
		return api.DashboardFieldKey_DASHBOARD_FIELD_KEY_STATE, true
	case "value":
		return api.DashboardFieldKey_DASHBOARD_FIELD_KEY_VALUE, true
	case "current_temperature_celsius":
		return api.DashboardFieldKey_DASHBOARD_FIELD_KEY_CURRENT_TEMPERATURE_CELSIUS, true
	case "setpoint_celsius":
		return api.DashboardFieldKey_DASHBOARD_FIELD_KEY_SETPOINT_CELSIUS, true
	case "measurement":
		return api.DashboardFieldKey_DASHBOARD_FIELD_KEY_MEASUREMENT, true
	}
	return 0, false
}
func fieldMatches(b projectedBinding, key api.DashboardFieldKey) bool {
	switch key {
	case api.DashboardFieldKey_DASHBOARD_FIELD_KEY_CURRENT_TEMPERATURE_CELSIUS:
		return b.native.Trait() == apiresources.Temperature && b.resource.GetNumericMeasurement() != nil && b.resource.GetNumericMeasurement().Unit == "Cel"
	case api.DashboardFieldKey_DASHBOARD_FIELD_KEY_SETPOINT_CELSIUS:
		return b.native.Trait() == apiresources.Thermal
	case api.DashboardFieldKey_DASHBOARD_FIELD_KEY_MEASUREMENT:
		return b.resource.GetNumericMeasurement() != nil
	case api.DashboardFieldKey_DASHBOARD_FIELD_KEY_VALUE:
		return b.native.Trait() == apiresources.Helper || b.resource.GetNumericMeasurement() != nil || b.native.Trait() == apiresources.Binary
	case api.DashboardFieldKey_DASHBOARD_FIELD_KEY_STATE:
		switch b.native.Trait() {
		case apiresources.Power, apiresources.Thermal, apiresources.Helper, apiresources.Binary, apiresources.Cover, apiresources.Lock, apiresources.Alarm, apiresources.Fan, apiresources.Weather, apiresources.Presence:
			return true
		}
	}
	return false
}
func (p *apiProjector) field(alias domain.HAAlias, spelling string) (*api.DashboardFieldBinding, projectedBinding, error) {
	key, ok := fieldKey(spelling)
	if !ok {
		return nil, projectedBinding{}, ErrUnprojectableRecipe
	}
	selected, err := uniqueBinding(p.bindings(alias), func(b projectedBinding) bool { return fieldMatches(b, key) })
	if err != nil {
		return nil, projectedBinding{}, err
	}
	return &api.DashboardFieldBinding{Capability: selected.resource.Name, Field: key}, selected, nil
}
func (p *apiProjector) literalField(alias domain.HAAlias, spelling, value string) (*api.DashboardFieldBinding, *api.DashboardLiteral, error) {
	field, binding, err := p.field(alias, spelling)
	if err != nil {
		return nil, nil, err
	}
	literal, err := typedLiteral(binding, field.Field, value)
	return field, literal, err
}

var exactDecimal = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

func typedLiteral(b projectedBinding, field api.DashboardFieldKey, text string) (*api.DashboardLiteral, error) {
	if len(text) > 256 || strings.ContainsRune(text, 0) {
		return nil, ErrUnprojectableRecipe
	}
	result := &api.DashboardLiteral{}
	boolean := func() (bool, bool) {
		switch text {
		case "on", "true":
			return true, true
		case "off", "false":
			return false, true
		}
		return false, false
	}
	number := func() (float64, bool) {
		value, err := strconv.ParseFloat(text, 64)
		return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
	}
	enum := func(values map[string]int32, prefix string) (int32, bool) {
		value, ok := values[prefix+strings.ToUpper(text)]
		return value, ok && value != 0
	}
	if field == api.DashboardFieldKey_DASHBOARD_FIELD_KEY_SETPOINT_CELSIUS || field == api.DashboardFieldKey_DASHBOARD_FIELD_KEY_CURRENT_TEMPERATURE_CELSIUS {
		value, ok := number()
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_NumberValue{NumberValue: value}
		return result, nil
	}
	switch b.native.Trait() {
	case apiresources.Power:
		value, ok := boolean()
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		power := api.PowerValue_POWER_VALUE_OFF
		if value {
			power = api.PowerValue_POWER_VALUE_ON
		}
		result.Value = &api.DashboardLiteral_PowerValue{PowerValue: power}
	case apiresources.Binary:
		value, ok := boolean()
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_BooleanValue{BooleanValue: value}
	case apiresources.Temperature, apiresources.Humidity, apiresources.Measurement:
		if !exactDecimal.MatchString(text) {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_DecimalValue{DecimalValue: text}
	case apiresources.Thermal:
		// The HA source spelling denotes the same automatic heat/cool mode
		// normalized by the scene importer; it is not a public API value.
		if text == "heat_cool" {
			text = "auto"
		}
		value, ok := enum(api.TemperatureMode_value, "TEMPERATURE_MODE_")
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_TemperatureMode{TemperatureMode: api.TemperatureMode(value)}
	case apiresources.Cover:
		value, ok := enum(api.CoverState_value, "COVER_STATE_")
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_CoverState{CoverState: api.CoverState(value)}
	case apiresources.Lock:
		value, ok := enum(api.LockState_value, "LOCK_STATE_")
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_LockState{LockState: api.LockState(value)}
	case apiresources.Alarm:
		value, ok := enum(api.AlarmState_value, "ALARM_STATE_")
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_AlarmState{AlarmState: api.AlarmState(value)}
	case apiresources.Fan:
		value, ok := enum(api.FanSpeed_value, "FAN_SPEED_")
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_FanSpeed{FanSpeed: api.FanSpeed(value)}
	case apiresources.Presence:
		values := map[string]api.Residence{"home": api.Residence_RESIDENCE_HOME, "not_home": api.Residence_RESIDENCE_AWAY, "unknown": api.Residence_RESIDENCE_UNKNOWN}
		value, ok := values[text]
		if !ok {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_Residence{Residence: value}
	case apiresources.Weather:
		spellings := map[string]string{"sunny": "CLEAR_DAY", "clear-night": "CLEAR_NIGHT", "partlycloudy": "PARTLY_CLOUDY", "snowy-rainy": "SNOWY_RAINY", "lightning-rainy": "LIGHTNING_RAINY", "windy-variant": "WINDY_VARIANT"}
		name, ok := spellings[text]
		if !ok {
			name = strings.ToUpper(text)
		}
		value, ok := api.WeatherCondition_value["WEATHER_CONDITION_"+name]
		if !ok || value == 0 {
			return nil, ErrUnprojectableRecipe
		}
		result.Value = &api.DashboardLiteral_WeatherCondition{WeatherCondition: api.WeatherCondition(value)}
	case apiresources.Helper:
		switch b.resource.GetHelper().GetDefinition().GetKind() {
		case api.HelperKind_HELPER_KIND_BOOLEAN:
			value, ok := boolean()
			if !ok {
				return nil, ErrUnprojectableRecipe
			}
			result.Value = &api.DashboardLiteral_BooleanValue{BooleanValue: value}
		case api.HelperKind_HELPER_KIND_COUNTER:
			value, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, ErrUnprojectableRecipe
			}
			result.Value = &api.DashboardLiteral_IntegerValue{IntegerValue: value}
		case api.HelperKind_HELPER_KIND_NUMBER:
			value, ok := number()
			if !ok {
				return nil, ErrUnprojectableRecipe
			}
			result.Value = &api.DashboardLiteral_NumberValue{NumberValue: value}
		case api.HelperKind_HELPER_KIND_TEXT, api.HelperKind_HELPER_KIND_SELECT:
			result.Value = &api.DashboardLiteral_TextValue{TextValue: text}
		default:
			return nil, ErrUnprojectableRecipe
		}
	default:
		return nil, ErrUnprojectableRecipe
	}
	return result, nil
}
