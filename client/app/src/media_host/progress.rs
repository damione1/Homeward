//! Presentation of admitted command facts; ACK never maps to Confirmed.
use crate::ProActionPhase;
use home_client_actions::capabilities::{
    CapabilitySnapshot, CapabilitySubmission, CapabilityTracking,
};
use home_wire_contract::api_command_operations::{CommandCompletion, CommandOutcome, CommandPhase};
pub(super) fn presentation(
    value: Option<&CapabilitySnapshot>,
) -> (ProActionPhase, &'static str, &'static str) {
    crate::device_view::progress::quiet(facts(value), value)
}
fn facts(value: Option<&CapabilitySnapshot>) -> (ProActionPhase, &'static str, &'static str) {
    let Some(value) = value else {
        return (ProActionPhase::Idle, "", "");
    };
    match value.submission {
        CapabilitySubmission::Prepared => (ProActionPhase::Preparing, "Preparing intention", ""),
        CapabilitySubmission::Sending => {
            (ProActionPhase::Sending, "Sending intention", "One attempt")
        }
        CapabilitySubmission::NotSent => (
            ProActionPhase::Refused,
            "Not sent",
            "The admitted context changed",
        ),
        CapabilitySubmission::Rejected => (
            ProActionPhase::Refused,
            "Rejected",
            "The server refused this intention",
        ),
        CapabilitySubmission::Uncertain => (
            ProActionPhase::Uncertain,
            "Outcome unknown",
            "No replay; reconcile the server state",
        ),
        CapabilitySubmission::Closed => (
            ProActionPhase::Unavailable,
            "Context closed",
            "Private progress was removed",
        ),
        CapabilitySubmission::Accepted => {
            if value.tracking == CapabilityTracking::Stopped {
                return (
                    ProActionPhase::Stopped,
                    "Tracking stopped",
                    "This does not cancel a physical command",
                );
            }
            if value.tracking == CapabilityTracking::Interrupted {
                return (
                    ProActionPhase::Interrupted,
                    "Tracking interrupted",
                    "Outcome remains unconfirmed; no replay",
                );
            }
            if value.tracking == CapabilityTracking::Unavailable {
                return (
                    ProActionPhase::Unavailable,
                    "Live progress unavailable",
                    "The durable intention remains; no replay",
                );
            }
            let Some(operation) = value.operation.as_ref() else {
                return (ProActionPhase::Unavailable, "Progress unavailable", "");
            };
            match operation.completion() {
                CommandCompletion::Success {
                    outcome: CommandOutcome::Confirmed,
                    ..
                } => (
                    ProActionPhase::Confirmed,
                    "Confirmed",
                    "Qualified native confirmation",
                ),
                CommandCompletion::Success {
                    outcome: CommandOutcome::AcknowledgedUnverified,
                    ..
                } => (
                    ProActionPhase::AcknowledgedUnverified,
                    "Acknowledged",
                    "Physical result not established",
                ),
                CommandCompletion::Failure(_) => match operation.phase() {
                    CommandPhase::Failed => (
                        ProActionPhase::Failed,
                        "Failed",
                        "Canonical terminal server result",
                    ),
                    CommandPhase::Cancelled => (
                        ProActionPhase::Cancelled,
                        "Cancelled",
                        "Cancellation established before effect commitment",
                    ),
                    CommandPhase::Indeterminate => (
                        ProActionPhase::Indeterminate,
                        "Outcome indeterminate",
                        "Reconciliation only; no replay",
                    ),
                    CommandPhase::Accepted
                    | CommandPhase::Dispatched
                    | CommandPhase::Acknowledged
                    | CommandPhase::Confirmed => {
                        (ProActionPhase::Unavailable, "Incompatible progress", "")
                    }
                },
                CommandCompletion::Pending => match operation.phase() {
                    CommandPhase::Accepted => (
                        ProActionPhase::Accepted,
                        "Accepted",
                        "Durable intention; effect not established",
                    ),
                    CommandPhase::Dispatched => (
                        ProActionPhase::Dispatched,
                        "Dispatched",
                        "Effect commitment crossed",
                    ),
                    CommandPhase::Acknowledged => (
                        ProActionPhase::Acknowledged,
                        "Acknowledged",
                        "Physical result not established",
                    ),
                    CommandPhase::Confirmed
                    | CommandPhase::Failed
                    | CommandPhase::Cancelled
                    | CommandPhase::Indeterminate => {
                        (ProActionPhase::Unavailable, "Incompatible progress", "")
                    }
                },
            }
        }
    }
}
