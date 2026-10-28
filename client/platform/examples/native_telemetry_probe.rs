//! Explicit opt-in, read-only native collector qualification. No network client
//! or installation identity. Run under the shared frontend build slot.
use home_client_platform::telemetry::{
    self, Cadence, Consent, Lifecycle, NativeCollector, Platform,
};
use serde_json::json;
use std::time::{Instant, SystemTime};

fn main() {
    assert_eq!(
        std::env::args().skip(1).collect::<Vec<_>>(),
        ["--collect-once"],
        "explicit --collect-once required"
    );
    let started = Instant::now();
    let snapshot = telemetry::sample(
        &mut NativeCollector,
        Consent {
            battery: true,
            connectivity: true,
            activity: true,
        },
        Lifecycle::Foreground,
        SystemTime::now(),
    );
    let micros = started.elapsed().as_micros();
    let platform = if cfg!(target_os = "macos") {
        Platform::MacOs
    } else if cfg!(target_os = "android") {
        Platform::Android
    } else {
        Platform::Other
    };
    println!(
        "{}",
        json!({
            "platform":format!("{platform:?}"),
            "battery":format!("{:?}",snapshot.battery),
            "connectivity":format!("{:?}",snapshot.connectivity),
            "activity":format!("{:?}",snapshot.activity),
            "sample_microseconds":micros,
            "default_cadence_seconds":Cadence::default().duration().as_secs(),
            "capabilities":telemetry::capabilities(platform).iter().map(|c|json!({"feature":format!("{:?}",c.feature),"availability":format!("{:?}",c.availability),"permission":format!("{:?}",c.permission),"source":format!("{:?}",c.source)})).collect::<Vec<_>>(),
            "physical_actions":false,"network_publication":false,"location_camera_collection":false,
            "scope":"Actual local OS observations; foreground lifecycle supplied by this CLI, not an OS sleep/resume qualification. One sample timing is not a battery drain measurement."
        })
    );
}
