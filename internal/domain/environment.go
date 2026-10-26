package domain

import (
	"errors"
	"math"
	"time"
	"unicode/utf8"
)

var ErrInvalidEnvironmentValue = errors.New("invalid environment value")

type PersonID struct{ value string }
type PresenceZoneID struct{ value string }

func ParsePersonID(value string) (PersonID, error) {
	if len(value) > 256 {
		return PersonID{}, ErrInvalidEnvironmentValue
	}
	value, err := parseIdentity(value, ErrInvalidEnvironmentValue)
	return PersonID{value: value}, err
}
func (id PersonID) Value() (string, error) {
	parsed, err := ParsePersonID(id.value)
	return parsed.value, err
}
func ParsePresenceZoneID(value string) (PresenceZoneID, error) {
	if len(value) > 256 {
		return PresenceZoneID{}, ErrInvalidEnvironmentValue
	}
	value, err := parseIdentity(value, ErrInvalidEnvironmentValue)
	return PresenceZoneID{value: value}, err
}
func (id PresenceZoneID) Value() (string, error) {
	parsed, err := ParsePresenceZoneID(id.value)
	return parsed.value, err
}

type WeatherCondition uint8

const (
	WeatherConditionUnspecified WeatherCondition = iota
	WeatherUnknown
	WeatherClearDay
	WeatherClearNight
	WeatherCloudy
	WeatherPartlyCloudy
	WeatherRainy
	WeatherPouring
	WeatherSnowy
	WeatherSnowyRainy
	WeatherHail
	WeatherLightning
	WeatherLightningRainy
	WeatherWindy
	WeatherWindyVariant
	WeatherFog
	WeatherExceptional
)

func (v WeatherCondition) Valid() bool {
	switch v {
	case WeatherUnknown, WeatherClearDay, WeatherClearNight, WeatherCloudy, WeatherPartlyCloudy,
		WeatherRainy, WeatherPouring, WeatherSnowy, WeatherSnowyRainy, WeatherHail, WeatherLightning,
		WeatherLightningRainy, WeatherWindy, WeatherWindyVariant, WeatherFog, WeatherExceptional:
		return true
	case WeatherConditionUnspecified:
	default:
	}
	return false
}

type TemperatureUnit uint8

const (
	TemperatureUnitUnspecified TemperatureUnit = iota
	TemperatureCelsius
	TemperatureFahrenheit
	TemperatureKelvin
)

type Temperature struct {
	Value float64
	Unit  TemperatureUnit
}

func (v Temperature) Valid() bool {
	if !environmentFinite(v.Value) {
		return false
	}
	switch v.Unit {
	case TemperatureCelsius:
		return v.Value >= -273.15
	case TemperatureFahrenheit:
		return v.Value >= -459.67
	case TemperatureKelvin:
		return v.Value >= 0
	case TemperatureUnitUnspecified:
	default:
	}
	return false
}

type SpeedUnit uint8

const (
	SpeedUnitUnspecified SpeedUnit = iota
	SpeedMetersPerSecond
	SpeedKilometersPerHour
	SpeedMilesPerHour
	SpeedKnots
)

type Wind struct {
	Speed          float64
	Unit           SpeedUnit
	BearingDegrees *float64
}

func (v Wind) Valid() bool {
	if !environmentFinite(v.Speed) || v.Speed < 0 || v.BearingDegrees != nil &&
		(!environmentFinite(*v.BearingDegrees) || *v.BearingDegrees < 0 || *v.BearingDegrees > 360) {
		return false
	}
	switch v.Unit {
	case SpeedMetersPerSecond, SpeedKilometersPerHour, SpeedMilesPerHour, SpeedKnots:
		return true
	case SpeedUnitUnspecified:
	default:
	}
	return false
}

type PrecipitationUnit uint8

const (
	PrecipitationUnitUnspecified PrecipitationUnit = iota
	PrecipitationMillimeters
	PrecipitationInches
)

type Precipitation struct {
	Amount float64
	Unit   PrecipitationUnit
}

func (v Precipitation) Valid() bool {
	if !environmentFinite(v.Amount) || v.Amount < 0 {
		return false
	}
	switch v.Unit {
	case PrecipitationMillimeters, PrecipitationInches:
		return true
	case PrecipitationUnitUnspecified:
	default:
	}
	return false
}

type ForecastKind uint8

const (
	ForecastKindUnspecified ForecastKind = iota
	ForecastHourly
	ForecastDaily
)

type ForecastPeriod struct {
	StartsAt, EndsAt                time.Time
	Kind                            ForecastKind
	Condition                       *WeatherCondition
	Temperature, Low, High          *Temperature
	PrecipitationProbabilityPercent *float64
	Precipitation                   *Precipitation
}

