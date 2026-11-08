//! Current public binary corpus. Removed State/Action envelopes are not a
//! private native contract and must never be reintroduced to decode old goldens.
use buffa::{EnumValue, Message};
use home_wire_contract::{
    api_contract::{encode_command, parse_command},
    api_observations::parse_quality,
    home::api::v2 as wire,
};

fn command(name: &str) -> wire::ExecuteCapabilityRequest {
    let line = include_str!("../../../contracts/api_v2_commands.tsv")
        .lines()
        .find(|line| line.split('\t').next() == Some(name))
        .expect("current golden");
    let hex = line.split('\t').nth(1).unwrap();
    let bytes: Vec<_> = hex
        .as_bytes()
        .chunks_exact(2)
        .map(|v| u8::from_str_radix(std::str::from_utf8(v).unwrap(), 16).unwrap())
        .collect();
    let request = wire::ExecuteCapabilityRequest::decode_from_slice(&bytes).unwrap();
    // Field order in a cross-language corpus need not equal encoder order;
    // the decoded message must survive a canonical binary round trip exactly.
    assert_eq!(roundtrip(&request), request);
    request
}
fn roundtrip<M: Message + Default>(message: &M) -> M {
    let bytes = message.encode_to_vec();
    let decoded = M::decode_from_slice(&bytes).unwrap();
    assert_eq!(decoded.encode_to_vec(), bytes);
    decoded
}
fn future<M: Message + Default>(message: &M) -> M {
    let mut bytes = message.encode_to_vec();
    bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
    let decoded = M::decode_from_slice(&bytes).unwrap();
    assert_eq!(decoded.encode_to_vec(), bytes);
    decoded
}
#[test]
fn command_preserves_optional_cas_zero_and_timestamp() {
    let mut request = command("helper-operation-1");
    request.expire_time.as_option_mut().unwrap().nanos = 123_456_789;
    let decoded = roundtrip(&request);
    let Some(wire::execute_capability_request::Command::Helper(helper)) = &decoded.command else {
        panic!("helper");
    };
    assert_eq!(helper.expected_helper_revision, Some(0));
    assert_eq!(decoded.expire_time.nanos, 123_456_789);
    assert_eq!(
        parse_command(&encode_command(&parse_command(&decoded).unwrap())),
        parse_command(&decoded)
    );
    assert!(parse_command(&future(&request)).is_err());
}
#[test]
fn unknown_values_remain_decodable_without_becoming_actionable() {
    let quality = wire::ObservationQuality {
        availability: EnumValue::Unknown(99),
        ..Default::default()
    };
    let decoded = roundtrip(&quality);
    assert_eq!(decoded.availability.to_i32(), 99);
    assert!(parse_quality(&decoded).is_err());
    let opaque = wire::OpaqueCapability {
        schema_id: "vendor.example.v1".into(),
        major_version: 1,
        display_name: "Synthetic readable capability".into(),
        descriptor_sha256: vec![1; 32],
        ..Default::default()
    };
    assert_eq!(roundtrip(&opaque), opaque);
    // A future command oneof is retained by decoding, never executable.
    let mut request = command("missing-intent");
    request = future(&request);
    assert!(request.command.is_none());
    assert!(parse_command(&request).is_err());
}
#[test]
fn pagination_and_watch_keep_opaque_checkpoints_without_public_native_sequences() {
    let first = wire::QueryEntityViewResponse {
        next_page_token: "opaque-next".into(),
        ..Default::default()
    };
    let final_page = wire::QueryEntityViewResponse {
        resume_token: "opaque-resume".into(),
        ..Default::default()
    };
    assert_eq!(roundtrip(&first), first);
    assert!(first.resume_token.is_empty());
    assert!(final_page.next_page_token.is_empty());
    let watch = wire::WatchEntityViewRequest {
        name: "homes/synthetic/entityViews/capture-fixture".into(),
        resume_token: roundtrip(&final_page).resume_token,
        ..Default::default()
    };
    assert_eq!(roundtrip(&watch).resume_token, "opaque-resume");
    // Stale epoch/configuration and filtered stream sequence were removed from
    // the public envelope. Capture/etag admission is qualified by cache tests.
}
#[test]
fn empty_alias_and_absent_targets_are_rejected() {
    for name in ["", "switch.synthetic", "Host:opaque/É-42"] {
        let mut request = command("off");
        request.name = name.into();
        assert_eq!(parse_command(&request).unwrap_err().field, "name");
    }
}
#[test]
fn admission_rejects_missing_and_unknown_branches_without_weakening_decoding() {
    for name in [
        "missing-intent",
        "empty-power",
        "unknown-power",
        "unknown-root",
        "unknown-nested",
        "unknown-time",
        "no-expiry",
        "bad-uuid",
    ] {
        let request = command(name);
        assert!(parse_command(&roundtrip(&request)).is_err(), "{name}");
    }
    for name in ["off", "zero", "uuid-v4"] {
        assert!(parse_command(&command(name)).is_ok(), "{name}");
    }
}
#[test]
fn helper_presence_values_and_cas_are_admitted_without_legacy_envelopes() {
    for name in [
        "helper-operation-1",
        "helper-operation-2",
        "helper-number-zero",
        "helper-empty-text",
        "helper-date",
        "helper-midnight",
    ] {
        let request = command(name);
        let parsed = parse_command(&request).unwrap();
        assert_eq!(parse_command(&encode_command(&parsed)), Ok(parsed));
    }
    for name in [
        "helper-no-cas",
        "helper-negative-cas",
        "helper-missing-set-value",
        "helper-toggle-value",
        "helper-unknown-operation",
        "helper-unknown-local-date",
        "helper-invalid-date",
        "helper-time-fraction",
    ] {
        assert!(parse_command(&command(name)).is_err(), "{name}");
    }
    let mut request = command("helper-operation-1");
    let Some(wire::execute_capability_request::Command::Helper(helper)) = &mut request.command
    else {
        panic!("helper");
    };
    helper.value = future(helper.value.as_option().unwrap()).into();
    assert!(parse_command(&request).is_err());
}
#[test]
fn capability_constraints_and_explicit_off_zero_survive_binary_codec() {
    let light = wire::LightControlCapability {
        supports_power: true,
        brightness_percent: wire::NumberBounds {
            minimum: 0.0,
            maximum: 100.0,
            step: Some(1.0),
            ..Default::default()
        }
        .into(),
        observation: wire::LightControlObservation {
            on: Some(false),
            brightness_percent: Some(0.0),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    assert_eq!(roundtrip(&light), light);
    assert_eq!(roundtrip(&light).observation.on, Some(false));
    assert_eq!(roundtrip(&light).observation.brightness_percent, Some(0.0));
    let fan = wire::FanControlCapability {
        supports_power: false,
        level: wire::ControlLevelDefinition {
            definition: Some(wire::control_level_definition::Definition::Steps(
                wire::ControlChoices {
                    choices: ["Low", "High"]
                        .into_iter()
                        .map(|key| wire::ControlChoice {
                            key: key.into(),
                            display_name: key.into(),
                            ..Default::default()
                        })
                        .collect(),
                    ..Default::default()
                }
                .into(),
            )),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    assert_eq!(roundtrip(&fan), fan);
    let climate = wire::TemperatureControlCapability {
        setpoint_celsius: wire::NumberBounds {
            minimum: 16.0,
            maximum: 30.0,
            step: Some(0.5),
            ..Default::default()
        }
        .into(),
        mode_options: vec![
            wire::TemperatureMode::Off.into(),
            wire::TemperatureMode::Heat.into(),
        ],
        observed_activity_options: vec![
            wire::TemperatureActivity::Idle.into(),
            wire::TemperatureActivity::Heating.into(),
        ],
        ..Default::default()
    };
    assert_eq!(roundtrip(&climate), climate);
    let measurement = wire::NumericMeasurementCapability {
        unit: "W".into(),
        precision: 1,
        bounds: wire::NumberBounds {
            minimum: 0.0,
            maximum: 2000.0,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    assert_eq!(roundtrip(&measurement), measurement);
    for name in ["light-off", "light-zero-color-transition"] {
        assert!(parse_command(&roundtrip(&command(name))).is_ok());
    }
}
