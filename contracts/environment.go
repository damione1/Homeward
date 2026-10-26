package contracts

import (
	"time"

	env "github.com/damione1/Homeward/gen/home/environment/v1"
	"github.com/damione1/Homeward/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var weatherConditions = map[env.WeatherCondition]domain.WeatherCondition{
	env.WeatherCondition_WEATHER_CONDITION_UNKNOWN:         domain.WeatherUnknown,
	env.WeatherCondition_WEATHER_CONDITION_CLEAR_DAY:       domain.WeatherClearDay,
	env.WeatherCondition_WEATHER_CONDITION_CLEAR_NIGHT:     domain.WeatherClearNight,
	env.WeatherCondition_WEATHER_CONDITION_CLOUDY:          domain.WeatherCloudy,
	env.WeatherCondition_WEATHER_CONDITION_PARTLY_CLOUDY:   domain.WeatherPartlyCloudy,
	env.WeatherCondition_WEATHER_CONDITION_RAINY:           domain.WeatherRainy,
	env.WeatherCondition_WEATHER_CONDITION_POURING:         domain.WeatherPouring,
	env.WeatherCondition_WEATHER_CONDITION_SNOWY:           domain.WeatherSnowy,
	env.WeatherCondition_WEATHER_CONDITION_SNOWY_RAINY:     domain.WeatherSnowyRainy,
	env.WeatherCondition_WEATHER_CONDITION_HAIL:            domain.WeatherHail,
	env.WeatherCondition_WEATHER_CONDITION_LIGHTNING:       domain.WeatherLightning,
	env.WeatherCondition_WEATHER_CONDITION_LIGHTNING_RAINY: domain.WeatherLightningRainy,
	env.WeatherCondition_WEATHER_CONDITION_WINDY:           domain.WeatherWindy,
	env.WeatherCondition_WEATHER_CONDITION_WINDY_VARIANT:   domain.WeatherWindyVariant,
	env.WeatherCondition_WEATHER_CONDITION_FOG:             domain.WeatherFog,
	env.WeatherCondition_WEATHER_CONDITION_EXCEPTIONAL:     domain.WeatherExceptional,
}
var temperatureUnits = map[env.TemperatureUnit]domain.TemperatureUnit{
	env.TemperatureUnit_TEMPERATURE_UNIT_CELSIUS:    domain.TemperatureCelsius,
	env.TemperatureUnit_TEMPERATURE_UNIT_FAHRENHEIT: domain.TemperatureFahrenheit,
	env.TemperatureUnit_TEMPERATURE_UNIT_KELVIN:     domain.TemperatureKelvin,
}
var speedUnits = map[env.SpeedUnit]domain.SpeedUnit{
	env.SpeedUnit_SPEED_UNIT_METERS_PER_SECOND:   domain.SpeedMetersPerSecond,
	env.SpeedUnit_SPEED_UNIT_KILOMETERS_PER_HOUR: domain.SpeedKilometersPerHour,
	env.SpeedUnit_SPEED_UNIT_MILES_PER_HOUR:      domain.SpeedMilesPerHour,
	env.SpeedUnit_SPEED_UNIT_KNOTS:               domain.SpeedKnots,
}
var precipitationUnits = map[env.PrecipitationUnit]domain.PrecipitationUnit{
	env.PrecipitationUnit_PRECIPITATION_UNIT_MILLIMETERS: domain.PrecipitationMillimeters,
	env.PrecipitationUnit_PRECIPITATION_UNIT_INCHES:      domain.PrecipitationInches,
}
var forecastKinds = map[env.ForecastKind]domain.ForecastKind{
	env.ForecastKind_FORECAST_KIND_HOURLY: domain.ForecastHourly,
	env.ForecastKind_FORECAST_KIND_DAILY:  domain.ForecastDaily,
}
var residenceStates = map[env.Residence]domain.Residence{
	env.Residence_RESIDENCE_HOME:    domain.ResidenceHome,
	env.Residence_RESIDENCE_AWAY:    domain.ResidenceAway,
	env.Residence_RESIDENCE_UNKNOWN: domain.ResidenceUnknown,
}
var presenceConfidences = map[env.PresenceConfidence]domain.PresenceConfidence{
	env.PresenceConfidence_PRESENCE_CONFIDENCE_UNKNOWN: domain.PresenceConfidenceUnknown,
	env.PresenceConfidence_PRESENCE_CONFIDENCE_LOW:     domain.PresenceConfidenceLow,
	env.PresenceConfidence_PRESENCE_CONFIDENCE_MEDIUM:  domain.PresenceConfidenceMedium,
	env.PresenceConfidence_PRESENCE_CONFIDENCE_HIGH:    domain.PresenceConfidenceHigh,
}

// Admission is atomic. A missing optional field is preserved; an invalid
// present field cannot silently become an absent reading or observed zero.
type environmentDecoder struct{ invalid bool }

