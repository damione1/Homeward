//! Client action identity and progress, independent of widgets and observed
//! values. No persistence, offline queue or automatic command resubmission.
#![deny(clippy::wildcard_enum_match_arm)]

/// Minted by the cache/UI adapter from the exact identity, capability, write
/// permission and model generation captured by the gesture. Called again after
/// waiting for session access, immediately before consuming admission.
pub type AdmissionCheck = std::sync::Arc<dyn Fn() -> bool + Send + Sync>;

mod access;
pub mod calendars;
pub mod lists;
pub use access::{WriteAccess, write_access};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum IdentityFailure {
    Invalid,
    RandomUnavailable,
}

pub mod capabilities;
mod request_id;
pub use request_id::new_request_id;
