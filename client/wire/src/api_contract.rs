//! API v2 boundary: decode-compatible future input is not executable intent.
//! Authorization and live admission remain server responsibilities.
use crate::home::api::v2 as wire;
use buffa::EnumValue;
use home_client_model::api::{
    self as domain, CapabilityName, Command, Deadline, Intent, Percentage, Power, RequestId,
};
use home_client_model::api::{
    AlarmOperation, Celsius, ClockTime, ColorTarget, ColorTemperature, CoverIntent, CoverOperation,
    FanIntent, Finite, HelperIntent, HelperOperation, HelperText, HelperValue, LevelTarget,
    LightIntent, LocalDate, LockOperation, Rgb8, SelectorKey, TemperatureFanTarget,
    TemperatureMode, TemperatureRange, TemperatureTarget, ThermalIntent, Transition,
};

/// Generated boundary error vocabulary; not an independent list of reasons.
pub type ViolationKind = wire::ValidationReason;
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Violation {
    pub field: &'static str,
    pub kind: ViolationKind,
}
fn reject(field: &'static str, kind: ViolationKind) -> Violation {
    Violation { field, kind }
}

/// Explicit generated-enum match: adding a wire variant breaks compilation.
pub fn parse_power(value: EnumValue<wire::PowerValue>) -> Result<Power, Violation> {
    match value {
        EnumValue::Known(wire::PowerValue::On) => Ok(Power::On),
        EnumValue::Known(wire::PowerValue::Off) => Ok(Power::Off),
        EnumValue::Known(wire::PowerValue::Unspecified) => {
            Err(reject("power.value", ViolationKind::MissingIntent))
        }
        EnumValue::Unknown(_) => Err(reject("power.value", ViolationKind::UnknownValue)),
    }
}

/// Pure conversion; never sends, refreshes, retries or chooses another trait.
pub fn parse_command(request: &wire::ExecuteCapabilityRequest) -> Result<Command, Violation> {
    use wire::{execute_capability_request::Command as WireCommand, set_level_command::Target};
    let unknown_intent = command_unknown(&request.command);
    if !request.__buffa_unknown_fields.is_empty()
        || unknown_intent
        || request
            .expire_time
            .as_option()
            .is_some_and(|value| !value.__buffa_unknown_fields.is_empty())
    {
        return Err(reject("", ViolationKind::UnknownField));
    }
    let target = CapabilityName::try_from(request.name.as_str())
        .map_err(|_| reject("name", ViolationKind::InvalidName))?;
    let intent = match &request.command {
        Some(WireCommand::Power(value)) => Intent::Power(parse_power(value.value)?),
        Some(WireCommand::Level(value)) => match value.target {
            Some(Target::Percentage(percentage)) => Intent::Level(
                Percentage::try_from(percentage)
                    .map_err(|_| reject("level.percentage", ViolationKind::OutOfRange))?,
            ),
            None => return Err(reject("level", ViolationKind::MissingIntent)),
        },
        Some(WireCommand::Media(v)) => {
            Intent::Media(crate::api_media_contract::parse_media_command(v)?)
        }
        Some(WireCommand::Light(v)) => Intent::Light(parse_light(v)?),
        Some(WireCommand::Fan(v)) => Intent::Fan(parse_fan(v)?),
        Some(WireCommand::TemperatureControl(v)) => Intent::Thermal(parse_thermal(v)?),
        Some(WireCommand::Cover(v)) => Intent::Cover(parse_cover(v)?),
        Some(WireCommand::Lock(v)) => Intent::Lock(parse_lock(v)?),
        Some(WireCommand::Alarm(v)) => Intent::Alarm(parse_alarm(v)?),
        Some(WireCommand::Helper(v)) => Intent::Helper(parse_helper(v)?),
        None => return Err(reject("", ViolationKind::MissingIntent)),
    };
    let request_id = if request.request_id.is_empty() {
        None
    } else {
        Some(
            RequestId::try_from(request.request_id.as_str())
                .map_err(|_| reject("request_id", ViolationKind::InvalidRequestId))?,
        )
    };
    let expiry = request
        .expire_time
        .as_option()
        .ok_or_else(|| reject("expire_time", ViolationKind::InvalidDeadline))?;
    let deadline = Deadline::new(expiry.seconds, expiry.nanos)
        .map_err(|_| reject("expire_time", ViolationKind::InvalidDeadline))?;
    let command = Command::new(target, intent, request_id, deadline);
    if request.etag.is_empty() {
        Ok(command)
    } else {
        Ok(command.with_definition_etag(
            domain::DefinitionEtag::try_from(request.etag.as_str())
                .map_err(|_| reject("etag", ViolationKind::OutOfRange))?,
        ))
    }
}

