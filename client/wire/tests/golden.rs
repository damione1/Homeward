use buffa::Message;
use home_wire_contract::home::{
    actions::v1 as actions, registry::v1 as registry, state::v1 as state,
};
fn golden(name: &str) -> Vec<u8> {
    std::fs::read(format!(
        "{}/../../contracts/golden/{name}.bin",
        env!("CARGO_MANIFEST_DIR")
    ))
    .unwrap()
}
fn roundtrip<M: Message + Default>(name: &str) -> M {
    let bytes = golden(name);
    let m = M::decode_from_slice(&bytes).unwrap();
    assert_eq!(m.encode_to_vec(), bytes);
    m
}
#[test]
fn command_preserves_uint64_presence_and_timestamp() {
    let c: actions::CommandRequest = roundtrip("command");
    assert_eq!(c.config_revision, 9_007_199_254_740_993);
    assert_eq!(c.expected_revision, Some(0));
    assert_eq!(c.expires_at.nanos, 123_456_789);
    let mut bytes = golden("command");
    bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
    assert_eq!(
        actions::CommandRequest::decode_from_slice(&bytes)
            .unwrap()
            .encode_to_vec(),
        bytes
    );
}
#[test]
fn unknown_values_remain_readable_and_extension_execution_is_refused() {
    let s: state::ObservedState = roundtrip("unknown-quality");
    assert_eq!(s.quality.availability.to_i32(), 99);
    assert_eq!(s.revision, u64::MAX);
    assert_eq!(s.source_seq, Some(0));
    let d: registry::CapabilityDescription = roundtrip("unknown-description");
    assert_eq!(d.display_name, "Synthetic readable capability");
    let c: actions::CommandRequest = roundtrip("unknown-extension");
    assert_eq!(
        home_wire_contract::validate_command(&c, "epoch-a", c.config_revision),
        Err(rejection(
            actions::ErrorCode::UnsupportedCapability,
            "capability_schema_id"
        ))
    );
    let c: actions::CommandRequest = roundtrip("command");
    assert!(home_wire_contract::validate_command(&c, "epoch-a", c.config_revision).is_ok());
    assert_eq!(
        home_wire_contract::validate_command(&c, "epoch-b", c.config_revision),
        Err(rejection(actions::ErrorCode::StaleEpoch, "admission_epoch"))
    );
    assert_eq!(
        home_wire_contract::validate_command(&c, "epoch-a", c.config_revision + 1),
        Err(rejection(
            actions::ErrorCode::StaleConfiguration,
            "config_revision"
        ))
    );
}
#[test]
fn pages_share_generation_and_filtered_cursor_is_not_stream_sequence() {
    let a: state::OpenViewResponse = roundtrip("snapshot-page-1");
    let b: state::ReadPageResponse = roundtrip("snapshot-page-2");
    assert_eq!(a.cursor, b.page.cursor);
    assert_eq!(a.snapshot_token, b.page.snapshot_token);
    assert!(!a.next_page_token.is_empty());
    assert!(b.page.next_page_token.is_empty());
    let delta: state::WatchViewResponse = roundtrip("filtered-stream");
    assert_eq!(delta.stream_sequence, 1);
    assert_eq!(delta.cursor.global_sequence, 108);
    let terminal: state::WatchViewResponse = roundtrip("resync-required");
    assert_eq!(terminal.stream_sequence, 2);
    assert!(matches!(
        terminal.frame,
        Some(state::watch_view_response::Frame::ResyncRequired(_))
    ));
    let _: actions::ContractError = roundtrip("structured-error");
}

#[test]
fn empty_and_absent_references_are_rejected() {
    for name in ["empty-entity-id", "empty-alias", "absent-target"] {
        let c: actions::CommandRequest = roundtrip(name);
        assert_eq!(
            home_wire_contract::validate_command(&c, "epoch-a", c.config_revision),
            Err(rejection(actions::ErrorCode::InvalidArgument, "target"))
        );
    }
}

fn rejection(code: actions::ErrorCode, path: &str) -> Box<actions::ContractError> {
    Box::new(actions::ContractError {
        code: code.into(),
        field_path: path.into(),
        message: "command rejected by contract".into(),
        ..Default::default()
    })
}

#[test]
fn admission_rejects_missing_and_unknown_branches_without_weakening_wire_decode() {
    use actions::action_args::Arguments;
    use registry::entity_ref::Reference;
    let original: actions::CommandRequest = roundtrip("command");
    let mut cases = Vec::new();
    let mut c = original.clone();
    c.command_id.clear();
    cases.push((c, "command_id"));
    let mut c = original.clone();
    c.admission_epoch.clear();
    cases.push((c, "admission_epoch"));
    let mut c = original.clone();
    c.target = Default::default();
    cases.push((c, "target"));
    let mut c = original.clone();
    c.arguments = Default::default();
    cases.push((c, "arguments"));
    let mut c = original.clone();
    // Unknown future oneof branch remains decodable and round-trippable.
    let args = actions::ActionArgs::decode_from_slice(&[0x1a, 0]).unwrap();
    assert_eq!(args.encode_to_vec(), [0x1a, 0]);
    c.arguments = args.into();
    cases.push((c, "arguments"));
    let mut c = original.clone();
    c.arguments = actions::ActionArgs {
        arguments: Some(Arguments::Extension(Box::default())),
        ..Default::default()
    }
    .into();
    cases.push((c, "arguments"));
    for (c, path) in cases {
        assert_eq!(
            home_wire_contract::validate_command(&c, "epoch-a", c.config_revision),
            Err(rejection(actions::ErrorCode::InvalidArgument, path))
        );
    }
    let mut c = original.clone();
    c.admission_epoch.clear();
    assert_eq!(
        home_wire_contract::validate_command(&c, "", c.config_revision),
        Err(rejection(
            actions::ErrorCode::InvalidArgument,
            "admission_epoch"
        ))
    );
    for reference in [
        Reference::EntityId("Host:opaque/É-42".into()),
        Reference::HaAlias("switch.synthetic".into()),
    ] {
        let mut c = original.clone();
        c.target = registry::EntityRef {
            reference: Some(reference),
            ..Default::default()
        }
        .into();
        c.config_revision = 0;
        assert!(home_wire_contract::validate_command(&c, "epoch-a", 0).is_ok());
    }
}
