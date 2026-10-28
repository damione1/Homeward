//! Provisional logic facade on the same verified connection. Each explicit
//! mutation has one attempt; lookup reads never retry a start or cancellation.
use crate::{AccessToken, Failure, StateTransport, services::home::logic::v1 as service};
use connectrpc::client::HttpClient;
use home_wire_contract::home::logic::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct LogicTransport {
    pub(crate) client: service::LogicServiceClient<HttpClient>,
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
impl LogicTransport {
    unary!(
        list_definitions,
        list_definitions_with_options,
        ListDefinitionsRequest,
        ListDefinitionsResponse
    );
    unary!(
        list_runs,
        list_runs_with_options,
        ListRunsRequest,
        ListRunsResponse
    );
    unary!(
        start_run,
        start_run_with_options,
        StartRunRequest,
        StartRunResponse
    );
    unary!(get_run, get_run_with_options, GetRunRequest, GetRunResponse);
    unary!(
        cancel_run,
        cancel_run_with_options,
        CancelRunRequest,
        CancelRunResponse
    );
    unary!(
        read_trace,
        read_trace_with_options,
        ReadTraceRequest,
        ReadTraceResponse
    );
}
