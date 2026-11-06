//! Session ownership and the secure OS persistence boundary, outside UI widgets.
#![deny(clippy::wildcard_enum_match_arm)]

mod api;
mod coordinator;
mod record;
mod state;
mod store;
pub use coordinator::{GenerationGuard, SessionCoordinator, SourceProof};
pub use record::{RecordFailure, RefreshState, SecretRecord};
pub use state::{
    AccessLease, Generation, Phase, ServerRevocation, SessionFailure, SessionReaction, SessionView,
};
pub use store::{CredentialKey, SecureStore, StoreFailure};

/// Dependency injection for synthetic cross-crate integration tests. This
/// feature is absent from application and Android production builds.
#[cfg(feature = "test-support")]
pub mod testing {
    pub use crate::{api::AuthApi, coordinator::Clock};

    pub fn coordinator(
        api: std::sync::Arc<dyn AuthApi>,
        store: std::sync::Arc<dyn crate::SecureStore>,
        clock: std::sync::Arc<dyn Clock>,
    ) -> crate::SessionCoordinator {
        crate::SessionCoordinator::with_parts(api, store, clock)
    }
}
