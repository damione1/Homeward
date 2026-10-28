use super::*;
use crate::administration::IntegrationId;
use crate::{
    ProAdminActionRow, ProAdminChoice, ProAdminField, ProAdminFieldKind, ProAdminSection,
    ProAdminStage, TableColumn, TableRow,
};
use buffa::{EnumValue, Enumeration};
use home_wire_contract::{
    auth_contract::{AccountId, FactorCredentialId, InvitationId, McpKeyId, SessionId},
    home::{auth::v1 as auth, observability::v1 as obs},
};
use slint::{ModelRc, VecModel};

#[derive(Clone, PartialEq, Eq)]
pub(super) struct FactKey(String);
#[derive(Clone, PartialEq, Eq)]
pub(super) enum Target {
    Account(AccountId),
    Session(SessionId),
    Factor(FactorCredentialId),
    Key(McpKeyId),
    Invitation(InvitationId),
    Integration(IntegrationId),
    Configuration(super::configuration::Selection),
    Fact(FactKey),
}
#[derive(Clone)]
pub(super) struct Row {
    pub token: i32,
    pub target: Target,
    pub title: String,
    pub detail: String,
}
fn integration_type(value: &str) -> &str {
    if value.is_empty() {
        "Configuration opaque · type non déclaré"
    } else {
        value
    }
}
pub(super) fn fact(key: impl Into<String>) -> Target {
    Target::Fact(FactKey(key.into()))
}
fn name<E: Enumeration>(value: EnumValue<E>) -> String {
    match value {
        EnumValue::Known(value) => value.proto_name().into(),
        EnumValue::Unknown(_) => "Non pris en charge".into(),
    }
}
pub(super) fn at(value: &buffa_types::google::protobuf::Timestamp) -> String {
    chrono::DateTime::from_timestamp(value.seconds, value.nanos as u32)
        .map(|date| date.format("%Y-%m-%d %H:%M UTC").to_string())
        .unwrap_or_else(|| "Date non fournie".into())
}
fn role(value: EnumValue<auth::AccountRole>) -> &'static str {
    match value {
        EnumValue::Known(auth::AccountRole::Admin) => "Administrateur",
        EnumValue::Known(auth::AccountRole::User) => "Utilisateur",
        EnumValue::Known(auth::AccountRole::Unspecified) | EnumValue::Unknown(_) => {
            "Rôle non pris en charge"
        }
    }
}
fn gauge(value: &obs::GaugeMeasurement) -> String {
    match &value.result {
        Some(obs::gauge_measurement::Result::Value(value)) => value.to_string(),
        Some(obs::gauge_measurement::Result::NotMeasured(_)) => "Non mesuré".into(),
        Some(obs::gauge_measurement::Result::Unavailable(_)) => "Indisponible".into(),
        None => "Non fourni".into(),
    }
}

fn counter(value: &obs::CounterMeasurement) -> String {
    match &value.result {
        Some(obs::counter_measurement::Result::Exact(value)) => value.to_string(),
        Some(obs::counter_measurement::Result::Saturated(_)) => "Compteur saturé".into(),
        Some(obs::counter_measurement::Result::NotMeasured(_)) => "Non mesuré".into(),
        Some(obs::counter_measurement::Result::Unavailable(_)) => "Indisponible".into(),
        None => "Non fourni".into(),
    }
}
fn lag(value: &obs::LagMeasurement) -> String {
    match &value.result {
        Some(obs::lag_measurement::Result::SameEpochPositions(value)) => {
            format!("{value} positions")
        }
        Some(obs::lag_measurement::Result::Invalidated(_)) => "Invalidé".into(),
        Some(obs::lag_measurement::Result::EpochMismatch(_)) => "Epoch différent".into(),
        Some(obs::lag_measurement::Result::Unavailable(_)) => "Indisponible".into(),
        None => "Non fourni".into(),
    }
}
fn position(value: &obs::PositionMeasurement) -> String {
    match &value.result {
        Some(obs::position_measurement::Result::Value(value)) => {
            format!("{} / {}", hex(&value.epoch), value.sequence)
        }
        Some(obs::position_measurement::Result::Unavailable(_)) => "Indisponible".into(),
        None => "Non fourni".into(),
    }
}
pub(super) fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|value| format!("{value:02x}")).collect()
}
fn next(value: &obs::PageContinuation) -> Option<PageToken> {
    match &value.result {
        Some(obs::page_continuation::Result::NextPageToken(token)) => {
            Some(PageToken::parse(token).expect("admitted page token"))
        }
        Some(obs::page_continuation::Result::End(_)) | None => None,
    }
}