/// Exhaustive reverse conversion for the two admitted intentions, preserving
/// explicit OFF/zero and the caller's optional deduplication key.
pub fn encode_command(value: &Command) -> wire::ExecuteCapabilityRequest {
    use wire::{execute_capability_request::Command as W, set_level_command::Target};
    let command = match value.intent() {
        Intent::Power(power) => W::Power(Box::new(wire::SetPowerCommand {
            value: match power {
                Power::On => wire::PowerValue::On,
                Power::Off => wire::PowerValue::Off,
            }
            .into(),
            ..Default::default()
        })),
        Intent::Level(percentage) => W::Level(Box::new(wire::SetLevelCommand {
            target: Some(Target::Percentage(percentage.value())),
            ..Default::default()
        })),
        Intent::Media(v) => W::Media(Box::new(crate::api_media_contract::encode_media_command(
            &v,
        ))),
        Intent::Light(v) => W::Light(Box::new(encode_light(v))),
        Intent::Fan(v) => W::Fan(Box::new(encode_fan(&v))),
        Intent::Thermal(v) => W::TemperatureControl(Box::new(encode_thermal(v))),
        Intent::Cover(v) => W::Cover(Box::new(encode_cover(v))),
        Intent::Lock(v) => W::Lock(Box::new(encode_lock(v))),
        Intent::Alarm(v) => W::Alarm(Box::new(encode_alarm(v))),
        Intent::Helper(v) => W::Helper(Box::new(encode_helper(&v))),
    };
    wire::ExecuteCapabilityRequest {
        name: value.target().as_str().into(),
        etag: value
            .definition_etag()
            .map_or("", domain::DefinitionEtag::as_str)
            .into(),
        request_id: value
            .request_id()
            .map_or("", domain::RequestId::as_str)
            .into(),
        expire_time: buffa_types::google::protobuf::Timestamp {
            seconds: value.deadline().seconds(),
            nanos: value.deadline().nanos(),
            ..Default::default()
        }
        .into(),
        command: Some(command),
        ..Default::default()
    }
}

