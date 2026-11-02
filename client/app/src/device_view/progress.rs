use crate::ProActionPhase as Phase;
use buffa::EnumValue;
use home_client_actions::{ActionSnapshot, LiveState, SubmissionState};
use home_wire_contract::home::actions::v1 as wire;
type Presentation = (Phase, &'static str, &'static str);
fn contract_error(error: &wire::ContractError) -> &'static str {
    match error.code {
        EnumValue::Known(wire::ErrorCode::InvalidArgument) => {
            "The requested value was rejected before admission."
        }
        EnumValue::Known(wire::ErrorCode::UnsupportedCapability) => {
            "This capability can no longer be controlled."
        }
        EnumValue::Known(wire::ErrorCode::StaleEpoch) => {
            "The server restarted. Repeat the gesture in the new view."
        }
        EnumValue::Known(wire::ErrorCode::StaleConfiguration) => {
            "The configuration changed. Repeat the gesture in the new view."
        }
        EnumValue::Known(wire::ErrorCode::PermissionDenied) => "Your authorization was removed.",
        EnumValue::Known(wire::ErrorCode::Unspecified) | EnumValue::Unknown(_) => {
            "This version does not recognize the rejection."
        }
    }
}
pub(crate) fn presentation(snapshot: Option<&ActionSnapshot>) -> Presentation {
    let Some(action) = snapshot else {
        return (Phase::Idle, "", "");
    };
    // Only live server progress can assert confirmation. A durable dispatch
    // result, a matching cached value or a Submit response cannot do so.
    if let Some(point) = &action.progress {
        match point.status {
            EnumValue::Known(wire::ActionStatus::Confirmed) => {
                return (
                    Phase::Confirmed,
                    "Effect confirmed",
                    "Server confirmation received; the observed state remains separate.",
                );
            }
            EnumValue::Known(wire::ActionStatus::Failed) => {
                return (
                    Phase::Failed,
                    "Command failed",
                    "Server tracking reports a failure. The observed state is unchanged.",
                );
            }
            EnumValue::Known(wire::ActionStatus::Cancelled) => {
                return (
                    Phase::Cancelled,
                    "Command cancelled",
                    "The server ended this action as cancelled.",
                );
            }
            EnumValue::Known(wire::ActionStatus::Indeterminate) => {
                return (
                    Phase::Indeterminate,
                    "Couldn’t confirm the result",
                    "The command may have executed. It will not be submitted again automatically.",
                );
            }
            EnumValue::Known(
                wire::ActionStatus::Accepted
                | wire::ActionStatus::Dispatched
                | wire::ActionStatus::Acknowledged,
            ) => {}
            EnumValue::Known(wire::ActionStatus::Unspecified) | EnumValue::Unknown(_) => {
                return (
                    Phase::Unavailable,
                    "Unrecognized progress",
                    "This version cannot interpret the received command status.",
                );
            }
        }
    }
    match action.live {
        LiveState::Stopped => {
            return (
                Phase::Stopped,
                "Tracking stopped on this device",
                "This does not cancel a command the server has already accepted.",
            );
        }
        LiveState::Unavailable => {
            return (
                Phase::Unavailable,
                "Live tracking unavailable",
                "The saved result is retained. No additional physical effect is inferred.",
            );
        }
        LiveState::Interrupted => {
            return (
                Phase::Interrupted,
                "Tracking interrupted",
                "Checking the same command again; no new submission.",
            );
        }
        LiveState::NotRequested | LiveState::Available => {}
    }
    if let Some(response) = &action.response
        && response.status == wire::ActionStatus::Failed
        && let Some(error) = response.error.as_option()
    {
        return (Phase::Refused, "Rejected", contract_error(error));
    }
    if let Some(point) = &action.progress {
        let detail = if action.submission == SubmissionState::Uncertain {
            "Initial response lost; the same command was found."
        } else {
            "The observed state remains independent of this command."
        };
        return match point.status {
            EnumValue::Known(wire::ActionStatus::Accepted) => {
                (Phase::Accepted, "Command accepted", detail)
            }
            EnumValue::Known(wire::ActionStatus::Dispatched) => {
                (Phase::Dispatched, "Command forwarded", detail)
            }
            EnumValue::Known(wire::ActionStatus::Acknowledged) => (
                Phase::Acknowledged,
                "Acknowledged · waiting for confirmation",
                detail,
            ),
            EnumValue::Known(
                wire::ActionStatus::Confirmed
                | wire::ActionStatus::Failed
                | wire::ActionStatus::Cancelled
                | wire::ActionStatus::Indeterminate
                | wire::ActionStatus::Unspecified,
            )
            | EnumValue::Unknown(_) => (
                Phase::Unavailable,
                "Tracking unavailable",
                "No additional confirmation is inferred.",
            ),
        };
    }
    match action.submission {
        SubmissionState::Prepared => (
            Phase::Preparing,
            "Checking Command",
            "The action will be discarded if the view or your permissions change.",
        ),
        SubmissionState::InFlight => (
            Phase::Sending,
            "Sending",
            "This action is submitted only once.",
        ),
        SubmissionState::Replied => (
            Phase::Accepted,
            "Response received · tracking",
            "The submission response does not confirm the physical effect.",
        ),
        SubmissionState::Uncertain => (
            Phase::Uncertain,
            "Submission uncertain",
            "Checking the same command; it will not be resubmitted automatically.",
        ),
        SubmissionState::NotSent => (
            Phase::Refused,
            "Not sent",
            "The gesture context was no longer valid before submission.",
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn snapshot() -> ActionSnapshot {
        ActionSnapshot {
            id: "synthetic-command".parse().unwrap(),
            submission: SubmissionState::Replied,
            live: LiveState::NotRequested,
            response: Some(wire::CommandResponse {
                command_id: "synthetic-command".into(),
                status: wire::ActionStatus::Confirmed.into(),
                ..Default::default()
            }),
            failure: None,
            durable: None,
            progress: None,
            issue: None,
        }
    }
    #[test]
    fn dispatch_results_never_become_confirmation_and_ack_expiry_and_refusal_stay_distinct() {
        let mut action = snapshot();
        assert_eq!(presentation(Some(&action)).0, Phase::Accepted);
        action.live = LiveState::Unavailable;
        action.durable = Some(wire::DurableActionResult {
            response: action.response.clone().into(),
            state: wire::DurableActionState::Result.into(),
            error: wire::DurableActionError::None.into(),
            ..Default::default()
        });
        assert_eq!(presentation(Some(&action)).0, Phase::Unavailable);
        action.live = LiveState::Available;
        action.progress = Some(wire::ActionProgress {
            admission_epoch: "aabbccddeeff00112233445566778899".into(),
            config_revision: 1,
            sequence: 3,
            status: wire::ActionStatus::Acknowledged.into(),
            ..Default::default()
        });
        assert_eq!(presentation(Some(&action)).0, Phase::Acknowledged);
        action.progress.as_mut().unwrap().status = wire::ActionStatus::Indeterminate.into();
        assert_eq!(presentation(Some(&action)).0, Phase::Indeterminate);
        action.progress = None;
        action.response.as_mut().unwrap().status = wire::ActionStatus::Failed.into();
        action.response.as_mut().unwrap().error = wire::ContractError {
            code: wire::ErrorCode::StaleConfiguration.into(),
            ..Default::default()
        }
        .into();
        let refused = presentation(Some(&action));
        assert_eq!(refused.0, Phase::Refused);
        assert!(refused.2.contains("configuration"));
        action.progress = Some(wire::ActionProgress {
            status: wire::ActionStatus::Confirmed.into(),
            ..Default::default()
        });
        assert_eq!(presentation(Some(&action)).0, Phase::Confirmed);
    }
    #[test]
    fn stopping_local_tracking_does_not_render_a_physical_cancellation() {
        let mut action = snapshot();
        action.live = LiveState::Stopped;
        assert_eq!(presentation(Some(&action)).0, Phase::Stopped);
        action.progress = Some(wire::ActionProgress {
            status: wire::ActionStatus::Cancelled.into(),
            ..Default::default()
        });
        assert_eq!(presentation(Some(&action)).0, Phase::Cancelled);
    }
}
