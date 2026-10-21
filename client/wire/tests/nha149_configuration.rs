use buffa::{EnumValue, Enumeration, Message};
use home_wire_contract::configuration_contract::{
    ConfigurationRequest, admit_configuration_activation, admit_configuration_read,
    admit_configuration_request,
};
use home_wire_contract::home::configuration::v1 as wire;

#[test]
fn nha149_configuration_go_rust_admission_parity() {
    for line in include_str!("../../../contracts/configuration_cases.tsv").lines() {
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let columns: Vec<&str> = line.split('\t').collect();
        assert_eq!(columns.len(), 9);
        let number = |n: usize| columns[n].parse::<i64>().expect("shared integer");
        let id = if columns[1] == "-" { "" } else { columns[1] };
        let request = wire::WriteDocumentRequest {
            id: Some(wire::DocumentId {
                value: id.into(),
                ..Default::default()
            })
            .into(),
            expected: Some(wire::DocumentVersion {
                revision: number(2),
                source_sha256: vec![number(4) as u8; number(3) as usize],
                ..Default::default()
            })
            .into(),
            expected_workspace_sha256: vec![number(6) as u8; number(5) as usize],
            exact_yaml_source: vec![b'x'; number(7) as usize],
            ..Default::default()
        };
        assert_eq!(
            admit_configuration_request(ConfigurationRequest::Write(&request)).is_ok(),
            columns[8] == "true",
            "{}",
            columns[0]
        );
    }
}

#[test]
fn nha149_nested_unknown_wire_fields_stay_compatible_but_not_admitted() {
    // ReadDocumentRequest.id={value:"operational.main", unknown field2=1}.
    let raw = b"\x0a\x14\x0a\x10operational.main\x10\x01";
    let request =
        wire::ReadDocumentRequest::decode_from_slice(raw).expect("compatible wire decoding");
    assert_eq!(request.encode_to_vec().as_slice(), raw.as_slice());
    assert!(admit_configuration_request(ConfigurationRequest::Read(&request)).is_err());
}

#[test]
fn nha149_configuration_activation_go_rust_parity() {
    for line in include_str!("../../../contracts/configuration_activation_cases.tsv").lines() {
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let columns: Vec<&str> = line.split('\t').collect();
        assert_eq!(columns.len(), 5);
        let number = |n: usize| {
            columns[n]
                .parse::<i32>()
                .expect("shared activation integer")
        };
        let outcome = wire::ActivationOutcome::from_i32(number(1))
            .map(EnumValue::Known)
            .unwrap_or(EnumValue::Unknown(number(1)));
        let transaction = wire::TransactionOutcome::from_i32(number(2))
            .map(EnumValue::Known)
            .unwrap_or(EnumValue::Unknown(number(2)));
        let revision = number(3);
        let activation = wire::ActivationResult {
            outcome,
            transaction,
            generation_revision: (revision != 0).then_some(i64::from(revision)),
            ..Default::default()
        };
        assert_eq!(
            admit_configuration_activation(&activation).is_ok(),
            columns[4] == "true",
            "{}",
            columns[0]
        );
    }
}

#[test]
fn nha149_reads_support_operational_and_retained_yaml_metadata() {
    use wire::document::WorkspaceState;
    let time = buffa_types::google::protobuf::Timestamp {
        seconds: 1_792_314_000,
        ..Default::default()
    };
    let operational = wire::Document {
        id: wire::DocumentId {
            value: "operational.main".into(),
            ..Default::default()
        }
        .into(),
        kind: wire::DocumentKind::OperationalYaml.into(),
        schema_version: "homeward.operational/1".into(),
        syntax_profile: "homeward.operational-yaml/1".into(),
        version: wire::DocumentVersion {
            revision: 2,
            source_sha256: vec![1; 32],
            ..Default::default()
        }
        .into(),
        updated_at: time.clone().into(),
        active: true,
        workspace_state: Some(WorkspaceState::Workspace(Box::new(
            wire::WorkspaceObservation {
                source_sha256: vec![1; 32],
                updated_at: time.clone().into(),
                dirty: wire::DirtyState::Clean.into(),
                ..Default::default()
            },
        ))),
        protected_source_reference: vec![2; 32],
        ..Default::default()
    };
    let mut read = wire::ReadDocumentResponse {
        document: operational.into(),
        active_generation: wire::ActiveGeneration {
            revision: 3,
            manifest_sha256: vec![3; 32],
            ..Default::default()
        }
        .into(),
        resume_token: "r".repeat(118),
        ..Default::default()
    };
    assert!(admit_configuration_read(&read).is_ok());
    let document = read.document.as_option_mut().expect("document");
    document.id = wire::DocumentId {
        value: "retained.yaml.source".into(),
        ..Default::default()
    }
    .into();
    document.kind = wire::DocumentKind::YamlSource.into();
    document.schema_version = "ha-declarative-2".into();
    document.syntax_profile = "ha-2026.10/declarative-2".into();
    document.workspace_state = Some(WorkspaceState::WorkspaceUnobserved(Box::new(
        wire::WorkspaceUnobserved {
            reason: wire::WorkspaceUnobservedReason::NoPhysicalLocator.into(),
            ..Default::default()
        },
    )));
    assert!(admit_configuration_read(&read).is_ok());
    read.document
        .as_option_mut()
        .expect("document")
        .workspace_state = None;
    assert!(
        admit_configuration_read(&read).is_err(),
        "missing observation state is not clean"
    );
    read.document
        .as_option_mut()
        .expect("document")
        .workspace_state = Some(WorkspaceState::WorkspaceUnobserved(Box::new(
        wire::WorkspaceUnobserved {
            reason: EnumValue::Unknown(99),
            ..Default::default()
        },
    )));
    assert!(
        admit_configuration_read(&read).is_err(),
        "future unobserved reason was operationally admitted"
    );
}
