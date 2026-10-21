use buffa::{EnumValue, Enumeration, Message};
use home_wire_contract::home::{configuration::v1 as config, integrationadmin::v1 as wire};
use home_wire_contract::integration_admin_contract::{
    IntegrationAdminRequest, admit_integration_admin_request,
};

#[test]
fn nha148_go_rust_mutation_admission_parity() {
    for line in include_str!("../../../contracts/integration_admin_cases.tsv").lines() {
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let columns: Vec<&str> = line.split('\t').collect();
        assert_eq!(columns.len(), 8);
        let number = |n: usize| columns[n].parse::<i64>().expect("shared integer");
        let state = wire::DesiredState::from_i32(number(1) as i32)
            .map(EnumValue::Known)
            .unwrap_or(EnumValue::Unknown(number(1) as i32));
        let request = wire::SetInstanceEnabledRequest {
            integration_instance_id: "nha148-instance".into(),
            desired_state: state,
            precondition: wire::MutationPrecondition {
                expected: config::DocumentVersion {
                    revision: number(2),
                    source_sha256: vec![number(4) as u8; number(3) as usize],
                    ..Default::default()
                }
                .into(),
                expected_workspace_sha256: vec![number(6) as u8; number(5) as usize],
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        assert_eq!(
            admit_integration_admin_request(IntegrationAdminRequest::SetEnabled(&request)).is_ok(),
            columns[7] == "true",
            "{}",
            columns[0]
        );
    }
}
#[test]
fn nha148_future_nested_wire_decodes_but_fails_operational_admission() {
    let mut raw = wire::GetInstanceRequest {
        integration_instance_id: "nha148".into(),
        ..Default::default()
    }
    .encode_to_vec();
    raw.extend_from_slice(&[0x10, 0x01]);
    let decoded = wire::GetInstanceRequest::decode_from_slice(&raw).expect("compatible wire");
    assert_eq!(decoded.encode_to_vec(), raw);
    assert!(admit_integration_admin_request(IntegrationAdminRequest::Instance(&decoded)).is_err());
    assert!(
        admit_integration_admin_request(IntegrationAdminRequest::Create(
            &wire::CreateInstanceRequest::default()
        ))
        .is_err()
    );
}
#[test]
fn nha148_public_reference_cannot_target_another_instance() {
    let request = wire::CreateInstanceRequest {
        type_id: "mqtt".into(),
        integration_instance_id: "nha148".into(),
        desired_state: wire::DesiredState::Disabled.into(),
        configuration: wire::PublicConfiguration {
            schema_version: "homeward.integration/mqtt/1".into(),
            yaml_source: b"instance: nha148".to_vec(),
            secrets: vec![wire::SecretReference {
                field: "password_secret".into(),
                integration_instance_id: "foreign".into(),
                name: "credential".into(),
                ..Default::default()
            }],
            ..Default::default()
        }
        .into(),
        precondition: wire::MutationPrecondition {
            expected: config::DocumentVersion {
                revision: 1,
                source_sha256: vec![1; 32],
                ..Default::default()
            }
            .into(),
            expected_workspace_sha256: vec![2; 32],
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    assert!(admit_integration_admin_request(IntegrationAdminRequest::Create(&request)).is_err());
}
