use super::*;
use crate::{
    ProAdminActionRow, ProAdminChoice, ProAdminField, ProAdminFieldKind, ProAdminStage,
    TableColumn, TableRow,
};
use buffa::EnumValue;
use home_client_platform::telemetry as os;
use slint::{ModelRc, VecModel};

pub(super) struct Projection {
    generation: i32,
    stage: ProAdminStage,
    status: String,
    notice: String,
    rows: Vec<(String, String)>,
    selected: i32,
    actions: Vec<ProAdminActionRow>,
    fields: Vec<ProAdminField>,
    choices: Vec<ProAdminChoice>,
    editor_actions: Vec<ProAdminActionRow>,
    editor: bool,
    editor_title: String,
}
pub(super) fn empty(generation: i32) -> Projection {
    Projection {
        generation,
        stage: ProAdminStage::Unavailable,
        status: "Partage désactivé".into(),
        notice: String::new(),
        rows: Vec::new(),
        selected: -1,
        actions: Vec::new(),
        fields: Vec::new(),
        choices: Vec::new(),
        editor_actions: Vec::new(),
        editor: false,
        editor_title: String::new(),
    }
}
fn command(
    action: ProAdminAction,
    label: &str,
    enabled: bool,
    destructive: bool,
) -> ProAdminActionRow {
    ProAdminActionRow {
        action,
        label: label.into(),
        enabled,
        destructive,
    }
}
fn field(token: i32, label: &str, value: String) -> ProAdminField {
    ProAdminField {
        token,
        label: label.into(),
        description: "".into(),
        value: value.into(),
        kind: ProAdminFieldKind::Plain,
        editable: false,
    }
}
fn enabled(value: bool) -> &'static str {
    if value { "Partagé" } else { "Non partagé" }
}
fn feature(value: EnumValue<wire::Feature>) -> &'static str {
    // Labels belong to the one native feature catalogue, not wire spellings.
    let platform = os::capabilities(os::Platform::MacOs);
    let mapped = super::native::capabilities(os::Platform::MacOs);
    mapped
        .iter()
        .position(|c| c.feature == value)
        .map_or("Non pris en charge", |index| {
            platform[index].feature.label()
        })
}
fn availability(value: EnumValue<wire::Availability>) -> &'static str {
    match value {
        EnumValue::Known(wire::Availability::Available) => "Disponible",
        EnumValue::Known(wire::Availability::Partial) => "Partiel",
        EnumValue::Known(wire::Availability::Unsupported) => "Non pris en charge",
        EnumValue::Known(wire::Availability::PendingQualification) => "À qualifier sur l’appareil",
        EnumValue::Known(wire::Availability::Unspecified) | EnumValue::Unknown(_) => "Non admis",
    }
}
fn permission(value: EnumValue<wire::Permission>) -> &'static str {
    match value {
        EnumValue::Known(wire::Permission::NotRequired) => "Autorisation OS non requise",
        EnumValue::Known(wire::Permission::NotRequested) => "Permission non demandée",
        EnumValue::Known(wire::Permission::Granted) => "Permission accordée",
        EnumValue::Known(wire::Permission::Denied) => "Permission refusée",
        EnumValue::Known(wire::Permission::Restricted) => "Permission restreinte",
        EnumValue::Known(wire::Permission::Unsupported) => "Permission non disponible",
        EnumValue::Known(wire::Permission::Unspecified) | EnumValue::Unknown(_) => {
            "Permission non admise"
        }
    }
}
fn source(value: EnumValue<wire::Source>) -> &'static str {
    match value {
        EnumValue::Known(wire::Source::MacOsPowerSources) => "Batterie macOS",
        EnumValue::Known(wire::Source::MacOsNetworkConfiguration) => "Route locale macOS",
        EnumValue::Known(wire::Source::AndroidBatteryManager) => "Batterie Android",
        EnumValue::Known(wire::Source::AndroidConnectivityManager) => "Route locale Android",
        EnumValue::Known(wire::Source::NativeLifecycle) => "Visibilité de l’application",
        EnumValue::Known(wire::Source::None) => "Aucune source qualifiée",
        EnumValue::Known(wire::Source::Unspecified) | EnumValue::Unknown(_) => "Source non admise",
    }
}
fn absent(value: EnumValue<wire::NoData>) -> &'static str {
    match value {
        EnumValue::Known(wire::NoData::Disabled) => "Non partagé",
        EnumValue::Known(wire::NoData::Unsupported) => "Non pris en charge",
        EnumValue::Known(wire::NoData::NoHardware) => "Sans batterie",
        EnumValue::Known(wire::NoData::Unavailable) => "Mesure indisponible",
        EnumValue::Known(wire::NoData::PermissionDenied) => "Refus de l’OS",
        EnumValue::Known(wire::NoData::Stale) => "Mesure périmée",
        EnumValue::Known(wire::NoData::Unspecified) | EnumValue::Unknown(_) => "Non admis",
    }
}
fn observation(feature: EnumValue<wire::Feature>, snapshot: &wire::Snapshot) -> String {
    match feature {
        EnumValue::Known(
            wire::Feature::BatteryLevel | wire::Feature::BatteryState | wire::Feature::Charging,
        ) => match &snapshot.battery.observation {
            Some(wire::battery_observation::Observation::NoData(n)) => absent(*n).into(),
            Some(wire::battery_observation::Observation::Value(b)) => {
                if feature == wire::Feature::BatteryLevel {
                    format!("{} %", b.percent)
                } else {
                    match b.state {
                        EnumValue::Known(wire::PowerState::Battery) => "Sur batterie",
                        EnumValue::Known(wire::PowerState::Charging) => "En charge",
                        EnumValue::Known(wire::PowerState::ExternalPower) => {
                            "Sur secteur, sans charge"
                        }
                        EnumValue::Known(wire::PowerState::Full) => "Batterie pleine",
                        EnumValue::Known(wire::PowerState::Unspecified) | EnumValue::Unknown(_) => {
                            "Non admis"
                        }
                    }
                    .into()
                }
            }
            None => "Aucune mesure".into(),
        },
        EnumValue::Known(wire::Feature::Connectivity) => match snapshot.connectivity.observation {
            Some(wire::connectivity_observation::Observation::NoData(n)) => absent(n).into(),
            Some(wire::connectivity_observation::Observation::Value(v)) => match v {
                EnumValue::Known(wire::Connectivity::LocallyReachable) => {
                    "Route locale joignable — Internet non vérifié"
                }
                EnumValue::Known(wire::Connectivity::ConnectionRequired) => "Connexion requise",
                EnumValue::Known(wire::Connectivity::NoLocalRoute) => "Aucune route locale",
                EnumValue::Known(wire::Connectivity::Unspecified) | EnumValue::Unknown(_) => {
                    "Non admis"
                }
            }
            .into(),
            None => "Aucune mesure".into(),
        },
        EnumValue::Known(wire::Feature::Activity) => match snapshot.activity.observation {
            Some(wire::activity_observation::Observation::NoData(n)) => absent(n).into(),
            Some(wire::activity_observation::Observation::Value(v)) => match v {
                EnumValue::Known(wire::AppActivity::Foreground) => {
                    "Application visible — présence humaine non déduite"
                }
                EnumValue::Known(wire::AppActivity::Unspecified) | EnumValue::Unknown(_) => {
                    "Non admis"
                }
            }
            .into(),
            None => "Aucune mesure".into(),
        },
        EnumValue::Known(
            wire::Feature::Focus
            | wire::Feature::DoNotDisturb
            | wire::Feature::Kiosk
            | wire::Feature::ScreenSaver
            | wire::Feature::Brightness
            | wire::Feature::Volume
            | wire::Feature::Location
            | wire::Feature::CameraMotion
            | wire::Feature::Notifications
            | wire::Feature::Unspecified,
        )
        | EnumValue::Unknown(_) => "Aucune collecte".into(),
    }
}
pub(super) fn project(state: &State) -> Projection {
    let mut p = empty(state.generation);
    if !state.visible || !state.active || !state.foreground {
        return p;
    }
    p.stage = match state.stage {
        Stage::Disconnected | Stage::Unsupported => ProAdminStage::Unsupported,
        Stage::Loading => ProAdminStage::Loading,
        Stage::Unregistered => ProAdminStage::Empty,
        Stage::Ready | Stage::Busy | Stage::Uncertain | Stage::Revoked | Stage::Expired => {
            ProAdminStage::Ready
        }
        Stage::Suspended | Stage::Offline => ProAdminStage::Offline,
        Stage::Denied => ProAdminStage::Denied,
        Stage::Invalid => ProAdminStage::Invalid,
    };
    p.status = if state.running {
        "Vérification en cours"
    } else {
        match state.stage {
            Stage::Disconnected => "Déconnecté",
            Stage::Loading => "Vérification du service",
            Stage::Unregistered => "Aucune installation — partage désactivé",
            Stage::Ready => {
                if state.gate == Gate::UserStopped {
                    "Collecte arrêtée localement"
                } else {
                    "Installation prête"
                }
            }
            Stage::Busy => "Opération en cours",
            Stage::Uncertain => "Résultat à vérifier",
            Stage::Suspended => "Application suspendue",
            Stage::Offline => "Hors ligne — collecte arrêtée",
            Stage::Denied => "Accès retiré",
            Stage::Revoked => "Installation révoquée",
            Stage::Expired => "Installation expirée",
            Stage::Unsupported => "Service non disponible",
            Stage::Invalid => "Réponse non admise",
        }
    }
    .into();
    p.notice = if state.context.is_some() {
        "Serveur de démonstration. Partage privé de cette installation ; localisation et caméra non collectées. ".to_owned()+&state.notice
    } else {
        state.notice.clone()
    };
    let idle = !state.running && state.pending.is_none();
    p.actions = vec![
        command(ProAdminAction::Refresh, "Actualiser", !state.running, false),
        command(
            ProAdminAction::Create,
            "Inscrire ce compagnon",
            idle && state.record.is_none() && state.context.is_some(),
            false,
        ),
        command(
            ProAdminAction::Enable,
            "Modifier le partage",
            idle && state.stage == Stage::Ready,
            false,
        ),
        command(
            ProAdminAction::Disable,
            "Arrêter la collecte locale",
            state.record.is_some(),
            false,
        ),
        command(
            ProAdminAction::Revoke,
            "Révoquer l’installation",
            idle && state.stage == Stage::Ready,
            true,
        ),
    ];
    let platform = if cfg!(target_os = "macos") {
        os::Platform::MacOs
    } else if cfg!(target_os = "android") {
        os::Platform::Android
    } else {
        os::Platform::Other
    };
    let capabilities = state.record.as_ref().map_or_else(
        || native::capabilities(platform),
        |r| r.device.capabilities.clone(),
    );
    let mut selected_detail = None;
    for (index, capability) in capabilities.into_iter().enumerate() {
        let value = state
            .record
            .as_ref()
            .and_then(|r| r.latest.as_option())
            .map_or_else(
                || "Aucune mesure retenue".into(),
                |s| {
                    let age = chrono::Utc::now()
                        .timestamp()
                        .saturating_sub(s.captured_at.seconds);
                    let maximum = state
                        .record
                        .as_ref()
                        .and_then(|r| admit::cadence_seconds(r.consent.cadence))
                        .unwrap_or(60)
                        * 2;
                    if age < 0 || age as u64 >= maximum {
                        "Mesure périmée".into()
                    } else {
                        observation(capability.feature, s)
                    }
                },
            );
        if index == state.selected {
            selected_detail = Some(ProAdminField {
                token: 14,
                label: "Mesure sélectionnée".into(),
                description: format!(
                    "{} · {} · {} · {}",
                    feature(capability.feature),
                    availability(capability.availability),
                    permission(capability.permission),
                    source(capability.source)
                )
                .into(),
                value: value.clone().into(),
                kind: ProAdminFieldKind::Multiline,
                editable: false,
            });
        }
        p.rows.push((feature(capability.feature).into(), value));
    }
    p.selected = state.selected.min(13) as i32;
    if let Some(record) = &state.record {
        p.fields = vec![
            field(
                10,
                "Modèle et OS",
                format!("{} · {}", record.device.model, record.device.os_version),
            ),
            field(
                11,
                "Consentement du serveur",
                format!(
                    "Batterie : {} · Réseau : {} · Activité : {}",
                    enabled(record.consent.battery),
                    enabled(record.consent.connectivity),
                    enabled(record.consent.activity)
                ),
            ),
            field(
                12,
                "Cadence",
                format!(
                    "{} secondes — aucun rattrapage hors ligne",
                    admit::cadence_seconds(record.consent.cadence).unwrap_or(0)
                ),
            ),
            field(
                13,
                "Coût de collecte sur cette session",
                format!(
                    "{} échantillons · {} µs cumulées dans le collecteur ; consommation batterie non mesurée",
                    state.samples,
                    state.sample_time.as_micros()
                ),
            ),
        ];
        if let Some(detail) = selected_detail {
            p.fields.insert(0, detail);
        }
        p.editor_title = "Informations de cette installation".into();
    }
    if let Some(draft) = &state.draft {
        p.editor = true;
        p.fields.clear();
        p.editor_title = match draft {
            Draft::Register(_) => "Confirmer l’inscription sans partage",
            Draft::Consent { .. } => "Choisir le partage autorisé",
            Draft::Revoke { .. } => "Confirmer la révocation",
        }
        .into();
        match draft {
            Draft::Register(_)=>p.fields.push(field(10,"Ce qui sera créé","Une identité d’installation et d’appareil, liées à votre session. Tous les partages restent désactivés.".into())),
            Draft::Revoke{..}=>p.fields.push(field(10,"Conséquence","Arrêt de publication et suppression des mesures retenues pour cette installation. Votre connexion reste ouverte.".into())),
            Draft::Consent{prior,desired,..}=>{
                for (choice_field,feature,value) in [(state::Field::Battery,wire::Feature::BatteryLevel,desired.battery),(state::Field::Connectivity,wire::Feature::Connectivity,desired.connectivity),(state::Field::Activity,wire::Feature::Activity,desired.activity)] {
                    let token=choice_field as i32;let label=choice_field.label();
                    p.fields.push(field(token,label,enabled(value).into()));
                    for (option,on) in [(1,true),(2,false)] {
                        p.choices.push(ProAdminChoice{token:option,field_token:token,label:format!("{} : {}",label,if on{"partager"}else{"ne pas partager"}).into(),selected:on==value,enabled:!state.running&&(!on||admit::can_collect(&prior.device,feature))});
                    }
                }
                p.fields.push(field(state::Field::Cadence as i32,state::Field::Cadence.label(),format!("{} secondes",admit::cadence_seconds(desired.cadence).unwrap_or(0))));
                for (token,cadence) in [(1,wire::Cadence::Seconds30),(2,wire::Cadence::Minute1),(3,wire::Cadence::Minutes5)] {
                    p.choices.push(ProAdminChoice{token,field_token:state::Field::Cadence as i32,label:format!("Toutes les {} secondes",admit::cadence_seconds(cadence.into()).expect("known cadence")).into(),selected:desired.cadence==cadence,enabled:!state.running});
                }
            },
        }
        p.editor_actions = vec![
            command(
                ProAdminAction::Save,
                "Confirmer",
                !state.running,
                matches!(draft, Draft::Revoke { .. }),
            ),
            command(ProAdminAction::Cancel, "Annuler", !state.running, false),
        ];
    }
    p
}
pub(super) fn apply(window: &HomewardWindow, p: Projection) {
    window.set_companion_generation(p.generation);
    window.set_companion_stage(p.stage);
    window.set_companion_status(p.status.into());
    window.set_companion_notice(p.notice.into());
    window.set_companion_active_index(p.selected);
    window.set_companion_actions(ModelRc::new(VecModel::from(p.actions)));
    window.set_companion_columns(ModelRc::new(VecModel::from(vec![
        TableColumn {
            token: 1,
            label: "Capacité".into(),
            ..Default::default()
        },
        TableColumn {
            token: 2,
            label: "Dernière mesure".into(),
            ..Default::default()
        },
    ])));
    window.set_companion_rows(ModelRc::new(VecModel::from(
        p.rows
            .into_iter()
            .enumerate()
            .map(|(index, (a, b))| TableRow {
                token: (index + 1) as i32,
                selected: index as i32 == p.selected,
                cells: ModelRc::new(VecModel::from(vec![a.into(), b.into()])),
                disabled: false,
            })
            .collect::<Vec<_>>(),
    )));
    window.set_companion_fields(ModelRc::new(VecModel::from(p.fields)));
    window.set_companion_choices(ModelRc::new(VecModel::from(p.choices)));
    window.set_companion_editor_actions(ModelRc::new(VecModel::from(p.editor_actions)));
    window.set_companion_editor_visible(p.editor);
    window.set_companion_editor_title(p.editor_title.into());
}
