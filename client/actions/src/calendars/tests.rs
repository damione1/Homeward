use super::*;
use buffa::{Enumeration, MessageName};
use connectrpc::{ConnectError, ErrorDetail};
use std::sync::{
    Arc,
    atomic::{AtomicBool, AtomicUsize, Ordering},
};
use tokio::sync::Notify;
const NAME: &str = "homes/synthetic/calendars/id-63616c";
fn calendar() -> wire::Calendar {
    wire::Calendar {
        name: NAME.into(),
        display_name: "Synthetic".into(),
        source_entity: "homes/synthetic/entities/source".into(),
        etag: "W/\"opaque-current\"".into(),
        origin: wire::CalendarOrigin::Local.into(),
        freshness: wire::CalendarFreshness::Fresh.into(),
        provenance: wire::CalendarProvenance::Synthetic.into(),
        mutation_access: wire::CalendarMutationAccess::Allowed.into(),
        ..Default::default()
    }
}
fn prepared() -> PreparedCalendarMutation {
    let value = calendar();
    let name = CalendarName::try_from(NAME).unwrap();
    let current = admit::admit_calendar(&value, &name).unwrap();
    PreparedCalendarMutation::rename(&current, "Updated".into()).unwrap()
}
#[derive(Clone, Copy)]
enum FakeReply {
    Committed,
    Unknown,
    Stall,
    ForeignReceipt,
}
struct Fake {
    reply: FakeReply,
    calls: AtomicUsize,
    started: Notify,
    invalidate: Option<Arc<AtomicBool>>,
}
impl Fake {
    fn new(reply: FakeReply) -> Self {
        Self {
            reply,
            calls: AtomicUsize::new(0),
            started: Notify::new(),
            invalidate: None,
        }
    }
}
fn failure(status: RpcCode, reason: wire::ApiErrorReason) -> Failure {
    use home_wire_contract::{api_errors::DOMAIN, google::rpc::ErrorInfo};
    let mut error = ConnectError::new(status, "synthetic private content").with_detail(
        ErrorDetail::from_message(
            ErrorInfo::FULL_NAME,
            &ErrorInfo {
                domain: DOMAIN.into(),
                reason: reason
                    .proto_name()
                    .trim_start_matches("API_ERROR_REASON_")
                    .into(),
                ..Default::default()
            },
        ),
    );
    if reason == wire::ApiErrorReason::CommitOutcomeUnknown {
        error = error.with_detail(ErrorDetail::from_message(
            wire::ExecutionUncertainty::FULL_NAME,
            &wire::ExecutionUncertainty {
                phase: wire::UncertaintyPhase::Commit.into(),
                disposition: wire::ReconciliationDisposition::ReconcileOnly.into(),
                ..Default::default()
            },
        ));
    }
    Failure::from(error)
}
impl SubmitApi for Fake {
    async fn submit(&self, _request: &Request) -> Result<super::Reply, SubmitFailure> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.started.notify_one();
        if let Some(guard) = &self.invalidate {
            guard.store(false, Ordering::SeqCst)
        }
        match self.reply {
            FakeReply::Stall => std::future::pending().await,
            FakeReply::Unknown => Err(SubmitFailure::Transport(failure(
                RpcCode::Aborted,
                wire::ApiErrorReason::CommitOutcomeUnknown,
            ))),
            FakeReply::Committed | FakeReply::ForeignReceipt => {
                let mut value = calendar();
                value.etag = "W/\"opaque-new\"".into();
                if matches!(self.reply, FakeReply::ForeignReceipt) {
                    value.name = "homes/synthetic/calendars/id-666f726569676e".into();
                }
                Ok(super::Reply::Calendar(value))
            }
        }
    }
}
#[tokio::test]
async fn cancellation_or_changed_admission_before_polling_never_dispatches() {
    let api = Fake::new(FakeReply::Committed);
    let cancel = CancellationToken::new();
    let access = CancellationToken::new();
    let result = execute(&api, prepared(), &cancel, &access, || false, || true).await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::NotSent(NotSentReason::AdmissionChanged)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 0);
    cancel.cancel();
    let result = execute(&api, prepared(), &cancel, &access, || true, || true).await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::NotSent(NotSentReason::Cancelled)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 0);
}
#[tokio::test]
async fn an_uncertain_dispatched_write_is_consumed_once_and_never_replayed() {
    let api = Fake::new(FakeReply::Unknown);
    let cancel = CancellationToken::new();
    let access = CancellationToken::new();
    let result = execute(&api, prepared(), &cancel, &access, || true, || true).await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::CommitUnknown(_)
    ));
    tokio::task::yield_now().await;
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
    let api = Fake::new(FakeReply::Stall);
    let canceler = async {
        api.started.notified().await;
        cancel.cancel()
    };
    let (result, ()) = tokio::join!(
        execute(&api, prepared(), &cancel, &access, || true, || true),
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
    let api = Fake::new(FakeReply::ForeignReceipt);
    let result = execute(&api, prepared(), &cancel, &access, || true, || true).await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::CommitUnknown(UnknownReason::InvalidReceipt)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
    let guard = Arc::new(AtomicBool::new(true));
    let mut api = Fake::new(FakeReply::Committed);
    api.invalidate = Some(guard.clone());
    let result = execute(
        &api,
        prepared(),
        &cancel,
        &access,
        || true,
        || guard.load(Ordering::SeqCst),
    )
    .await;
    assert!(matches!(
        result.outcome,
        CalendarWriteOutcome::CommitUnknown(UnknownReason::Superseded)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
    let api = Fake::new(FakeReply::Committed);
    let result = execute(&api, prepared(), &cancel, &access, || true, || true).await;
    assert!(matches!(result.outcome, CalendarWriteOutcome::Committed(_)));
}
#[test]
fn canonical_conflict_requires_matching_status_and_unknown_details_fail_closed() {
    assert!(matches!(
        classify(&failure(
            RpcCode::Aborted,
            wire::ApiErrorReason::EtagMismatch
        )),
        CalendarWriteOutcome::Conflict
    ));
    assert!(matches!(
        classify(&failure(
            RpcCode::PermissionDenied,
            wire::ApiErrorReason::EtagMismatch
        )),
        CalendarWriteOutcome::CommitUnknown(UnknownReason::Protocol)
    ));
    assert!(matches!(
        classify(&failure(
            RpcCode::PermissionDenied,
            wire::ApiErrorReason::AccessDenied
        )),
        CalendarWriteOutcome::AuthorityTerminated
    ));
}
#[test]
fn delete_never_enables_cascade_implicitly() {
    let name = CalendarName::try_from(NAME).unwrap();
    let value = admit::admit_calendar(&calendar(), &name).unwrap();
    let prepared = PreparedCalendarMutation::delete(&value, false).unwrap();
    match prepared.request {
        Request::Delete(request, _) => assert!(!request.force),
        Request::Create(..)
        | Request::Rename(..)
        | Request::CreateEvent(..)
        | Request::UpdateEvent(..)
        | Request::DeleteEvent(..) => panic!("wrong prepared mutation"),
    }
}

#[tokio::test]
async fn watch_invalidating_new_dispatches_does_not_discard_an_owned_commit_receipt() {
    let dispatch = Arc::new(AtomicBool::new(true));
    let mut api = Fake::new(FakeReply::Committed);
    api.invalidate = Some(dispatch.clone());
    let result = execute(
        &api,
        prepared(),
        &CancellationToken::new(),
        &CancellationToken::new(),
        || dispatch.load(Ordering::SeqCst),
        || true,
    )
    .await;
    assert!(!dispatch.load(Ordering::SeqCst));
    assert!(matches!(result.outcome, CalendarWriteOutcome::Committed(_)));
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
}
