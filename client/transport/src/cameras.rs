//! Bounded camera metadata/images on the existing native verified TLS pool.
//! No provider URLs, image cache or device command execution belongs here.
use crate::{AccessToken, Failure, StateTransport, services::home::cameras::v1 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::cameras::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct CameraTransport {
    pub(crate) client: service::CameraServiceClient<HttpClient>,
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
                    response.map(|value|value.into_owned()).map_err(Failure::from),
            }
        }
    };
}
impl CameraTransport {
    unary!(
        list_cameras,
        list_cameras_with_options,
        ListCamerasRequest,
        ListCamerasResponse
    );
    unary!(
        open_session,
        open_session_with_options,
        OpenSessionRequest,
        OpenSessionResponse
    );
    unary!(
        close_session,
        close_session_with_options,
        CloseSessionRequest,
        CloseSessionResponse
    );
    unary!(
        read_image,
        read_image_with_options,
        ReadImageRequest,
        ReadImageResponse
    );
    unary!(
        list_events,
        list_events_with_options,
        ListEventsRequest,
        ListEventsResponse
    );

    pub async fn watch_session(
        &self,
        request: wire::WatchSessionRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<CameraWatch, Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.watch_session_with_options(request, options) =>
                response.map(|inner|CameraWatch {inner:Some(inner),cancel:cancel.child_token()}).map_err(Failure::from),
        }
    }
}
type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    wire::__buffa::view::WatchSessionResponseView<'static>,
>;
pub struct CameraWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}
impl CameraWatch {
    pub async fn next(&mut self) -> Result<Option<wire::WatchSessionResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! { biased;
            () = self.cancel.cancelled() => Err(Failure::cancelled()),
            response = inner.message() => response.map(|frame|frame.map(|value|value.to_owned_message())).map_err(Failure::from),
        };
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}
impl Drop for CameraWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
