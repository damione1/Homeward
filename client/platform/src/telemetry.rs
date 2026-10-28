//! Opt-in native observations. Availability is independent of consent and value;
//! absence, suspension and API failure never turn into a physical zero/off/away.
use std::time::{Duration, SystemTime};

#[cfg(target_os = "macos")]
mod macos;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Feature {
    BatteryLevel,
    BatteryState,
    Charging,
    Connectivity,
    Activity,
    Focus,
    DoNotDisturb,
    Kiosk,
    ScreenSaver,
    Brightness,
    Volume,
    Location,
    CameraMotion,
    Notifications,
}
impl Feature {
    pub const ALL: [Self; 14] = [
        Self::BatteryLevel,
        Self::BatteryState,
        Self::Charging,
        Self::Connectivity,
        Self::Activity,
        Self::Focus,
        Self::DoNotDisturb,
        Self::Kiosk,
        Self::ScreenSaver,
        Self::Brightness,
        Self::Volume,
        Self::Location,
        Self::CameraMotion,
        Self::Notifications,
    ];

    pub const fn label(self) -> &'static str {
        match self {
            Self::BatteryLevel => "Niveau de batterie",
            Self::BatteryState => "État de batterie",
            Self::Charging => "Charge en cours",
            Self::Connectivity => "Connectivité locale",
            Self::Activity => "Activité de l’application",
            Self::Focus => "Focus système",
            Self::DoNotDisturb => "Ne pas déranger",
            Self::Kiosk => "Kiosk",
            Self::ScreenSaver => "Économiseur d’écran",
            Self::Brightness => "Luminosité de l’écran",
            Self::Volume => "Volume système",
            Self::Location => "Localisation",
            Self::CameraMotion => "Mouvement caméra",
            Self::Notifications => "Collecte des notifications OS",
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Availability {
    Available,
    Partial,
    Unsupported,
    PendingQualification,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Permission {
    NotRequired,
    NotRequested,
    Granted,
    Denied,
    Restricted,
    Unsupported,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Source {
    MacOsPowerSources,
    MacOsNetworkConfiguration,
    NativeLifecycle,
    None,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Platform {
    MacOs,
    Android,
    Other,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Capability {
    pub feature: Feature,
    pub availability: Availability,
    pub permission: Permission,
    pub source: Source,
}

pub fn capabilities(platform: Platform) -> [Capability; 14] {
    Feature::ALL.map(|feature| {
        let (availability, permission, source) = match (platform, feature) {
            (
                Platform::MacOs,
                Feature::BatteryLevel | Feature::BatteryState | Feature::Charging,
            ) => (
                Availability::Available,
                Permission::NotRequired,
                Source::MacOsPowerSources,
            ),
            (Platform::MacOs, Feature::Connectivity) => (
                Availability::Partial,
                Permission::NotRequired,
                Source::MacOsNetworkConfiguration,
            ),
            (Platform::MacOs, Feature::Activity) => (
                Availability::Partial,
                Permission::NotRequired,
                Source::NativeLifecycle,
            ),
            (
                Platform::Android,
                Feature::BatteryLevel
                | Feature::BatteryState
                | Feature::Charging
                | Feature::Connectivity
                | Feature::Activity
                | Feature::DoNotDisturb
                | Feature::Kiosk
                | Feature::ScreenSaver
                | Feature::Brightness
                | Feature::Volume
                | Feature::Location
                | Feature::CameraMotion
                | Feature::Notifications,
            ) => (
                Availability::PendingQualification,
                Permission::NotRequested,
                Source::None,
            ),
            (
                Platform::MacOs,
                Feature::Focus
                | Feature::DoNotDisturb
                | Feature::Kiosk
                | Feature::ScreenSaver
                | Feature::Brightness
                | Feature::Volume
                | Feature::Location
                | Feature::CameraMotion
                | Feature::Notifications,
            )
            | (Platform::Android, Feature::Focus)
            | (
                Platform::Other,
                Feature::BatteryLevel
                | Feature::BatteryState
                | Feature::Charging
                | Feature::Connectivity
                | Feature::Activity
                | Feature::Focus
                | Feature::DoNotDisturb
                | Feature::Kiosk
                | Feature::ScreenSaver
                | Feature::Brightness
                | Feature::Volume
                | Feature::Location
                | Feature::CameraMotion
                | Feature::Notifications,
            ) => (
                Availability::Unsupported,
                Permission::Unsupported,
                Source::None,
            ),
        };
        Capability {
            feature,
            availability,
            permission,
            source,
        }
    })
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Consent {
    pub battery: bool,
    pub connectivity: bool,
    /// App foreground visibility only; never geographic or human presence.
    pub activity: bool,
}
// Location/camera are intentionally absent: no consent can enable an
// unimplemented or unqualified collection path.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Cadence {
    Seconds30,
    #[default]
    Minute1,
    Minutes5,
}
impl Cadence {
    pub const fn duration(self) -> Duration {
        Duration::from_secs(match self {
            Self::Seconds30 => 30,
            Self::Minute1 => 60,
            Self::Minutes5 => 300,
        })
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Lifecycle {
    Foreground,
    Suspended,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Observation<T> {
    Disabled,
    Unsupported,
    NoHardware,
    Unavailable,
    PermissionDenied,
    Stale,
    Value(T),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct BatteryPercent(u8);
impl BatteryPercent {
    pub fn from_capacity(current: i64, maximum: i64) -> Option<Self> {
        if maximum <= 0 || current < 0 || current > maximum {
            return None;
        }
        let percent = (i128::from(current) * 100 + i128::from(maximum) / 2) / i128::from(maximum);
        Some(Self(u8::try_from(percent).ok()?))
    }
    pub fn get(self) -> u8 {
        self.0
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PowerState {
    Battery,
    Charging,
    ExternalPower,
    Full,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Battery {
    pub percent: BatteryPercent,
    pub state: PowerState,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Connectivity {
    LocallyReachable,
    ConnectionRequired,
    NoLocalRoute,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AppActivity {
    Foreground,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Snapshot {
    pub captured_at: SystemTime,
    pub battery: Observation<Battery>,
    pub connectivity: Observation<Connectivity>,
    pub activity: Observation<AppActivity>,
}
impl Snapshot {
    /// Backwards wall-clock jumps invalidate freshness, too. This does not
    /// infer geographic departure or make network connectivity an Internet test.
    pub fn is_fresh(&self, now: SystemTime, cadence: Cadence) -> bool {
        now.duration_since(self.captured_at)
            .is_ok_and(|age| age < cadence.duration() * 2)
    }
}

pub trait Collector {
    fn battery(&mut self) -> Observation<Battery>;
    fn connectivity(&mut self) -> Observation<Connectivity>;
}

pub fn sample(
    collector: &mut impl Collector,
    consent: Consent,
    lifecycle: Lifecycle,
    now: SystemTime,
) -> Snapshot {
    fn observe<T>(
        enabled: bool,
        lifecycle: Lifecycle,
        read: impl FnOnce() -> Observation<T>,
    ) -> Observation<T> {
        if !enabled {
            return Observation::Disabled;
        }
        match lifecycle {
            Lifecycle::Foreground => read(),
            Lifecycle::Suspended => Observation::Stale,
        }
    }
    let battery = observe(consent.battery, lifecycle, || collector.battery());
    let connectivity = observe(consent.connectivity, lifecycle, || collector.connectivity());
    let activity = observe(consent.activity, lifecycle, || {
        Observation::Value(AppActivity::Foreground)
    });
    Snapshot {
        captured_at: now,
        battery,
        connectivity,
        activity,
    }
}

pub struct NativeCollector;
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct DeviceInfo {
    pub platform: Platform,
    pub model: String,
    pub os_version: String,
}
/// Descriptive model/OS only, never a serial/hostname/network identifier.
/// Call from an owned worker after an explicit registration intention.
pub fn device_info() -> Option<DeviceInfo> {
    #[cfg(target_os = "macos")]
    return macos::device_info();
    #[cfg(not(target_os = "macos"))]
    None
}
impl Collector for NativeCollector {
    fn battery(&mut self) -> Observation<Battery> {
        #[cfg(target_os = "macos")]
        return macos::battery();
        #[cfg(not(target_os = "macos"))]
        Observation::Unsupported
    }
    fn connectivity(&mut self) -> Observation<Connectivity> {
        #[cfg(target_os = "macos")]
        return macos::connectivity();
        #[cfg(not(target_os = "macos"))]
        Observation::Unsupported
    }
}

#[cfg(test)]
mod tests;
