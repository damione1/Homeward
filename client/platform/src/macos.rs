use home_client_session::{CredentialKey, SecureStore, StoreFailure};
use security_framework::os::macos::keychain::SecKeychain;
use security_framework::passwords;
use std::sync::Mutex;
use zeroize::Zeroizing;

pub fn app_data_directory() -> Result<std::path::PathBuf, StoreFailure> {
    let home = std::env::var_os("HOME").ok_or(StoreFailure::Unavailable)?;
    Ok(std::path::PathBuf::from(home).join("Library/Application Support/Homeward"))
}

const SERVICE: &str = "ca.homeward.session.v1";
const NOT_FOUND: i32 = -25300;
static KEYCHAIN_CALL: Mutex<()> = Mutex::new(());

fn without_prompt<T>(
    operation: impl FnOnce() -> Result<T, StoreFailure>,
) -> Result<T, StoreFailure> {
    let _serial = KEYCHAIN_CALL
        .lock()
        .map_err(|_| StoreFailure::Unavailable)?;
    let allowed = SecKeychain::user_interaction_allowed().map_err(|_| StoreFailure::Unavailable)?;
    let _guard = if allowed {
        Some(SecKeychain::disable_user_interaction().map_err(|_| StoreFailure::Unavailable)?)
    } else {
        None
    };
    operation()
}

pub struct NativeStore {
    account: String,
    _lease: std::fs::File,
}

impl NativeStore {
    pub fn new(key: CredentialKey) -> Result<Self, StoreFailure> {
        Ok(Self {
            account: key.identifier(),
            _lease: crate::lease::acquire(&app_data_directory()?, key)?,
        })
    }
}

impl SecureStore for NativeStore {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        without_prompt(
            || match passwords::get_generic_password(SERVICE, &self.account) {
                Ok(bytes) if bytes.len() <= 32 * 1024 => Ok(Some(Zeroizing::new(bytes))),
                Ok(bytes) => {
                    let _wipe = Zeroizing::new(bytes);
                    Err(StoreFailure::Corrupt)
                }
                Err(error) if error.code() == NOT_FOUND => Ok(None),
                Err(_) => Err(StoreFailure::Unavailable),
            },
        )
    }

    fn save(&self, record: &[u8]) -> Result<(), StoreFailure> {
        if record.len() > 32 * 1024 {
            return Err(StoreFailure::Corrupt);
        }
        without_prompt(|| {
            passwords::set_generic_password(SERVICE, &self.account, record)
                .map_err(|_| StoreFailure::Unavailable)
        })
    }

    fn clear(&self) -> Result<(), StoreFailure> {
        without_prompt(
            || match passwords::delete_generic_password(SERVICE, &self.account) {
                Ok(()) => Ok(()),
                Err(error) if error.code() == NOT_FOUND => Ok(()),
                Err(_) => Err(StoreFailure::Unavailable),
            },
        )
    }
}
