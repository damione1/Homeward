use buffa::MessageName;
use connectrpc::{ConnectError, ErrorDetail};
use home_client_transport::{DetailFailure, Failure};
use home_wire_contract::home::{
    auth::v1 as auth, configuration::v1::DocumentVersion, integrationadmin::v1 as integration,
    observability::v1 as observation,
};

#[test]
fn integration_conflict_preserves_exact_version_and_rejects_ambiguous_details() {
    let conflict = integration::IntegrationError {
        code: integration::ErrorCode::Conflict.into(),
        current: Some(DocumentVersion {
            revision: 19,
            source_sha256: vec![7; 32],
            ..Default::default()
        })
        .into(),
        ..Default::default()
    };
    let detail = ErrorDetail::from_message(integration::IntegrationError::FULL_NAME, &conflict);
    let failure = Failure::from(
        ConnectError::aborted("private YAML and credential").with_detail(detail.clone()),
    );
    assert_eq!(failure.integration_error(), Ok(Some(conflict)));
    assert!(!format!("{failure:?} {failure}").contains("private"));
    let duplicate = Failure::from(
        ConnectError::aborted("private")
            .with_detail(detail.clone())
            .with_detail(detail),
    );
    assert_eq!(duplicate.integration_error(), Err(DetailFailure::Duplicate));
    let malformed = Failure::from(ConnectError::aborted("private").with_detail(ErrorDetail {
        type_url: integration::IntegrationError::TYPE_URL.into(),
        value: Some("%%%".into()),
        debug: None,
    }));
    assert_eq!(malformed.integration_error(), Err(DetailFailure::Malformed));
}

#[test]
fn policy_and_factor_errors_remain_distinct_from_native_session_revocation() {
    let policy = auth::AuthorizationPolicyError {
        code: auth::AuthorizationPolicyErrorCode::VersionConflict.into(),
        ..Default::default()
    };
    let factor = auth::SecondFactorPolicyError {
        code: 999.into(),
        ..Default::default()
    };
    let account = auth::AuthError {
        code: 999.into(),
        ..Default::default()
    };
    let failure = Failure::from(
        ConnectError::permission_denied("private")
            .with_detail(ErrorDetail::from_message(
                auth::AuthorizationPolicyError::FULL_NAME,
                &policy,
            ))
            .with_detail(ErrorDetail::from_message(
                auth::SecondFactorPolicyError::FULL_NAME,
                &factor,
            ))
            .with_detail(ErrorDetail::from_message(
                auth::AuthError::FULL_NAME,
                &account,
            )),
    );
    assert_eq!(failure.authorization_policy_error(), Ok(Some(policy)));
    assert_eq!(failure.second_factor_policy_error(), Ok(Some(factor)));
    assert_eq!(failure.auth_error(), Ok(Some(account)));
    assert_eq!(failure.authorization_termination(), Ok(None));
}

#[test]
fn future_observability_reason_is_retained_without_disclosing_remote_text() {
    let error = observation::ObservabilityError {
        code: 999.into(),
        ..Default::default()
    };
    let failure = Failure::from(
        ConnectError::unavailable("private log payload").with_detail(ErrorDetail::from_message(
            observation::ObservabilityError::FULL_NAME,
            &error,
        )),
    );
    assert_eq!(failure.observability_error(), Ok(Some(error)));
    assert!(!format!("{failure:?} {failure}").contains("private"));
}
