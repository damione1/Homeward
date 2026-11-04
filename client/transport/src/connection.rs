use std::{io::Cursor, sync::Arc, time::Duration};

use connectrpc::client::{ClientConfig, HttpClient};
use http::Uri;
use rustls_platform_verifier::BuilderVerifierExt;

#[cfg(feature = "private-qualification")]
use crate::{CompanionTransport, LogicTransport, NotificationTransport};
use crate::{HomeTransport, OperationsTransport};

use crate::{
    AccountTransport, AuthTransport, CalendarTransport,
    ConfigurationApplyTransport, ConfigurationPlanTransport, ConfigurationSimulationTransport,
    ConfigurationTransport, DashboardTransport, Failure, HistoryTransport, IntegrationTransport,
    McpKeyTransport, MediaTransport, ObservabilityTransport,
    PolicyTransport, TaskTransport, services::home,
};

/// A shared verified TLS connection configuration. Service facades reuse the
/// same HTTP connection pool; no service changes the endpoint or trust roots.
#[derive(Clone)]
pub struct Connection {
    http: HttpClient,
    config: ClientConfig,
}

impl Connection {
    pub fn entity_views(&self) -> crate::EntityViewTransport {
        crate::EntityViewTransport {
            client: home::api::v2::EntityViewServiceClient::new(
                self.http.clone(),
                self.config
                    .clone()
                    .with_default_max_message_size(16 * 1024 * 1024),
            ),
        }
    }

