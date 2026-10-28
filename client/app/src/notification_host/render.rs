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
            EnumValue::Known(wire::Permission::NotRequested) => "non demandée",
            EnumValue::Known(wire::Permission::Denied) => {
                "refusée · modifiable dans les réglages du système"
            }
            EnumValue::Known(wire::Permission::Authorized) => "accordée",
            EnumValue::Known(wire::Permission::Provisional) => "provisoire",
            EnumValue::Known(wire::Permission::Ephemeral) => "temporaire",
            EnumValue::Known(wire::Permission::Unknown | wire::Permission::Unspecified)
            | EnumValue::Unknown(_) => "inconnue",
        };
        return format!(
            "Autorisation {platform} {permission} · Bannières : {} · Centre OS : {}",
            setting(v.alerts),
            setting(v.notification_center)
        );
    }
    match v.source {
        EnumValue::Known(wire::PermissionSource::BundleRequired) => "Ouvrez l’application macOS installée pour recevoir des notifications natives.",
        EnumValue::Known(wire::PermissionSource::AndroidPendingQualification) => "Android/Fire : réception en arrière-plan à qualifier, sans Google Play Services obligatoire.",
        EnumValue::Known(wire::PermissionSource::Unsupported) => "Permissions OS indisponibles ; l’inbox reste distincte de l’affichage natif.",
        EnumValue::Known(wire::PermissionSource::Unspecified | wire::PermissionSource::MacOsUserNotifications | wire::PermissionSource::AndroidNotificationManager) | EnumValue::Unknown(_) => "Permission non admise",
    }.into()
}

fn setting(value: EnumValue<wire::Setting>) -> &'static str {
    match value {
        EnumValue::Known(wire::Setting::Enabled) => "activé",
        EnumValue::Known(wire::Setting::Disabled) => "désactivé",
        EnumValue::Known(wire::Setting::Unsupported) => "non disponible",
        EnumValue::Known(wire::Setting::Unknown | wire::Setting::Unspecified)
        | EnumValue::Unknown(_) => "inconnu",
    }
}
fn transport(value: &wire::Delivery) -> &'static str {
    match value.transport_acceptance {
        EnumValue::Known(wire::TransportAcceptance::AcceptedForegroundLan) => "Transport accepté",
        EnumValue::Known(wire::TransportAcceptance::ProviderRejected) => "Refus du fournisseur",
        EnumValue::Known(wire::TransportAcceptance::Unavailable) => "Transport indisponible",
        EnumValue::Known(wire::TransportAcceptance::Unspecified) | EnumValue::Unknown(_) => {
            "Transport non admis"
        }
    }
}
fn presentation(value: &wire::Delivery) -> &'static str {
    match value.presentation {
        EnumValue::Known(wire::Presentation::NotAttempted) => "Présentation non tentée",
        EnumValue::Known(wire::Presentation::OsAccepted) => {
            "Demande acceptée par l’OS · affichage non attesté"
        }
        EnumValue::Known(wire::Presentation::PresentInOsCenter) => "Remise attestée au centre OS",
        EnumValue::Known(wire::Presentation::OutcomeUnknown) => {
            "Résultat OS inconnu · aucun renvoi"
        }
        EnumValue::Known(wire::Presentation::PermissionDenied) => "Permission refusée",
        EnumValue::Known(wire::Presentation::Unavailable) => "Présentation indisponible",
        EnumValue::Known(wire::Presentation::Unspecified) | EnumValue::Unknown(_) => {
            "Présentation non admise"
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
        .filter_map(|entry| {
            let key = state.row_key(&admit::DeliveryId::parse(&entry.id.value).ok()?)?;
            let received = if entry.reception == wire::Reception::Received {
                "Reçu"
            } else {
                "Réception à acquitter"
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
                    "\nPrésentation au premier plan demandée"
                }
                os::ForegroundEvidence::SuppressedByDelegate => {
                    "\nPrésentation au premier plan suspendue"
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
            status: state.stage.label().into(),
            permission: permission(&state.permission).into(),
            notice: if state.notice.is_empty() {
                "Les messages sont propres à cette installation. La réception locale nécessite l’application au premier plan ; aucune réception permanente en arrière-plan n’est garantie.".into()
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
                os::Backend::MacOs => "Demander l’autorisation macOS",
                os::Backend::Android => "Configurer les notifications Android",
                os::Backend::Unavailable => "Notifications indisponibles",
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
    window.set_notification_rows(ModelRc::new(VecModel::from(rows)));
}
