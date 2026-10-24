//! Session ownership and the secure OS persistence boundary, outside UI widgets.
#![deny(clippy::wildcard_enum_match_arm)]

mod api;
mod coordinator;
mod record;
mod state;
mod store;
pub use coordinator::SessionCoordinator;
pub use record::{RecordFailure, RefreshState, SecretRecord};
pub use state::{
    AccessLease, Generation, Phase, ServerRevocation, SessionFailure, SessionReaction, SessionView,
};
pub use store::{CredentialKey, SecureStore, StoreFailure};
