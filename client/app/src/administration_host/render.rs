use super::*;
use crate::administration::IntegrationId;
use crate::{
    ProAdminActionRow, ProAdminChoice, ProAdminField, ProAdminFieldKind, ProAdminSection,
    ProAdminStage, TableColumn, TableRow,
};
use buffa::{EnumValue, Enumeration};
use home_wire_contract::{
    auth_contract::{AccountName, FactorCredentialName, InvitationName, McpKeyName, SessionName},
    home::{api::v2 as auth, api::v2 as obs},
};
use slint::{ModelRc, VecModel};

#[derive(Clone, PartialEq, Eq)]
pub(super) struct FactKey(String);
#[derive(Clone, PartialEq, Eq)]
pub(super) enum Target {
    Account(AccountName),
    Session(SessionName),
    Factor(FactorCredentialName),
    Key(McpKeyName),
    Invitation(InvitationName),
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
        "Opaque configuration · type not declared"
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
        EnumValue::Unknown(_) => "Unsupported".into(),
    }
}
pub(super) fn at(value: &buffa_types::google::protobuf::Timestamp) -> String {
    chrono::DateTime::from_timestamp(value.seconds, value.nanos as u32)
        .map(|date| date.format("%Y-%m-%d %H:%M UTC").to_string())
        .unwrap_or_else(|| "Date not provided".into())
}
fn role(value: EnumValue<auth::AccountRole>) -> &'static str {
    match value {
        EnumValue::Known(auth::AccountRole::Admin) => "Administrator",
        EnumValue::Known(auth::AccountRole::User) => "User",
        EnumValue::Known(auth::AccountRole::Unspecified) | EnumValue::Unknown(_) => {
            "Unsupported role"
        }
    }
}
fn gauge(value: &obs::ObservationGaugeMeasurement) -> String {
    match &value.result {
        Some(obs::observation_gauge_measurement::Result::Value(value)) => value.to_string(),
        Some(obs::observation_gauge_measurement::Result::NotMeasured(_)) => "Not measured".into(),
        Some(obs::observation_gauge_measurement::Result::Unavailable(_)) => "Unavailable".into(),
        None => "Not provided".into(),
    }
}

