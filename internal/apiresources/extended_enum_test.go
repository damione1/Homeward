package apiresources

import (
	controls "github.com/damione1/Homeward/gen/home/controls/v1"
	environment "github.com/damione1/Homeward/gen/home/environment/v1"
	helpers "github.com/damione1/Homeward/gen/home/helpers/v1"
	solar "github.com/damione1/Homeward/gen/home/solar/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
	"testing"
)

func TestExtendedNativeEnumDescriptorCoverage(t *testing.T) {
	tests := []struct {
		descriptor protoreflect.EnumDescriptor
		convert    func(int32) error
	}{
		{controls.CoverState(0).Descriptor(), func(v int32) error { _, err := convertCoverState(controls.CoverState(v)); return err }},
		{controls.CoverOperation(0).Descriptor(), func(v int32) error { _, err := convertCoverOperation(controls.CoverOperation(v)); return err }},
		{controls.LockState(0).Descriptor(), func(v int32) error { _, err := convertLockState(controls.LockState(v)); return err }},
		{controls.LockOperation(0).Descriptor(), func(v int32) error { _, err := convertLockOperation(controls.LockOperation(v)); return err }},
		{controls.AlarmState(0).Descriptor(), func(v int32) error { _, err := convertAlarmState(controls.AlarmState(v)); return err }},
		{controls.AlarmOperation(0).Descriptor(), func(v int32) error { _, err := convertAlarmOperation(controls.AlarmOperation(v)); return err }},
		{solar.SolarEventKind(0).Descriptor(), func(v int32) error { _, err := convertSolarEventKind(solar.SolarEventKind(v)); return err }},
		{solar.SolarHorizon(0).Descriptor(), func(v int32) error { _, err := convertSolarHorizon(solar.SolarHorizon(v)); return err }},
		{solar.SolarAbsenceReason(0).Descriptor(), func(v int32) error { _, err := convertSolarAbsenceReason(solar.SolarAbsenceReason(v)); return err }},
		{environment.WeatherCondition(0).Descriptor(), func(v int32) error { _, err := convertWeatherCondition(environment.WeatherCondition(v)); return err }},
		{environment.TemperatureUnit(0).Descriptor(), func(v int32) error { _, err := convertTemperatureUnit(environment.TemperatureUnit(v)); return err }},
		{environment.SpeedUnit(0).Descriptor(), func(v int32) error { _, err := convertSpeedUnit(environment.SpeedUnit(v)); return err }},
		{environment.PrecipitationUnit(0).Descriptor(), func(v int32) error { _, err := convertPrecipitationUnit(environment.PrecipitationUnit(v)); return err }},
		{environment.ForecastKind(0).Descriptor(), func(v int32) error { _, err := convertForecastKind(environment.ForecastKind(v)); return err }},
		{environment.Residence(0).Descriptor(), func(v int32) error { _, err := convertResidence(environment.Residence(v)); return err }},
		{environment.PresenceConfidence(0).Descriptor(), func(v int32) error {
			_, err := convertPresenceConfidence(environment.PresenceConfidence(v))
			return err
		}},
		{helpers.HelperKind(0).Descriptor(), func(v int32) error { _, err := convertHelperKind(helpers.HelperKind(v)); return err }},
		{helpers.HelperOperation(0).Descriptor(), func(v int32) error { _, err := convertHelperOperation(helpers.HelperOperation(v)); return err }},
		{helpers.HelperReason(0).Descriptor(), func(v int32) error { _, err := convertHelperReason(helpers.HelperReason(v)); return err }},
	}
	for _, test := range tests {
		values := test.descriptor.Values()
		for i := 0; i < values.Len(); i++ {
			v := values.Get(i)
			if err := test.convert(int32(v.Number())); err != nil {
				t.Fatalf("unmapped native value %s: %v", v.FullName(), err)
			}
		}
		if test.convert(991) == nil {
			t.Fatalf("unknown native value admitted by %s", test.descriptor.FullName())
		}
	}
}
