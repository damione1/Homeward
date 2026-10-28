//! Provisional notification facade on the shared verified TLS connection.
//! One attempt per explicit request; no acknowledgement retry or offline queue.
use crate::{AccessToken, Failure, StateTransport, services::home::notifications::v1 as service};
use connectrpc::client::HttpClient;
use home_wire_contract::home::notifications::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct NotificationTransport {
    pub(crate) client: service::NotificationServiceClient<HttpClient>,
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
impl NotificationTransport {
    unary!(
        describe_notifications,
        describe_notifications_with_options,
        DescribeNotificationsRequest,
        DescribeNotificationsResponse
    );
    unary!(
        register_destination,
        register_destination_with_options,
        RegisterDestinationRequest,
        RegisterDestinationResponse
    );
    unary!(
        get_destination,
        get_destination_with_options,
        GetDestinationRequest,
        GetDestinationResponse
    );
    unary!(
        update_destination,
        update_destination_with_options,
        UpdateDestinationRequest,
        UpdateDestinationResponse
    );
    unary!(
        read_inbox,
        read_inbox_with_options,
        ReadInboxRequest,
        ReadInboxResponse
    );
    unary!(
        acknowledge_delivery,
        acknowledge_delivery_with_options,
        AcknowledgeDeliveryRequest,
        AcknowledgeDeliveryResponse
    );
    unary!(
        get_delivery,
        get_delivery_with_options,
        GetDeliveryRequest,
        GetDeliveryResponse
    );
    unary!(
        resolve_open,
        resolve_open_with_options,
        ResolveOpenRequest,
        ResolveOpenResponse
    );
    unary!(
        revoke_destination,
        revoke_destination_with_options,
        RevokeDestinationRequest,
        RevokeDestinationResponse
    );
}
