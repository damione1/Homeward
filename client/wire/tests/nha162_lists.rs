//! TaskService v2 resources preserve exact identities and independent editable values.
use buffa::Message;
use home_client_model::{
    api::HomeName,
    api_tasks::*,
    local_time::{DisplayTimeZone, UtcInstant},
};
use home_wire_contract::{home::api::v2 as wire, tasks_contract::*};
fn name() -> TaskName {
    TaskName::new(
        TaskListName::new(
            HomeName::try_from("homes/synthetic").unwrap(),
            " list / été ".parse().unwrap(),
        ),
        "same UID".parse().unwrap(),
    )
}
fn task() -> wire::Task {
    wire::Task {
        name: name().as_str().to_owned(),
        etag: "\"opaque-parent-and-child\"".to_owned(),
        summary: "Synthetic".to_owned(),
        description: Some(String::new()),
        ..Default::default()
    }
}
#[test]
fn canonical_names_opaque_etags_zero_false_and_optional_empty_survive_wire_roundtrip() {
    let original = task();
    let decoded = wire::Task::decode_from_slice(&original.encode_to_vec()).unwrap();
    let admitted = parse_task(&decoded).unwrap();
    assert_eq!(admitted.name(), &name());
    assert_eq!(admitted.key().list().as_str(), " list / été ");
    assert!(!admitted.content().completed());
    assert_eq!(admitted.position(), 0);
    assert_eq!(admitted.content().description().unwrap().as_str(), "");
    assert!(admitted.completion_time().is_none());
    assert_eq!(encode_task(&admitted), original);
    for etag in [
        "",
        "unquoted",
        "W/\"weak\"",
        "\"embedded\"quote\"",
        "\"line\nfeed\"",
    ] {
        let mut value = task();
        value.etag = etag.to_owned();
        assert!(parse_task(&value).is_err());
    }
}
#[test]
fn civil_dates_and_explicit_iana_instants_keep_distinct_content_without_child_identity_on_append() {
    let zone: DisplayTimeZone = "America/Toronto".parse().unwrap();
    for due in [
        TaskDue::None,
        TaskDue::Date("2026-11-03".parse().unwrap()),
        TaskDue::Instant(TaskDueInstant::new(UtcInstant::new(0, 0).unwrap(), zone).unwrap()),
    ] {
        let content = TaskContent::new(TaskText::try_from("Synthetic").unwrap(), false, None, due);
        let encoded = encode_task_content(&content);
        assert_eq!(parse_task_content(&encoded).unwrap(), content);
        let append = encode_append_content(&content);
        assert_eq!(append.summary, "Synthetic");
        assert!(!append.completed);
        assert!(append.description.is_none());
    }
}
#[test]
fn unsupported_origin_and_inconsistent_completion_never_admit_resources() {
    let mut value = task();
    value.completion_time = buffa_types::google::protobuf::Timestamp {
        seconds: 1,
        ..Default::default()
    }
    .into();
    assert!(parse_task(&value).is_err());
    value.completed = true;
    assert!(parse_task(&value).is_ok());
    let mut list = wire::TaskList {
        name: name().parent().as_str().to_owned(),
        etag: "\"opaque\"".to_owned(),
        display_name: "List".to_owned(),
        update_time: buffa_types::google::protobuf::Timestamp {
            seconds: 1,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    assert!(parse_task_list(&list).is_err());
    list.origin = wire::TaskListOrigin::Native.into();
    assert!(parse_task_list(&list).is_ok());
    list.origin = buffa::EnumValue::Unknown(999);
    assert!(parse_task_list(&list).is_err());
}
