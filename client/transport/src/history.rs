//! Single-attempt read-only history calls on the shared verified TLS pool.
//! The host owns query generations, bounded pagination and response admission;
//! wire page/not-retained/stale-cursor/budget variants remain intact here.
use crate::{AccessToken, Failure, StateTransport, services::home::history::v1 as service};
use connectrpc::client::HttpClient;
use home_wire_contract::home::history::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct HistoryTransport {
    pub(crate) client: service::HistoryServiceClient<HttpClient>,
}

macro_rules! query {
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
impl HistoryTransport {
    query!(
        query_observations,
        query_observations_with_options,
        QueryObservationsRequest,
        QueryObservationsResponse
    );
    query!(
        query_aggregates,
        query_aggregates_with_options,
        QueryAggregatesRequest,
        QueryAggregatesResponse
    );
    query!(
        query_execution_traces,
        query_execution_traces_with_options,
        QueryExecutionTracesRequest,
        QueryExecutionTracesResponse
    );
}