func (d *environmentDecoder) condition(value *env.WeatherCondition) *domain.WeatherCondition {
	if value == nil {
		return nil
	}
	admitted, err := admitNativeEnum(*value, weatherConditions)
	if err != nil {
		d.invalid = true
		return nil
	}
	return &admitted
}
func (d *environmentDecoder) temperature(value *env.Temperature) *domain.Temperature {
	if value == nil {
		return nil
	}
	unit, err := admitNativeEnum(value.Unit, temperatureUnits)
	if err != nil || value.Value == nil {
		d.invalid = true
		return nil
	}
	return &domain.Temperature{Value: *value.Value, Unit: unit}
}
func (d *environmentDecoder) wind(value *env.Wind) *domain.Wind {
	if value == nil {
		return nil
	}
	unit, err := admitNativeEnum(value.Unit, speedUnits)
	if err != nil || value.Speed == nil {
		d.invalid = true
		return nil
	}
	return &domain.Wind{Speed: *value.Speed, Unit: unit, BearingDegrees: value.BearingDegrees}
}
func (d *environmentDecoder) precipitation(value *env.Precipitation) *domain.Precipitation {
	if value == nil {
		return nil
	}
	unit, err := admitNativeEnum(value.Unit, precipitationUnits)
	if err != nil || value.Amount == nil {
		d.invalid = true
		return nil
	}
	return &domain.Precipitation{Amount: *value.Amount, Unit: unit}
}
func (d *environmentDecoder) instant(value *timestamppb.Timestamp) time.Time {
	if value == nil || value.CheckValid() != nil {
		d.invalid = true
		return time.Time{}
	}
	return value.AsTime()
}
func (d *environmentDecoder) forecast(value *env.Forecast) *domain.Forecast {
	if value == nil {
		return nil
	}
	if len(value.Periods) == 0 || len(value.Periods) > domain.MaxForecastPeriods {
		d.invalid = true
		return nil
	}
	out := &domain.Forecast{IssuedAt: d.instant(value.IssuedAt), ValidUntil: d.instant(value.ValidUntil), Periods: make([]domain.ForecastPeriod, 0, len(value.Periods))}
	for _, period := range value.Periods {
		if period == nil {
			d.invalid = true
			continue
		}
		kind, err := admitNativeEnum(period.Kind, forecastKinds)
		if err != nil {
			d.invalid = true
		}
		out.Periods = append(out.Periods, domain.ForecastPeriod{
			StartsAt: d.instant(period.StartsAt), EndsAt: d.instant(period.EndsAt), Kind: kind,
			Condition: d.condition(period.Condition), Temperature: d.temperature(period.Temperature),
			Low: d.temperature(period.Low), High: d.temperature(period.High),
			PrecipitationProbabilityPercent: period.PrecipitationProbabilityPercent,
			Precipitation:                   d.precipitation(period.Precipitation),
		})
	}
	return out
}

func AdmitWireWeatherValue(value *env.WeatherValue) (domain.WeatherValue, error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) {
		return domain.WeatherValue{}, ErrInvalidWireState
	}
	d := &environmentDecoder{}
	parts := domain.WeatherParts{Condition: d.condition(value.Condition), Temperature: d.temperature(value.Temperature),
		HumidityPercent: value.HumidityPercent, Wind: d.wind(value.Wind), Forecast: d.forecast(value.Forecast), SourceLabel: value.SourceLabel}
	if d.invalid {
		return domain.WeatherValue{}, ErrInvalidWireState
	}
	return domain.NewWeatherValue(parts)
}

type environmentEncoder struct{ invalid bool }

func (e *environmentEncoder) condition(value *domain.WeatherCondition) *env.WeatherCondition {
	if value == nil {
		return nil
	}
	out, err := wireNativeEnum(*value, weatherConditions)
	if err != nil {
		e.invalid = true
		return nil
	}
	return &out
}
func (e *environmentEncoder) temperature(value *domain.Temperature) *env.Temperature {
	if value == nil {
		return nil
	}
	unit, err := wireNativeEnum(value.Unit, temperatureUnits)
	if err != nil {
		e.invalid = true
		return nil
	}
	return &env.Temperature{Value: environmentCopy(&value.Value), Unit: unit}
}
func (e *environmentEncoder) wind(value *domain.Wind) *env.Wind {
	if value == nil {
		return nil
	}
	unit, err := wireNativeEnum(value.Unit, speedUnits)
	if err != nil {
		e.invalid = true
		return nil
	}
	return &env.Wind{Speed: environmentCopy(&value.Speed), Unit: unit, BearingDegrees: environmentCopy(value.BearingDegrees)}
}
func (e *environmentEncoder) precipitation(value *domain.Precipitation) *env.Precipitation {
	if value == nil {
		return nil
	}
	unit, err := wireNativeEnum(value.Unit, precipitationUnits)
	if err != nil {
		e.invalid = true
		return nil
	}
	return &env.Precipitation{Amount: environmentCopy(&value.Amount), Unit: unit}
}
func (e *environmentEncoder) forecast(value *domain.Forecast) *env.Forecast {
	if value == nil {
		return nil
	}
	out := &env.Forecast{IssuedAt: timestamppb.New(value.IssuedAt), ValidUntil: timestamppb.New(value.ValidUntil), Periods: make([]*env.ForecastPeriod, 0, len(value.Periods))}
	for _, period := range value.Periods {
		kind, err := wireNativeEnum(period.Kind, forecastKinds)
		if err != nil {
			e.invalid = true
		}
		out.Periods = append(out.Periods, &env.ForecastPeriod{StartsAt: timestamppb.New(period.StartsAt), EndsAt: timestamppb.New(period.EndsAt), Kind: kind,
			Condition: e.condition(period.Condition), Temperature: e.temperature(period.Temperature), Low: e.temperature(period.Low), High: e.temperature(period.High),
			PrecipitationProbabilityPercent: environmentCopy(period.PrecipitationProbabilityPercent), Precipitation: e.precipitation(period.Precipitation)})
	}
	return out
}
func EncodeWeatherValue(value domain.WeatherValue) (*env.WeatherValue, error) {
	parts, err := value.Components()
	if err != nil {
		return nil, err
	}
	e := &environmentEncoder{}
	out := &env.WeatherValue{Condition: e.condition(parts.Condition), Temperature: e.temperature(parts.Temperature),
		HumidityPercent: environmentCopy(parts.HumidityPercent), Wind: e.wind(parts.Wind), Forecast: e.forecast(parts.Forecast), SourceLabel: parts.SourceLabel}
	if e.invalid {
		return nil, ErrInvalidWireState
	}
	return out, nil
}

