// Explicit private-source to public conversions. No protobuf integer casts or raw payload forwarding.
package apiresources

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	environment "github.com/damione1/Homeward/gen/home/environment/v1"
	helpers "github.com/damione1/Homeward/gen/home/helpers/v1"
	solar "github.com/damione1/Homeward/gen/home/solar/v1"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"math"
)

func convertCoverState(v controls.CoverState) (api.CoverState, error) {
	switch v {
	case controls.CoverState_COVER_STATE_UNSPECIFIED:
		return api.CoverState_COVER_STATE_UNSPECIFIED, nil
	case controls.CoverState_COVER_STATE_OPEN:
		return api.CoverState_COVER_STATE_OPEN, nil
	case controls.CoverState_COVER_STATE_CLOSED:
		return api.CoverState_COVER_STATE_CLOSED, nil
	case controls.CoverState_COVER_STATE_OPENING:
		return api.CoverState_COVER_STATE_OPENING, nil
	case controls.CoverState_COVER_STATE_CLOSING:
		return api.CoverState_COVER_STATE_CLOSING, nil
	default:
		return 0, ErrProjection
	}
}

func convertCoverOperation(v controls.CoverOperation) (api.CoverOperation, error) {
	switch v {
	case controls.CoverOperation_COVER_OPERATION_UNSPECIFIED:
		return api.CoverOperation_COVER_OPERATION_UNSPECIFIED, nil
	case controls.CoverOperation_COVER_OPERATION_OPEN:
		return api.CoverOperation_COVER_OPERATION_OPEN, nil
	case controls.CoverOperation_COVER_OPERATION_CLOSE:
		return api.CoverOperation_COVER_OPERATION_CLOSE, nil
	case controls.CoverOperation_COVER_OPERATION_STOP:
		return api.CoverOperation_COVER_OPERATION_STOP, nil
	default:
		return 0, ErrProjection
	}
}

func convertLockState(v controls.LockState) (api.LockState, error) {
	switch v {
	case controls.LockState_LOCK_STATE_UNSPECIFIED:
		return api.LockState_LOCK_STATE_UNSPECIFIED, nil
	case controls.LockState_LOCK_STATE_LOCKED:
		return api.LockState_LOCK_STATE_LOCKED, nil
	case controls.LockState_LOCK_STATE_UNLOCKED:
		return api.LockState_LOCK_STATE_UNLOCKED, nil
	case controls.LockState_LOCK_STATE_LOCKING:
		return api.LockState_LOCK_STATE_LOCKING, nil
	case controls.LockState_LOCK_STATE_UNLOCKING:
		return api.LockState_LOCK_STATE_UNLOCKING, nil
	case controls.LockState_LOCK_STATE_JAMMED:
		return api.LockState_LOCK_STATE_JAMMED, nil
	case controls.LockState_LOCK_STATE_OPENING:
		return api.LockState_LOCK_STATE_OPENING, nil
	case controls.LockState_LOCK_STATE_OPEN:
		return api.LockState_LOCK_STATE_OPEN, nil
	default:
		return 0, ErrProjection
	}
}

func convertLockOperation(v controls.LockOperation) (api.LockOperation, error) {
	switch v {
	case controls.LockOperation_LOCK_OPERATION_UNSPECIFIED:
		return api.LockOperation_LOCK_OPERATION_UNSPECIFIED, nil
	case controls.LockOperation_LOCK_OPERATION_LOCK:
		return api.LockOperation_LOCK_OPERATION_LOCK, nil
	case controls.LockOperation_LOCK_OPERATION_UNLOCK:
		return api.LockOperation_LOCK_OPERATION_UNLOCK, nil
	default:
		return 0, ErrProjection
	}
}

func convertAlarmState(v controls.AlarmState) (api.AlarmState, error) {
	switch v {
	case controls.AlarmState_ALARM_STATE_UNSPECIFIED:
		return api.AlarmState_ALARM_STATE_UNSPECIFIED, nil
	case controls.AlarmState_ALARM_STATE_DISARMED:
		return api.AlarmState_ALARM_STATE_DISARMED, nil
	case controls.AlarmState_ALARM_STATE_ARMED_HOME:
		return api.AlarmState_ALARM_STATE_ARMED_HOME, nil
	case controls.AlarmState_ALARM_STATE_ARMED_AWAY:
		return api.AlarmState_ALARM_STATE_ARMED_AWAY, nil
	case controls.AlarmState_ALARM_STATE_ARMED_NIGHT:
		return api.AlarmState_ALARM_STATE_ARMED_NIGHT, nil
	case controls.AlarmState_ALARM_STATE_ARMED_VACATION:
		return api.AlarmState_ALARM_STATE_ARMED_VACATION, nil
	case controls.AlarmState_ALARM_STATE_ARMED_CUSTOM_BYPASS:
		return api.AlarmState_ALARM_STATE_ARMED_CUSTOM_BYPASS, nil
	case controls.AlarmState_ALARM_STATE_PENDING:
		return api.AlarmState_ALARM_STATE_PENDING, nil
	case controls.AlarmState_ALARM_STATE_ARMING:
		return api.AlarmState_ALARM_STATE_ARMING, nil
	case controls.AlarmState_ALARM_STATE_DISARMING:
		return api.AlarmState_ALARM_STATE_DISARMING, nil
	case controls.AlarmState_ALARM_STATE_TRIGGERED:
		return api.AlarmState_ALARM_STATE_TRIGGERED, nil
	default:
		return 0, ErrProjection
	}
}

