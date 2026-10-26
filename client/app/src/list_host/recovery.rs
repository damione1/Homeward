use buffa::EnumValue;
use home_client_transport::{Failure, FailureKind, RpcCode};
use home_wire_contract::{home::lists::v1::ListErrorCode as Code, lists_contract};

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
    let detail = match failure.list_error() {
        Ok(Some(detail)) if lists_contract::admit_error(&detail).is_ok() => detail,
        Ok(None) => {
            return if failure.may_resume_read() {
                Recovery::Offline
            } else {
                Recovery::Stop
            };
        }
        Ok(Some(_)) | Err(_) => return Recovery::Stop,
    };
    match detail.code {
        EnumValue::Known(Code::StaleCursor)
            if failure.kind() == FailureKind::Rpc(RpcCode::FailedPrecondition) =>
        {
            Recovery::Reconnect
        }
        EnumValue::Known(Code::Unavailable | Code::Closed)
            if failure.kind() == FailureKind::Rpc(RpcCode::Unavailable) =>
        {
            Recovery::Offline
        }
        EnumValue::Known(Code::NotFound)
            if failure.kind() == FailureKind::Rpc(RpcCode::NotFound) =>
        {
            Recovery::Reset
        }
        EnumValue::Known(
            Code::Invalid
            | Code::Denied
            | Code::NotFound
            | Code::Conflict
            | Code::StaleCursor
            | Code::Capacity
            | Code::Unavailable
            | Code::CommitUnknown
            | Code::CommittedResponseWithheld
            | Code::Closed
            | Code::Unspecified,
        )
        | EnumValue::Unknown(_) => Recovery::Stop,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa::MessageName;
    use connectrpc::{ConnectError, ErrorDetail};
    use home_wire_contract::home::lists::v1::ListError;
    fn detail(code: Code) -> ErrorDetail {
        ErrorDetail::from_message(
            ListError::FULL_NAME,
            &ListError {
                code: code.into(),
                ..Default::default()
            },
        )
    }
    #[test]
    fn stale_cursor_recaptures_but_denial_future_details_and_authority_errors_purge() {
        let stale = Failure::from(
            ConnectError::failed_precondition("fixture").with_detail(detail(Code::StaleCursor)),
        );
        assert_eq!(transport(&stale), Recovery::Reconnect);
        let denied = Failure::from(
            ConnectError::permission_denied("fixture").with_detail(detail(Code::Denied)),
        );
        assert_eq!(transport(&denied), Recovery::Stop);
        let unknown = Failure::from(ConnectError::unavailable("fixture").with_detail(
            ErrorDetail::from_message(
                ListError::FULL_NAME,
                &ListError {
                    code: EnumValue::Unknown(999),
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
            ConnectError::unavailable("fixture").with_detail(detail(Code::Unavailable)),
        );
        assert_eq!(transport(&unavailable), Recovery::Offline);
        let duplicate = Failure::from(
            ConnectError::unavailable("fixture")
                .with_detail(detail(Code::Unavailable))
                .with_detail(detail(Code::Unavailable)),
        );
        assert_eq!(transport(&duplicate), Recovery::Stop);
        let contradictory = Failure::from(
            ConnectError::unavailable("fixture").with_detail(detail(Code::StaleCursor)),
        );
        assert_eq!(transport(&contradictory), Recovery::Stop);
    }
}
