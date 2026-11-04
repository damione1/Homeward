package apiresources

import (
	api "github.com/damione1/Homeward/gen/home/api/v2"
	registry "github.com/damione1/Homeward/gen/home/registry/v1"
	state "github.com/damione1/Homeward/gen/home/state/v1"
	"google.golang.org/protobuf/proto"
	"math"
)

// projectCover keeps declaration support distinct from observed values.
func projectCover(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetCover() == nil {
		return nil, ErrProjection
	}
	definition, err := convertCoverConstraints(d.GetCover())
	if err != nil {
		return nil, err
	}
	capability := &api.CoverCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertCoverValue(raw.GetCover())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.CoverObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: Cover, value: &api.Capability{Trait: &api.Capability_Cover{Cover: capability}}}}, nil
}

// projectLock keeps declaration support distinct from observed values.
func projectLock(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetLock() == nil {
		return nil, ErrProjection
	}
	definition, err := convertLockConstraints(d.GetLock())
	if err != nil {
		return nil, err
	}
	capability := &api.LockCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertLockValue(raw.GetLock())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.LockObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: Lock, value: &api.Capability{Trait: &api.Capability_Lock{Lock: capability}}}}, nil
}

// projectAlarm keeps declaration support distinct from observed values.
func projectAlarm(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetAlarm() == nil {
		return nil, ErrProjection
	}
	definition, err := convertAlarmConstraints(d.GetAlarm())
	if err != nil {
		return nil, err
	}
	capability := &api.AlarmCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertAlarmValue(raw.GetAlarm())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.AlarmObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: Alarm, value: &api.Capability{Trait: &api.Capability_Alarm{Alarm: capability}}}}, nil
}

// projectHelper keeps declaration support distinct from observed values.
func projectHelper(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetHelper() == nil {
		return nil, ErrProjection
	}
	definition, err := convertHelperConstraints(d.GetHelper())
	if err != nil {
		return nil, err
	}
	capability := &api.HelperCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertHelperValue(raw.GetHelper())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.HelperObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: Helper, value: &api.Capability{Trait: &api.Capability_Helper{Helper: capability}}}}, nil
}

// projectSolar keeps declaration support distinct from observed values.
func projectSolar(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetSolar() == nil {
		return nil, ErrProjection
	}
	definition, err := convertSolarConstraints(d.GetSolar())
	if err != nil {
		return nil, err
	}
	capability := &api.SolarCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertSolarValue(raw.GetSolar())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.SolarObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: Solar, value: &api.Capability{Trait: &api.Capability_Solar{Solar: capability}}}}, nil
}

// projectSolarEvent keeps declaration support distinct from observed values.
func projectSolarEvent(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetSolarEvent() == nil {
		return nil, ErrProjection
	}
	definition, err := convertSolarEventConstraints(d.GetSolarEvent())
	if err != nil {
		return nil, err
	}
	capability := &api.SolarEventCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertSolarEventValue(raw.GetSolarEvent())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.SolarEventObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: SolarEvent, value: &api.Capability{Trait: &api.Capability_SolarEvent{SolarEvent: capability}}}}, nil
}

// projectWeather keeps declaration support distinct from observed values.
func projectWeather(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetWeather() == nil {
		return nil, ErrProjection
	}
	definition, err := convertWeatherConstraints(d.GetWeather())
	if err != nil {
		return nil, err
	}
	capability := &api.WeatherCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertWeatherValue(raw.GetWeather())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.WeatherObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: Weather, value: &api.Capability{Trait: &api.Capability_Weather{Weather: capability}}}}, nil
}

// projectPresence keeps declaration support distinct from observed values.
func projectPresence(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetPresence() == nil {
		return nil, ErrProjection
	}
	definition, err := convertPresenceConstraints(d.GetPresence())
	if err != nil {
		return nil, err
	}
	capability := &api.PresenceCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertPresenceValue(raw.GetPresence())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.PresenceObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: Presence, value: &api.Capability{Trait: &api.Capability_Presence{Presence: capability}}}}, nil
}

