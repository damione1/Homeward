//! Bounded representation and identity binding for the provisional companion
//! API. Authority, consent and freshness remain server/owner responsibilities.
use crate::{
    auth_contract::{AccountId, SessionId},
    home::companion::v1 as wire,
};
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use std::collections::BTreeSet;

pub const MAX_BYTES: u32 = 32 * 1024;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Invalid {
    Representation,
    ForeignIdentity,
    StaleContext,
}
macro_rules! identity {
    ($name:ident) => {
        #[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
        pub struct $name(String);
        impl $name {
            pub fn parse(raw: &str) -> Result<Self, Invalid> {
                if raw.len() == 32
                    && raw
                        .bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
                {
                    Ok(Self(raw.into()))
                } else {
                    Err(Invalid::Representation)
                }
            }
            pub fn as_str(&self) -> &str {
                &self.0
            }
        }
    };
}
identity!(InstallationId);
identity!(DeviceId);
identity!(RequestId);
identity!(Epoch);
macro_rules! known {
    ($name:ident, $ty:path, $($variant:ident),+ $(,)?) => {
        fn $name(value: EnumValue<$ty>) -> bool {
            use $ty as V;
            match value {
                EnumValue::Known($(V::$variant)|+) => true,
                EnumValue::Known(V::Unspecified) | EnumValue::Unknown(_) => false,
            }
        }
    };
}
known!(platform, wire::Platform, MacOs, Android);
known!(
    feature,
    wire::Feature,
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
    Notifications
);
known!(
    availability,
    wire::Availability,
    Available,
    Partial,
    Unsupported,
    PendingQualification
);
known!(
    permission,
    wire::Permission,
    NotRequired,
    NotRequested,
    Granted,
    Denied,
    Restricted,
    Unsupported
);
known!(
    source,
    wire::Source,
    MacOsPowerSources,
    MacOsNetworkConfiguration,
    NativeLifecycle,
    None
);
known!(
    no_data,
    wire::NoData,
    Disabled,
    Unsupported,
    NoHardware,
    Unavailable,
    PermissionDenied,
    Stale
);
known!(
    power,
    wire::PowerState,
    Battery,
    Charging,
    ExternalPower,
    Full
);
known!(
    connectivity,
    wire::Connectivity,
    LocallyReachable,
    ConnectionRequired,
    NoLocalRoute
);
known!(activity, wire::AppActivity, Foreground);
known!(state, wire::InstallationState, Active, Revoked, Expired);

