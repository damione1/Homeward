use super::*;
use buffa::{Enumeration, MessageName};
use connectrpc::{ConnectError, ErrorDetail};
use home_client_model::lists::ItemUid;
use home_wire_contract::{
    api_errors::DOMAIN, google::rpc::ErrorInfo, home::api::v2::ApiErrorReason as Code,
};
use std::sync::{
    Mutex,
    atomic::{AtomicUsize, Ordering},
};

fn home() -> HomeName {
    HomeName::try_from("homes/synthetic").unwrap()
}
fn revision(list: &str) -> TaskListRevision {
    TaskListRevision::new(
        TaskListName::new(home(), list.parse().unwrap()),
        home_client_model::api_tasks::TaskEtag::try_from("\"opaque-list\"").unwrap(),
    )
}
fn target(list: &str, uid: &str) -> ItemTarget {
    ItemTarget::new(
        revision(list),
        TaskRevision::new(
            TaskName::new(revision(list).name().clone(), uid.parse().unwrap()),
            home_client_model::api_tasks::TaskEtag::try_from("\"opaque-child-and-parent\"")
                .unwrap(),
        ),
    )
    .unwrap()
}
fn payload() -> TaskContent {
    TaskContent::new(
        TaskText::try_from("Synthetic task").unwrap(),
        false,
        Some(home_client_model::api_tasks::TaskDescription::try_from("").unwrap()),
        home_client_model::api_tasks::TaskDue::None,
    )
}
fn receipt(list: &str, uid: Option<&str>) -> Reply {
    match uid {
        Some(uid) => Reply::Task(wire::Task {
            name: target(list, uid).item.name().as_str().to_owned(),
            etag: "\"another-opaque-etag\"".to_owned(),
            summary: "Synthetic task".to_owned(),
            ..Default::default()
        }),
        None => Reply::List(wire::TaskList {
            name: revision(list).name().as_str().to_owned(),
            etag: "\"another-opaque-etag\"".to_owned(),
            display_name: "Synthetic list".to_owned(),
            origin: wire::TaskListOrigin::Native.into(),
            update_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1,
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }),
    }
}
fn complete() -> PreparedListMutation {
    PreparedListMutation::new(ListWriteIntent::Complete {
        target: target("a", "same"),
        completed: false,
    })
    .unwrap()
}
#[test]
fn explicit_masks_preserve_false_and_empty_and_append_cannot_carry_resource_fields() {
    let Request::Complete(request) = complete().request else {
        panic!("complete")
    };
    assert!(!request.task.completed);
    assert_eq!(request.update_mask.paths, ["completed"]);
    assert_eq!(request.task.etag, "\"opaque-child-and-parent\"");
    let prepared = PreparedListMutation::new(ListWriteIntent::Add {
        revision: revision("a"),
        payload: payload(),
    })
    .unwrap();
    let Request::Add(request) = prepared.request else {
        panic!("append")
    };
    assert_eq!(request.name, revision("a").name().as_str());
    assert_eq!(request.etag, "\"opaque-list\"");
    assert!(!request.task.completed);
    assert_eq!(request.task.description, Some(String::new()));
    let prepared = PreparedListMutation::new(ListWriteIntent::Edit {
        target: target("a", "same"),
        payload: payload(),
    })
    .unwrap();
    let Request::Edit(request) = prepared.request else {
        panic!("edit")
    };
    assert_eq!(
        request.update_mask.paths,
        [
            "summary",
            "completed",
            "description",
            "due_date",
            "due_instant"
        ]
    );
    assert!(request.task.due.is_none());
}
#[test]
fn cascade_requires_an_explicit_decision_and_foreign_targets_are_rejected() {
    for force in [false, true] {
        let prepared = PreparedListMutation::new(ListWriteIntent::DeleteList {
            revision: revision("a"),
            force,
        })
        .unwrap();
        let Request::DeleteList(request) = prepared.request else {
            panic!("delete")
        };
        assert_eq!(request.force, force);
    }
    assert!(ItemTarget::new(revision("a"), target("b", "same").item).is_err());
    for after in [
        target("b", "other").key().clone(),
        target("a", "same").key().clone(),
    ] {
        assert!(
            PreparedListMutation::new(ListWriteIntent::Move {
                target: target("a", "same"),
                destination: MoveDestination::After(after)
            })
            .is_err()
        );
    }
    let prepared = PreparedListMutation::new(ListWriteIntent::Move {
        target: target("a", "same"),
        destination: MoveDestination::After(ItemKey::new(
            "a".parse().unwrap(),
            "other".parse::<ItemUid>().unwrap(),
        )),
    })
    .unwrap();
    let Request::Move(request) = prepared.request else {
        panic!("move")
    };
    assert!(
        matches!(request.destination,Some(wire::move_task_request::Destination::Predecessor(name)) if name==target("a","other").item.name().as_str())
    );
}
#[test]
fn responses_bind_exact_canonical_identity_without_ordering_opaque_etags() {
    assert!(
        confirm(
            Expected::Item(target("a", "same").item.name().clone()),
            receipt("a", Some("same"))
        )
        .is_some()
    );
    for (list, uid) in [("b", Some("same")), ("a", Some("other")), ("a", None)] {
        assert!(
            confirm(
                Expected::Item(target("a", "same").item.name().clone()),
                receipt(list, uid)
            )
            .is_none()
        );
    }
    assert!(
        confirm(
            Expected::DeletedItem(target("a", "same").item.name().clone()),
            Reply::Deleted
        )
        .is_some()
    );
    assert!(
        confirm(
            Expected::Item(target("a", "same").item.name().clone()),
            Reply::Deleted
        )
        .is_none()
    );
}
struct Api {
    calls: AtomicUsize,
    response: Mutex<Option<Result<Reply, SubmitFailure>>>,
}
impl Api {
    fn new(response: Result<Reply, SubmitFailure>) -> Self {
        Self {
            calls: AtomicUsize::new(0),
            response: Mutex::new(Some(response)),
        }
    }
}
impl SubmitApi for Api {
    async fn submit(&self, _: Request) -> Result<Reply, SubmitFailure> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.response
            .lock()
            .unwrap()
            .take()
            .expect("a mutation must never be retried")
    }
}
fn reason_detail(code: Code) -> ErrorDetail {
    ErrorDetail::from_message(
        ErrorInfo::FULL_NAME,
        &ErrorInfo {
            domain: DOMAIN.into(),
            reason: code
                .proto_name()
                .trim_start_matches("API_ERROR_REASON_")
                .into(),
            ..Default::default()
        },
    )
}
fn typed(code: Code, rpc: ConnectError) -> Failure {
    use home_wire_contract::home::api::v2::{
        ExecutionUncertainty, ReconciliationDisposition, UncertaintyPhase,
    };
    let mut error = rpc.with_detail(reason_detail(code));
    if code == Code::CommitOutcomeUnknown {
        error = error.with_detail(ErrorDetail::from_message(
            ExecutionUncertainty::FULL_NAME,
            &ExecutionUncertainty {
                phase: UncertaintyPhase::Commit.into(),
                disposition: ReconciliationDisposition::ReconcileOnly.into(),
                ..Default::default()
            },
        ));
    }
    Failure::from(error)
}

