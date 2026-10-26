package contracts

import (
	"math"
	"testing"
	"time"

	actions "github.com/damione1/Homeward/gen/home/actions/v1"
	env "github.com/damione1/Homeward/gen/home/environment/v1"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	"github.com/damione1/Homeward/internal/domain"
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

func nhf010EnvironmentCases(t *testing.T) []struct {
	description *registry.CapabilityDescription
	value       domain.StateValue
} {
	t.Helper()
	weather, err := AdmitWireWeatherValue(nhf010WireWeather())
	if err != nil {
		t.Fatal(err)
	}
	weatherState, _ := domain.NewWeatherStateValue(weather)
	person, _ := domain.ParsePersonID("synthetic-person")
	presence, _ := domain.NewPresenceValue(domain.PresenceParts{Person: person, Residence: domain.ResidenceUnknown, Confidence: domain.PresenceConfidenceUnknown})
	presenceState, _ := domain.NewPresenceStateValue(presence)
	details, _ := domain.NewPresenceDetailsValue(domain.PresenceDetailsParts{Person: person, Coordinates: &domain.Coordinates{}})
	detailState, _ := domain.NewPresenceDetailsStateValue(details)
	return []struct {
		description *registry.CapabilityDescription
		value       domain.StateValue
	}{
		{&registry.CapabilityDescription{SchemaId: WeatherSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic weather", Constraints: &registry.CapabilityDescription_Weather{Weather: &env.WeatherConstraints{}}}, weatherState},
		{&registry.CapabilityDescription{SchemaId: PresenceSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic aggregate", Constraints: &registry.CapabilityDescription_Presence{Presence: &env.PresenceConstraints{}}}, presenceState},
		{&registry.CapabilityDescription{SchemaId: PresenceDetailsSchema, Major: 1, Endpoint: "value", DisplayName: "Synthetic private details", Constraints: &registry.CapabilityDescription_PresenceDetails{PresenceDetails: &env.PresenceDetailsConstraints{}}}, detailState},
	}
}

func TestNHF010EnvironmentalObservationsRequireTheirExactReadOnlyCapability(t *testing.T) {
	q, _ := domain.NewQuality(domain.AvailabilityAvailable, domain.FreshnessStale, domain.ProvenanceEstimated)
	cases := nhf010EnvironmentCases(t)
	for _, item := range cases {
		catalog, _ := nha174Catalog(t, item.description)
		if err := catalog.AdmitReadOnlyDriverCapability(item.description); err != nil {
			t.Fatal("read-only environment declaration rejected", err)
		}
		wire, _ := testNHA017Wire(t, nil, q, &item.value, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), 1)
		context := &StateCapabilityAdmission{Catalog: catalog, Endpoint: item.description.Endpoint}
		admitted, err := AdmitWireState(wire, nil, context)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := EncodeObservedState(admitted)
		if err != nil || !proto.Equal(wire, encoded) {
			t.Fatal("typed state round trip lost environment metadata", err)
		}
		if _, err := AdmitWireState(wire, nil, nil); err == nil {
			t.Fatal("missing catalogue context admitted")
		}
		context.Endpoint = "other"
		if _, err := AdmitWireState(wire, nil, context); err == nil {
			t.Fatal("wrong endpoint admitted")
		}
		for _, other := range cases {
			if other.description.SchemaId == item.description.SchemaId {
				continue
			}
			foreign, _ := nha174Catalog(t, other.description)
			if _, err := AdmitWireState(wire, nil, &StateCapabilityAdmission{Catalog: foreign, Endpoint: other.description.Endpoint}); err == nil {
				t.Fatal("foreign capability admitted an environment payload")
			}
		}
		cmd := testNHA014Command(item.description.SchemaId, item.description.Endpoint, &actions.ActionArgs{Arguments: &actions.ActionArgs_SetSwitch{SetSwitch: &actions.SetSwitch{On: true}}})
		if err := catalog.AdmitCapabilityCommand(cmd, "epoch-a", 1); err == nil || err.Code != actions.ErrorCode_ERROR_CODE_UNSUPPORTED_CAPABILITY || err.FieldPath != "capability_schema_id" {
			t.Fatal("read-only environment accepted a write command")
		}
	}
}