    pub fn capabilities(&self) -> crate::CapabilityTransport {
        crate::CapabilityTransport {
            client: home::api::v2::CapabilityServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn operations(&self) -> OperationsTransport {
        OperationsTransport {
            client: crate::services::google::longrunning::OperationsClient::new(
                self.http.clone(),
                self.config
                    .clone()
                    .with_default_max_message_size(2 * 1024 * 1024),
            ),
        }
    }

    pub fn home(&self) -> HomeTransport {
        HomeTransport {
            client: home::api::v2::HomeServiceClient::new(self.http.clone(), self.config.clone()),
        }
    }

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
        let builder = rustls::ClientConfig::builder_with_provider(Arc::new(
            rustls::crypto::aws_lc_rs::default_provider(),
        ))
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(|_| Failure::invalid_configuration())?;
        let tls = if trusted_ca_pem.is_empty() {
            #[cfg(target_os = "android")]
            if !ANDROID_TRUST_READY.load(std::sync::atomic::Ordering::Acquire) {
                return Err(Failure::invalid_configuration());
            }
            builder
                .with_platform_verifier()
                .map_err(|_| Failure::invalid_configuration())?
                .with_no_client_auth()
        } else {
            // An explicit private CA remains an isolated trust set. Invalid
            // supplied material never falls back to the device trust store.
            let mut roots = rustls::RootCertStore::empty();
            for certificate in rustls_pemfile::certs(&mut Cursor::new(trusted_ca_pem)) {
                roots
                    .add(certificate.map_err(|_| Failure::invalid_configuration())?)
                    .map_err(|_| Failure::invalid_configuration())?;
            }
            if roots.is_empty() {
                return Err(Failure::invalid_configuration());
            }
            builder.with_root_certificates(roots).with_no_client_auth()
        };
        let http = HttpClient::builder()
            .tcp_connect_timeout(Duration::from_secs(3))
            .establishment_timeout(Duration::from_secs(5))
            .with_tls(Arc::new(tls));
        let config = ClientConfig::new(uri).with_default_max_message_size(1024 * 1024);
        Ok(Self { http, config })
    }

    pub fn auth(&self) -> AuthTransport {
        AuthTransport {
            client: home::api::v2::AuthServiceClient::new(self.http.clone(), self.config.clone()),
        }
    }

    pub fn accounts(&self) -> AccountTransport {
        AccountTransport {
            client: home::api::v2::AccountServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn policies(&self) -> PolicyTransport {
        PolicyTransport {
            client: home::api::v2::AuthorizationPolicyServiceClient::new(
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
            client: home::api::v2::McpKeyServiceClient::new(self.http.clone(), self.config.clone()),
        }
    }

    pub fn integrations(&self) -> IntegrationTransport {
        IntegrationTransport {
            client: home::api::v2::IntegrationServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn observability(&self) -> ObservabilityTransport {
        ObservabilityTransport {
            client: home::api::v2::ObservabilityServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    #[cfg(feature = "private-qualification")]
    pub fn notifications(&self) -> NotificationTransport {
        NotificationTransport {
            client: home::testing::notifications::v1::NotificationServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    #[cfg(feature = "private-qualification")]
    pub fn companion(&self) -> CompanionTransport {
        CompanionTransport {
            client: home::testing::companion::v1::CompanionServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    #[cfg(feature = "private-qualification")]
    pub fn logic(&self) -> LogicTransport {
        LogicTransport {
            client: home::testing::logic::v1::LogicServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    // Exact user-provided source may occupy 1 MiB before protobuf metadata.
    // Keep this larger envelope specific to the four configuration services.
    pub fn configuration(&self) -> ConfigurationTransport {
        ConfigurationTransport {
            client: home::api::v2::ConfigurationServiceClient::new(
                self.http.clone(),
                self.config
                    .clone()
                    .with_default_max_message_size(2 * 1024 * 1024),
            ),
        }
    }

    pub fn configuration_plans(&self) -> ConfigurationPlanTransport {
        ConfigurationPlanTransport {
            client: home::api::v2::ConfigurationPlanServiceClient::new(
                self.http.clone(),
                self.config
                    .clone()
                    .with_default_max_message_size(2 * 1024 * 1024),
            ),
        }
    }

    pub fn configuration_simulation(&self) -> ConfigurationSimulationTransport {
        ConfigurationSimulationTransport {
            client: home::api::v2::ConfigurationSimulationServiceClient::new(
                self.http.clone(),
                self.config
                    .clone()
                    .with_default_max_message_size(2 * 1024 * 1024),
            ),
        }
    }

    pub fn configuration_apply(&self) -> ConfigurationApplyTransport {
        ConfigurationApplyTransport {
            client: home::api::v2::ConfigurationApplyServiceClient::new(
                self.http.clone(),
                self.config
                    .clone()
                    .with_default_max_message_size(2 * 1024 * 1024),
            ),
        }
    }

    pub fn dashboard(&self) -> DashboardTransport {
        DashboardTransport {
            homes: self.home(),
            client: home::api::v2::DashboardServiceClient::new(
                self.http.clone(),
                self.config
                    .clone()
                    .with_default_max_message_size(2 * 1024 * 1024),
            ),
        }
    }
    pub fn history(&self) -> HistoryTransport {
        HistoryTransport {
            client: home::api::v2::HistoryServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }
    pub fn media(&self) -> MediaTransport {
        MediaTransport {
            homes: self.home(),
            client: home::api::v2::MediaServiceClient::new(self.http.clone(), self.config.clone()),
        }
    }
    pub fn calendars(&self) -> CalendarTransport {
        CalendarTransport {
            homes: self.home(),
            client: home::api::v2::CalendarServiceClient::new(
                self.http.clone(),
                self.config.clone(),
            ),
        }
    }

    pub fn tasks(&self) -> TaskTransport {
        TaskTransport {
            homes: self.home(),
            client: home::api::v2::TaskServiceClient::new(self.http.clone(), self.config.clone()),
        }
    }
}

#[cfg(target_os = "android")]
static ANDROID_TRUST_READY: std::sync::atomic::AtomicBool =
    std::sync::atomic::AtomicBool::new(false);

/// Initialize Android's native certificate verifier before starting networking.
///
/// # Safety
/// Both pointers must be live references owned by the calling AndroidApp.
#[cfg(target_os = "android")]
pub unsafe fn initialize_android_trust(
    vm: *mut std::ffi::c_void,
    activity: *mut std::ffi::c_void,
) -> Result<(), Failure> {
    if vm.is_null() || activity.is_null() {
        return Err(Failure::invalid_configuration());
    }
    let vm = unsafe { jni::JavaVM::from_raw(vm.cast()) };
    vm.attach_current_thread(|env| {
        // AndroidApp lends a global reference. Borrow it without ownership,
        // then create the local reference expected by the verifier initializer.
        let raw: jni::sys::jobject = activity.cast();
        let borrowed =
            unsafe { env.as_cast_raw::<jni::objects::Global<jni::objects::JObject>>(&raw)? };
        let local = env.new_local_ref(borrowed)?;
        rustls_platform_verifier::android::init_with_env(env, local)
    })
    .map_err(|_| Failure::invalid_configuration())?;
    ANDROID_TRUST_READY.store(true, std::sync::atomic::Ordering::Release);
    Ok(())
}
