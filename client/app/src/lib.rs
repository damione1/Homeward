//! Shared native entry point. Network and session ownership arrive in NHF-002/003.
#![deny(clippy::wildcard_enum_match_arm)]
slint::include_modules!();

// Keep the two existing domain/codec crates at the shared application boundary.
pub use home_client_model as model;
pub use home_wire_contract as wire;
pub use slint_kit_pro as kit_pro;

pub fn run() -> Result<(), slint::PlatformError> {
    let window = HomewardWindow::new()?;
    let weak = window.as_weak();
    window.on_inspect_foundation(move || {
        if let Some(window) = weak.upgrade() {
            window.set_foundation_status("Les deux kits sont chargés.".into());
        }
    });
    window.run()
}

#[cfg(target_os = "android")]
#[unsafe(no_mangle)]
fn android_main(app: slint::android::AndroidApp) {
    slint::android::init(app).expect("initialize Android activity");
    run().expect("run Homeward native shell");
}
