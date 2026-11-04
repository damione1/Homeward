//! Single-attempt Dashboard v2 reads over the verified TLS pool.
//! Query binds configuration to a capture; it never dispatches a command.
use crate::{AccessToken, Failure, services::home::api::v2 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::api::v2 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct DashboardTransport {
    pub(crate) homes: crate::HomeTransport,
    pub(crate) client: service::DashboardServiceClient<HttpClient>,
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
                result = self.client.$method(request, options) =>
                    result.map(|value| value.into_owned()).map_err(Failure::from),
            }
        }
    };
}
impl DashboardTransport {
    pub fn home(&self) -> &crate::HomeTransport {
        &self.homes
    }
    unary!(
        get_dashboard,
        get_dashboard_with_options,
        GetDashboardRequest,
        Dashboard
    );
    unary!(
        list_dashboards,
        list_dashboards_with_options,
        ListDashboardsRequest,
        ListDashboardsResponse
    );
    unary!(
        query_dashboard,
        query_dashboard_with_options,
        QueryDashboardRequest,
        QueryDashboardResponse
    );
    pub async fn watch_dashboards(
        &self,
        request: wire::WatchDashboardsRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<DashboardWatch, Failure> {
        let options = token.options(timeout)?;
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            result = self.client.watch_dashboards_with_options(request, options) =>
                result.map(|inner| DashboardWatch { inner: Some(inner), cancel: cancel.child_token() }).map_err(Failure::from),
        }
    }
}
type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    wire::__buffa::view::WatchDashboardsResponseView<'static>,
>;
pub struct DashboardWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}
impl DashboardWatch {
    pub async fn next(&mut self) -> Result<Option<wire::WatchDashboardsResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! { biased;
            () = self.cancel.cancelled() => Err(Failure::cancelled()),
            result = inner.message() => result.map(|value| value.map(|message| message.to_owned_message())).map_err(Failure::from),
        };
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}
impl Drop for DashboardWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
