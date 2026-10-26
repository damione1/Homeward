use super::*;
use buffa::MessageName;
use connectrpc::{ConnectError, ErrorDetail};
use home_client_model::lists::ItemUid;
use std::sync::{
    Mutex,
    atomic::{AtomicUsize, Ordering},
};

fn revision(list: &str) -> ListRevision {
    ListRevision::new(list.parse().unwrap(), 11).unwrap()
}
fn target(list: &str, uid: &str) -> ItemTarget {
    ItemTarget::new(
        revision(list),
        ItemRevision::new(ItemKey::new(list.parse().unwrap(), uid.parse().unwrap()), 7).unwrap(),
    )
    .unwrap()
}
fn payload() -> wire::ItemPayload {
    wire::ItemPayload {
        summary: "Synthetic task".into(),
        completed: Some(false),
        description: Some(String::new()),
        ..Default::default()
    }
}
fn receipt(list: &str, uid: Option<&str>) -> wire::MutationReceipt {
    let mut receipt = wire::MutationReceipt::default();
    receipt.list_id.get_or_insert_default().value = list.into();
    receipt.list_revision.get_or_insert_default().list_id = receipt.list_id.clone();
    receipt.list_revision.get_or_insert_default().number = 12;
    if let Some(uid) = uid {
        receipt.item_uid.get_or_insert_default().value = uid.into();
        receipt.item_revision.get_or_insert_default().list_id = receipt.list_id.clone();
        receipt.item_revision.get_or_insert_default().item_uid = receipt.item_uid.clone();
        receipt.item_revision.get_or_insert_default().number = 8;
    }
    receipt
}
fn complete() -> PreparedListMutation {
    PreparedListMutation::new(ListWriteIntent::Complete {
        target: target("a", "same"),
        completed: false,
    })
    .unwrap()
}

#[test]
fn preparation_preserves_false_optional_empty_values_and_identity_bound_revisions() {
    let PreparedListMutation {
        request: Request::Complete(request),
        ..
    } = complete()
    else {
        panic!("completion")
    };
    assert_eq!(request.completed, Some(false));
    assert_eq!(request.list_id.value, "a");
    assert_eq!(request.item_uid.value, "same");
    assert_eq!(request.expected_list_revision.number, 11);
    assert_eq!(request.expected_item_revision.number, 7);
    assert_eq!(request.expected_item_revision.item_uid.value, "same");
    let PreparedListMutation {
        request: Request::Edit(request),
        ..
    } = PreparedListMutation::new(ListWriteIntent::Edit {
        target: target(" opaque / 项目 ", " UID "),
        payload: payload(),
    })
    .unwrap()
    else {
        panic!("edit")
    };
    assert_eq!(request.list_id.value, " opaque / 项目 ");
    assert_eq!(request.item_uid.value, " UID ");
    assert_eq!(request.item.completed, Some(false));
    assert_eq!(request.item.description, Some(String::new()));
}

#[test]
fn mismatched_item_revisions_and_move_destinations_never_become_requests() {
    assert!(matches!(
        ItemTarget::new(revision("a"), target("b", "same").item),
        Err(PrepareFailure::MismatchedIdentity)
    ));
    for destination in [
        ItemKey::new("b".parse().unwrap(), "same".parse().unwrap()),
        target("a", "same").key().clone(),
    ] {
        assert!(matches!(
            PreparedListMutation::new(ListWriteIntent::Move {
                target: target("a", "same"),
                destination: MoveDestination::After(destination)
            }),
            Err(PrepareFailure::InvalidDestination)
        ));
    }
    let prepared = PreparedListMutation::new(ListWriteIntent::Move {
        target: target("a", "same"),
        destination: MoveDestination::After(ItemKey::new(
            "a".parse().unwrap(),
            "after".parse::<ItemUid>().unwrap(),
        )),
    })
    .unwrap();
    let Request::Move(request) = prepared.request else {
        panic!("move")
    };
    assert!(
        matches!(request.destination, Some(wire::move_item_request::Destination::AfterUid(uid)) if uid.value == "after")
    );
    let prepared = PreparedListMutation::new(ListWriteIntent::Move {
        target: target("a", "same"),
        destination: MoveDestination::AtStart,
    })
    .unwrap();
    let Request::Move(request) = prepared.request else {
        panic!("move")
    };
    assert!(matches!(
        request.destination,
        Some(wire::move_item_request::Destination::AtStart(_))
    ));
}

