//! Anonymous acceptance uses the verified profile transport. It never creates a
//! native session, retries a write, persists the invitation, or logs its inputs.
use home_client_transport::{AccountTransport, FailureKind, RpcCode};
use home_wire_contract::{auth_contract, home::auth::v1 as auth};
use std::time::Duration;
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

pub(crate) struct Request {
    pub token: Zeroizing<String>,
    pub password: Zeroizing<String>,
    pub display_name: String,
}
impl Request {
    pub fn valid(&self) -> bool {
        !self.token.is_empty()
            && self.token.len() <= 4096
            && !self.token.contains(['\0', '\r', '\n'])
            && !self.password.is_empty()
            && self.password.len() <= 4096
            && !self.password.contains('\0')
            && self.display_name.len() <= 256
            && !self.display_name.contains(['\0', '\r', '\n'])
    }
}
pub(crate) enum Outcome {
    Accepted { login: String },
    Rejected,
    Uncertain,
    Cancelled,
}
pub(crate) async fn accept(
    api: &AccountTransport,
    request: Request,
    cancel: &CancellationToken,
) -> Outcome {
    if cancel.is_cancelled() {
        return Outcome::Cancelled;
    }
    if !request.valid() {
        return Outcome::Rejected;
    }
    let response = api
        .accept_invitation(
            auth::AcceptInvitationRequest {
                invitation_token: request.token.to_string(),
                password: request.password.to_string(),
                display_name: request.display_name,
                ..Default::default()
            },
            Duration::from_secs(4),
            cancel,
        )
        .await;
    match response {
        Ok(response) => {
            if response.__buffa_unknown_fields.is_empty()
                && let Some(account) = response.account.into_option()
                && auth_contract::admit_account(&account).is_ok()
                && account.status == auth::AccountStatus::Active
                && account.login_name.len() <= 256
            {
                Outcome::Accepted {
                    login: account.login_name,
                }
            } else {
                Outcome::Uncertain
            }
        }
        Err(error) => match error.kind() {
            FailureKind::Rpc(code) => {
                if matches!(
                    code,
                    RpcCode::InvalidArgument
                        | RpcCode::Unauthenticated
                        | RpcCode::PermissionDenied
                        | RpcCode::AlreadyExists
                        | RpcCode::NotFound
                        | RpcCode::FailedPrecondition
                ) {
                    Outcome::Rejected
                } else {
                    Outcome::Uncertain
                }
            }
            FailureKind::InvalidConfiguration
            | FailureKind::UntrustedCertificate
            | FailureKind::WrongServerName
            | FailureKind::TlsRejected
            | FailureKind::NetworkInterrupted
            | FailureKind::Deadline
            | FailureKind::Cancelled => Outcome::Uncertain,
        },
    }
}
