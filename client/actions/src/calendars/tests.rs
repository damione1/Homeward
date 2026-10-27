use super::*;
use buffa::MessageName;
use connectrpc::{ConnectError, ErrorDetail};
use std::sync::{
    Arc,
    atomic::{AtomicBool, AtomicUsize, Ordering},
};
use tokio::sync::Notify;
fn revision(entity: &str, number: u64) -> wire::Revision {
    wire::Revision {
        resource: wire::EventKey {
            calendar: wire::CalendarId {
                entity_id: entity.into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }
        .into(),
        presence: wire::Presence::Present.into(),
        number,
        ..Default::default()
    }
}
fn prepared() -> PreparedCalendarMutation {
    let id = wire::CalendarId {
        entity_id: "synthetic-only".into(),
        ..Default::default()
    };
    let calendar = wire::Calendar {
        id: id.clone().into(),
        origin: wire::Origin::Local.into(),
        name: Some("Synthetic".into()),
        revision: revision("synthetic-only", 3).into(),
        quality: wire::Quality::Fresh.into(),
        mutation_access: wire::MutationAccess::Allowed.into(),
        ..Default::default()
    };
    let current = admit::admit_calendar(&calendar, &admit::calendar_id(&id).unwrap()).unwrap();
    PreparedCalendarMutation::calendar(
        &current,
        wire::MutateCalendarRequest {
            id: id.into(),
            mutation: wire::Mutation::Update.into(),
            expected_revision: calendar.revision,
            name: "Updated".into(),
            ..Default::default()
        },
    )
    .unwrap()
}
#[derive(Clone, Copy)]
enum Reply {
    Committed,
    Unknown,
    Stall,
    ForeignReceipt,
}
struct Fake {
    reply: Reply,
    calls: AtomicUsize,
    started: Notify,
    invalidate: Option<Arc<AtomicBool>>,
}
impl Fake {
    fn new(reply: Reply) -> Self {
        Self {
            reply,
            calls: AtomicUsize::new(0),
            started: Notify::new(),
            invalidate: None,
        }
    }
}
fn failure(status: RpcCode, code: EnumValue<wire::ErrorCode>) -> Failure {
    let detail = wire::CalendarError {
        code,
        ..Default::default()
    };
    Failure::from(
        ConnectError::new(status, "synthetic private content").with_detail(
            ErrorDetail::from_message(wire::CalendarError::FULL_NAME, &detail),
        ),
    )
}
impl SubmitApi for Fake {
    async fn submit(&self, _request: &Request) -> Result<wire::MutationReceipt, SubmitFailure> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.started.notify_one();
        if let Some(guard) = &self.invalidate {
            guard.store(false, Ordering::SeqCst)
        }
        match self.reply {
            Reply::Stall => std::future::pending().await,
            Reply::Unknown => Err(SubmitFailure::Transport(failure(
                RpcCode::Aborted,
                wire::ErrorCode::CommitUnknown.into(),
            ))),
            Reply::Committed | Reply::ForeignReceipt => {
                let entity = match self.reply {
                    Reply::ForeignReceipt => "foreign",
                    Reply::Committed | Reply::Unknown | Reply::Stall => "synthetic-only",
                };
                Ok(wire::MutationReceipt {
                    revision: revision(entity, 4).into(),
                    calendar_revision: revision(entity, 4).into(),
                    ..Default::default()
                })
            }
        }
    }
}
#[tokio::test]
async fn cancellation_or_changed_admission_before_polling_never_dispatches() {
    let api = Fake::new(Reply::Committed);
    let cancel = CancellationToken::new();
    let access = CancellationToken::new();
    let result = execute(&api, prepared(), &cancel, &access, || false).await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::NotSent(NotSentReason::AdmissionChanged)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 0);
    cancel.cancel();
    let result = execute(&api, prepared(), &cancel, &access, || true).await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::NotSent(NotSentReason::Cancelled)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 0);
}
#[tokio::test]
async fn an_uncertain_dispatched_write_is_consumed_once_and_never_replayed() {
    let api = Fake::new(Reply::Unknown);
    let cancel = CancellationToken::new();
    let access = CancellationToken::new();
    let result = execute(&api, prepared(), &cancel, &access, || true).await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::CommitUnknown(_)
    ));
    tokio::task::yield_now().await;
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
    let api = Fake::new(Reply::Stall);
    let canceler = async {
        api.started.notified().await;
        cancel.cancel()
    };
    let (result, ()) = tokio::join!(
        execute(&api, prepared(), &cancel, &access, || true),
        canceler
    );
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::CommitUnknown(UnknownReason::Cancelled)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
}
#[tokio::test]
async fn a_receipt_from_another_resource_or_superseded_context_cannot_commit_the_draft() {
    let cancel = CancellationToken::new();
    let access = CancellationToken::new();
    let api = Fake::new(Reply::ForeignReceipt);
    let result = execute(&api, prepared(), &cancel, &access, || true).await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::CommitUnknown(UnknownReason::InvalidReceipt)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
    let guard = Arc::new(AtomicBool::new(true));
    let mut api = Fake::new(Reply::Committed);
    api.invalidate = Some(guard.clone());
    let result = execute(&api, prepared(), &cancel, &access, || {
        guard.load(Ordering::SeqCst)
    })
    .await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::CommitUnknown(UnknownReason::Superseded)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
    let api = Fake::new(Reply::Committed);
    let result = execute(&api, prepared(), &cancel, &access, || true).await;
    assert!(matches!(result.outcome, CalendarWriteOutcome::Committed(_)));
}
#[test]
fn typed_conflict_and_uncertainty_require_the_corresponding_rpc_status() {
    assert!(matches!(
        classify(&failure(RpcCode::Aborted, wire::ErrorCode::Conflict.into())),
        CalendarWriteOutcome::Conflict
    ));
    assert!(matches!(
        classify(&failure(
            RpcCode::Aborted,
            wire::ErrorCode::CommittedResponseWithheld.into()
        )),
        CalendarWriteOutcome::CommittedResponseWithheld
    ));
    assert!(matches!(
        classify(&failure(
            RpcCode::PermissionDenied,
            wire::ErrorCode::Conflict.into()
        )),
        CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol)
    ));
    assert!(matches!(
        classify(&failure(RpcCode::Aborted, EnumValue::Unknown(999))),
        CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol)
    ));
}
