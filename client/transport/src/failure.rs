use std::{error::Error, fmt};

use base64::{
    Engine as _,
    engine::general_purpose::{STANDARD, STANDARD_NO_PAD},
};
use buffa::{Message, MessageName};
use connectrpc::{ConnectError, ErrorCode};
use home_wire_contract::home::{
    auth::v1::AuthorizationTermination,
};
use home_wire_contract::{
    api_errors::{self, ApiFailure, InvalidApiError},
    google::rpc::Status,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DetailFailure {
    Malformed,
    Duplicate,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FailureKind {
    InvalidConfiguration,
    UntrustedCertificate,
    WrongServerName,
    TlsRejected,
    NetworkInterrupted,
    Deadline,
    Cancelled,
    Rpc(ErrorCode),
}

/// The diagnostic representation excludes headers, response text and credentials.
pub struct Failure {
    kind: FailureKind,
    not_dispatched: bool,
    rpc: Option<Box<ConnectError>>,
    api: Result<Option<ApiFailure>, InvalidApiError>,
}

impl Failure {
    /// Canonical details are parsed once on receipt, independently of transport
    /// failures. Malformed details stay an explicit failure.
    pub fn api_failure(&self) -> Result<Option<&ApiFailure>, InvalidApiError> {
        self.api
            .as_ref()
            .map(|value| value.as_ref())
            .map_err(|error| *error)
    }

    fn canonical_read_recovery(&self) -> bool {
        use home_wire_contract::home::api::v2::ApiErrorReason;
        match self.api_failure() {
            Ok(None) => true,
            Ok(Some(failure)) => !matches!(
                failure.reason(),
                ApiErrorReason::AuthenticationRequired | ApiErrorReason::AccessDenied
            ),
            Err(_) => false,
        }
    }
    pub fn kind(&self) -> FailureKind {
        self.kind
    }

    /// True only when the typed HTTP connection failure proves the request
    /// never reached a sender. This does not make TLS failures retryable and
    /// must never reclassify send, response, deadline or cancellation errors.
    pub fn request_not_dispatched(&self) -> bool {
        self.not_dispatched
    }

    /// A read may reconnect after a transient transport/remote cancellation.
    /// Local cancellation, TLS and any authority detail always stop it. This
    /// decision must never be used to replay a command or session refresh.
    pub fn may_resume_read(&self) -> bool {
        matches!(
            self.kind,
            FailureKind::NetworkInterrupted
                | FailureKind::Deadline
                | FailureKind::Rpc(
                    ErrorCode::Unavailable
                        | ErrorCode::Canceled
                        | ErrorCode::Internal
                        | ErrorCode::Unknown
                        | ErrorCode::ResourceExhausted
                )
        ) && matches!(self.authorization_termination(), Ok(None))
            && self.canonical_read_recovery()
    }

    /// Only the identified, durably saved renewal operation may use this
    /// recovery policy, after admitting its canonical error. It never authorizes
    /// replaying another mutation.
    pub fn may_recover_renewal(&self) -> bool {
        use crate::RpcCode;
        let transient = matches!(
            self.kind,
            FailureKind::NetworkInterrupted
                | FailureKind::Deadline
                | FailureKind::Rpc(
                    RpcCode::Unavailable
                        | RpcCode::Canceled
                        | RpcCode::Internal
                        | RpcCode::Unknown
                        | RpcCode::ResourceExhausted
                        | RpcCode::Unauthenticated
                )
        );
        transient
            && matches!(self.authorization_termination(), Ok(None))
            && self.canonical_read_recovery()
    }

    /// Inspect typed details explicitly; callers must not log the raw response.
    pub fn rpc(&self) -> Option<&ConnectError> {
        self.rpc.as_deref()
    }

    /// Decode the existing contract with Buffa. Unknown details stay available
    /// in `rpc()`; a malformed or duplicate known detail is never treated as absent.
    pub fn authorization_termination(
        &self,
    ) -> Result<Option<AuthorizationTermination>, DetailFailure> {
        self.typed_detail()
    }

    fn typed_detail<T: Message + MessageName>(&self) -> Result<Option<T>, DetailFailure> {
        let mut found = None;
        for detail in self.rpc().into_iter().flat_map(|rpc| &rpc.details) {
            if detail.type_url != T::FULL_NAME && detail.type_url != T::TYPE_URL {
                continue;
            }
            if found.is_some() {
                return Err(DetailFailure::Duplicate);
            }
            let value = detail.value.as_deref().ok_or(DetailFailure::Malformed)?;
            let bytes = STANDARD_NO_PAD
                .decode(value)
                .or_else(|_| STANDARD.decode(value))
                .map_err(|_| DetailFailure::Malformed)?;
            found = Some(T::decode_from_slice(&bytes).map_err(|_| DetailFailure::Malformed)?);
        }
        Ok(found)
    }

    pub(crate) fn invalid_configuration() -> Self {
        Self {
            kind: FailureKind::InvalidConfiguration,
            not_dispatched: false,
            rpc: None,
            api: Ok(None),
        }
    }

    pub(crate) fn cancelled() -> Self {
        Self {
            kind: FailureKind::Cancelled,
            not_dispatched: false,
            rpc: None,
            api: Ok(None),
        }
    }
}

impl From<ConnectError> for Failure {
    fn from(error: ConnectError) -> Self {
        let mut kind = FailureKind::Rpc(error.code);
        let mut not_dispatched = false;
        // Caller cancellation is produced by our CancellationToken branches.
        // A server's Canceled status (including graceful restart) stays an RPC.
        if error.code == ErrorCode::DeadlineExceeded {
            kind = FailureKind::Deadline;
        }
        let mut sources: Vec<&(dyn Error + 'static)> = vec![&error];
        for _ in 0..16 {
            let Some(current) = sources.pop() else { break };
            if let Some(connection) = current.downcast_ref::<hyper_util::client::legacy::Error>() {
                not_dispatched |= connection.is_connect();
            }
            if current.is::<connectrpc::client::ResponseBodyReadError>() {
                not_dispatched = false;
                kind = FailureKind::NetworkInterrupted;
                break;
            }
            if let Some(tls) = current.downcast_ref::<rustls::Error>() {
                kind = if let rustls::Error::InvalidCertificate(reason) = tls {
                    if matches!(reason, rustls::CertificateError::UnknownIssuer) {
                        FailureKind::UntrustedCertificate
                    } else if matches!(
                        reason,
                        rustls::CertificateError::NotValidForName
                            | rustls::CertificateError::NotValidForNameContext { .. }
                    ) {
                        FailureKind::WrongServerName
                    } else {
                        FailureKind::TlsRejected
                    }
                } else {
                    FailureKind::TlsRejected
                };
                break;
            }
            if let Some(source) = current.source() {
                sources.push(source);
            }
            // io::Error's payload is not necessarily exposed through Error::source.
            if let Some(io) = current.downcast_ref::<std::io::Error>()
                && let Some(payload) = io.get_ref()
            {
                sources.push(payload);
            }
        }
        let api = admit_api_error(&error);
        Self {
            kind,
            not_dispatched,
            rpc: Some(Box::new(error)),
            api,
        }
    }
}

impl fmt::Debug for Failure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Failure").field("kind", &self.kind).finish()
    }
}

impl fmt::Display for Failure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:?}", self.kind)
    }
}

