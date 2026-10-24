//! Native secure services. No production plaintext persistence fallback.
#![deny(clippy::wildcard_enum_match_arm)]
#[cfg(any(target_os = "macos", target_os = "android"))]
mod lease;

#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "macos")]
pub use macos::{NativeStore, app_data_directory};

#[cfg(target_os = "android")]
mod android;
#[cfg(target_os = "android")]
pub use android::{NativeStore, NativeStoreContext};
