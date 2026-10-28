use std::{io::Cursor, sync::Arc, time::Duration};

use connectrpc::client::{ClientConfig, HttpClient};
use http::Uri;

use crate::{
    AccountTransport, ActionTransport, AuthTransport, CalendarTransport, CameraTransport,
    DashboardTransport, Failure, HistoryTransport, IntegrationTransport, ListTransport,
    McpKeyTransport, MediaTransport, ObservabilityTransport, PolicyTransport, StateTransport,
    services::home,
};

/// A shared verified TLS connection configuration. Service facades reuse the
/// same HTTP connection pool; no service changes the endpoint or trust roots.
#[derive(Clone)]
pub struct Connection {
    http: HttpClient,
    config: ClientConfig,
}

impl Connection {
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
        Ok(Self { http, config })
    }

    pub fn state(&self) -> StateTransport {
        StateTransport {
            client: home::state::v1::StateServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn auth(&self) -> AuthTransport {
        AuthTransport {
            client: home::auth::v1::AuthServiceClient::new(self.http.clone(), self.config.clone()),
        }
    }

    pub fn accounts(&self) -> AccountTransport {
        AccountTransport {
            client: home::auth::v1::AccountServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn policies(&self) -> PolicyTransport {
        PolicyTransport {
            client: home::auth::v1::AuthorizationPolicyServiceClient::new(
                self.http.clone(),
                // A valid policy source alone may occupy 1 MiB; leave bounded
                // space for its protobuf metadata without widening other APIs.
                self.config
                    .clone()
                    .with_default_max_message_size(2 * 1024 * 1024),
            ),
        }
    }

    pub fn mcp_keys(&self) -> McpKeyTransport {
        McpKeyTransport {
            client: home::auth::v1::McpKeyServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn integrations(&self) -> IntegrationTransport {
        IntegrationTransport {
            client: home::integrationadmin::v1::IntegrationServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn observability(&self) -> ObservabilityTransport {
        ObservabilityTransport {
            client: home::observability::v1::ObservabilityServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn dashboard(&self) -> DashboardTransport {
        DashboardTransport {
            client: home::dashboard::v1::DashboardServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }
    pub fn actions(&self) -> ActionTransport {
        ActionTransport {
            client: home::actions::v1::ActionServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }
    pub fn history(&self) -> HistoryTransport {
        HistoryTransport {
            client: home::history::v1::HistoryServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }
    pub fn media(&self) -> MediaTransport {
        MediaTransport {
            client: home::media::v1::MediaServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }
    pub fn calendars(&self) -> CalendarTransport {
        CalendarTransport {
            client: home::calendars::v1::CalendarServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }
    pub fn cameras(&self) -> CameraTransport {
        CameraTransport {
            client: home::cameras::v1::CameraServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }
    pub fn lists(&self) -> ListTransport {
        ListTransport {
            client: home::lists::v1::ListServiceClient::new(self.http.clone(), self.config.clone()),
        }
    }
}
