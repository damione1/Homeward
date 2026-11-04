//! The ExecuteCapability operation_info allowlist. Neither an arbitrary Any nor
//! a provider acknowledgement can become physical confirmation.
use crate::{
    api_errors::{ApiFailure, admit_status},
    api_operations::{InvalidOperation, admit_envelope},
    google::longrunning as operations,
    home::api::v2 as wire,
};
use buffa::{EnumValue, Message};
use home_client_model::{
    api::{CapabilityName, Command, OperationName},
    local_time::UtcInstant,
};
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CommandPhase {
    Accepted,
    Dispatched,
    Acknowledged,
    Confirmed,
    Failed,
    Cancelled,
    Indeterminate,
}
impl CommandPhase {
    fn rank(self) -> u8 {
        match self {
            Self::Accepted => 0,
            Self::Dispatched => 1,
            Self::Acknowledged => 2,
            Self::Confirmed | Self::Failed | Self::Cancelled | Self::Indeterminate => 3,
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CommandOutcome {
    Confirmed,
    AcknowledgedUnverified,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CommandCompletion {
    Pending,
    Success {
        outcome: CommandOutcome,
        observed_time: Option<UtcInstant>,
    },
    Failure(ApiFailure),
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CommandOperation {
    name: OperationName,
    target: CapabilityName,
    request_id: Option<home_client_model::api::RequestId>,
    created: UtcInstant,
    phase: CommandPhase,
    live: bool,
    completion: CommandCompletion,
}
impl CommandOperation {
    pub fn name(&self) -> &OperationName {
        &self.name
    }
    pub fn target(&self) -> &CapabilityName {
        &self.target
    }
    pub fn phase(&self) -> CommandPhase {
        self.phase
    }
    pub fn created(&self) -> UtcInstant {
        self.created
    }
    pub fn live_progress_available(&self) -> bool {
        self.live
    }
    pub fn completion(&self) -> &CommandCompletion {
        &self.completion
    }
    pub fn done(&self) -> bool {
        !matches!(self.completion, CommandCompletion::Pending)
    }
}
fn instant(
    value: &buffa_types::google::protobuf::Timestamp,
) -> Result<UtcInstant, InvalidOperation> {
    if !value.__buffa_unknown_fields.is_empty()
        || (value.seconds == -62_135_596_800 && value.nanos == 0)
    {
        return Err(InvalidOperation::Malformed);
    }
    UtcInstant::new(value.seconds, value.nanos).map_err(|_| InvalidOperation::Malformed)
}
fn phase(value: EnumValue<wire::CommandPhase>) -> Result<CommandPhase, InvalidOperation> {
    use EnumValue::{Known, Unknown};
    match value {
        Known(wire::CommandPhase::Accepted) => Ok(CommandPhase::Accepted),
        Known(wire::CommandPhase::Dispatched) => Ok(CommandPhase::Dispatched),
        Known(wire::CommandPhase::Acknowledged) => Ok(CommandPhase::Acknowledged),
        Known(wire::CommandPhase::Confirmed) => Ok(CommandPhase::Confirmed),
        Known(wire::CommandPhase::Failed) => Ok(CommandPhase::Failed),
        Known(wire::CommandPhase::Cancelled) => Ok(CommandPhase::Cancelled),
        Known(wire::CommandPhase::Indeterminate) => Ok(CommandPhase::Indeterminate),
        Known(wire::CommandPhase::Unspecified) | Unknown(_) => Err(InvalidOperation::Unsupported),
    }
}
pub fn admit_command_operation(
    raw: &operations::Operation,
    command: &Command,
    previous: Option<&CommandOperation>,
) -> Result<CommandOperation, InvalidOperation> {
    let name = OperationName::try_from(raw.name.as_str()).map_err(|_| InvalidOperation::Name)?;
    admit_envelope(raw, &name)?;
    if name.home() != command.target().home() {
        return Err(InvalidOperation::Name);
    }
    let metadata = &raw.metadata;
    if metadata.type_url != "type.googleapis.com/home.api.v2.CommandMetadata" {
        return Err(InvalidOperation::Unsupported);
    }
    let value = wire::CommandMetadata::decode_from_slice(&metadata.value)
        .map_err(|_| InvalidOperation::Malformed)?;
    if !value.__buffa_unknown_fields.is_empty()
        || value.capability != command.target().as_str()
        || value.request_id != command.request_id().map_or("", |id| id.as_str())
    {
        return Err(InvalidOperation::Inconsistent);
    }
    let created = instant(
        value
            .create_time
            .as_option()
            .ok_or(InvalidOperation::Malformed)?,
    )?;
    let phase = phase(value.phase)?;
    let completion = match &raw.result {
        None => {
            match phase {
                CommandPhase::Accepted | CommandPhase::Dispatched | CommandPhase::Acknowledged => {}
                CommandPhase::Confirmed
                | CommandPhase::Failed
                | CommandPhase::Cancelled
                | CommandPhase::Indeterminate => return Err(InvalidOperation::Inconsistent),
            }
            CommandCompletion::Pending
        }
        Some(operations::operation::Result::Response(result)) => {
            if result.type_url != "type.googleapis.com/home.api.v2.ExecuteCapabilityResponse"
                || result.value.len() > 64 * 1024
            {
                return Err(InvalidOperation::Unsupported);
            }
            let result = wire::ExecuteCapabilityResponse::decode_from_slice(&result.value)
                .map_err(|_| InvalidOperation::Malformed)?;
            if !result.__buffa_unknown_fields.is_empty()
                || result.capability != command.target().as_str()
            {
                return Err(InvalidOperation::Inconsistent);
            }
            let observed_time = result.observed_time.as_option().map(instant).transpose()?;
            let outcome = match result.outcome {
                EnumValue::Known(wire::CommandOutcome::Confirmed) => {
                    if phase != CommandPhase::Confirmed {
                        return Err(InvalidOperation::Inconsistent);
                    }
                    CommandOutcome::Confirmed
                }
                EnumValue::Known(wire::CommandOutcome::AcknowledgedUnverified) => {
                    if phase != CommandPhase::Acknowledged || observed_time.is_some() {
                        return Err(InvalidOperation::Inconsistent);
                    }
                    CommandOutcome::AcknowledgedUnverified
                }
                EnumValue::Known(wire::CommandOutcome::Unspecified) | EnumValue::Unknown(_) => {
                    return Err(InvalidOperation::Unsupported);
                }
            };
            CommandCompletion::Success {
                outcome,
                observed_time,
            }
        }
        Some(operations::operation::Result::Error(error)) => {
            match phase {
                CommandPhase::Failed | CommandPhase::Cancelled | CommandPhase::Indeterminate => {}
                CommandPhase::Accepted
                | CommandPhase::Dispatched
                | CommandPhase::Acknowledged
                | CommandPhase::Confirmed => return Err(InvalidOperation::Inconsistent),
            }
            let failure = admit_status(error).map_err(|_| InvalidOperation::Malformed)?;
            if (phase == CommandPhase::Indeterminate) != failure.uncertainty().is_some()
                || (phase == CommandPhase::Cancelled)
                    != (failure.reason() == wire::ApiErrorReason::RequestCancelled)
                || failure.operation().is_some_and(|handle| handle != &name)
            {
                return Err(InvalidOperation::Inconsistent);
            }
            CommandCompletion::Failure(failure)
        }
    };
    let operation = CommandOperation {
        name,
        target: command.target().clone(),
        request_id: command.request_id().cloned(),
        created,
        phase,
        live: value.live_progress_available,
        completion,
    };
    if let Some(prior) = previous {
        if prior.name != operation.name
            || prior.target != operation.target
            || prior.request_id != operation.request_id
            || prior.created != operation.created
            || (prior.done() && prior != &operation)
            || operation.phase.rank() < prior.phase.rank()
        {
            return Err(InvalidOperation::Inconsistent);
        }
    }
    Ok(operation)
}
