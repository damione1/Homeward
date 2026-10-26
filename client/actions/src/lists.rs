//! Consuming, single-attempt ListService writes. A draft or conflict never
//! becomes an offline queue; only an admitted matching receipt means committed.
use crate::AdmissionCheck;
use buffa::EnumValue;
use home_client_model::lists::{ItemKey, ItemRevision, ListId, ListRevision};
use home_client_session::{AccessLease, SessionCoordinator, SessionFailure};
use home_client_transport::{Failure, FailureKind, ListTransport, RpcCode};
use home_wire_contract::{home::lists::v1 as wire, lists_contract};
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
    list: ListRevision,
    item: ItemRevision,
}
impl ItemTarget {
    pub fn new(list: ListRevision, item: ItemRevision) -> Result<Self, PrepareFailure> {
        if list.list() != item.item().list() {
            return Err(PrepareFailure::MismatchedIdentity);
        }
        Ok(Self { list, item })
    }
    pub fn key(&self) -> &ItemKey {
        self.item.item()
    }
    pub fn list_revision(&self) -> &ListRevision {
        &self.list
    }
    pub fn item_revision(&self) -> &ItemRevision {
        &self.item
    }
}

pub enum MoveDestination {
    AtStart,
    After(ItemKey),
}
pub enum ListWriteIntent {
    Create {
        name: String,
    },
    Rename {
        revision: ListRevision,
        name: String,
    },
    DeleteList {
        revision: ListRevision,
    },
    Add {
        revision: ListRevision,
        payload: wire::ItemPayload,
    },
    Edit {
        target: ItemTarget,
        payload: wire::ItemPayload,
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
    Create(wire::CreateListRequest),
    Rename(wire::RenameListRequest),
    DeleteList(wire::DeleteListRequest),
    Add(wire::AddItemRequest),
    Edit(wire::EditItemRequest),
    DeleteItem(wire::DeleteItemRequest),
    Complete(wire::SetCompletedRequest),
    Move(wire::MoveItemRequest),
}
enum Expected {
    CreatedList,
    List(ListRevision),
    AddedItem(ListRevision),
    Item(ItemTarget),
}

/// Not Clone: submitting consumes this opportunity, including on cancellation
/// or an uncertain result. An explicit new user action needs new admission.
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
    Denied,
    NotFound,
    Capacity,
    Unavailable,
    Closed,
}

pub enum ListWriteOutcome {
    NotSent(NotSentReason),
    Committed(ConfirmedMutation),
    Conflict,
    CommitUnknown(WriteIssue),
    CommittedResponseWithheld,
    Rejected(Rejection),
    AuthorityTerminated,
}
pub struct ConfirmedMutation {
    receipt: Box<wire::MutationReceipt>,
}
impl ConfirmedMutation {
    pub fn receipt(&self) -> &wire::MutationReceipt {
        &self.receipt
    }
}

fn id(value: &ListId) -> wire::ListId {
    wire::ListId {
        value: value.as_str().into(),
        ..Default::default()
    }
}
fn list_revision(value: &ListRevision) -> wire::ListRevision {
    wire::ListRevision {
        list_id: id(value.list()).into(),
        number: value.number().get(),
        ..Default::default()
    }
}
fn item_revision(value: &ItemRevision) -> wire::ItemRevision {
    wire::ItemRevision {
        list_id: id(value.item().list()).into(),
        item_uid: wire::ItemUid {
            value: value.item().uid().as_str().into(),
            ..Default::default()
        }
        .into(),
        number: value.number().get(),
        ..Default::default()
    }
}
fn uid(target: &ItemTarget) -> wire::ItemUid {
    wire::ItemUid {
        value: target.key().uid().as_str().into(),
        ..Default::default()
    }
}