pub(super) fn rows(payload: &Payload, identity: &Identity) -> (Vec<Row>, Option<PageToken>) {
    let mut rows = Vec::new();
    let mut page = None;
    let mut push = |target: Target, title: String, detail: String| {
        let token = i32::try_from(rows.len() + 1).expect("bounded admin capture");
        rows.push(Row {
            token,
            target,
            title,
            detail,
        });
    };
    match payload {
        Payload::Configuration(value) => return super::configuration::rows(value),
        Payload::Personal { profile } => {
            push(
                fact("identity/account"),
                "Compte de la session".into(),
                identity.account.as_str().into(),
            );
            push(
                fact("identity/session"),
                "Session native".into(),
                identity.session.as_str().into(),
            );
            if let Some(profile) = profile {
                push(
                    fact("identity/name"),
                    "Nom d'affichage".into(),
                    profile.display_name.clone(),
                );
                push(
                    fact("identity/role"),
                    "Rôle fourni par le serveur".into(),
                    role(profile.role).into(),
                );
            }
        }
        Payload::Accounts(values) => {
            for value in values {
                push(
                    Target::Account(AccountId::try_from(&*value.id).expect("admitted account")),
                    value.display_name.clone(),
                    format!(
                        "{} · {} · {}",
                        value.login_name,
                        role(value.role),
                        name(value.status)
                    ),
                );
            }
        }
        Payload::Sessions(values) => {
            for value in values {
                push(
                    Target::Session(SessionId::try_from(&*value.id).expect("admitted session")),
                    if value.id.value == identity.session.as_str() {
                        "Session courante".into()
                    } else {
                        value.id.value.clone()
                    },
                    format!(
                        "Expiration {}{}",
                        at(&value.expires_at),
                        if value.revoked_at.is_set() {
                            " · Révoquée"
                        } else {
                            ""
                        }
                    ),
                );
            }
        }
        Payload::Factors(values) => {
            for value in values {
                push(
                    Target::Factor(
                        FactorCredentialId::try_from(&*value.id).expect("admitted factor"),
                    ),
                    "Clé de sécurité".into(),
                    format!("{} · créée {}", value.id.value, at(&value.created_at)),
                );
            }
        }
        Payload::MfaPolicy(value) => {
            push(
                fact("mfa/required"),
                "Second facteur administrateur".into(),
                if value.require_admin_factor {
                    "Obligatoire"
                } else {
                    "Facultatif"
                }
                .into(),
            );
            push(
                fact("mfa/version"),
                "Révision de la politique".into(),
                value.version.revision.to_string(),
            );
        }
        Payload::Keys(values) => {
            for value in values {
                push(
                    Target::Key(McpKeyId::try_from(&*value.id).expect("admitted MCP key")),
                    value.client_name.clone(),
                    format!(
                        "Expiration {}{}",
                        at(&value.expires_at),
                        if value.revoked_at.is_set() {
                            " · Révoquée"
                        } else {
                            ""
                        }
                    ),
                );
            }
        }
        Payload::Invitations(values) => {
            for value in values {
                push(
                    Target::Invitation(
                        InvitationId::try_from(&*value.id).expect("admitted invitation"),
                    ),
                    value.login_name.clone(),
                    format!(
                        "{} · {} · expiration {}",
                        role(value.role),
                        if value.revoked_at.is_set() {
                            "Révoquée"
                        } else if value.accepted_at.is_set() {
                            "Acceptée"
                        } else {
                            "En attente"
                        },
                        at(&value.expires_at)
                    ),
                );
            }
        }
        Payload::Policy { account, response } => {
            let detail = match &response.result {
                Some(auth::get_policy_response::Result::Present(value)) => format!(
                    "Révision {} · mise à jour {}",
                    value.version.revision,
                    at(&value.updated_at)
                ),
                Some(auth::get_policy_response::Result::Absent(_)) => {
                    "Aucune politique définie".into()
                }
                None => "Réponse non fournie".into(),
            };
            push(
                fact(format!("policy/{}", account.as_str())),
                "Politique du compte".into(),
                detail,
            );
        }
        Payload::Integrations(value) => {
            for row in &value.response.instances {
                push(
                    Target::Integration(
                        IntegrationId::parse(&row.integration_instance_id)
                            .expect("admitted integration"),
                    ),
                    row.integration_instance_id.clone(),
                    format!(
                        "{} · {}",
                        integration_type(&row.type_id),
                        name(row.desired_state)
                    ),
                );
            }
            if value.response.instances.is_empty() {
                push(
                    fact("integrations/catalog"),
                    "Types disponibles".into(),
                    value
                        .types
                        .iter()
                        .map(|kind| kind.type_id.as_str())
                        .collect::<Vec<_>>()
                        .join(", "),
                );
            }
        }
        Payload::Integration(value) => {
            let row = &value.response.instance;
            push(
                fact("integration/type"),
                "Type d'intégration".into(),
                integration_type(&row.type_id).into(),
            );
            push(
                fact("integration/state"),
                "État demandé".into(),
                name(row.desired_state),
            );
            push(
                fact("integration/schema"),
                "Schéma public".into(),
                match &value.kind {
                    Ok(kind) => kind.schema_version.clone(),
                    Err(_) => "Schéma indisponible".into(),
                },
            );
            match &value.health {
                Ok(health) => {
                    let report = match &health.report.result {
                        Some(obs::integration_report_presence::Result::Present(report)) => {
                            name(report.status)
                        }
                        Some(obs::integration_report_presence::Result::NoReport(_)) => {
                            "Aucun rapport".into()
                        }
                        None => "Non fourni".into(),
                    };
                    let phase = match &health.supervision.result {
                        Some(obs::integration_supervision::Result::Supervised(value)) => {
                            name(value.phase)
                        }
                        Some(obs::integration_supervision::Result::NotSupervised(_)) => {
                            "Non supervisé".into()
                        }
                        None => "Non fourni".into(),
                    };
                    push(fact("integration/health"), "Santé déclarée".into(), report);
                    push(fact("integration/supervision"), "Supervision".into(), phase);
                }
                Err(_) => push(
                    fact("integration/health"),
                    "Santé déclarée".into(),
                    "Service de santé indisponible".into(),
                ),
            }
        }
        Payload::Health(value) => {
            push(
                fact("health/server"),
                "Serveur".into(),
                match value.health.server.readiness {
                    EnumValue::Known(obs::ServerReadiness::Ready) => "Prêt",
                    EnumValue::Known(obs::ServerReadiness::Starting) => "Démarrage",
                    EnumValue::Known(obs::ServerReadiness::Draining) => "Arrêt en cours",
                    EnumValue::Known(obs::ServerReadiness::CoreUnavailable) => "Indisponible",
                    EnumValue::Known(obs::ServerReadiness::Unspecified) | EnumValue::Unknown(_) => {
                        "Non fourni"
                    }
                }
                .into(),
            );
            push(
                fact("health/durable"),
                "Stockage durable".into(),
                name(value.health.durable.availability),
            );
            push(
                fact("health/recorder"),
                "Enregistreur".into(),
                match &value.health.recorder.result {
                    Some(obs::recorder_health::Result::Observed(value)) => {
                        format!("{} · {}", name(value.phase), name(value.state))
                    }
                    Some(obs::recorder_health::Result::NotInstalled(_)) => "Non installé".into(),
                    Some(obs::recorder_health::Result::Unavailable(_)) => "Indisponible".into(),
                    None => "Non fourni".into(),
                },
            );
            for pilot in &value.health.pilots {
                push(
                    fact(format!("health/pilot/{}", name(pilot.phase))),
                    format!("Intégrations · {}", name(pilot.phase)),
                    pilot.instances.to_string(),
                );
            }
        }
        Payload::Metrics(value) => metrics(&value.metrics, &mut push),
        Payload::Logs(value) => match &value.result {
            Some(obs::query_runtime_logs_response::Result::Page(value)) => {
                for row in &value.rows {
                    push(
                        fact(format!(
                            "log/{}/{}",
                            hex(&value.process_epoch.value),
                            row.sequence
                        )),
                        log_message(row.event.code).into(),
                        format!(
                            "{} · {} · {}",
                            at(&row.event.occurred_at),
                            name(row.event.level),
                            name(row.event.component)
                        ),
                    );
                }
                page = next(&value.continuation);
            }
            Some(obs::query_runtime_logs_response::Result::CursorEvicted(value)) => push(
                fact("logs/evicted"),
                "Historique du journal tronqué".into(),
                format!("Première séquence conservée : {}", value.retained_from),
            ),
            Some(obs::query_runtime_logs_response::Result::StaleCursor(_)) => push(
                fact("logs/stale"),
                "Capture expirée".into(),
                "Rechargez la première page du journal.".into(),
            ),
            Some(obs::query_runtime_logs_response::Result::Unavailable(_)) => push(
                fact("logs/unavailable"),
                "Journal indisponible".into(),
                "Réessayez avec une nouvelle lecture.".into(),
            ),
            None => (),
        },
        Payload::Audits(value) => match &value.result {
            Some(obs::query_audits_response::Result::Page(value)) => {
                for row in &value.rows {
                    let (title, detail) = match &row.result {
                        Some(obs::audit_row::Result::Authentication(value)) => (
                            name(value.action),
                            format!("{} · {}", at(&value.occurred_at), name(value.outcome)),
                        ),
                        Some(obs::audit_row::Result::SecretAccess(value)) => (
                            "Accès à un secret refusé".into(),
                            format!("{} · {}", at(&value.occurred_at), name(value.reason)),
                        ),
                        Some(obs::audit_row::Result::StoredMetadataInvalid(_)) => (
                            "Métadonnées historiques invalides".into(),
                            "Valeurs non exposées".into(),
                        ),
                        None => ("Entrée non fournie".into(), String::new()),
                    };
                    push(
                        fact(format!("audit/{}/{}", name(value.source), row.id)),
                        title,
                        detail,
                    );
                }
                page = next(&value.continuation);
            }
            Some(obs::query_audits_response::Result::StaleCursor(_)) => push(
                fact("audits/stale"),
                "Capture expirée".into(),
                "Rechargez la première page des audits.".into(),
            ),
            Some(obs::query_audits_response::Result::Unavailable(_)) => push(
                fact("audits/unavailable"),
                "Audits indisponibles".into(),
                "Réessayez avec une nouvelle lecture.".into(),
            ),
            None => (),
        },
    }
    (rows, page)
}

