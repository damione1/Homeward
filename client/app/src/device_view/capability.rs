//! Parse the finite native capability vocabulary once at the wire boundary.
use super::{DeviceIntent, Power, Refusal};
use home_wire_contract::home::{actions::v1 as action, registry::v1 as wire};
#[derive(Clone, Copy, PartialEq)]
pub(super) struct Bounds {
    pub minimum: f64,
    pub maximum: f64,
    pub step: Option<f64>,
}
#[derive(Clone, Copy, PartialEq)]
pub(super) enum Control {
    Switch {
        power: bool,
    },
    Light {
        power: bool,
        brightness: Option<Bounds>,
    },
    Binary,
    Measurement,
    Unsupported,
}
impl Power {
    fn on(self) -> bool {
        match self {
            Self::On => true,
            Self::Off => false,
        }
    }
}
impl Bounds {
    fn parse(value: &wire::NumberBounds) -> Option<Self> {
        (value.__buffa_unknown_fields.is_empty()
            && value.minimum.is_finite()
            && value.maximum.is_finite()
            && value.minimum >= 0.
            && value.maximum <= 100.
            && value.maximum > value.minimum
            && value.step.is_none_or(|step| {
                step.is_finite() && step > 0. && step <= value.maximum - value.minimum
            }))
        .then_some(Self {
            minimum: value.minimum,
            maximum: value.maximum,
            step: value.step,
        })
    }
    fn canonical(self, value: f64) -> Result<f64, Refusal> {
        // Slint emits renderer f32 values. Accept only its boundary rounding;
        // canonical bounds and the optional device step remain server f64 facts.
        let tolerance = f64::from(f32::EPSILON) * self.maximum.abs().max(1.) * 2.;
        if !value.is_finite()
            || value < self.minimum - tolerance
            || value > self.maximum + tolerance
        {
            return Err(Refusal::InvalidValue);
        }
        let value = value.clamp(self.minimum, self.maximum);
        let canonical = match self.step {
            Some(step) => {
                let units = (self.maximum - self.minimum) / step;
                if !units.is_finite() {
                    return Err(Refusal::InvalidValue);
                }
                // Match the server's 1e-9 step tolerance, including decimal
                // boundaries such as (0.3 - 0.1) / 0.1 just below two.
                let maximum = if (units - units.round()).abs() <= 1e-9 {
                    units.round()
                } else {
                    units.floor()
                };
                let result = (self.minimum
                    + ((value - self.minimum) / step).round().min(maximum) * step)
                    .clamp(self.minimum, self.maximum);
                let actual = (result - self.minimum) / step;
                if !actual.is_finite() || (actual - actual.round()).abs() > 1e-9 {
                    return Err(Refusal::InvalidValue);
                }
                result
            }
            None => value,
        };
        if canonical.is_finite() {
            Ok(canonical)
        } else {
            Err(Refusal::InvalidValue)
        }
    }
}
impl Control {
    pub fn parse(capability: &wire::CapabilityDescription) -> Self {
        use wire::capability_description::Constraints as C;
        if capability.major != 1 || !capability.__buffa_unknown_fields.is_empty() {
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
                        brightness: c.brightness_percent.as_option().and_then(Bounds::parse),
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
            Some(
                C::Fan(_)
                | C::Climate(_)
                | C::Extension(_)
                | C::Helper(_)
                | C::Solar(_)
                | C::SolarEvent(_),
            )
            | None => Self::Unsupported,
        }
    }
    pub fn arguments(self, intent: DeviceIntent) -> Result<action::ActionArgs, Refusal> {
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
                Self::Switch { power: false }
                | Self::Light { power: false, .. }
                | Self::Binary
                | Self::Measurement
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
                | Self::Unsupported => return Err(Refusal::Unsupported),
            },
        };
        Ok(action::ActionArgs {
            arguments: Some(arguments),
            ..Default::default()
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn renderer_rounding_preserves_decimal_endpoints_and_never_leaves_the_device_grid() {
        let bounds = Bounds {
            minimum: 0.1,
            maximum: 0.3,
            step: Some(0.1),
        };
        assert_eq!(bounds.canonical(f64::from(0.3_f32)).unwrap(), 0.3);
        let bounds = Bounds {
            minimum: 20.,
            maximum: 80.,
            step: Some(7.),
        };
        assert_eq!(bounds.canonical(80.).unwrap(), 76.);
        assert!(matches!(bounds.canonical(81.), Err(Refusal::InvalidValue)));
    }
}