func (v ForecastPeriod) Valid() bool {
	if !environmentInstant(v.StartsAt) || !environmentInstant(v.EndsAt) || !v.StartsAt.Before(v.EndsAt) {
		return false
	}
	switch v.Kind {
	case ForecastHourly, ForecastDaily:
	case ForecastKindUnspecified:
		return false
	default:
		return false
	}
	if v.Condition != nil && !v.Condition.Valid() || v.Temperature != nil && !v.Temperature.Valid() ||
		v.Low != nil && !v.Low.Valid() || v.High != nil && !v.High.Valid() ||
		v.PrecipitationProbabilityPercent != nil && !environmentPercent(*v.PrecipitationProbabilityPercent) ||
		v.Precipitation != nil && !v.Precipitation.Valid() {
		return false
	}
	return v.Low == nil || v.High == nil || v.Low.Unit == v.High.Unit && v.Low.Value <= v.High.Value
}

const MaxForecastPeriods = 256

type Forecast struct {
	IssuedAt, ValidUntil time.Time
	Periods              []ForecastPeriod
}

func (v Forecast) Valid() bool {
	if !environmentInstant(v.IssuedAt) || !environmentInstant(v.ValidUntil) ||
		!v.IssuedAt.Before(v.ValidUntil) || len(v.Periods) == 0 || len(v.Periods) > MaxForecastPeriods {
		return false
	}
	for index, period := range v.Periods {
		if !period.Valid() || index > 0 && period.StartsAt.Before(v.Periods[index-1].EndsAt) {
			return false
		}
	}
	return true
}

type WeatherParts struct {
	Condition       *WeatherCondition
	Temperature     *Temperature
	HumidityPercent *float64
	Wind            *Wind
	Forecast        *Forecast
	SourceLabel     string
}

type WeatherValue struct {
	parts WeatherParts
	valid bool
}

func NewWeatherValue(parts WeatherParts) (WeatherValue, error) {
	if !validWeatherParts(parts) {
		return WeatherValue{}, ErrInvalidEnvironmentValue
	}
	return WeatherValue{parts: cloneWeatherParts(parts), valid: true}, nil
}
func (v WeatherValue) Components() (WeatherParts, error) {
	if !v.valid || !validWeatherParts(v.parts) {
		return WeatherParts{}, ErrInvalidEnvironmentValue
	}
	return cloneWeatherParts(v.parts), nil
}
func validWeatherParts(v WeatherParts) bool {
	return environmentLabel(v.SourceLabel) &&
		(v.Condition != nil || v.Temperature != nil || v.HumidityPercent != nil || v.Wind != nil || v.Forecast != nil) &&
		(v.Condition == nil || v.Condition.Valid()) && (v.Temperature == nil || v.Temperature.Valid()) &&
		(v.HumidityPercent == nil || environmentPercent(*v.HumidityPercent)) &&
		(v.Wind == nil || v.Wind.Valid()) && (v.Forecast == nil || v.Forecast.Valid())
}

func cloneWeatherParts(v WeatherParts) WeatherParts {
	v.Condition = cloneEnvironment(v.Condition)
	v.Temperature = cloneEnvironment(v.Temperature)
	v.HumidityPercent = cloneEnvironment(v.HumidityPercent)
	if v.Wind != nil {
		v.Wind = cloneEnvironment(v.Wind)
		v.Wind.BearingDegrees = cloneEnvironment(v.Wind.BearingDegrees)
	}
	if v.Forecast != nil {
		v.Forecast = cloneEnvironment(v.Forecast)
		v.Forecast.IssuedAt, v.Forecast.ValidUntil = v.Forecast.IssuedAt.UTC(), v.Forecast.ValidUntil.UTC()
		v.Forecast.Periods = append([]ForecastPeriod(nil), v.Forecast.Periods...)
		for index := range v.Forecast.Periods {
			period := &v.Forecast.Periods[index]
			period.StartsAt, period.EndsAt = period.StartsAt.UTC(), period.EndsAt.UTC()
			period.Condition = cloneEnvironment(period.Condition)
			period.Temperature, period.Low, period.High = cloneEnvironment(period.Temperature), cloneEnvironment(period.Low), cloneEnvironment(period.High)
			period.PrecipitationProbabilityPercent = cloneEnvironment(period.PrecipitationProbabilityPercent)
			period.Precipitation = cloneEnvironment(period.Precipitation)
		}
	}
	return v
}

type Residence uint8

