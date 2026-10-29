//! Shared native entry point and owned secure-session host.
#![deny(clippy::wildcard_enum_match_arm)]
slint::include_modules!();

/// Every product/probe window owns one initialized Kit validation engine.
pub(crate) fn create_window() -> Result<HomewardWindow, slint::PlatformError> {
    let window = HomewardWindow::new()?;
    slint_kit::init_form_validation!(crate, window);
    Ok(window)
}

pub mod administration;
mod administration_host;
mod cache_view;
mod calendar;
mod calendar_host;
mod camera;
mod camera_host;
mod companion_host;
pub mod configuration;
mod control_text;
mod dashboard;
mod dashboard_catalogue;
mod dashboard_focus;
mod dashboard_gestures;
mod dashboard_host;
mod dashboard_layout;
mod dashboard_presentation;
mod dashboard_selection;
mod dashboard_visibility;
mod device_view;
#[cfg(test)]
mod environment_probe;
mod environment_text;
mod environment_view;
pub mod history;
mod history_host;
mod invitation;
mod list_host;
mod list_state;
pub mod media;
mod media_host;
mod navigation;
mod notification_host;
mod preferences;
mod profile;
mod raster;
mod registration_store;
mod session_host;
mod video_host;

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
    let _ = home_client_platform::notifications::initialize(&app);
    let lifecycle = session_host::LifecycleBridge::new();
    let events = lifecycle.clone();
    slint::android::init_with_event_listener(app, move |event| {
        use slint::android::android_activity::{MainEvent, PollEvent};
        if matches!(event, PollEvent::Main(MainEvent::Destroy)) {
            home_client_platform::notifications::destroyed();
        }
        if matches!(
            event,
            PollEvent::Main(
                MainEvent::Pause
                    | MainEvent::Stop
                    | MainEvent::TerminateWindow { .. }
                    | MainEvent::Destroy
            )
        ) {
            events.set(session_host::Lifecycle::Suspended);
        } else if matches!(
            event,
            PollEvent::Main(MainEvent::Resume { .. } | MainEvent::InitWindow { .. })
        ) {
            events.set(session_host::Lifecycle::Foreground);
        }
    })
    .expect("initialize Android activity");
    session_host::run_with_lifecycle(session_host::NativeServices::Android(context), lifecycle)
        .expect("run Homeward native shell");
}