#[test]
fn invalid_drafts_are_refused_before_creating_a_submission_opportunity() {
    for name in [String::new(), "bad\nname".into(), "é".repeat(257)] {
        assert!(matches!(
            PreparedListMutation::new(ListWriteIntent::Create { name }),
            Err(PrepareFailure::InvalidName)
        ));
    }
    assert!(
        PreparedListMutation::new(ListWriteIntent::Create {
            name: "é".repeat(256)
        })
        .is_ok()
    );
    let mut absent = payload();
    absent.completed = None;
    assert!(matches!(
        PreparedListMutation::new(ListWriteIntent::Add {
            revision: revision("a"),
            payload: absent
        }),
        Err(PrepareFailure::InvalidPayload)
    ));
}

#[test]
fn receipts_must_match_the_exact_target_and_advance_both_revisions() {
    assert!(
        confirm(
            Expected::Item(target("a", "same")),
            receipt("a", Some("same"))
        )
        .is_some()
    );
    assert!(
        confirm(
            Expected::Item(target("a", "same")),
            receipt("b", Some("same"))
        )
        .is_none()
    );
    assert!(
        confirm(
            Expected::Item(target("a", "same")),
            receipt("a", Some("other"))
        )
        .is_none()
    );
    assert!(confirm(Expected::Item(target("a", "same")), receipt("a", None)).is_none());
    let mut stale_list = receipt("a", Some("same"));
    stale_list.list_revision.get_or_insert_default().number = 11;
    assert!(confirm(Expected::Item(target("a", "same")), stale_list).is_none());
    let mut stale_item = receipt("a", Some("same"));
    stale_item.item_revision.get_or_insert_default().number = 7;
    assert!(confirm(Expected::Item(target("a", "same")), stale_item).is_none());
    assert!(confirm(Expected::List(revision("a")), receipt("a", Some("same"))).is_none());
    assert!(confirm(Expected::AddedItem(revision("a")), receipt("a", None)).is_none());
    assert!(confirm(Expected::CreatedList, receipt("new", Some("same"))).is_none());
}

struct Api {
    calls: AtomicUsize,
    response: Mutex<Option<Result<wire::MutationReceipt, SubmitFailure>>>,
}
impl Api {
    fn new(response: Result<wire::MutationReceipt, SubmitFailure>) -> Self {
        Self {
            calls: AtomicUsize::new(0),
            response: Mutex::new(Some(response)),
        }
    }
}
impl SubmitApi for Api {
    async fn submit(&self, _: Request) -> Result<wire::MutationReceipt, SubmitFailure> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.response
            .lock()
            .unwrap()
            .take()
            .expect("a mutation must never be retried")
    }
}
fn typed(code: wire::ListErrorCode) -> Failure {
    Failure::from(
        ConnectError::aborted("untrusted private server text").with_detail(
            ErrorDetail::from_message(
                wire::ListError::FULL_NAME,
                &wire::ListError {
                    code: code.into(),
                    ..Default::default()
                },
            ),
        ),
    )
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
                wire::ListErrorCode::Conflict,
            ))),
            Scenario::Unknown => Err(SubmitFailure::Transport(typed(
                wire::ListErrorCode::CommitUnknown,
            ))),
            Scenario::Withheld => Err(SubmitFailure::Transport(typed(
                wire::ListErrorCode::CommittedResponseWithheld,
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
                ListWriteOutcome::CommittedResponseWithheld
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
    let detail = ErrorDetail::from_message(
        wire::ListError::FULL_NAME,
        &wire::ListError {
            code: wire::ListErrorCode::Conflict.into(),
            ..Default::default()
        },
    );
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
            wire::ListError::FULL_NAME,
            &wire::ListError {
                code: EnumValue::Unknown(999),
                ..Default::default()
            },
        ),
    ));
    assert!(matches!(
        classify(&unknown),
        ListWriteOutcome::CommitUnknown(WriteIssue::UnsupportedResponse)
    ));
    let authority = Failure::from(ConnectError::unauthenticated("private").with_detail(
        ErrorDetail {
            type_url: "type.googleapis.com/home.auth.v1.AuthorizationTermination".into(),
            value: Some("%%%".into()),
            debug: None,
        },
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
            wire::ListError::FULL_NAME,
            &wire::ListError {
                code: wire::ListErrorCode::Conflict.into(),
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
        async fn submit(&self, _: Request) -> Result<wire::MutationReceipt, SubmitFailure> {
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