pub fn parse_temperature_mode(
    v: EnumValue<wire::TemperatureMode>,
) -> Result<TemperatureMode, Violation> {
    match v {
        EnumValue::Known(wire::TemperatureMode::Off) => Ok(TemperatureMode::Off),
        EnumValue::Known(wire::TemperatureMode::Heat) => Ok(TemperatureMode::Heat),
        EnumValue::Known(wire::TemperatureMode::Cool) => Ok(TemperatureMode::Cool),
        EnumValue::Known(wire::TemperatureMode::Auto) => Ok(TemperatureMode::Auto),
        EnumValue::Known(wire::TemperatureMode::Dry) => Ok(TemperatureMode::Dry),
        EnumValue::Known(wire::TemperatureMode::FanOnly) => Ok(TemperatureMode::FanOnly),
        EnumValue::Known(wire::TemperatureMode::Unspecified) => Err(reject(
            "temperature_control.mode",
            ViolationKind::MissingIntent,
        )),
        EnumValue::Unknown(_) => Err(reject(
            "temperature_control.mode",
            ViolationKind::UnknownValue,
        )),
    }
}
fn encode_temperature_mode(v: TemperatureMode) -> EnumValue<wire::TemperatureMode> {
    match v {
        TemperatureMode::Off => wire::TemperatureMode::Off,
        TemperatureMode::Heat => wire::TemperatureMode::Heat,
        TemperatureMode::Cool => wire::TemperatureMode::Cool,
        TemperatureMode::Auto => wire::TemperatureMode::Auto,
        TemperatureMode::Dry => wire::TemperatureMode::Dry,
        TemperatureMode::FanOnly => wire::TemperatureMode::FanOnly,
    }
    .into()
}
pub fn parse_temperature_fan_target(
    v: EnumValue<wire::TemperatureFanTarget>,
) -> Result<TemperatureFanTarget, Violation> {
    match v {
        EnumValue::Known(wire::TemperatureFanTarget::On) => Ok(TemperatureFanTarget::On),
        EnumValue::Known(wire::TemperatureFanTarget::Auto) => Ok(TemperatureFanTarget::Auto),
        EnumValue::Known(wire::TemperatureFanTarget::Unspecified) => Err(reject(
            "temperature_control.fan_target",
            ViolationKind::MissingIntent,
        )),
        EnumValue::Unknown(_) => Err(reject(
            "temperature_control.fan_target",
            ViolationKind::UnknownValue,
        )),
    }
}
fn encode_temperature_fan_target(v: TemperatureFanTarget) -> EnumValue<wire::TemperatureFanTarget> {
    match v {
        TemperatureFanTarget::On => wire::TemperatureFanTarget::On,
        TemperatureFanTarget::Auto => wire::TemperatureFanTarget::Auto,
    }
    .into()
}
pub fn parse_cover_operation(
    v: EnumValue<wire::CoverOperation>,
) -> Result<CoverOperation, Violation> {
    match v {
        EnumValue::Known(wire::CoverOperation::Open) => Ok(CoverOperation::Open),
        EnumValue::Known(wire::CoverOperation::Close) => Ok(CoverOperation::Close),
        EnumValue::Known(wire::CoverOperation::Stop) => Ok(CoverOperation::Stop),
        EnumValue::Known(wire::CoverOperation::Unspecified) => {
            Err(reject("cover.operation", ViolationKind::MissingIntent))
        }
        EnumValue::Unknown(_) => Err(reject("cover.operation", ViolationKind::UnknownValue)),
    }
}
fn encode_cover_operation(v: CoverOperation) -> EnumValue<wire::CoverOperation> {
    match v {
        CoverOperation::Open => wire::CoverOperation::Open,
        CoverOperation::Close => wire::CoverOperation::Close,
        CoverOperation::Stop => wire::CoverOperation::Stop,
    }
    .into()
}
pub fn parse_lock_operation(v: EnumValue<wire::LockOperation>) -> Result<LockOperation, Violation> {
    match v {
        EnumValue::Known(wire::LockOperation::Lock) => Ok(LockOperation::Lock),
        EnumValue::Known(wire::LockOperation::Unlock) => Ok(LockOperation::Unlock),
        EnumValue::Known(wire::LockOperation::Unspecified) => {
            Err(reject("lock.operation", ViolationKind::MissingIntent))
        }
        EnumValue::Unknown(_) => Err(reject("lock.operation", ViolationKind::UnknownValue)),
    }
}
fn encode_lock_operation(v: LockOperation) -> EnumValue<wire::LockOperation> {
    match v {
        LockOperation::Lock => wire::LockOperation::Lock,
        LockOperation::Unlock => wire::LockOperation::Unlock,
    }
    .into()
}
pub fn parse_alarm_operation(
    v: EnumValue<wire::AlarmOperation>,
) -> Result<AlarmOperation, Violation> {
    match v {
        EnumValue::Known(wire::AlarmOperation::ArmHome) => Ok(AlarmOperation::ArmHome),
        EnumValue::Known(wire::AlarmOperation::ArmAway) => Ok(AlarmOperation::ArmAway),
        EnumValue::Known(wire::AlarmOperation::ArmNight) => Ok(AlarmOperation::ArmNight),
        EnumValue::Known(wire::AlarmOperation::Disarm) => Ok(AlarmOperation::Disarm),
        EnumValue::Known(wire::AlarmOperation::Unspecified) => {
            Err(reject("alarm.operation", ViolationKind::MissingIntent))
        }
        EnumValue::Unknown(_) => Err(reject("alarm.operation", ViolationKind::UnknownValue)),
    }
}
fn encode_alarm_operation(v: AlarmOperation) -> EnumValue<wire::AlarmOperation> {
    match v {
        AlarmOperation::ArmHome => wire::AlarmOperation::ArmHome,
        AlarmOperation::ArmAway => wire::AlarmOperation::ArmAway,
        AlarmOperation::ArmNight => wire::AlarmOperation::ArmNight,
        AlarmOperation::Disarm => wire::AlarmOperation::Disarm,
    }
    .into()
}
pub fn parse_helper_operation(
    v: EnumValue<wire::HelperOperation>,
) -> Result<HelperOperation, Violation> {
    match v {
        EnumValue::Known(wire::HelperOperation::Set) => Ok(HelperOperation::Set),
        EnumValue::Known(wire::HelperOperation::Toggle) => Ok(HelperOperation::Toggle),
        EnumValue::Known(wire::HelperOperation::Increment) => Ok(HelperOperation::Increment),
        EnumValue::Known(wire::HelperOperation::Decrement) => Ok(HelperOperation::Decrement),
        EnumValue::Known(wire::HelperOperation::Reset) => Ok(HelperOperation::Reset),
        EnumValue::Known(wire::HelperOperation::Press) => Ok(HelperOperation::Press),
        EnumValue::Known(wire::HelperOperation::Next) => Ok(HelperOperation::Next),
        EnumValue::Known(wire::HelperOperation::Previous) => Ok(HelperOperation::Previous),
        EnumValue::Known(wire::HelperOperation::Unspecified) => {
            Err(reject("helper.operation", ViolationKind::MissingIntent))
        }
        EnumValue::Unknown(_) => Err(reject("helper.operation", ViolationKind::UnknownValue)),
    }
}
fn encode_helper_operation(v: HelperOperation) -> EnumValue<wire::HelperOperation> {
    match v {
        HelperOperation::Set => wire::HelperOperation::Set,
        HelperOperation::Toggle => wire::HelperOperation::Toggle,
        HelperOperation::Increment => wire::HelperOperation::Increment,
        HelperOperation::Decrement => wire::HelperOperation::Decrement,
        HelperOperation::Reset => wire::HelperOperation::Reset,
        HelperOperation::Press => wire::HelperOperation::Press,
        HelperOperation::Next => wire::HelperOperation::Next,
        HelperOperation::Previous => wire::HelperOperation::Previous,
    }
    .into()
}

