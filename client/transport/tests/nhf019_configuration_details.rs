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
fn configuration_conflict_is_canonical_without_exposing_source_or_inventing_a_version() {
    // Current etags come from an explicit resource read; error messages never
    // supply a replacement version or authorize replay of the mutation.
    let detail = detail("ETAG_MISMATCH");
    let failure = Failure::from(
        ConnectError::aborted("private candidate source; revision 23").with_detail(detail.clone()),
    );
    let api = failure.api_failure().unwrap().unwrap();
    assert_eq!(api.reason(), ApiErrorReason::EtagMismatch);
    assert!(api.operation().is_none());
    assert!(api.uncertainty().is_none());
    assert!(!failure.request_not_dispatched());
    assert!(!format!("{failure:?} {failure}").contains("private"));
    assert!(!format!("{failure:?} {failure}").contains("23"));

    let duplicate = Failure::from(
        ConnectError::aborted("private candidate source")
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
fn unknown_configuration_reason_is_rejected_without_inventing_a_conflict() {
    let failure = Failure::from(
        ConnectError::unavailable("private workspace path").with_detail(detail("FUTURE_REASON")),
    );
    assert_eq!(failure.api_failure(), Err(InvalidApiError::Unsupported));
    assert!(!failure.may_resume_read());
    assert!(!failure.may_recover_renewal());
    assert!(!format!("{failure:?} {failure}").contains("private"));
}
