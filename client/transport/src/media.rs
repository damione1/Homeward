//! Read-only media and bounded artwork over the shared verified TLS pool.
//! Commands continue through ActionTransport and the session action controller.
use crate::{AccessToken, Failure, StateTransport, services::home::media::v1 as service};
use connectrpc::client::HttpClient;
use home_wire_contract::home::media::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct MediaTransport {
    pub(crate) client: service::MediaServiceClient<HttpClient>,
}

macro_rules! read {
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

impl MediaTransport {
    read!(
        read_snapshot,
        read_snapshot_with_options,
        ReadSnapshotRequest,
        ReadSnapshotResponse
    );
    read!(
        read_artwork,
        read_artwork_with_options,
        ReadArtworkRequest,
        ReadArtworkResponse
    );
}
