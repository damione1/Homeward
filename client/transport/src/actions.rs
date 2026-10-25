use std::time::Duration;

use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::actions::v1 as actions;
use tokio_util::sync::CancellationToken;

use crate::{AccessToken, Failure, StateTransport, services::home::actions::v1 as service};

/// Single-attempt action RPCs on the shared verified TLS connection. The host
/// owns admission, command IDs, session rotation and recovery through Get/Watch.
#[derive(Clone)]
pub struct ActionTransport {
    pub(crate) client: service::ActionServiceClient<HttpClient>,
}

impl ActionTransport {
    pub async fn submit_action(
        &self,
        request: actions::SubmitActionRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<actions::SubmitActionResponse, Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.submit_action_with_options(request, options) => {
                response.map(|value| value.into_owned()).map_err(Failure::from)
            }
        }
    }

    pub async fn get_action(
        &self,
        request: actions::GetActionRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<actions::GetActionResponse, Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.get_action_with_options(request, options) => {
                response.map(|value| value.into_owned()).map_err(Failure::from)
            }
        }
    }

    pub async fn watch_action(
        &self,
        request: actions::WatchActionRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<ActionWatch, Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.watch_action_with_options(request, options) => {
                response.map(|inner| ActionWatch { inner: Some(inner), cancel: cancel.child_token() }).map_err(Failure::from)
            }
        }
    }
}

type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    actions::__buffa::view::WatchActionResponseView<'static>,
>;

pub struct ActionWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}

impl ActionWatch {
    pub async fn next(&mut self) -> Result<Option<actions::WatchActionResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! {
            biased;
            () = self.cancel.cancelled() => Err(Failure::cancelled()),
            response = inner.message() => response.map(|value| value.map(|message| message.to_owned_message())).map_err(Failure::from),
        };
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}

impl Drop for ActionWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