// projectPresenceDetails keeps declaration support distinct from observed values.
func projectPresenceDetails(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetPresenceDetails() == nil {
		return nil, ErrProjection
	}
	definition, err := convertPresenceDetailsConstraints(d.GetPresenceDetails())
	if err != nil {
		return nil, err
	}
	capability := &api.PresenceDetailsCapability{Definition: definition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		value, err := convertPresenceDetailsValue(raw.GetPresenceDetails())
		if err != nil {
			return nil, err
		}
		capability.Observation = &api.PresenceDetailsObservation{Value: value, ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
	}
	return []Capability{{trait: PresenceDetails, value: &api.Capability{Trait: &api.Capability_PresenceDetails{PresenceDetails: capability}}}}, nil
}

func projectExtension(d *registry.CapabilityDescription, _ *state.ObservedState) ([]Capability, error) {
	if d.GetExtension() == nil || d.SchemaId == "" || len(d.DescriptorSha256) != 32 {
		return nil, ErrProjection
	}
	return []Capability{{trait: Opaque, value: &api.Capability{Trait: &api.Capability_Opaque{Opaque: &api.OpaqueCapability{SchemaId: d.SchemaId, MajorVersion: int64(d.Major), DisplayName: d.DisplayName, DescriptorSha256: append([]byte(nil), d.DescriptorSha256...)}}}}}, nil
}
func projectFan(d *registry.CapabilityDescription, raw *state.ObservedState) ([]Capability, error) {
	if d.GetFan() == nil {
		return nil, ErrProjection
	}
	c := d.GetFan()
	capability := &api.FanControlCapability{SupportsPower: c.SetOn, Speed: &api.SelectorDefinition{Options: append([]string(nil), c.SpeedOptions...)}}
	seen := map[string]bool{}
	for _, key := range c.SpeedOptions {
		if key == "" || seen[key] {
			return nil, ErrProjection
		}
		seen[key] = true
	}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		observation := &api.FanControlObservation{ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
		if value := raw.GetFan(); value != nil {
			v, err := fanSpeed(value.Speed)
			if err != nil {
				return nil, err
			}
			observation.Speed = &v
		}
		capability.Observation = observation
	}
	return []Capability{{trait: Fan, value: &api.Capability{Trait: &api.Capability_FanControl{FanControl: capability}}}}, nil
}
func fanSpeed(v state.FanSpeed) (api.FanSpeed, error) {
	switch v {
	case state.FanSpeed_FAN_SPEED_OFF:
		return api.FanSpeed_FAN_SPEED_OFF, nil
	case state.FanSpeed_FAN_SPEED_LOW:
		return api.FanSpeed_FAN_SPEED_LOW, nil
	case state.FanSpeed_FAN_SPEED_MEDIUM:
		return api.FanSpeed_FAN_SPEED_MEDIUM, nil
	case state.FanSpeed_FAN_SPEED_MEDIUM_HIGH:
		return api.FanSpeed_FAN_SPEED_MEDIUM_HIGH, nil
	case state.FanSpeed_FAN_SPEED_HIGH:
		return api.FanSpeed_FAN_SPEED_HIGH, nil
	case state.FanSpeed_FAN_SPEED_UNSPECIFIED:
		return 0, ErrProjection
	default:
		return 0, ErrProjection
	}
}
func lightControl(c *registry.LightConstraints, raw *state.ObservedState) (*api.LightControlCapability, error) {
	brightness, err := bounds(c.BrightnessPercent)
	if err != nil {
		return nil, err
	}
	color, err := bounds(c.ColorTempMireds)
	if err != nil {
		return nil, err
	}
	transition, err := bounds(c.TransitionSeconds)
	if err != nil {
		return nil, err
	}
	result := &api.LightControlCapability{SupportsPower: c.SetOn, BrightnessPercent: brightness, ColorTemperatureMireds: color, TransitionDurationBounds: transition}
	if raw != nil {
		q, err := quality(raw.Quality)
		if err != nil {
			return nil, err
		}
		v := &api.LightControlObservation{ObservedTime: proto.CloneOf(raw.ObservedAt), Quality: q}
		if x := raw.GetLight(); x != nil {
			v.On = x.On
			v.BrightnessPercent = x.BrightnessPercent
			if x.ColorTempMireds != nil {
				if *x.ColorTempMireds > math.MaxInt32 {
					return nil, ErrProjection
				}
				n := int32(*x.ColorTempMireds)
				v.ColorTemperatureMireds = &n
			}
		}
		result.Observation = v
	}
	return result, nil
}