#[tokio::test]
async fn conflict_unknown_and_withheld_commits_are_distinct_and_each_makes_one_attempt() {
    enum Scenario {
        Conflict,
        Unknown,
        Withheld,
        Network,
        Malformed,
        ForeignReceipt,
        Success,
    }
    for scenario in [
        Scenario::Conflict,
        Scenario::Unknown,
        Scenario::Withheld,
        Scenario::Network,
        Scenario::Malformed,
        Scenario::ForeignReceipt,
        Scenario::Success,
    ] {
        let response = match scenario {
            Scenario::Conflict => Err(SubmitFailure::Transport(typed(
                Code::EtagMismatch,
                ConnectError::aborted("private"),
            ))),
            Scenario::Unknown => Err(SubmitFailure::Transport(typed(
                Code::CommitOutcomeUnknown,
                ConnectError::unknown("private"),
            ))),
            Scenario::Withheld => Err(SubmitFailure::Transport(typed(
                Code::AccessDenied,
                ConnectError::permission_denied("private"),
            ))),
            Scenario::Network => Err(SubmitFailure::Transport(Failure::from(
                ConnectError::unavailable("network cut"),
            ))),
            Scenario::Malformed => Err(SubmitFailure::Protocol),
            Scenario::ForeignReceipt => Ok(receipt("b", Some("same"))),
            Scenario::Success => Ok(receipt("a", Some("same"))),
        };
        let api = Api::new(response);
        let result = execute(
            &api,
            complete(),
            &CancellationToken::new(),
            &CancellationToken::new(),
            || true,
        )
        .await;
        assert_eq!(api.calls.load(Ordering::SeqCst), 1);
        match scenario {
            Scenario::Conflict => assert!(matches!(result.outcome, ListWriteOutcome::Conflict)),
            Scenario::Unknown | Scenario::Network => assert!(matches!(
                result.outcome,
                ListWriteOutcome::CommitUnknown(WriteIssue::Transport(_))
            )),
            Scenario::Withheld => assert!(matches!(
                result.outcome,
                ListWriteOutcome::AuthorityTerminated
            )),
            Scenario::Malformed => assert!(matches!(
                result.outcome,
                ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse)
            )),
            Scenario::ForeignReceipt => assert!(matches!(
                result.outcome,
                ListWriteOutcome::CommitUnknown(WriteIssue::InvalidReceipt)
            )),
            Scenario::Success => assert!(matches!(result.outcome, ListWriteOutcome::Committed(_))),
        }
    }
}

