//! Native asynchronous transport over the existing Buffa wire types.
#![deny(clippy::wildcard_enum_match_arm)]

mod client;
mod failure;
mod reconnect;
pub use client::{AccessToken, StateTransport, StateWatch};
pub use failure::{DetailFailure, Failure, FailureKind};
pub use reconnect::ReconnectPolicy;

#[allow(clippy::all)]
#[rustfmt::skip]
pub mod services {
    pub mod home {
        pub mod state {
            pub mod v1 { include!("generated/home.state.v1.rs"); }
        }
    }
}
