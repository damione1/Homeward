use home_client_transport::{AccessToken, FailureKind};
use home_wire_contract::auth_contract::{AccountId, SessionId};
use tokio_util::sync::CancellationToken;

use crate::StoreFailure;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Generation(pub(crate) u64);
impl Generation {
    pub fn value(self) -> u64 {
        self.0
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Phase {
    SignedOut,
    Busy,
    Challenge { recovery: bool, fido2: bool },
    EnrollmentRequired,
    Ready,
    Refreshing,
    NeedsLogin,
    CredentialRejected,
    RateLimited,
    NetworkUnavailable,
    AuthorityUnavailable,
    RightsChanged,
    StorageUnavailable,
    UnsupportedResponse,
    Closed,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SessionView {
    pub generation: Generation,
    pub phase: Phase,
    pub failure: Option<SessionFailure>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SessionFailure {
    Cancelled,
    NeedsLogin,
    Busy,
    UnsupportedFactor,
    InvalidResponse,
    Clock,
    Store(StoreFailure),
    Transport(FailureKind),
    Closed,
    RefreshUncertain(FailureKind),
    AuthorityUnavailable,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SessionReaction {
    Unchanged,
    IgnoredOldGeneration,
    ViewSuspended,
    ViewReset,
    CredentialsDiscarded,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ServerRevocation {
    Acknowledged,
    Uncertain,
    NotRequired,
}

impl std::fmt::Display for SessionFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for SessionFailure {}

/// The cache must discard a late result whose generation is no longer current.
/// This credential lease deliberately has no Debug implementation.
pub struct AccessLease {
    pub generation: Generation,
    pub cancellation: CancellationToken,
    pub(crate) token: AccessToken,
    pub(crate) account: AccountId,
    pub(crate) session: SessionId,
}
impl AccessLease {
    pub fn token(&self) -> &AccessToken {
        &self.token
    }

    /// Identity admitted from the same session record as this credential.
    /// It is metadata, never a local authorization decision.
    pub fn account_id(&self) -> &AccountId {
        &self.account
    }

    pub fn session_id(&self) -> &SessionId {
        &self.session
    }
}
