//! Consuming single-attempt TaskService writes. Drafts never become an offline queue.
use crate::AdmissionCheck;
use home_client_model::{
    api::HomeName,
    api_tasks::{
        TaskContent, TaskListName, TaskListResource, TaskListRevision, TaskName, TaskResource,
        TaskRevision, TaskText,
    },
    lists::ItemKey,
};
use home_client_session::{AccessLease, SessionCoordinator, SessionFailure};
use home_client_transport::{Failure, FailureKind, RpcCode, TaskTransport};
use home_wire_contract::{home::api::v2 as wire, tasks_contract};
use std::{future::Future, time::Duration};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PrepareFailure {
    InvalidName,
    InvalidPayload,
    MismatchedIdentity,
    InvalidDestination,
}
#[derive(Clone)]
pub struct ItemTarget {
    list: TaskListRevision,
    item: TaskRevision,
}
impl ItemTarget {
    pub fn new(list: TaskListRevision, item: TaskRevision) -> Result<Self, PrepareFailure> {
        if item.name().parent() != list.name() {
            return Err(PrepareFailure::MismatchedIdentity);
        }
        Ok(Self { list, item })
    }
    pub fn key(&self) -> &ItemKey {
        self.item.item()
    }
    pub fn list_revision(&self) -> &TaskListRevision {
        &self.list
    }
    pub fn item_revision(&self) -> &TaskRevision {
        &self.item
    }
}
pub enum MoveDestination {
    AtStart,
    After(ItemKey),
}
pub enum ListWriteIntent {
    Create {
        home: HomeName,
        name: String,
    },
    Rename {
        revision: TaskListRevision,
        name: String,
    },
    /// Cascade is an explicit caller decision. The current UI always uses false.
    DeleteList {
        revision: TaskListRevision,
        force: bool,
    },
    Add {
        revision: TaskListRevision,
        payload: TaskContent,
    },
    Edit {
        target: ItemTarget,
        payload: TaskContent,
    },
    DeleteItem {
        target: ItemTarget,
    },
    Complete {
        target: ItemTarget,
        completed: bool,
    },
    Move {
        target: ItemTarget,
        destination: MoveDestination,
    },
}
enum Request {
    Create(wire::CreateTaskListRequest),
    Rename(wire::UpdateTaskListRequest),
    DeleteList(wire::DeleteTaskListRequest),
    Add(wire::AppendTaskRequest),
    Edit(wire::UpdateTaskRequest),
    DeleteItem(wire::DeleteTaskRequest),
    Complete(wire::UpdateTaskRequest),
    Move(wire::MoveTaskRequest),
}
enum Expected {
    CreatedList(HomeName),
    List(TaskListName),
    AddedItem(TaskListName),
    Item(TaskName),
    DeletedList(TaskListName),
    DeletedItem(TaskName),
}
/// Not Clone: submission consumes this single opportunity, even on uncertainty.
pub struct PreparedListMutation {
    request: Request,
    expected: Expected,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NotSentReason {
    Cancelled,
    AdmissionChanged,
    Session(SessionFailure),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WriteIssue {
    Transport(FailureKind),
    UnsupportedResponse,
    InvalidReceipt,
    Cancelled,
    Superseded,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Rejection {
    Invalid,
    NotFound,
    Capacity,
    HasChildren,
}
pub enum ListWriteOutcome {
    NotSent(NotSentReason),
    Committed(Box<ConfirmedMutation>),
    Conflict,
    CommitUnknown(WriteIssue),
    Rejected(Rejection),
    AuthorityTerminated,
}
/// Direct admitted TaskService result, with deletion identity retained from the consumed request.
pub enum ConfirmedMutation {
    List(TaskListResource),
    Task(TaskResource),
    DeletedList(TaskListName),
    DeletedTask(TaskName),
}
fn mask(paths: &[&str]) -> buffa_types::google::protobuf::FieldMask {
    buffa_types::google::protobuf::FieldMask {
        paths: paths.iter().map(|path| (*path).to_owned()).collect(),
        ..Default::default()
    }
}
impl PreparedListMutation {
    pub fn new(intent: ListWriteIntent) -> Result<Self, PrepareFailure> {
        let (request, expected) = match intent {
            ListWriteIntent::Create { home, name } => {
                TaskText::try_from(name.as_str()).map_err(|_| PrepareFailure::InvalidName)?;
                (
                    Request::Create(wire::CreateTaskListRequest {
                        parent: home.as_str().to_owned(),
                        task_list: wire::TaskList {
                            display_name: name,
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    }),
                    Expected::CreatedList(home),
                )
            }
            ListWriteIntent::Rename { revision, name } => {
                TaskText::try_from(name.as_str()).map_err(|_| PrepareFailure::InvalidName)?;
                (
                    Request::Rename(wire::UpdateTaskListRequest {
                        task_list: wire::TaskList {
                            name: revision.name().as_str().to_owned(),
                            etag: revision.etag().as_str().to_owned(),
                            display_name: name,
                            ..Default::default()
                        }
                        .into(),
                        update_mask: mask(&["display_name"]).into(),
                        ..Default::default()
                    }),
                    Expected::List(revision.name().clone()),
                )
            }
            ListWriteIntent::DeleteList { revision, force } => (
                Request::DeleteList(wire::DeleteTaskListRequest {
                    name: revision.name().as_str().to_owned(),
                    etag: revision.etag().as_str().to_owned(),
                    force,
                    ..Default::default()
                }),
                Expected::DeletedList(revision.name().clone()),
            ),
            ListWriteIntent::Add { revision, payload } => (
                Request::Add(wire::AppendTaskRequest {
                    name: revision.name().as_str().to_owned(),
                    etag: revision.etag().as_str().to_owned(),
                    task: tasks_contract::encode_append_content(&payload).into(),
                    ..Default::default()
                }),
                Expected::AddedItem(revision.name().clone()),
            ),
            ListWriteIntent::Edit { target, payload } => {
                let mut task = tasks_contract::encode_task_content(&payload);
                task.name = target.item.name().as_str().to_owned();
                task.etag = target.item.etag().as_str().to_owned();
                (
                    Request::Edit(wire::UpdateTaskRequest {
                        task: task.into(),
                        update_mask: mask(&[
                            "summary",
                            "completed",
                            "description",
                            "due_date",
                            "due_instant",
                        ])
                        .into(),
                        ..Default::default()
                    }),
                    Expected::Item(target.item.name().clone()),
                )
            }
            ListWriteIntent::Complete { target, completed } => (
                Request::Complete(wire::UpdateTaskRequest {
                    task: wire::Task {
                        name: target.item.name().as_str().to_owned(),
                        etag: target.item.etag().as_str().to_owned(),
                        completed,
                        ..Default::default()
                    }
                    .into(),
                    update_mask: mask(&["completed"]).into(),
                    ..Default::default()
                }),
                Expected::Item(target.item.name().clone()),
            ),
            ListWriteIntent::DeleteItem { target } => (
                Request::DeleteItem(wire::DeleteTaskRequest {
                    name: target.item.name().as_str().to_owned(),
                    etag: target.item.etag().as_str().to_owned(),
                    ..Default::default()
                }),
                Expected::DeletedItem(target.item.name().clone()),
            ),
            ListWriteIntent::Move {
                target,
                destination,
            } => {
                let destination = match destination {
                    MoveDestination::AtStart => {
                        wire::move_task_request::Destination::First(Default::default())
                    }
                    MoveDestination::After(key) => {
                        if key.list() != target.key().list() || &key == target.key() {
                            return Err(PrepareFailure::InvalidDestination);
                        }
                        wire::move_task_request::Destination::Predecessor(
                            TaskName::new(target.item.name().parent().clone(), key.uid().clone())
                                .as_str()
                                .to_owned(),
                        )
                    }
                };
                (
                    Request::Move(wire::MoveTaskRequest {
                        name: target.item.name().as_str().to_owned(),
                        etag: target.item.etag().as_str().to_owned(),
                        destination: Some(destination),
                        ..Default::default()
                    }),
                    Expected::Item(target.item.name().clone()),
                )
            }
        };
        Ok(Self { request, expected })
    }
    /// The session coordinator is the only credential owner. Admission is
    /// checked again after waiting for access and after the single RPC result.
    pub async fn submit(
        self,
        transport: &TaskTransport,
        session: &SessionCoordinator,
        cancel: &CancellationToken,
        admission: AdmissionCheck,
    ) -> ListWriteOutcome {
        if cancel.is_cancelled() {
            return ListWriteOutcome::NotSent(NotSentReason::Cancelled);
        }
        if !admission() {
            return ListWriteOutcome::NotSent(NotSentReason::AdmissionChanged);
        }
        let lease = match session.access(cancel).await {
            Ok(lease) => lease,
            Err(error) => return ListWriteOutcome::NotSent(NotSentReason::Session(error)),
        };
        let result = execute(
            &NativeApi {
                transport,
                lease: &lease,
            },
            self,
            cancel,
            &lease.cancellation,
            || session.is_current(lease.generation) && admission(),
        )
        .await;
        if let Some(failure) = &result.failure {
            let _ = session.observe_failure(lease.generation, failure).await;
        }
        result.outcome
    }
}

enum Reply {
    List(wire::TaskList),
    Task(wire::Task),
    Deleted,
}
enum SubmitFailure {
    Transport(Failure),
    Protocol,
}
trait SubmitApi: Sync {
    fn submit(&self, request: Request)
    -> impl Future<Output = Result<Reply, SubmitFailure>> + Send;
}
struct NativeApi<'a> {
    transport: &'a TaskTransport,
    lease: &'a AccessLease,
}
impl SubmitApi for NativeApi<'_> {
    async fn submit(&self, request: Request) -> Result<Reply, SubmitFailure> {
        macro_rules! call {
            ($method:ident,$request:expr) => {
                self.transport
                    .$method(
                        $request,
                        self.lease.token(),
                        Duration::from_secs(10),
                        &self.lease.cancellation,
                    )
                    .await
                    .map_err(SubmitFailure::Transport)?
            };
        }
        let reply = match request {
            Request::Create(request) => Reply::List(call!(create_task_list, request)),
            Request::Rename(request) => Reply::List(call!(update_task_list, request)),
            Request::Edit(request) | Request::Complete(request) => {
                Reply::Task(call!(update_task, request))
            }
            Request::Move(request) => Reply::Task(call!(move_task, request)),
            Request::Add(request) => {
                let response = call!(append_task, request);
                if !response.__buffa_unknown_fields.is_empty() {
                    return Err(SubmitFailure::Protocol);
                }
                Reply::Task(
                    response
                        .task
                        .as_option()
                        .cloned()
                        .ok_or(SubmitFailure::Protocol)?,
                )
            }
            Request::DeleteList(request) => {
                let response = call!(delete_task_list, request);
                if !response.__buffa_unknown_fields.is_empty() {
                    return Err(SubmitFailure::Protocol);
                }
                Reply::Deleted
            }
            Request::DeleteItem(request) => {
                let response = call!(delete_task, request);
                if !response.__buffa_unknown_fields.is_empty() {
                    return Err(SubmitFailure::Protocol);
                }
                Reply::Deleted
            }
        };
        Ok(reply)
    }
}
struct Submission {
    outcome: ListWriteOutcome,
    failure: Option<Failure>,
}
async fn execute(
    api: &impl SubmitApi,
    prepared: PreparedListMutation,
    cancel: &CancellationToken,
    access: &CancellationToken,
    admission: impl Fn() -> bool,
) -> Submission {
    let outcome = if cancel.is_cancelled() || access.is_cancelled() {
        ListWriteOutcome::NotSent(NotSentReason::Cancelled)
    } else if !admission() {
        ListWriteOutcome::NotSent(NotSentReason::AdmissionChanged)
    } else {
        let response = tokio::select! { biased;
            () = cancel.cancelled() => return Submission { outcome: ListWriteOutcome::CommitUnknown(WriteIssue::Cancelled), failure: None },
            () = access.cancelled() => return Submission { outcome: ListWriteOutcome::CommitUnknown(WriteIssue::Superseded), failure: None },
            response = api.submit(prepared.request) => response,
        };
        if cancel.is_cancelled() || access.is_cancelled() || !admission() {
            let failure = match response {
                Err(SubmitFailure::Transport(failure)) => Some(failure),
                Ok(_) | Err(SubmitFailure::Protocol) => None,
            };
            return Submission {
                outcome: ListWriteOutcome::CommitUnknown(WriteIssue::Superseded),
                failure,
            };
        }
        match response {
            Ok(receipt) => match confirm(prepared.expected, receipt) {
                Some(confirmed) => ListWriteOutcome::Committed(Box::new(confirmed)),
                None => ListWriteOutcome::CommitUnknown(WriteIssue::InvalidReceipt),
            },
            Err(SubmitFailure::Protocol) => {
                ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse)
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

fn confirm(expected: Expected, reply: Reply) -> Option<ConfirmedMutation> {
    match (expected, reply) {
        (Expected::CreatedList(home), Reply::List(value)) => {
            let value = tasks_contract::parse_task_list(&value).ok()?;
            (value.name().home() == &home).then_some(ConfirmedMutation::List(value))
        }
        (Expected::List(name), Reply::List(value)) => {
            let value = tasks_contract::parse_task_list(&value).ok()?;
            (value.name() == &name).then_some(ConfirmedMutation::List(value))
        }
        (Expected::AddedItem(parent), Reply::Task(value)) => {
            let value = tasks_contract::parse_task(&value).ok()?;
            (value.name().parent() == &parent).then_some(ConfirmedMutation::Task(value))
        }
        (Expected::Item(name), Reply::Task(value)) => {
            let value = tasks_contract::parse_task(&value).ok()?;
            (value.name() == &name).then_some(ConfirmedMutation::Task(value))
        }
        (Expected::DeletedList(name), Reply::Deleted) => Some(ConfirmedMutation::DeletedList(name)),
        (Expected::DeletedItem(name), Reply::Deleted) => Some(ConfirmedMutation::DeletedTask(name)),
        (Expected::CreatedList(_) | Expected::List(_), Reply::Task(_) | Reply::Deleted)
        | (Expected::AddedItem(_) | Expected::Item(_), Reply::List(_) | Reply::Deleted)
        | (Expected::DeletedList(_) | Expected::DeletedItem(_), Reply::List(_) | Reply::Task(_)) => {
            None
        }
    }
}
fn classify(failure: &Failure) -> ListWriteOutcome {
    if failure.kind() == FailureKind::Rpc(RpcCode::Unauthenticated) {
        return ListWriteOutcome::AuthorityTerminated;
    }
    let detail = match failure.api_failure() {
        Ok(Some(detail)) => detail,
        Ok(None) => return ListWriteOutcome::CommitUnknown(WriteIssue::Transport(failure.kind())),
        Err(_) => return ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse),
    };
    use home_wire_contract::home::api::v2::ApiErrorReason as R;
    match detail.reason() {
        R::AuthenticationRequired | R::AccessDenied | R::AuthorityUnavailable => {
            ListWriteOutcome::AuthorityTerminated
        }
        R::EtagMismatch => ListWriteOutcome::Conflict,
        R::ValidationFailed => ListWriteOutcome::Rejected(Rejection::Invalid),
        R::ResourceNotFound => ListWriteOutcome::Rejected(Rejection::NotFound),
        R::RateLimited | R::QueryBudgetExceeded | R::RequestTooLarge => {
            ListWriteOutcome::Rejected(Rejection::Capacity)
        }
        R::CommitOutcomeUnknown
        | R::EffectOutcomeUnknown
        | R::ServiceUnavailable
        | R::RequestCancelled
        | R::RequestDeadlineExceeded
        | R::InternalFailure
        | R::StoredDataIntegrity => {
            ListWriteOutcome::CommitUnknown(WriteIssue::Transport(failure.kind()))
        }
        R::StatePreconditionFailed => ListWriteOutcome::Rejected(Rejection::HasChildren),
        R::ResourceAlreadyExists
        | R::RequestIdReused
        | R::ConfigChanged
        | R::TopologyChanged
        | R::CapabilityUnavailable
        | R::SourceReadOnly
        | R::ConfigRestartRequired
        | R::ResyncRequired
        | R::StreamCapacity
        | R::MethodNotSupported
        | R::Unspecified => ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse),
    }
}

#[cfg(test)]
mod tests;
