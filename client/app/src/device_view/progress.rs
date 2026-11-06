use crate::ProActionPhase as Phase;
use home_client_actions::capabilities::{
    CapabilitySnapshot, CapabilitySubmission, CapabilityTracking,
};
use home_wire_contract::api_command_operations::{CommandCompletion, CommandOutcome, CommandPhase};
use home_wire_contract::home::api::v2::ApiErrorReason;

type Presentation = (Phase, &'static str, &'static str);

// Keep command facts and admission phases intact. Normal progress is shown by
// the control's busy state; observed values are the only successful visual result.
pub(crate) fn quiet(
    presentation: Presentation,
    snapshot: Option<&CapabilitySnapshot>,
) -> Presentation {
    let routine = match presentation.0 {
        Phase::Preparing
        | Phase::Sending
        | Phase::Accepted
        | Phase::Dispatched
        | Phase::Confirmed => true,
        Phase::Acknowledged => snapshot
            .and_then(|snapshot| snapshot.operation.as_ref())
            .is_some_and(|operation| !operation.done()),
        Phase::Idle
        | Phase::Refused
        | Phase::Uncertain
        | Phase::Stopped
        | Phase::Unavailable
        | Phase::Interrupted
        | Phase::Failed
        | Phase::Cancelled
        | Phase::Indeterminate => false,
    };
    if routine {
        (presentation.0, "", "")
    } else {
        presentation
    }
}
pub(crate) fn presentation(snapshot: Option<&CapabilitySnapshot>) -> Presentation {
    quiet(facts(snapshot), snapshot)
}
fn facts(snapshot: Option<&CapabilitySnapshot>) -> Presentation {
    let Some(action) = snapshot else {
        return (Phase::Idle, "", "");
    };
    if let Some(operation) = &action.operation {
        match operation.completion() {
            CommandCompletion::Success {
                outcome: CommandOutcome::Confirmed,
                ..
            } => {
                return (
                    Phase::Confirmed,
                    "Effect confirmed",
                    "The server recorded confirmation. Observed state remains separate.",
                );
            }
            CommandCompletion::Success {
                outcome: CommandOutcome::AcknowledgedUnverified,
                ..
            } => {
                return (
                    Phase::Acknowledged,
                    "Acknowledged · effect unverified",
                    "Tracking has ended. The acknowledgement does not establish the physical result.",
                );
            }
            CommandCompletion::Failure(failure) => {
                if failure.uncertainty().is_some() {
                    return (
                        Phase::Indeterminate,
                        "Couldn’t confirm the result",
                        "The command may have executed. It will not be submitted again automatically.",
                    );
                }
                if failure.reason() == ApiErrorReason::RequestCancelled {
                    return (
                        Phase::Cancelled,
                        "Command ended as cancelled",
                        "Only the server’s recorded result establishes this status.",
                    );
                }
                return (
                    Phase::Failed,
                    "Command failed",
                    "The server recorded a failure. No observed value is used to infer an effect.",
                );
            }
            CommandCompletion::Pending => {}
        }
    }
    match action.tracking {
        CapabilityTracking::Stopped => {
            return (
                Phase::Stopped,
                "Tracking stopped on this device",
                "This does not cancel a command the server has already accepted.",
            );
        }
        CapabilityTracking::Unavailable => {
            return (
                Phase::Unavailable,
                "Live tracking unavailable",
                "The recorded progress is retained. No additional physical effect is inferred.",
            );
        }
        CapabilityTracking::Interrupted => {
            return (
                Phase::Interrupted,
                "Tracking interrupted",
                "The outcome remains unconfirmed. No new submission will be made.",
            );
        }
        CapabilityTracking::Waiting | CapabilityTracking::Live | CapabilityTracking::Complete => {}
    }
    if let Some(operation) = &action.operation {
        return match operation.phase() {
            CommandPhase::Accepted => (
                Phase::Accepted,
                "Command accepted",
                "The durable admission does not establish an effect.",
            ),
            CommandPhase::Dispatched => (
                Phase::Dispatched,
                "Command forwarded",
                "The physical result has not been confirmed.",
            ),
            CommandPhase::Acknowledged => (
                Phase::Acknowledged,
                "Acknowledged · waiting for confirmation",
                "The observed state remains independent of this command.",
            ),
            CommandPhase::Confirmed
            | CommandPhase::Failed
            | CommandPhase::Cancelled
            | CommandPhase::Indeterminate => (
                Phase::Unavailable,
                "Tracking unavailable",
                "No additional confirmation is inferred.",
            ),
        };
    }
    match action.submission {
        CapabilitySubmission::Prepared => (
            Phase::Preparing,
            "Checking command",
            "The gesture is discarded if its view or permissions change.",
        ),
        CapabilitySubmission::Sending => (
            Phase::Sending,
            "Sending",
            "This action is submitted only once.",
        ),
        CapabilitySubmission::Accepted => (
            Phase::Accepted,
            "Command accepted",
            "Tracking the same operation without another submission.",
        ),
        CapabilitySubmission::Rejected => (
            Phase::Refused,
            "Rejected",
            "The server refused this intention. Refresh the view before another gesture.",
        ),
        CapabilitySubmission::Uncertain => (
            Phase::Uncertain,
            "Submission uncertain",
            "The command may have executed. It will not be submitted again automatically.",
        ),
        CapabilitySubmission::NotSent => (
            Phase::Refused,
            "Not sent",
            "The gesture context was no longer valid before submission.",
        ),
        CapabilitySubmission::Closed => (
            Phase::Stopped,
            "Tracking closed",
            "Closing this device’s tracking does not cancel a physical effect.",
        ),
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use buffa::Message;
    use buffa_types::google::protobuf::{Any, Timestamp};
    use home_client_model::api::{CapabilityName, Command, Deadline, Intent, Power, RequestId};
    use home_wire_contract::{
        api_command_operations::admit_command_operation, google::longrunning, home::api::v2 as wire,
    };

    pub(crate) fn snapshot(outcome: Option<wire::CommandOutcome>) -> CapabilitySnapshot {
        let id = RequestId::try_from("12345678-1234-4123-8123-123456789abc").unwrap();
        let target =
            CapabilityName::try_from("homes/synthetic/entities/relay/capabilities/power").unwrap();
        let command = Command::new(
            target.clone(),
            Intent::Power(Power::On),
            Some(id.clone()),
            Deadline::new(1800000000, 0).unwrap(),
        );
        let phase = if outcome == Some(wire::CommandOutcome::Confirmed) {
            wire::CommandPhase::Confirmed
        } else {
            wire::CommandPhase::Acknowledged
        };
        let metadata = wire::CommandMetadata {
            capability: target.as_str().into(),
            request_id: id.as_str().into(),
            create_time: Timestamp::from_unix_secs(1700000000).into(),
            phase: phase.into(),
            live_progress_available: outcome.is_none(),
            ..Default::default()
        };
        let raw = longrunning::Operation {
            name: "homes/synthetic/operations/cmd-test".into(),
            done: outcome.is_some(),
            metadata: Any {
                type_url: "type.googleapis.com/home.api.v2.CommandMetadata".into(),
                value: metadata.encode_to_vec().into(),
                ..Default::default()
            }
            .into(),
            result: outcome.map(|outcome| {
                longrunning::operation::Result::Response(Box::new(Any {
                    type_url: "type.googleapis.com/home.api.v2.ExecuteCapabilityResponse".into(),
                    value: wire::ExecuteCapabilityResponse {
                        capability: target.as_str().into(),
                        outcome: outcome.into(),
                        ..Default::default()
                    }
                    .encode_to_vec()
                    .into(),
                    ..Default::default()
                }))
            }),
            ..Default::default()
        };
        CapabilitySnapshot {
            request_id: id,
            submission: CapabilitySubmission::Accepted,
            tracking: CapabilityTracking::Live,
            operation: Some(admit_command_operation(&raw, &command, None).unwrap()),
            failure: None,
            issue: None,
        }
    }
    #[test]
    fn pending_and_terminal_ack_never_render_confirmation() {
        let pending = snapshot(None);
        assert_eq!(presentation(Some(&pending)).0, Phase::Acknowledged);
        assert_eq!(presentation(Some(&pending)), (Phase::Acknowledged, "", ""));
        let terminal = snapshot(Some(wire::CommandOutcome::AcknowledgedUnverified));
        assert_eq!(presentation(Some(&terminal)).0, Phase::Acknowledged);
        assert!(presentation(Some(&terminal)).1.contains("unverified"));
        let confirmed = snapshot(Some(wire::CommandOutcome::Confirmed));
        assert_eq!(presentation(Some(&confirmed)), (Phase::Confirmed, "", ""));
    }
    #[test]
    fn stopping_tracking_cannot_cancel_an_effect_or_erase_a_recorded_confirmation() {
        let mut pending = snapshot(None);
        pending.tracking = CapabilityTracking::Stopped;
        assert_eq!(presentation(Some(&pending)).0, Phase::Stopped);
        let mut confirmed = snapshot(Some(wire::CommandOutcome::Confirmed));
        confirmed.tracking = CapabilityTracking::Stopped;
        assert_eq!(presentation(Some(&confirmed)), (Phase::Confirmed, "", ""));
    }
    #[test]
    fn routine_progress_is_silent_but_problems_keep_their_feedback() {
        let mut progress = snapshot(None);
        progress.operation = None;
        for submission in [
            CapabilitySubmission::Prepared,
            CapabilitySubmission::Sending,
            CapabilitySubmission::Accepted,
        ] {
            progress.submission = submission;
            let (_, label, detail) = presentation(Some(&progress));
            assert!(label.is_empty() && detail.is_empty());
        }
        for submission in [
            CapabilitySubmission::Rejected,
            CapabilitySubmission::NotSent,
            CapabilitySubmission::Uncertain,
            CapabilitySubmission::Closed,
        ] {
            progress.submission = submission;
            let (_, label, detail) = presentation(Some(&progress));
            assert!(!label.is_empty() && !detail.is_empty());
        }
    }
}