func convertAlarmOperation(v controls.AlarmOperation) (api.AlarmOperation, error) {
	switch v {
	case controls.AlarmOperation_ALARM_OPERATION_UNSPECIFIED:
		return api.AlarmOperation_ALARM_OPERATION_UNSPECIFIED, nil
	case controls.AlarmOperation_ALARM_OPERATION_ARM_HOME:
		return api.AlarmOperation_ALARM_OPERATION_ARM_HOME, nil
	case controls.AlarmOperation_ALARM_OPERATION_ARM_AWAY:
		return api.AlarmOperation_ALARM_OPERATION_ARM_AWAY, nil
	case controls.AlarmOperation_ALARM_OPERATION_ARM_NIGHT:
		return api.AlarmOperation_ALARM_OPERATION_ARM_NIGHT, nil
	case controls.AlarmOperation_ALARM_OPERATION_DISARM:
		return api.AlarmOperation_ALARM_OPERATION_DISARM, nil
	default:
		return 0, ErrProjection
	}
}

func convertSolarEventKind(v solar.SolarEventKind) (api.SolarEventKind, error) {
	switch v {
	case solar.SolarEventKind_SOLAR_EVENT_KIND_UNSPECIFIED:
		return api.SolarEventKind_SOLAR_EVENT_KIND_UNSPECIFIED, nil
	case solar.SolarEventKind_SOLAR_EVENT_KIND_DAWN:
		return api.SolarEventKind_SOLAR_EVENT_KIND_DAWN, nil
	case solar.SolarEventKind_SOLAR_EVENT_KIND_DUSK:
		return api.SolarEventKind_SOLAR_EVENT_KIND_DUSK, nil
	case solar.SolarEventKind_SOLAR_EVENT_KIND_MIDNIGHT:
		return api.SolarEventKind_SOLAR_EVENT_KIND_MIDNIGHT, nil
	case solar.SolarEventKind_SOLAR_EVENT_KIND_NOON:
		return api.SolarEventKind_SOLAR_EVENT_KIND_NOON, nil
	case solar.SolarEventKind_SOLAR_EVENT_KIND_RISING:
		return api.SolarEventKind_SOLAR_EVENT_KIND_RISING, nil
	case solar.SolarEventKind_SOLAR_EVENT_KIND_SETTING:
		return api.SolarEventKind_SOLAR_EVENT_KIND_SETTING, nil
	default:
		return 0, ErrProjection
	}
}

func convertSolarHorizon(v solar.SolarHorizon) (api.SolarHorizon, error) {
	switch v {
	case solar.SolarHorizon_SOLAR_HORIZON_UNSPECIFIED:
		return api.SolarHorizon_SOLAR_HORIZON_UNSPECIFIED, nil
	case solar.SolarHorizon_SOLAR_HORIZON_ABOVE:
		return api.SolarHorizon_SOLAR_HORIZON_ABOVE, nil
	case solar.SolarHorizon_SOLAR_HORIZON_BELOW:
		return api.SolarHorizon_SOLAR_HORIZON_BELOW, nil
	default:
		return 0, ErrProjection
	}
}

func convertSolarAbsenceReason(v solar.SolarAbsenceReason) (api.SolarAbsenceReason, error) {
	switch v {
	case solar.SolarAbsenceReason_SOLAR_ABSENCE_REASON_UNSPECIFIED:
		return api.SolarAbsenceReason_SOLAR_ABSENCE_REASON_UNSPECIFIED, nil
	case solar.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_TRANSIT_ON_DATE:
		return api.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_TRANSIT_ON_DATE, nil
	case solar.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON:
		return api.SolarAbsenceReason_SOLAR_ABSENCE_REASON_NO_EVENT_WITHIN_SEARCH_HORIZON, nil
	default:
		return 0, ErrProjection
	}
}

