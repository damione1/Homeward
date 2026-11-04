//! One explicit typed capability execution; recovery never replays this call.
use crate::{AccessToken, Failure, services::home::api::v2 as service};
use connectrpc::client::HttpClient;
use home_wire_contract::{google::longrunning::Operation, home::api::v2::ExecuteCapabilityRequest};
use std::time::Duration;
use tokio_util::sync::CancellationToken;
#[derive(Clone)]
pub struct CapabilityTransport {
    pub(crate) client: service::CapabilityServiceClient<HttpClient>,
}
impl CapabilityTransport {
    pub async fn execute_capability(
        &self,
        request: ExecuteCapabilityRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<Operation, Failure> {
        let options = token.options(timeout)?;
        tokio::select! {biased;()=cancel.cancelled()=>Err(Failure::cancelled()),response=self.client.execute_capability_with_options(request,options)=>response.map(|value|value.into_owned()).map_err(Failure::from)}
    }
}
