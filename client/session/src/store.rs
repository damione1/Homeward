use std::{error::Error, fmt};

use zeroize::Zeroizing;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StoreFailure {
    Unavailable,
    Corrupt,
    Unsupported,
    InUse,
}

impl fmt::Display for StoreFailure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{self:?}")
    }
}
impl Error for StoreFailure {}

/// A stable digest of the verified endpoint and trust identity, not a row index.
#[derive(Clone, Copy, PartialEq, Eq)]
pub struct CredentialKey([u8; 32]);

impl CredentialKey {
    pub fn from_digest(digest: [u8; 32]) -> Self {
        Self(digest)
    }
    pub fn identifier(&self) -> String {
        self.0.iter().map(|byte| format!("{byte:02x}")).collect()
    }
}

/// A successful save/clear is a durable OS commit. Call this blocking boundary
/// from owned background work. Production has no memory/plain-file fallback.
pub trait SecureStore: Send + Sync + 'static {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure>;
    fn save(&self, record: &[u8]) -> Result<(), StoreFailure>;
    fn clear(&self) -> Result<(), StoreFailure>;
}