fn command_unknown(v: &Option<wire::execute_capability_request::Command>) -> bool {
    use wire::execute_capability_request::Command as W;
    match v {
        Some(W::Media(v)) => crate::api_media_contract::media_unknown_fields(v),
        Some(W::Power(v)) => !v.__buffa_unknown_fields.is_empty(),
        Some(W::Level(v)) => !v.__buffa_unknown_fields.is_empty(),
        Some(W::Light(v)) => {
            !v.__buffa_unknown_fields.is_empty()
                || v.power
                    .as_option()
                    .is_some_and(|v| !v.__buffa_unknown_fields.is_empty())
                || v.level
                    .as_option()
                    .is_some_and(|v| !v.__buffa_unknown_fields.is_empty())
                || v.transition_duration
                    .as_option()
                    .is_some_and(|v| !v.__buffa_unknown_fields.is_empty())
        }
        Some(W::Fan(v)) => {
            !v.__buffa_unknown_fields.is_empty()
                || v.power
                    .as_option()
                    .is_some_and(|v| !v.__buffa_unknown_fields.is_empty())
        }
        Some(W::TemperatureControl(v)) => {
            !v.__buffa_unknown_fields.is_empty()
                || match &v.setpoint {
                    Some(
                        wire::set_temperature_control_command::Setpoint::SetpointRangeCelsius(v),
                    ) => !v.__buffa_unknown_fields.is_empty(),
                    Some(wire::set_temperature_control_command::Setpoint::SetpointCelsius(_))
                    | None => false,
                }
        }
        Some(W::Cover(v)) => !v.__buffa_unknown_fields.is_empty(),
        Some(W::Lock(v)) => !v.__buffa_unknown_fields.is_empty(),
        Some(W::Alarm(v)) => !v.__buffa_unknown_fields.is_empty(),
        Some(W::Helper(v)) => {
            !v.__buffa_unknown_fields.is_empty() || v.value.as_option().is_some_and(helper_unknown)
        }
        None => false,
    }
}
fn helper_unknown(v: &wire::HelperInputValue) -> bool {
    use wire::helper_input_value::Value as V;
    !v.__buffa_unknown_fields.is_empty()
        || match &v.value {
            Some(V::Date(v)) => !v.__buffa_unknown_fields.is_empty(),
            Some(V::Time(v)) => !v.__buffa_unknown_fields.is_empty(),
            Some(V::LocalValue(v)) => {
                !v.__buffa_unknown_fields.is_empty()
                    || v.date
                        .as_option()
                        .is_some_and(|v| !v.__buffa_unknown_fields.is_empty())
                    || v.time
                        .as_option()
                        .is_some_and(|v| !v.__buffa_unknown_fields.is_empty())
            }
            Some(
                V::BooleanValue(_)
                | V::Number(_)
                | V::Counter(_)
                | V::SelectedOption(_)
                | V::Text(_),
            )
            | None => false,
        }
}
fn parse_light(v: &wire::SetLightCommand) -> Result<LightIntent, Violation> {
    let on = v
        .power
        .as_option()
        .map(|p| {
            parse_power(p.value)
                .map(|p| match p {
                    Power::On => true,
                    Power::Off => false,
                })
                .map_err(|e| reject("light.power.value", e.kind))
        })
        .transpose()?;
    let brightness = match v.level.as_option() {
        Some(level) => match level.target {
            Some(wire::set_level_command::Target::Percentage(x)) => Some(
                Percentage::try_from(x)
                    .map_err(|_| reject("light.level.percentage", ViolationKind::OutOfRange))?,
            ),
            None => return Err(reject("light.level", ViolationKind::MissingIntent)),
        },
        None => None,
    };
    let color = v
        .color_temperature_mireds
        .map(ColorTemperature::try_from)
        .transpose()
        .map_err(|_| reject("light.color_temperature_mireds", ViolationKind::OutOfRange))?;
    let transition = v
        .transition_duration
        .as_option()
        .map(|x| Transition::new(x.seconds, x.nanos))
        .transpose()
        .map_err(|_| reject("light.transition_duration", ViolationKind::OutOfRange))?;
    if transition.is_some() && on == Some(false) && (brightness.is_some() || color.is_some()) {
        return Err(reject("light", ViolationKind::InconsistentFields));
    }
    let rgb = v.rgb.as_option().map(parse_rgb).transpose()?;
    if rgb.is_some() && color.is_some() {
        return Err(reject("light.color", ViolationKind::InconsistentFields));
    }
    let color = color
        .map(ColorTarget::Temperature)
        .or_else(|| rgb.map(ColorTarget::Rgb));
    let preset = v
        .preset
        .as_deref()
        .map(SelectorKey::try_from)
        .transpose()
        .map_err(|_| reject("light.preset", ViolationKind::OutOfRange))?;
    LightIntent::new(on, brightness, color, preset, transition)
        .map_err(|_| reject("light", ViolationKind::MissingIntent))
}
fn parse_fan(v: &wire::SetFanCommand) -> Result<FanIntent, Violation> {
    let speed = v
        .speed
        .as_ref()
        .map(|x| SelectorKey::try_from(x.as_str()))
        .transpose()
        .map_err(|_| reject("fan.speed", ViolationKind::OutOfRange))?;
    FanIntent::new(
        v.power
            .as_option()
            .map(|p| {
                parse_power(p.value)
                    .map(|p| match p {
                        Power::On => true,
                        Power::Off => false,
                    })
                    .map_err(|e| reject("fan.power.value", e.kind))
            })
            .transpose()?,
        match (speed, v.level) {
            (Some(_), Some(_)) => {
                return Err(reject("fan.level", ViolationKind::InconsistentFields));
            }
            (Some(key), None) => Some(LevelTarget::Step(key)),
            (None, Some(value)) => Some(LevelTarget::Range(
                Finite::try_from(value)
                    .map_err(|_| reject("fan.level", ViolationKind::OutOfRange))?,
            )),
            (None, None) => None,
        },
    )
    .map_err(|_| reject("fan", ViolationKind::MissingIntent))
}
fn parse_thermal(v: &wire::SetTemperatureControlCommand) -> Result<ThermalIntent, Violation> {
    use wire::set_temperature_control_command::Setpoint as S;
    let target = match &v.setpoint {
        Some(S::SetpointCelsius(x)) => Some(TemperatureTarget::Single(
            Celsius::try_from(*x).map_err(|_| {
                reject(
                    "temperature_control.setpoint_celsius",
                    ViolationKind::OutOfRange,
                )
            })?,
        )),
        Some(S::SetpointRangeCelsius(x)) => {
            let low = x.low_celsius.ok_or_else(|| {
                reject(
                    "temperature_control.setpoint_range_celsius",
                    ViolationKind::MissingIntent,
                )
            })?;
            let high = x.high_celsius.ok_or_else(|| {
                reject(
                    "temperature_control.setpoint_range_celsius",
                    ViolationKind::MissingIntent,
                )
            })?;
            let range = Celsius::try_from(low)
                .and_then(|lo| Celsius::try_from(high).and_then(|hi| TemperatureRange::new(lo, hi)))
                .map_err(|_| {
                    reject(
                        "temperature_control.setpoint_range_celsius",
                        ViolationKind::OutOfRange,
                    )
                })?;
            Some(TemperatureTarget::Range(range))
        }
        None => None,
    };
    ThermalIntent::new(
        target,
        v.mode.map(parse_temperature_mode).transpose()?,
        v.fan_target.map(parse_temperature_fan_target).transpose()?,
    )
    .map_err(|_| reject("temperature_control", ViolationKind::MissingIntent))
}
fn parse_cover(v: &wire::SetCoverCommand) -> Result<CoverIntent, Violation> {
    use wire::set_cover_command::Target;
    match v.target {
        Some(Target::Operation(x)) => Ok(CoverIntent::Operation(parse_cover_operation(x)?)),
        Some(Target::PositionPercent(x)) => Percentage::try_from(x)
            .map(CoverIntent::Position)
            .map_err(|_| reject("cover.position_percent", ViolationKind::OutOfRange)),
        None => Err(reject("cover", ViolationKind::MissingIntent)),
    }
}
fn parse_lock(v: &wire::SetLockCommand) -> Result<LockOperation, Violation> {
    parse_lock_operation(v.operation)
}
fn parse_alarm(v: &wire::SetAlarmCommand) -> Result<AlarmOperation, Violation> {
    parse_alarm_operation(v.operation)
}
fn parse_local_date(v: &crate::google::r#type::Date) -> Result<LocalDate, domain::AdmissionError> {
    LocalDate::new(v.year, v.month, v.day)
}
fn parse_clock(v: &crate::google::r#type::TimeOfDay) -> Result<ClockTime, domain::AdmissionError> {
    if v.nanos != 0 {
        return Err(domain::AdmissionError::Intent);
    }
    ClockTime::new(v.hours, v.minutes, v.seconds)
}
fn parse_helper_value(v: &wire::HelperInputValue) -> Result<HelperValue, domain::AdmissionError> {
    use wire::helper_input_value::Value as V;
    match &v.value {
        Some(V::BooleanValue(x)) => Ok(HelperValue::Boolean(*x)),
        Some(V::Number(x)) => Finite::try_from(*x).map(HelperValue::Number),
        Some(V::Counter(x)) => Ok(HelperValue::Counter(*x)),
        Some(V::Date(x)) => parse_local_date(x).map(HelperValue::Date),
        Some(V::Time(x)) => parse_clock(x).map(HelperValue::Time),
        Some(V::LocalValue(x)) => {
            let d = x.date.as_option().ok_or(domain::AdmissionError::Intent)?;
            let t = x.time.as_option().ok_or(domain::AdmissionError::Intent)?;
            Ok(HelperValue::LocalDateTime(
                parse_local_date(d)?,
                parse_clock(t)?,
            ))
        }
        Some(V::SelectedOption(x)) => SelectorKey::try_from(x.as_str()).map(HelperValue::Selected),
        Some(V::Text(x)) => HelperText::try_from(x.as_str()).map(HelperValue::Text),
        None => Err(domain::AdmissionError::Intent),
    }
}
fn parse_helper(v: &wire::SetHelperCommand) -> Result<HelperIntent, Violation> {
    let operation = parse_helper_operation(v.operation)?;
    let revision = v.expected_helper_revision.ok_or_else(|| {
        reject(
            "helper.expected_helper_revision",
            ViolationKind::MissingIntent,
        )
    })?;
    if revision < 0 {
        return Err(reject(
            "helper.expected_helper_revision",
            ViolationKind::OutOfRange,
        ));
    }
    let value = v
        .value
        .as_option()
        .map(parse_helper_value)
        .transpose()
        .map_err(|_| reject("helper.value", ViolationKind::OutOfRange))?;
    if operation != HelperOperation::Set && value.is_some() {
        return Err(reject("helper.value", ViolationKind::InconsistentFields));
    }
    HelperIntent::new(operation, value, revision)
        .map_err(|_| reject("helper.value", ViolationKind::MissingIntent))
}
fn encode_light(v: LightIntent) -> wire::SetLightCommand {
    wire::SetLightCommand {
        power: v
            .on()
            .map(|on| wire::SetPowerCommand {
                value: if on {
                    wire::PowerValue::On
                } else {
                    wire::PowerValue::Off
                }
                .into(),
                ..Default::default()
            })
            .into(),
        level: v
            .brightness()
            .map(|p| wire::SetLevelCommand {
                target: Some(wire::set_level_command::Target::Percentage(p.value())),
                ..Default::default()
            })
            .into(),
        color_temperature_mireds: match v.color() {
            Some(ColorTarget::Temperature(v)) => Some(v.mireds()),
            _ => None,
        },
        rgb: match v.color() {
            Some(ColorTarget::Rgb(v)) => Some(encode_rgb(v)),
            _ => None,
        }
        .into(),
        preset: v.preset().map(|v| v.as_str().into()),
        transition_duration: v
            .transition()
            .map(|v| buffa_types::google::protobuf::Duration {
                seconds: v.seconds(),
                nanos: v.nanos(),
                ..Default::default()
            })
            .into(),
        ..Default::default()
    }
}
pub fn encode_fan(v: &FanIntent) -> wire::SetFanCommand {
    wire::SetFanCommand {
        power: v
            .on()
            .map(|on| wire::SetPowerCommand {
                value: if on {
                    wire::PowerValue::On
                } else {
                    wire::PowerValue::Off
                }
                .into(),
                ..Default::default()
            })
            .into(),
        speed: match v.level() {
            Some(LevelTarget::Step(key)) => Some(key.as_str().into()),
            _ => None,
        },
        level: match v.level() {
            Some(LevelTarget::Range(value)) => Some(value.value()),
            _ => None,
        },
        ..Default::default()
    }
}
pub fn encode_thermal(v: ThermalIntent) -> wire::SetTemperatureControlCommand {
    use wire::set_temperature_control_command::Setpoint as S;
    wire::SetTemperatureControlCommand {
        setpoint: v.target().map(|v| match v {
            TemperatureTarget::Single(v) => S::SetpointCelsius(v.value()),
            TemperatureTarget::Range(v) => {
                S::SetpointRangeCelsius(Box::new(wire::TemperatureRange {
                    low_celsius: Some(v.low().value()),
                    high_celsius: Some(v.high().value()),
                    ..Default::default()
                }))
            }
        }),
        mode: v.mode().map(encode_temperature_mode),
        fan_target: v.fan().map(encode_temperature_fan_target),
        ..Default::default()
    }
}
fn encode_cover(v: CoverIntent) -> wire::SetCoverCommand {
    use wire::set_cover_command::Target as T;
    wire::SetCoverCommand {
        target: Some(match v {
            CoverIntent::Operation(x) => T::Operation(encode_cover_operation(x)),
            CoverIntent::Position(x) => T::PositionPercent(x.value()),
        }),
        ..Default::default()
    }
}
fn encode_lock(v: LockOperation) -> wire::SetLockCommand {
    wire::SetLockCommand {
        operation: encode_lock_operation(v),
        ..Default::default()
    }
}
fn encode_alarm(v: AlarmOperation) -> wire::SetAlarmCommand {
    wire::SetAlarmCommand {
        operation: encode_alarm_operation(v),
        ..Default::default()
    }
}
fn encode_date(v: LocalDate) -> crate::google::r#type::Date {
    let (year, month, day) = v.components();
    crate::google::r#type::Date {
        year,
        month,
        day,
        ..Default::default()
    }
}
fn encode_clock(v: ClockTime) -> crate::google::r#type::TimeOfDay {
    let (hours, minutes, seconds) = v.components();
    crate::google::r#type::TimeOfDay {
        hours,
        minutes,
        seconds,
        ..Default::default()
    }
}
fn encode_helper_value(v: &HelperValue) -> wire::HelperInputValue {
    use wire::helper_input_value::Value as V;
    wire::HelperInputValue {
        value: Some(match v {
            HelperValue::Boolean(x) => V::BooleanValue(*x),
            HelperValue::Number(x) => V::Number(x.value()),
            HelperValue::Counter(x) => V::Counter(*x),
            HelperValue::Date(x) => V::Date(Box::new(encode_date(*x))),
            HelperValue::Time(x) => V::Time(Box::new(encode_clock(*x))),
            HelperValue::LocalDateTime(d, t) => V::LocalValue(Box::new(wire::LocalDateTime {
                date: encode_date(*d).into(),
                time: encode_clock(*t).into(),
                ..Default::default()
            })),
            HelperValue::Selected(x) => V::SelectedOption(x.as_str().into()),
            HelperValue::Text(x) => V::Text(x.as_str().into()),
        }),
        ..Default::default()
    }
}
fn encode_helper(v: &HelperIntent) -> wire::SetHelperCommand {
    wire::SetHelperCommand {
        operation: encode_helper_operation(v.operation()),
        value: v.value().map(encode_helper_value).into(),
        expected_helper_revision: Some(v.revision()),
        ..Default::default()
    }
}