func convertWeatherCondition(v environment.WeatherCondition) (api.WeatherCondition, error) {
	switch v {
	case environment.WeatherCondition_WEATHER_CONDITION_UNSPECIFIED:
		return api.WeatherCondition_WEATHER_CONDITION_UNSPECIFIED, nil
	case environment.WeatherCondition_WEATHER_CONDITION_UNKNOWN:
		return api.WeatherCondition_WEATHER_CONDITION_UNKNOWN, nil
	case environment.WeatherCondition_WEATHER_CONDITION_CLEAR_DAY:
		return api.WeatherCondition_WEATHER_CONDITION_CLEAR_DAY, nil
	case environment.WeatherCondition_WEATHER_CONDITION_CLEAR_NIGHT:
		return api.WeatherCondition_WEATHER_CONDITION_CLEAR_NIGHT, nil
	case environment.WeatherCondition_WEATHER_CONDITION_CLOUDY:
		return api.WeatherCondition_WEATHER_CONDITION_CLOUDY, nil
	case environment.WeatherCondition_WEATHER_CONDITION_PARTLY_CLOUDY:
		return api.WeatherCondition_WEATHER_CONDITION_PARTLY_CLOUDY, nil
	case environment.WeatherCondition_WEATHER_CONDITION_RAINY:
		return api.WeatherCondition_WEATHER_CONDITION_RAINY, nil
	case environment.WeatherCondition_WEATHER_CONDITION_POURING:
		return api.WeatherCondition_WEATHER_CONDITION_POURING, nil
	case environment.WeatherCondition_WEATHER_CONDITION_SNOWY:
		return api.WeatherCondition_WEATHER_CONDITION_SNOWY, nil
	case environment.WeatherCondition_WEATHER_CONDITION_SNOWY_RAINY:
		return api.WeatherCondition_WEATHER_CONDITION_SNOWY_RAINY, nil
	case environment.WeatherCondition_WEATHER_CONDITION_HAIL:
		return api.WeatherCondition_WEATHER_CONDITION_HAIL, nil
	case environment.WeatherCondition_WEATHER_CONDITION_LIGHTNING:
		return api.WeatherCondition_WEATHER_CONDITION_LIGHTNING, nil
	case environment.WeatherCondition_WEATHER_CONDITION_LIGHTNING_RAINY:
		return api.WeatherCondition_WEATHER_CONDITION_LIGHTNING_RAINY, nil
	case environment.WeatherCondition_WEATHER_CONDITION_WINDY:
		return api.WeatherCondition_WEATHER_CONDITION_WINDY, nil
	case environment.WeatherCondition_WEATHER_CONDITION_WINDY_VARIANT:
		return api.WeatherCondition_WEATHER_CONDITION_WINDY_VARIANT, nil
	case environment.WeatherCondition_WEATHER_CONDITION_FOG:
		return api.WeatherCondition_WEATHER_CONDITION_FOG, nil
	case environment.WeatherCondition_WEATHER_CONDITION_EXCEPTIONAL:
		return api.WeatherCondition_WEATHER_CONDITION_EXCEPTIONAL, nil
	default:
		return 0, ErrProjection
	}
}

func convertTemperatureUnit(v environment.TemperatureUnit) (api.TemperatureUnit, error) {
	switch v {
	case environment.TemperatureUnit_TEMPERATURE_UNIT_UNSPECIFIED:
		return api.TemperatureUnit_TEMPERATURE_UNIT_UNSPECIFIED, nil
	case environment.TemperatureUnit_TEMPERATURE_UNIT_CELSIUS:
		return api.TemperatureUnit_TEMPERATURE_UNIT_CELSIUS, nil
	case environment.TemperatureUnit_TEMPERATURE_UNIT_FAHRENHEIT:
		return api.TemperatureUnit_TEMPERATURE_UNIT_FAHRENHEIT, nil
	case environment.TemperatureUnit_TEMPERATURE_UNIT_KELVIN:
		return api.TemperatureUnit_TEMPERATURE_UNIT_KELVIN, nil
	default:
		return 0, ErrProjection
	}
}

func convertSpeedUnit(v environment.SpeedUnit) (api.SpeedUnit, error) {
	switch v {
	case environment.SpeedUnit_SPEED_UNIT_UNSPECIFIED:
		return api.SpeedUnit_SPEED_UNIT_UNSPECIFIED, nil
	case environment.SpeedUnit_SPEED_UNIT_METERS_PER_SECOND:
		return api.SpeedUnit_SPEED_UNIT_METERS_PER_SECOND, nil
	case environment.SpeedUnit_SPEED_UNIT_KILOMETERS_PER_HOUR:
		return api.SpeedUnit_SPEED_UNIT_KILOMETERS_PER_HOUR, nil
	case environment.SpeedUnit_SPEED_UNIT_MILES_PER_HOUR:
		return api.SpeedUnit_SPEED_UNIT_MILES_PER_HOUR, nil
	case environment.SpeedUnit_SPEED_UNIT_KNOTS:
		return api.SpeedUnit_SPEED_UNIT_KNOTS, nil
	default:
		return 0, ErrProjection
	}
}

func convertPrecipitationUnit(v environment.PrecipitationUnit) (api.PrecipitationUnit, error) {
	switch v {
	case environment.PrecipitationUnit_PRECIPITATION_UNIT_UNSPECIFIED:
		return api.PrecipitationUnit_PRECIPITATION_UNIT_UNSPECIFIED, nil
	case environment.PrecipitationUnit_PRECIPITATION_UNIT_MILLIMETERS:
		return api.PrecipitationUnit_PRECIPITATION_UNIT_MILLIMETERS, nil
	case environment.PrecipitationUnit_PRECIPITATION_UNIT_INCHES:
		return api.PrecipitationUnit_PRECIPITATION_UNIT_INCHES, nil
	default:
		return 0, ErrProjection
	}
}

