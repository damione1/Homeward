//! Parse Android SDK values once at the JNI boundary. Missing/unknown data
//! never becomes a zero percent, a disconnected route or a charging state.
use super::*;
#[derive(Clone, Copy)]
enum Status {
    Unknown,
    Charging,
    Discharging,
    NotCharging,
    Full,
}
impl Status {
    fn parse(raw: i32) -> Option<Self> {
        match raw {
            1 => Some(Self::Unknown),
            2 => Some(Self::Charging),
            3 => Some(Self::Discharging),
            4 => Some(Self::NotCharging),
            5 => Some(Self::Full),
            _ => None,
        }
    }
}
pub(super) fn battery(raw: [i32; 5]) -> Observation<Battery> {
    let [kind, level, scale, status, plugged] = raw;
    match kind {
        2 => return Observation::NoHardware,
        1 => (),
        _ => return Observation::Unavailable,
    }
    let Some(percent) = BatteryPercent::from_capacity(level.into(), scale.into()) else {
        return Observation::Unavailable;
    };
    // AC, USB, wireless and dock are the public SDK plug flags. Reject future
    // flags instead of inventing an external-power observation.
    if plugged < 0 || plugged & !0xf != 0 {
        return Observation::Unavailable;
    }
    let Some(status) = Status::parse(status) else {
        return Observation::Unavailable;
    };
    let state = match status {
        Status::Charging => PowerState::Charging,
        Status::Discharging => PowerState::Battery,
        Status::NotCharging if plugged != 0 => PowerState::ExternalPower,
        Status::NotCharging => PowerState::Battery,
        Status::Full => PowerState::Full,
        Status::Unknown => return Observation::Unavailable,
    };
    Observation::Value(Battery { percent, state })
}
pub(super) fn connectivity(raw: i32) -> Observation<Connectivity> {
    match raw {
        1 => Observation::Value(Connectivity::LocallyReachable),
        2 => Observation::Value(Connectivity::ConnectionRequired),
        3 => Observation::Value(Connectivity::NoLocalRoute),
        4 => Observation::PermissionDenied,
        _ => Observation::Unavailable,
    }
}
