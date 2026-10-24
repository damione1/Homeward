use std::time::Duration;

use connectrpc::client::{CallOptions, HttpClient};
use home_wire_contract::home::auth::v1 as auth;
use tokio_util::sync::CancellationToken;

use crate::{AccessToken, Failure, StateTransport, services::home::auth::v1 as service};

/// Single-attempt Auth calls. In particular, an uncertain RefreshSession is
/// never retried here. The session coordinator owns rotation and persistence.
#[derive(Clone)]
pub struct AuthTransport {
    pub(crate) client: service::AuthServiceClient<HttpClient>,
}

macro_rules! unauthenticated_call {
    ($name:ident, $wire:ident, $request:ident, $response:ident) => {
        pub async fn $name(&self, request: auth::$request, timeout: Duration, cancel: &CancellationToken) -> Result<auth::$response, Failure> {
            tokio::select! {
                biased;
                () = cancel.cancelled() => Err(Failure::cancelled()),
                response = self.client.$wire(request, CallOptions::default().with_timeout(timeout)) => {
                    response.map(|value| value.into_owned()).map_err(Failure::from)
                }
            }
        }
    };
}

impl AuthTransport {
    pub async fn list_sessions(
        &self,
        request: auth::ListSessionsRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<auth::ListSessionsResponse, Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.list_sessions_with_options(request, options) => response.map(|value| value.into_owned()).map_err(Failure::from),
        }
    }
    unauthenticated_call!(
        start_login,
        start_login_with_options,
        StartLoginRequest,
        StartLoginResponse
    );
    unauthenticated_call!(
        complete_login,
        complete_login_with_options,
        CompleteLoginRequest,
        CompleteLoginResponse
    );
    unauthenticated_call!(
        refresh_session,
        refresh_session_with_options,
        RefreshSessionRequest,
        RefreshSessionResponse
    );

    pub async fn revoke_session(
        &self,
        request: auth::RevokeSessionRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<(), Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.revoke_session_with_options(request, options) => response.map(|_| ()).map_err(Failure::from),
        }
    }
}
