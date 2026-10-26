package contracts

import (
	"math"
	"testing"
	"time"

	env "github.com/damione1/Homeward/gen/home/environment/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func nhf010WireWeather() *env.WeatherValue {
	issued := time.Date(2026, 3, 8, 6, 30, 0, 42, time.UTC)
	return &env.WeatherValue{
		Condition:       env.WeatherCondition_WEATHER_CONDITION_UNKNOWN.Enum(),
		Temperature:     &env.Temperature{Value: proto.Float64(0), Unit: env.TemperatureUnit_TEMPERATURE_UNIT_CELSIUS},
		HumidityPercent: proto.Float64(0),
		Wind:            &env.Wind{Speed: proto.Float64(0), Unit: env.SpeedUnit_SPEED_UNIT_KNOTS},
		SourceLabel:     "Synthetic provider",
		Forecast: &env.Forecast{IssuedAt: timestamppb.New(issued), ValidUntil: timestamppb.New(issued.Add(time.Hour)),
			Periods: []*env.ForecastPeriod{{StartsAt: timestamppb.New(issued), EndsAt: timestamppb.New(issued.Add(time.Hour)),
				Kind:                            env.ForecastKind_FORECAST_KIND_HOURLY,
				Low:                             &env.Temperature{Value: proto.Float64(0), Unit: env.TemperatureUnit_TEMPERATURE_UNIT_FAHRENHEIT},
				High:                            &env.Temperature{Value: proto.Float64(5), Unit: env.TemperatureUnit_TEMPERATURE_UNIT_FAHRENHEIT},
				PrecipitationProbabilityPercent: proto.Float64(0),
				Precipitation:                   &env.Precipitation{Amount: proto.Float64(0), Unit: env.PrecipitationUnit_PRECIPITATION_UNIT_INCHES}}}},
	}
}

func nhf010Unknown(message proto.Message) {
	raw := protowire.AppendTag(nil, 99, protowire.BytesType)
	raw = protowire.AppendString(raw, "synthetic refused field")
	message.ProtoReflect().SetUnknown(raw)
}

