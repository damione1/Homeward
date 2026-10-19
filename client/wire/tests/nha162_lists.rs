use buffa::{EnumValue, Message};
use home_wire_contract::{home::lists::v1 as lists, lists_contract};

#[test]
fn nha162_false_presence_and_civil_date_survive_wire() {
    let mut payload = lists::ItemPayload {
        summary: "Synthetic item".into(),
        completed: Some(false),
        ..Default::default()
    };
    let encoded = payload.encode_to_vec();
    let decoded = lists::ItemPayload::decode_from_slice(&encoded).unwrap();
    assert_eq!(decoded.completed, Some(false));
    assert!(lists_contract::admit_item_payload(&decoded).is_ok());

    payload.completed = None;
    assert!(lists_contract::admit_item_payload(&payload).is_err());
    payload.completed = Some(false);
    payload.due = Some(lists::item_payload::Due::DueDate("2026-02-29".into()));
    assert!(lists_contract::admit_item_payload(&payload).is_err());
    payload.due = Some(lists::item_payload::Due::DueDate("2028-02-29".into()));
    assert!(lists_contract::admit_item_payload(&payload).is_ok());
}

#[test]
fn nha162_list_revision_origin_and_unknown_values_are_closed() {
    let mut snapshot = lists::ListSnapshot {
        origin: lists::ListOrigin::LocalTodo.into(),
        legacy_alias: Some("todo.brico".into()),
        name: "Synthetic tasks".into(),
        ..Default::default()
    };
    snapshot.id.get_or_insert_default().value = "list-synthetic".into();
    snapshot.revision.get_or_insert_default().number = 1;
    snapshot
        .revision
        .get_or_insert_default()
        .list_id
        .get_or_insert_default()
        .value = "list-synthetic".into();
    snapshot.updated_at.get_or_insert_default().seconds = 1;
    assert!(lists_contract::admit_list_snapshot(&snapshot).is_ok());

    snapshot
        .revision
        .get_or_insert_default()
        .list_id
        .get_or_insert_default()
        .value = "list-other".into();
    assert!(lists_contract::admit_list_snapshot(&snapshot).is_err());
    snapshot
        .revision
        .get_or_insert_default()
        .list_id
        .get_or_insert_default()
        .value = "list-synthetic".into();
    snapshot.origin = lists::ListOrigin::Native.into();
    assert!(lists_contract::admit_list_snapshot(&snapshot).is_err());
    snapshot.legacy_alias = None;
    assert!(lists_contract::admit_list_snapshot(&snapshot).is_ok());
    snapshot.origin = EnumValue::Unknown(99);
    assert!(lists_contract::admit_list_snapshot(&snapshot).is_err());
}
