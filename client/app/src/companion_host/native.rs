//! Homeward wire adapter; native collectors remain independent of its API.
use super::wire;
use home_client_platform::telemetry as os;
use home_wire_contract::companion_contract;
use std::time::{Duration, Instant, SystemTime};
use tokio_util::sync::CancellationToken;

pub(super) trait Source: Send + Sync {
    fn device(&self) -> Option<wire::DeviceDescription>;
    fn sample(
        &self,
        consent: &wire::Consent,
        cancel: &CancellationToken,
    ) -> Option<(wire::Snapshot, Duration)>;
}
pub(super) struct Native;

pub(super) fn platform(value: os::Platform) -> Option<wire::Platform> {
    match value {
        os::Platform::MacOs => Some(wire::Platform::MacOs),
        os::Platform::Android => Some(wire::Platform::Android),
        os::Platform::Other => None,
    }
}
fn feature(value: os::Feature) -> wire::Feature {
    match value {
        os::Feature::BatteryLevel => wire::Feature::BatteryLevel,
        os::Feature::BatteryState => wire::Feature::BatteryState,
        os::Feature::Charging => wire::Feature::Charging,
        os::Feature::Connectivity => wire::Feature::Connectivity,
        os::Feature::Activity => wire::Feature::Activity,
        os::Feature::Focus => wire::Feature::Focus,
        os::Feature::DoNotDisturb => wire::Feature::DoNotDisturb,
        os::Feature::Kiosk => wire::Feature::Kiosk,
        os::Feature::ScreenSaver => wire::Feature::ScreenSaver,
        os::Feature::Brightness => wire::Feature::Brightness,
        os::Feature::Volume => wire::Feature::Volume,
        os::Feature::Location => wire::Feature::Location,
        os::Feature::CameraMotion => wire::Feature::CameraMotion,
        os::Feature::Notifications => wire::Feature::Notifications,
    }
}
fn capability(value: os::Capability) -> wire::Capability {
    wire::Capability {
        feature: feature(value.feature).into(),
        availability: match value.availability {
            os::Availability::Available => wire::Availability::Available,
            os::Availability::Partial => wire::Availability::Partial,
            os::Availability::Unsupported => wire::Availability::Unsupported,
            os::Availability::PendingQualification => wire::Availability::PendingQualification,
        }
        .into(),
        permission: match value.permission {
            os::Permission::NotRequired => wire::Permission::NotRequired,
            os::Permission::NotRequested => wire::Permission::NotRequested,
            os::Permission::Granted => wire::Permission::Granted,
            os::Permission::Denied => wire::Permission::Denied,
            os::Permission::Restricted => wire::Permission::Restricted,
            os::Permission::Unsupported => wire::Permission::Unsupported,
        }
        .into(),
        source: match value.source {
            os::Source::MacOsPowerSources => wire::Source::MacOsPowerSources,
            os::Source::MacOsNetworkConfiguration => wire::Source::MacOsNetworkConfiguration,
            os::Source::NativeLifecycle => wire::Source::NativeLifecycle,
            os::Source::AndroidBatteryManager => wire::Source::AndroidBatteryManager,
            os::Source::AndroidConnectivityManager => wire::Source::AndroidConnectivityManager,
            os::Source::None => wire::Source::None,
        }
        .into(),
        ..Default::default()
    }
}
pub(super) fn capabilities(value: os::Platform) -> Vec<wire::Capability> {
    os::capabilities(value)
        .into_iter()
        .map(capability)
        .collect()
}
fn absent<T>(value: os::Observation<T>) -> Result<T, wire::NoData> {
    match value {
        os::Observation::Value(v) => Ok(v),
        os::Observation::Disabled => Err(wire::NoData::Disabled),
        os::Observation::Unsupported => Err(wire::NoData::Unsupported),
        os::Observation::NoHardware => Err(wire::NoData::NoHardware),
        os::Observation::Unavailable => Err(wire::NoData::Unavailable),
        os::Observation::PermissionDenied => Err(wire::NoData::PermissionDenied),
        os::Observation::Stale => Err(wire::NoData::Stale),
    }
}
pub(super) fn encode(value: os::Snapshot) -> Option<wire::Snapshot> {
    let at = value
        .captured_at
        .duration_since(SystemTime::UNIX_EPOCH)
        .ok()?;
    Some(wire::Snapshot {
        captured_at: buffa_types::google::protobuf::Timestamp {
            seconds: at.as_secs().try_into().ok()?,
            nanos: at.subsec_nanos().try_into().ok()?,
            ..Default::default()
        }
        .into(),
        battery: wire::BatteryObservation {
            observation: Some(match absent(value.battery) {
                Ok(b) => wire::battery_observation::Observation::Value(Box::new(wire::Battery {
                    percent: u32::from(b.percent.get()),
                    state: match b.state {
                        os::PowerState::Battery => wire::PowerState::Battery,
                        os::PowerState::Charging => wire::PowerState::Charging,
                        os::PowerState::ExternalPower => wire::PowerState::ExternalPower,
                        os::PowerState::Full => wire::PowerState::Full,
                    }
                    .into(),
                    ..Default::default()
                })),
                Err(n) => wire::battery_observation::Observation::NoData(n.into()),
            }),
            ..Default::default()
        }
        .into(),
        connectivity: wire::ConnectivityObservation {
            observation: Some(match absent(value.connectivity) {
                Ok(n) => wire::connectivity_observation::Observation::Value(
                    match n {
                        os::Connectivity::LocallyReachable => wire::Connectivity::LocallyReachable,
                        os::Connectivity::ConnectionRequired => {
                            wire::Connectivity::ConnectionRequired
                        }
                        os::Connectivity::NoLocalRoute => wire::Connectivity::NoLocalRoute,
                    }
                    .into(),
                ),
                Err(n) => wire::connectivity_observation::Observation::NoData(n.into()),
            }),
            ..Default::default()
        }
        .into(),
        activity: wire::ActivityObservation {
            observation: Some(match absent(value.activity) {
                Ok(os::AppActivity::Foreground) => wire::activity_observation::Observation::Value(
                    wire::AppActivity::Foreground.into(),
                ),
                Err(n) => wire::activity_observation::Observation::NoData(n.into()),
            }),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    })
}
struct Guarded<'a>(&'a CancellationToken);
impl os::Collector for Guarded<'_> {
    fn battery(&mut self) -> os::Observation<os::Battery> {
        if self.0.is_cancelled() {
            os::Observation::Stale
        } else {
            os::Collector::battery(&mut os::NativeCollector)
        }
    }
    fn connectivity(&mut self) -> os::Observation<os::Connectivity> {
        if self.0.is_cancelled() {
            os::Observation::Stale
        } else {
            os::Collector::connectivity(&mut os::NativeCollector)
        }
    }
}
impl Source for Native {
    fn device(&self) -> Option<wire::DeviceDescription> {
        let info = os::device_info()?;
        let value = wire::DeviceDescription {
            platform: platform(info.platform)?.into(),
            model: info.model,
            os_version: info.os_version,
            capabilities: capabilities(info.platform),
            ..Default::default()
        };
        companion_contract::valid_device(&value).then_some(value)
    }
    fn sample(
        &self,
        consent: &wire::Consent,
        cancel: &CancellationToken,
    ) -> Option<(wire::Snapshot, Duration)> {
        if cancel.is_cancelled() {
            return None;
        }
        let started = Instant::now();
        let snapshot = os::sample(
            &mut Guarded(cancel),
            os::Consent {
                battery: consent.battery,
                connectivity: consent.connectivity,
                activity: consent.activity,
            },
            os::Lifecycle::Foreground,
            SystemTime::now(),
        );
        let elapsed = started.elapsed();
        if cancel.is_cancelled() {
            return None;
        }
        let encoded = encode(snapshot)?;
        (companion_contract::valid_snapshot(&encoded)
            && companion_contract::snapshot_matches_consent(&encoded, consent))
        .then_some((encoded, elapsed))
    }
}
