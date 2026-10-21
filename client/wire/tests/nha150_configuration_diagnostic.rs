use buffa::{EnumValue, Message};
use home_wire_contract::configuration_contract::admit_configuration_read;
use home_wire_contract::home::configuration::v1 as wire;

fn diagnostic_read() -> wire::ReadDocumentResponse {
    let at = buffa_types::google::protobuf::Timestamp {
        seconds: 1_792_540_800,
        ..Default::default()
    };
    wire::ReadDocumentResponse {
        document: wire::Document {
            id: wire::DocumentId {
                value: "operational.main".into(),
                ..Default::default()
            }
            .into(),
            kind: wire::DocumentKind::OperationalYaml.into(),
            schema_version: "homeward.operational/1".into(),
            syntax_profile: "homeward.operational-yaml/1".into(),
            version: wire::DocumentVersion {
                revision: 1,
                source_sha256: vec![1; 32],
                ..Default::default()
            }
            .into(),
            updated_at: at.clone().into(),
            active: true,
            protected_source_reference: vec![2; 32],
            workspace_state: Some(wire::document::WorkspaceState::Workspace(Box::new(
                wire::WorkspaceObservation {
                    source_sha256: vec![3; 32],
                    updated_at: at.clone().into(),
                    dirty: wire::DirtyState::Rejected.into(),
                    diagnostic: wire::WorkspaceDiagnostic {
                        document: wire::DocumentId {
                            value: "operational.main".into(),
                            ..Default::default()
                        }
                        .into(),
                        observed_source_sha256: vec![3; 32],
                        observed_at: at.into(),
                        code: wire::DiagnosticCode::CandidateRejected.into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        active_generation: wire::ActiveGeneration {
            revision: 1,
            manifest_sha256: vec![4; 32],
            ..Default::default()
        }
        .into(),
        resume_token: "x".repeat(118),
        ..Default::default()
    }
}

#[test]
fn nha150_configuration_diagnostic_refuses_stale_foreign_and_unknown_bindings() {
    let valid = diagnostic_read();
    assert!(admit_configuration_read(&valid).is_ok());
    for case in 0..8 {
        let mut read = valid.clone();
        let Some(wire::document::WorkspaceState::Workspace(workspace)) = &mut read
            .document
            .as_option_mut()
            .expect("document")
            .workspace_state
        else {
            panic!("fixture workspace");
        };
        if case == 6 {
            workspace.diagnostic = None.into();
        } else {
            let diagnostic = workspace.diagnostic.as_option_mut().expect("diagnostic");
            match case {
                0 => {
                    diagnostic
                        .document
                        .as_option_mut()
                        .expect("source document")
                        .value = "bootstrap.launch".into()
                }
                1 => diagnostic.observed_source_sha256[0] = 9,
                2 => {
                    diagnostic
                        .observed_at
                        .as_option_mut()
                        .expect("date")
                        .seconds += 1
                }
                3 => diagnostic.code = EnumValue::Unknown(999),
                4 => diagnostic.code = wire::DiagnosticCode::Unspecified.into(),
                5 => {
                    workspace.dirty = wire::DirtyState::Clean.into();
                    workspace.source_sha256 = vec![1; 32];
                    diagnostic.observed_source_sha256 = vec![1; 32];
                }
                7 => diagnostic.observed_at = None.into(),
                _ => unreachable!(),
            }
        }
        // A rejected diagnostic is optional for older compatible servers; all
        // supplied diagnostics must nevertheless carry a complete binding.
        assert_eq!(
            admit_configuration_read(&read).is_ok(),
            case == 6,
            "case {case}"
        );
    }
}

#[test]
fn nha150_unknown_diagnostic_wire_decodes_without_admission() {
    let mut read = diagnostic_read();
    let Some(wire::document::WorkspaceState::Workspace(workspace)) = &mut read
        .document
        .as_option_mut()
        .expect("document")
        .workspace_state
    else {
        panic!("fixture workspace")
    };
    // Diagnostic field5 is not known. Generated decoding retains it, while the
    // public operational validator refuses this unknown nested field.
    let mut raw = workspace
        .diagnostic
        .as_option()
        .expect("diagnostic")
        .encode_to_vec();
    raw.extend_from_slice(&[0x28, 0x01]);
    workspace.diagnostic = wire::WorkspaceDiagnostic::decode_from_slice(&raw)
        .expect("compatible diagnostic")
        .into();
    let decoded = wire::ReadDocumentResponse::decode_from_slice(&read.encode_to_vec())
        .expect("compatible response");
    assert!(admit_configuration_read(&decoded).is_err());
}
