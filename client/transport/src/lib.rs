//! Native asynchronous transport over the existing Buffa wire types.
#![deny(clippy::wildcard_enum_match_arm)]

mod administration;
mod auth;
mod calendars;
mod credential;
mod entity_views;
pub use entity_views::{EntityViewTransport, EntityViewWatch};
#[cfg(feature = "private-qualification")]
mod companion;
mod configuration;
mod connection;
mod dashboard;
mod failure;
mod history;
mod home;
#[cfg(feature = "private-qualification")]
mod logic;
mod media;
#[cfg(feature = "private-qualification")]
mod notifications;
mod operations;
mod reconnect;
mod tasks;
pub use administration::{
    AccountTransport, IntegrationTransport, McpKeyTransport, ObservabilityTransport,
    PolicyTransport,
};
pub use auth::AuthTransport;
pub use calendars::{CalendarTransport, CalendarWatch};
#[cfg(feature = "private-qualification")]
pub use companion::CompanionTransport;
pub use configuration::{
    ConfigurationApplyTransport, ConfigurationPlanTransport, ConfigurationSimulationTransport,
    ConfigurationTransport, ConfigurationWatch,
};
pub use connection::Connection;
#[cfg(target_os = "android")]
pub use connection::initialize_android_trust;
pub use connectrpc::ErrorCode as RpcCode;
pub use credential::AccessToken;
pub use dashboard::{DashboardTransport, DashboardWatch};
pub use failure::{Failure, FailureKind};
pub use history::HistoryTransport;
pub use home::HomeTransport;
pub use home_wire_contract::api_errors::{ApiFailure, InvalidApiError};
#[cfg(feature = "private-qualification")]
pub use logic::LogicTransport;
#[cfg(feature = "private-qualification")]
pub use notifications::NotificationTransport;
pub use operations::OperationsTransport;
pub use reconnect::{FibonacciBackoff, ReconnectPolicy};
pub use tasks::{TaskTransport, TaskWatch};

#[allow(clippy::all)]
#[rustfmt::skip]
pub mod services {
    pub mod google { pub mod longrunning { include!("generated/google.longrunning.rs"); } }
    pub mod home {
        pub mod api { pub mod v2 { include!("generated/home.api.v2.rs"); } }
        #[cfg(feature = "private-qualification")]
        pub mod testing {
            pub mod notifications { pub mod v1 { include!("generated/home.testing.notifications.v1.rs"); } }
            pub mod companion { pub mod v1 { include!("generated/home.testing.companion.v1.rs"); } }
            pub mod logic { pub mod v1 { include!("generated/home.testing.logic.v1.rs"); } }
        }
    }
}

mod capabilities;
pub use capabilities::CapabilityTransport;

pub use media::{MediaSessionWatch, MediaTransport, MediaVideoWatch};
