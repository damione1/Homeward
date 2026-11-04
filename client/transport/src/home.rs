//! Single-attempt home discovery on the shared verified TLS connection.
//! The server owns the persistent installation identity and current visibility.
use crate::{AccessToken, Failure, services::home::api::v2 as service};
use connectrpc::client::HttpClient;
use home_wire_contract::home::api::v2 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct HomeTransport {
    pub(crate) client: service::HomeServiceClient<HttpClient>,
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
            let options = token.options(timeout)?;
            tokio::select! { biased;
                () = cancel.cancelled() => Err(Failure::cancelled()),
                response = self.client.$method(request, options) => {
                    response.map(|value| value.into_owned()).map_err(Failure::from)
                }
            }
        }
    };
}
impl HomeTransport {
    unary!(
        get_entity,
        get_entity_with_options,
        GetEntityRequest,
        Entity
    );
    unary!(
        list_entities,
        list_entities_with_options,
        ListEntitiesRequest,
        ListEntitiesResponse
    );
    unary!(
        get_capability,
        get_capability_with_options,
        GetCapabilityRequest,
        Capability
    );
    unary!(
        list_capabilities,
        list_capabilities_with_options,
        ListCapabilitiesRequest,
        ListCapabilitiesResponse
    );
    unary!(get_home, get_home_with_options, GetHomeRequest, Home);
    unary!(
        list_homes,
        list_homes_with_options,
        ListHomesRequest,
        ListHomesResponse
    );
}
