//! Parse the finite native capability vocabulary once at the wire boundary.
use super::{
    DeviceIntent, Power, Refusal,
    climate::{Climate, Fan},
    helper::{Helper, HelperRevision},
    light::ColorTemperature,
    number::Bounds,
};
use home_wire_contract::home::{actions::v1 as action, registry::v1 as wire};
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
    Climate(Climate),
    Fan(Fan),
    Helper(Helper),
    Binary,
    Measurement,
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
    pub fn parse(capability: &wire::CapabilityDescription) -> Self {
        use wire::capability_description::Constraints as C;
        if capability.major != 1
            || !capability.__buffa_unknown_fields.is_empty()
            || !capability.descriptor_set.is_empty()
            || !capability.descriptor_sha256.is_empty()
        {
            return Self::Unsupported;
        }
        match capability.constraints.as_ref() {
            Some(C::Switch(c)) => {
                if capability.schema_id == "core.switch.v1" && c.__buffa_unknown_fields.is_empty() {
                    Self::Switch { power: c.set_on }
                } else {
                    Self::Unsupported
                }
            }
            Some(C::Light(c)) => {
                if capability.schema_id == "core.light.v1" && c.__buffa_unknown_fields.is_empty() {
                    Self::Light {
                        power: c.set_on,
                        brightness: c
                            .brightness_percent
                            .as_option()
                            .and_then(Bounds::percentage),
                        color: c
                            .color_temp_mireds
                            .as_option()
                            .and_then(ColorTemperature::parse),
                    }
                } else {
                    Self::Unsupported
                }
            }
            Some(C::Binary(c)) => {
                if capability.schema_id == "core.binary.v1" && c.__buffa_unknown_fields.is_empty() {
                    Self::Binary
                } else {
                    Self::Unsupported
                }
            }
            Some(C::Measurement(c)) => {
                if capability.schema_id == "core.measurement.v1"
                    && c.__buffa_unknown_fields.is_empty()
                {
                    Self::Measurement
                } else {
                    Self::Unsupported
                }
            }
            Some(C::Climate(value)) => {
                if capability.schema_id == "core.climate.v1" {
                    Climate::parse(value)
                        .map(Self::Climate)
                        .unwrap_or(Self::Unsupported)
                } else {
                    Self::Unsupported
                }
            }
            Some(C::Fan(value)) => {
                if capability.schema_id == "core.fan.v1" {
                    Fan::parse(value)
                        .map(Self::Fan)
                        .unwrap_or(Self::Unsupported)
                } else {
                    Self::Unsupported
                }
            }
            Some(C::Helper(value)) => {
                if capability.schema_id == "core.helper.v1" {
                    Helper::parse(value)
                        .map(Self::Helper)
                        .unwrap_or(Self::Unsupported)
                } else {
                    Self::Unsupported
                }
            }
            Some(C::Extension(_) | C::Solar(_) | C::SolarEvent(_)) | None => Self::Unsupported,
        }
    }
    pub fn arguments(
        &self,
        intent: DeviceIntent,
        helper_revision: Option<HelperRevision>,
    ) -> Result<action::ActionArgs, Refusal> {
        use action::action_args::Arguments as A;
        let arguments = match intent {
            DeviceIntent::Power(value) => match self {
                Self::Switch { power: true } => A::SetSwitch(
                    action::SetSwitch {
                        on: value.on(),
                        ..Default::default()
                    }
                    .into(),
                ),
                Self::Light { power: true, .. } => A::SetLight(
                    action::SetLight {
                        on: Some(value.on()),
                        ..Default::default()
                    }
                    .into(),
                ),
                Self::Fan(fan) => A::SetFan(fan.power(value)?.into()),
                Self::Switch { power: false }
                | Self::Light { power: false, .. }
                | Self::Binary
                | Self::Measurement
                | Self::Climate(_)
                | Self::Helper(_)
                | Self::Unsupported => return Err(Refusal::Unsupported),
            },
            DeviceIntent::Brightness(value) => match self {
                Self::Light {
                    brightness: Some(bounds),
                    ..
                } => A::SetLight(
                    action::SetLight {
                        brightness_percent: Some(bounds.canonical(value)?),
                        ..Default::default()
                    }
                    .into(),
                ),
                Self::Light {
                    brightness: None, ..
                }
                | Self::Switch { .. }
                | Self::Binary
                | Self::Measurement
                | Self::Climate(_)
                | Self::Fan(_)
                | Self::Helper(_)
                | Self::Unsupported => return Err(Refusal::Unsupported),
            },
            DeviceIntent::ColorTemperature(value) => match self {
                Self::Light {
                    color: Some(color), ..
                } => A::SetLight(
                    action::SetLight {
                        color_temp_mireds: Some(color.admit(value)?.wire()),
                        ..Default::default()
                    }
                    .into(),
                ),
                Self::Light { color: None, .. }
                | Self::Switch { .. }
                | Self::Climate(_)
                | Self::Fan(_)
                | Self::Helper(_)
                | Self::Binary
                | Self::Measurement
                | Self::Unsupported => return Err(Refusal::Unsupported),
            },
            DeviceIntent::Climate(intent) => match self {
                Self::Climate(climate) => A::SetClimate(climate.arguments(intent)?.into()),
                Self::Switch { .. }
                | Self::Light { .. }
                | Self::Fan(_)
                | Self::Binary
                | Self::Measurement
                | Self::Helper(_)
                | Self::Unsupported => return Err(Refusal::Unsupported),
            },
            DeviceIntent::FanSpeed(speed) => match self {
                Self::Fan(fan) => A::SetFan(fan.speed(speed)?.into()),
                Self::Switch { .. }
                | Self::Light { .. }
                | Self::Climate(_)
                | Self::Binary
                | Self::Measurement
                | Self::Helper(_)
                | Self::Unsupported => return Err(Refusal::Unsupported),
            },
            DeviceIntent::Helper(intent) => match self {
                Self::Helper(helper) => {
                    A::Helper(helper.arguments(intent, helper_revision)?.into())
                }
                Self::Switch { .. }
                | Self::Light { .. }
                | Self::Climate(_)
                | Self::Fan(_)
                | Self::Binary
                | Self::Measurement
                | Self::Unsupported => return Err(Refusal::Unsupported),
            },
        };
        Ok(action::ActionArgs {
            arguments: Some(arguments),
            ..Default::default()
        })
    }
}