fn counter(value: &obs::ObservationCounterMeasurement) -> String {
    match &value.result {
        Some(obs::observation_counter_measurement::Result::Exact(value)) => value.to_string(),
        Some(obs::observation_counter_measurement::Result::Saturated(value)) => {
            format!("At least {}", value.lower_bound)
        }
        Some(obs::observation_counter_measurement::Result::NotMeasured(_)) => "Not measured".into(),
        Some(obs::observation_counter_measurement::Result::Unavailable(_)) => "Unavailable".into(),
        None => "Not provided".into(),
    }
}
fn lag(value: &obs::ObservationLagMeasurement) -> String {
    match &value.result {
        Some(obs::observation_lag_measurement::Result::SameEpochPositions(value)) => {
            format!("{value} positions")
        }
        Some(obs::observation_lag_measurement::Result::Invalidated(_)) => "Invalidated".into(),
        Some(obs::observation_lag_measurement::Result::EpochMismatch(_)) => {
            "Different epoch".into()
        }
        Some(obs::observation_lag_measurement::Result::Unavailable(_)) => "Unavailable".into(),
        None => "Not provided".into(),
    }
}
fn position(value: &obs::ObservationPositionMeasurement) -> String {
    match &value.result {
        Some(obs::observation_position_measurement::Result::Value(value)) => {
            format!("{} / {}", hex(&value.epoch), value.sequence)
        }
        Some(obs::observation_position_measurement::Result::Unavailable(_)) => "Unavailable".into(),
        None => "Not provided".into(),
    }
}
pub(super) fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|value| format!("{value:02x}")).collect()
}
fn next(value: &str) -> Option<PageToken> {
    if value.is_empty() {
        None
    } else {
        Some(PageToken::parse(value).expect("admitted page token"))
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
                "Session Account".into(),
                identity.account.as_str().into(),
            );
            push(
                fact("identity/session"),
                "Native Session".into(),
                identity.session.as_str().into(),
            );
            if let Some(profile) = profile {
                push(
                    fact("identity/name"),
                    "Display Name".into(),
                    profile.display_name.clone(),
                );
                push(
                    fact("identity/role"),
                    "Server-Provided Role".into(),
                    role(profile.role).into(),
                );
            }
        }
        Payload::Accounts(values) => {
            page = values.next.clone();
            for value in values {
                push(
                    Target::Account(
                        AccountName::try_from(value.name.as_str()).expect("admitted account"),
                    ),
                    value.display_name.clone(),
                    format!(
                        "{} · {} · {}",
                        value.login_name,
                        role(value.role),
                        name(value.access)
                    ),
                );
            }
        }
        Payload::Sessions(values) => {
            page = values.next.clone();
            for value in values {
                push(
                    Target::Session(
                        SessionName::try_from(value.name.as_str()).expect("admitted session"),
                    ),
                    if value.name == identity.session.as_str() {
                        "Current Session".into()
                    } else {
                        value.name.clone()
                    },
                    format!(
                        "Expires {}{}",
                        at(&value.expire_time),
                        if value.revoke_time.is_set() {
                            " · Revoked"
                        } else {
                            ""
                        }
                    ),
                );
            }
        }
        Payload::Factors(values) => {
            page = values.next.clone();
            for value in values {
                push(
                    Target::Factor(
                        FactorCredentialName::try_from(value.name.as_str())
                            .expect("admitted factor"),
                    ),
                    "Security Key".into(),
                    format!("{} · created {}", value.name, at(&value.create_time)),
                );
            }
        }
        Payload::MfaPolicy(value) => {
            push(
                fact("mfa/required"),
                "Administrator Second Factor".into(),
                if value.require_admin_factor {
                    "Required"
                } else {
                    "Optional"
                }
                .into(),
            );
            push(
                fact("mfa/version"),
                "Policy Revision".into(),
                value.etag.clone(),
            );
        }
        Payload::Keys(values) => {
            page = values.next.clone();
            for value in values {
                push(
                    Target::Key(
                        McpKeyName::try_from(value.name.as_str()).expect("admitted MCP key"),
                    ),
                    value.client_name.clone(),
                    format!(
                        "Expires {}{}",
                        at(&value.expire_time),
                        if value.revoke_time.is_set() {
                            " · Revoked"
                        } else {
                            ""
                        }
                    ),
                );
            }
        }
        Payload::Invitations(values) => {
            page = values.next.clone();
            for value in values {
                push(
                    Target::Invitation(
                        InvitationName::try_from(value.name.as_str()).expect("admitted invitation"),
                    ),
                    value.login_name.clone(),
                    format!(
                        "{} · {} · expires {}",
                        role(value.role),
                        if value.revoke_time.is_set() {
                            "Revoked"
                        } else if value.accept_time.is_set() {
                            "Accepted"
                        } else {
                            "Pending"
                        },
                        at(&value.expire_time)
                    ),
                );
            }
        }
        Payload::Policy { account, response } => {
            let detail = if response.origin == auth::AuthorizationPolicyOrigin::Default {
                "Default policy · no configurable permissions".into()
            } else {
                format!("Updated {}", at(&response.update_time))
            };
            push(
                fact(format!("policy/{}", account.as_str())),
                "Account Policy".into(),
                detail,
            );
        }
        Payload::Integrations(value) => {
            for row in &value.response.integration_instances {
                let type_label = value
                    .types
                    .iter()
                    .find(|kind| kind.name == row.integration_type)
                    .map_or(row.integration_type.as_str(), |kind| kind.type_key.as_str());
                push(
                    Target::Integration(
                        IntegrationId::parse(&row.name).expect("admitted integration"),
                    ),
                    row.instance_key.clone(),
                    format!(
                        "{} · {}",
                        integration_type(type_label),
                        name(row.desired_state)
                    ),
                );
            }
            if value.response.integration_instances.is_empty() {
                push(
                    fact("integrations/catalog"),
                    "Available Types".into(),
                    value
                        .types
                        .iter()
                        .map(|kind| kind.type_key.as_str())
                        .collect::<Vec<_>>()
                        .join(", "),
                );
            }
        }
        Payload::Integration(value) => {
            let row = &value.instance;
            let type_label = value
                .kind
                .as_ref()
                .map_or(row.integration_type.as_str(), |kind| kind.type_key.as_str());
            push(
                fact("integration/type"),
                "Integration Type".into(),
                integration_type(type_label).into(),
            );
            push(
                fact("integration/state"),
                "Requested State".into(),
                name(row.desired_state),
            );
            push(
                fact("integration/schema"),
                "Public Schema".into(),
                match &value.kind {
                    Ok(kind) => kind.schema_version.clone(),
                    Err(_) => "Schema unavailable".into(),
                },
            );
            match &value.health {
                Ok(health) => {
                    let report = match &health.report.result {
                        Some(obs::observation_integration_report_presence::Result::Present(
                            report,
                        )) => name(report.status),
                        Some(obs::observation_integration_report_presence::Result::NoReport(_)) => {
                            "No report".into()
                        }
                        None => "Not provided".into(),
                    };
                    let phase = match &health.supervision.result {
                        Some(obs::observation_integration_supervision::Result::Supervised(
                            value,
                        )) => name(value.phase),
                        Some(obs::observation_integration_supervision::Result::NotSupervised(
                            _,
                        )) => "Not supervised".into(),
                        None => "Not provided".into(),
                    };
                    push(fact("integration/health"), "Reported Health".into(), report);
                    push(fact("integration/supervision"), "Supervision".into(), phase);
                }
                Err(_) => push(
                    fact("integration/health"),
                    "Reported Health".into(),
                    "Health service unavailable".into(),
                ),
            }
        }
        Payload::Health(value) => {
            push(
                fact("health/server"),
                "Server".into(),
                match value.server.readiness {
                    EnumValue::Known(obs::ObservationServerReadiness::Ready) => "Ready",
                    EnumValue::Known(obs::ObservationServerReadiness::Starting) => "Starting",
                    EnumValue::Known(obs::ObservationServerReadiness::Draining) => "Stopping",
                    EnumValue::Known(obs::ObservationServerReadiness::CoreUnavailable) => {
                        "Unavailable"
                    }
                    EnumValue::Known(obs::ObservationServerReadiness::Unspecified)
                    | EnumValue::Unknown(_) => "Not provided",
                }
                .into(),
            );
            push(
                fact("health/durable"),
                "Persistent Storage".into(),
                name(value.durable.availability),
            );
            push(
                fact("health/recorder"),
                "Recorder".into(),
                match &value.recorder.result {
                    Some(obs::observation_recorder_health::Result::Observed(value)) => {
                        format!("{} · {}", name(value.phase), name(value.state))
                    }
                    Some(obs::observation_recorder_health::Result::NotInstalled(_)) => {
                        "Not installed".into()
                    }
                    Some(obs::observation_recorder_health::Result::Unavailable(_)) => {
                        "Unavailable".into()
                    }
                    None => "Not provided".into(),
                },
            );
            for pilot in &value.pilots {
                push(
                    fact(format!("health/pilot/{}", name(pilot.phase))),
                    format!("Integrations · {}", name(pilot.phase)),
                    pilot.instances.to_string(),
                );
            }
        }
        Payload::Metrics(value) => metrics(value, &mut push),
        Payload::Logs(value) => {
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
            page = next(&value.next_page_token);
        }
        Payload::Audits(value) => {
            for row in &value.rows {
                let (title, detail) = match &row.result {
                    Some(obs::observation_audit_row::Result::Authentication(value)) => (
                        name(value.action),
                        format!("{} · {}", at(&value.occurred_at), name(value.outcome)),
                    ),
                    Some(obs::observation_audit_row::Result::SecretAccess(value)) => (
                        "Secret access denied".into(),
                        format!("{} · {}", at(&value.occurred_at), name(value.reason)),
                    ),
                    Some(obs::observation_audit_row::Result::StoredMetadataInvalid(_)) => (
                        "Invalid historical metadata".into(),
                        "Values Not Exposed".into(),
                    ),
                    None => ("Entry not provided".into(), String::new()),
                };
                push(
                    fact(format!("audit/{}/{}", name(value.source), row.id)),
                    title,
                    detail,
                );
            }
            page = next(&value.next_page_token);
        }
    }
    (rows, page)
}

