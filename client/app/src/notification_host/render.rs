use super::*;
use crate::{NotificationPresentation, NotificationRow, OptionRow};
use buffa::EnumValue;
use slint::{ModelRc, VecModel};
pub(super) struct Projection {
    view: NotificationPresentation,
    rows: Vec<(String, String, bool)>,
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
    let label = match v.source {
        EnumValue::Known(wire::PermissionSource::BundleRequired) => {
            "Ouvrez l’application macOS installée pour recevoir des notifications natives."
        }
        EnumValue::Known(wire::PermissionSource::AndroidPendingQualification) => {
            "Android/Fire : réception en arrière-plan à qualifier, sans Google Play Services obligatoire."
        }
        EnumValue::Known(wire::PermissionSource::Unsupported) => {
            "Permissions OS indisponibles ; l’inbox reste distincte de l’affichage natif."
        }
        EnumValue::Known(wire::PermissionSource::MacOsUserNotifications) => match v.permission {
            EnumValue::Known(wire::Permission::NotRequested) => "Autorisation macOS non demandée",
            EnumValue::Known(wire::Permission::Denied) => {
                "Autorisation macOS refusée · modifiable dans les réglages du système"
            }
            EnumValue::Known(wire::Permission::Authorized) => "Autorisation macOS accordée",
            EnumValue::Known(wire::Permission::Provisional) => "Autorisation macOS provisoire",
            EnumValue::Known(wire::Permission::Ephemeral) => "Autorisation macOS temporaire",
            EnumValue::Known(wire::Permission::Unknown | wire::Permission::Unspecified)
            | EnumValue::Unknown(_) => "Autorisation OS inconnue",
        },
        EnumValue::Known(wire::PermissionSource::Unspecified) | EnumValue::Unknown(_) => {
            "Permission non admise"
        }
    };
    if v.source == wire::PermissionSource::MacOsUserNotifications {
        format!(
            "{label} · Bannières : {} · Centre OS : {}",
            setting(v.alerts),
            setting(v.notification_center)
        )
    } else {
        label.into()
    }
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
        .map(|entry| {
            let received = if entry.reception == wire::Reception::Received {
                "Reçu"
            } else {
                "Réception à acquitter"
            };
            (
                entry.title.clone(),
                format!(
                    "{}\n{} · {}\n{}",
                    entry.body,
                    transport(entry),
                    received,
                    presentation(entry)
                ),
                entry.read_state == wire::ReadState::Read,
            )
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
            can_permission: free
                && state.permission.source == wire::PermissionSource::MacOsUserNotifications
                && state.permission.permission == wire::Permission::NotRequested,
            can_revoke: free && registered,
            confirming: state.draft.is_some(),
            confirmation: state.draft.map_or("", state::Draft::label).into(),
        },
        rows,
    }
}
pub(super) fn apply(window: &HomewardWindow, projection: Projection) {
    let generation = projection.view.generation;
    let rows = projection
        .rows
        .into_iter()
        .enumerate()
        .map(|(index, (label, description, read))| NotificationRow {
            item: OptionRow {
                token: index as i32 + 1,
                generation,
                label: label.into(),
                description: description.into(),
                disabled: false,
            },
            read,
        })
        .collect::<Vec<_>>();
    window.set_notification_view(projection.view);
    window.set_notification_rows(ModelRc::new(VecModel::from(rows)));
}
