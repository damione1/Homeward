//! API v2 boundary: decode-compatible future input is not executable intent.
//! Authorization and live admission remain server responsibilities.
use crate::home::api::v2 as wire;
use buffa::EnumValue;
use home_client_model::api::{
    self as domain, CapabilityName, Command, Deadline, Intent, Percentage, Power, RequestId,
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
    let unknown_intent = match &request.command {
        Some(WireCommand::Power(value)) => !value.__buffa_unknown_fields.is_empty(),
        Some(WireCommand::Level(value)) => !value.__buffa_unknown_fields.is_empty(),
        None => false,
    };
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
    Ok(Command::new(target, intent, request_id, deadline))
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
    };
    wire::ExecuteCapabilityRequest {
        name: value.target().as_str().into(),
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
