//! Standard Operations calls on the shared verified TLS pool. Each invocation
//! makes one attempt; cancelling a local read never cancels a server operation.
use crate::{AccessToken, Failure, services::google::longrunning as service};
use buffa_types::google::protobuf::Empty;
use connectrpc::client::HttpClient;
use home_wire_contract::google::longrunning as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct OperationsTransport {
    pub(crate) client: service::OperationsClient<HttpClient>,
}

macro_rules! unary {
    ($name:ident, $method:ident, $request:ident, $response:ty) => {
        pub async fn $name(
            &self,
            request: wire::$request,
            token: &AccessToken,
            timeout: Duration,
            cancel: &CancellationToken,
        ) -> Result<$response, Failure> {
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

impl OperationsTransport {
    unary!(
        get_operation,
        get_operation_with_options,
        GetOperationRequest,
        wire::Operation
    );
    unary!(
        wait_operation,
        wait_operation_with_options,
        WaitOperationRequest,
        wire::Operation
    );
    unary!(
        list_operations,
        list_operations_with_options,
        ListOperationsRequest,
        wire::ListOperationsResponse
    );
    unary!(
        cancel_operation,
        cancel_operation_with_options,
        CancelOperationRequest,
        Empty
    );
    unary!(
        delete_operation,
        delete_operation_with_options,
        DeleteOperationRequest,
        Empty
    );
}