func TestNHF010WeatherWireRoundTripPreservesUnitsAbsenceZeroAndNanoseconds(t *testing.T) {
	input := nhf010WireWeather()
	raw, err := proto.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded env.WeatherValue
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	value, err := AdmitWireWeatherValue(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeWeatherValue(value)
	if err != nil || !proto.Equal(encoded, input) {
		t.Fatal("weather lost typed data in its wire round trip", err)
	}
	*decoded.Temperature.Value = 80
	*decoded.Forecast.Periods[0].Low.Value = -20
	*encoded.HumidityPercent = 100
	*encoded.Forecast.Periods[0].PrecipitationProbabilityPercent = 90
	again, err := EncodeWeatherValue(value)
	if err != nil || !proto.Equal(again, input) {
		t.Fatal("wire caller mutated admitted weather", err)
	}
	if again.Wind.BearingDegrees != nil || again.Forecast.Periods[0].Temperature != nil || again.Forecast.Periods[0].Condition != nil {
		t.Fatal("an absent optional weather field became an observation")
	}
}

func TestNHF010WeatherWireRefusesUnknownAndPartialPresentFieldsAtomically(t *testing.T) {
	for _, mutate := range []func(*env.WeatherValue){
		func(v *env.WeatherValue) { v.Temperature.Value = nil },
		func(v *env.WeatherValue) { v.Temperature.Unit = env.TemperatureUnit(255) },
		func(v *env.WeatherValue) { *v.Temperature.Value = math.Inf(1) },
		func(v *env.WeatherValue) { v.Condition = env.WeatherCondition(255).Enum() },
		func(v *env.WeatherValue) { v.Wind.Speed = nil },
		func(v *env.WeatherValue) { v.Wind.Unit = env.SpeedUnit(255) },
		func(v *env.WeatherValue) { v.Forecast.IssuedAt = nil },
		func(v *env.WeatherValue) { v.Forecast.ValidUntil.Nanos = -1 },
		func(v *env.WeatherValue) { v.Forecast.Periods[0] = nil },
		func(v *env.WeatherValue) { v.Forecast.Periods[0].Kind = env.ForecastKind(255) },
		func(v *env.WeatherValue) { v.Forecast.Periods[0].Precipitation.Amount = nil },
		func(v *env.WeatherValue) { v.Forecast.Periods[0].Precipitation.Unit = env.PrecipitationUnit(255) },
		func(v *env.WeatherValue) { nhf010Unknown(v) },
		func(v *env.WeatherValue) { nhf010Unknown(v.Temperature) },
		func(v *env.WeatherValue) { nhf010Unknown(v.Forecast.Periods[0]) },
		func(v *env.WeatherValue) { nhf010Unknown(v.Forecast.IssuedAt) },
	} {
		value := nhf010WireWeather()
		mutate(value)
		if _, err := AdmitWireWeatherValue(value); err == nil {
			t.Fatal("invalid wire weather was accepted or silently dropped a field")
		}
	}
	if _, err := AdmitWireWeatherValue(nil); err == nil {
		t.Fatal("nil weather admitted")
	}
	if _, err := AdmitWireWeatherValue(&env.WeatherValue{}); err == nil {
		t.Fatal("empty weather admitted")
	}
}

func TestNHF010PresenceWireCannotCarryPrivateLocationThroughTheAggregateCodec(t *testing.T) {
	aggregate := &env.PresenceValue{PersonId: "synthetic-person", Residence: env.Residence_RESIDENCE_UNKNOWN,
		Confidence: env.PresenceConfidence_PRESENCE_CONFIDENCE_UNKNOWN, SourceLabel: "Synthetic source"}
	value, err := AdmitWirePresenceValue(aggregate)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodePresenceValue(value)
	if err != nil || !proto.Equal(encoded, aggregate) {
		t.Fatal("aggregate wire changed", err)
	}
	if fields := aggregate.ProtoReflect().Descriptor().Fields(); fields.ByName("coordinates") != nil || fields.ByName("zone") != nil {
		t.Fatal("aggregate type acquired private location fields")
	}
	private := &env.PresenceDetailsValue{PersonId: "synthetic-person", Zone: &env.PresenceZone{ZoneId: "synthetic-zone", Label: "Permitted zone"},
		Coordinates: &env.Coordinates{LatitudeDegrees: proto.Float64(0), LongitudeDegrees: proto.Float64(0), AccuracyMeters: proto.Float64(0)}}
	details, err := AdmitWirePresenceDetailsValue(private)
	if err != nil {
		t.Fatal(err)
	}
	result, err := EncodePresenceDetailsValue(details)
	if err != nil || !proto.Equal(result, private) {
		t.Fatal("private location lost optional zero", err)
	}
	raw, err := proto.Marshal(private)
	if err != nil {
		t.Fatal(err)
	}
	var confused env.PresenceValue
	if err := proto.Unmarshal(raw, &confused); err != nil {
		t.Fatal(err)
	}
	if _, err := AdmitWirePresenceValue(&confused); err == nil {
		t.Fatal("private details accepted as simplified presence")
	}
	*private.Coordinates.LatitudeDegrees = 40
	*result.Coordinates.LongitudeDegrees = -50
	again, err := EncodePresenceDetailsValue(details)
	if err != nil || *again.Coordinates.LatitudeDegrees != 0 || *again.Coordinates.LongitudeDegrees != 0 {
		t.Fatal("wire location aliases its caller", err)
	}
}

func TestNHF010PresenceWireRejectsMissingIdentityUnknownEnumsAndPartialCoordinates(t *testing.T) {
	for _, value := range []*env.PresenceValue{nil, {},
		{PersonId: "synthetic-person", Residence: env.Residence(255), Confidence: env.PresenceConfidence_PRESENCE_CONFIDENCE_HIGH},
		{PersonId: "synthetic-person", Residence: env.Residence_RESIDENCE_HOME, Confidence: env.PresenceConfidence(255)},
	} {
		if _, err := AdmitWirePresenceValue(value); err == nil {
			t.Fatal("invalid aggregate admitted")
		}
	}
	for _, position := range []*env.Coordinates{
		{}, {LatitudeDegrees: proto.Float64(0)}, {LongitudeDegrees: proto.Float64(0)},
		{LatitudeDegrees: proto.Float64(91), LongitudeDegrees: proto.Float64(0)},
		{LatitudeDegrees: proto.Float64(0), LongitudeDegrees: proto.Float64(0), AccuracyMeters: proto.Float64(-1)},
	} {
		value := &env.PresenceDetailsValue{PersonId: "synthetic-person", Coordinates: position}
		if _, err := AdmitWirePresenceDetailsValue(value); err == nil {
			t.Fatal("partial or invalid coordinates became a real position")
		}
	}
}

func TestNHF010EnvironmentEnumMappingsCoverEveryDeclaredOperationalVariant(t *testing.T) {
	for _, row := range []struct {
		descriptor protoreflect.EnumDescriptor
		entries    int
	}{
		{env.WeatherCondition(0).Descriptor(), len(weatherConditions)},
		{env.TemperatureUnit(0).Descriptor(), len(temperatureUnits)},
		{env.SpeedUnit(0).Descriptor(), len(speedUnits)},
		{env.PrecipitationUnit(0).Descriptor(), len(precipitationUnits)},
		{env.ForecastKind(0).Descriptor(), len(forecastKinds)},
		{env.Residence(0).Descriptor(), len(residenceStates)},
		{env.PresenceConfidence(0).Descriptor(), len(presenceConfidences)},
	} {
		if row.descriptor.Values().Len() != row.entries+1 {
			t.Fatal("wire enum changed without an explicit domain mapping", row.descriptor.FullName())
		}
	}
}
