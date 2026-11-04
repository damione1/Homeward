use buffa::MessageName;
use connectrpc::{ConnectError, ErrorDetail};
use home_client_transport::Failure;
use home_wire_contract::{
    api_errors::{DOMAIN, InvalidApiError},
    google::rpc::ErrorInfo,
    home::api::v2::ApiErrorReason,
};

fn detail(reason: &str) -> ErrorDetail {
    ErrorDetail::from_message(
        ErrorInfo::FULL_NAME,
        &ErrorInfo {
            reason: reason.into(),
            domain: DOMAIN.into(),
            ..Default::default()
        },
    )
}

#[test]
fn integration_conflict_is_canonical_and_rejects_ambiguous_details() {
    let detail = detail("ETAG_MISMATCH");
    let failure = Failure::from(
        ConnectError::aborted("private YAML and credential").with_detail(detail.clone()),
    );
    assert_eq!(
        failure.api_failure().map(|v| v.map(|f| f.reason())),
        Ok(Some(ApiErrorReason::EtagMismatch))
    );
    assert!(!failure.request_not_dispatched());
    assert!(!format!("{failure:?} {failure}").contains("private"));
    let duplicate = Failure::from(
        ConnectError::aborted("private")
            .with_detail(detail.clone())
            .with_detail(detail),
    );
    assert_eq!(duplicate.api_failure(), Err(InvalidApiError::Duplicate));
    let malformed = Failure::from(ConnectError::aborted("private").with_detail(ErrorDetail {
        type_url: ErrorInfo::TYPE_URL.into(),
        value: Some("%%%".into()),
        debug: None,
    }));
    assert_eq!(malformed.api_failure(), Err(InvalidApiError::Malformed));
}

#[test]
fn policy_and_factor_errors_remain_distinct_from_native_session_revocation() {
    let conflict = Failure::from(
        ConnectError::aborted("private policy source").with_detail(detail("ETAG_MISMATCH")),
    );
    assert_eq!(
        conflict.api_failure().map(|v| v.map(|f| f.reason())),
        Ok(Some(ApiErrorReason::EtagMismatch))
    );
    let revoked = Failure::from(
        ConnectError::unauthenticated("private session")
            .with_detail(detail("AUTHENTICATION_REQUIRED")),
    );
    assert_eq!(
        revoked.api_failure().map(|v| v.map(|f| f.reason())),
        Ok(Some(ApiErrorReason::AuthenticationRequired))
    );
    assert!(!revoked.may_resume_read());
    assert!(!revoked.may_recover_renewal());
}

#[test]
fn future_observability_reason_is_rejected_without_disclosing_remote_text() {
    let failure = Failure::from(
        ConnectError::unavailable("private log payload").with_detail(detail("FUTURE_REASON")),
    );
    assert_eq!(failure.api_failure(), Err(InvalidApiError::Unsupported));
    assert!(!failure.may_resume_read());
    assert!(!format!("{failure:?} {failure}").contains("private"));
}