impl PreparedListMutation {
    pub fn new(intent: ListWriteIntent) -> Result<Self, PrepareFailure> {
        let (request, expected) = match intent {
            ListWriteIntent::Create { name } => {
                lists_contract::admit_list_name(&name).map_err(|_| PrepareFailure::InvalidName)?;
                (
                    Request::Create(wire::CreateListRequest {
                        name,
                        ..Default::default()
                    }),
                    Expected::CreatedList,
                )
            }
            ListWriteIntent::Rename { revision, name } => {
                lists_contract::admit_list_name(&name).map_err(|_| PrepareFailure::InvalidName)?;
                (
                    Request::Rename(wire::RenameListRequest {
                        list_id: id(revision.list()).into(),
                        expected_revision: list_revision(&revision).into(),
                        name,
                        ..Default::default()
                    }),
                    Expected::List(revision),
                )
            }
            ListWriteIntent::DeleteList { revision } => (
                Request::DeleteList(wire::DeleteListRequest {
                    list_id: id(revision.list()).into(),
                    expected_revision: list_revision(&revision).into(),
                    ..Default::default()
                }),
                Expected::List(revision),
            ),
            ListWriteIntent::Add { revision, payload } => {
                lists_contract::admit_item_payload(&payload)
                    .map_err(|_| PrepareFailure::InvalidPayload)?;
                (
                    Request::Add(wire::AddItemRequest {
                        list_id: id(revision.list()).into(),
                        expected_list_revision: list_revision(&revision).into(),
                        item: payload.into(),
                        ..Default::default()
                    }),
                    Expected::AddedItem(revision),
                )
            }
            ListWriteIntent::Edit { target, payload } => {
                lists_contract::admit_item_payload(&payload)
                    .map_err(|_| PrepareFailure::InvalidPayload)?;
                (
                    Request::Edit(wire::EditItemRequest {
                        list_id: id(target.key().list()).into(),
                        item_uid: uid(&target).into(),
                        expected_list_revision: list_revision(&target.list).into(),
                        expected_item_revision: item_revision(&target.item).into(),
                        item: payload.into(),
                        ..Default::default()
                    }),
                    Expected::Item(target),
                )
            }
            ListWriteIntent::DeleteItem { target } => (
                Request::DeleteItem(wire::DeleteItemRequest {
                    list_id: id(target.key().list()).into(),
                    item_uid: uid(&target).into(),
                    expected_list_revision: list_revision(&target.list).into(),
                    expected_item_revision: item_revision(&target.item).into(),
                    ..Default::default()
                }),
                Expected::Item(target),
            ),
            ListWriteIntent::Complete { target, completed } => (
                Request::Complete(wire::SetCompletedRequest {
                    list_id: id(target.key().list()).into(),
                    item_uid: uid(&target).into(),
                    expected_list_revision: list_revision(&target.list).into(),
                    expected_item_revision: item_revision(&target.item).into(),
                    completed: Some(completed),
                    ..Default::default()
                }),
                Expected::Item(target),
            ),
            ListWriteIntent::Move {
                target,
                destination,
            } => {
                let destination = match destination {
                    MoveDestination::AtStart => {
                        wire::move_item_request::Destination::AtStart(Default::default())
                    }
                    MoveDestination::After(after) => {
                        if after.list() != target.key().list() || &after == target.key() {
                            return Err(PrepareFailure::InvalidDestination);
                        }
                        wire::move_item_request::Destination::AfterUid(
                            wire::ItemUid {
                                value: after.uid().as_str().into(),
                                ..Default::default()
                            }
                            .into(),
                        )
                    }
                };
                (
                    Request::Move(wire::MoveItemRequest {
                        list_id: id(target.key().list()).into(),
                        item_uid: uid(&target).into(),
                        expected_list_revision: list_revision(&target.list).into(),
                        expected_item_revision: item_revision(&target.item).into(),
                        destination: Some(destination),
                        ..Default::default()
                    }),
                    Expected::Item(target),
                )
            }
        };
        Ok(Self { request, expected })
    }

