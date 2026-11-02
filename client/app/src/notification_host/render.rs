use super::*;
use crate::{NotificationPresentation, NotificationRow, OptionRow};
use buffa::EnumValue;
use slint::{ModelRc, VecModel};
pub(super) struct Projection {
    view: NotificationPresentation,
    rows: Vec<Row>,
}
struct Row {
    key: state::RowKey,
    label: String,
    description: String,
    read: bool,
}
pub(super) fn empty(generation: i32) -> Projection {
    let state = State {
        generation,
        stage: Stage::WaitingParent,
        ..Default::default()
    };
    project(&state)
}
fn permission(v: &wire::PermissionSnapshot) -> String {
    let platform = match v.source {
        EnumValue::Known(wire::PermissionSource::MacOsUserNotifications) => Some("macOS"),
        EnumValue::Known(wire::PermissionSource::AndroidNotificationManager) => Some("Android"),
        EnumValue::Known(
            wire::PermissionSource::BundleRequired
            | wire::PermissionSource::AndroidPendingQualification
            | wire::PermissionSource::Unsupported
            | wire::PermissionSource::Unspecified,
        )
        | EnumValue::Unknown(_) => None,
    };
    if let Some(platform) = platform {
        let permission = match v.permission {
            EnumValue::Known(wire::Permission::NotRequested) => "not requested",
            EnumValue::Known(wire::Permission::Denied) => "denied · change it in system settings",
            EnumValue::Known(wire::Permission::Authorized) => "granted",
            EnumValue::Known(wire::Permission::Provisional) => "provisoire",
            EnumValue::Known(wire::Permission::Ephemeral) => "temporaire",
            EnumValue::Known(wire::Permission::Unknown | wire::Permission::Unspecified)
            | EnumValue::Unknown(_) => "unknown",
        };
        return format!(
            "{platform} permission {permission} · Banners: {} · System notification center: {}",
            setting(v.alerts),
            setting(v.notification_center)
        );
    }
    match v.source {
        EnumValue::Known(wire::PermissionSource::BundleRequired) => "Open the installed macOS app to receive native notifications.",
        EnumValue::Known(wire::PermissionSource::AndroidPendingQualification) => "Android/Fire: background reception has not been qualified; Google Play Services is not required.",
        EnumValue::Known(wire::PermissionSource::Unsupported) => "OS permissions unavailable; the inbox is separate from native notification display.",
        EnumValue::Known(wire::PermissionSource::Unspecified | wire::PermissionSource::MacOsUserNotifications | wire::PermissionSource::AndroidNotificationManager) | EnumValue::Unknown(_) => "Permission rejected",
    }.into()
}

