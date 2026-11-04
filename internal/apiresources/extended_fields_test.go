package apiresources

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	environment "github.com/damione1/Homeward/gen/home/environment/v1"
	helpers "github.com/damione1/Homeward/gen/home/helpers/v1"
	solar "github.com/damione1/Homeward/gen/home/solar/v1"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/reflect/protoreflect"
	"testing"
)

// The static field sets make a native schema addition fail until its public
// projection is consciously updated; matching enums alone cannot detect loss.
func TestNativeMessageFieldProjectionCoverage(t *testing.T) {
	type pair struct {
		source, target protoreflect.MessageDescriptor
		fields         []protoreflect.FieldNumber
	}
	pairs := []pair{
		{(&controls.PercentBounds{}).ProtoReflect().Descriptor(), (&api.PercentageBounds{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3}},
		{(&controls.CoverConstraints{}).ProtoReflect().Descriptor(), (&api.CoverDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3, 4, 5}},
		{(&controls.CoverValue{}).ProtoReflect().Descriptor(), (&api.CoverValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3}},
		{(&controls.LockConstraints{}).ProtoReflect().Descriptor(), (&api.LockDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2}},
		{(&controls.LockValue{}).ProtoReflect().Descriptor(), (&api.LockValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1}},
		{(&controls.AlarmConstraints{}).ProtoReflect().Descriptor(), (&api.AlarmDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2}},
		{(&controls.AlarmValue{}).ProtoReflect().Descriptor(), (&api.AlarmValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1}},
		{(&solar.SolarOccurrence{}).ProtoReflect().Descriptor(), (&api.SolarOccurrence{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2}},
		{(&solar.SolarValue{}).ProtoReflect().Descriptor(), (&api.SolarValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{(&solar.SolarEventValue{}).ProtoReflect().Descriptor(), (&api.SolarEventValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2}},
		{(&solar.SolarConstraints{}).ProtoReflect().Descriptor(), (&api.SolarDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{}},
		{(&solar.SolarEventConstraints{}).ProtoReflect().Descriptor(), (&api.SolarEventDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1}},
		{(&environment.Temperature{}).ProtoReflect().Descriptor(), (&api.Temperature{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2}},
		{(&environment.Wind{}).ProtoReflect().Descriptor(), (&api.Wind{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3}},
		{(&environment.Precipitation{}).ProtoReflect().Descriptor(), (&api.Precipitation{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2}},
		{(&environment.ForecastPeriod{}).ProtoReflect().Descriptor(), (&api.ForecastPeriod{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		{(&environment.Forecast{}).ProtoReflect().Descriptor(), (&api.Forecast{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3}},
		{(&environment.WeatherValue{}).ProtoReflect().Descriptor(), (&api.WeatherValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3, 4, 5, 6}},
		{(&environment.WeatherConstraints{}).ProtoReflect().Descriptor(), (&api.WeatherDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{}},
		{(&environment.PresenceValue{}).ProtoReflect().Descriptor(), (&api.PresenceValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3, 4}},
		{(&environment.PresenceConstraints{}).ProtoReflect().Descriptor(), (&api.PresenceDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{}},
		{(&environment.PresenceZone{}).ProtoReflect().Descriptor(), (&api.PresenceZone{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2}},
		{(&environment.Coordinates{}).ProtoReflect().Descriptor(), (&api.Coordinates{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3}},
		{(&environment.PresenceDetailsValue{}).ProtoReflect().Descriptor(), (&api.PresenceDetailsValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3, 4}},
		{(&environment.PresenceDetailsConstraints{}).ProtoReflect().Descriptor(), (&api.PresenceDetailsDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{}},
		{(&helpers.CivilDate{}).ProtoReflect().Descriptor(), (&date.Date{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3}},
		{(&helpers.CivilTime{}).ProtoReflect().Descriptor(), (&timeofday.TimeOfDay{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3}},
		{(&helpers.CivilDateTime{}).ProtoReflect().Descriptor(), (&api.LocalDateTime{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2}},
		{(&helpers.HelperValue{}).ProtoReflect().Descriptor(), (&api.HelperValue{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{(&helpers.HelperConstraints{}).ProtoReflect().Descriptor(), (&api.HelperDefinition{}).ProtoReflect().Descriptor(), []protoreflect.FieldNumber{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
	}
	for _, pair := range pairs {
		if pair.source.Fields().Len() != len(pair.fields) {
			t.Fatalf("native %s added fields without an explicit projection", pair.source.FullName())
		}
		for _, number := range pair.fields {
			if pair.source.Fields().ByNumber(number) == nil || pair.target.Fields().ByNumber(number) == nil {
				t.Fatalf("unmapped %s field %d", pair.source.FullName(), number)
			}
		}
	}
}
