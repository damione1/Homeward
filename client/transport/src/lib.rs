//! Native asynchronous transport over the existing Buffa wire types.
#![deny(clippy::wildcard_enum_match_arm)]

mod actions;
mod auth;
mod client;
mod connection;
mod dashboard;
mod failure;
mod reconnect;
pub use actions::{ActionTransport, ActionWatch};
pub use auth::AuthTransport;
pub use client::{AccessToken, StateTransport, StateWatch};
pub use connection::Connection;
pub use connectrpc::ErrorCode as RpcCode;
pub use dashboard::{DashboardTransport, DashboardWatch};
pub use failure::{DetailFailure, Failure, FailureKind};
pub use reconnect::ReconnectPolicy;

#[allow(clippy::all)]
#[rustfmt::skip]
pub mod services {
    pub mod home {
        pub mod actions {
            pub mod v1 { include!("generated/home.actions.v1.rs"); }
        }
        pub mod auth {
            pub mod v1 { include!("generated/home.auth.v1.rs"); }
        }
        pub mod state {
            pub mod v1 { include!("generated/home.state.v1.rs"); }
        }
        pub mod dashboard {
            pub mod v1 { include!("generated/home.dashboard.v1.rs"); }
        }
    }
}
