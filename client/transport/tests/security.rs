use std::{
    error::Error,
    future::{Future, poll_fn},
    task::Poll,
    time::Duration,
};

use buffa::MessageName;
use connectrpc::{ConnectError, ErrorDetail};
use home_client_transport::{AccessToken, DetailFailure, Failure, FailureKind, ReconnectPolicy};
use home_wire_contract::home::auth::v1::{
    AuthorizationTermination, AuthorizationTerminationReason,
};
use tokio_util::sync::CancellationToken;

#[test]
fn typed_details_keep_future_reasons_and_reject_ambiguous_or_corrupt_known_details() {
    let known = AuthorizationTermination {
        reason: AuthorizationTerminationReason::CredentialRevoked.into(),
        ..Default::default()
    };
    let detail = ErrorDetail::from_message(AuthorizationTermination::FULL_NAME, &known);
    let unknown = ErrorDetail {
        type_url: "future.Detail".into(),
        value: Some("AA".into()),
        debug: None,
    };
    let failure = Failure::from(
        ConnectError::unauthenticated("secret credential")
            .with_detail(unknown)
            .with_detail(detail.clone()),
    );
    assert_eq!(failure.authorization_termination().unwrap(), Some(known));
    assert_eq!(failure.rpc().unwrap().details.len(), 2);
    assert!(!format!("{failure:?} {failure}").contains("secret"));
    assert!(failure.source().is_none());
    let duplicate = Failure::from(
        ConnectError::unauthenticated("private")
            .with_detail(detail.clone())
            .with_detail(detail),
    );
    assert_eq!(
        duplicate.authorization_termination(),
        Err(DetailFailure::Duplicate)
    );
    let invalid = Failure::from(ConnectError::unauthenticated("private").with_detail(
        ErrorDetail {
            type_url: AuthorizationTermination::TYPE_URL.into(),
            value: Some("%%%".into()),
            debug: None,
        },
    ));
    assert_eq!(
        invalid.authorization_termination(),
        Err(DetailFailure::Malformed)
    );
    let future = AuthorizationTermination {
        reason: 999.into(),
        ..Default::default()
    };
    let failure = Failure::from(ConnectError::unauthenticated("private").with_detail(
        ErrorDetail::from_message(AuthorizationTermination::FULL_NAME, &future),
    ));
    assert_eq!(failure.authorization_termination().unwrap(), Some(future));
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