impl Error for Failure {}

fn admit_api_error(error: &ConnectError) -> Result<Option<ApiFailure>, InvalidApiError> {
    if !error.details.iter().any(|detail| {
        matches!(
            detail.type_url.as_str(),
            "google.rpc.ErrorInfo"
                | "type.googleapis.com/google.rpc.ErrorInfo"
                | "home.api.v2.ExecutionUncertainty"
                | "type.googleapis.com/home.api.v2.ExecutionUncertainty"
        )
    }) {
        return Ok(None);
    }
    if error.details.len() > 10 {
        return Err(InvalidApiError::Limit);
    }
    let mut status = Status {
        code: error.code.grpc_code() as i32,
        ..Default::default()
    };
    let mut total = 0;
    let mut seen = std::collections::BTreeSet::new();
    for detail in &error.details {
        if detail.type_url.len() > 256 {
            return Err(InvalidApiError::Limit);
        }
        let kind = detail
            .type_url
            .strip_prefix("type.googleapis.com/")
            .unwrap_or(&detail.type_url);
        if !seen.insert(kind) {
            return Err(InvalidApiError::Duplicate);
        }
        let value = detail.value.as_deref().ok_or(InvalidApiError::Malformed)?;
        if value.len() > api_errors::MAX_DETAILS_BYTES.div_ceil(3) * 4 {
            return Err(InvalidApiError::Limit);
        }
        let bytes = STANDARD_NO_PAD
            .decode(value)
            .or_else(|_| STANDARD.decode(value))
            .map_err(|_| InvalidApiError::Malformed)?;
        total += bytes.len();
        if total > api_errors::MAX_DETAILS_BYTES {
            return Err(InvalidApiError::Limit);
        }
        status.details.push(buffa_types::google::protobuf::Any {
            type_url: detail.type_url.clone(),
            value: bytes.into(),
            ..Default::default()
        });
    }
    api_errors::admit_status(&status).map(Some)
}
