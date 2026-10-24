//! Shared native entry point and owned secure-session host.
#![deny(clippy::wildcard_enum_match_arm)]
slint::include_modules!();

mod profile;
mod session_host;

// Keep the two existing domain/codec crates at the shared application boundary.
pub use home_client_model as model;
pub use home_wire_contract as wire;
pub use slint_kit_pro as kit_pro;

pub fn run() -> Result<(), slint::PlatformError> {
    #[cfg(target_os = "macos")]
    return session_host::run(session_host::NativeServices::MacOs);
    #[cfg(target_os = "android")]
    return session_host::run(session_host::NativeServices::Android(Err(
        home_client_session::StoreFailure::Unavailable,
    )));
    #[cfg(not(any(target_os = "macos", target_os = "android")))]
    return session_host::run(session_host::NativeServices::Unsupported);
}

#[cfg(target_os = "android")]
#[unsafe(no_mangle)]
fn android_main(app: slint::android::AndroidApp) {
    let context = home_client_platform::NativeStoreContext::new(&app);
    slint::android::init(app).expect("initialize Android activity");
    session_host::run(session_host::NativeServices::Android(context))
        .expect("run Homeward native shell");
}