pub fn cadence_seconds(value: EnumValue<wire::Cadence>) -> Option<u64> {
    match value {
        EnumValue::Known(wire::Cadence::Seconds30) => Some(30),
        EnumValue::Known(wire::Cadence::Minute1) => Some(60),
        EnumValue::Known(wire::Cadence::Minutes5) => Some(300),
        EnumValue::Known(wire::Cadence::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
fn bounded_text(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value.trim() == value
        && !value.chars().any(char::is_control)
}
fn stamp(value: &Timestamp) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && (1..=253_402_300_799).contains(&value.seconds)
        && (0..1_000_000_000).contains(&value.nanos)
}
fn context(v: &wire::Context) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && v.deployment == wire::Deployment::SyntheticOnly
        && Epoch::parse(&v.epoch).is_ok()
}
fn id(v: &wire::InstallationId) -> bool {
    v.__buffa_unknown_fields.is_empty() && InstallationId::parse(&v.value).is_ok()
}
fn capability_profile(platform: EnumValue<wire::Platform>, c: &wire::Capability) -> bool {
    let profile = match platform {
        EnumValue::Known(wire::Platform::MacOs) => match c.feature {
            EnumValue::Known(
                wire::Feature::BatteryLevel | wire::Feature::BatteryState | wire::Feature::Charging,
            ) => Some((
                wire::Availability::Available,
                wire::Source::MacOsPowerSources,
            )),
            EnumValue::Known(wire::Feature::Connectivity) => Some((
                wire::Availability::Partial,
                wire::Source::MacOsNetworkConfiguration,
            )),
            EnumValue::Known(wire::Feature::Activity) => {
                Some((wire::Availability::Partial, wire::Source::NativeLifecycle))
            }
            EnumValue::Known(
                wire::Feature::Focus
                | wire::Feature::DoNotDisturb
                | wire::Feature::Kiosk
                | wire::Feature::ScreenSaver
                | wire::Feature::Brightness
                | wire::Feature::Volume
                | wire::Feature::Location
                | wire::Feature::CameraMotion
                | wire::Feature::Notifications,
            ) => Some((wire::Availability::Unsupported, wire::Source::None)),
            EnumValue::Known(wire::Feature::Unspecified) | EnumValue::Unknown(_) => None,
        },
        EnumValue::Known(wire::Platform::Android) => match c.feature {
            EnumValue::Known(wire::Feature::Focus) => {
                Some((wire::Availability::Unsupported, wire::Source::None))
            }
            EnumValue::Known(
                wire::Feature::BatteryLevel
                | wire::Feature::BatteryState
                | wire::Feature::Charging
                | wire::Feature::Connectivity
                | wire::Feature::Activity
                | wire::Feature::DoNotDisturb
                | wire::Feature::Kiosk
                | wire::Feature::ScreenSaver
                | wire::Feature::Brightness
                | wire::Feature::Volume
                | wire::Feature::Location
                | wire::Feature::CameraMotion
                | wire::Feature::Notifications,
            ) => Some((wire::Availability::PendingQualification, wire::Source::None)),
            EnumValue::Known(wire::Feature::Unspecified) | EnumValue::Unknown(_) => None,
        },
        EnumValue::Known(wire::Platform::Unspecified) | EnumValue::Unknown(_) => None,
    };
    profile.is_some_and(|(availability, source)| {
        c.availability == availability
            && c.source == source
            && if availability == wire::Availability::Unsupported {
                c.permission == wire::Permission::Unsupported
            } else if availability == wire::Availability::PendingQualification {
                c.permission == wire::Permission::NotRequested
            } else {
                matches!(
                    c.permission,
                    EnumValue::Known(
                        wire::Permission::NotRequired
                            | wire::Permission::Granted
                            | wire::Permission::Denied
                            | wire::Permission::Restricted
                    )
                )
            }
    })
}
pub fn valid_device(v: &wire::DeviceDescription) -> bool {
    if !v.__buffa_unknown_fields.is_empty()
        || !platform(v.platform)
        || !bounded_text(&v.model)
        || !bounded_text(&v.os_version)
        || v.capabilities.len() != 14
    {
        return false;
    }
    let mut features = BTreeSet::new();
    v.capabilities.iter().all(|c| {
        c.__buffa_unknown_fields.is_empty()
            && feature(c.feature)
            && features.insert(c.feature.to_i32())
            && availability(c.availability)
            && permission(c.permission)
            && source(c.source)
            && capability_profile(v.platform, c)
    })
}
fn consent(v: &wire::Consent) -> bool {
    v.__buffa_unknown_fields.is_empty() && cadence_seconds(v.cadence).is_some()
}
pub fn can_collect(device: &wire::DeviceDescription, feature: wire::Feature) -> bool {
    device
        .capabilities
        .iter()
        .find(|c| c.feature == feature)
        .is_some_and(|c| {
            matches!(
                c.availability,
                EnumValue::Known(wire::Availability::Available | wire::Availability::Partial)
            ) && matches!(
                c.permission,
                EnumValue::Known(wire::Permission::NotRequired | wire::Permission::Granted)
            ) && c.source != wire::Source::None
        })
}
pub fn valid_consent(v: &wire::Consent, device: &wire::DeviceDescription) -> bool {
    consent(v)
        && (!v.battery
            || [
                wire::Feature::BatteryLevel,
                wire::Feature::BatteryState,
                wire::Feature::Charging,
            ]
            .into_iter()
            .all(|f| can_collect(device, f)))
        && (!v.connectivity || can_collect(device, wire::Feature::Connectivity))
        && (!v.activity || can_collect(device, wire::Feature::Activity))
}
fn valid_battery(v: &wire::BatteryObservation) -> bool {
    if !v.__buffa_unknown_fields.is_empty() {
        return false;
    }
    match &v.observation {
        Some(wire::battery_observation::Observation::Value(b)) => {
            b.__buffa_unknown_fields.is_empty() && b.percent <= 100 && power(b.state)
        }
        Some(wire::battery_observation::Observation::NoData(n)) => no_data(*n),
        None => false,
    }
}
fn valid_connectivity(v: &wire::ConnectivityObservation) -> bool {
    if !v.__buffa_unknown_fields.is_empty() {
        return false;
    }
    match v.observation {
        Some(wire::connectivity_observation::Observation::Value(c)) => connectivity(c),
        Some(wire::connectivity_observation::Observation::NoData(n)) => {
            no_data(n) && n != wire::NoData::NoHardware
        }
        None => false,
    }
}
fn valid_activity(v: &wire::ActivityObservation) -> bool {
    if !v.__buffa_unknown_fields.is_empty() {
        return false;
    }
    match v.observation {
        Some(wire::activity_observation::Observation::Value(a)) => activity(a),
        Some(wire::activity_observation::Observation::NoData(n)) => {
            no_data(n) && n != wire::NoData::NoHardware
        }
        None => false,
    }
}
pub fn valid_snapshot(v: &wire::Snapshot) -> bool {
    v.__buffa_unknown_fields.is_empty()
        && v.captured_at.as_option().is_some_and(stamp)
        && v.battery.as_option().is_some_and(valid_battery)
        && v.connectivity.as_option().is_some_and(valid_connectivity)
        && v.activity.as_option().is_some_and(valid_activity)
}
/// A nonconsented group must be Disabled, even if the value is locally known.
/// An enabled group may report a typed absence/refusal, never Disabled.
pub fn snapshot_matches_consent(v: &wire::Snapshot, consent: &wire::Consent) -> bool {
    use wire::{
        activity_observation::Observation as A, battery_observation::Observation as B,
        connectivity_observation::Observation as C,
    };
    if !valid_snapshot(v) {
        return false;
    }
    let battery_disabled =
        matches!(&v.battery.observation, Some(B::NoData(n)) if *n == wire::NoData::Disabled);
    let connectivity_disabled =
        matches!(&v.connectivity.observation, Some(C::NoData(n)) if *n == wire::NoData::Disabled);
    let activity_disabled =
        matches!(&v.activity.observation, Some(A::NoData(n)) if *n == wire::NoData::Disabled);
    battery_disabled != consent.battery
        && connectivity_disabled != consent.connectivity
        && activity_disabled != consent.activity
}
pub fn valid_installation(v: &wire::Installation) -> bool {
    if !v.__buffa_unknown_fields.is_empty()
        || !v.id.as_option().is_some_and(id)
        || !v.device_id.as_option().is_some_and(|d| {
            d.__buffa_unknown_fields.is_empty() && DeviceId::parse(&d.value).is_ok()
        })
        || v.id.value == v.device_id.value
        || !v
            .account_id
            .as_option()
            .is_some_and(|a| bounded_text(&a.value) && AccountId::try_from(a).is_ok())
        || !v
            .session_id
            .as_option()
            .is_some_and(|s| bounded_text(&s.value) && SessionId::try_from(s).is_ok())
        || !v.device.as_option().is_some_and(valid_device)
        || !v
            .consent
            .as_option()
            .is_some_and(|c| valid_consent(c, &v.device))
        || v.revision == 0
        || v.revision == u64::MAX
        || v.last_sequence == u64::MAX
        || !state(v.state)
        || !v.expires_at.as_option().is_some_and(stamp)
        || RequestId::parse(&v.registration_request_id).is_err()
    {
        return false;
    }
    let retained = match (v.latest.as_option(), v.accepted_at.as_option()) {
        (None, None) => true,
        (Some(s), Some(t)) => {
            v.last_sequence > 0
                && stamp(t)
                && (v.consent.battery || v.consent.connectivity || v.consent.activity)
                && snapshot_matches_consent(s, &v.consent)
        }
        (Some(_), None) | (None, Some(_)) => false,
    };
    retained
        && match v.state {
            EnumValue::Known(wire::InstallationState::Active) => true,
            EnumValue::Known(
                wire::InstallationState::Revoked | wire::InstallationState::Expired,
            ) => {
                !v.consent.battery
                    && !v.consent.connectivity
                    && !v.consent.activity
                    && v.latest.as_option().is_none()
                    && v.accepted_at.as_option().is_none()
            }
            EnumValue::Known(wire::InstallationState::Unspecified) | EnumValue::Unknown(_) => false,
        }
}

pub enum Request<'a> {
    Describe(&'a wire::DescribeCompanionRequest),
    Register(&'a wire::RegisterInstallationRequest),
    Get(&'a wire::GetInstallationRequest),
    Consent(&'a wire::UpdateConsentRequest),
    Publish(&'a wire::PublishTelemetryRequest),
    Revoke(&'a wire::RevokeInstallationRequest),
}
pub fn admit_request(v: Request<'_>) -> Result<(), Invalid> {
    let valid = match v {
        Request::Describe(r) => r.__buffa_unknown_fields.is_empty(),
        Request::Register(r) => {
            r.encoded_len() <= MAX_BYTES
                && r.__buffa_unknown_fields.is_empty()
                && RequestId::parse(&r.request_id).is_ok()
                && r.device.as_option().is_some_and(valid_device)
                && r.expected.as_option().is_some_and(context)
        }
        Request::Get(r) => {
            r.encoded_len() <= MAX_BYTES
                && r.__buffa_unknown_fields.is_empty()
                && match &r.selector {
                    Some(wire::get_installation_request::Selector::Id(i)) => id(i),
                    Some(wire::get_installation_request::Selector::RegistrationRequestId(i)) => {
                        RequestId::parse(i).is_ok()
                    }
                    None => false,
                }
        }
        Request::Consent(r) => {
            r.encoded_len() <= MAX_BYTES
                && r.__buffa_unknown_fields.is_empty()
                && r.expected.as_option().is_some_and(context)
                && r.id.as_option().is_some_and(id)
                && r.expected_revision > 0
                && r.expected_revision < u64::MAX
                && r.consent.as_option().is_some_and(consent)
        }
        Request::Publish(r) => {
            r.encoded_len() <= MAX_BYTES
                && r.__buffa_unknown_fields.is_empty()
                && r.expected.as_option().is_some_and(context)
                && r.id.as_option().is_some_and(id)
                && r.expected_revision > 0
                && r.expected_revision < u64::MAX
                && r.sequence > 0
                && r.sequence < u64::MAX
                && r.snapshot.as_option().is_some_and(valid_snapshot)
        }
        Request::Revoke(r) => {
            r.encoded_len() <= MAX_BYTES
                && r.__buffa_unknown_fields.is_empty()
                && r.expected.as_option().is_some_and(context)
                && r.id.as_option().is_some_and(id)
                && r.expected_revision > 0
                && r.expected_revision < u64::MAX
        }
    };
    if valid {
        Ok(())
    } else {
        Err(Invalid::Representation)
    }
}
pub enum Response<'a> {
    Describe(&'a wire::DescribeCompanionResponse),
    Register(&'a wire::RegisterInstallationResponse),
    Get(&'a wire::GetInstallationResponse),
    Consent(&'a wire::UpdateConsentResponse),
    Publish(&'a wire::PublishTelemetryResponse),
    Revoke(&'a wire::RevokeInstallationResponse),
}
pub fn admit_response(v: Response<'_>) -> Result<(), Invalid> {
    macro_rules! valid {
        ($r:ident) => {
            $r.encoded_len() <= MAX_BYTES
                && $r.__buffa_unknown_fields.is_empty()
                && $r.context.as_option().is_some_and(context)
                && $r.installation.as_option().is_some_and(valid_installation)
        };
    }
    let valid = match v {
        Response::Describe(r) => {
            r.__buffa_unknown_fields.is_empty()
                && r.encoded_len() <= MAX_BYTES
                && r.context.as_option().is_some_and(context)
        }
        Response::Register(r) => valid!(r),
        Response::Get(r) => valid!(r),
        Response::Consent(r) => valid!(r),
        Response::Publish(r) => valid!(r),
        Response::Revoke(r) => valid!(r),
    };
    if valid {
        Ok(())
    } else {
        Err(Invalid::Representation)
    }
}
pub fn bind_native(
    v: &wire::Installation,
    account: &AccountId,
    session: &SessionId,
) -> Result<(), Invalid> {
    if valid_installation(v)
        && v.account_id.value == account.as_str()
        && v.session_id.value == session.as_str()
    {
        Ok(())
    } else {
        Err(Invalid::ForeignIdentity)
    }
}
pub fn bind_existing(
    context: &wire::Context,
    value: &wire::Installation,
    expected_context: &wire::Context,
    expected: &wire::Installation,
) -> Result<(), Invalid> {
    if !valid_installation(value) || !valid_installation(expected) {
        return Err(Invalid::Representation);
    }
    if context != expected_context {
        return Err(Invalid::StaleContext);
    }
    if value.id != expected.id
        || value.device_id != expected.device_id
        || value.account_id != expected.account_id
        || value.session_id != expected.session_id
        || value.device != expected.device
        || value.registration_request_id != expected.registration_request_id
        || value.expires_at != expected.expires_at
    {
        return Err(Invalid::ForeignIdentity);
    }
    Ok(())
}