    /// The session coordinator is the only credential owner. Admission is
    /// checked again after waiting for access and after the single RPC result.
    pub async fn submit(
        self,
        transport: &ListTransport,
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

enum SubmitFailure {
    Transport(Failure),
    Protocol,
}
trait SubmitApi: Sync {
    fn submit(
        &self,
        request: Request,
    ) -> impl Future<Output = Result<wire::MutationReceipt, SubmitFailure>> + Send;
}
struct NativeApi<'a> {
    transport: &'a ListTransport,
    lease: &'a AccessLease,
}
impl SubmitApi for NativeApi<'_> {
    async fn submit(&self, request: Request) -> Result<wire::MutationReceipt, SubmitFailure> {
        macro_rules! call {
            ($method:ident, $request:expr) => {{
                let response = self
                    .transport
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
            Request::Create(request) => call!(create_list, request),
            Request::Rename(request) => call!(rename_list, request),
            Request::DeleteList(request) => call!(delete_list, request),
            Request::Add(request) => call!(add_item, request),
            Request::Edit(request) => call!(edit_item, request),
            Request::DeleteItem(request) => call!(delete_item, request),
            Request::Complete(request) => call!(set_completed, request),
            Request::Move(request) => call!(move_item, request),
        }
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
                Some(confirmed) => ListWriteOutcome::Committed(confirmed),
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

fn confirm(expected: Expected, receipt: wire::MutationReceipt) -> Option<ConfirmedMutation> {
    lists_contract::admit_receipt(&receipt).ok()?;
    let list = receipt.list_id.as_option()?;
    let list_number = receipt.list_revision.as_option()?.number;
    let list_matches = |expected: &ListRevision| {
        expected.list().as_str() == list.value && list_number > expected.number().get()
    };
    let valid = match expected {
        Expected::CreatedList => !receipt.item_uid.is_set(),
        Expected::List(revision) => list_matches(&revision) && !receipt.item_uid.is_set(),
        Expected::AddedItem(revision) => list_matches(&revision) && receipt.item_uid.is_set(),
        Expected::Item(target) => {
            list_matches(&target.list)
                && receipt
                    .item_uid
                    .as_option()
                    .is_some_and(|uid| uid.value == target.key().uid().as_str())
                && receipt
                    .item_revision
                    .as_option()
                    .is_some_and(|revision| revision.number > target.item.number().get())
        }
    };
    valid.then_some(ConfirmedMutation {
        receipt: Box::new(receipt),
    })
}

fn classify(failure: &Failure) -> ListWriteOutcome {
    if failure.kind() == FailureKind::Rpc(RpcCode::Unauthenticated)
        || !matches!(failure.authorization_termination(), Ok(None))
    {
        return ListWriteOutcome::AuthorityTerminated;
    }
    let detail = match failure.list_error() {
        Ok(Some(detail)) if lists_contract::admit_error(&detail).is_ok() => detail,
        Ok(None) => return ListWriteOutcome::CommitUnknown(WriteIssue::Transport(failure.kind())),
        Ok(Some(_)) | Err(_) => {
            return ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse);
        }
    };
    use wire::ListErrorCode as Code;
    let status = match detail.code {
        EnumValue::Known(Code::Invalid) => RpcCode::InvalidArgument,
        EnumValue::Known(Code::Denied) => RpcCode::PermissionDenied,
        EnumValue::Known(Code::NotFound) => RpcCode::NotFound,
        EnumValue::Known(
            Code::Conflict | Code::CommitUnknown | Code::CommittedResponseWithheld,
        ) => RpcCode::Aborted,
        EnumValue::Known(Code::StaleCursor) => RpcCode::FailedPrecondition,
        EnumValue::Known(Code::Capacity) => RpcCode::ResourceExhausted,
        EnumValue::Known(Code::Unavailable | Code::Closed) => RpcCode::Unavailable,
        EnumValue::Known(Code::Unspecified) | EnumValue::Unknown(_) => {
            return ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse);
        }
    };
    if failure.kind() != FailureKind::Rpc(status) {
        return ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse);
    }
    match detail.code {
        EnumValue::Known(Code::Conflict) => ListWriteOutcome::Conflict,
        EnumValue::Known(Code::CommitUnknown) => {
            ListWriteOutcome::CommitUnknown(WriteIssue::Transport(failure.kind()))
        }
        EnumValue::Known(Code::CommittedResponseWithheld) => {
            ListWriteOutcome::CommittedResponseWithheld
        }
        EnumValue::Known(Code::Invalid) => ListWriteOutcome::Rejected(Rejection::Invalid),
        EnumValue::Known(Code::Denied) => ListWriteOutcome::Rejected(Rejection::Denied),
        EnumValue::Known(Code::NotFound) => ListWriteOutcome::Rejected(Rejection::NotFound),
        EnumValue::Known(Code::Capacity) => ListWriteOutcome::Rejected(Rejection::Capacity),
        EnumValue::Known(Code::Unavailable) => ListWriteOutcome::Rejected(Rejection::Unavailable),
        EnumValue::Known(Code::Closed) => ListWriteOutcome::Rejected(Rejection::Closed),
        EnumValue::Known(Code::StaleCursor | Code::Unspecified) | EnumValue::Unknown(_) => {
            ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse)
        }
    }
}

#[cfg(test)]
mod tests;