func convertForecastKind(v environment.ForecastKind) (api.ForecastKind, error) {
	switch v {
	case environment.ForecastKind_FORECAST_KIND_UNSPECIFIED:
		return api.ForecastKind_FORECAST_KIND_UNSPECIFIED, nil
	case environment.ForecastKind_FORECAST_KIND_HOURLY:
		return api.ForecastKind_FORECAST_KIND_HOURLY, nil
	case environment.ForecastKind_FORECAST_KIND_DAILY:
		return api.ForecastKind_FORECAST_KIND_DAILY, nil
	default:
		return 0, ErrProjection
	}
}

func convertResidence(v environment.Residence) (api.Residence, error) {
	switch v {
	case environment.Residence_RESIDENCE_UNSPECIFIED:
		return api.Residence_RESIDENCE_UNSPECIFIED, nil
	case environment.Residence_RESIDENCE_HOME:
		return api.Residence_RESIDENCE_HOME, nil
	case environment.Residence_RESIDENCE_AWAY:
		return api.Residence_RESIDENCE_AWAY, nil
	case environment.Residence_RESIDENCE_UNKNOWN:
		return api.Residence_RESIDENCE_UNKNOWN, nil
	default:
		return 0, ErrProjection
	}
}

func convertPresenceConfidence(v environment.PresenceConfidence) (api.PresenceConfidence, error) {
	switch v {
	case environment.PresenceConfidence_PRESENCE_CONFIDENCE_UNSPECIFIED:
		return api.PresenceConfidence_PRESENCE_CONFIDENCE_UNSPECIFIED, nil
	case environment.PresenceConfidence_PRESENCE_CONFIDENCE_UNKNOWN:
		return api.PresenceConfidence_PRESENCE_CONFIDENCE_UNKNOWN, nil
	case environment.PresenceConfidence_PRESENCE_CONFIDENCE_LOW:
		return api.PresenceConfidence_PRESENCE_CONFIDENCE_LOW, nil
	case environment.PresenceConfidence_PRESENCE_CONFIDENCE_MEDIUM:
		return api.PresenceConfidence_PRESENCE_CONFIDENCE_MEDIUM, nil
	case environment.PresenceConfidence_PRESENCE_CONFIDENCE_HIGH:
		return api.PresenceConfidence_PRESENCE_CONFIDENCE_HIGH, nil
	default:
		return 0, ErrProjection
	}
}

func convertHelperKind(v helpers.HelperKind) (api.HelperKind, error) {
	switch v {
	case helpers.HelperKind_HELPER_KIND_UNSPECIFIED:
		return api.HelperKind_HELPER_KIND_UNSPECIFIED, nil
	case helpers.HelperKind_HELPER_KIND_BOOLEAN:
		return api.HelperKind_HELPER_KIND_BOOLEAN, nil
	case helpers.HelperKind_HELPER_KIND_NUMBER:
		return api.HelperKind_HELPER_KIND_NUMBER, nil
	case helpers.HelperKind_HELPER_KIND_COUNTER:
		return api.HelperKind_HELPER_KIND_COUNTER, nil
	case helpers.HelperKind_HELPER_KIND_BUTTON_PRESS:
		return api.HelperKind_HELPER_KIND_BUTTON_PRESS, nil
	case helpers.HelperKind_HELPER_KIND_DATE:
		return api.HelperKind_HELPER_KIND_DATE, nil
	case helpers.HelperKind_HELPER_KIND_TIME:
		return api.HelperKind_HELPER_KIND_TIME, nil
	case helpers.HelperKind_HELPER_KIND_DATETIME:
		return api.HelperKind_HELPER_KIND_DATETIME, nil
	case helpers.HelperKind_HELPER_KIND_SELECT:
		return api.HelperKind_HELPER_KIND_SELECT, nil
	case helpers.HelperKind_HELPER_KIND_TEXT:
		return api.HelperKind_HELPER_KIND_TEXT, nil
	default:
		return 0, ErrProjection
	}
}

func convertHelperOperation(v helpers.HelperOperation) (api.HelperOperation, error) {
	switch v {
	case helpers.HelperOperation_HELPER_OPERATION_UNSPECIFIED:
		return api.HelperOperation_HELPER_OPERATION_UNSPECIFIED, nil
	case helpers.HelperOperation_HELPER_OPERATION_SET:
		return api.HelperOperation_HELPER_OPERATION_SET, nil
	case helpers.HelperOperation_HELPER_OPERATION_TOGGLE:
		return api.HelperOperation_HELPER_OPERATION_TOGGLE, nil
	case helpers.HelperOperation_HELPER_OPERATION_INCREMENT:
		return api.HelperOperation_HELPER_OPERATION_INCREMENT, nil
	case helpers.HelperOperation_HELPER_OPERATION_DECREMENT:
		return api.HelperOperation_HELPER_OPERATION_DECREMENT, nil
	case helpers.HelperOperation_HELPER_OPERATION_RESET:
		return api.HelperOperation_HELPER_OPERATION_RESET, nil
	case helpers.HelperOperation_HELPER_OPERATION_PRESS:
		return api.HelperOperation_HELPER_OPERATION_PRESS, nil
	case helpers.HelperOperation_HELPER_OPERATION_NEXT:
		return api.HelperOperation_HELPER_OPERATION_NEXT, nil
	case helpers.HelperOperation_HELPER_OPERATION_PREVIOUS:
		return api.HelperOperation_HELPER_OPERATION_PREVIOUS, nil
	default:
		return 0, ErrProjection
	}
}

