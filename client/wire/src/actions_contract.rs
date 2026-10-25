//! Structural action-response admission. Rights and physical confirmation
//! remain server facts; a durable dispatch record is not live confirmation.
use crate::home::actions::v1 as wire;
use buffa::EnumValue;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AdmissionError {
    Invalid,
}

pub fn command_id(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256
}

pub fn admission_epoch(value: &str) -> bool {
    value.len() == 32
        && value.bytes().all(|b| b.is_ascii_hexdigit())
        && value.bytes().any(|b| b != b'0')
}

pub fn status(value: EnumValue<wire::ActionStatus>) -> Result<wire::ActionStatus, AdmissionError> {
    use wire::ActionStatus as S;
    match value {
        EnumValue::Known(
            value @ (S::Accepted
            | S::Dispatched
            | S::Acknowledged
            | S::Confirmed
            | S::Failed
            | S::Cancelled
            | S::Indeterminate),
        ) => Ok(value),
        EnumValue::Known(S::Unspecified) | EnumValue::Unknown(_) => Err(AdmissionError::Invalid),
    }
}

pub fn command_response(value: &wire::CommandResponse) -> Result<(), AdmissionError> {
    let error_known = value.error.as_option().is_none_or(|error| {
        let code_known = match error.code {
            EnumValue::Known(
                wire::ErrorCode::InvalidArgument
                | wire::ErrorCode::UnsupportedCapability
                | wire::ErrorCode::StaleEpoch
                | wire::ErrorCode::StaleConfiguration
                | wire::ErrorCode::PermissionDenied,
            ) => true,
            EnumValue::Known(wire::ErrorCode::Unspecified) | EnumValue::Unknown(_) => false,
        };
        code_known
            && error.__buffa_unknown_fields.is_empty()
            && error
                .detail
                .as_option()
                .is_none_or(|detail| detail.__buffa_unknown_fields.is_empty())
    });
    if value.__buffa_unknown_fields.is_empty()
        && command_id(&value.command_id)
        && status(value.status).is_ok()
        && error_known
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn progress(value: &wire::ActionProgress) -> Result<(), AdmissionError> {
    if value.__buffa_unknown_fields.is_empty()
        && value.sequence > 0
        && value.config_revision > 0
        && admission_epoch(&value.admission_epoch)
        && status(value.status).is_ok()
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn durable(value: &wire::DurableActionResult) -> Result<(), AdmissionError> {
    let response = value.response.as_option().ok_or(AdmissionError::Invalid)?;
    command_response(response)?;
    let known_error = match value.error {
        EnumValue::Known(
            wire::DurableActionError::None
            | wire::DurableActionError::RouteBusy
            | wire::DurableActionError::RouteUnavailable
            | wire::DurableActionError::Cancelled
            | wire::DurableActionError::Deadline
            | wire::DurableActionError::Other
            | wire::DurableActionError::OutcomeIndeterminate,
        ) => true,
        EnumValue::Known(wire::DurableActionError::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let consistent = match value.state {
        EnumValue::Known(wire::DurableActionState::Accepted) => {
            response.status == wire::ActionStatus::Accepted
                && value.error == wire::DurableActionError::None
        }
        EnumValue::Known(wire::DurableActionState::Dispatching) => {
            response.status == wire::ActionStatus::Indeterminate
                && value.error == wire::DurableActionError::None
        }
        EnumValue::Known(wire::DurableActionState::Result) => {
            response.status != wire::ActionStatus::Indeterminate
        }
        EnumValue::Known(wire::DurableActionState::Indeterminate) => {
            response.status == wire::ActionStatus::Indeterminate
        }
        EnumValue::Known(wire::DurableActionState::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if value.__buffa_unknown_fields.is_empty() && known_error && consistent {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn get_response(value: &wire::GetActionResponse) -> Result<(), AdmissionError> {
    use wire::get_action_response::Live;
    durable(value.durable.as_option().ok_or(AdmissionError::Invalid)?)?;
    let live_known = match &value.live {
        Some(Live::Progress(point)) => progress(point).is_ok(),
        Some(Live::Unavailable(empty)) => empty.__buffa_unknown_fields.is_empty(),
        None => false,
    };
    if value.__buffa_unknown_fields.is_empty() && live_known {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn watch_response(value: &wire::WatchActionResponse) -> Result<(), AdmissionError> {
    use wire::watch_action_response::Frame;
    let frame_known = match &value.frame {
        Some(Frame::Progress(point)) => progress(point).is_ok(),
        Some(Frame::Unavailable(empty)) => {
            value.stream_sequence == 1 && empty.__buffa_unknown_fields.is_empty()
        }
        None => false,
    };
    if value.__buffa_unknown_fields.is_empty() && value.stream_sequence > 0 && frame_known {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}

pub fn operation_failure(value: &wire::ActionOperationFailure) -> Result<(), AdmissionError> {
    use wire::ActionOperationFailureKind as K;
    let known = match value.kind {
        EnumValue::Known(
            K::LedgerUnavailable
            | K::CommitOutcomeUnknown
            | K::OutcomeIndeterminate
            | K::IdConflict
            | K::PolicyConflict
            | K::RouteBusy
            | K::RouteUnavailable
            | K::Capacity
            | K::Cancelled
            | K::Deadline
            | K::ResultUnavailable
            | K::IntentTooLarge
            | K::ClientSlow,
        ) => true,
        EnumValue::Known(K::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if value.__buffa_unknown_fields.is_empty()
        && known
        && value
            .response
            .as_option()
            .is_none_or(|response| command_response(response).is_ok())
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}
