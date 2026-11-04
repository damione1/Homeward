use buffa::Message;
use home_wire_contract::{home::api::v2 as wire, tasks_contract::*};
fn future<T: Message>(value: &T) -> Vec<u8> {
    let mut bytes = value.encode_to_vec();
    bytes.extend_from_slice(&[0xfa, 0x7f, 0x00]);
    bytes
}
fn task() -> wire::Task {
    wire::Task {
        summary: "Synthetic".to_owned(),
        ..Default::default()
    }
}
#[test]
fn unknown_fields_in_content_dates_instants_and_timestamps_invalidate_protected_projection() {
    assert!(parse_task_content(&task()).is_ok());
    let value = wire::Task::decode_from_slice(&future(&task())).unwrap();
    assert!(parse_task_content(&value).is_err());
    let date = home_wire_contract::google::r#type::Date {
        year: 2026,
        month: 11,
        day: 3,
        ..Default::default()
    };
    let date = home_wire_contract::google::r#type::Date::decode_from_slice(&future(&date)).unwrap();
    let mut value = task();
    value.due = Some(wire::task::Due::DueDate(Box::new(date)));
    assert!(parse_task_content(&value).is_err());
    let timestamp = buffa_types::google::protobuf::Timestamp::default();
    let unknown =
        buffa_types::google::protobuf::Timestamp::decode_from_slice(&future(&timestamp)).unwrap();
    let instant = wire::TaskDueInstant {
        due_time: unknown.into(),
        time_zone: "Etc/UTC".to_owned(),
        ..Default::default()
    };
    value.due = Some(wire::task::Due::DueInstant(Box::new(instant)));
    assert!(parse_task_content(&value).is_err());
    let instant = wire::TaskDueInstant {
        due_time: timestamp.into(),
        time_zone: "Etc/UTC".to_owned(),
        ..Default::default()
    };
    let unknown = wire::TaskDueInstant::decode_from_slice(&future(&instant)).unwrap();
    value.due = Some(wire::task::Due::DueInstant(Box::new(unknown)));
    assert!(parse_task_content(&value).is_err());
}
#[test]
fn watch_unknown_fields_nonpositive_sequence_and_unrecognized_frame_are_not_actionable() {
    let value = wire::WatchTaskListsResponse {
        sequence: 1,
        frame: Some(wire::watch_task_lists_response::Frame::Ready(
            Default::default(),
        )),
        ..Default::default()
    };
    assert!(parse_task_watch(&value).is_ok());
    let unknown = wire::WatchTaskListsResponse::decode_from_slice(&future(&value)).unwrap();
    assert!(parse_task_watch(&unknown).is_err());
    for sequence in [-1, 0] {
        let mut value = value.clone();
        value.sequence = sequence;
        assert!(parse_task_watch(&value).is_err());
    }
    let mut missing = value;
    missing.frame = None;
    assert!(parse_task_watch(&missing).is_err());
}