fn metrics(value: &obs::MetricsSnapshot, push: &mut impl FnMut(Target, String, String)) {
    let mut metric = |key: String, label: String, value: String| push(fact(key), label, value);
    for queue in &value.queues {
        let group = name(queue.queue_class);
        match &queue.result {
            Some(obs::queue_aggregate::Result::Installed(value)) => {
                for (key, label, value) in [
                    ("queues", "Files actives", gauge(&value.active_queues)),
                    ("queued", "Entrées en attente", gauge(&value.queued_entries)),
                    (
                        "capacity",
                        "Capacité en entrées",
                        gauge(&value.capacity_entries),
                    ),
                    ("degraded", "Files dégradées", gauge(&value.degraded_queues)),
                    (
                        "bytes",
                        "Octets comptabilisés en attente",
                        gauge(&value.queued_accounted_bytes),
                    ),
                    (
                        "capacity-bytes",
                        "Capacité comptabilisée en octets",
                        gauge(&value.capacity_accounted_bytes),
                    ),
                    (
                        "inflight",
                        "Entrées en cours",
                        gauge(&value.in_flight_entries),
                    ),
                    (
                        "pending",
                        "Commandes admises en attente",
                        gauge(&value.pending_entries),
                    ),
                    ("rejected", "Refus cumulés", counter(&value.rejected_total)),
                    (
                        "lost",
                        "Livraisons perdues cumulées",
                        counter(&value.lost_deliveries_total),
                    ),
                    (
                        "coalesced",
                        "Regroupements cumulés",
                        counter(&value.coalesced_total),
                    ),
                ] {
                    metric(
                        format!("queue/{group}/{key}"),
                        format!("{group} · {label}"),
                        value,
                    );
                }
            }
            Some(obs::queue_aggregate::Result::NotInstalled(_)) => {
                metric(format!("queue/{group}"), group, "Non installé".into())
            }
            Some(obs::queue_aggregate::Result::Unavailable(_)) => {
                metric(format!("queue/{group}"), group, "Indisponible".into())
            }
            None => (),
        }
    }
    for row in &value.queue_lags {
        let group = name(row.queue_class);
        let value = match &row.result {
            Some(obs::queue_lag::Result::Installed(value)) => lag(value),
            Some(obs::queue_lag::Result::NotInstalled(_)) => "Non installé".into(),
            Some(obs::queue_lag::Result::Unavailable(_)) => "Indisponible".into(),
            None => "Non fourni".into(),
        };
        metric(
            format!("lag/{group}"),
            format!("{group} · retard en positions"),
            value,
        );
    }
    match &value.recorder.result {
        Some(obs::recorder_metrics::Result::Installed(value)) => {
            for (key, label, value) in [
                ("queued-bytes", "Octets en file", gauge(&value.queued_bytes)),
                (
                    "pending-bytes",
                    "Octets en attente",
                    gauge(&value.pending_bytes),
                ),
                (
                    "capacity-bytes",
                    "Capacité totale en octets",
                    gauge(&value.total_capacity_bytes),
                ),
                (
                    "queue-capacity",
                    "Capacité de file en octets",
                    gauge(&value.queue_capacity_bytes),
                ),
                ("queued", "Entrées en file", gauge(&value.queued_entries)),
                (
                    "retries",
                    "Reprises cumulées",
                    counter(&value.retries_total),
                ),
                (
                    "lost",
                    "Entrées perdues cumulées",
                    counter(&value.lost_entries_total),
                ),
                (
                    "excluded",
                    "Exclusions par politique",
                    counter(&value.policy_excluded_total),
                ),
                ("checkpoint", "Checkpoint", position(&value.checkpoint)),
                ("tail", "Fin de journal", position(&value.tail)),
                (
                    "lag",
                    "Retard en positions",
                    lag(&value.recorder_lag_positions),
                ),
            ] {
                metric(
                    format!("recorder/{key}"),
                    format!("Enregistreur · {label}"),
                    value,
                );
            }
            for gap in &value.committed_gaps {
                let reason = name(gap.reason);
                for (key, label, value) in [
                    ("rows", "Trous confirmés", counter(&gap.rows_total)),
                    (
                        "known-lost",
                        "Borne inférieure connue des pertes",
                        counter(&gap.known_lost_lower_bound_total),
                    ),
                    (
                        "unknown",
                        "Trous sans décompte connu",
                        counter(&gap.rows_without_known_count_total),
                    ),
                ] {
                    metric(
                        format!("recorder/gap/{reason}/{key}"),
                        format!("{reason} · {label}"),
                        value,
                    );
                }
            }
        }
        Some(obs::recorder_metrics::Result::NotInstalled(_)) => metric(
            "recorder".into(),
            "Enregistreur".into(),
            "Non installé".into(),
        ),
        Some(obs::recorder_metrics::Result::Unavailable(_)) => metric(
            "recorder".into(),
            "Enregistreur".into(),
            "Indisponible".into(),
        ),
        None => (),
    }
    metric(
        "actions/progress".into(),
        "Actions indéterminées · progression".into(),
        counter(&value.actions.progress_indeterminate_total),
    );
    metric(
        "actions/recovered".into(),
        "Actions indéterminées · récupérations durables".into(),
        counter(&value.actions.durable_recovered_indeterminate_total),
    );
    for row in &value.actions.first_attempts {
        let ledger = name(row.ledger);
        metric(
            format!("actions/first/{ledger}"),
            format!("Premières tentatives indéterminées · {ledger}"),
            counter(&row.indeterminate_total),
        );
    }
    match &value.runs.result {
        Some(obs::run_metrics::Result::Installed(value)) => {
            metric(
                "runs/preparing".into(),
                "Exécutions en préparation".into(),
                gauge(&value.preparing),
            );
            for kind in &value.kinds {
                let group = name(kind.kind);
                for (key, label, value) in [
                    ("active", "Actives", gauge(&kind.active)),
                    ("pending", "En attente", gauge(&kind.pending)),
                    ("cancelled", "Annulées", counter(&kind.cancelled_total)),
                    ("superseded", "Remplacées", counter(&kind.superseded_total)),
                ] {
                    metric(
                        format!("runs/{group}/{key}"),
                        format!("{group} · {label}"),
                        value,
                    );
                }
            }
        }
        Some(obs::run_metrics::Result::NotInstalled(_)) => {
            metric("runs".into(), "Exécutions".into(), "Non installé".into())
        }
        Some(obs::run_metrics::Result::Unavailable(_)) => {
            metric("runs".into(), "Exécutions".into(), "Indisponible".into())
        }
        None => (),
    }
    for (key, label, value) in [
        (
            "rows",
            "Lignes conservées",
            gauge(&value.journal.retained_rows),
        ),
        (
            "bytes",
            "Octets comptabilisés",
            gauge(&value.journal.charged_bytes),
        ),
        (
            "evicted",
            "Évictions cumulées",
            counter(&value.journal.evicted_total),
        ),
    ] {
        metric(
            format!("journal/{key}"),
            format!("Journal · {label}"),
            value,
        );
    }
    for cut in &value.cuts {
        let owner = name(cut.owner);
        metric(
            format!("cut/{owner}"),
            format!("Capture indépendante · {owner}"),
            at(&cut.captured_at),
        );
    }
}
fn log_message(value: EnumValue<obs::LogCode>) -> &'static str {
    match value {
        EnumValue::Known(obs::LogCode::ProcessConfigReadFailed) => {
            "Lecture de configuration refusée"
        }
        EnumValue::Known(obs::LogCode::ProcessConfigAdmissionFailed) => "Configuration non admise",
        EnumValue::Known(obs::LogCode::ProcessRuntimeStartFailed) => {
            "Démarrage du serveur impossible"
        }
        EnumValue::Known(obs::LogCode::ProcessRuntimeFailed) => "Arrêt du runtime sur erreur",
        EnumValue::Known(obs::LogCode::ProcessReady) => "Serveur prêt",
        EnumValue::Known(obs::LogCode::ProcessStopped) => "Serveur arrêté",
        EnumValue::Known(obs::LogCode::TransportInternalDiagnostic) => {
            "Diagnostic interne du transport"
        }
        EnumValue::Known(obs::LogCode::MqttUnknownDiscovery) => "Découverte MQTT inconnue",
        EnumValue::Known(obs::LogCode::MqttInvalidDiscovery) => "Découverte MQTT invalide",
        EnumValue::Known(obs::LogCode::MqttUnconfiguredSource) => "Source MQTT non configurée",
        EnumValue::Known(obs::LogCode::MqttUnsupportedEffect) => "Effet MQTT non pris en charge",
        EnumValue::Known(obs::LogCode::MqttUnsupportedColorMode) => {
            "Mode couleur MQTT non pris en charge"
        }
        EnumValue::Known(obs::LogCode::MqttInvalidState) => "État MQTT invalide",
        EnumValue::Known(obs::LogCode::MqttInvalidAvailability) => "Disponibilité MQTT invalide",
        EnumValue::Known(obs::LogCode::MqttRetirementFailed) => "Retrait MQTT impossible",
        EnumValue::Known(obs::LogCode::MqttSnapshotOverflow) => "Capture MQTT saturée",
        EnumValue::Known(obs::LogCode::MqttUnconfiguredActionAnchor) => {
            "Cible d'action MQTT non configurée"
        }
        EnumValue::Known(obs::LogCode::MqttAmbiguousActionDevice) => "Cible d'action MQTT ambiguë",
        EnumValue::Known(obs::LogCode::MqttActionOverflow) => "File d'actions MQTT saturée",
        EnumValue::Known(obs::LogCode::MqttInvalidAction) => "Action MQTT invalide",
        EnumValue::Known(obs::LogCode::PilotStarting) => "Démarrage d'intégration",
        EnumValue::Known(obs::LogCode::PilotRunning) => "Intégration en cours d'exécution",
        EnumValue::Known(obs::LogCode::PilotRetrying) => "Reprise d'intégration",
        EnumValue::Known(obs::LogCode::PilotDegraded) => "Intégration dégradée",
        EnumValue::Known(obs::LogCode::PilotStopped) => "Intégration arrêtée",
        EnumValue::Known(obs::LogCode::PilotJoinTimeout) => "Délai d'arrêt d'intégration dépassé",
        EnumValue::Known(obs::LogCode::PilotTaskCapacity) => "Capacité de tâches atteinte",
        EnumValue::Known(obs::LogCode::PilotFactoryFailed) => "Création d'intégration impossible",
        EnumValue::Known(obs::LogCode::Unspecified) | EnumValue::Unknown(_) => {
            "Événement non pris en charge"
        }
    }
}

