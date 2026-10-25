use buffa::Message;
use home_client_actions::{ActionRecord, CommandId, LiveState, SubmissionState, TrackingError};
use home_wire_contract::{actions_contract as admit, home::actions::v1 as wire};

const EPOCH: &str = "aabbccddeeff00112233445566778899";
const ID: &str = "opaque command identity";

fn record() -> ActionRecord {
    ActionRecord::new(ID.parse().unwrap(), EPOCH, 7).unwrap()
}
fn response(status: wire::ActionStatus) -> wire::CommandResponse {
    wire::CommandResponse {
        command_id: ID.into(),
        status: status.into(),
        ..Default::default()
    }
}
fn point(status: wire::ActionStatus, sequence: u64) -> wire::ActionProgress {
    wire::ActionProgress {
        admission_epoch: EPOCH.into(),
        config_revision: 7,
        status: status.into(),
        sequence,
        ..Default::default()
    }
}
fn frame(
    status: wire::ActionStatus,
    sequence: u64,
    stream_sequence: u64,
) -> wire::WatchActionResponse {
    wire::WatchActionResponse {
        stream_sequence,
        frame: Some(wire::watch_action_response::Frame::Progress(
            point(status, sequence).into(),
        )),
        ..Default::default()
    }
}
fn unavailable(durable: wire::DurableActionResult) -> wire::GetActionResponse {
    wire::GetActionResponse {
        durable: Some(durable).into(),
        live: Some(wire::get_action_response::Live::Unavailable(
            wire::NoLiveActionProgress::default().into(),
        )),
        ..Default::default()
    }
}

#[test]
fn ack_and_even_a_dispatch_record_named_confirmed_are_not_physical_confirmation() {
    let mut action = record();
    action.begin_submission().unwrap();
    action
        .submitted(wire::SubmitActionResponse {
            result: Some(response(wire::ActionStatus::Acknowledged)).into(),
            ..Default::default()
        })
        .unwrap();
    assert!(!action.confirmed());
    action
        .got(unavailable(wire::DurableActionResult {
            state: wire::DurableActionState::Result.into(),
            response: Some(response(wire::ActionStatus::Confirmed)).into(),
            error: wire::DurableActionError::None.into(),
            ..Default::default()
        }))
        .unwrap();
    assert_eq!(action.live(), LiveState::Unavailable);
    assert!(
        !action.confirmed(),
        "a dispatch record never asserts a physical effect"
    );
    assert_eq!(
        action.response().unwrap().status,
        wire::ActionStatus::Acknowledged
    );
    assert_eq!(
        action.begin_submission(),
        Err(TrackingError::AlreadySubmitted)
    );
}

#[test]
fn lost_submit_and_read_only_recovery_keep_the_exact_id_without_rearming_submission() {
    let mut action = record();
    action.begin_submission().unwrap();
    action
        .submission_uncertain(Some(wire::ActionOperationFailure {
            kind: wire::ActionOperationFailureKind::CommitOutcomeUnknown.into(),
            response: Some(response(wire::ActionStatus::Acknowledged)).into(),
            ..Default::default()
        }))
        .unwrap();
    assert_eq!(action.submission(), SubmissionState::Uncertain);
    assert_eq!(action.id().as_str(), ID);
    action
        .got(unavailable(wire::DurableActionResult {
            state: wire::DurableActionState::Indeterminate.into(),
            response: Some(response(wire::ActionStatus::Indeterminate)).into(),
            error: wire::DurableActionError::Deadline.into(),
            ..Default::default()
        }))
        .unwrap();
    assert_eq!(
        action.durable().unwrap().error,
        wire::DurableActionError::Deadline
    );
    assert!(!action.confirmed());
    action.interrupted();
    action.begin_watch().unwrap();
    assert_eq!(
        action.begin_submission(),
        Err(TrackingError::AlreadySubmitted)
    );
    let mut recovered = ActionRecord::recover(action.id().clone(), EPOCH, 7).unwrap();
    assert_eq!(recovered.id(), action.id());
    assert_eq!(
        recovered.begin_submission(),
        Err(TrackingError::AlreadySubmitted)
    );
    assert!(!format!("{:?}", recovered.id()).contains(ID));
}

