package domain

import (
	"math"
	"testing"
	"time"
)

func nhf010WeatherParts() WeatherParts {
	zero := 0.0
	issued := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC)
	return WeatherParts{
		Temperature:     &Temperature{Value: 0, Unit: TemperatureCelsius},
		HumidityPercent: &zero,
		Wind:            &Wind{Speed: 0, Unit: SpeedMetersPerSecond, BearingDegrees: &zero},
		SourceLabel:     "Synthetic weather source",
		Forecast: &Forecast{IssuedAt: issued, ValidUntil: issued.Add(6 * time.Hour), Periods: []ForecastPeriod{{
			StartsAt: issued, EndsAt: issued.Add(time.Hour), Kind: ForecastHourly,
			Low: &Temperature{Value: 0, Unit: TemperatureCelsius}, High: &Temperature{Value: 5, Unit: TemperatureCelsius},
			PrecipitationProbabilityPercent: &zero,
			Precipitation:                   &Precipitation{Amount: 0, Unit: PrecipitationMillimeters},
		}}},
	}
}

func TestNHF010WeatherOwnsEveryForecastFieldAndKeepsMissingAndZeroDistinct(t *testing.T) {
	parts := nhf010WeatherParts()
	value, err := NewWeatherValue(parts)
	if err != nil {
		t.Fatal(err)
	}
	parts.Temperature.Value = 30
	*parts.HumidityPercent = 90
	*parts.Wind.BearingDegrees = 180
	parts.Forecast.Periods[0].Low.Value = -20
	*parts.Forecast.Periods[0].PrecipitationProbabilityPercent = 80
	parts.Forecast.Periods[0].Precipitation.Amount = 10
	got, err := value.Components()
	if err != nil {
		t.Fatal(err)
	}
	if got.Temperature.Value != 0 || *got.HumidityPercent != 0 || *got.Wind.BearingDegrees != 0 ||
		got.Condition != nil || got.Forecast.Periods[0].Temperature != nil ||
		got.Forecast.Periods[0].Low.Value != 0 || *got.Forecast.Periods[0].PrecipitationProbabilityPercent != 0 ||
		got.Forecast.Periods[0].Precipitation.Amount != 0 {
		t.Fatal("caller mutation, missing field or observed zero changed the immutable weather value")
	}
	got.Forecast.Periods[0].High.Value = 90
	got.Forecast.Periods = append(got.Forecast.Periods, ForecastPeriod{})
	*got.Wind.BearingDegrees = 300
	again, err := value.Components()
	if err != nil || len(again.Forecast.Periods) != 1 || again.Forecast.Periods[0].High.Value != 5 || *again.Wind.BearingDegrees != 0 {
		t.Fatal("exported weather parts alias immutable state", err)
	}
}

func TestNHF010WeatherRejectsUnknownUnitsInvalidRangesAndUnboundedOrOverlappingForecasts(t *testing.T) {
	for _, change := range []func(*WeatherParts){
		func(v *WeatherParts) { bad := WeatherCondition(255); v.Condition = &bad },
		func(v *WeatherParts) { v.Temperature.Unit = TemperatureUnitUnspecified },
		func(v *WeatherParts) { v.Temperature.Unit = TemperatureUnit(255) },
		func(v *WeatherParts) { v.Temperature = &Temperature{Value: -1, Unit: TemperatureKelvin} },
		func(v *WeatherParts) { v.Temperature.Value = math.NaN() },
		func(v *WeatherParts) { *v.HumidityPercent = 101 },
		func(v *WeatherParts) { v.Wind.Speed = -1 },
		func(v *WeatherParts) { v.Wind.Unit = SpeedUnit(255) },
		func(v *WeatherParts) { *v.Wind.BearingDegrees = math.Inf(1) },
		func(v *WeatherParts) { v.Forecast.ValidUntil = v.Forecast.IssuedAt },
		func(v *WeatherParts) { v.Forecast.Periods = nil },
		func(v *WeatherParts) {
			original := v.Forecast.Periods[0]
			v.Forecast.Periods = make([]ForecastPeriod, MaxForecastPeriods+1)
			for index := range v.Forecast.Periods {
				period := original
				period.StartsAt = original.StartsAt.Add(time.Duration(index) * time.Hour)
				period.EndsAt = period.StartsAt.Add(time.Hour)
				v.Forecast.Periods[index] = period
			}
		},
		func(v *WeatherParts) { v.Forecast.Periods[0].Kind = ForecastKind(255) },
		func(v *WeatherParts) { v.Forecast.Periods[0].EndsAt = v.Forecast.Periods[0].StartsAt },
		func(v *WeatherParts) { v.Forecast.Periods = append(v.Forecast.Periods, v.Forecast.Periods[0]) },
		func(v *WeatherParts) { v.Forecast.Periods[0].High.Unit = TemperatureFahrenheit },
		func(v *WeatherParts) { v.Forecast.Periods[0].Low.Value = 6 },
		func(v *WeatherParts) { *v.Forecast.Periods[0].PrecipitationProbabilityPercent = -1 },
		func(v *WeatherParts) { v.Forecast.Periods[0].Precipitation.Unit = PrecipitationUnit(255) },
	} {
		parts := nhf010WeatherParts()
		change(&parts)
		if _, err := NewWeatherValue(parts); err == nil {
			t.Fatal("invalid weather admitted")
		}
	}
	if _, err := NewWeatherValue(WeatherParts{}); err == nil {
		t.Fatal("empty weather became an observation")
	}
}