func convertHelperReason(v helpers.HelperReason) (api.HelperReason, error) {
	switch v {
	case helpers.HelperReason_HELPER_REASON_UNSPECIFIED:
		return api.HelperReason_HELPER_REASON_UNSPECIFIED, nil
	case helpers.HelperReason_HELPER_REASON_NO_AUTHORIZED_TARGET:
		return api.HelperReason_HELPER_REASON_NO_AUTHORIZED_TARGET, nil
	default:
		return 0, ErrProjection
	}
}

func convertPercentBounds(v *controls.PercentBounds) (*api.PercentageBounds, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.PercentageBounds{}

	{
		r.Minimum = v.Minimum
	}

	{
		r.Maximum = v.Maximum
	}

	{
		if v.Step != nil {
			convertedValue := *v.Step
			r.Step = &convertedValue
		}
	}
	return r, nil
}

func convertCoverConstraints(v *controls.CoverConstraints) (*api.CoverDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.CoverDefinition{}

	{
		for _, item := range v.Operations {
			converted, err := convertCoverOperation(item)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.Operations = append(r.Operations, convertedValue)
		}
	}

	{
		converted, err := convertPercentBounds(v.SetPositionPercent)
		if err != nil {
			return nil, err
		}
		r.SetPositionPercent = converted
	}

	{
		for _, item := range v.ObservedStates {
			converted, err := convertCoverState(item)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.ObservedStates = append(r.ObservedStates, convertedValue)
		}
	}

	{
		r.ObservesPositionPercent = v.ObservesPositionPercent
	}

	{
		r.ObservesObstruction = v.ObservesObstruction
	}
	return r, nil
}

func convertCoverValue(v *controls.CoverValue) (*api.CoverValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.CoverValue{}

	{
		if v.State != nil {
			converted, err := convertCoverState(*v.State)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.State = &convertedValue
		}
	}

	{
		if v.PositionPercent != nil {
			convertedValue := *v.PositionPercent
			r.PositionPercent = &convertedValue
		}
	}

	{
		if v.Obstructed != nil {
			convertedValue := *v.Obstructed
			r.Obstructed = &convertedValue
		}
	}
	return r, nil
}

func convertLockConstraints(v *controls.LockConstraints) (*api.LockDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.LockDefinition{}

	{
		for _, item := range v.Operations {
			converted, err := convertLockOperation(item)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.Operations = append(r.Operations, convertedValue)
		}
	}

	{
		for _, item := range v.ObservedStates {
			converted, err := convertLockState(item)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.ObservedStates = append(r.ObservedStates, convertedValue)
		}
	}
	return r, nil
}

func convertLockValue(v *controls.LockValue) (*api.LockValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.LockValue{}

	{
		converted, err := convertLockState(v.State)
		if err != nil {
			return nil, err
		}
		r.State = converted
	}
	return r, nil
}

func convertAlarmConstraints(v *controls.AlarmConstraints) (*api.AlarmDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.AlarmDefinition{}
	for _, operation := range v.CodeRequiredOperations {
		converted, err := convertAlarmOperation(operation)
		if err != nil {
			return nil, err
		}
		r.CodeRequiredOperations = append(r.CodeRequiredOperations, converted)
	}

	{
		for _, item := range v.Operations {
			converted, err := convertAlarmOperation(item)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.Operations = append(r.Operations, convertedValue)
		}
	}

	{
		for _, item := range v.ObservedStates {
			converted, err := convertAlarmState(item)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.ObservedStates = append(r.ObservedStates, convertedValue)
		}
	}
	return r, nil
}

func convertAlarmValue(v *controls.AlarmValue) (*api.AlarmValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.AlarmValue{}

	{
		converted, err := convertAlarmState(v.State)
		if err != nil {
			return nil, err
		}
		r.State = converted
	}
	return r, nil
}

func convertSolarOccurrence(v *solar.SolarOccurrence) (*api.SolarOccurrence, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.SolarOccurrence{}
	switch x := v.Occurrence.(type) {
	case *solar.SolarOccurrence_At:
		if x == nil {
			return nil, ErrProjection
		}
		convertedValue := proto.CloneOf(x.At)
		r.Occurrence = &api.SolarOccurrence_EventTime{EventTime: convertedValue}
	case *solar.SolarOccurrence_Absent:
		if x == nil {
			return nil, ErrProjection
		}
		converted, err := convertSolarAbsenceReason(x.Absent)
		if err != nil {
			return nil, err
		}
		convertedValue := converted
		r.Occurrence = &api.SolarOccurrence_Absent{Absent: convertedValue}
	case nil:
	default:
		return nil, ErrProjection
	}
	return r, nil
}

