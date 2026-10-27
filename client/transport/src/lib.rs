//! Native asynchronous transport over the existing Buffa wire types.
#![deny(clippy::wildcard_enum_match_arm)]

mod actions;
mod administration;
mod auth;
mod calendars;
mod cameras;
mod client;
mod connection;
mod dashboard;
mod failure;
mod history;
mod lists;
mod media;
mod reconnect;
pub use actions::{ActionTransport, ActionWatch};
pub use administration::{
    AccountTransport, IntegrationTransport, McpKeyTransport, ObservabilityTransport,
    PolicyTransport,
};
pub use auth::AuthTransport;
pub use calendars::{CalendarTransport, CalendarWatch};
pub use cameras::{CameraTransport, CameraWatch};
pub use client::{AccessToken, StateTransport, StateWatch};
pub use connection::Connection;
pub use connectrpc::ErrorCode as RpcCode;
pub use dashboard::{DashboardTransport, DashboardWatch};
pub use failure::{DetailFailure, Failure, FailureKind};
pub use history::HistoryTransport;
pub use lists::{ListTransport, ListWatch};
pub use media::MediaTransport;
pub use reconnect::ReconnectPolicy;

#[allow(clippy::all)]
#[rustfmt::skip]
pub mod services {
    pub mod home {
        pub mod cameras {
            pub mod v1 { include!("generated/home.cameras.v1.rs"); }
        }
        pub mod calendars {
            pub mod v1 { include!("generated/home.calendars.v1.rs"); }
        }
        pub mod media {
            pub mod v1 { include!("generated/home.media.v1.rs"); }
        }
        pub mod actions {
            pub mod v1 { include!("generated/home.actions.v1.rs"); }
        }
        pub mod auth {
            pub mod v1 { include!("generated/home.auth.v1.rs"); }
        }
        pub mod integrationadmin {
            pub mod v1 { include!("generated/home.integrationadmin.v1.rs"); }
        }
        pub mod observability {
            pub mod v1 { include!("generated/home.observability.v1.rs"); }
        }
        pub mod state {
            pub mod v1 { include!("generated/home.state.v1.rs"); }
        }
        pub mod dashboard {
            pub mod v1 { include!("generated/home.dashboard.v1.rs"); }
        }
        pub mod history {
            pub mod v1 { include!("generated/home.history.v1.rs"); }
        }
        pub mod lists {
            pub mod v1 { include!("generated/home.lists.v1.rs"); }
        }
    }
}
