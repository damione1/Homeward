//! Immutable instance constraints and typed climate/fan command construction.
use super::{
    Power, Refusal,
    number::{Bounds, NumericInput},
};
use home_wire_contract::home::{actions::v1 as action, registry::v1 as wire, state::v1 as state};

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
fn mode(value: wire::ClimateMode) -> bool {
    match value {
        wire::ClimateMode::Off
        | wire::ClimateMode::Heat
        | wire::ClimateMode::Cool
        | wire::ClimateMode::Auto
        | wire::ClimateMode::Dry
        | wire::ClimateMode::FanOnly => true,
        wire::ClimateMode::Unspecified => false,
    }
}
fn activity(value: wire::ClimateActivity) -> bool {
    match value {
        wire::ClimateActivity::Idle
        | wire::ClimateActivity::Heating
        | wire::ClimateActivity::Cooling
        | wire::ClimateActivity::Drying
        | wire::ClimateActivity::FanOnly => true,
        wire::ClimateActivity::Unspecified => false,
    }
}
fn fan_target(value: wire::ClimateFanTarget) -> bool {
    match value {
        wire::ClimateFanTarget::On | wire::ClimateFanTarget::Auto => true,
        wire::ClimateFanTarget::Unspecified => false,
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
    fn deltas(value: &wire::ClimateSetpointRangeConstraints) -> bool {
        value.__buffa_unknown_fields.is_empty()
            && value
                .minimum_delta_celsius
                .is_none_or(|value| value.is_finite() && value >= 0.)
            && value
                .maximum_delta_celsius
                .is_none_or(|value| value.is_finite() && value >= 0.)
            && !matches!((value.minimum_delta_celsius,value.maximum_delta_celsius),(Some(minimum),Some(maximum)) if minimum>maximum)
    }
    fn parse(value: &wire::ClimateSetpointRangeConstraints) -> Option<Self> {
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
    fn observation(value: &wire::ClimateSetpointRangeConstraints) -> Option<()> {
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
    pub modes: Vec<wire::ClimateMode>,
    pub activities: Vec<wire::ClimateActivity>,
    pub humidity: bool,
    pub fan_activity: bool,
    pub fan_targets: Vec<wire::ClimateFanTarget>,
}
#[derive(Clone, Debug, PartialEq)]
pub(super) struct Climate {
    setpoint: Option<Bounds>,
    range: Option<Range>,
    modes: Vec<wire::ClimateMode>,
    fan_targets: Vec<wire::ClimateFanTarget>,
    observes: Observes,
}
#[derive(Clone, Debug)]
pub(crate) enum ClimateIntent {
    Setpoint(NumericInput),
    Range(NumericInput, NumericInput),
    Mode(wire::ClimateMode),
    FanTarget(wire::ClimateFanTarget),
}
impl Climate {
    pub fn setpoint(&self) -> Option<Bounds> {
        self.setpoint
    }
    pub fn range(&self) -> Option<Range> {
        self.range
    }
    pub fn modes(&self) -> &[wire::ClimateMode] {
        &self.modes
    }
    pub fn fan_targets(&self) -> &[wire::ClimateFanTarget] {
        &self.fan_targets
    }
    pub fn observes(&self) -> &Observes {
        &self.observes
    }
    pub fn parse(value: &wire::ClimateConstraints) -> Option<Self> {
        if !value.__buffa_unknown_fields.is_empty() {
            return None;
        }
        let observation = |observation: &wire::ClimateTemperatureObservation| {
            if !observation.__buffa_unknown_fields.is_empty() {
                return None;
            }
            optional(observation.bounds.as_option(), temperature).map(|_| ())
        };
        let current = optional(value.observed_temperature_celsius.as_option(), observation)?;
        let setpoint = optional(value.observed_setpoint_celsius.as_option(), observation)?;
        let range = optional(
            value.observed_setpoint_range_celsius.as_option(),
            Range::observation,
        )?;
        let humidity = optional(value.observed_humidity_percent.as_option(), |value| {
            Bounds::parse(value).filter(|bounds| bounds.minimum() >= 0. && bounds.maximum() <= 100.)
        })?;
        Some(Self {
            setpoint: optional(value.setpoint_celsius.as_option(), temperature)?,
            range: optional(value.setpoint_range_celsius.as_option(), Range::parse)?,
            modes: options(&value.mode_options, mode, 6)?,
            fan_targets: options(&value.fan_target_options, fan_target, 2)?,
            observes: Observes {
                temperature: current.is_some(),
                setpoint: setpoint.is_some(),
                range: range.is_some(),
                humidity: humidity.is_some(),
                fan_activity: value.observes_fan_activity,
                modes: options(&value.observed_mode_options, mode, 6)?,
                activities: options(&value.activity_options, activity, 5)?,
                fan_targets: options(&value.observed_fan_target_options, fan_target, 2)?,
            },
        })
    }
    pub fn arguments(&self, intent: ClimateIntent) -> Result<action::SetClimate, Refusal> {
        let mut arguments = action::SetClimate::default();
        match intent {
            ClimateIntent::Setpoint(input) => {
                arguments.setpoint_celsius =
                    Some(input.admit(self.setpoint.ok_or(Refusal::Unsupported)?)?)
            }
            ClimateIntent::Range(low, high) => {
                let (low, high) = self.range.ok_or(Refusal::Unsupported)?.admit(low, high)?;
                arguments.heat_threshold_celsius = Some(low);
                arguments.cool_threshold_celsius = Some(high);
            }
            ClimateIntent::Mode(mode) => {
                if !self.modes.contains(&mode) {
                    return Err(Refusal::Unsupported);
                }
                arguments.mode = Some(mode.into());
            }
            ClimateIntent::FanTarget(target) => {
                if !self.fan_targets.contains(&target) {
                    return Err(Refusal::Unsupported);
                }
                arguments.fan_target = Some(target.into());
            }
        }
        Ok(arguments)
    }
}
#[derive(Clone, Debug, PartialEq)]
pub(super) struct Fan {
    power: bool,
    speeds: Vec<state::FanSpeed>,
}
impl Fan {
    pub fn can_power(&self) -> bool {
        self.power
    }
    pub fn speeds(&self) -> &[state::FanSpeed] {
        &self.speeds
    }
    fn speed_name(speed: state::FanSpeed) -> Option<&'static str> {
        match speed {
            state::FanSpeed::Off => Some("Off"),
            state::FanSpeed::Low => Some("Low"),
            state::FanSpeed::Medium => Some("Medium"),
            state::FanSpeed::MediumHigh => Some("MediumHigh"),
            state::FanSpeed::High => Some("High"),
            state::FanSpeed::Unspecified => None,
        }
    }
    pub fn parse(value: &wire::FanConstraints) -> Option<Self> {
        if !value.__buffa_unknown_fields.is_empty() || value.speed_options.len() > 5 {
            return None;
        }
        let mut speeds = Vec::with_capacity(value.speed_options.len());
        for value in &value.speed_options {
            let speed = <state::FanSpeed as buffa::Enumeration>::values()
                .iter()
                .copied()
                .find(|speed| Self::speed_name(*speed) == Some(value.as_str()))?;
            if speeds.contains(&speed) {
                return None;
            }
            speeds.push(speed);
        }
        Some(Self {
            power: value.set_on,
            speeds,
        })
    }
    pub fn power(&self, value: Power) -> Result<action::SetFan, Refusal> {
        if !self.power {
            return Err(Refusal::Unsupported);
        }
        Ok(action::SetFan {
            on: Some(value.on()),
            ..Default::default()
        })
    }
    pub fn speed(&self, speed: state::FanSpeed) -> Result<action::SetFan, Refusal> {
        if !self.speeds.contains(&speed) {
            return Err(Refusal::Unsupported);
        }
        let name = Self::speed_name(speed).ok_or(Refusal::Unsupported)?;
        Ok(action::SetFan {
            speed: Some(name.into()),
            ..Default::default()
        })
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
        let control = Climate::parse(&wire::ClimateConstraints {
            observed_temperature_celsius: wire::ClimateTemperatureObservation::default().into(),
            observed_setpoint_celsius: wire::ClimateTemperatureObservation::default().into(),
            observed_mode_options: vec![wire::ClimateMode::Heat.into()],
            activity_options: vec![wire::ClimateActivity::Heating.into()],
            observed_fan_target_options: vec![wire::ClimateFanTarget::Auto.into()],
            observes_fan_activity: true,
            ..Default::default()
        })
        .unwrap();
        assert!(
            control.observes.temperature
                && control.observes.setpoint
                && control.observes.fan_activity
        );
        assert!(!control.observes.range && !control.observes.humidity);
        assert_eq!(control.observes.modes, [wire::ClimateMode::Heat]);
        assert_eq!(
            control.observes.activities,
            [wire::ClimateActivity::Heating]
        );
        assert_eq!(control.observes.fan_targets, [wire::ClimateFanTarget::Auto]);
        assert!(matches!(
            control.arguments(ClimateIntent::Setpoint(text("0"))),
            Err(Refusal::Unsupported)
        ));
        assert!(matches!(
            control.arguments(ClimateIntent::Mode(wire::ClimateMode::Heat)),
            Err(Refusal::Unsupported)
        ));
        assert!(matches!(
            control.arguments(ClimateIntent::FanTarget(wire::ClimateFanTarget::Auto)),
            Err(Refusal::Unsupported)
        ));
    }
    #[test]
    fn zero_negative_decimal_comma_and_renderer_rounding_keep_exact_server_bounds() {
        let control = Climate::parse(&wire::ClimateConstraints {
            setpoint_celsius: limits().into(),
            ..Default::default()
        })
        .unwrap();
        assert_eq!(
            control
                .arguments(ClimateIntent::Setpoint(text("0")))
                .unwrap()
                .setpoint_celsius,
            Some(0.)
        );
        assert_eq!(
            control
                .arguments(ClimateIntent::Setpoint(text("-0,5")))
                .unwrap()
                .setpoint_celsius,
            Some(-0.5)
        );
        assert_eq!(
            control
                .arguments(ClimateIntent::Setpoint(NumericInput::Renderer(f64::from(
                    20.499998_f32
                ))))
                .unwrap()
                .setpoint_celsius,
            Some(20.5)
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
        let control = Climate::parse(&wire::ClimateConstraints {
            setpoint_range_celsius: wire::ClimateSetpointRangeConstraints {
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
        assert_eq!(arguments.heat_threshold_celsius, Some(0.));
        assert_eq!(arguments.cool_threshold_celsius, Some(5.));
        assert_eq!(arguments.setpoint_celsius, None);
        assert!(arguments.mode.is_none() && arguments.fan_target.is_none());
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
        let control = Climate::parse(&wire::ClimateConstraints {
            mode_options: vec![
                wire::ClimateMode::Heat.into(),
                wire::ClimateMode::Off.into(),
            ],
            fan_target_options: vec![
                wire::ClimateFanTarget::On.into(),
                wire::ClimateFanTarget::Auto.into(),
            ],
            ..Default::default()
        })
        .unwrap();
        let mode = control
            .arguments(ClimateIntent::Mode(wire::ClimateMode::Heat))
            .unwrap();
        assert_eq!(mode.mode.unwrap().as_known(), Some(wire::ClimateMode::Heat));
        assert!(mode.fan_target.is_none());
        let fan = control
            .arguments(ClimateIntent::FanTarget(wire::ClimateFanTarget::Auto))
            .unwrap();
        assert_eq!(
            fan.fan_target.unwrap().as_known(),
            Some(wire::ClimateFanTarget::Auto)
        );
        assert!(fan.mode.is_none());
        assert!(matches!(
            control.arguments(ClimateIntent::Mode(wire::ClimateMode::Cool)),
            Err(Refusal::Unsupported)
        ));
        assert!(matches!(
            control.arguments(ClimateIntent::Mode(wire::ClimateMode::Unspecified)),
            Err(Refusal::Unsupported)
        ));
    }
    #[test]
    fn future_duplicates_unknown_nested_fields_and_impossible_ranges_are_inert() {
        assert!(
            Climate::parse(&wire::ClimateConstraints {
                mode_options: vec![buffa::EnumValue::Unknown(99)],
                ..Default::default()
            })
            .is_none()
        );
        assert!(
            Climate::parse(&wire::ClimateConstraints {
                mode_options: vec![
                    wire::ClimateMode::Heat.into(),
                    wire::ClimateMode::Heat.into()
                ],
                ..Default::default()
            })
            .is_none()
        );
        let unknown = wire::NumberBounds::decode_from_slice(&[0xa0, 0x06, 0x01]).unwrap();
        assert!(
            Climate::parse(&wire::ClimateConstraints {
                setpoint_celsius: unknown.into(),
                ..Default::default()
            })
            .is_none()
        );
        assert!(
            Climate::parse(&wire::ClimateConstraints {
                setpoint_range_celsius: wire::ClimateSetpointRangeConstraints {
                    low_celsius: limits().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })
            .is_none()
        );
        assert!(
            Climate::parse(&wire::ClimateConstraints {
                setpoint_range_celsius: wire::ClimateSetpointRangeConstraints {
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
        let control = Fan::parse(&wire::FanConstraints {
            set_on: true,
            speed_options: vec!["Low".into(), "High".into()],
            ..Default::default()
        })
        .unwrap();
        let power = control.power(Power::On).unwrap();
        assert_eq!(power.on, Some(true));
        assert!(power.speed.is_none());
        let speed = control.speed(state::FanSpeed::High).unwrap();
        assert_eq!(speed.speed.as_deref(), Some("High"));
        assert!(speed.on.is_none());
        assert!(matches!(
            control.speed(state::FanSpeed::Off),
            Err(Refusal::Unsupported)
        ));
        assert!(matches!(
            control.speed(state::FanSpeed::Unspecified),
            Err(Refusal::Unsupported)
        ));
        for values in [
            vec!["Turbo".into()],
            vec!["Low".into(), "Low".into()],
            vec![" Low".into()],
        ] {
            assert!(
                Fan::parse(&wire::FanConstraints {
                    speed_options: values,
                    ..Default::default()
                })
                .is_none()
            );
        }
        assert!(matches!(
            Fan::parse(&wire::FanConstraints::default())
                .unwrap()
                .power(Power::On),
            Err(Refusal::Unsupported)
        ));
    }
}
