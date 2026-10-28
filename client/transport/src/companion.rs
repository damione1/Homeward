//! Provisional companion facade on the shared verified TLS connection.
//! One attempt per explicit request; no publication retry or offline queue.
use crate::{AccessToken, Failure, StateTransport, services::home::companion::v1 as service};
use connectrpc::client::HttpClient;
use home_wire_contract::home::companion::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct CompanionTransport {
    pub(crate) client: service::CompanionServiceClient<HttpClient>,
}
macro_rules! unary {
    ($name:ident, $method:ident, $request:ident, $response:ident) => {
        pub async fn $name(
            &self,
            request: wire::$request,
            token: &AccessToken,
            timeout: Duration,
            cancel: &CancellationToken,
        ) -> Result<wire::$response, Failure> {
            let options = StateTransport::options(token, timeout)?;
            tokio::select! { biased;
                () = cancel.cancelled() => Err(Failure::cancelled()),
                response = self.client.$method(request, options) =>
                    response.map(|value| value.into_owned()).map_err(Failure::from),
            }
        }
    };
}
impl CompanionTransport {
    unary!(
        describe_companion,
        describe_companion_with_options,
        DescribeCompanionRequest,
        DescribeCompanionResponse
    );
    unary!(
        register_installation,
        register_installation_with_options,
        RegisterInstallationRequest,
        RegisterInstallationResponse
    );
    unary!(
        get_installation,
        get_installation_with_options,
        GetInstallationRequest,
        GetInstallationResponse
    );
    unary!(
        update_consent,
        update_consent_with_options,
        UpdateConsentRequest,
        UpdateConsentResponse
    );
    unary!(
        publish_telemetry,
        publish_telemetry_with_options,
        PublishTelemetryRequest,
        PublishTelemetryResponse
    );
    unary!(
        revoke_installation,
        revoke_installation_with_options,
        RevokeInstallationRequest,
        RevokeInstallationResponse
    );
}
