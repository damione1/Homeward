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
    let target = registry::EntityRef::decode_from_slice(&[0x9a, 0x06, 0]).unwrap();
    assert_eq!(target.encode_to_vec(), [0x9a, 0x06, 0]);
    assert!(target.reference.is_none());
    c.target = target.into();
    cases.push((c, "target"));
    let mut c = original.clone();
    c.arguments = Default::default();
    cases.push((c, "arguments"));
    let mut c = original.clone();
    // Unknown future oneof branch remains decodable and round-trippable.
    let args = actions::ActionArgs::decode_from_slice(&[0x9a, 0x06, 0]).unwrap();
    assert_eq!(args.encode_to_vec(), [0x9a, 0x06, 0]);
    assert!(args.arguments.is_none());
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

#[test]
fn nha014_capability_constraints_and_action_presence_survive_rust_codec() {
    use registry::capability_description::Constraints;
    let view: state::OpenViewResponse = roundtrip("capability-instances");
    assert_eq!(view.capabilities.len(), 6);
    for capability in &view.capabilities {
        match &capability.constraints {
            Some(Constraints::Switch(c)) => {
                assert_eq!(capability.schema_id, "core.switch.v1");
                assert!(c.set_on);
            }
            Some(Constraints::Light(c)) => {
                assert_eq!(capability.schema_id, "core.light.v1");
                assert!(c.set_on);
                let bounds = c.brightness_percent.as_option().unwrap();
                assert_eq!(
                    (bounds.minimum, bounds.maximum, bounds.step),
                    (0.0, 100.0, Some(1.0))
                );
            }
            Some(Constraints::Fan(c)) => {
                assert_eq!(capability.schema_id, "core.fan.v1");
                assert!(!c.set_on);
                assert_eq!(c.speed_options, ["low", "high"]);
            }
            Some(Constraints::Climate(c)) => {
                assert_eq!(capability.schema_id, "core.climate.v1");
                let bounds = c.setpoint_celsius.as_option().unwrap();
                assert_eq!(
                    (bounds.minimum, bounds.maximum, bounds.step),
                    (16.0, 30.0, Some(0.5))
                );
                assert_eq!(c.mode_options.len(), 2);
                assert_eq!(c.mode_options[0], registry::ClimateMode::Off);
                assert_eq!(c.mode_options[1], registry::ClimateMode::Heat);
                assert_eq!(c.activity_options.len(), 2);
                assert_eq!(c.activity_options[0], registry::ClimateActivity::Idle);
                assert_eq!(c.activity_options[1], registry::ClimateActivity::Heating);
            }
            Some(Constraints::Measurement(c)) => {
                assert_eq!(capability.schema_id, "core.measurement.v1");
                assert_eq!(c.unit, "W");
                assert_eq!(c.precision, 1);
                assert_eq!(c.semantic, registry::MeasurementSemantic::Measurement);
                assert_eq!(c.aggregation, registry::MeasurementAggregation::Current);
                let bounds = c.value.as_option().unwrap();
                assert_eq!(
                    (bounds.minimum, bounds.maximum, bounds.step),
                    (0.0, 2000.0, None)
                );
            }
            Some(Constraints::Binary(c)) => {
                assert_eq!(capability.schema_id, "core.binary.v1");
                assert_eq!(c.device_class, None);
            }
            Some(Constraints::Extension(_)) => panic!("unexpected extension in core fixture"),
            None => panic!("missing constraints"),
        }
    }
    let light: actions::CommandRequest = roundtrip("light-command");
    match &light.arguments.arguments {
        Some(actions::action_args::Arguments::SetLight(value)) => {
            assert_eq!(value.on, Some(false));
            assert_eq!(value.brightness_percent, Some(0.0));
        }
        _ => panic!("light action branch lost"),
    }
    let extension: registry::CapabilityDescription = roundtrip("extension-description");
    assert_eq!(extension.schema_id, "vendor.example.v1");
    assert!(!extension.descriptor_set.is_empty());
    assert_eq!(extension.descriptor_sha256.len(), 32);
    match extension.constraints {
        Some(Constraints::Extension(value)) => {
            assert_eq!(
                value.action_message.as_deref(),
                Some("vendor.example.v1.SetLevel")
            );
        }
        _ => panic!("extension descriptor lost its typed action reference"),
    }
}