pub fn parse_rgb(v: &wire::RgbColor) -> Result<Rgb8, Violation> {
    if !v.__buffa_unknown_fields.is_empty() {
        return Err(reject("rgb", ViolationKind::OutOfRange));
    }
    let channel = |v: Option<u32>| {
        v.and_then(|v| u8::try_from(v).ok())
            .ok_or_else(|| reject("rgb", ViolationKind::OutOfRange))
    };
    Ok(Rgb8 {
        red: channel(v.red)?,
        green: channel(v.green)?,
        blue: channel(v.blue)?,
    })
}
pub fn encode_rgb(v: Rgb8) -> wire::RgbColor {
    wire::RgbColor {
        red: Some(u32::from(v.red)),
        green: Some(u32::from(v.green)),
        blue: Some(u32::from(v.blue)),
        ..Default::default()
    }
}

#[cfg(test)]
mod modular_control_tests {
    use super::*;
    #[test]
    fn rgb_preset_remains_one_atomic_intention() {
        let value = wire::SetLightCommand {
            rgb: Some(wire::RgbColor {
                red: Some(0),
                green: Some(137),
                blue: Some(255),
                ..Default::default()
            })
            .into(),
            preset: Some("scene:night/7".into()),
            ..Default::default()
        };
        let admitted = parse_light(&value).unwrap();
        let encoded = encode_light(admitted);
        assert_eq!(encoded.rgb, value.rgb);
        assert_eq!(encoded.preset, value.preset);
        let mut contradictory = value.clone();
        contradictory.color_temperature_mireds = Some(300);
        assert!(parse_light(&contradictory).is_err());
        let mut invalid = value;
        invalid.rgb = Some(wire::RgbColor {
            red: Some(0),
            green: Some(137),
            blue: Some(256),
            ..Default::default()
        })
        .into();
        assert!(parse_light(&invalid).is_err());
    }
    #[test]
    fn exact_fan_key_or_native_range_never_interpolates() {
        for key in ["quiet/7", "boost:91", "night/13"] {
            let value = wire::SetFanCommand {
                speed: Some(key.into()),
                ..Default::default()
            };
            assert_eq!(
                encode_fan(&parse_fan(&value).unwrap()).speed.as_deref(),
                Some(key)
            );
        }
        let value = wire::SetFanCommand {
            level: Some(7.5),
            ..Default::default()
        };
        assert_eq!(encode_fan(&parse_fan(&value).unwrap()).level, Some(7.5));
        let mut ambiguous = value;
        ambiguous.speed = Some("7.5".into());
        assert!(parse_fan(&ambiguous).is_err());
    }
}
