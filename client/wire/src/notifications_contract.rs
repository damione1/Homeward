//! Bounded notification representation, installation binding and monotonic
//! receipt admission. Native permission and resource authority stay with their
//! actual OS/server owners; no URI is an actionable notification target.
use crate::{
    auth_contract::{AccountId, SessionId},
    companion_contract::{DeviceId, Epoch, InstallationId},
    home::{companion::v1 as companion, notifications::v1 as wire},
};
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use std::collections::BTreeSet;

pub const MAX_BYTES: u32 = 128 * 1024;
pub const MAX_ENTRIES: usize = 64;
pub const MAX_LIFETIME_SECONDS: i64 = 3600;
pub const MINIMUM_POLL_SECONDS: u32 = 5;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Invalid {
    Representation,
    ForeignIdentity,
    StaleContext,
    Regression,
    Expired,
}
macro_rules! identity {
    ($name:ident) => {
        #[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
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
identity!(DestinationId);
identity!(DeliveryId);
identity!(RequestId);
macro_rules! clean {
    ($v:expr) => {
        $v.__buffa_unknown_fields.is_empty() && $v.encoded_len() <= MAX_BYTES
    };
}
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
known!(
    permission,
    wire::Permission,
    NotRequested,
    Denied,
    Authorized,
    Provisional,
    Ephemeral,
    Unknown
);
known!(
    setting,
    wire::Setting,
    Enabled,
    Disabled,
    Unsupported,
    Unknown
);
known!(
    destination_state,
    wire::DestinationState,
    Active,
    Revoked,
    Expired
);
known!(reception, wire::Reception, NotReceived, Received);
known!(
    presentation,
    wire::Presentation,
    NotAttempted,
    OsAccepted,
    PresentInOsCenter,
    OutcomeUnknown,
    PermissionDenied,
    Unavailable
);
known!(read_state, wire::ReadState, Unread, Read, Dismissed);
known!(
    receipt_kind,
    wire::ReceiptKind,
    Received,
    OsAccepted,
    PresentInOsCenter,
    OutcomeUnknown,
    PermissionDenied,
    Unavailable,
    Read,
    Dismissed
);

fn stamp(v: &Timestamp) -> bool {
    clean!(v) && (1..=253_402_300_799).contains(&v.seconds) && (0..1_000_000_000).contains(&v.nanos)
}
fn time(value: &Timestamp) -> (i64, i32) {
    (value.seconds, value.nanos)
}
pub fn valid_context(v: &companion::Context) -> bool {
    clean!(v)
        && v.deployment == companion::Deployment::SyntheticOnly
        && Epoch::parse(&v.epoch).is_ok()
}
fn destination_id(v: &wire::DestinationId) -> bool {
    clean!(v) && DestinationId::parse(&v.value).is_ok()
}
fn delivery_id(v: &wire::DeliveryId) -> bool {
    clean!(v) && DeliveryId::parse(&v.value).is_ok()
}
fn revision(value: u64) -> bool {
    value > 0 && value < u64::MAX
}
fn plain(value: &str, maximum: usize, multiline: bool) -> bool {
    !value.trim().is_empty()
        && value.len() <= maximum
        && !value
            .chars()
            .any(|c| c.is_control() && !(multiline && c == '\n'))
}
pub fn valid_permission(v: &wire::PermissionSnapshot) -> bool {
    if !clean!(v)
        || !permission(v.permission)
        || !setting(v.alerts)
        || !setting(v.notification_center)
    {
        return false;
    }
    let no_surfaces = v.alerts == wire::Setting::Unsupported
        && v.notification_center == wire::Setting::Unsupported;
    match v.source {
        EnumValue::Known(wire::PermissionSource::MacOsUserNotifications) => true,
        EnumValue::Known(
            wire::PermissionSource::BundleRequired | wire::PermissionSource::Unsupported,
        ) => v.permission == wire::Permission::Unknown && no_surfaces,
        EnumValue::Known(wire::PermissionSource::AndroidPendingQualification) => {
            matches!(
                v.permission,
                EnumValue::Known(wire::Permission::NotRequested | wire::Permission::Unknown)
            ) && no_surfaces
        }
        EnumValue::Known(wire::PermissionSource::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
pub fn can_present(v: &wire::PermissionSnapshot) -> bool {
    valid_permission(v)
        && v.source == wire::PermissionSource::MacOsUserNotifications
        && matches!(
            v.permission,
            EnumValue::Known(
                wire::Permission::Authorized
                    | wire::Permission::Provisional
                    | wire::Permission::Ephemeral
            )
        )
        && (v.alerts == wire::Setting::Enabled || v.notification_center == wire::Setting::Enabled)
}
fn permission_for_platform(
    platform: EnumValue<companion::Platform>,
    v: &wire::PermissionSnapshot,
) -> bool {
    if !valid_permission(v) {
        return false;
    }
    match platform {
        EnumValue::Known(companion::Platform::MacOs) => matches!(
            v.source,
            EnumValue::Known(
                wire::PermissionSource::MacOsUserNotifications
                    | wire::PermissionSource::BundleRequired
                    | wire::PermissionSource::Unsupported
            )
        ),
        EnumValue::Known(companion::Platform::Android) => matches!(
            v.source,
            EnumValue::Known(
                wire::PermissionSource::AndroidPendingQualification
                    | wire::PermissionSource::Unsupported
            )
        ),
        EnumValue::Known(companion::Platform::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
pub fn valid_destination(v: &wire::Destination) -> bool {
    clean!(v)
        && v.id.as_option().is_some_and(destination_id)
        && v.installation_id
            .as_option()
            .is_some_and(|id| clean!(id) && InstallationId::parse(&id.value).is_ok())
        && v.device_id
            .as_option()
            .is_some_and(|id| clean!(id) && DeviceId::parse(&id.value).is_ok())
        && v.account_id
            .as_option()
            .is_some_and(|id| clean!(id) && AccountId::parse(&id.value).is_ok())
        && v.session_id
            .as_option()
            .is_some_and(|id| clean!(id) && SessionId::parse(&id.value).is_ok())
        && destination_state(v.state)
        && revision(v.revision)
        && v.permission
            .as_option()
            .is_some_and(|p| permission_for_platform(v.platform, p))
        && (!v.native_delivery
            || (v.state == wire::DestinationState::Active && can_present(&v.permission)))
        && v.expires_at.as_option().is_some_and(stamp)
        && RequestId::parse(&v.last_request_id).is_ok()
        && RequestId::parse(&v.registration_request_id).is_ok()
}
pub fn valid_target(v: &wire::Target) -> bool {
    clean!(v)
        && match &v.resource {
            Some(wire::target::Resource::Home(empty)) => clean!(empty),
            Some(wire::target::Resource::Entity(id)) => {
                clean!(id)
                    && plain(&id.value, 256, false)
                    && id.value.trim() == id.value
                    && id.value.parse::<home_client_model::EntityId>().is_ok()
            }
            None => false,
        }
}
fn transport_pair(v: &wire::Delivery) -> bool {
    match v.transport_acceptance {
        EnumValue::Known(wire::TransportAcceptance::AcceptedForegroundLan) => {
            v.transport_reason == wire::TransportReason::None
        }
        EnumValue::Known(wire::TransportAcceptance::ProviderRejected) => {
            v.transport_reason == wire::TransportReason::ProviderFailure
        }
        EnumValue::Known(wire::TransportAcceptance::Unavailable) => {
            v.transport_reason == wire::TransportReason::NoProvider
        }
        EnumValue::Known(wire::TransportAcceptance::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn presentation_pair(v: &wire::Delivery) -> bool {
    match v.presentation {
        EnumValue::Known(
            wire::Presentation::NotAttempted
            | wire::Presentation::PermissionDenied
            | wire::Presentation::Unavailable,
        ) => true,
        EnumValue::Known(
            wire::Presentation::OsAccepted
            | wire::Presentation::PresentInOsCenter
            | wire::Presentation::OutcomeUnknown,
        ) => {
            v.transport_acceptance == wire::TransportAcceptance::AcceptedForegroundLan
                && v.reception == wire::Reception::Received
        }
        EnumValue::Known(wire::Presentation::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
pub fn valid_delivery(v: &wire::Delivery) -> bool {
    clean!(v)
        && v.id.as_option().is_some_and(delivery_id)
        && v.destination_id.as_option().is_some_and(destination_id)
        && revision(v.revision)
        && plain(&v.title, 128, false)
        && plain(&v.body, 1024, true)
        && v.created_at.as_option().is_some_and(stamp)
        && v.expires_at.as_option().is_some_and(stamp)
        && time(&v.created_at) < time(&v.expires_at)
        && (
            v.expires_at.seconds.saturating_sub(v.created_at.seconds),
            v.expires_at.nanos - v.created_at.nanos,
        ) <= (MAX_LIFETIME_SECONDS, 0)
        && v.target.as_option().is_some_and(valid_target)
        && transport_pair(v)
        && reception(v.reception)
        && presentation(v.presentation)
        && presentation_pair(v)
        && read_state(v.read_state)
        && (v.read_state == wire::ReadState::Unread || v.reception == wire::Reception::Received)
        && (v.last_request_id.is_empty() || RequestId::parse(&v.last_request_id).is_ok())
}
/// Freshness/consent admission before a native presentation, after receipt and
/// fresh server/native session checks. It never replays a previously attempted
/// message when permissions or foreground visibility return.
pub fn eligible_for_native(
    value: &wire::Delivery,
    destination: &wire::Destination,
    now: &Timestamp,
) -> bool {
    stamp(now)
        && bind_delivery(value, destination, None).is_ok()
        && destination.state == wire::DestinationState::Active
        && destination.native_delivery
        && can_present(&destination.permission)
        && value.transport_acceptance == wire::TransportAcceptance::AcceptedForegroundLan
        && value.reception == wire::Reception::Received
        && value.presentation == wire::Presentation::NotAttempted
        && value.read_state == wire::ReadState::Unread
        && time(&value.created_at) <= time(now)
        && time(now) < time(&value.expires_at)
        && time(now) < time(&destination.expires_at)
}
pub enum Request<'a> {
    Describe(&'a wire::DescribeNotificationsRequest),
    Register(&'a wire::RegisterDestinationRequest),
    Get(&'a wire::GetDestinationRequest),
    Update(&'a wire::UpdateDestinationRequest),
    Inbox(&'a wire::ReadInboxRequest),
    Acknowledge(&'a wire::AcknowledgeDeliveryRequest),
    Delivery(&'a wire::GetDeliveryRequest),
    Open(&'a wire::ResolveOpenRequest),
    Revoke(&'a wire::RevokeDestinationRequest),
}
pub fn admit_request(value: Request<'_>) -> Result<(), Invalid> {
    let valid = match value {
        Request::Describe(r) => clean!(r),
        Request::Register(r) => {
            clean!(r)
                && r.expected_context.as_option().is_some_and(valid_context)
                && r.installation_id
                    .as_option()
                    .is_some_and(|id| clean!(id) && InstallationId::parse(&id.value).is_ok())
                && RequestId::parse(&r.request_id).is_ok()
                && r.permission.as_option().is_some_and(valid_permission)
        }
        Request::Get(r) => {
            clean!(r)
                && match &r.selector {
                    Some(wire::get_destination_request::Selector::Id(id)) => destination_id(id),
                    Some(wire::get_destination_request::Selector::RegistrationRequestId(id)) => {
                        RequestId::parse(id).is_ok()
                    }
                    None => false,
                }
        }
        Request::Inbox(r) => clean!(r) && r.id.as_option().is_some_and(destination_id),
        Request::Update(r) => {
            clean!(r)
                && r.expected_context.as_option().is_some_and(valid_context)
                && r.id.as_option().is_some_and(destination_id)
                && revision(r.expected_revision)
                && RequestId::parse(&r.request_id).is_ok()
                && r.permission.as_option().is_some_and(valid_permission)
                && (!r.native_delivery || can_present(&r.permission))
        }
        Request::Acknowledge(r) => {
            clean!(r)
                && r.expected_context.as_option().is_some_and(valid_context)
                && r.destination_id.as_option().is_some_and(destination_id)
                && r.delivery_id.as_option().is_some_and(delivery_id)
                && revision(r.expected_revision)
                && RequestId::parse(&r.request_id).is_ok()
                && receipt_kind(r.kind)
                && r.observed_at.as_option().is_some_and(stamp)
        }
        Request::Delivery(r) => {
            clean!(r)
                && r.destination_id.as_option().is_some_and(destination_id)
                && r.delivery_id.as_option().is_some_and(delivery_id)
        }
        Request::Open(r) => {
            clean!(r)
                && r.destination_id.as_option().is_some_and(destination_id)
                && r.delivery_id.as_option().is_some_and(delivery_id)
        }
        Request::Revoke(r) => {
            clean!(r)
                && r.expected_context.as_option().is_some_and(valid_context)
                && r.id.as_option().is_some_and(destination_id)
                && revision(r.expected_revision)
                && RequestId::parse(&r.request_id).is_ok()
        }
    };
    if valid {
        Ok(())
    } else {
        Err(Invalid::Representation)
    }
}
pub enum Response<'a> {
    Describe(&'a wire::DescribeNotificationsResponse),
    Register(&'a wire::RegisterDestinationResponse),
    Get(&'a wire::GetDestinationResponse),
    Update(&'a wire::UpdateDestinationResponse),
    Inbox(&'a wire::ReadInboxResponse),
    Acknowledge(&'a wire::AcknowledgeDeliveryResponse),
    Delivery(&'a wire::GetDeliveryResponse),
    Open(&'a wire::ResolveOpenResponse),
    Revoke(&'a wire::RevokeDestinationResponse),
}
pub fn admit_response(value: Response<'_>) -> Result<(), Invalid> {
    macro_rules! with_destination {
        ($r:ident) => {
            clean!($r)
                && $r.context.as_option().is_some_and(valid_context)
                && $r.destination.as_option().is_some_and(valid_destination)
        };
    }
    macro_rules! with_delivery {
        ($r:ident) => {
            clean!($r)
                && $r.context.as_option().is_some_and(valid_context)
                && $r.delivery.as_option().is_some_and(valid_delivery)
        };
    }
    let valid = match value {
        Response::Describe(r) => {
            clean!(r)
                && r.context.as_option().is_some_and(valid_context)
                && r.foreground == wire::ForegroundTransport::NativeSessionLanPoll
                && r.mac_os_background == wire::BackgroundTransport::Unavailable
                && r.android_background
                    == wire::BackgroundTransport::AndroidForegroundServicePendingQualification
                && r.minimum_poll_seconds == MINIMUM_POLL_SECONDS
                && r.maximum_message_lifetime_seconds == MAX_LIFETIME_SECONDS as u32
                && r.maximum_inbox_entries == MAX_ENTRIES as u32
                && !r.google_play_services_required
        }
        Response::Register(r) => with_destination!(r),
        Response::Get(r) => with_destination!(r),
        Response::Update(r) => with_destination!(r),
        Response::Revoke(r) => with_destination!(r),
        Response::Acknowledge(r) => with_delivery!(r),
        Response::Delivery(r) => with_delivery!(r),
        Response::Inbox(r) => {
            let mut ids = BTreeSet::new();
            with_destination!(r)
                && revision(r.revision)
                && r.entries.len() <= MAX_ENTRIES
                && r.entries.iter().all(|entry| {
                    valid_delivery(entry)
                        && entry.destination_id == r.destination.id
                        && ids.insert(entry.id.value.as_str())
                })
        }
        Response::Open(r) => {
            clean!(r)
                && r.context.as_option().is_some_and(valid_context)
                && r.destination_id.as_option().is_some_and(destination_id)
                && r.delivery_id.as_option().is_some_and(delivery_id)
                && r.authorized_target.as_option().is_some_and(valid_target)
                && r.valid_until.as_option().is_some_and(stamp)
        }
    };
    if valid {
        Ok(())
    } else {
        Err(Invalid::Representation)
    }
}
pub fn bind_native(
    destination: &wire::Destination,
    installation: &companion::Installation,
    account: &AccountId,
    session: &SessionId,
) -> Result<(), Invalid> {
    if !valid_destination(destination)
        || !crate::companion_contract::valid_installation(installation)
    {
        return Err(Invalid::Representation);
    }
    if installation.state != companion::InstallationState::Active
        || destination.installation_id != installation.id
        || destination.device_id != installation.device_id
        || destination.account_id.value != account.as_str()
        || destination.session_id.value != session.as_str()
        || destination.account_id != installation.account_id
        || destination.session_id != installation.session_id
        || destination.platform != installation.device.platform
        || time(&destination.expires_at) > time(&installation.expires_at)
    {
        return Err(Invalid::ForeignIdentity);
    }
    Ok(())
}
pub fn bind_existing(
    context: &companion::Context,
    value: &wire::Destination,
    expected_context: &companion::Context,
    prior: &wire::Destination,
) -> Result<(), Invalid> {
    if !valid_destination(value) || !valid_destination(prior) || !valid_context(context) {
        return Err(Invalid::Representation);
    }
    if context != expected_context {
        return Err(Invalid::StaleContext);
    }
    if value.id != prior.id
        || value.installation_id != prior.installation_id
        || value.device_id != prior.device_id
        || value.account_id != prior.account_id
        || value.session_id != prior.session_id
        || value.platform != prior.platform
        || value.registration_request_id != prior.registration_request_id
    {
        return Err(Invalid::ForeignIdentity);
    }
    if value.revision < prior.revision
        || value.expires_at != prior.expires_at
        || (prior.state != wire::DestinationState::Active && value.state != prior.state)
        || (value.revision == prior.revision && value != prior)
    {
        return Err(Invalid::Regression);
    }
    Ok(())
}
pub fn presentation_can_advance(
    prior: EnumValue<wire::Presentation>,
    next: EnumValue<wire::Presentation>,
) -> bool {
    if !presentation(next) {
        return false;
    }
    match prior {
        EnumValue::Known(wire::Presentation::NotAttempted) => true,
        EnumValue::Known(wire::Presentation::OsAccepted) => matches!(
            next,
            EnumValue::Known(
                wire::Presentation::OsAccepted | wire::Presentation::PresentInOsCenter
            )
        ),
        EnumValue::Known(wire::Presentation::OutcomeUnknown) => matches!(
            next,
            EnumValue::Known(
                wire::Presentation::OutcomeUnknown
                    | wire::Presentation::OsAccepted
                    | wire::Presentation::PresentInOsCenter
            )
        ),
        EnumValue::Known(
            wire::Presentation::PresentInOsCenter
            | wire::Presentation::PermissionDenied
            | wire::Presentation::Unavailable,
        ) => prior == next,
        EnumValue::Known(wire::Presentation::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
pub fn bind_delivery(
    value: &wire::Delivery,
    destination: &wire::Destination,
    prior: Option<&wire::Delivery>,
) -> Result<(), Invalid> {
    if !valid_delivery(value) || !valid_destination(destination) {
        return Err(Invalid::Representation);
    }
    if value.destination_id != destination.id {
        return Err(Invalid::ForeignIdentity);
    }
    if let Some(prior) = prior {
        if !valid_delivery(prior) {
            return Err(Invalid::Representation);
        }
        if value.id != prior.id || value.destination_id != prior.destination_id {
            return Err(Invalid::ForeignIdentity);
        }
        if value.revision < prior.revision
            || value.title != prior.title
            || value.body != prior.body
            || value.created_at != prior.created_at
            || value.expires_at != prior.expires_at
            || value.target != prior.target
            || value.transport_acceptance != prior.transport_acceptance
            || value.transport_reason != prior.transport_reason
            || !presentation_can_advance(prior.presentation, value.presentation)
            || (prior.reception == wire::Reception::Received && value.reception != prior.reception)
            || (prior.read_state == wire::ReadState::Dismissed
                && value.read_state != prior.read_state)
            || (prior.read_state == wire::ReadState::Read
                && value.read_state == wire::ReadState::Unread)
            || (value.revision == prior.revision && value != prior)
        {
            return Err(Invalid::Regression);
        }
    }
    Ok(())
}
pub fn bind_open(
    value: &wire::ResolveOpenResponse,
    context: &companion::Context,
    destination: &wire::Destination,
    delivery: &wire::Delivery,
    now: &Timestamp,
) -> Result<(), Invalid> {
    admit_response(Response::Open(value))?;
    if !stamp(now) {
        return Err(Invalid::Representation);
    }
    if value.context.as_option() != Some(context) {
        return Err(Invalid::StaleContext);
    }
    bind_delivery(delivery, destination, None)?;
    if value.destination_id != destination.id
        || value.delivery_id != delivery.id
        || value.authorized_target != delivery.target
    {
        return Err(Invalid::ForeignIdentity);
    }
    if destination.state != wire::DestinationState::Active
        || delivery.transport_acceptance != wire::TransportAcceptance::AcceptedForegroundLan
        || delivery.read_state == wire::ReadState::Dismissed
        || time(now) < time(&delivery.created_at)
        || time(now) >= time(&delivery.expires_at)
        || time(now) >= time(&destination.expires_at)
        || time(now) >= time(&value.valid_until)
        || time(&value.valid_until) > (now.seconds.saturating_add(5), now.nanos)
        || time(&value.valid_until) > time(&delivery.expires_at)
        || time(&value.valid_until) > time(&destination.expires_at)
    {
        return Err(Invalid::Expired);
    }
    Ok(())
}
