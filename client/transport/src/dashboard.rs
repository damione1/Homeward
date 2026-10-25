//! Read-only dashboard facade over the same verified TLS pool as State/Auth.
use crate::{AccessToken, Failure, StateTransport, services::home::dashboard::v1 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::dashboard::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct DashboardTransport {
    pub(crate) client: service::DashboardServiceClient<HttpClient>,
}
impl DashboardTransport {
    fn support() -> wire::ClientLayoutSupport {
        wire::ClientLayoutSupport {
            profile: "homeward-dashboard-1".into(),
            version: 1,
            kinds: [
                wire::LayoutKind::View,
                wire::LayoutKind::Section,
                wire::LayoutKind::Grid,
                wire::LayoutKind::HorizontalStack,
                wire::LayoutKind::Heading,
                wire::LayoutKind::Tile,
                wire::LayoutKind::Unsupported,
            ]
            .into_iter()
            .map(Into::into)
            .collect(),
            ..Default::default()
        }
    }
    pub async fn list(
        &self,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<wire::ListDashboardsResponse, Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            result = self.client.list_dashboards_with_options(wire::ListDashboardsRequest { support: Self::support().into(), ..Default::default() }, options) =>
                result.map(|value| value.into_owned()).map_err(Failure::from),
        }
    }
    pub async fn get(
        &self,
        id: String,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<wire::GetDashboardResponse, Failure> {
        let options = StateTransport::options(token, timeout)?;
        let request = wire::GetDashboardRequest {
            support: Self::support().into(),
            id: wire::DashboardId {
                value: id,
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            result = self.client.get_dashboard_with_options(request, options) =>
                result.map(|value| value.into_owned()).map_err(Failure::from),
        }
    }
    pub async fn watch(
        &self,
        resume_token: String,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<DashboardWatch, Failure> {
        let options = StateTransport::options(token, timeout)?;
        let request = wire::WatchDashboardsRequest {
            resume_token,
            ..Default::default()
        };
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