func convertSolarValue(v *solar.SolarValue) (*api.SolarValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.SolarValue{}

	{
		converted, err := convertSolarHorizon(v.Horizon)
		if err != nil {
			return nil, err
		}
		r.Horizon = converted
	}

	{
		if v.ElevationDegrees != nil {
			convertedValue := *v.ElevationDegrees
			r.ElevationDegrees = &convertedValue
		}
	}

	{
		if v.AzimuthDegrees != nil {
			convertedValue := *v.AzimuthDegrees
			r.AzimuthDegrees = &convertedValue
		}
	}

	{
		if v.Rising != nil {
			convertedValue := *v.Rising
			r.Rising = &convertedValue
		}
	}

	{
		converted, err := convertSolarOccurrence(v.NextDawn)
		if err != nil {
			return nil, err
		}
		r.NextDawn = converted
	}

	{
		converted, err := convertSolarOccurrence(v.NextDusk)
		if err != nil {
			return nil, err
		}
		r.NextDusk = converted
	}

	{
		converted, err := convertSolarOccurrence(v.NextMidnight)
		if err != nil {
			return nil, err
		}
		r.NextMidnight = converted
	}

	{
		converted, err := convertSolarOccurrence(v.NextNoon)
		if err != nil {
			return nil, err
		}
		r.NextNoon = converted
	}

	{
		converted, err := convertSolarOccurrence(v.NextRising)
		if err != nil {
			return nil, err
		}
		r.NextRising = converted
	}

	{
		converted, err := convertSolarOccurrence(v.NextSetting)
		if err != nil {
			return nil, err
		}
		r.NextSetting = converted
	}
	return r, nil
}

func convertSolarEventValue(v *solar.SolarEventValue) (*api.SolarEventValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.SolarEventValue{}

	{
		converted, err := convertSolarEventKind(v.Event)
		if err != nil {
			return nil, err
		}
		r.Event = converted
	}

	{
		converted, err := convertSolarOccurrence(v.Occurrence)
		if err != nil {
			return nil, err
		}
		r.Occurrence = converted
	}
	return r, nil
}

func convertSolarConstraints(v *solar.SolarConstraints) (*api.SolarDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.SolarDefinition{}
	return r, nil
}

func convertSolarEventConstraints(v *solar.SolarEventConstraints) (*api.SolarEventDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.SolarEventDefinition{}

	{
		converted, err := convertSolarEventKind(v.Event)
		if err != nil {
			return nil, err
		}
		r.Event = converted
	}
	return r, nil
}

func convertTemperature(v *environment.Temperature) (*api.Temperature, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.Temperature{}

	{
		if v.Value != nil {
			convertedValue := *v.Value
			r.Value = &convertedValue
		}
	}

	{
		converted, err := convertTemperatureUnit(v.Unit)
		if err != nil {
			return nil, err
		}
		r.Unit = converted
	}
	return r, nil
}

func convertWind(v *environment.Wind) (*api.Wind, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.Wind{}

	{
		if v.Speed != nil {
			convertedValue := *v.Speed
			r.Speed = &convertedValue
		}
	}

	{
		converted, err := convertSpeedUnit(v.Unit)
		if err != nil {
			return nil, err
		}
		r.Unit = converted
	}

	{
		if v.BearingDegrees != nil {
			convertedValue := *v.BearingDegrees
			r.BearingDegrees = &convertedValue
		}
	}
	return r, nil
}

func convertPrecipitation(v *environment.Precipitation) (*api.Precipitation, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.Precipitation{}

	{
		if v.Amount != nil {
			convertedValue := *v.Amount
			r.Amount = &convertedValue
		}
	}

	{
		converted, err := convertPrecipitationUnit(v.Unit)
		if err != nil {
			return nil, err
		}
		r.Unit = converted
	}
	return r, nil
}

func convertForecastPeriod(v *environment.ForecastPeriod) (*api.ForecastPeriod, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.ForecastPeriod{}

	{
		r.StartTime = proto.CloneOf(v.StartsAt)
	}

	{
		r.EndTime = proto.CloneOf(v.EndsAt)
	}

	{
		converted, err := convertForecastKind(v.Kind)
		if err != nil {
			return nil, err
		}
		r.Kind = converted
	}

	{
		if v.Condition != nil {
			converted, err := convertWeatherCondition(*v.Condition)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.Condition = &convertedValue
		}
	}

	{
		converted, err := convertTemperature(v.Temperature)
		if err != nil {
			return nil, err
		}
		r.Temperature = converted
	}

	{
		converted, err := convertTemperature(v.Low)
		if err != nil {
			return nil, err
		}
		r.Low = converted
	}

	{
		converted, err := convertTemperature(v.High)
		if err != nil {
			return nil, err
		}
		r.High = converted
	}

	{
		if v.PrecipitationProbabilityPercent != nil {
			convertedValue := *v.PrecipitationProbabilityPercent
			r.PrecipitationProbabilityPercent = &convertedValue
		}
	}

	{
		converted, err := convertPrecipitation(v.Precipitation)
		if err != nil {
			return nil, err
		}
		r.Precipitation = converted
	}
	return r, nil
}

