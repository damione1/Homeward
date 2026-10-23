use std::{io::Cursor, sync::Arc, time::Duration};

use connectrpc::client::{CallOptions, ClientConfig, ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::state::v1 as state;
use http::{HeaderValue, Uri};
use tokio_util::sync::CancellationToken;

use crate::{Failure, services::home::state::v1 as service};

/// An in-memory credential. Session coordination and secure persistence belong to NHF-003.
#[derive(Clone)]
pub struct AccessToken(HeaderValue);

impl AccessToken {
    pub fn parse(token: &str) -> Result<Self, Failure> {
        if token.is_empty() {
            return Err(Failure::invalid_configuration());
        }
        let mut header = HeaderValue::from_str(&format!("Bearer {token}"))
            .map_err(|_| Failure::invalid_configuration())?;
        header.set_sensitive(true);
        Ok(Self(header))
    }
}

/// Read RPCs only: this layer never owns UI widgets, refreshes a session or retries writes.
#[derive(Clone)]
pub struct StateTransport {
    client: service::StateServiceClient<HttpClient>,
}

impl StateTransport {
    /// Require an HTTPS origin and explicitly supplied trusted CA certificates.
    /// Rustls keeps its standard chain and server-name verification enabled.
    pub fn new(origin: &str, trusted_ca_pem: &[u8]) -> Result<Self, Failure> {
        let uri: Uri = origin
            .parse()
            .map_err(|_| Failure::invalid_configuration())?;
        if uri.scheme_str() != Some("https")
            || uri.authority().is_none()
            || !matches!(uri.path(), "" | "/")
            || uri.query().is_some()
            || uri
                .authority()
                .is_some_and(|value| value.as_str().contains('@'))
        {
            return Err(Failure::invalid_configuration());
        }
        let mut roots = rustls::RootCertStore::empty();
        for certificate in rustls_pemfile::certs(&mut Cursor::new(trusted_ca_pem)) {
            roots
                .add(certificate.map_err(|_| Failure::invalid_configuration())?)
                .map_err(|_| Failure::invalid_configuration())?;
        }
        if roots.is_empty() {
            return Err(Failure::invalid_configuration());
        }
        // Select the provider explicitly even if another workspace dependency enables ring.
        let tls = rustls::ClientConfig::builder_with_provider(Arc::new(
            rustls::crypto::aws_lc_rs::default_provider(),
        ))
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(|_| Failure::invalid_configuration())?
        .with_root_certificates(roots)
        .with_no_client_auth();
        let http = HttpClient::builder()
            .tcp_connect_timeout(Duration::from_secs(3))
            .establishment_timeout(Duration::from_secs(5))
            .with_tls(Arc::new(tls));
        let config = ClientConfig::new(uri).with_default_max_message_size(1024 * 1024);
        Ok(Self {
            client: service::StateServiceClient::new(http, config),
        })
    }

    fn options(token: &AccessToken, timeout: Duration) -> Result<CallOptions, Failure> {
        CallOptions::default()
            .with_timeout(timeout)
            .try_with_header(http::header::AUTHORIZATION, token.0.clone())
            .map_err(Failure::from)
    }

    pub async fn open_view(
        &self,
        request: state::OpenViewRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<state::OpenViewResponse, Failure> {
        let options = Self::options(token, timeout)?;
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.open_view_with_options(request, options) => {
                response.map(|value| value.into_owned()).map_err(Failure::from)
            }
        }
    }

    pub async fn read_page(
        &self,
        request: state::ReadPageRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<state::ReadPageResponse, Failure> {
        let options = Self::options(token, timeout)?;
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.read_page_with_options(request, options) => {
                response.map(|value| value.into_owned()).map_err(Failure::from)
            }
        }
    }

    pub async fn watch_view(
        &self,
        request: state::WatchViewRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<StateWatch, Failure> {
        let options = Self::options(token, timeout)?;
        tokio::select! {
            biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.watch_view_with_options(request, options) => {
                response.map(|inner| StateWatch { inner: Some(inner), cancel: cancel.child_token() }).map_err(Failure::from)
            }
        }
    }
}

type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    state::__buffa::view::WatchViewResponseView<'static>,
>;

pub struct StateWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}

impl StateWatch {
    pub async fn next(&mut self) -> Result<Option<state::WatchViewResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! {
            biased;
            () = self.cancel.cancelled() => Err(Failure::cancelled()),
            response = inner.message() => response.map(|value| value.map(|message| message.to_owned_message())).map_err(Failure::from),
        };
        if !matches!(result, Ok(Some(_))) {
            self.inner = None; // Drop the body immediately on cancellation, EOF or error.
        }
        result
    }
}

impl Drop for StateWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
