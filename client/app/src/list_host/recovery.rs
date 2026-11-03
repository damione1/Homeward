use home_client_transport::Failure;
use home_wire_contract::home::api::v2::ApiErrorReason as Code;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Recovery {
    Offline,
    Reconnect,
    Reset,
    Stop,
}

pub(super) fn transport(failure: &Failure) -> Recovery {
    if !matches!(failure.authorization_termination(), Ok(None)) {
        return Recovery::Stop;
    }
    let detail = match failure.api_failure() {
        Ok(Some(detail)) => detail,
        Ok(None) => {
            return if failure.may_resume_read() {
                Recovery::Offline
            } else {
                Recovery::Stop
            };
        }
        Err(_) => return Recovery::Stop,
    };
    match detail.reason() {
        Code::ResyncRequired => Recovery::Reconnect,
        Code::ServiceUnavailable => Recovery::Offline,
        Code::ResourceNotFound => Recovery::Reset,
        Code::AuthenticationRequired
        | Code::AccessDenied
        | Code::ValidationFailed
        | Code::ResourceAlreadyExists
        | Code::RequestIdReused
        | Code::EtagMismatch
        | Code::ConfigChanged
        | Code::TopologyChanged
        | Code::CapabilityUnavailable
        | Code::SourceReadOnly
        | Code::RateLimited
        | Code::RequestTooLarge
        | Code::StreamCapacity
        | Code::QueryBudgetExceeded
        | Code::RequestCancelled
        | Code::RequestDeadlineExceeded
        | Code::CommitOutcomeUnknown
        | Code::EffectOutcomeUnknown
        | Code::InternalFailure
        | Code::StoredDataIntegrity
        | Code::MethodNotSupported
        | Code::Unspecified => Recovery::Stop,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa::{Enumeration, MessageName};
    use connectrpc::{ConnectError, ErrorDetail};
    use home_wire_contract::{api_errors::DOMAIN, google::rpc::ErrorInfo};
    fn detail(code: Code) -> ErrorDetail {
        ErrorDetail::from_message(
            ErrorInfo::FULL_NAME,
            &ErrorInfo {
                domain: DOMAIN.into(),
                reason: code
                    .proto_name()
                    .trim_start_matches("API_ERROR_REASON_")
                    .into(),
                ..Default::default()
            },
        )
    }
    #[test]
    fn stale_cursor_recaptures_but_denial_future_details_and_authority_errors_purge() {
        let stale = Failure::from(
            ConnectError::failed_precondition("fixture").with_detail(detail(Code::ResyncRequired)),
        );
        assert_eq!(transport(&stale), Recovery::Reconnect);
        let denied = Failure::from(
            ConnectError::permission_denied("fixture").with_detail(detail(Code::AccessDenied)),
        );
        assert_eq!(transport(&denied), Recovery::Stop);
        let unknown = Failure::from(ConnectError::unavailable("fixture").with_detail(
            ErrorDetail::from_message(
                ErrorInfo::FULL_NAME,
                &ErrorInfo {
                    domain: DOMAIN.into(),
                    reason: "FUTURE_REASON".into(),
                    ..Default::default()
                },
            ),
        ));
        assert_eq!(transport(&unknown), Recovery::Stop);
        let authority = Failure::from(ConnectError::unavailable("fixture").with_detail(
            ErrorDetail {
                type_url: "type.googleapis.com/home.auth.v1.AuthorizationTermination".into(),
                value: Some("%%%".into()),
                debug: None,
            },
        ));
        assert_eq!(transport(&authority), Recovery::Stop);
    }
    #[test]
    fn transient_read_failures_keep_only_an_offline_cache_and_ambiguous_details_never_resume() {
        let network = Failure::from(ConnectError::unavailable("fixture"));
        assert_eq!(transport(&network), Recovery::Offline);
        let unavailable = Failure::from(
            ConnectError::unavailable("fixture").with_detail(detail(Code::ServiceUnavailable)),
        );
        assert_eq!(transport(&unavailable), Recovery::Offline);
        let duplicate = Failure::from(
            ConnectError::unavailable("fixture")
                .with_detail(detail(Code::ServiceUnavailable))
                .with_detail(detail(Code::ServiceUnavailable)),
        );
        assert_eq!(transport(&duplicate), Recovery::Stop);
        let contradictory = Failure::from(
            ConnectError::unavailable("fixture").with_detail(detail(Code::ResyncRequired)),
        );
        assert_eq!(transport(&contradictory), Recovery::Stop);
    }
}
