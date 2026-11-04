//! Immutable instance constraints and typed climate/fan command construction.
use super::{
    Power, Refusal,
    number::{Bounds, NumericInput},
};
use home_client_model::api as model;
use home_wire_contract::home::api::v2 as wire;

fn options<E: buffa::Enumeration>(
    values: &[buffa::EnumValue<E>],
    valid: impl Fn(E) -> bool,
    maximum: usize,
) -> Option<Vec<E>> {
    if values.len() > maximum {
        return None;
    }
    let mut result = Vec::with_capacity(values.len());
    for value in values {
        let value = value.as_known()?;
        if !valid(value) || result.contains(&value) {
            return None;
        }
        result.push(value);
    }
    Some(result)
}
fn mode(value: wire::TemperatureMode) -> bool {
    match value {
        wire::TemperatureMode::Off
        | wire::TemperatureMode::Heat
        | wire::TemperatureMode::Cool
        | wire::TemperatureMode::Auto
        | wire::TemperatureMode::Dry
        | wire::TemperatureMode::FanOnly => true,
        wire::TemperatureMode::Unspecified => false,
    }
}
fn activity(value: wire::TemperatureActivity) -> bool {
    match value {
        wire::TemperatureActivity::Idle
        | wire::TemperatureActivity::Heating
        | wire::TemperatureActivity::Cooling
        | wire::TemperatureActivity::Drying
        | wire::TemperatureActivity::FanOnly => true,
        wire::TemperatureActivity::Unspecified => false,
    }
}
fn fan_target(value: wire::TemperatureFanTarget) -> bool {
    match value {
        wire::TemperatureFanTarget::On | wire::TemperatureFanTarget::Auto => true,
        wire::TemperatureFanTarget::Unspecified => false,
    }
}
fn temperature(value: &wire::NumberBounds) -> Option<Bounds> {
    Bounds::parse(value).filter(|bounds| bounds.minimum() >= -273.15)
}
fn optional<T, U>(value: Option<&T>, parse: impl Fn(&T) -> Option<U>) -> Option<Option<U>> {
    match value {
        Some(value) => parse(value).map(Some),
        None => Some(None),
    }
}
#[derive(Clone, Copy, Debug, PartialEq)]
pub(super) struct Range {
    low: Bounds,
    high: Bounds,
    minimum_delta: Option<f64>,
    maximum_delta: Option<f64>,
}
impl Range {
    pub fn bounds(self) -> (Bounds, Bounds) {
        (self.low, self.high)
    }
    fn deltas(value: &wire::TemperatureRangeBounds) -> bool {
        value.__buffa_unknown_fields.is_empty()
            && value
                .minimum_delta_celsius
                .is_none_or(|value| value.is_finite() && value >= 0.)
            && value
                .maximum_delta_celsius
                .is_none_or(|value| value.is_finite() && value >= 0.)
            && !matches!((value.minimum_delta_celsius,value.maximum_delta_celsius),(Some(minimum),Some(maximum)) if minimum>maximum)
    }
    fn parse(value: &wire::TemperatureRangeBounds) -> Option<Self> {
        if !Self::deltas(value) {
            return None;
        }
        let low = temperature(value.low_celsius.as_option()?)?;
        let high = temperature(value.high_celsius.as_option()?)?;
        if high.maximum() < low.minimum()
            || value
                .minimum_delta_celsius
                .is_some_and(|delta| high.maximum() - low.minimum() < delta)
            || value
                .maximum_delta_celsius
                .is_some_and(|delta| high.minimum() - low.maximum() > delta)
        {
            return None;
        }
        Some(Self {
            low,
            high,
            minimum_delta: value.minimum_delta_celsius,
            maximum_delta: value.maximum_delta_celsius,
        })
    }
    pub fn admit(self, low: NumericInput, high: NumericInput) -> Result<(f64, f64), Refusal> {
        let low = low.admit(self.low)?;
        let high = high.admit(self.high)?;
        let delta = high - low;
        if low > high
            || self.minimum_delta.is_some_and(|minimum| delta < minimum)
            || self.maximum_delta.is_some_and(|maximum| delta > maximum)
        {
            return Err(Refusal::InvalidValue);
        }
        Ok((low, high))
    }
    fn observation(value: &wire::TemperatureRangeBounds) -> Option<()> {
        if !Self::deltas(value) {
            return None;
        }
        let low = optional(value.low_celsius.as_option(), temperature)?;
        let high = optional(value.high_celsius.as_option(), temperature)?;
        if let (Some(low), Some(high)) = (low, high)
            && (high.maximum() < low.minimum()
                || value
                    .minimum_delta_celsius
                    .is_some_and(|delta| high.maximum() - low.minimum() < delta)
                || value
                    .maximum_delta_celsius
                    .is_some_and(|delta| high.minimum() - low.maximum() > delta))
        {
            return None;
        }
        Some(())
    }
}
#[derive(Clone, Debug, PartialEq)]
pub(super) struct Observes {
    pub temperature: bool,
    pub setpoint: bool,
    pub range: bool,
    pub modes: Vec<wire::TemperatureMode>,
    pub activities: Vec<wire::TemperatureActivity>,
    pub humidity: bool,
    pub fan_activity: bool,
    pub fan_targets: Vec<wire::TemperatureFanTarget>,
}
#[derive(Clone, Debug, PartialEq)]
pub(super) struct Climate {
    setpoint: Option<Bounds>,
    range: Option<Range>,
    modes: Vec<wire::TemperatureMode>,
    fan_targets: Vec<wire::TemperatureFanTarget>,
    observes: Observes,
}
#[derive(Clone, Debug)]
pub(crate) enum ClimateIntent {
    Setpoint(NumericInput),
    Range(NumericInput, NumericInput),
    Mode(wire::TemperatureMode),
    FanTarget(wire::TemperatureFanTarget),
}
impl Climate {
    pub fn restricted(&self, temperature: bool, modes: bool) -> Self {
        Self {
            setpoint: self.setpoint.filter(|_| temperature),
            range: self.range.filter(|_| temperature),
            modes: if modes {
                self.modes.clone()
            } else {
                Vec::new()
            },
            fan_targets: Vec::new(),
            observes: self.observes.clone(),
        }
    }
    pub fn setpoint(&self) -> Option<Bounds> {
        self.setpoint
    }
    pub fn range(&self) -> Option<Range> {
        self.range
    }
    pub fn modes(&self) -> &[wire::TemperatureMode] {
        &self.modes
    }
    pub fn fan_targets(&self) -> &[wire::TemperatureFanTarget] {
        &self.fan_targets
    }
    pub fn observes(&self) -> &Observes {
        &self.observes
    }
    pub fn parse(value: &wire::TemperatureControlCapability) -> Option<Self> {
        if !value.__buffa_unknown_fields.is_empty() {
            return None;
        }
        let observation = |observation: &wire::TemperatureObservationDefinition| {
            if !observation.__buffa_unknown_fields.is_empty() {
                return None;
            }
            optional(observation.bounds.as_option(), temperature).map(|_| ())
        };
        let setpoint = optional(value.observed_setpoint_celsius.as_option(), observation)?;
        let range = optional(
            value.observed_setpoint_range_celsius.as_option(),
            Range::observation,
        )?;
        Some(Self {
            setpoint: optional(value.setpoint_celsius.as_option(), temperature)?,
            range: optional(value.setpoint_range_celsius.as_option(), Range::parse)?,
            modes: options(&value.mode_options, mode, 6)?,
            fan_targets: options(&value.fan_target_options, fan_target, 2)?,
            observes: Observes {
                temperature: false, // NumericMeasurement is a separate capability.
                setpoint: setpoint.is_some(),
                range: range.is_some(),
                humidity: false, // NumericMeasurement is a separate capability.
                fan_activity: value.observes_fan_activity,
                modes: options(&value.observed_mode_options, mode, 6)?,
                activities: options(&value.observed_activity_options, activity, 5)?,
                fan_targets: options(&value.observed_fan_target_options, fan_target, 2)?,
            },
        })
    }
    pub fn arguments(&self, intent: ClimateIntent) -> Result<model::ThermalIntent, Refusal> {
        let (target, mode, fan) = match intent {
            ClimateIntent::Setpoint(input) => {
                let value = input.admit(self.setpoint.ok_or(Refusal::Unsupported)?)?;
                (
                    Some(model::TemperatureTarget::Single(
                        model::Celsius::try_from(value).map_err(|_| Refusal::InvalidValue)?,
                    )),
                    None,
                    None,
                )
            }
            ClimateIntent::Range(low, high) => {
                let (low, high) = self.range.ok_or(Refusal::Unsupported)?.admit(low, high)?;
                let low = model::Celsius::try_from(low).map_err(|_| Refusal::InvalidValue)?;
                let high = model::Celsius::try_from(high).map_err(|_| Refusal::InvalidValue)?;
                (
                    Some(model::TemperatureTarget::Range(
                        model::TemperatureRange::new(low, high)
                            .map_err(|_| Refusal::InvalidValue)?,
                    )),
                    None,
                    None,
                )
            }
            ClimateIntent::Mode(mode) => {
                if !self.modes.contains(&mode) {
                    return Err(Refusal::Unsupported);
                }
                let mode = match mode {
                    wire::TemperatureMode::Off => model::TemperatureMode::Off,
                    wire::TemperatureMode::Heat => model::TemperatureMode::Heat,
                    wire::TemperatureMode::Cool => model::TemperatureMode::Cool,
                    wire::TemperatureMode::Auto => model::TemperatureMode::Auto,
                    wire::TemperatureMode::Dry => model::TemperatureMode::Dry,
                    wire::TemperatureMode::FanOnly => model::TemperatureMode::FanOnly,
                    wire::TemperatureMode::Unspecified => return Err(Refusal::Unsupported),
                };
                (None, Some(mode), None)
            }
            ClimateIntent::FanTarget(target) => {
                if !self.fan_targets.contains(&target) {
                    return Err(Refusal::Unsupported);
                }
                let target = match target {
                    wire::TemperatureFanTarget::On => model::TemperatureFanTarget::On,
                    wire::TemperatureFanTarget::Auto => model::TemperatureFanTarget::Auto,
                    wire::TemperatureFanTarget::Unspecified => return Err(Refusal::Unsupported),
                };
                (None, None, Some(target))
            }
        };
        model::ThermalIntent::new(target, mode, fan).map_err(|_| Refusal::InvalidValue)
    }
}
#[derive(Clone, Debug, PartialEq)]
pub(super) struct Fan {
    power: bool,
    speeds: Vec<wire::FanSpeed>,
}
impl Fan {
    pub fn restricted(&self, power: bool, speed: bool) -> Self {
        Self {
            power: self.power && power,
            speeds: if speed {
                self.speeds.clone()
            } else {
                Vec::new()
            },
        }
    }
    pub fn can_power(&self) -> bool {
        self.power
    }
    pub fn speeds(&self) -> &[wire::FanSpeed] {
        &self.speeds
    }
    fn speed_name(speed: wire::FanSpeed) -> Option<&'static str> {
        match speed {
            wire::FanSpeed::Off => Some("Off"),
            wire::FanSpeed::Low => Some("Low"),
            wire::FanSpeed::Medium => Some("Medium"),
            wire::FanSpeed::MediumHigh => Some("MediumHigh"),
            wire::FanSpeed::High => Some("High"),
            wire::FanSpeed::Unspecified => None,
        }
    }
    pub fn parse(value: &wire::FanControlCapability) -> Option<Self> {
        let options = value
            .speed
            .as_option()
            .map(|speed| speed.options.as_slice())
            .unwrap_or(&[]);
        if !value.__buffa_unknown_fields.is_empty()
            || value
                .speed
                .as_option()
                .is_some_and(|speed| !speed.__buffa_unknown_fields.is_empty())
            || options.len() > 5
        {
            return None;
        }
        let mut speeds = Vec::with_capacity(options.len());
        for value in options {
            let speed = <wire::FanSpeed as buffa::Enumeration>::values()
                .iter()
                .copied()
                .find(|speed| Self::speed_name(*speed) == Some(value.as_str()))?;
            if speeds.contains(&speed) {
                return None;
            }
            speeds.push(speed);
        }
        Some(Self {
            power: value.supports_power,
            speeds,
        })
    }
    pub fn power(&self, value: Power) -> Result<model::FanIntent, Refusal> {
        if !self.power {
            return Err(Refusal::Unsupported);
        }
        model::FanIntent::new(Some(value.on()), None).map_err(|_| Refusal::InvalidValue)
    }
    pub fn speed(&self, speed: wire::FanSpeed) -> Result<model::FanIntent, Refusal> {
        if !self.speeds.contains(&speed) {
            return Err(Refusal::Unsupported);
        }
        let name = Self::speed_name(speed).ok_or(Refusal::Unsupported)?;
        model::FanIntent::new(
            None,
            Some(model::SelectorKey::try_from(name).map_err(|_| Refusal::InvalidValue)?),
        )
        .map_err(|_| Refusal::InvalidValue)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa::Message;
    fn limits() -> wire::NumberBounds {
        wire::NumberBounds {
            minimum: -5.,
            maximum: 30.,
            step: Some(0.5),
            ..Default::default()
        }
    }
    fn text(value: &str) -> NumericInput {
        NumericInput::Text(value.into())
    }
    #[test]
    fn observation_advertisements_never_create_command_capabilities() {
        let control = Climate::parse(&wire::TemperatureControlCapability {
            observed_setpoint_celsius: wire::TemperatureObservationDefinition::default().into(),
            observed_mode_options: vec![wire::TemperatureMode::Heat.into()],
            observed_activity_options: vec![wire::TemperatureActivity::Heating.into()],
            observed_fan_target_options: vec![wire::TemperatureFanTarget::Auto.into()],
            observes_fan_activity: true,
            ..Default::default()
        })
        .unwrap();
        assert!(
            !control.observes.temperature
                && control.observes.setpoint
                && control.observes.fan_activity
        );
        assert!(!control.observes.range && !control.observes.humidity);
        assert_eq!(control.observes.modes, [wire::TemperatureMode::Heat]);
        assert_eq!(
            control.observes.activities,
            [wire::TemperatureActivity::Heating]
        );
        assert_eq!(
            control.observes.fan_targets,
            [wire::TemperatureFanTarget::Auto]
        );
        assert!(matches!(
            control.arguments(ClimateIntent::Setpoint(text("0"))),
            Err(Refusal::Unsupported)
        ));
        assert!(matches!(
            control.arguments(ClimateIntent::Mode(wire::TemperatureMode::Heat)),
            Err(Refusal::Unsupported)
        ));
        assert!(matches!(
            control.arguments(ClimateIntent::FanTarget(wire::TemperatureFanTarget::Auto)),
            Err(Refusal::Unsupported)
        ));
    }
    #[test]
    fn zero_negative_decimal_comma_and_renderer_rounding_keep_exact_server_bounds() {
        let control = Climate::parse(&wire::TemperatureControlCapability {
            setpoint_celsius: limits().into(),
            ..Default::default()
        })
        .unwrap();
        assert_eq!(
            control
                .arguments(ClimateIntent::Setpoint(text("0")))
                .unwrap()
                .target(),
            Some(model::TemperatureTarget::Single(
                model::Celsius::try_from(0.).unwrap()
            ))
        );
        assert_eq!(
            control
                .arguments(ClimateIntent::Setpoint(text("-0,5")))
                .unwrap()
                .target(),
            Some(model::TemperatureTarget::Single(
                model::Celsius::try_from(-0.5).unwrap()
            ))
        );
        assert_eq!(
            control
                .arguments(ClimateIntent::Setpoint(NumericInput::Renderer(f64::from(
                    20.499998_f32
                ))))
                .unwrap()
                .target(),
            Some(model::TemperatureTarget::Single(
                model::Celsius::try_from(20.5).unwrap()
            ))
        );
        for value in ["-", "NaN", "inf", "30.5", "-5.5", "20.25", "1,000.5"] {
            assert!(
                matches!(
                    control.arguments(ClimateIntent::Setpoint(text(value))),
                    Err(Refusal::InvalidValue)
                ),
                "{value}"
            );
        }
    }
    #[test]
    fn ranges_submit_both_validated_endpoints_and_never_invent_a_single_setpoint() {
        let control = Climate::parse(&wire::TemperatureControlCapability {
            setpoint_range_celsius: wire::TemperatureRangeBounds {
                low_celsius: limits().into(),
                high_celsius: limits().into(),
                minimum_delta_celsius: Some(2.),
                maximum_delta_celsius: Some(10.),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })
        .unwrap();
        let arguments = control
            .arguments(ClimateIntent::Range(text("0"), text("5")))
            .unwrap();
        let Some(model::TemperatureTarget::Range(range)) = arguments.target() else {
            panic!("atomic range")
        };
        assert_eq!(range.low().value(), 0.);
        assert_eq!(range.high().value(), 5.);
        assert!(arguments.mode().is_none() && arguments.fan().is_none());
        for (low, high) in [("0", "1"), ("0", "15"), ("5", "0"), ("0", "5.25")] {
            assert!(matches!(
                control.arguments(ClimateIntent::Range(text(low), text(high))),
                Err(Refusal::InvalidValue)
            ));
        }
        assert!(matches!(
            control.arguments(ClimateIntent::Setpoint(text("5"))),
            Err(Refusal::Unsupported)
        ));
    }
    #[test]
    fn requested_mode_and_fan_target_are_independent_and_instance_limited() {
        let control = Climate::parse(&wire::TemperatureControlCapability {
            mode_options: vec![
                wire::TemperatureMode::Heat.into(),
                wire::TemperatureMode::Off.into(),
            ],
            fan_target_options: vec![
                wire::TemperatureFanTarget::On.into(),
                wire::TemperatureFanTarget::Auto.into(),
            ],
            ..Default::default()
        })
        .unwrap();
        let mode = control
            .arguments(ClimateIntent::Mode(wire::TemperatureMode::Heat))
            .unwrap();
        assert_eq!(mode.mode(), Some(model::TemperatureMode::Heat));
        assert!(mode.fan().is_none());
        let fan = control
            .arguments(ClimateIntent::FanTarget(wire::TemperatureFanTarget::Auto))
            .unwrap();
        assert_eq!(fan.fan(), Some(model::TemperatureFanTarget::Auto));
        assert!(fan.mode().is_none());
        assert!(matches!(
            control.arguments(ClimateIntent::Mode(wire::TemperatureMode::Cool)),
            Err(Refusal::Unsupported)
        ));
        assert!(matches!(
            control.arguments(ClimateIntent::Mode(wire::TemperatureMode::Unspecified)),
            Err(Refusal::Unsupported)
        ));
    }
    #[test]
    fn future_duplicates_unknown_nested_fields_and_impossible_ranges_are_inert() {
        assert!(
            Climate::parse(&wire::TemperatureControlCapability {
                mode_options: vec![buffa::EnumValue::Unknown(99)],
                ..Default::default()
            })
            .is_none()
        );
        assert!(
            Climate::parse(&wire::TemperatureControlCapability {
                mode_options: vec![
                    wire::TemperatureMode::Heat.into(),
                    wire::TemperatureMode::Heat.into()
                ],
                ..Default::default()
            })
            .is_none()
        );
        let unknown = wire::NumberBounds::decode_from_slice(&[0xa0, 0x06, 0x01]).unwrap();
        assert!(
            Climate::parse(&wire::TemperatureControlCapability {
                setpoint_celsius: unknown.into(),
                ..Default::default()
            })
            .is_none()
        );
        assert!(
            Climate::parse(&wire::TemperatureControlCapability {
                setpoint_range_celsius: wire::TemperatureRangeBounds {
                    low_celsius: limits().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })
            .is_none()
        );
        assert!(
            Climate::parse(&wire::TemperatureControlCapability {
                setpoint_range_celsius: wire::TemperatureRangeBounds {
                    low_celsius: limits().into(),
                    high_celsius: limits().into(),
                    minimum_delta_celsius: Some(40.),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })
            .is_none()
        );
    }
    #[test]
    fn fan_power_and_speed_are_distinct_and_unknown_speed_names_never_fall_back() {
        let control = Fan::parse(&wire::FanControlCapability {
            supports_power: true,
            speed: wire::SelectorDefinition {
                options: vec!["Low".into(), "High".into()],
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })
        .unwrap();
        let power = control.power(Power::On).unwrap();
        assert_eq!(power.on(), Some(true));
        assert!(power.speed().is_none());
        let speed = control.speed(wire::FanSpeed::High).unwrap();
        assert_eq!(speed.speed().map(|value| value.as_str()), Some("High"));
        assert!(speed.on().is_none());
        assert!(matches!(
            control.speed(wire::FanSpeed::Off),
            Err(Refusal::Unsupported)
        ));
        assert!(matches!(
            control.speed(wire::FanSpeed::Unspecified),
            Err(Refusal::Unsupported)
        ));
        for values in [
            vec!["Turbo".into()],
            vec!["Low".into(), "Low".into()],
            vec![" Low".into()],
        ] {
            assert!(
                Fan::parse(&wire::FanControlCapability {
                    speed: wire::SelectorDefinition {
                        options: values,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                })
                .is_none()
            );
        }
        assert!(matches!(
            Fan::parse(&wire::FanControlCapability::default())
                .unwrap()
                .power(Power::On),
            Err(Refusal::Unsupported)
        ));
    }
}
