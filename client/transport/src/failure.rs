use std::{error::Error, fmt};

use base64::{
    Engine as _,
    engine::general_purpose::{STANDARD, STANDARD_NO_PAD},
};
use buffa::{Message, MessageName};
use connectrpc::{ConnectError, ErrorCode};
use home_wire_contract::home::{
    actions::v1::ActionOperationFailure,
    auth::v1::{
        AuthError, AuthorizationPolicyError, AuthorizationTermination, SecondFactorPolicyError,
    },
    calendars::v1::CalendarError,
    integrationadmin::v1::IntegrationError,
    lists::v1::ListError,
    observability::v1::ObservabilityError,
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
    rpc: Option<Box<ConnectError>>,
}

impl Failure {
    pub fn kind(&self) -> FailureKind {
        self.kind
    }

    /// A read may reconnect after a transient transport/remote cancellation.
    /// Local cancellation, TLS and any authority detail always stop it. This
    /// decision must never be used to replay a command or session refresh.
    pub fn may_resume_read(&self) -> bool {
        matches!(
            self.kind,
            FailureKind::NetworkInterrupted
                | FailureKind::Deadline
                | FailureKind::Rpc(ErrorCode::Unavailable | ErrorCode::Canceled)
        ) && matches!(self.authorization_termination(), Ok(None))
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

    /// A dispatch response can accompany an error through this detail. Keep
    /// its exact command ID and uncertainty; an error is no retry permission.
    pub fn action_operation_failure(
        &self,
    ) -> Result<Option<ActionOperationFailure>, DetailFailure> {
        self.typed_detail()
    }

    /// A list conflict or uncertain commit is a typed outcome, never a reason
    /// to replay the mutation or expose the server's free-form error message.
    pub fn list_error(&self) -> Result<Option<ListError>, DetailFailure> {
        self.typed_detail()
    }

    /// Calendar conflict/uncertainty is an explicit domain outcome; an Aborted
    /// RPC status alone cannot distinguish them and never permits a replay.
    pub fn calendar_error(&self) -> Result<Option<CalendarError>, DetailFailure> {
        self.typed_detail()
    }

    pub fn auth_error(&self) -> Result<Option<AuthError>, DetailFailure> {
        self.typed_detail()
    }

    pub fn second_factor_policy_error(
        &self,
    ) -> Result<Option<SecondFactorPolicyError>, DetailFailure> {
        self.typed_detail()
    }

    pub fn authorization_policy_error(
        &self,
    ) -> Result<Option<AuthorizationPolicyError>, DetailFailure> {
        self.typed_detail()
    }

    /// Preserve the exact conflicting document version; a conflict never permits retry.
    pub fn integration_error(&self) -> Result<Option<IntegrationError>, DetailFailure> {
        self.typed_detail()
    }

    pub fn observability_error(&self) -> Result<Option<ObservabilityError>, DetailFailure> {
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
            rpc: None,
        }
    }

    pub(crate) fn cancelled() -> Self {
        Self {
            kind: FailureKind::Cancelled,
            rpc: None,
        }
    }
}

impl From<ConnectError> for Failure {
    fn from(error: ConnectError) -> Self {
        let mut kind = FailureKind::Rpc(error.code);
        // Caller cancellation is produced by our CancellationToken branches.
        // A server's Canceled status (including graceful restart) stays an RPC.
        if error.code == ErrorCode::DeadlineExceeded {
            kind = FailureKind::Deadline;
        }
        let mut sources: Vec<&(dyn Error + 'static)> = vec![&error];
        for _ in 0..16 {
            let Some(current) = sources.pop() else { break };
            if current.is::<connectrpc::client::ResponseBodyReadError>() {
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
        Self {
            kind,
            rpc: Some(Box::new(error)),
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