#[tokio::test]
async fn cancellation_or_changed_admission_before_dispatch_makes_no_rpc() {
    enum Stop {
        Cancel,
        Access,
        Admission,
    }
    for stop in [Stop::Cancel, Stop::Access, Stop::Admission] {
        let cancel = CancellationToken::new();
        let access = CancellationToken::new();
        match stop {
            Stop::Cancel => cancel.cancel(),
            Stop::Access => access.cancel(),
            Stop::Admission => {}
        }
        let api = Api::new(Ok(receipt("a", Some("same"))));
        let result = execute(&api, complete(), &cancel, &access, || {
            !matches!(stop, Stop::Admission)
        })
        .await;
        assert!(matches!(result.outcome, ListWriteOutcome::NotSent(_)));
        assert_eq!(api.calls.load(Ordering::SeqCst), 0);
    }
}

#[tokio::test]
async fn a_reply_racing_with_projection_withdrawal_never_reports_saved() {
    let calls = AtomicUsize::new(0);
    let api = Api::new(Ok(receipt("a", Some("same"))));
    let result = execute(
        &api,
        complete(),
        &CancellationToken::new(),
        &CancellationToken::new(),
        || calls.fetch_add(1, Ordering::SeqCst) == 0,
    )
    .await;
    assert!(matches!(
        result.outcome,
        ListWriteOutcome::CommitUnknown(WriteIssue::Superseded)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
}

#[test]
fn ambiguous_or_unknown_typed_errors_and_authority_termination_do_not_become_conflicts() {
    let detail = reason_detail(Code::EtagMismatch);
    let duplicate = Failure::from(
        ConnectError::aborted("private")
            .with_detail(detail.clone())
            .with_detail(detail),
    );
    assert!(matches!(
        classify(&duplicate),
        ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse)
    ));
    let unknown = Failure::from(ConnectError::aborted("private").with_detail(
        ErrorDetail::from_message(
            ErrorInfo::FULL_NAME,
            &ErrorInfo {
                domain: DOMAIN.into(),
                reason: "FUTURE_REASON".into(),
                ..Default::default()
            },
        ),
    ));
    assert!(matches!(
        classify(&unknown),
        ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse)
    ));
    let authority = Failure::from(ConnectError::unauthenticated("private").with_detail(
        ErrorDetail::from_message(
            ErrorInfo::FULL_NAME,
            &ErrorInfo {
                domain: DOMAIN.into(),
                reason: "AUTHENTICATION_REQUIRED".into(),
                ..Default::default()
            },
        ),
    ));
    assert!(matches!(
        classify(&authority),
        ListWriteOutcome::AuthorityTerminated
    ));
    let unauthenticated = Failure::from(ConnectError::unauthenticated("private"));
    assert!(matches!(
        classify(&unauthenticated),
        ListWriteOutcome::AuthorityTerminated
    ));
    let contradictory = Failure::from(ConnectError::unavailable("private").with_detail(
        ErrorDetail::from_message(
            ErrorInfo::FULL_NAME,
            &ErrorInfo {
                domain: DOMAIN.into(),
                reason: "ETAG_MISMATCH".into(),
                ..Default::default()
            },
        ),
    ));
    assert!(matches!(
        classify(&contradictory),
        ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse)
    ));
}

#[tokio::test]
async fn late_authority_failure_is_still_available_to_the_session_after_admission_changes() {
    let checks = AtomicUsize::new(0);
    let api = Api::new(Err(SubmitFailure::Transport(Failure::from(
        ConnectError::unauthenticated("private"),
    ))));
    let result = execute(
        &api,
        complete(),
        &CancellationToken::new(),
        &CancellationToken::new(),
        || checks.fetch_add(1, Ordering::SeqCst) == 0,
    )
    .await;
    assert!(matches!(
        result.outcome,
        ListWriteOutcome::CommitUnknown(WriteIssue::Superseded)
    ));
    assert!(result.failure.is_some());
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn cancelling_a_dispatched_write_drops_the_wait_and_never_retries() {
    struct Blocked {
        started: tokio::sync::Notify,
        calls: AtomicUsize,
        dropped: AtomicUsize,
    }
    struct DropWait<'a>(&'a AtomicUsize);
    impl Drop for DropWait<'_> {
        fn drop(&mut self) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }
    impl SubmitApi for Blocked {
        async fn submit(&self, _: Request) -> Result<Reply, SubmitFailure> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let _drop = DropWait(&self.dropped);
            self.started.notify_one();
            std::future::pending().await
        }
    }
    let api = Blocked {
        started: tokio::sync::Notify::new(),
        calls: AtomicUsize::new(0),
        dropped: AtomicUsize::new(0),
    };
    let cancel = CancellationToken::new();
    let access = CancellationToken::new();
    let stopping = async {
        api.started.notified().await;
        cancel.cancel();
    };
    let submitting = execute(&api, complete(), &cancel, &access, || true);
    let (result, ()) = tokio::time::timeout(Duration::from_secs(2), async {
        tokio::join!(submitting, stopping)
    })
    .await
    .unwrap();
    assert!(matches!(
        result.outcome,
        ListWriteOutcome::CommitUnknown(WriteIssue::Cancelled)
    ));
    assert_eq!(api.calls.load(Ordering::SeqCst), 1);
    assert_eq!(api.dropped.load(Ordering::SeqCst), 1);
}
