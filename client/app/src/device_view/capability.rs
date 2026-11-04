//! Parse the finite native capability vocabulary once at the wire boundary.
use super::{
    DeviceIntent, Power, Refusal,
    climate::{Climate, Fan},
    controls::{Alarm, Cover, Lock},
    helper::{Helper, HelperRevision},
    light::ColorTemperature,
    number::Bounds,
};
use home_wire_contract::home::api::v2 as wire;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ObservationKind {
    Weather,
    Presence,
    Details,
    Solar,
    SolarEvent(wire::SolarEventKind),
}
#[derive(Clone, PartialEq)]
pub(super) enum Control {
    Switch {
        power: bool,
    },
    Light {
        power: bool,
        brightness: Option<Bounds>,
        color: Option<ColorTemperature>,
    },
    Level {
        bounds: Option<Bounds>,
    },
    Climate(Climate),
    Fan(Fan),
    Helper(Helper),
    Cover(Cover),
    Lock(Lock),
    Alarm(Alarm),
    Binary,
    Measurement,
    ReadOnly(ObservationKind),
    Unsupported,
}
impl Power {
    pub(super) fn on(self) -> bool {
        match self {
            Self::On => true,
            Self::Off => false,
        }
    }
}
impl Control {
    pub fn parse(capability: &wire::Capability) -> Self {
        use wire::capability::Trait as T;
        if !capability.__buffa_unknown_fields.is_empty() {
            return Self::Unsupported;
        }
        match capability.r#trait.as_ref() {
            Some(T::Power(value)) if value.__buffa_unknown_fields.is_empty() => Self::Switch {
                power: value.supports_set,
            },
            Some(T::Level(value)) if value.__buffa_unknown_fields.is_empty() => {
                let bounds = value
                    .bounds
                    .as_option()
                    .and_then(Bounds::percentage_definition);
                if value.supports_set && bounds.is_none() {
                    return Self::Unsupported;
                }
                Self::Level {
                    bounds: value.supports_set.then_some(bounds).flatten(),
                }
            }
            Some(T::LightControl(value)) if value.__buffa_unknown_fields.is_empty() => {
                let brightness = value
                    .brightness_percent
                    .as_option()
                    .and_then(Bounds::percentage);
                let color = value
                    .color_temperature_mireds
                    .as_option()
                    .and_then(ColorTemperature::parse);
                if (value.brightness_percent.is_set() && brightness.is_none())
                    || (value.color_temperature_mireds.is_set() && color.is_none())
                    || value
                        .transition_duration_bounds
                        .as_option()
                        .is_some_and(|bounds| Bounds::parse(bounds).is_none())
                {
                    return Self::Unsupported;
                }
                Self::Light {
                    power: value.supports_power,
                    brightness,
                    color,
                }
            }
            Some(T::TemperatureControl(value)) => Climate::parse(value)
                .map(Self::Climate)
                .unwrap_or(Self::Unsupported),
            Some(T::FanControl(value)) => Fan::parse(value)
                .map(Self::Fan)
                .unwrap_or(Self::Unsupported),
            Some(T::Helper(value)) if value.__buffa_unknown_fields.is_empty() => value
                .definition
                .as_option()
                .and_then(Helper::parse)
                .map(Self::Helper)
                .unwrap_or(Self::Unsupported),
            Some(T::Cover(value)) if value.__buffa_unknown_fields.is_empty() => value
                .definition
                .as_option()
                .and_then(Cover::parse)
                .map(Self::Cover)
                .unwrap_or(Self::Unsupported),
            Some(T::Lock(value)) if value.__buffa_unknown_fields.is_empty() => value
                .definition
                .as_option()
                .and_then(Lock::parse)
                .map(Self::Lock)
                .unwrap_or(Self::Unsupported),
            Some(T::Alarm(value)) if value.__buffa_unknown_fields.is_empty() => value
                .definition
                .as_option()
                .and_then(Alarm::parse)
                .map(Self::Alarm)
                .unwrap_or(Self::Unsupported),
            Some(T::NumericMeasurement(value)) if value.__buffa_unknown_fields.is_empty() => {
                Self::Measurement
            }
            Some(T::BinaryObservation(value)) if value.__buffa_unknown_fields.is_empty() => {
                Self::Binary
            }
            Some(T::Weather(value)) if value.__buffa_unknown_fields.is_empty() => {
                Self::ReadOnly(ObservationKind::Weather)
            }
            Some(T::Presence(value)) if value.__buffa_unknown_fields.is_empty() => {
                Self::ReadOnly(ObservationKind::Presence)
            }
            Some(T::PresenceDetails(value)) if value.__buffa_unknown_fields.is_empty() => {
                Self::ReadOnly(ObservationKind::Details)
            }
            Some(T::Solar(value)) if value.__buffa_unknown_fields.is_empty() => {
                Self::ReadOnly(ObservationKind::Solar)
            }
            Some(T::SolarEvent(value)) if value.__buffa_unknown_fields.is_empty() => {
                let Some(definition) = value
                    .definition
                    .as_option()
                    .filter(|d| d.__buffa_unknown_fields.is_empty())
                else {
                    return Self::Unsupported;
                };
                match definition.event.as_known() {
                    Some(
                        event @ (wire::SolarEventKind::Dawn
                        | wire::SolarEventKind::Dusk
                        | wire::SolarEventKind::Midnight
                        | wire::SolarEventKind::Noon
                        | wire::SolarEventKind::Rising
                        | wire::SolarEventKind::Setting),
                    ) => Self::ReadOnly(ObservationKind::SolarEvent(event)),
                    Some(wire::SolarEventKind::Unspecified) | None => Self::Unsupported,
                }
            }
            Some(
                T::Power(_)
                | T::Level(_)
                | T::LightControl(_)
                | T::Helper(_)
                | T::Cover(_)
                | T::Lock(_)
                | T::Alarm(_)
                | T::NumericMeasurement(_)
                | T::BinaryObservation(_)
                | T::Weather(_)
                | T::Presence(_)
                | T::PresenceDetails(_)
                | T::Solar(_)
                | T::SolarEvent(_)
                | T::Opaque(_)
                | T::MediaPlayback(_),
            )
            | None => Self::Unsupported,
        }
    }
    pub fn arguments(
        &self,
        intent: DeviceIntent,
        helper_revision: Option<HelperRevision>,
    ) -> Result<wire::execute_capability_request::Command, Refusal> {
        use wire::execute_capability_request::Command as C;
        // Matching both the control and intent prevents one trait borrowing an
        // unrelated sibling's support. Current action_access is fenced separately.
        match (self, intent) {
            (Self::Switch { power: true }, DeviceIntent::Power(value)) => {
                Ok(C::Power(Box::new(wire::SetPowerCommand {
                    value: if value.on() {
                        wire::PowerValue::On
                    } else {
                        wire::PowerValue::Off
                    }
                    .into(),
                    ..Default::default()
                })))
            }
            (Self::Light { power: true, .. }, DeviceIntent::Power(value)) => {
                Ok(C::Light(Box::new(wire::SetLightCommand {
                    power: wire::SetPowerCommand {
                        value: if value.on() {
                            wire::PowerValue::On
                        } else {
                            wire::PowerValue::Off
                        }
                        .into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                })))
            }
            (
                Self::Light {
                    brightness: Some(bounds),
                    ..
                },
                DeviceIntent::Brightness(value),
            ) => Ok(C::Light(Box::new(wire::SetLightCommand {
                level: wire::SetLevelCommand {
                    target: Some(wire::set_level_command::Target::Percentage(
                        bounds.canonical(value)?,
                    )),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }))),
            (
                Self::Level {
                    bounds: Some(bounds),
                },
                DeviceIntent::Brightness(value),
            ) => Ok(C::Level(Box::new(wire::SetLevelCommand {
                target: Some(wire::set_level_command::Target::Percentage(
                    bounds.canonical(value)?,
                )),
                ..Default::default()
            }))),
            (
                Self::Light {
                    color: Some(color), ..
                },
                DeviceIntent::ColorTemperature(value),
            ) => Ok(C::Light(Box::new(wire::SetLightCommand {
                color_temperature_mireds: Some(
                    i32::try_from(color.admit(value)?.wire()).map_err(|_| Refusal::InvalidValue)?,
                ),
                ..Default::default()
            }))),
            (Self::Climate(value), DeviceIntent::Climate(intent)) => {
                Ok(C::TemperatureControl(Box::new(
                    home_wire_contract::api_contract::encode_thermal(value.arguments(intent)?),
                )))
            }
            (Self::Fan(value), DeviceIntent::Power(intent)) => Ok(C::Fan(Box::new(
                home_wire_contract::api_contract::encode_fan(&value.power(intent)?),
            ))),
            (Self::Fan(value), DeviceIntent::FanSpeed(intent)) => Ok(C::Fan(Box::new(
                home_wire_contract::api_contract::encode_fan(&value.speed(intent)?),
            ))),
            (Self::Helper(value), DeviceIntent::Helper(intent)) => Ok(C::Helper(Box::new(
                value.arguments(intent, helper_revision)?,
            ))),
            (Self::Cover(value), DeviceIntent::Cover(intent)) => {
                Ok(C::Cover(Box::new(value.arguments(intent)?)))
            }
            (Self::Lock(value), DeviceIntent::Lock(intent)) => {
                Ok(C::Lock(Box::new(value.arguments(intent)?)))
            }
            (Self::Alarm(value), DeviceIntent::Alarm(intent)) => {
                Ok(C::Alarm(Box::new(value.arguments(intent)?)))
            }
            (
                Self::Switch { .. }
                | Self::Level { .. }
                | Self::Light { .. }
                | Self::Climate(_)
                | Self::Fan(_)
                | Self::Helper(_)
                | Self::Cover(_)
                | Self::Lock(_)
                | Self::Alarm(_)
                | Self::Binary
                | Self::Measurement
                | Self::ReadOnly(_)
                | Self::Unsupported,
                DeviceIntent::Power(_)
                | DeviceIntent::Brightness(_)
                | DeviceIntent::ColorTemperature(_)
                | DeviceIntent::Climate(_)
                | DeviceIntent::FanSpeed(_)
                | DeviceIntent::Helper(_)
                | DeviceIntent::Cover(_)
                | DeviceIntent::Lock(_)
                | DeviceIntent::Alarm(_),
            ) => Err(Refusal::Unsupported),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn observation_traits_never_gain_a_power_intention() {
        use wire::capability::Trait as T;
        for trait_value in [
            T::Weather(Box::default()),
            T::Presence(Box::default()),
            T::PresenceDetails(Box::default()),
        ] {
            let capability = wire::Capability {
                r#trait: Some(trait_value),
                ..Default::default()
            };
            let control = Control::parse(&capability);
            assert!(matches!(control, Control::ReadOnly(_)));
            assert!(
                control
                    .arguments(DeviceIntent::Power(Power::On), None)
                    .is_err()
            );
        }
    }
    #[test]
    fn level_and_light_intentions_keep_their_exact_trait_and_zero() {
        let bounds = Bounds::percentage_definition(&wire::PercentageBounds {
            minimum: 0.,
            maximum: 100.,
            ..Default::default()
        })
        .unwrap();
        let level = Control::Level {
            bounds: Some(bounds),
        };
        assert!(
            matches!(level.arguments(DeviceIntent::Brightness(0.), None).unwrap(), wire::execute_capability_request::Command::Level(value) if value.target == Some(wire::set_level_command::Target::Percentage(0.)))
        );
        assert!(
            level
                .arguments(DeviceIntent::Power(Power::Off), None)
                .is_err()
        );
    }
}