func AdmitWirePresenceValue(value *env.PresenceValue) (domain.PresenceValue, error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) {
		return domain.PresenceValue{}, ErrInvalidWireState
	}
	person, err := domain.ParsePersonID(value.PersonId)
	if err != nil {
		return domain.PresenceValue{}, err
	}
	residence, err := admitNativeEnum(value.Residence, residenceStates)
	if err != nil {
		return domain.PresenceValue{}, err
	}
	confidence, err := admitNativeEnum(value.Confidence, presenceConfidences)
	if err != nil {
		return domain.PresenceValue{}, err
	}
	return domain.NewPresenceValue(domain.PresenceParts{Person: person, Residence: residence, Confidence: confidence, SourceLabel: value.SourceLabel})
}
func EncodePresenceValue(value domain.PresenceValue) (*env.PresenceValue, error) {
	parts, err := value.Components()
	if err != nil {
		return nil, err
	}
	person, err := parts.Person.Value()
	if err != nil {
		return nil, err
	}
	residence, err := wireNativeEnum(parts.Residence, residenceStates)
	if err != nil {
		return nil, err
	}
	confidence, err := wireNativeEnum(parts.Confidence, presenceConfidences)
	if err != nil {
		return nil, err
	}
	return &env.PresenceValue{PersonId: person, Residence: residence, Confidence: confidence, SourceLabel: parts.SourceLabel}, nil
}

func AdmitWirePresenceDetailsValue(value *env.PresenceDetailsValue) (domain.PresenceDetailsValue, error) {
	if value == nil || !knownFieldsOnly(value.ProtoReflect()) {
		return domain.PresenceDetailsValue{}, ErrInvalidWireState
	}
	person, err := domain.ParsePersonID(value.PersonId)
	if err != nil {
		return domain.PresenceDetailsValue{}, err
	}
	parts := domain.PresenceDetailsParts{Person: person, SourceLabel: value.SourceLabel}
	if value.Zone != nil {
		id, err := domain.ParsePresenceZoneID(value.Zone.ZoneId)
		if err != nil {
			return domain.PresenceDetailsValue{}, err
		}
		parts.Zone = &domain.PresenceZone{ID: id, Label: value.Zone.Label}
	}
	if position := value.Coordinates; position != nil {
		if position.LatitudeDegrees == nil || position.LongitudeDegrees == nil {
			return domain.PresenceDetailsValue{}, ErrInvalidWireState
		}
		parts.Coordinates = &domain.Coordinates{LatitudeDegrees: *position.LatitudeDegrees, LongitudeDegrees: *position.LongitudeDegrees, AccuracyMeters: position.AccuracyMeters}
	}
	return domain.NewPresenceDetailsValue(parts)
}
func EncodePresenceDetailsValue(value domain.PresenceDetailsValue) (*env.PresenceDetailsValue, error) {
	parts, err := value.Components()
	if err != nil {
		return nil, err
	}
	person, err := parts.Person.Value()
	if err != nil {
		return nil, err
	}
	out := &env.PresenceDetailsValue{PersonId: person, SourceLabel: parts.SourceLabel}
	if parts.Zone != nil {
		id, err := parts.Zone.ID.Value()
		if err != nil {
			return nil, err
		}
		out.Zone = &env.PresenceZone{ZoneId: id, Label: parts.Zone.Label}
	}
	if position := parts.Coordinates; position != nil {
		out.Coordinates = &env.Coordinates{LatitudeDegrees: environmentCopy(&position.LatitudeDegrees), LongitudeDegrees: environmentCopy(&position.LongitudeDegrees), AccuracyMeters: environmentCopy(position.AccuracyMeters)}
	}
	return out, nil
}

func environmentCopy[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
