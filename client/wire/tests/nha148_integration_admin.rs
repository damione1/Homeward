use buffa::Message;
use home_wire_contract::home::api::v2 as wire;
use home_wire_contract::integration_admin_contract::{
    IntegrationAdminRequest, admit_integration_admin_request,
};

#[test]
fn nha148_go_rust_mutation_admission_parity() {
    for line in include_str!("../../../contracts/integration_admin_cases.tsv").lines() {
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let c: Vec<_> = line.split('\t').collect();
        assert_eq!(c.len(), 7);
        let request = wire::UpdateIntegrationInstanceRequest {
            integration_instance: wire::IntegrationInstance {
                name: c[1].into(),
                desired_state: c[2].parse::<i32>().unwrap().into(),
                etag: c[3].into(),
                ..Default::default()
            }
            .into(),
            update_mask: buffa_types::google::protobuf::FieldMask {
                paths: if c[4].is_empty() {
                    vec![]
                } else {
                    c[4].split(',').map(str::to_owned).collect()
                },
                ..Default::default()
            }
            .into(),
            request_id: c[5].into(),
            ..Default::default()
        };
        assert_eq!(
            admit_integration_admin_request(IntegrationAdminRequest::Update(&request)).is_ok(),
            c[6] == "true",
            "{}",
            c[0]
        );
    }
}
#[test]
fn nha148_future_wire_decodes_but_fails_operational_admission() {
    let mut raw = wire::GetIntegrationInstanceRequest {
        name: "homes/synthetic/integrationInstances/instance-fixture".into(),
        ..Default::default()
    }
    .encode_to_vec();
    raw.extend_from_slice(&[0x98, 0x06, 0x01]);
    let decoded = wire::GetIntegrationInstanceRequest::decode_from_slice(&raw).unwrap();
    assert_eq!(decoded.encode_to_vec(), raw);
    assert!(admit_integration_admin_request(IntegrationAdminRequest::Instance(&decoded)).is_err());
    assert!(
        admit_integration_admin_request(IntegrationAdminRequest::Create(
            &wire::CreateIntegrationInstanceRequest::default()
        ))
        .is_err()
    );
}
#[test]
fn nha148_public_reference_cannot_target_another_instance() {
    let mut request = wire::CreateIntegrationInstanceRequest {
        parent: "homes/synthetic".into(),
        integration_instance: wire::IntegrationInstance {
            instance_key: "nha148".into(),
            integration_type: "homes/synthetic/integrationTypes/mqtt".into(),
            desired_state: wire::IntegrationDesiredState::Disabled.into(),
            configuration: Some(wire::integration_instance::Configuration::Editable(
                Box::new(wire::IntegrationConfiguration {
                    schema_version: "homeward.integration/mqtt/1".into(),
                    yaml_source: b"instance: nha148".to_vec(),
                    secrets: vec![wire::IntegrationSecretReference {
                        field: "password_secret".into(),
                        instance_key: "foreign".into(),
                        secret_key: "credential".into(),
                        ..Default::default()
                    }],
                    ..Default::default()
                }),
            )),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    assert!(admit_integration_admin_request(IntegrationAdminRequest::Create(&request)).is_err());
    let Some(wire::integration_instance::Configuration::Editable(cfg)) = request
        .integration_instance
        .as_option_mut()
        .unwrap()
        .configuration
        .as_mut()
    else {
        panic!("editable fixture");
    };
    cfg.secrets[0].instance_key = "nha148".into();
    assert!(admit_integration_admin_request(IntegrationAdminRequest::Create(&request)).is_ok());
}