func TestNHF010PresenceAggregateAndDetailsRemainSeparateAndImmutable(t *testing.T) {
	person, err := ParsePersonID("synthetic-person")
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := NewPresenceValue(PresenceParts{Person: person, Residence: ResidenceUnknown,
		Confidence: PresenceConfidenceUnknown, SourceLabel: "Synthetic source"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := aggregate.Components()
	if err != nil || got.Residence != ResidenceUnknown || got.Confidence != PresenceConfidenceUnknown {
		t.Fatal("unknown geographic presence inferred as home or away", err)
	}
	zoneID, _ := ParsePresenceZoneID("synthetic-zone")
	zero := 0.0
	zone := &PresenceZone{ID: zoneID, Label: "Synthetic permitted zone"}
	coordinates := &Coordinates{LatitudeDegrees: 0, LongitudeDegrees: 0, AccuracyMeters: &zero}
	details, err := NewPresenceDetailsValue(PresenceDetailsParts{Person: person, Zone: zone, Coordinates: coordinates})
	if err != nil {
		t.Fatal(err)
	}
	zone.Label = "changed"
	coordinates.LatitudeDegrees = 45
	*coordinates.AccuracyMeters = 100
	private, err := details.Components()
	if err != nil || private.Zone.Label != "Synthetic permitted zone" || private.Coordinates.LatitudeDegrees != 0 || *private.Coordinates.AccuracyMeters != 0 {
		t.Fatal("private data aliased its source or lost zero", err)
	}
	private.Zone.Label = "changed again"
	private.Coordinates.LongitudeDegrees = 30
	again, err := details.Components()
	if err != nil || again.Zone.Label != "Synthetic permitted zone" || again.Coordinates.LongitudeDegrees != 0 {
		t.Fatal("private output mutated cached source", err)
	}
	empty, err := NewPresenceDetailsValue(PresenceDetailsParts{Person: person})
	if err != nil {
		t.Fatal(err)
	}
	absent, err := empty.Components()
	if err != nil || absent.Zone != nil || absent.Coordinates != nil {
		t.Fatal("missing location became coordinates", err)
	}
}

func TestNHF010PresenceRefusesInvalidIdentityConfidenceAndCoordinates(t *testing.T) {
	person, _ := ParsePersonID("synthetic-person")
	for _, parts := range []PresenceParts{
		{Residence: ResidenceHome, Confidence: PresenceConfidenceHigh},
		{Person: person, Residence: ResidenceUnspecified, Confidence: PresenceConfidenceHigh},
		{Person: person, Residence: Residence(255), Confidence: PresenceConfidenceHigh},
		{Person: person, Residence: ResidenceHome, Confidence: PresenceConfidenceUnspecified},
		{Person: person, Residence: ResidenceHome, Confidence: PresenceConfidence(255)},
	} {
		if _, err := NewPresenceValue(parts); err == nil {
			t.Fatal("invalid aggregate admitted")
		}
	}
	for _, coordinates := range []Coordinates{
		{LatitudeDegrees: 91}, {LongitudeDegrees: 181}, {LatitudeDegrees: math.NaN()}, {LongitudeDegrees: math.Inf(1)},
	} {
		if _, err := NewPresenceDetailsValue(PresenceDetailsParts{Person: person, Coordinates: &coordinates}); err == nil {
			t.Fatal("invalid private coordinates admitted")
		}
	}
	if _, err := ParsePersonID(""); err == nil {
		t.Fatal("empty person identity admitted")
	}
	if _, err := ParsePresenceZoneID(""); err == nil {
		t.Fatal("empty zone identity admitted")
	}
}
