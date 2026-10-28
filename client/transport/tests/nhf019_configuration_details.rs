use buffa::MessageName;
use connectrpc::{ConnectError, ErrorDetail};
use home_client_transport::{DetailFailure, Failure};
use home_wire_contract::home::configuration::v1 as configuration;

#[test]
fn configuration_conflict_keeps_exact_sql_revision_without_exposing_source() {
    let conflict = configuration::ConfigurationError {
        code: configuration::ErrorCode::Conflict.into(),
        current: Some(configuration::DocumentVersion {
            revision: 23,
            source_sha256: vec![9; 32],
            ..Default::default()
        })
        .into(),
        ..Default::default()
    };
    let detail = ErrorDetail::from_message(configuration::ConfigurationError::FULL_NAME, &conflict);
    let failure = Failure::from(
        ConnectError::aborted("private candidate source").with_detail(detail.clone()),
    );
    assert_eq!(failure.configuration_error(), Ok(Some(conflict)));
    assert_eq!(failure.authorization_termination(), Ok(None));
    assert!(!format!("{failure:?} {failure}").contains("private"));

    let duplicate = Failure::from(
        ConnectError::aborted("private candidate source")
            .with_detail(detail.clone())
            .with_detail(detail),
    );
    assert_eq!(
        duplicate.configuration_error(),
        Err(DetailFailure::Duplicate)
    );

    let malformed = Failure::from(ConnectError::aborted("private").with_detail(ErrorDetail {
        type_url: configuration::ConfigurationError::TYPE_URL.into(),
        value: Some("%%%".into()),
        debug: None,
    }));
    assert_eq!(
        malformed.configuration_error(),
        Err(DetailFailure::Malformed)
    );
}

#[test]
fn unknown_configuration_reason_is_preserved_without_inventing_a_conflict() {
    let error = configuration::ConfigurationError {
        code: 999.into(),
        ..Default::default()
    };
    let failure = Failure::from(
        ConnectError::unavailable("private workspace path").with_detail(ErrorDetail::from_message(
            configuration::ConfigurationError::FULL_NAME,
            &error,
        )),
    );
    assert_eq!(failure.configuration_error(), Ok(Some(error)));
    assert_eq!(failure.integration_error(), Ok(None));
    assert!(!format!("{failure:?} {failure}").contains("private"));
}