#[test]
fn action_sequence_survives_stream_reconnect_but_foreign_or_reordered_points_are_rejected() {
    let mut action = record();
    action.begin_submission().unwrap();
    action.submission_uncertain(None).unwrap();
    action.begin_watch().unwrap();
    action
        .watched(frame(wire::ActionStatus::Acknowledged, 3, 1))
        .unwrap();
    action.interrupted();
    action.begin_watch().unwrap();
    action
        .watched(frame(wire::ActionStatus::Acknowledged, 3, 1))
        .unwrap();
    assert_eq!(
        action.watched(frame(wire::ActionStatus::Dispatched, 2, 2)),
        Err(TrackingError::OutOfOrder)
    );
    let mut foreign = frame(wire::ActionStatus::Confirmed, 4, 2);
    let Some(wire::watch_action_response::Frame::Progress(value)) = &mut foreign.frame else {
        panic!("progress fixture")
    };
    value.admission_epoch = "11223344556677889900112233445566".into();
    assert_eq!(action.watched(foreign), Err(TrackingError::ForeignContext));
    assert!(!action.confirmed());
    action
        .watched(frame(wire::ActionStatus::Confirmed, 4, 2))
        .unwrap();
    assert!(action.confirmed());
    assert_eq!(
        action.watched(frame(wire::ActionStatus::Failed, 5, 3)),
        Err(TrackingError::OutOfOrder)
    );
    assert!(
        action.confirmed(),
        "a contradictory late frame does not replace an admitted terminal point"
    );
}

#[test]
fn foreign_get_and_future_nested_fields_leave_the_previous_action_untouched() {
    let mut action = ActionRecord::recover(ID.parse().unwrap(), EPOCH, 7).unwrap();
    let mut foreign = unavailable(wire::DurableActionResult {
        state: wire::DurableActionState::Accepted.into(),
        response: Some(response(wire::ActionStatus::Accepted)).into(),
        error: wire::DurableActionError::None.into(),
        ..Default::default()
    });
    foreign
        .durable
        .as_option_mut()
        .unwrap()
        .response
        .as_option_mut()
        .unwrap()
        .command_id = "another command".into();
    assert_eq!(action.got(foreign), Err(TrackingError::ForeignCommand));
    assert!(action.durable().is_none());
    let mut future = frame(wire::ActionStatus::Accepted, 1, 1);
    let Some(wire::watch_action_response::Frame::Progress(value)) = &mut future.frame else {
        panic!("progress fixture")
    };
    let mut encoded = value.encode_to_vec();
    encoded.extend([0xf8, 0x07, 0x01]);
    **value = wire::ActionProgress::decode_from_slice(&encoded).unwrap();
    assert_eq!(action.watched(future), Err(TrackingError::InvalidResponse));
    assert!(action.progress().is_none());
    assert_eq!(action.live(), LiveState::NotRequested);
}

#[test]
fn stop_is_local_tracking_only_and_closed_streams_cannot_revive_it() {
    let mut action = record();
    assert_eq!(action.begin_watch(), Err(TrackingError::NotSubmitted));
    action.begin_submission().unwrap();
    action.submission_uncertain(None).unwrap();
    action.stop_tracking();
    action.interrupted();
    assert_eq!(action.live(), LiveState::Stopped);
    assert_eq!(
        action.watched(frame(wire::ActionStatus::Confirmed, 4, 1)),
        Err(TrackingError::Stopped)
    );
    assert_eq!(action.begin_watch(), Err(TrackingError::Stopped));
    assert_eq!(action.submission(), SubmissionState::Uncertain);
    assert_eq!(
        action.begin_submission(),
        Err(TrackingError::AlreadySubmitted)
    );
}

#[test]
fn unknown_action_branches_and_inconsistent_durable_records_are_never_admitted() {
    let mut future = frame(wire::ActionStatus::Accepted, 1, 1);
    let Some(wire::watch_action_response::Frame::Progress(value)) = &mut future.frame else {
        panic!("progress fixture")
    };
    value.status = 999.into();
    assert!(admit::watch_response(&future).is_err());
    let inconsistent = wire::DurableActionResult {
        state: wire::DurableActionState::Accepted.into(),
        response: Some(response(wire::ActionStatus::Confirmed)).into(),
        error: wire::DurableActionError::None.into(),
        ..Default::default()
    };
    assert!(admit::durable(&inconsistent).is_err());
    let no_live = wire::WatchActionResponse {
        stream_sequence: 2,
        frame: Some(wire::watch_action_response::Frame::Unavailable(
            wire::NoLiveActionProgress::default().into(),
        )),
        ..Default::default()
    };
    assert!(admit::watch_response(&no_live).is_err());
}

#[test]
fn opaque_command_ids_are_exact_bounded_and_generated_without_secrets_in_debug() {
    assert!("".parse::<CommandId>().is_err());
    assert!("a".repeat(257).parse::<CommandId>().is_err());
    assert_eq!(
        " Exact identity ".parse::<CommandId>().unwrap().as_str(),
        " Exact identity "
    );
    let first = CommandId::generate().unwrap();
    let second = CommandId::generate().unwrap();
    assert_ne!(first, second);
    assert_eq!(first.as_str().len(), 32);
}
