//! Consuming CalendarService mutations. Each prepared intention has one dispatch.
use crate::AdmissionCheck;
use home_client_model::api_calendars::{
    CalendarEventName, CalendarEventVersion, CalendarName, CalendarVersion,
};
use home_client_session::{AccessLease, SessionCoordinator, SessionFailure};
use home_client_transport::{CalendarTransport, Failure, FailureKind, RpcCode};
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};
use std::{future::Future, time::Duration};
use tokio_util::sync::CancellationToken;

enum Request {
    Create(wire::CreateCalendarRequest, CalendarName),
    Rename(wire::UpdateCalendarRequest, CalendarName),
    Delete(wire::DeleteCalendarRequest, CalendarName),
    CreateEvent(wire::CreateCalendarEventRequest, CalendarName),
    UpdateEvent(wire::UpdateCalendarEventRequest, CalendarEventName),
    DeleteEvent(wire::DeleteCalendarEventRequest, CalendarEventName),
}
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
    HasChildren,
}
pub enum CalendarWriteOutcome {
    NotSent(NotSentReason),
    Committed(Box<ConfirmedMutation>),
    Conflict,
    CommitUnknown(UnknownReason),
    Rejected(Rejection),
    AuthorityTerminated,
}
/// Direct admitted resource result; deletion retains the consumed identity.
pub enum ConfirmedMutation {
    Calendar(admit::Admitted<wire::Calendar>),
    Event(admit::Admitted<wire::CalendarEvent>),
    DeletedCalendar(CalendarName),
    DeletedEvent(CalendarEventName),
}
impl ConfirmedMutation {
    pub fn calendar_version(&self) -> Option<CalendarVersion> {
        match self {
            Self::Calendar(value) => admit::calendar_version(value.wire()).ok(),
            Self::Event(value) => {
                let version = admit::event_version(value.wire()).ok()?;
                Some(CalendarVersion::new(
                    version.name().parent().clone(),
                    version.calendar_etag().clone(),
                ))
            }
            Self::DeletedCalendar(_) | Self::DeletedEvent(_) => None,
        }
    }
}
fn mask(paths: &[&str]) -> buffa_types::google::protobuf::FieldMask {
    buffa_types::google::protobuf::FieldMask {
        paths: paths.iter().map(|value| (*value).to_owned()).collect(),
        ..Default::default()
    }
}
fn editable(
    current: &admit::Admitted<wire::Calendar>,
) -> Result<CalendarVersion, admit::AdmissionError> {
    if !admit::calendar_editable(current) {
        return Err(admit::AdmissionError::ReadOnly);
    }
    admit::calendar_version(current.wire())
}
fn content(mut value: wire::CalendarEvent) -> Result<wire::CalendarEvent, admit::AdmissionError> {
    admit::event_content(&value, true)?;
    value.name.clear();
    value.etag.clear();
    value.calendar_etag.clear();
    value.update_time = Default::default();
    Ok(value)
}
impl PreparedCalendarMutation {
    pub fn create(
        source: &admit::Admitted<wire::CalendarSource>,
        display_name: String,
    ) -> Result<Self, admit::AdmissionError> {
        let source = source.wire();
        if source.materialized
            || source.origin != wire::CalendarOrigin::Local
            || source.mutation_access != wire::CalendarMutationAccess::Allowed
        {
            return Err(admit::AdmissionError::ReadOnly);
        }
        if display_name.is_empty() || display_name.len() > 512 || display_name.contains('\0') {
            return Err(admit::AdmissionError::Invalid);
        }
        let name = CalendarName::try_from(source.calendar.as_str())
            .map_err(|_| admit::AdmissionError::Invalid)?;
        Ok(Self {
            request: Request::Create(
                wire::CreateCalendarRequest {
                    parent: name.home().as_str().into(),
                    calendar: wire::Calendar {
                        source_entity: source.source_entity.clone(),
                        display_name,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
                name,
            ),
        })
    }
    pub fn rename(
        current: &admit::Admitted<wire::Calendar>,
        display_name: String,
    ) -> Result<Self, admit::AdmissionError> {
        let version = editable(current)?;
        if display_name.is_empty() || display_name.len() > 512 || display_name.contains('\0') {
            return Err(admit::AdmissionError::Invalid);
        }
        Ok(Self {
            request: Request::Rename(
                wire::UpdateCalendarRequest {
                    calendar: wire::Calendar {
                        name: version.name().as_str().into(),
                        etag: version.etag().as_str().into(),
                        display_name,
                        ..Default::default()
                    }
                    .into(),
                    update_mask: mask(&["display_name"]).into(),
                    ..Default::default()
                },
                version.name().clone(),
            ),
        })
    }
    pub fn delete(
        current: &admit::Admitted<wire::Calendar>,
        force: bool,
    ) -> Result<Self, admit::AdmissionError> {
        let version = editable(current)?;
        Ok(Self {
            request: Request::Delete(
                wire::DeleteCalendarRequest {
                    name: version.name().as_str().into(),
                    etag: version.etag().as_str().into(),
                    force,
                    ..Default::default()
                },
                version.name().clone(),
            ),
        })
    }
    pub fn create_event(
        current: &admit::Admitted<wire::Calendar>,
        value: wire::CalendarEvent,
    ) -> Result<Self, admit::AdmissionError> {
        let version = editable(current)?;
        let value = content(value)?;
        Ok(Self {
            request: Request::CreateEvent(
                wire::CreateCalendarEventRequest {
                    parent: version.name().as_str().into(),
                    calendar_event: value.into(),
                    ..Default::default()
                },
                version.name().clone(),
            ),
        })
    }
    pub fn update_event(
        current: &admit::Admitted<wire::Calendar>,
        previous: &CalendarEventVersion,
        value: wire::CalendarEvent,
    ) -> Result<Self, admit::AdmissionError> {
        let version = editable(current)?;
        if previous.name().parent() != version.name() {
            return Err(admit::AdmissionError::ForeignResource);
        }
        let mut value = content(value)?;
        value.name = previous.name().as_str().into();
        value.etag = previous.etag().as_str().into();
        Ok(Self {
            request: Request::UpdateEvent(
                wire::UpdateCalendarEventRequest {
                    calendar_event: value.into(),
                    update_mask: mask(&["*"]).into(),
                    ..Default::default()
                },
                previous.name().clone(),
            ),
        })
    }
    pub fn delete_event(
        current: &admit::Admitted<wire::Calendar>,
        previous: &CalendarEventVersion,
    ) -> Result<Self, admit::AdmissionError> {
        let version = editable(current)?;
        if previous.name().parent() != version.name() {
            return Err(admit::AdmissionError::ForeignResource);
        }
        Ok(Self {
            request: Request::DeleteEvent(
                wire::DeleteCalendarEventRequest {
                    name: previous.name().as_str().into(),
                    etag: previous.etag().as_str().into(),
                    ..Default::default()
                },
                previous.name().clone(),
            ),
        })
    }
    /// `admission` checks eligibility until dispatch. `receipt_current` checks
    /// the same operation's session/resource ownership after dispatch; it must
    /// survive a Watch update caused by the mutation itself.
    pub async fn submit(
        self,
        api: &CalendarTransport,
        session: &SessionCoordinator,
        cancel: &CancellationToken,
        admission: AdmissionCheck,
        receipt_current: AdmissionCheck,
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
            || session.is_current(lease.generation) && receipt_current(),
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
enum Reply {
    Calendar(wire::Calendar),
    Event(wire::CalendarEvent),
    Deleted,
}
trait SubmitApi: Sync {
    fn submit(
        &self,
        request: &Request,
    ) -> impl Future<Output = Result<Reply, SubmitFailure>> + Send;
}
struct NativeApi<'a> {
    api: &'a CalendarTransport,
    lease: &'a AccessLease,
}
impl SubmitApi for NativeApi<'_> {
    async fn submit(&self, request: &Request) -> Result<Reply, SubmitFailure> {
        macro_rules! call {
            ($method:ident,$request:expr,$reply:ident) => {{
                let value = self
                    .api
                    .$method(
                        $request.clone(),
                        self.lease.token(),
                        Duration::from_secs(10),
                        &self.lease.cancellation,
                    )
                    .await
                    .map_err(SubmitFailure::Transport)?;
                if !value.__buffa_unknown_fields.is_empty() {
                    return Err(SubmitFailure::Protocol);
                }
                Reply::$reply(value)
            }};
        }
        macro_rules! delete {
            ($method:ident,$request:expr) => {{
                let value = self
                    .api
                    .$method(
                        $request.clone(),
                        self.lease.token(),
                        Duration::from_secs(10),
                        &self.lease.cancellation,
                    )
                    .await
                    .map_err(SubmitFailure::Transport)?;
                if !value.__buffa_unknown_fields.is_empty() {
                    return Err(SubmitFailure::Protocol);
                }
                Reply::Deleted
            }};
        }
        Ok(match request {
            Request::Create(value, _) => call!(create_calendar, value, Calendar),
            Request::Rename(value, _) => call!(update_calendar, value, Calendar),
            Request::Delete(value, _) => delete!(delete_calendar, value),
            Request::CreateEvent(value, _) => call!(create_calendar_event, value, Event),
            Request::UpdateEvent(value, _) => call!(update_calendar_event, value, Event),
            Request::DeleteEvent(value, _) => delete!(delete_calendar_event, value),
        })
    }
}
fn confirm(request: &Request, reply: Reply) -> Option<ConfirmedMutation> {
    match (request, reply) {
        (Request::Create(_, name) | Request::Rename(_, name), Reply::Calendar(value)) => Some(
            ConfirmedMutation::Calendar(admit::admit_calendar(&value, name).ok()?),
        ),
        (Request::CreateEvent(_, parent), Reply::Event(value)) => {
            let version = admit::event_version(&value).ok()?;
            if version.name().parent() != parent {
                return None;
            };
            Some(ConfirmedMutation::Event(
                admit::admit_event(&value, version.name()).ok()?,
            ))
        }
        (Request::UpdateEvent(_, name), Reply::Event(value)) => Some(ConfirmedMutation::Event(
            admit::admit_event(&value, name).ok()?,
        )),
        (Request::Delete(_, name), Reply::Deleted) => {
            Some(ConfirmedMutation::DeletedCalendar(name.clone()))
        }
        (Request::DeleteEvent(_, name), Reply::Deleted) => {
            Some(ConfirmedMutation::DeletedEvent(name.clone()))
        }
        (Request::Create(..) | Request::Rename(..), Reply::Event(_) | Reply::Deleted)
        | (
            Request::CreateEvent(..) | Request::UpdateEvent(..),
            Reply::Calendar(_) | Reply::Deleted,
        )
        | (Request::Delete(..) | Request::DeleteEvent(..), Reply::Calendar(_) | Reply::Event(_)) => {
            None
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
    receipt_current: impl Fn() -> bool,
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
        // Watch may already contain this mutation's new revision. Its freshness
        // and visual generation admit a new dispatch, not a receipt belonging
        // to an operation that has already been dispatched.
        if cancel.is_cancelled() || access.is_cancelled() || !receipt_current() {
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
            Ok(reply) => match confirm(&prepared.request, reply) {
                Some(value) => CalendarWriteOutcome::Committed(Box::new(value)),
                None => CalendarWriteOutcome::CommitUnknown(UnknownReason::InvalidReceipt),
            },
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
    let detail = match failure.api_failure() {
        Ok(Some(value)) => value,
        Ok(None) => {
            return CalendarWriteOutcome::CommitUnknown(UnknownReason::Transport(failure.kind()));
        }
        Err(_) => return CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol),
    };
    use wire::ApiErrorReason as R;
    match detail.reason() {
        R::AuthenticationRequired | R::AccessDenied => CalendarWriteOutcome::AuthorityTerminated,
        R::EtagMismatch | R::ResourceAlreadyExists => CalendarWriteOutcome::Conflict,
        R::ValidationFailed => CalendarWriteOutcome::Rejected(Rejection::Invalid),
        R::ResourceNotFound => CalendarWriteOutcome::Rejected(Rejection::NotFound),
        R::SourceReadOnly => CalendarWriteOutcome::Rejected(Rejection::ReadOnly),
        R::StatePreconditionFailed => CalendarWriteOutcome::Rejected(Rejection::HasChildren),
        R::RateLimited | R::QueryBudgetExceeded | R::RequestTooLarge => {
            CalendarWriteOutcome::Rejected(Rejection::Capacity)
        }
        R::CommitOutcomeUnknown
        | R::EffectOutcomeUnknown
        | R::ServiceUnavailable
        | R::RequestCancelled
        | R::RequestDeadlineExceeded
        | R::InternalFailure
        | R::StoredDataIntegrity => {
            CalendarWriteOutcome::CommitUnknown(UnknownReason::Transport(failure.kind()))
        }
        R::RequestIdReused
        | R::ConfigChanged
        | R::TopologyChanged
        | R::CapabilityUnavailable
        | R::ConfigRestartRequired
        | R::ResyncRequired
        | R::StreamCapacity
        | R::MethodNotSupported
        | R::Unspecified => CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol),
    }
}
#[cfg(test)]
mod tests;