fn setting(value: EnumValue<wire::Setting>) -> &'static str {
    match value {
        EnumValue::Known(wire::Setting::Enabled) => "enabled",
        EnumValue::Known(wire::Setting::Disabled) => "disabled",
        EnumValue::Known(wire::Setting::Unsupported) => "unavailable",
        EnumValue::Known(wire::Setting::Unknown | wire::Setting::Unspecified)
        | EnumValue::Unknown(_) => "unknown",
    }
}
fn transport(value: &wire::Delivery) -> &'static str {
    match value.transport_acceptance {
        EnumValue::Known(wire::TransportAcceptance::AcceptedForegroundLan) => "Transport accepted",
        EnumValue::Known(wire::TransportAcceptance::ProviderRejected) => "Provider rejection",
        EnumValue::Known(wire::TransportAcceptance::Unavailable) => "Transport unavailable",
        EnumValue::Known(wire::TransportAcceptance::Unspecified) | EnumValue::Unknown(_) => {
            "Transport rejected"
        }
    }
}
fn presentation(value: &wire::Delivery) -> &'static str {
    match value.presentation {
        EnumValue::Known(wire::Presentation::NotAttempted) => "Presentation not attempted",
        EnumValue::Known(wire::Presentation::OsAccepted) => {
            "Request accepted by the OS · display not verified"
        }
        EnumValue::Known(wire::Presentation::PresentInOsCenter) => {
            "Delivery to the system notification center verified"
        }
        EnumValue::Known(wire::Presentation::OutcomeUnknown) => {
            "OS result unknown · no resubmission"
        }
        EnumValue::Known(wire::Presentation::PermissionDenied) => "Permission denied",
        EnumValue::Known(wire::Presentation::Unavailable) => "Presentation unavailable",
        EnumValue::Known(wire::Presentation::Unspecified) | EnumValue::Unknown(_) => {
            "Presentation rejected"
        }
    }
}
pub(super) fn project(state: &State) -> Projection {
    let registered = state.parent.is_some()
        && state
            .destination
            .as_ref()
            .is_some_and(|d| d.state == wire::DestinationState::Active);
    let free = state.active
        && state.foreground
        && !state.running
        && state.pending.is_none()
        && state.draft.is_none();
    let rows = state
        .entries
        .iter()
        .filter(|_| state.foreground)
        .filter_map(|entry| {
            let key = state.row_key(&admit::DeliveryId::parse(&entry.id.value).ok()?)?;
            let received = if entry.reception == wire::Reception::Received {
                "Received"
            } else {
                "Receipt awaiting acknowledgment"
            };
            let foreground = state
                .native
                .as_ref()
                .and_then(|scope| {
                    os::PresentationId::parse(&entry.id.value)
                        .ok()
                        .map(|id| scope.foreground_evidence(&id))
                })
                .unwrap_or(os::ForegroundEvidence::NotObserved);
            let foreground = match foreground {
                os::ForegroundEvidence::NotObserved => "",
                os::ForegroundEvidence::AllowedByDelegate => {
                    "\nForeground notification presentation requested"
                }
                os::ForegroundEvidence::SuppressedByDelegate => {
                    "\nForeground notification presentation paused"
                }
            };
            Some(Row {
                key,
                label: entry.title.clone(),
                description: format!(
                    "{}\n{} · {}\n{}{foreground}",
                    entry.body,
                    transport(entry),
                    received,
                    presentation(entry)
                ),
                read: entry.read_state == wire::ReadState::Read,
            })
        })
        .collect();
    Projection {
        view: NotificationPresentation {
            generation: state.generation,
            status: if state.background_running() && matches!(state.stage, Stage::Ready) {
                "Local reception with Android service active · isolated development server".into()
            } else {
                state.stage.label().into()
            },
            permission: permission(&state.permission).into(),
            notice: if state.notice.is_empty() {
                if state.background_supported {
                    "Messages are specific to this installation. Receive them in the foreground or through the separately enabled service. Continuous background reception is not guaranteed.".into()
                } else {
                    "Messages are specific to this installation. Local reception requires the app in the foreground; continuous background reception is not guaranteed.".into()
                }
            } else {
                state.notice.as_str().into()
            },
            busy: state.running,
            registered,
            native_enabled: registered
                && !state.local_stopped
                && state
                    .destination
                    .as_ref()
                    .is_some_and(|d| d.native_delivery),
            can_register: free && !registered && state.parent.is_some(),
            can_enable: free
                && registered
                && admit::can_present(&state.permission)
                && (state.local_stopped
                    || state
                        .destination
                        .as_ref()
                        .is_some_and(|d| !d.native_delivery)),
            permission_action_label: match os::backend() {
                os::Backend::MacOs => "Request macOS Permission",
                os::Backend::Android => "Configure Android Notifications",
                os::Backend::Unavailable => "Notifications unavailable",
            }
            .into(),
            can_permission: free
                && matches!(
                    state.permission.source,
                    EnumValue::Known(
                        wire::PermissionSource::MacOsUserNotifications
                            | wire::PermissionSource::AndroidNotificationManager
                    )
                )
                && (state.permission.permission == wire::Permission::NotRequested
                    || (state.permission.source
                        == wire::PermissionSource::AndroidNotificationManager
                        && state.permission.permission == wire::Permission::Authorized
                        && state.permission.notification_center == wire::Setting::Unknown)),
            can_revoke: free && registered,
            confirming: state.draft.is_some(),
            confirmation: state.draft.map_or("", state::Draft::label).into(),
            background_status: if !state.background_supported {
                "".into()
            } else {
                match state.background.as_ref().map(|v| v.source.status()) {
                    None | Some(os::ReceptionStatus::Stopped) => "Background reception stopped · enable it explicitly to resume".into(),
                    Some(os::ReceptionStatus::Starting) => "Starting the system service…".into(),
                    Some(os::ReceptionStatus::Running) => "Background reception active · stop it from the system notification · uses network and battery. Doze/Fire may delay or stop reception. It will not restart automatically.".into(),
                }
            },
            can_start_background: free
                && state.background_supported
                && state.background.is_none()
                && state.open_binding().is_some()
                && admit::can_present(&state.permission),
            can_stop_background: state.background.is_some(),
        },
        rows,
    }
}
pub(super) fn apply(window: &HomewardWindow, projection: Projection) {
    let rows = projection
        .rows
        .into_iter()
        .map(|row| NotificationRow {
            item: OptionRow {
                token: row.key.token,
                generation: row.key.generation,
                label: row.label.into(),
                description: row.description.into(),
                disabled: false,
            },
            read: row.read,
        })
        .collect::<Vec<_>>();
    window.set_notification_view(projection.view);
    window.set_notification_has_unread(rows.iter().any(|row| !row.read));
    window.set_notification_rows(ModelRc::new(VecModel::from(rows)));
}