pub(super) fn failure(error: ReadError) -> Stage {
    match error {
        ReadError::Session(error) => {
            let _ = error;
            Stage::Denied
        }
        ReadError::Transport(kind) => match kind {
            home_client_transport::FailureKind::NetworkInterrupted
            | home_client_transport::FailureKind::Deadline => Stage::Offline,
            home_client_transport::FailureKind::Rpc(code) => {
                if code == home_client_transport::RpcCode::PermissionDenied
                    || code == home_client_transport::RpcCode::Unauthenticated
                {
                    Stage::Denied
                } else if code == home_client_transport::RpcCode::Unimplemented {
                    Stage::Unsupported
                } else {
                    Stage::Unavailable
                }
            }
            home_client_transport::FailureKind::InvalidConfiguration
            | home_client_transport::FailureKind::UntrustedCertificate
            | home_client_transport::FailureKind::WrongServerName
            | home_client_transport::FailureKind::TlsRejected => Stage::Invalid,
            home_client_transport::FailureKind::Cancelled => Stage::Loading,
        },
        ReadError::Invalid => Stage::Invalid,
        ReadError::Unsupported => Stage::Unsupported,
        ReadError::Cancelled => Stage::Loading,
    }
}
fn stage(value: &Stage) -> ProAdminStage {
    match value {
        Stage::Loading => ProAdminStage::Loading,
        Stage::Ready => ProAdminStage::Ready,
        Stage::Empty => ProAdminStage::Empty,
        Stage::Denied => ProAdminStage::Denied,
        Stage::Offline => ProAdminStage::Offline,
        Stage::Unavailable => ProAdminStage::Unavailable,
        Stage::Unsupported => ProAdminStage::Unsupported,
        Stage::Invalid => ProAdminStage::Invalid,
    }
}
fn status(value: &Stage) -> &'static str {
    match value {
        Stage::Loading => "Chargement…",
        Stage::Ready => "Lecture autorisée",
        Stage::Empty => "Aucune entrée",
        Stage::Denied => "Accès refusé",
        Stage::Offline => "Connexion interrompue",
        Stage::Unavailable => "Service indisponible",
        Stage::Unsupported => "Réponse non prise en charge",
        Stage::Invalid => "Réponse refusée",
    }
}
pub(super) struct Projection {
    source: Option<(SessionCoordinator, Generation)>,
    active: bool,
    generation: i32,
    section: Section,
    stage: Stage,
    sections: Vec<Section>,
    rows: Vec<Row>,
    selected: Option<Target>,
    next: bool,
    fields: Vec<(String, String, bool)>,
    commands: Vec<forms::Command>,
    configuration_actions: Vec<ProAdminActionRow>,
    editor: Option<Editor>,
    secret: Option<(
        crate::administration::SecretKind,
        zeroize::Zeroizing<String>,
    )>,
}
struct Editor {
    actions: Option<Vec<ProAdminActionRow>>,
    title: &'static str,
    notice: &'static str,
    editable: bool,
    submittable: bool,
    destructive: bool,
    fields: Vec<ProAdminField>,
    choices: Vec<ProAdminChoice>,
}
impl Projection {
    pub fn new(state: &State, source: Option<(SessionCoordinator, Generation)>) -> Self {
        let administrator = state
            .private
            .authority()
            .is_some_and(|authority| authority.administrator());
        let mut fields = Vec::new();
        if let Some(target) = &state.selected
            && let Some(row) = state.rows.iter().find(|row| &row.target == target)
        {
            fields.push(("Élément".into(), row.title.clone(), false));
            fields.push(("Détails".into(), row.detail.clone(), false));
        }
        if let Some(payload) = state
            .private
            .pane(state.section)
            .and_then(|pane| pane.content.as_ref())
        {
            match payload{
                Payload::Policy{response,..}=>if let Some(auth::get_policy_response::Result::Present(value))=&response.result{fields.push(("Source de la politique".into(),String::from_utf8(value.yaml_source.clone()).expect("admitted policy UTF-8"),true));},
                Payload::Integration(value)=>if let Some(home_wire_contract::home::integrationadmin::v1::integration_instance::Configuration::Editable(value))=&value.response.instance.configuration{fields.push(("Paramètres publics".into(),String::from_utf8(value.yaml_source.clone()).expect("admitted integration UTF-8"),true));for reference in &value.secrets{fields.push((format!("Référence · {}",reference.field),reference.name.clone(),false));}},
                Payload::Personal{..}|Payload::Sessions(_)|Payload::Factors(_)|Payload::MfaPolicy(_)|Payload::Keys(_)|Payload::Accounts(_)|Payload::Invitations(_)|Payload::Integrations(_)|Payload::Health(_)|Payload::Metrics(_)|Payload::Logs(_)|Payload::Audits(_)|Payload::Configuration(_)=>(),
            }
        }
        Self {
            source,
            active: state.active,
            generation: state.generation,
            section: state.section,
            stage: state.stage.clone(),
            sections: Section::ALL
                .into_iter()
                .filter(|section| !section.administrative() || administrator)
                .collect(),
            rows: state.rows.clone(),
            selected: state.selected.clone(),
            next: state.next.is_some(),
            fields,
            commands: forms::available(state),
            configuration_actions: if state.section == Section::Configuration {
                state.configuration.actions()
            } else {
                Vec::new()
            },
            editor: if state.section == Section::Configuration && state.configuration.opened() {
                Some(Editor {
                    actions: Some(state.configuration.editor_actions()),
                    title: state.configuration.title(),
                    notice: state.configuration.notice(),
                    editable: state.configuration.editable(),
                    submittable: false,
                    destructive: true,
                    fields: state.configuration.fields(),
                    choices: state.configuration.choices(),
                })
            } else {
                state.private.draft().map(|draft| Editor {
                    actions: None,
                    title: draft.title(),
                    notice: draft.notice,
                    editable: draft.editable(),
                    submittable: draft.state == crate::administration::MutationState::Draft,
                    destructive: draft.destructive(),
                    fields: draft.fields(),
                    choices: draft.choices(),
                })
            },
            secret: state.private.secret_kind().zip(
                state
                    .private
                    .secret()
                    .map(|value| zeroize::Zeroizing::new(value.into())),
            ),
        }
    }
}
pub(super) fn clear(window: &HomewardWindow) {
    window.set_admin_rows(ModelRc::default());
    window.set_admin_fields(ModelRc::default());
    window.set_admin_sections(ModelRc::default());
    window.set_admin_actions(ModelRc::default());
    window.set_admin_editor_actions(ModelRc::default());
    window.set_admin_choices(ModelRc::default());
    window.set_admin_editor_visible(false);
    window.set_admin_editable(false);
    window.set_admin_editor_notice("".into());
    window.set_admin_editor_title("".into());
    window.set_admin_stage(ProAdminStage::Loading);
    window.set_admin_active_index(-1);
    window.set_admin_selected_token(0);
}
pub(super) fn render(window: &HomewardWindow, value: Projection) {
    if !value.active
        || !value
            .source
            .as_ref()
            .is_some_and(|(session, generation)| session.is_current(*generation))
    {
        clear(window);
        return;
    }
    window.set_admin_generation(value.generation);
    window.set_admin_title(value.section.label().into());
    window.set_admin_stage(stage(&value.stage));
    window.set_admin_status(status(&value.stage).into());
    window.set_admin_notice(
        if value.section == Section::Factors {
            "L'enrôlement matériel n'est pas pris en charge sur cet appareil."
        } else {
            ""
        }
        .into(),
    );
    window.set_admin_sections(ModelRc::new(VecModel::from(
        value
            .sections
            .into_iter()
            .map(|section| ProAdminSection {
                token: i32::try_from(
                    Section::ALL
                        .iter()
                        .position(|candidate| *candidate == section)
                        .expect("known section")
                        + 1,
                )
                .expect("bounded sections"),
                generation: value.generation,
                label: section.label().into(),
                selected: section == value.section,
            })
            .collect::<Vec<_>>(),
    )));
    window.set_admin_columns(ModelRc::new(VecModel::from(vec![
        TableColumn {
            token: 1,
            label: "Élément".into(),
            ..Default::default()
        },
        TableColumn {
            token: 2,
            label: "État et détails".into(),
            ..Default::default()
        },
    ])));
    window.set_admin_active_index(
        value
            .selected
            .as_ref()
            .and_then(|target| value.rows.iter().position(|row| &row.target == target))
            .and_then(|index| i32::try_from(index).ok())
            .unwrap_or(-1),
    );
    window.set_admin_selected_token(
        value
            .selected
            .as_ref()
            .and_then(|target| value.rows.iter().find(|row| &row.target == target))
            .map_or(1, |row| row.token),
    );
    window.set_admin_rows(ModelRc::new(VecModel::from(
        value
            .rows
            .into_iter()
            .map(|row| TableRow {
                token: row.token,
                selected: value.selected.as_ref() == Some(&row.target),
                cells: ModelRc::new(VecModel::from(vec![row.title.into(), row.detail.into()])),
                disabled: false,
            })
            .collect::<Vec<_>>(),
    )));
    let mut actions = vec![ProAdminActionRow {
        action: ProAdminAction::Refresh,
        label: "Recharger".into(),
        enabled: true,
        destructive: false,
    }];
    actions.extend(value.commands.into_iter().map(|command| ProAdminActionRow {
        action: command.action(),
        label: command.label().into(),
        enabled: true,
        destructive: command.destructive(),
    }));
    actions.extend(value.configuration_actions);
    if value.next {
        actions.push(ProAdminActionRow {
            action: ProAdminAction::More,
            label: "Page suivante".into(),
            enabled: true,
            destructive: false,
        });
    }
    window.set_admin_actions(ModelRc::new(VecModel::from(actions)));
    window.set_admin_fields(ModelRc::new(VecModel::from(
        value
            .fields
            .into_iter()
            .enumerate()
            .map(|(index, (label, value, multiline))| ProAdminField {
                token: i32::try_from(index + 1).expect("bounded fields"),
                label: label.into(),
                description: "Donnée fournie par le serveur".into(),
                value: value.into(),
                kind: if multiline {
                    ProAdminFieldKind::Multiline
                } else {
                    ProAdminFieldKind::Plain
                },
                editable: false,
            })
            .collect::<Vec<_>>(),
    )));
    window.set_admin_editor_actions(ModelRc::new(VecModel::from(vec![ProAdminActionRow {
        action: ProAdminAction::Cancel,
        label: "Fermer les détails".into(),
        enabled: true,
        destructive: false,
    }])));
    window.set_admin_editor_visible(value.editor.is_some());
    window.set_admin_editable(false);
    window.set_admin_choices(ModelRc::default());
    window.set_admin_editor_title(format!("Détails · {}", value.section.label()).into());
    window.set_admin_editor_notice("".into());
    if let Some(editor) = value.editor {
        window.set_admin_selected_token(1);
        window.set_admin_editable(editor.editable);
        window.set_admin_editor_title(editor.title.into());
        window.set_admin_editor_notice(editor.notice.into());
        window.set_admin_fields(ModelRc::new(VecModel::from(editor.fields)));
        window.set_admin_choices(ModelRc::new(VecModel::from(editor.choices)));
        window.set_admin_editor_actions(ModelRc::new(VecModel::from(
            editor.actions.unwrap_or_else(|| {
                vec![
                    ProAdminActionRow {
                        action: ProAdminAction::Save,
                        label: "Confirmer".into(),
                        enabled: editor.submittable,
                        destructive: editor.destructive,
                    },
                    ProAdminActionRow {
                        action: ProAdminAction::Cancel,
                        label: "Annuler".into(),
                        enabled: true,
                        destructive: false,
                    },
                ]
            }),
        )));
    }
    if let Some((kind, secret)) = value.secret {
        let label = match kind {
            crate::administration::SecretKind::Invitation => "Jeton d’invitation",
            crate::administration::SecretKind::McpKey => "Secret de la clé MCP",
            crate::administration::SecretKind::RecoveryCodes => "Codes de secours",
        };
        window.set_admin_selected_token(1);
        window.set_admin_editor_visible(true);
        window.set_admin_editable(false);
        window.set_admin_choices(ModelRc::default());
        window.set_admin_editor_title(label.into());
        window.set_admin_editor_notice("Conservez ce secret maintenant. Après fermeture, il ne sera plus affiché et ne pourra pas être relu depuis le serveur.".into());
        window.set_admin_fields(ModelRc::new(VecModel::from(vec![ProAdminField {
            token: 1,
            label: label.into(),
            description: "Affichage ponctuel lié à cette session".into(),
            value: secret.as_str().into(),
            kind: ProAdminFieldKind::Password,
            editable: false,
        }])));
        window.set_admin_editor_actions(ModelRc::new(VecModel::from(vec![ProAdminActionRow {
            action: ProAdminAction::CloseSecret,
            label: "J’ai conservé le secret · fermer".into(),
            enabled: true,
            destructive: false,
        }])));
    }
}

#[cfg(test)]
mod presence_tests {
    use super::*;
    #[test]
    fn unavailable_and_unmeasured_metrics_are_distinct_from_a_measured_zero() {
        let zero = obs::GaugeMeasurement {
            result: Some(obs::gauge_measurement::Result::Value(0)),
            ..Default::default()
        };
        let unmeasured = obs::GaugeMeasurement {
            result: Some(obs::gauge_measurement::Result::NotMeasured(Box::default())),
            ..Default::default()
        };
        assert_eq!(gauge(&zero), "0");
        assert_eq!(gauge(&unmeasured), "Non mesuré");
        assert_eq!(gauge(&obs::GaugeMeasurement::default()), "Non fourni");
    }
}