func convertForecast(v *environment.Forecast) (*api.Forecast, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.Forecast{}

	{
		r.IssueTime = proto.CloneOf(v.IssuedAt)
	}

	{
		r.ExpireTime = proto.CloneOf(v.ValidUntil)
	}

	{
		for _, item := range v.Periods {
			converted, err := convertForecastPeriod(item)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.Periods = append(r.Periods, convertedValue)
		}
	}
	return r, nil
}

func convertWeatherValue(v *environment.WeatherValue) (*api.WeatherValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.WeatherValue{}

	{
		if v.Condition != nil {
			converted, err := convertWeatherCondition(*v.Condition)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.Condition = &convertedValue
		}
	}

	{
		converted, err := convertTemperature(v.Temperature)
		if err != nil {
			return nil, err
		}
		r.Temperature = converted
	}

	{
		if v.HumidityPercent != nil {
			convertedValue := *v.HumidityPercent
			r.HumidityPercent = &convertedValue
		}
	}

	{
		converted, err := convertWind(v.Wind)
		if err != nil {
			return nil, err
		}
		r.Wind = converted
	}

	{
		converted, err := convertForecast(v.Forecast)
		if err != nil {
			return nil, err
		}
		r.Forecast = converted
	}

	{
		r.SourceLabel = v.SourceLabel
	}
	return r, nil
}

func convertWeatherConstraints(v *environment.WeatherConstraints) (*api.WeatherDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.WeatherDefinition{}
	return r, nil
}

func convertPresenceValue(v *environment.PresenceValue) (*api.PresenceValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.PresenceValue{}

	{
		r.PersonId = v.PersonId
	}

	{
		converted, err := convertResidence(v.Residence)
		if err != nil {
			return nil, err
		}
		r.Residence = converted
	}

	{
		converted, err := convertPresenceConfidence(v.Confidence)
		if err != nil {
			return nil, err
		}
		r.Confidence = converted
	}

	{
		r.SourceLabel = v.SourceLabel
	}
	return r, nil
}

func convertPresenceConstraints(v *environment.PresenceConstraints) (*api.PresenceDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.PresenceDefinition{}
	return r, nil
}

func convertPresenceZone(v *environment.PresenceZone) (*api.PresenceZone, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.PresenceZone{}

	{
		r.ZoneId = v.ZoneId
	}

	{
		r.Label = v.Label
	}
	return r, nil
}

func convertCoordinates(v *environment.Coordinates) (*api.Coordinates, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.Coordinates{}

	{
		if v.LatitudeDegrees != nil {
			convertedValue := *v.LatitudeDegrees
			r.LatitudeDegrees = &convertedValue
		}
	}

	{
		if v.LongitudeDegrees != nil {
			convertedValue := *v.LongitudeDegrees
			r.LongitudeDegrees = &convertedValue
		}
	}

	{
		if v.AccuracyMeters != nil {
			convertedValue := *v.AccuracyMeters
			r.AccuracyMeters = &convertedValue
		}
	}
	return r, nil
}

func convertPresenceDetailsValue(v *environment.PresenceDetailsValue) (*api.PresenceDetailsValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.PresenceDetailsValue{}

	{
		r.PersonId = v.PersonId
	}

	{
		converted, err := convertPresenceZone(v.Zone)
		if err != nil {
			return nil, err
		}
		r.Zone = converted
	}

	{
		converted, err := convertCoordinates(v.Coordinates)
		if err != nil {
			return nil, err
		}
		r.Coordinates = converted
	}

	{
		r.SourceLabel = v.SourceLabel
	}
	return r, nil
}

func convertPresenceDetailsConstraints(v *environment.PresenceDetailsConstraints) (*api.PresenceDetailsDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.PresenceDetailsDefinition{}
	return r, nil
}

func convertCivilDate(v *helpers.CivilDate) (*date.Date, error) {
	if v == nil {
		return nil, nil
	}
	r := &date.Date{}

	{
		if v.Year > math.MaxInt32 {
			return nil, ErrProjection
		}
		r.Year = int32(v.Year)
	}

	{
		if v.Month > math.MaxInt32 {
			return nil, ErrProjection
		}
		r.Month = int32(v.Month)
	}

	{
		if v.Day > math.MaxInt32 {
			return nil, ErrProjection
		}
		r.Day = int32(v.Day)
	}
	return r, nil
}

func convertCivilTime(v *helpers.CivilTime) (*timeofday.TimeOfDay, error) {
	if v == nil {
		return nil, nil
	}
	r := &timeofday.TimeOfDay{}

	{
		if v.Hour > math.MaxInt32 {
			return nil, ErrProjection
		}
		r.Hours = int32(v.Hour)
	}

	{
		if v.Minute > math.MaxInt32 {
			return nil, ErrProjection
		}
		r.Minutes = int32(v.Minute)
	}

	{
		if v.Second > math.MaxInt32 {
			return nil, ErrProjection
		}
		r.Seconds = int32(v.Second)
	}
	return r, nil
}

func convertCivilDateTime(v *helpers.CivilDateTime) (*api.LocalDateTime, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.LocalDateTime{}

	{
		converted, err := convertCivilDate(v.Date)
		if err != nil {
			return nil, err
		}
		r.Date = converted
	}

	{
		converted, err := convertCivilTime(v.Time)
		if err != nil {
			return nil, err
		}
		r.Time = converted
	}
	return r, nil
}

