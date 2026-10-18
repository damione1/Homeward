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
        Err("unsupported_capability")
    );
    let c: actions::CommandRequest = roundtrip("command");
    assert!(home_wire_contract::validate_command(&c, "epoch-a", c.config_revision).is_ok());
    assert_eq!(
        home_wire_contract::validate_command(&c, "epoch-b", c.config_revision),
        Err("stale_epoch")
    );
    assert_eq!(
        home_wire_contract::validate_command(&c, "epoch-a", c.config_revision + 1),
        Err("stale_configuration")
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
            Err("invalid_target")
        );
    }
}
