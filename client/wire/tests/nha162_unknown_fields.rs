use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{home::lists::v1 as lists, lists_contract};

fn valid_instant() -> lists::DueInstant {
    let mut instant = lists::DueInstant {
        zone: "UTC".into(),
        ..Default::default()
    };
    instant.at.get_or_insert_default().seconds = 1;
    instant
}

fn payload(due: Option<lists::item_payload::Due>) -> lists::ItemPayload {
    lists::ItemPayload {
        summary: "Synthetic future-field item".into(),
        completed: Some(false),
        due,
        ..Default::default()
    }
}

#[test]
fn nha162_future_due_decodes_but_is_not_an_admitted_absent_or_known_due() {
    use lists::item_payload::Due;
    for due in [
        None,
        Some(Due::DueDate("2028-02-29".into())),
        Some(Due::from(valid_instant())),
    ] {
        let original = payload(due);
        assert!(lists_contract::admit_item_payload(&original).is_ok());
        let mut bytes = original.encode_to_vec();
        // ItemPayload field 7 is a decodable future length-delimited variant.
        bytes.extend_from_slice(&[0x3a, 0x01, b'x']);
        let decoded = lists::ItemPayload::decode_from_slice(&bytes).unwrap();
        assert!(!decoded.__buffa_unknown_fields.is_empty());
        assert!(lists_contract::admit_item_payload(&decoded).is_err());
    }
}

#[test]
fn nha162_nested_due_and_completion_unknowns_decode_but_fail_admission() {
    use lists::item_payload::Due;
    let mut bytes = valid_instant().encode_to_vec();
    bytes.extend_from_slice(&[0x1a, 0x01, b'x']);
    let decoded = lists::DueInstant::decode_from_slice(&bytes).unwrap();
    assert!(!decoded.__buffa_unknown_fields.is_empty());
    assert!(lists_contract::admit_item_payload(&payload(Some(Due::from(decoded)))).is_err());

    let original = valid_instant();
    let mut bytes = original.at.as_option().unwrap().encode_to_vec();
    bytes.extend_from_slice(&[0x18, 0x01]);
    let timestamp = Timestamp::decode_from_slice(&bytes).unwrap();
    assert!(!timestamp.__buffa_unknown_fields.is_empty());
    let mut instant = valid_instant();
    *instant.at.get_or_insert_default() = timestamp.clone();
    assert!(lists_contract::admit_item_payload(&payload(Some(Due::from(instant)))).is_err());

    let mut completed = payload(None);
    completed.completed = Some(true);
    *completed.completed_at.get_or_insert_default() = timestamp;
    assert!(lists_contract::admit_item_payload(&completed).is_err());
}
