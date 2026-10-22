use buffa::{EnumValue, Message};
use home_wire_contract::configuration_contract::admit_configuration_read;
use home_wire_contract::home::configuration::v1 as wire;

fn source_read(
    kind: wire::DocumentKind,
    schema: &str,
    profile: &str,
) -> wire::ReadDocumentResponse {
    wire::ReadDocumentResponse {
        document: wire::Document {
            id: wire::DocumentId {
                value: "retained.source.fixture".into(),
                ..Default::default()
            }
            .into(),
            kind: kind.into(),
            schema_version: schema.into(),
            syntax_profile: profile.into(),
            version: wire::DocumentVersion {
                revision: 2,
                source_sha256: vec![1; 32],
                ..Default::default()
            }
            .into(),
            updated_at: buffa_types::google::protobuf::Timestamp {
                seconds: 1_792_314_000,
                ..Default::default()
            }
            .into(),
            protected_source_reference: vec![2; 32],
            workspace_state: Some(wire::document::WorkspaceState::WorkspaceUnobserved(
                Box::new(wire::WorkspaceUnobserved {
                    reason: wire::WorkspaceUnobservedReason::NoPhysicalLocator.into(),
                    ..Default::default()
                }),
            )),
            ..Default::default()
        }
        .into(),
        active_generation: wire::ActiveGeneration {
            revision: 3,
            manifest_sha256: vec![3; 32],
            ..Default::default()
        }
        .into(),
        resume_token: "r".repeat(118),
        ..Default::default()
    }
}

#[test]
fn nha055_five_source_schemas_survive_binary_read_admission() {
    let cases = [
        (
            wire::DocumentKind::DashboardSource,
            "homeward-dashboard-1",
            "ha-2026.10/lovelace-1",
        ),
        (
            wire::DocumentKind::AutomationSource,
            "ha-logic-1",
            "ha-2026.10/logic-1",
        ),
        (
            wire::DocumentKind::ScriptSource,
            "ha-logic-1",
            "ha-2026.10/logic-1",
        ),
        (
            wire::DocumentKind::SceneSource,
            "ha-scene-1",
            "ha-2026.10/scene-1",
        ),
        (
            wire::DocumentKind::OwnershipSource,
            "homeward.heating-ownership/1",
            "homeward.heating-ownership-json/1",
        ),
    ];
    for (kind, schema, profile) in cases {
        let read = source_read(kind, schema, profile);
        let raw = read.encode_to_vec();
        let decoded = wire::ReadDocumentResponse::decode_from_slice(&raw)
            .expect("source metadata remains wire-compatible");
        assert_eq!(decoded.document.kind, EnumValue::Known(kind));
        assert_eq!(decoded.document.schema_version, schema);
        assert_eq!(decoded.document.syntax_profile, profile);
        assert!(admit_configuration_read(&decoded).is_ok(), "{kind:?}");

        let mut wrong_profile = decoded.clone();
        wrong_profile
            .document
            .as_option_mut()
            .expect("document")
            .syntax_profile = "homeward.operational-yaml/1".into();
        assert!(
            admit_configuration_read(&wrong_profile).is_err(),
            "{kind:?}"
        );
        let mut wrong_schema = decoded.clone();
        wrong_schema
            .document
            .as_option_mut()
            .expect("document")
            .schema_version = "homeward.authorization/1".into();
        assert!(admit_configuration_read(&wrong_schema).is_err(), "{kind:?}");

        let mut future_kind = decoded.clone();
        future_kind.document.as_option_mut().expect("document").kind = EnumValue::Unknown(99);
        let future_raw = future_kind.encode_to_vec();
        let compatible = wire::ReadDocumentResponse::decode_from_slice(&future_raw)
            .expect("unknown enum remains decodable");
        assert!(admit_configuration_read(&compatible).is_err());

        let mut invalid_version = decoded.clone();
        invalid_version
            .document
            .as_option_mut()
            .expect("document")
            .version
            .as_option_mut()
            .expect("version")
            .revision = 0;
        assert!(admit_configuration_read(&invalid_version).is_err());
        invalid_version
            .document
            .as_option_mut()
            .expect("document")
            .version
            .as_option_mut()
            .expect("version")
            .revision = 2;
        invalid_version
            .document
            .as_option_mut()
            .expect("document")
            .version
            .as_option_mut()
            .expect("version")
            .source_sha256 = vec![0; 32];
        assert!(admit_configuration_read(&invalid_version).is_err());

        let mut observed = decoded.clone();
        observed
            .document
            .as_option_mut()
            .expect("document")
            .workspace_state = Some(wire::document::WorkspaceState::Workspace(Box::new(
            wire::WorkspaceObservation {
                source_sha256: vec![1; 32],
                updated_at: buffa_types::google::protobuf::Timestamp {
                    seconds: 1_792_314_000,
                    ..Default::default()
                }
                .into(),
                dirty: wire::DirtyState::Clean.into(),
                ..Default::default()
            },
        )));
        assert!(
            admit_configuration_read(&observed).is_ok(),
            "valid source observation: {kind:?}"
        );
        if let Some(wire::document::WorkspaceState::Workspace(workspace)) = &mut observed
            .document
            .as_option_mut()
            .expect("document")
            .workspace_state
        {
            workspace.source_sha256 = vec![9; 32];
        }
        assert!(
            admit_configuration_read(&observed).is_err(),
            "incoherent clean source: {kind:?}"
        );
    }
}
