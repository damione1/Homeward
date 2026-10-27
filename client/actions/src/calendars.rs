//! Consuming Calendar mutations. One opportunity, one dispatch and no replay.
use crate::AdmissionCheck;
use buffa::EnumValue;
use home_client_model::calendars::{CalendarRevision, EventRevision};
use home_client_session::{AccessLease, SessionCoordinator, SessionFailure};
use home_client_transport::{CalendarTransport, Failure, FailureKind, RpcCode};
use home_wire_contract::{calendars_contract as admit, home::calendars::v1 as wire};
use std::{future::Future, time::Duration};
use tokio_util::sync::CancellationToken;

enum Request {
    Calendar(Box<admit::Admitted<wire::MutateCalendarRequest>>),
    Event(
        Box<admit::Admitted<wire::MutateEventRequest>>,
        CalendarRevision,
    ),
}
/// Submitting consumes the prepared intention, even if its outcome is unknown.
pub struct PreparedCalendarMutation {
    request: Request,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NotSentReason {
    Cancelled,
    AdmissionChanged,
    Session(SessionFailure),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum UnknownReason {
    Cancelled,
    Superseded,
    Transport(FailureKind),
    Protocol,
    InvalidReceipt,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Rejection {
    Invalid,
    Denied,
    ReadOnly,
    NotFound,
    Capacity,
    Unavailable,
}
pub enum CalendarWriteOutcome {
    NotSent(NotSentReason),
    Committed(Box<admit::Admitted<wire::MutationReceipt>>),
    Conflict,
    CommitUnknown(UnknownReason),
    CommittedResponseWithheld,
    Rejected(Rejection),
    AuthorityTerminated,
}
impl PreparedCalendarMutation {
    pub fn calendar(
        current: &admit::Admitted<wire::Calendar>,
        request: wire::MutateCalendarRequest,
    ) -> Result<Self, admit::AdmissionError> {
        Ok(Self {
            request: Request::Calendar(Box::new(admit::admit_calendar_mutation(
                &request, current,
            )?)),
        })
    }
    pub fn event(
        current: &admit::Admitted<wire::Calendar>,
        previous: &EventRevision,
        request: wire::MutateEventRequest,
    ) -> Result<Self, admit::AdmissionError> {
        let id = admit::calendar_id(&current.wire().id).ok_or(admit::AdmissionError::Invalid)?;
        let calendar = admit::calendar_revision(&current.wire().revision, &id)
            .ok_or(admit::AdmissionError::Invalid)?;
        Ok(Self {
            request: Request::Event(
                Box::new(admit::admit_event_mutation(&request, current, previous)?),
                calendar,
            ),
        })
    }
    pub async fn submit(
        self,
        api: &CalendarTransport,
        session: &SessionCoordinator,
        cancel: &CancellationToken,
        admission: AdmissionCheck,
    ) -> CalendarWriteOutcome {
        if cancel.is_cancelled() {
            return CalendarWriteOutcome::NotSent(NotSentReason::Cancelled);
        }
        if !admission() {
            return CalendarWriteOutcome::NotSent(NotSentReason::AdmissionChanged);
        }
        let lease = match session.access(cancel).await {
            Ok(lease) => lease,
            Err(error) => return CalendarWriteOutcome::NotSent(NotSentReason::Session(error)),
        };
        let result = execute(
            &NativeApi { api, lease: &lease },
            self,
            cancel,
            &lease.cancellation,
            || session.is_current(lease.generation) && admission(),
        )
        .await;
        if let Some(failure) = result.failure {
            let _ = session.observe_failure(lease.generation, &failure).await;
        }
        result.outcome
    }
}
enum SubmitFailure {
    Transport(Failure),
    Protocol,
}
trait SubmitApi: Sync {
    fn submit(
        &self,
        request: &Request,
    ) -> impl Future<Output = Result<wire::MutationReceipt, SubmitFailure>> + Send;
}
struct NativeApi<'a> {
    api: &'a CalendarTransport,
    lease: &'a AccessLease,
}
impl SubmitApi for NativeApi<'_> {
    async fn submit(&self, request: &Request) -> Result<wire::MutationReceipt, SubmitFailure> {
        macro_rules! call {
            ($method:ident,$request:expr) => {{
                let response = self
                    .api
                    .$method(
                        $request,
                        self.lease.token(),
                        Duration::from_secs(10),
                        &self.lease.cancellation,
                    )
                    .await
                    .map_err(SubmitFailure::Transport)?;
                if !response.__buffa_unknown_fields.is_empty() {
                    return Err(SubmitFailure::Protocol);
                }
                response
                    .receipt
                    .as_option()
                    .cloned()
                    .ok_or(SubmitFailure::Protocol)
            }};
        }
        match request {
            Request::Calendar(request) => call!(mutate_calendar, request.wire().clone()),
            Request::Event(request, _) => call!(mutate_event, request.wire().clone()),
        }
    }
}
struct Submission {
    outcome: CalendarWriteOutcome,
    failure: Option<Failure>,
}
async fn execute(
    api: &impl SubmitApi,
    prepared: PreparedCalendarMutation,
    cancel: &CancellationToken,
    access: &CancellationToken,
    admission: impl Fn() -> bool,
) -> Submission {
    let outcome = if cancel.is_cancelled() || access.is_cancelled() {
        CalendarWriteOutcome::NotSent(NotSentReason::Cancelled)
    } else if !admission() {
        CalendarWriteOutcome::NotSent(NotSentReason::AdmissionChanged)
    } else {
        let response = tokio::select! {biased;
         ()=cancel.cancelled()=>return Submission{outcome:CalendarWriteOutcome::CommitUnknown(UnknownReason::Cancelled),failure:None},
         ()=access.cancelled()=>return Submission{outcome:CalendarWriteOutcome::CommitUnknown(UnknownReason::Superseded),failure:None},
         response=api.submit(&prepared.request)=>response,
        };
        if cancel.is_cancelled() || access.is_cancelled() || !admission() {
            let failure = match response {
                Err(SubmitFailure::Transport(failure)) => Some(failure),
                Ok(_) | Err(SubmitFailure::Protocol) => None,
            };
            return Submission {
                outcome: CalendarWriteOutcome::CommitUnknown(UnknownReason::Superseded),
                failure,
            };
        }
        match response {
            Ok(receipt) => {
                let confirmed = match &prepared.request {
                    Request::Calendar(request) => admit::admit_calendar_receipt(&receipt, request),
                    Request::Event(request, calendar) => {
                        admit::admit_event_receipt(&receipt, request, calendar)
                    }
                };
                match confirmed {
                    Ok(receipt) => CalendarWriteOutcome::Committed(Box::new(receipt)),
                    Err(_) => CalendarWriteOutcome::CommitUnknown(UnknownReason::InvalidReceipt),
                }
            }
            Err(SubmitFailure::Protocol) => {
                CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol)
            }
            Err(SubmitFailure::Transport(failure)) => {
                return Submission {
                    outcome: classify(&failure),
                    failure: Some(failure),
                };
            }
        }
    };
    Submission {
        outcome,
        failure: None,
    }
}
fn classify(failure: &Failure) -> CalendarWriteOutcome {
    if failure.kind() == FailureKind::Rpc(RpcCode::Unauthenticated)
        || !matches!(failure.authorization_termination(), Ok(None))
    {
        return CalendarWriteOutcome::AuthorityTerminated;
    }
    let detail = match failure.calendar_error() {
        Ok(Some(detail)) if detail.__buffa_unknown_fields.is_empty() => detail,
        Ok(None) => {
            return CalendarWriteOutcome::CommitUnknown(UnknownReason::Transport(failure.kind()));
        }
        Ok(Some(_)) | Err(_) => {
            return CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol);
        }
    };
    use wire::ErrorCode as Code;
    let expected = match detail.code {
        EnumValue::Known(Code::Invalid) => RpcCode::InvalidArgument,
        EnumValue::Known(Code::Denied) => RpcCode::PermissionDenied,
        EnumValue::Known(Code::NotFound) => RpcCode::NotFound,
        EnumValue::Known(
            Code::Conflict | Code::CommitUnknown | Code::CommittedResponseWithheld,
        ) => RpcCode::Aborted,
        EnumValue::Known(Code::ReadOnly | Code::StaleCursor) => RpcCode::FailedPrecondition,
        EnumValue::Known(Code::Capacity) => RpcCode::ResourceExhausted,
        EnumValue::Known(Code::Unavailable) => RpcCode::Unavailable,
        EnumValue::Known(Code::Unspecified) | EnumValue::Unknown(_) => {
            return CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol);
        }
    };
    if failure.kind() != FailureKind::Rpc(expected) {
        return CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol);
    }
    match detail.code {
        EnumValue::Known(Code::Invalid) => CalendarWriteOutcome::Rejected(Rejection::Invalid),
        EnumValue::Known(Code::Denied) => CalendarWriteOutcome::Rejected(Rejection::Denied),
        EnumValue::Known(Code::NotFound) => CalendarWriteOutcome::Rejected(Rejection::NotFound),
        EnumValue::Known(Code::Conflict) => CalendarWriteOutcome::Conflict,
        EnumValue::Known(Code::ReadOnly) => CalendarWriteOutcome::Rejected(Rejection::ReadOnly),
        EnumValue::Known(Code::Capacity) => CalendarWriteOutcome::Rejected(Rejection::Capacity),
        EnumValue::Known(Code::Unavailable) => {
            CalendarWriteOutcome::Rejected(Rejection::Unavailable)
        }
        EnumValue::Known(Code::CommitUnknown) => {
            CalendarWriteOutcome::CommitUnknown(UnknownReason::Transport(failure.kind()))
        }
        EnumValue::Known(Code::CommittedResponseWithheld) => {
            CalendarWriteOutcome::CommittedResponseWithheld
        }
        EnumValue::Known(Code::Unspecified | Code::StaleCursor) | EnumValue::Unknown(_) => {
            CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol)
        }
    }
}
#[cfg(test)]
mod tests;
