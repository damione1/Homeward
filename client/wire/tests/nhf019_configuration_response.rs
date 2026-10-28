use buffa::Message;
use home_wire_contract::{
    configuration_apply_contract::{ActivationRequest, admit_activation_request},
    configuration_contract::{ConfigurationResponse, admit_configuration_response},
    home::{
        configuration::v1 as config, configurationapply::v1 as apply, configurationplan::v1 as plan,
    },
};
fn complete() -> config::PageContinuation {
    config::PageContinuation {
        result: Some(config::page_continuation::Result::Complete(Box::default())),
        ..Default::default()
    }
}
fn version() -> config::DocumentVersion {
    config::DocumentVersion {
        revision: 7,
        source_sha256: vec![1; 32],
        ..Default::default()
    }
}
fn apply() -> apply::ApplyRequest {
    apply::ApplyRequest {
        candidate: plan::Candidate {
            family: plan::SourceFamily::Script.into(),
            identity: "synthetic.script".into(),
            entry: "entry.yaml".into(),
            files: vec![plan::SourceFile {
                logical_path: "entry.yaml".into(),
                exact_source: b"sequence: []".to_vec(),
                ..Default::default()
            }],
            ..Default::default()
        }
        .into(),
        expected: version().into(),
        expected_workspace_sha256: vec![2; 32],
        sources: vec![apply::SourcePrecondition {
            id: config::DocumentId {
                value: "synthetic.document".into(),
                ..Default::default()
            }
            .into(),
            logical_path: "entry.yaml".into(),
            sql_state: Some(apply::source_precondition::SqlState::SqlAbsent(
                Box::default(),
            )),
            workspace_state: Some(apply::source_precondition::WorkspaceState::WorkspaceAbsent(
                Box::default(),
            )),
            ..Default::default()
        }],
        ..Default::default()
    }
}
#[test]
fn a_purged_historical_version_has_no_invented_timestamp_or_schema() {
    let mut response = config::ListVersionsResponse {
        versions: vec![config::HistoricalVersion {
            id: config::DocumentId {
                value: "synthetic.document".into(),
                ..Default::default()
            }
            .into(),
            version: version().into(),
            availability: config::VersionAvailability::Purged.into(),
            ..Default::default()
        }],
        continuation: complete().into(),
        ..Default::default()
    };
    assert!(admit_configuration_response(ConfigurationResponse::Versions(&response)).is_ok());
    response.versions[0].schema_version = "invented".into();
    assert!(admit_configuration_response(ConfigurationResponse::Versions(&response)).is_err());
    response.versions[0].schema_version.clear();
    response.versions[0].availability = 999.into();
    assert!(admit_configuration_response(ConfigurationResponse::Versions(&response)).is_err());
}
#[test]
fn source_preconditions_require_both_states_and_reject_unknown_empty_payloads() {
    let mut request = apply();
    assert!(admit_activation_request(ActivationRequest::Apply(&request)).is_ok());
    request.sources[0].workspace_state = None;
    assert!(admit_activation_request(ActivationRequest::Apply(&request)).is_err());
    request = apply();
    request.sources[0].sql_state = Some(apply::source_precondition::SqlState::SqlAbsent(Box::new(
        buffa_types::google::protobuf::Empty::decode_from_slice(&[160, 6, 1]).unwrap(),
    )));
    assert!(admit_activation_request(ActivationRequest::Apply(&request)).is_err());
    request = apply();
    request.sources.push(request.sources[0].clone());
    assert!(admit_activation_request(ActivationRequest::Apply(&request)).is_err());
}
#[test]
fn rollback_never_guesses_which_historical_authority_to_restore() {
    let request = apply();
    let mut rollback = apply::RollbackRequest {
        id: config::DocumentId {
            value: "operational.main".into(),
            ..Default::default()
        }
        .into(),
        expected: version().into(),
        expected_workspace_sha256: vec![2; 32],
        family: plan::SourceFamily::Authorization.into(),
        identity: "synthetic.account".into(),
        sources: request.sources,
        historical_policy_version: version().into(),
        ..Default::default()
    };
    assert!(admit_activation_request(ActivationRequest::Rollback(&rollback)).is_ok());
    rollback.historical = config::ActiveGeneration {
        revision: 8,
        manifest_sha256: vec![3; 32],
        ..Default::default()
    }
    .into();
    assert!(admit_activation_request(ActivationRequest::Rollback(&rollback)).is_err());
    rollback.historical_policy_version = Default::default();
    assert!(admit_activation_request(ActivationRequest::Rollback(&rollback)).is_ok());
    rollback.family = 999.into();
    assert!(admit_activation_request(ActivationRequest::Rollback(&rollback)).is_err());
}
