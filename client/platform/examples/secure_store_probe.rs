//! Explicit synthetic OS-service probe; its key is a fresh namespace allocated
//! by the harness. Never reads or replaces an existing credential.
#![deny(clippy::wildcard_enum_match_arm)]

#[cfg(target_os = "macos")]
#[path = "support/store_phase.rs"]
mod store_phase;

#[cfg(target_os = "macos")]
fn main() -> Result<(), Box<dyn std::error::Error>> {
    use home_client_platform::NativeStore;
    use home_client_session::{CredentialKey, SecureStore};
    use store_phase::StorePhase;
    const VALUE: &[u8] = b"NHF003 synthetic OS-store record";
    let args: Vec<_> = std::env::args().collect();
    if args.len() != 4
        || args[1] != "--isolated-store-proof"
        || args[3].len() != 64
        || !args[3].bytes().all(|byte| byte.is_ascii_hexdigit())
    {
        return Err("explicit phase and fresh 64-hex namespace required".into());
    }
    let mut key = [0_u8; 32];
    let phase: StorePhase = args[2].parse()?;
    for (index, byte) in key.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&args[3][index * 2..index * 2 + 2], 16)?;
    }
    let constructed = NativeStore::new(CredentialKey::from_digest(key));
    match phase {
        StorePhase::Blocked => {
            if !matches!(constructed, Err(home_client_session::StoreFailure::InUse)) {
                return Err("second process was not refused".into());
            }
            println!("NHF003_KEYCHAIN_BLOCKED PASS");
            return Ok(());
        }
        StorePhase::Write
        | StorePhase::Read
        | StorePhase::Clear
        | StorePhase::Absent
        | StorePhase::Hold => {}
    }
    let store = constructed?;
    match phase {
        StorePhase::Write => {
            if store.load()?.is_some() {
                return Err("preserve existing credential".into());
            }
            store.save(VALUE)?;
        }
        StorePhase::Read | StorePhase::Clear => {
            let value = store.load()?.ok_or("synthetic record missing")?;
            if value.as_slice() != VALUE {
                return Err("preserve unexpected credential".into());
            }
            if phase == StorePhase::Clear {
                store.clear()?;
            }
        }
        StorePhase::Absent => {
            if store.load()?.is_some() {
                return Err("credential survived deletion".into());
            }
        }
        StorePhase::Hold => {
            use std::io::{Read as _, Write as _};
            if store.load()?.is_some() {
                return Err("preserve existing credential".into());
            }
            println!("NHF003_KEYCHAIN_HELD");
            std::io::stdout().flush()?;
            std::io::stdin().read_exact(&mut [0_u8; 1])?;
        }
        StorePhase::Blocked => return Err("blocked phase unexpectedly constructed a store".into()),
    }
    let label = serde_json::to_value(phase)?;
    println!(
        "NHF003_KEYCHAIN_{} PASS",
        label
            .as_str()
            .ok_or("invalid phase encoding")?
            .to_uppercase()
    );
    Ok(())
}

#[cfg(not(target_os = "macos"))]
fn main() {
    panic!("Use an actual Android APK context for the Keystore proof.");
}