func convertHelperValue(v *helpers.HelperValue) (*api.HelperValue, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.HelperValue{}

	{
		if v.HelperRevision > math.MaxInt64 {
			return nil, ErrProjection
		}
		r.HelperRevision = int64(v.HelperRevision)
	}
	switch x := v.Value.(type) {
	case *helpers.HelperValue_Boolean:
		if x == nil {
			return nil, ErrProjection
		}
		convertedValue := x.Boolean
		r.Value = &api.HelperValue_BooleanValue{BooleanValue: convertedValue}
	case *helpers.HelperValue_Number:
		if x == nil {
			return nil, ErrProjection
		}
		convertedValue := x.Number
		r.Value = &api.HelperValue_Number{Number: convertedValue}
	case *helpers.HelperValue_Counter:
		if x == nil {
			return nil, ErrProjection
		}
		convertedValue := x.Counter
		r.Value = &api.HelperValue_Counter{Counter: convertedValue}
	case *helpers.HelperValue_ButtonPressedAt:
		if x == nil {
			return nil, ErrProjection
		}
		convertedValue := proto.CloneOf(x.ButtonPressedAt)
		r.Value = &api.HelperValue_TriggerTime{TriggerTime: convertedValue}
	case *helpers.HelperValue_Date:
		if x == nil {
			return nil, ErrProjection
		}
		converted, err := convertCivilDate(x.Date)
		if err != nil {
			return nil, err
		}
		convertedValue := converted
		r.Value = &api.HelperValue_Date{Date: convertedValue}
	case *helpers.HelperValue_Time:
		if x == nil {
			return nil, ErrProjection
		}
		converted, err := convertCivilTime(x.Time)
		if err != nil {
			return nil, err
		}
		convertedValue := converted
		r.Value = &api.HelperValue_Time{Time: convertedValue}
	case *helpers.HelperValue_Datetime:
		if x == nil {
			return nil, ErrProjection
		}
		converted, err := convertCivilDateTime(x.Datetime)
		if err != nil {
			return nil, err
		}
		convertedValue := converted
		r.Value = &api.HelperValue_LocalValue{LocalValue: convertedValue}
	case *helpers.HelperValue_SelectedOption:
		if x == nil {
			return nil, ErrProjection
		}
		convertedValue := x.SelectedOption
		r.Value = &api.HelperValue_SelectedOption{SelectedOption: convertedValue}
	case *helpers.HelperValue_Text:
		if x == nil {
			return nil, ErrProjection
		}
		convertedValue := x.Text
		r.Value = &api.HelperValue_Text{Text: convertedValue}
	case nil:
	default:
		return nil, ErrProjection
	}
	return r, nil
}

func convertHelperConstraints(v *helpers.HelperConstraints) (*api.HelperDefinition, error) {
	if v == nil {
		return nil, nil
	}
	r := &api.HelperDefinition{}

	{
		converted, err := convertHelperKind(v.Kind)
		if err != nil {
			return nil, err
		}
		r.Kind = converted
	}

	{
		for _, item := range v.Operations {
			converted, err := convertHelperOperation(item)
			if err != nil {
				return nil, err
			}
			convertedValue := converted
			r.Operations = append(r.Operations, convertedValue)
		}
	}

	{
		if v.NumberMinimum != nil {
			convertedValue := *v.NumberMinimum
			r.NumberMinimum = &convertedValue
		}
	}

	{
		if v.NumberMaximum != nil {
			convertedValue := *v.NumberMaximum
			r.NumberMaximum = &convertedValue
		}
	}

	{
		if v.NumberStep != nil {
			convertedValue := *v.NumberStep
			r.NumberStep = &convertedValue
		}
	}

	{
		if v.CounterMinimum != nil {
			convertedValue := *v.CounterMinimum
			r.CounterMinimum = &convertedValue
		}
	}

	{
		if v.CounterMaximum != nil {
			convertedValue := *v.CounterMaximum
			r.CounterMaximum = &convertedValue
		}
	}

	{
		if v.CounterStep != nil {
			convertedValue := *v.CounterStep
			r.CounterStep = &convertedValue
		}
	}

	{
		for _, item := range v.SelectOptions {
			convertedValue := item
			r.SelectOptions = append(r.SelectOptions, convertedValue)
		}
	}

	{
		if v.TextMinimumLength != nil {
			if *v.TextMinimumLength > math.MaxInt32 {
				return nil, ErrProjection
			}
			convertedValue := int32(*v.TextMinimumLength)
			r.TextMinimumLength = &convertedValue
		}
	}

	{
		if v.TextMaximumLength != nil {
			if *v.TextMaximumLength > math.MaxInt32 {
				return nil, ErrProjection
			}
			convertedValue := int32(*v.TextMaximumLength)
			r.TextMaximumLength = &convertedValue
		}
	}

	{
		converted, err := convertHelperReason(v.UnavailableReason)
		if err != nil {
			return nil, err
		}
		r.UnavailableReason = converted
	}
	return r, nil
}
