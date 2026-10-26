use buffa::Message;
use home_wire_contract::home::{
    actions::v1 as actions, helpers::v1 as helpers, registry::v1 as registry, state::v1 as state,
};
fn golden(name: &str) -> Vec<u8> {
    let cwd = std::env::current_dir().expect("cannot locate golden fixture: no current directory");
    let path = cwd.join(format!("../../contracts/golden/{name}.bin"));
    std::fs::read(&path).unwrap_or_else(|error| {
        panic!(
            "cannot read golden fixture {} from test cwd {} (expected client/wire): {error}",
            path.display(),
            cwd.display()
        )
    })
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

fn helper_action_mut(request: &mut actions::CommandRequest) -> &mut helpers::HelperAction {
    let args = request
        .arguments
        .as_option_mut()
        .expect("missing action args");
    let Some(actions::action_args::Arguments::Helper(action)) = args.arguments.as_mut() else {
        panic!("missing helper action")
    };
    action
}

fn with_future_field<M: Message + Default>(message: &M) -> M {
    let mut bytes = message.encode_to_vec();
    bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
    let decoded = M::decode_from_slice(&bytes).unwrap();
    assert_eq!(decoded.encode_to_vec(), bytes);
    decoded
}

#[test]
fn nha097_helper_wire_presence_and_command_admission() {
    use actions::action_args::Arguments;
    use helpers::helper_value::Value;

    let original: actions::CommandRequest = roundtrip("command");
    let mut helper = original.clone();
    helper.capability_schema_id = "core.helper.v1".into();
    let action = helpers::HelperAction {
        operation: helpers::HelperOperation::Set.into(),
        expected_helper_revision: Some(0),
        value: helpers::HelperValue {
            value: Some(Value::Boolean(false)),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    helper.arguments = actions::ActionArgs {
        arguments: Some(Arguments::Helper(Box::new(action))),
        ..Default::default()
    }
    .into();
    let accept = |request: &actions::CommandRequest| {
        let bytes = request.encode_to_vec();
        let decoded = actions::CommandRequest::decode_from_slice(&bytes).unwrap();
        assert_eq!(decoded.encode_to_vec(), bytes);
        assert!(
            home_wire_contract::validate_command(&decoded, "epoch-a", decoded.config_revision)
                .is_ok()
        );
    };
    for value in [
        Value::Boolean(false),
        Value::Number(0.0),
        Value::Text(String::new()),
    ] {
        helper_action_mut(&mut helper).value = helpers::HelperValue {
            value: Some(value),
            ..Default::default()
        }
        .into();
        accept(&helper);
    }
    let date = helpers::CivilDate {
        year: 2026,
        month: 10,
        day: 19,
        ..Default::default()
    };
    let time = helpers::CivilTime {
        hour: 8,
        minute: 30,
        ..Default::default()
    };
    let datetime = helpers::CivilDateTime {
        date: date.clone().into(),
        time: time.clone().into(),
        ..Default::default()
    };
    for value in [
        Value::Date(Box::new(date.clone())),
        Value::Time(Box::new(time.clone())),
        Value::Datetime(Box::new(datetime.clone())),
    ] {
        helper_action_mut(&mut helper).value = helpers::HelperValue {
            value: Some(value),
            ..Default::default()
        }
        .into();
        accept(&helper);
    }
    let mut wrong_family = helper.clone();
    wrong_family.capability_schema_id = "core.switch.v1".into();
    assert_eq!(
        home_wire_contract::validate_command(
            &wrong_family,
            "epoch-a",
            wrong_family.config_revision
        ),
        Err(rejection(actions::ErrorCode::InvalidArgument, "arguments"))
    );
    let mut wrong_family = original;
    wrong_family.capability_schema_id = "core.helper.v1".into();
    assert_eq!(
        home_wire_contract::validate_command(
            &wrong_family,
            "epoch-a",
            wrong_family.config_revision
        ),
        Err(rejection(actions::ErrorCode::InvalidArgument, "arguments"))
    );
    let mut missing_revision = helper.clone();
    helper_action_mut(&mut missing_revision).expected_helper_revision = None;
    assert_eq!(
        home_wire_contract::validate_command(
            &missing_revision,
            "epoch-a",
            missing_revision.config_revision
        ),
        Err(rejection(
            actions::ErrorCode::InvalidArgument,
            "arguments.helper.expected_helper_revision"
        ))
    );
    let mut excessive_revision = helper.clone();
    helper_action_mut(&mut excessive_revision).expected_helper_revision = Some(i64::MAX as u64 + 1);
    assert_eq!(
        home_wire_contract::validate_command(
            &excessive_revision,
            "epoch-a",
            excessive_revision.config_revision
        ),
        Err(rejection(
            actions::ErrorCode::InvalidArgument,
            "arguments.helper"
        ))
    );
    for operation in [
        buffa::EnumValue::Known(helpers::HelperOperation::Unspecified),
        buffa::EnumValue::Unknown(99),
    ] {
        let mut rejected = helper.clone();
        helper_action_mut(&mut rejected).operation = operation;
        assert_eq!(
            home_wire_contract::validate_command(&rejected, "epoch-a", rejected.config_revision),
            Err(rejection(
                actions::ErrorCode::InvalidArgument,
                "arguments.helper.operation"
            ))
        );
    }
    let mut future_value = helper.clone();
    let mut value_bytes = helper_action_mut(&mut future_value)
        .value
        .as_option()
        .unwrap()
        .encode_to_vec();
    value_bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
    helper_action_mut(&mut future_value).value =
        helpers::HelperValue::decode_from_slice(&value_bytes)
            .unwrap()
            .into();
    assert_eq!(
        home_wire_contract::validate_command(
            &future_value,
            "epoch-a",
            future_value.config_revision
        ),
        Err(rejection(
            actions::ErrorCode::InvalidArgument,
            "arguments.helper.value"
        ))
    );
    let mut future_action = helper.clone();
    let mut action_bytes = helper_action_mut(&mut future_action).encode_to_vec();
    action_bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
    future_action.arguments = actions::ActionArgs {
        arguments: Some(Arguments::Helper(Box::new(
            helpers::HelperAction::decode_from_slice(&action_bytes).unwrap(),
        ))),
        ..Default::default()
    }
    .into();
    assert_eq!(
        home_wire_contract::validate_command(
            &future_action,
            "epoch-a",
            future_action.config_revision
        ),
        Err(rejection(
            actions::ErrorCode::InvalidArgument,
            "arguments.helper"
        ))
    );
    for value in [
        Value::Date(Box::new(with_future_field(&date))),
        Value::Time(Box::new(with_future_field(&time))),
        Value::Datetime(Box::new(with_future_field(&datetime))),
        Value::Datetime(Box::new(helpers::CivilDateTime {
            date: with_future_field(&date).into(),
            time: time.clone().into(),
            ..Default::default()
        })),
        Value::Datetime(Box::new(helpers::CivilDateTime {
            date: date.clone().into(),
            time: with_future_field(&time).into(),
            ..Default::default()
        })),
    ] {
        let mut future_civil = helper.clone();
        helper_action_mut(&mut future_civil).value = helpers::HelperValue {
            value: Some(value),
            ..Default::default()
        }
        .into();
        assert_eq!(
            home_wire_contract::validate_command(
                &future_civil,
                "epoch-a",
                future_civil.config_revision
            ),
            Err(rejection(
                actions::ErrorCode::InvalidArgument,
                "arguments.helper.value"
            ))
        );
    }
    let mut no_value = helper.clone();
    helper_action_mut(&mut no_value).value = Default::default();
    assert_eq!(
        home_wire_contract::validate_command(&no_value, "epoch-a", no_value.config_revision),
        Err(rejection(
            actions::ErrorCode::InvalidArgument,
            "arguments.helper.value"
        ))
    );
    helper_action_mut(&mut no_value).operation = helpers::HelperOperation::Toggle.into();
    accept(&no_value);
    helper_action_mut(&mut no_value).value = helpers::HelperValue {
        value: Some(Value::Boolean(false)),
        ..Default::default()
    }
    .into();
    assert_eq!(
        home_wire_contract::validate_command(&no_value, "epoch-a", no_value.config_revision),
        Err(rejection(
            actions::ErrorCode::InvalidArgument,
            "arguments.helper.value"
        ))
    );
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
            Some(Constraints::Helper(_)) => panic!("unexpected helper in core fixture"),
            Some(Constraints::Solar(_)) => panic!("unexpected solar in core fixture"),
            Some(Constraints::Cover(_) | Constraints::Lock(_) | Constraints::Alarm(_)) => {
                panic!("unexpected control in core fixture")
            }
            Some(
                Constraints::Weather(_)
                | Constraints::Presence(_)
                | Constraints::PresenceDetails(_),
            ) => {
                panic!("unexpected environmental observation in core fixture")
            }
            Some(Constraints::SolarEvent(_)) => panic!("unexpected solar event in core fixture"),
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