fn metrics(value: &obs::Metrics, push: &mut impl FnMut(Target, String, String)) {
    let mut metric = |key: String, label: String, value: String| push(fact(key), label, value);
    for queue in &value.queues {
        let group = name(queue.queue_class);
        match &queue.result {
            Some(obs::observation_queue_aggregate::Result::Installed(value)) => {
                for (key, label, value) in [
                    ("queues", "Active Queues", gauge(&value.active_queues)),
                    ("queued", "Pending entries", gauge(&value.queued_entries)),
                    ("capacity", "Entry Capacity", gauge(&value.capacity_entries)),
                    ("degraded", "Queues degraded", gauge(&value.degraded_queues)),
                    (
                        "bytes",
                        "Pending Accounted Bytes",
                        gauge(&value.queued_accounted_bytes),
                    ),
                    (
                        "capacity-bytes",
                        "Capacity measured in bytes",
                        gauge(&value.capacity_accounted_bytes),
                    ),
                    (
                        "inflight",
                        "Entries in progress",
                        gauge(&value.in_flight_entries),
                    ),
                    (
                        "pending",
                        "Accepted commands pending",
                        gauge(&value.pending_entries),
                    ),
                    (
                        "rejected",
                        "Total Rejections",
                        counter(&value.rejected_total),
                    ),
                    (
                        "lost",
                        "Total Lost Deliveries",
                        counter(&value.lost_deliveries_total),
                    ),
                    (
                        "coalesced",
                        "Total Coalesced Entries",
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
            Some(obs::observation_queue_aggregate::Result::NotInstalled(_)) => {
                metric(format!("queue/{group}"), group, "Not installed".into())
            }
            Some(obs::observation_queue_aggregate::Result::Unavailable(_)) => {
                metric(format!("queue/{group}"), group, "Unavailable".into())
            }
            None => (),
        }
    }
    for row in &value.queue_lags {
        let group = name(row.queue_class);
        let value = match &row.result {
            Some(obs::observation_queue_lag::Result::Installed(value)) => lag(value),
            Some(obs::observation_queue_lag::Result::NotInstalled(_)) => "Not installed".into(),
            Some(obs::observation_queue_lag::Result::Unavailable(_)) => "Unavailable".into(),
            None => "Not provided".into(),
        };
        metric(
            format!("lag/{group}"),
            format!("{group} · position lag"),
            value,
        );
    }
    match &value.recorder.result {
        Some(obs::observation_recorder_metrics::Result::Installed(value)) => {
            for (key, label, value) in [
                ("queued-bytes", "Queued Bytes", gauge(&value.queued_bytes)),
                (
                    "pending-bytes",
                    "Pending Bytes",
                    gauge(&value.pending_bytes),
                ),
                (
                    "capacity-bytes",
                    "Total Capacity in Bytes",
                    gauge(&value.total_capacity_bytes),
                ),
                (
                    "queue-capacity",
                    "Queue Capacity in Bytes",
                    gauge(&value.queue_capacity_bytes),
                ),
                ("queued", "Queued entries", gauge(&value.queued_entries)),
                ("retries", "Total Resumes", counter(&value.retries_total)),
                (
                    "lost",
                    "Total Lost Entries",
                    counter(&value.lost_entries_total),
                ),
                (
                    "excluded",
                    "Policy Exclusions",
                    counter(&value.policy_excluded_total),
                ),
                ("checkpoint", "Checkpoint", position(&value.checkpoint)),
                ("tail", "End of Log", position(&value.tail)),
                ("lag", "Position Lag", lag(&value.recorder_lag_positions)),
            ] {
                metric(
                    format!("recorder/{key}"),
                    format!("Recorder · {label}"),
                    value,
                );
            }
            for gap in &value.committed_gaps {
                let reason = name(gap.reason);
                for (key, label, value) in [
                    ("rows", "Confirmed Gaps", counter(&gap.rows_total)),
                    (
                        "known-lost",
                        "Known lower bound of losses",
                        counter(&gap.known_lost_lower_bound_total),
                    ),
                    (
                        "unknown",
                        "Gaps Without Known Counts",
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
        Some(obs::observation_recorder_metrics::Result::NotInstalled(_)) => {
            metric("recorder".into(), "Recorder".into(), "Not installed".into())
        }
        Some(obs::observation_recorder_metrics::Result::Unavailable(_)) => {
            metric("recorder".into(), "Recorder".into(), "Unavailable".into())
        }
        None => (),
    }
    metric(
        "actions/progress".into(),
        "Uncertain actions · progress".into(),
        counter(&value.actions.progress_indeterminate_total),
    );
    metric(
        "actions/recovered".into(),
        "Uncertain actions · persistent recovery records".into(),
        counter(&value.actions.durable_recovered_indeterminate_total),
    );
    for row in &value.actions.first_attempts {
        let ledger = name(row.ledger);
        metric(
            format!("actions/first/{ledger}"),
            format!("Initial uncertain attempts · {ledger}"),
            counter(&row.indeterminate_total),
        );
    }
    match &value.runs.result {
        Some(obs::observation_run_metrics::Result::Installed(value)) => {
            metric(
                "runs/preparing".into(),
                "Runs being prepared".into(),
                gauge(&value.preparing),
            );
            for kind in &value.kinds {
                let group = name(kind.kind);
                for (key, label, value) in [
                    ("active", "Active", gauge(&kind.active)),
                    ("pending", "Pending", gauge(&kind.pending)),
                    ("cancelled", "Cancelled", counter(&kind.cancelled_total)),
                    ("superseded", "Replaced", counter(&kind.superseded_total)),
                ] {
                    metric(
                        format!("runs/{group}/{key}"),
                        format!("{group} · {label}"),
                        value,
                    );
                }
            }
        }
        Some(obs::observation_run_metrics::Result::NotInstalled(_)) => {
            metric("runs".into(), "Runs".into(), "Not installed".into())
        }
        Some(obs::observation_run_metrics::Result::Unavailable(_)) => {
            metric("runs".into(), "Runs".into(), "Unavailable".into())
        }
        None => (),
    }
    for (key, label, value) in [
        ("rows", "Retained Rows", gauge(&value.journal.retained_rows)),
        (
            "bytes",
            "Accounted Bytes",
            gauge(&value.journal.charged_bytes),
        ),
        (
            "evicted",
            "Total Evictions",
            counter(&value.journal.evicted_total),
        ),
    ] {
        metric(format!("journal/{key}"), format!("Log · {label}"), value);
    }
    for cut in &value.cuts {
        let owner = name(cut.owner);
        metric(
            format!("cut/{owner}"),
            format!("Independent snapshot · {owner}"),
            at(&cut.captured_at),
        );
    }
}
fn log_message(value: EnumValue<obs::ObservationLogCode>) -> &'static str {
    match value {
        EnumValue::Known(obs::ObservationLogCode::ProcessConfigReadFailed) => {
            "Configuration reading denied"
        }
        EnumValue::Known(obs::ObservationLogCode::ProcessConfigAdmissionFailed) => {
            "Configuration rejected"
        }
        EnumValue::Known(obs::ObservationLogCode::ProcessRuntimeStartFailed) => {
            "Unable to start server"
        }
        EnumValue::Known(obs::ObservationLogCode::ProcessRuntimeFailed) => {
            "Runtime stopped on error"
        }
        EnumValue::Known(obs::ObservationLogCode::ProcessReady) => "Server ready",
        EnumValue::Known(obs::ObservationLogCode::ProcessStopped) => "Server stopped",
        EnumValue::Known(obs::ObservationLogCode::TransportInternalDiagnostic) => {
            "Internal transport diagnostic"
        }
        EnumValue::Known(obs::ObservationLogCode::MqttUnknownDiscovery) => "Unknown MQTT discovery",
        EnumValue::Known(obs::ObservationLogCode::MqttInvalidDiscovery) => "Invalid MQTT discovery",
        EnumValue::Known(obs::ObservationLogCode::MqttUnconfiguredSource) => {
            "MQTT source not configured"
        }
        EnumValue::Known(obs::ObservationLogCode::MqttUnsupportedEffect) => {
            "Unsupported MQTT effect"
        }
        EnumValue::Known(obs::ObservationLogCode::MqttUnsupportedColorMode) => {
            "Unsupported MQTT color mode"
        }
        EnumValue::Known(obs::ObservationLogCode::MqttInvalidState) => "Invalid MQTT state",
        EnumValue::Known(obs::ObservationLogCode::MqttInvalidAvailability) => {
            "Invalid MQTT availability"
        }
        EnumValue::Known(obs::ObservationLogCode::MqttRetirementFailed) => {
            "Unable to remove MQTT entry"
        }
        EnumValue::Known(obs::ObservationLogCode::MqttSnapshotOverflow) => "MQTT snapshot full",
        EnumValue::Known(obs::ObservationLogCode::MqttUnconfiguredActionAnchor) => {
            "MQTT action target not configured"
        }
        EnumValue::Known(obs::ObservationLogCode::MqttAmbiguousActionDevice) => {
            "Ambiguous MQTT action target"
        }
        EnumValue::Known(obs::ObservationLogCode::MqttActionOverflow) => "MQTT action queue full",
        EnumValue::Known(obs::ObservationLogCode::MqttInvalidAction) => "Invalid MQTT action",
        EnumValue::Known(obs::ObservationLogCode::PilotStarting) => "Starting Integration",
        EnumValue::Known(obs::ObservationLogCode::PilotRunning) => "Integration running",
        EnumValue::Known(obs::ObservationLogCode::PilotRetrying) => "Integration Recovery",
        EnumValue::Known(obs::ObservationLogCode::PilotDegraded) => "Integration degraded",
        EnumValue::Known(obs::ObservationLogCode::PilotStopped) => "Integration stopped",
        EnumValue::Known(obs::ObservationLogCode::PilotJoinTimeout) => "Integration stop timed out",
        EnumValue::Known(obs::ObservationLogCode::PilotTaskCapacity) => "Task capacity reached",
        EnumValue::Known(obs::ObservationLogCode::PilotFactoryFailed) => {
            "Unable to create integration"
        }
        EnumValue::Known(obs::ObservationLogCode::Unspecified) | EnumValue::Unknown(_) => {
            "Unsupported event"
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
        Stage::Loading => "Loading…",
        Stage::Ready => "Reading authorized",
        Stage::Empty => "No entries",
        Stage::Denied => "Access denied",
        Stage::Offline => "Connection interrupted",
        Stage::Unavailable => "Service unavailable",
        Stage::Unsupported => "Unsupported response",
        Stage::Invalid => "Response rejected",
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
            fields.push(("Item".into(), row.title.clone(), false));
            fields.push(("Details".into(), row.detail.clone(), false));
        }
        if let Some(payload) = state
            .private
            .pane(state.section)
            .and_then(|pane| pane.content.as_ref())
        {
            match payload{
                Payload::Policy{response,..}=>{fields.push(("Policy Source".into(),String::from_utf8(response.yaml_source.clone()).expect("admitted policy UTF-8"),true));},
                Payload::Integration(value)=>if let Some(home_wire_contract::home::api::v2::integration_instance::Configuration::Editable(value))=&value.instance.configuration{fields.push(("Public Settings".into(),String::from_utf8(value.yaml_source.clone()).expect("admitted integration UTF-8"),true));for reference in &value.secrets{fields.push((format!("Reference · {}",reference.field),reference.secret_key.clone(),false));}},
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
            "Hardware enrollment is unsupported on this device."
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
            label: "Item".into(),
            ..Default::default()
        },
        TableColumn {
            token: 2,
            label: "State and Details".into(),
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
        label: "Reload".into(),
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
            label: "Next Page".into(),
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
                description: "Data provided by the server".into(),
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
        label: "Close Details".into(),
        enabled: true,
        destructive: false,
    }])));
    window.set_admin_editor_visible(value.editor.is_some());
    window.set_admin_editable(false);
    window.set_admin_choices(ModelRc::default());
    window.set_admin_editor_title(format!("Details · {}", value.section.label()).into());
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
                        label: "Confirm".into(),
                        enabled: editor.submittable,
                        destructive: editor.destructive,
                    },
                    ProAdminActionRow {
                        action: ProAdminAction::Cancel,
                        label: "Cancel".into(),
                        enabled: true,
                        destructive: false,
                    },
                ]
            }),
        )));
    }
    if let Some((kind, secret)) = value.secret {
        let label = match kind {
            crate::administration::SecretKind::Invitation => "Invitation Token",
            crate::administration::SecretKind::McpKey => "MCP Key Secret",
            crate::administration::SecretKind::RecoveryCodes => "Recovery Codes",
        };
        window.set_admin_selected_token(1);
        window.set_admin_editor_visible(true);
        window.set_admin_editable(false);
        window.set_admin_choices(ModelRc::default());
        window.set_admin_editor_title(label.into());
        window.set_admin_editor_notice("Keep this secret now. After closing, it will no longer be shown and cannot be read again from the server.".into());
        window.set_admin_fields(ModelRc::new(VecModel::from(vec![ProAdminField {
            token: 1,
            label: label.into(),
            description: "One-time display linked to this session".into(),
            value: secret.as_str().into(),
            kind: ProAdminFieldKind::Password,
            editable: false,
        }])));
        window.set_admin_editor_actions(ModelRc::new(VecModel::from(vec![ProAdminActionRow {
            action: ProAdminAction::CloseSecret,
            label: "I Saved the Secret · Close".into(),
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
        let zero = obs::ObservationGaugeMeasurement {
            result: Some(obs::observation_gauge_measurement::Result::Value(0)),
            ..Default::default()
        };
        let unmeasured = obs::ObservationGaugeMeasurement {
            result: Some(obs::observation_gauge_measurement::Result::NotMeasured(
                Box::default(),
            )),
            ..Default::default()
        };
        assert_eq!(gauge(&zero), "0");
        assert_eq!(gauge(&unmeasured), "Not measured");
        assert_eq!(
            gauge(&obs::ObservationGaugeMeasurement::default()),
            "Not provided"
        );
    }
}
