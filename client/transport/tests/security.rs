use std::{
    error::Error,
    future::{Future, poll_fn},
    task::Poll,
    time::Duration,
};

use buffa::MessageName;
use connectrpc::{ConnectError, ErrorDetail};
use home_client_transport::{AccessToken, Failure, FailureKind, ReconnectPolicy};
use home_wire_contract::{
    api_errors::{DOMAIN, InvalidApiError},
    google::rpc::ErrorInfo,
    home::api::v2::ApiErrorReason,
};
use tokio_util::sync::CancellationToken;

#[test]
fn device_trust_requires_https_and_never_falls_back_from_invalid_private_material() {
    use home_client_transport::Connection;
    assert!(Connection::new("https://home.example.test", b"").is_ok());
    assert!(Connection::new("http://home.example.test", b"").is_err());
    assert!(Connection::new("https://home.example.test", b"not a certificate").is_err());
    assert!(Connection::new("https://home.example.test", b" ").is_err());
}

#[test]
fn canonical_details_reject_ambiguous_corrupt_and_future_reasons() {
    let known = ErrorInfo {
        reason: "AUTHENTICATION_REQUIRED".into(),
        domain: DOMAIN.into(),
        ..Default::default()
    };
    let detail = ErrorDetail::from_message(ErrorInfo::FULL_NAME, &known);
    let failure = Failure::from(
        ConnectError::unauthenticated("secret credential").with_detail(detail.clone()),
    );
    assert_eq!(
        failure.api_failure().unwrap().unwrap().reason(),
        ApiErrorReason::AuthenticationRequired
    );
    assert!(!format!("{failure:?} {failure}").contains("secret"));
    assert!(failure.source().is_none());
    assert!(!failure.may_resume_read());
    let duplicate = Failure::from(
        ConnectError::unauthenticated("private")
            .with_detail(detail.clone())
            .with_detail(detail),
    );
    assert_eq!(duplicate.api_failure(), Err(InvalidApiError::Duplicate));
    let invalid = Failure::from(ConnectError::unauthenticated("private").with_detail(
        ErrorDetail {
            type_url: ErrorInfo::TYPE_URL.into(),
            value: Some("%%%".into()),
            debug: None,
        },
    ));
    assert_eq!(invalid.api_failure(), Err(InvalidApiError::Malformed));
    let future = ErrorInfo {
        reason: "API_ERROR_REASON_FUTURE".into(),
        ..known
    };
    let failure = Failure::from(
        ConnectError::unauthenticated("private")
            .with_detail(ErrorDetail::from_message(ErrorInfo::FULL_NAME, &future)),
    );
    assert!(failure.api_failure().is_err());
    assert!(!failure.may_resume_read());
}

#[test]
fn certificate_failures_remain_distinct_through_nested_io_payloads() {
    for (reason, expected) in [
        (
            rustls::CertificateError::UnknownIssuer,
            FailureKind::UntrustedCertificate,
        ),
        (
            rustls::CertificateError::NotValidForName,
            FailureKind::WrongServerName,
        ),
        (
            rustls::CertificateError::BadSignature,
            FailureKind::TlsRejected,
        ),
    ] {
        let source = std::io::Error::other(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            rustls::Error::InvalidCertificate(reason),
        ));
        let failure = Failure::from(ConnectError::unavailable_from_transport(
            "private transport text",
            source,
        ));
        assert_eq!(failure.kind(), expected);
    }
    assert!(AccessToken::parse("a\r\nAuthorization: injected").is_err());
    assert!(AccessToken::parse("").is_err());
}

#[test]
fn local_body_read_provenance_cannot_be_forged_by_a_server_internal_message() {
    let remote = Failure::from(ConnectError::internal(
        "failed to read response body: connection reset",
    ));
    assert_eq!(
        remote.kind(),
        FailureKind::Rpc(connectrpc::ErrorCode::Internal)
    );
    let local = Failure::from(
        ConnectError::internal("private transport diagnostic")
            .with_source(connectrpc::client::ResponseBodyReadError),
    );
    assert_eq!(local.kind(), FailureKind::NetworkInterrupted);
    assert_eq!(local.rpc().unwrap().code, connectrpc::ErrorCode::Internal);
    assert!(!format!("{local:?} {local}").contains("private"));
    let detail = ErrorDetail::from_message(
        ErrorInfo::FULL_NAME,
        &ErrorInfo {
            domain: DOMAIN.into(),
            reason: "AUTHORITY_UNAVAILABLE".into(),
            ..Default::default()
        },
    );
    let terminated = Failure::from(
        ConnectError::unavailable("private")
            .with_source(connectrpc::client::ResponseBodyReadError)
            .with_detail(detail),
    );
    assert_eq!(
        terminated.api_failure().unwrap().unwrap().reason(),
        ApiErrorReason::AuthorityUnavailable
    );
    assert!(!terminated.may_resume_read());
    assert!(!terminated.may_recover_renewal());
}

#[test]
fn remote_cancellation_can_resume_a_read_but_authority_cancellation_cannot() {
    let remote = Failure::from(ConnectError::canceled("server shutdown"));
    assert_eq!(
        remote.kind(),
        FailureKind::Rpc(connectrpc::ErrorCode::Canceled)
    );
    assert!(remote.may_resume_read());
    let malformed_authority = Failure::from(ConnectError::canceled("server shutdown").with_detail(
        ErrorDetail {
            type_url: ErrorInfo::TYPE_URL.into(),
            value: None,
            debug: None,
        },
    ));
    assert!(!malformed_authority.may_resume_read());
    assert!(Failure::from(ConnectError::internal("server fault")).may_resume_read());
}

#[tokio::test(start_paused = true)]
async fn retry_budget_caps_waits_and_cancels_a_pending_wait() {
    assert!(ReconnectPolicy::new(0, Duration::from_secs(1), Duration::from_secs(1)).is_err());
    assert!(ReconnectPolicy::new(9, Duration::from_secs(1), Duration::from_secs(1)).is_err());
    let policy = ReconnectPolicy::new(4, Duration::from_secs(2), Duration::from_secs(3)).unwrap();
    let cancel = CancellationToken::new();
    let start = tokio::time::Instant::now();
    assert!(policy.wait_after(1, &cancel).await.unwrap());
    assert!(policy.wait_after(2, &cancel).await.unwrap());
    assert!(policy.wait_after(3, &cancel).await.unwrap());
    assert!(!policy.wait_after(4, &cancel).await.unwrap());
    assert_eq!(start.elapsed(), Duration::from_secs(8));
    let mut pending = Box::pin(policy.wait_after(1, &cancel));
    poll_fn(|cx| match pending.as_mut().poll(cx) {
        Poll::Pending => Poll::Ready(()),
        Poll::Ready(_) => panic!("backoff did not wait"),
    })
    .await;
    cancel.cancel();
    assert_eq!(pending.await.unwrap_err().kind(), FailureKind::Cancelled);
}