const (
	ResidenceUnspecified Residence = iota
	ResidenceHome
	ResidenceAway
	ResidenceUnknown
)

func (v Residence) Valid() bool {
	switch v {
	case ResidenceHome, ResidenceAway, ResidenceUnknown:
		return true
	case ResidenceUnspecified:
	default:
	}
	return false
}

type PresenceConfidence uint8

const (
	PresenceConfidenceUnspecified PresenceConfidence = iota
	PresenceConfidenceUnknown
	PresenceConfidenceLow
	PresenceConfidenceMedium
	PresenceConfidenceHigh
)

func (v PresenceConfidence) Valid() bool {
	switch v {
	case PresenceConfidenceUnknown, PresenceConfidenceLow, PresenceConfidenceMedium, PresenceConfidenceHigh:
		return true
	case PresenceConfidenceUnspecified:
	default:
	}
	return false
}

// Aggregate values cannot contain a location, even accidentally.
type PresenceParts struct {
	Person      PersonID
	Residence   Residence
	Confidence  PresenceConfidence
	SourceLabel string
}

type PresenceValue struct {
	parts PresenceParts
	valid bool
}

func NewPresenceValue(parts PresenceParts) (PresenceValue, error) {
	if _, err := parts.Person.Value(); err != nil || !parts.Residence.Valid() ||
		!parts.Confidence.Valid() || !environmentLabel(parts.SourceLabel) {
		return PresenceValue{}, ErrInvalidEnvironmentValue
	}
	return PresenceValue{parts: parts, valid: true}, nil
}
func (v PresenceValue) Components() (PresenceParts, error) {
	if !v.valid {
		return PresenceParts{}, ErrInvalidEnvironmentValue
	}
	if _, err := NewPresenceValue(v.parts); err != nil {
		return PresenceParts{}, err
	}
	return v.parts, nil
}

type PresenceZone struct {
	ID    PresenceZoneID
	Label string
}
type Coordinates struct {
	LatitudeDegrees, LongitudeDegrees float64
	AccuracyMeters                    *float64
}

func (v Coordinates) Valid() bool {
	return environmentFinite(v.LatitudeDegrees) && v.LatitudeDegrees >= -90 && v.LatitudeDegrees <= 90 &&
		environmentFinite(v.LongitudeDegrees) && v.LongitudeDegrees >= -180 && v.LongitudeDegrees <= 180 &&
		(v.AccuracyMeters == nil || environmentFinite(*v.AccuracyMeters) && *v.AccuracyMeters >= 0)
}

// Details belong to another registry entity with an independent read grant.
type PresenceDetailsParts struct {
	Person      PersonID
	Zone        *PresenceZone
	Coordinates *Coordinates
	SourceLabel string
}
type PresenceDetailsValue struct {
	parts PresenceDetailsParts
	valid bool
}

func NewPresenceDetailsValue(parts PresenceDetailsParts) (PresenceDetailsValue, error) {
	if _, err := parts.Person.Value(); err != nil || !environmentLabel(parts.SourceLabel) ||
		parts.Coordinates != nil && !parts.Coordinates.Valid() {
		return PresenceDetailsValue{}, ErrInvalidEnvironmentValue
	}
	if parts.Zone != nil {
		if _, err := parts.Zone.ID.Value(); err != nil || parts.Zone.Label == "" || !environmentLabel(parts.Zone.Label) {
			return PresenceDetailsValue{}, ErrInvalidEnvironmentValue
		}
	}
	return PresenceDetailsValue{parts: clonePresenceDetails(parts), valid: true}, nil
}
func (v PresenceDetailsValue) Components() (PresenceDetailsParts, error) {
	if !v.valid {
		return PresenceDetailsParts{}, ErrInvalidEnvironmentValue
	}
	copy, err := NewPresenceDetailsValue(v.parts)
	return copy.parts, err
}
func clonePresenceDetails(v PresenceDetailsParts) PresenceDetailsParts {
	v.Zone = cloneEnvironment(v.Zone)
	v.Coordinates = cloneEnvironment(v.Coordinates)
	if v.Coordinates != nil {
		v.Coordinates.AccuracyMeters = cloneEnvironment(v.Coordinates.AccuracyMeters)
	}
	return v
}

func cloneEnvironment[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func environmentFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func environmentPercent(value float64) bool {
	return environmentFinite(value) && value >= 0 && value <= 100
}
func environmentInstant(value time.Time) bool {
	return !value.IsZero() && value.UTC().Year() >= 1 && value.UTC().Year() <= 9999
}
func environmentLabel(value string) bool { return len(value) <= 160 && utf8.ValidString(value) }
