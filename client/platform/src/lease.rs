use home_client_session::{CredentialKey, StoreFailure};
use std::{
    fs::{DirBuilder, File, OpenOptions, TryLockError},
    os::unix::fs::{DirBuilderExt, OpenOptionsExt},
    path::Path,
};

/// Non-secret process coordination only; credentials remain in the OS store.
/// Retain the file: unlinking a live lock would allow a second inode/owner.
pub(crate) fn acquire(directory: &Path, key: CredentialKey) -> Result<File, StoreFailure> {
    let locks = directory.join("session-locks");
    DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&locks)
        .map_err(|_| StoreFailure::Unavailable)?;
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(locks.join(key.identifier() + ".lock"))
        .map_err(|_| StoreFailure::Unavailable)?;
    file.try_lock().map_err(|error| match error {
        TryLockError::WouldBlock => StoreFailure::InUse,
        TryLockError::Error(_) => StoreFailure::Unavailable,
    })?;
    Ok(file)
}
