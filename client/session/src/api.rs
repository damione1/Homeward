use std::time::Duration;

use async_trait::async_trait;
use home_client_transport::{AccessToken, AuthTransport, Failure};
use home_wire_contract::home::api::v2 as auth;
use tokio_util::sync::CancellationToken;

#[async_trait]
pub(crate) trait AuthApi: Send + Sync {
    async fn revoke(
        &self,
        request: auth::RevokeSessionRequest,
        token: &AccessToken,
        cancel: &CancellationToken,
    ) -> Result<(), Failure>;
    async fn start_login(
        &self,
        request: auth::StartLoginRequest,
        cancel: &CancellationToken,
    ) -> Result<auth::StartLoginResponse, Failure>;
    async fn complete_login(
        &self,
        request: auth::CompleteLoginRequest,
        cancel: &CancellationToken,
    ) -> Result<auth::CompleteLoginResponse, Failure>;
    async fn preflight_refresh(
        &self,
        request: auth::ListSessionsRequest,
        token: &AccessToken,
        cancel: &CancellationToken,
    ) -> Result<(), Failure>;
    async fn refresh(
        &self,
        request: auth::RefreshSessionRequest,
        cancel: &CancellationToken,
    ) -> Result<auth::RefreshSessionResponse, Failure>;
}

#[async_trait]
impl AuthApi for AuthTransport {
    async fn revoke(
        &self,
        request: auth::RevokeSessionRequest,
        token: &AccessToken,
        cancel: &CancellationToken,
    ) -> Result<(), Failure> {
        self.revoke_session(request, token, Duration::from_secs(5), cancel)
            .await
    }
    async fn start_login(
        &self,
        request: auth::StartLoginRequest,
        cancel: &CancellationToken,
    ) -> Result<auth::StartLoginResponse, Failure> {
        self.start_login(request, Duration::from_secs(10), cancel)
            .await
    }
    async fn complete_login(
        &self,
        request: auth::CompleteLoginRequest,
        cancel: &CancellationToken,
    ) -> Result<auth::CompleteLoginResponse, Failure> {
        self.complete_login(request, Duration::from_secs(10), cancel)
            .await
    }
    async fn preflight_refresh(
        &self,
        request: auth::ListSessionsRequest,
        token: &AccessToken,
        cancel: &CancellationToken,
    ) -> Result<(), Failure> {
        self.list_sessions(request, token, Duration::from_secs(5), cancel)
            .await
            .map(|_| ())
    }
    async fn refresh(
        &self,
        request: auth::RefreshSessionRequest,
        cancel: &CancellationToken,
    ) -> Result<auth::RefreshSessionResponse, Failure> {
        // One bounded dispatch. The owner persists a renewal key before send;
        // a later attempt recovers that same operation after a lost reply.
        self.refresh_session(request, Duration::from_secs(10), cancel)
            .await
    }
}
