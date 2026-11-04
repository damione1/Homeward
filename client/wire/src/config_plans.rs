//! Optional inert configuration plans: representation admission only. A receipt
//! does not authenticate its holder or grant activation/physical authority.
use crate::home::api::v2 as wire;
use buffa::EnumValue;
use std::collections::BTreeSet;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PlanAdmissionError {
    Invalid,
}

pub enum PlanRequest<'a> {
    Build(&'a wire::CreateConfigurationPlanRequest),
    Revalidate(&'a wire::RevalidateConfigurationPlanRequest),
    Get(&'a wire::GetConfigurationPlanRequest),
    List(&'a wire::ListConfigurationPlansRequest),
}
pub enum PlanResponse<'a> {
    Build(&'a wire::ConfigurationPlan),
    Revalidate(&'a wire::ConfigurationPlan),
    Get(&'a wire::ConfigurationPlan),
    List(&'a wire::ListConfigurationPlansResponse),
}
pub struct AdmittedPlanRequest<'a>(PlanRequest<'a>);
pub struct AdmittedPlanResponse<'a>(PlanResponse<'a>);
impl<'a> AdmittedPlanRequest<'a> {
    pub fn wire(&self) -> &PlanRequest<'a> {
        &self.0
    }
}
impl<'a> AdmittedPlanResponse<'a> {
    pub fn wire(&self) -> &PlanResponse<'a> {
        &self.0
    }
}
pub(crate) fn label(value: &str, max: usize) -> bool {
    !value.is_empty()
        && value.len() <= max
        && value.trim() == value
        && !value.contains(['\0', '\r', '\n'])
}
fn hash(value: &[u8]) -> bool {
    value.len() == 32 && value.iter().any(|byte| *byte != 0)
}
pub(crate) fn family(
    value: EnumValue<wire::ConfigurationPlanSourceFamily>,
) -> Option<wire::ConfigurationPlanSourceFamily> {
    match value {
        EnumValue::Known(
            value @ (wire::ConfigurationPlanSourceFamily::Operational
            | wire::ConfigurationPlanSourceFamily::DeclarativeV1
            | wire::ConfigurationPlanSourceFamily::DeclarativeV2
            | wire::ConfigurationPlanSourceFamily::Authorization
            | wire::ConfigurationPlanSourceFamily::Ownership
            | wire::ConfigurationPlanSourceFamily::Dashboard
            | wire::ConfigurationPlanSourceFamily::Automation
            | wire::ConfigurationPlanSourceFamily::Script
            | wire::ConfigurationPlanSourceFamily::Scene),
        ) => Some(value),
        EnumValue::Known(wire::ConfigurationPlanSourceFamily::Unspecified)
        | EnumValue::Unknown(_) => None,
    }
}
pub(crate) fn candidate(value: &wire::ConfigurationPlanCandidate) -> bool {
    if !value.__buffa_unknown_fields.is_empty()
        || family(value.family).is_none()
        || !label(&value.identity, 255)
        || !label(&value.entry, 4096)
        || !(1..=64).contains(&value.files.len())
    {
        return false;
    }
    let mut seen = BTreeSet::new();
    let mut bytes = 0usize;
    for file in &value.files {
        if !file.__buffa_unknown_fields.is_empty()
            || !label(&file.logical_path, 4096)
            || file.exact_source.is_empty()
            || !seen.insert(file.logical_path.as_str())
        {
            return false;
        }
        bytes += file.exact_source.len();
        if bytes > 1_048_576 {
            return false;
        }
    }
    seen.contains(value.entry.as_str())
}
pub fn admit_plan_request(
    value: PlanRequest<'_>,
) -> Result<AdmittedPlanRequest<'_>, PlanAdmissionError> {
    let valid = match &value {
        PlanRequest::Build(v) => {
            crate::configuration_contract::requested_resource_id(&v.configuration_plan_id)
                && v.__buffa_unknown_fields.is_empty()
                && (v.request_id.is_empty()
                    || home_client_model::api::RequestId::try_from(v.request_id.as_str()).is_ok())
                && crate::configuration_contract::home_name(&v.parent)
                && v.configuration_plan.as_option().is_some_and(|plan| {
                    plan.__buffa_unknown_fields.is_empty()
                        && plan.candidate.as_option().is_some_and(candidate)
                })
        }
        PlanRequest::Get(v) => v.__buffa_unknown_fields.is_empty() && plan_name(&v.name),
        PlanRequest::List(v) => {
            v.__buffa_unknown_fields.is_empty()
                && crate::configuration_contract::home_name(&v.parent)
                && v.page_size >= 0
                && v.page_token.len() <= 1200
        }
        PlanRequest::Revalidate(v) => {
            v.__buffa_unknown_fields.is_empty()
                && (v.request_id.is_empty()
                    || home_client_model::api::RequestId::try_from(v.request_id.as_str()).is_ok())
                && v.candidate.as_option().is_some_and(candidate)
                && plan_name(&v.name)
                && !v.etag.is_empty()
                && v.etag.len() <= 128
        }
    };
    if valid {
        Ok(AdmittedPlanRequest(value))
    } else {
        Err(PlanAdmissionError::Invalid)
    }
}
fn schema(family: wire::ConfigurationPlanSourceFamily, value: &str) -> bool {
    match family {
        wire::ConfigurationPlanSourceFamily::Operational => value == "homeward.operational/1",
        wire::ConfigurationPlanSourceFamily::DeclarativeV1 => value == "ha-declarative-1",
        wire::ConfigurationPlanSourceFamily::DeclarativeV2 => value == "ha-declarative-2",
        wire::ConfigurationPlanSourceFamily::Authorization => value == "homeward.authorization/1",
        wire::ConfigurationPlanSourceFamily::Ownership => value == "homeward.heating-ownership/1",
        wire::ConfigurationPlanSourceFamily::Dashboard => value == "homeward-dashboard-1",
        wire::ConfigurationPlanSourceFamily::Automation
        | wire::ConfigurationPlanSourceFamily::Script => value == "ha-logic-1",
        wire::ConfigurationPlanSourceFamily::Scene => value == "ha-scene-1",
        wire::ConfigurationPlanSourceFamily::Unspecified => false,
    }
}
fn effect(
    family: wire::ConfigurationPlanSourceFamily,
    value: EnumValue<wire::ConfigurationPlanEffect>,
) -> bool {
    match value {
        EnumValue::Known(wire::ConfigurationPlanEffect::None) => true,
        EnumValue::Known(wire::ConfigurationPlanEffect::Configuration) => matches!(
            family,
            wire::ConfigurationPlanSourceFamily::Operational
                | wire::ConfigurationPlanSourceFamily::DeclarativeV1
                | wire::ConfigurationPlanSourceFamily::DeclarativeV2
        ),
        EnumValue::Known(wire::ConfigurationPlanEffect::HumanRights) => {
            family == wire::ConfigurationPlanSourceFamily::Authorization
        }
        EnumValue::Known(wire::ConfigurationPlanEffect::TechnicalOwnership) => matches!(
            family,
            wire::ConfigurationPlanSourceFamily::Ownership
                | wire::ConfigurationPlanSourceFamily::Operational
        ),
        EnumValue::Known(wire::ConfigurationPlanEffect::Dashboard) => {
            family == wire::ConfigurationPlanSourceFamily::Dashboard
        }
        EnumValue::Known(wire::ConfigurationPlanEffect::RunTargets) => matches!(
            family,
            wire::ConfigurationPlanSourceFamily::Automation
                | wire::ConfigurationPlanSourceFamily::Script
                | wire::ConfigurationPlanSourceFamily::Scene
        ),
        EnumValue::Known(wire::ConfigurationPlanEffect::Unspecified) | EnumValue::Unknown(_) => {
            false
        }
    }
}
fn base(value: &wire::ConfigurationPlanBase) -> bool {
    if !value.__buffa_unknown_fields.is_empty() {
        return false;
    }
    match value.kind {
        EnumValue::Known(wire::ConfigurationPlanBaseKind::ActiveSql) => {
            value.revision.is_some_and(|v| v > 0)
                && value.source_sha256.as_ref().is_some_and(|v| hash(v))
        }
        EnumValue::Known(wire::ConfigurationPlanBaseKind::ImmutableReference) => {
            value.revision.is_none() && value.source_sha256.as_ref().is_some_and(|v| hash(v))
        }
        EnumValue::Known(wire::ConfigurationPlanBaseKind::NoActiveDocument) => {
            value.revision.is_none() && value.source_sha256.is_none()
        }
        EnumValue::Known(wire::ConfigurationPlanBaseKind::Unspecified) | EnumValue::Unknown(_) => {
            false
        }
    }
}
fn field(value: &str) -> bool {
    matches!(
        value,
        "android_tv"
            | "sonos"
            | "ecobee"
            | "disabled_integrations"
            | "remote_calendar"
            | "external_worker_bindings"
            | "external_worker_processes"
            | "mqtt"
            | "neviweb"
            | "hilo"
            | "energy"
            | "derived_power"
            | "solar"
            | "workday"
            | "external_workers"
            | "launch_ignored"
            | "input_boolean"
            | "input_number"
            | "counter"
            | "input_button"
            | "input_datetime"
            | "input_select"
            | "input_text"
            | "group"
            | "human_policy"
            | "technical_ownership"
            | "dashboard_views"
            | "dashboard_strategy"
            | "run_triggers"
            | "run_conditions"
            | "run_actions"
            | "run_policy"
            | "run_variables"
            | "run_metadata"
            | "scene_members"
            | "scene_metadata"
            | "definition"
    )
}
fn diagnostic(value: EnumValue<wire::ConfigurationPlanDiagnosticCode>) -> bool {
    match value {
        EnumValue::Known(
            wire::ConfigurationPlanDiagnosticCode::InvalidSchema
            | wire::ConfigurationPlanDiagnosticCode::ExcludedReference
            | wire::ConfigurationPlanDiagnosticCode::UnsupportedSource
            | wire::ConfigurationPlanDiagnosticCode::Capacity
            | wire::ConfigurationPlanDiagnosticCode::BaseUnavailable,
        ) => true,
        EnumValue::Known(wire::ConfigurationPlanDiagnosticCode::Unspecified)
        | EnumValue::Unknown(_) => false,
    }
}
fn plan_name(value: &str) -> bool {
    let Some((parent, id)) = value.split_once("/configurationPlans/") else {
        return false;
    };
    crate::configuration_contract::home_name(parent)
        && !id.is_empty()
        && id.len() <= 63
        && !id.starts_with('-')
        && !id.ends_with('-')
        && id
            .bytes()
            .all(|v| v.is_ascii_lowercase() || v.is_ascii_digit() || v == b'-')
}
fn plan(value: &wire::ConfigurationPlan) -> bool {
    let Some(created) = value.create_time.as_option() else {
        return false;
    };
    let Some(updated) = value.update_time.as_option() else {
        return false;
    };
    let Some(expires) = value.expire_time.as_option() else {
        return false;
    };
    if (updated.seconds, updated.nanos) < (created.seconds, created.nanos)
        || (expires.seconds, expires.nanos) <= (created.seconds, created.nanos)
        || (updated.seconds, updated.nanos) > (expires.seconds, expires.nanos)
    {
        return false;
    }
    let Some(family) = family(value.family) else {
        return false;
    };
    if !value.__buffa_unknown_fields.is_empty()
        || !plan_name(&value.name)
        || !label(&value.etag, 256)
        || value.candidate.is_set()
        || !value
            .create_time
            .as_option()
            .is_some_and(crate::configuration_contract::timestamp)
        || !value
            .update_time
            .as_option()
            .is_some_and(crate::configuration_contract::timestamp)
        || !value
            .expire_time
            .as_option()
            .is_some_and(crate::configuration_contract::timestamp)
        || home_client_model::auth::AccountName::try_from(value.author.as_str()).is_err()
        || !label(&value.identity, 255)
        || !schema(family, &value.schema_version)
        || !hash(&value.source_sha256)
        || !value.base.as_option().is_some_and(base)
        || value.generation_revision < 1
        || !hash(&value.manifest_sha256)
        || !hash(&value.observed_workspace_sha256)
        || !effect(family, value.effect)
        || value.application_available
        || value.changes.len() > 64
        || value.diagnostics.len() > 5
        || !value.diagnostics.iter().all(|v| diagnostic(*v))
    {
        return false;
    }
    let mut previous = "";
    let mut maximum: EnumValue<wire::ConfigurationPlanEffect> =
        wire::ConfigurationPlanEffect::None.into();
    for change in &value.changes {
        if !change.__buffa_unknown_fields.is_empty()
            || !field(&change.field)
            || (((change.field == "android_tv" || change.field == "sonos")
                || change.field == "ecobee"
                || change.field == "remote_calendar"
                || change.field == "disabled_integrations")
                && family != wire::ConfigurationPlanSourceFamily::Operational)
            || change.field.as_str() <= previous
            || !effect(family, change.effect)
            || change.effect == wire::ConfigurationPlanEffect::None
            || change.effect.to_i32() > value.effect.to_i32()
            || !(0..=4096).contains(&change.references_added)
            || !(0..=4096).contains(&change.references_removed)
        {
            return false;
        }
        previous = &change.field;
        if change.effect.to_i32() > maximum.to_i32() {
            maximum = change.effect;
        }
        match change.operation {
            EnumValue::Known(
                wire::ConfigurationPlanChangeOperation::Added
                | wire::ConfigurationPlanChangeOperation::Removed
                | wire::ConfigurationPlanChangeOperation::Replaced,
            ) => {}
            EnumValue::Known(wire::ConfigurationPlanChangeOperation::Unspecified)
            | EnumValue::Unknown(_) => {
                return false;
            }
        }
    }
    if value.semantic_sha256.is_some() && value.effect != maximum {
        return false;
    }
    match value.state {
        EnumValue::Known(wire::ConfigurationPlanState::Admitted) => {
            value.semantic_sha256.as_ref().is_some_and(|v| hash(v))
                && value.diagnostics.is_empty()
                && (44..=4200).contains(&value.receipt.len())
                && (value.changes.is_empty()
                    == (value.effect == wire::ConfigurationPlanEffect::None))
        }
        EnumValue::Known(wire::ConfigurationPlanState::Rejected) => {
            value.semantic_sha256.is_none()
                && value.changes.is_empty()
                && !value.diagnostics.is_empty()
                && value.effect != wire::ConfigurationPlanEffect::None
                && (44..=4200).contains(&value.receipt.len())
        }
        EnumValue::Known(wire::ConfigurationPlanState::Stale) => {
            value.receipt.is_empty()
                && ((value.semantic_sha256.as_ref().is_some_and(|v| hash(v))
                    && value.diagnostics.is_empty())
                    || (value.semantic_sha256.is_none() && !value.diagnostics.is_empty()))
        }
        EnumValue::Known(wire::ConfigurationPlanState::Unspecified) | EnumValue::Unknown(_) => {
            false
        }
    }
}
pub fn admit_plan_response(
    value: PlanResponse<'_>,
) -> Result<AdmittedPlanResponse<'_>, PlanAdmissionError> {
    let valid = match &value {
        PlanResponse::Build(v) => {
            v.__buffa_unknown_fields.is_empty()
                && plan(v)
                && v.state != wire::ConfigurationPlanState::Stale
        }
        PlanResponse::Revalidate(v) | PlanResponse::Get(v) => {
            v.__buffa_unknown_fields.is_empty() && plan(v)
        }
        PlanResponse::List(v) => {
            v.__buffa_unknown_fields.is_empty()
                && v.configuration_plans.len() <= 100
                && v.next_page_token.len() <= 1200
                && v.configuration_plans.iter().all(plan)
        }
    };
    if valid {
        Ok(AdmittedPlanResponse(value))
    } else {
        Err(PlanAdmissionError::Invalid)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn nha053_future_values_and_nested_fields_are_not_operational() {
        let candidate = wire::ConfigurationPlanCandidate {
            family: wire::ConfigurationPlanSourceFamily::Dashboard.into(),
            identity: "dashboard.preview".into(),
            entry: "candidate.yaml".into(),
            files: vec![wire::ConfigurationPlanSourceFile {
                logical_path: "candidate.yaml".into(),
                exact_source: b"views: []".to_vec(),
                ..Default::default()
            }],
            ..Default::default()
        };
        let mut request = wire::CreateConfigurationPlanRequest {
            parent: "homes/synthetic".into(),
            configuration_plan: wire::ConfigurationPlan {
                candidate: candidate.into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        assert!(admit_plan_request(PlanRequest::Build(&request)).is_ok());
        request
            .configuration_plan
            .as_option_mut()
            .unwrap()
            .candidate
            .as_option_mut()
            .unwrap()
            .family = EnumValue::Unknown(999);
        assert!(admit_plan_request(PlanRequest::Build(&request)).is_err());
    }
    #[test]
    fn nha053_absent_revision_is_not_zero_or_invented_active() {
        let mut value = wire::ConfigurationPlanBase {
            kind: wire::ConfigurationPlanBaseKind::NoActiveDocument.into(),
            ..Default::default()
        };
        assert!(base(&value));
        value.revision = Some(0);
        assert!(!base(&value));
        value.kind = wire::ConfigurationPlanBaseKind::ActiveSql.into();
        value.source_sha256 = Some(vec![1; 32]);
        assert!(!base(&value));
        value.revision = Some(1);
        assert!(base(&value));
    }
    #[test]
    fn nha053_effect_families_never_collapse() {
        assert!(effect(
            wire::ConfigurationPlanSourceFamily::Authorization,
            wire::ConfigurationPlanEffect::HumanRights.into()
        ));
        assert!(!effect(
            wire::ConfigurationPlanSourceFamily::Dashboard,
            wire::ConfigurationPlanEffect::HumanRights.into()
        ));
        assert!(!effect(
            wire::ConfigurationPlanSourceFamily::Automation,
            wire::ConfigurationPlanEffect::Dashboard.into()
        ));
        assert!(!effect(
            wire::ConfigurationPlanSourceFamily::Ownership,
            EnumValue::Unknown(999)
        ));
    }
}

#[cfg(test)]
mod remote_calendar_tests {
    use super::*;

    #[test]
    fn nha053_remote_calendar_response_refuses_private_paths_and_layout_effect() {
        let plan = wire::ConfigurationPlan {
            name: "homes/synthetic/configurationPlans/p-synthetic".into(),
            etag: "synthetic-plan-etag".into(),
            create_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000000,
                ..Default::default()
            }
            .into(),
            update_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000000,
                ..Default::default()
            }
            .into(),
            expire_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000600,
                ..Default::default()
            }
            .into(),

            state: wire::ConfigurationPlanState::Admitted.into(),
            author: "homes/test/accounts/admin".into(),
            family: wire::ConfigurationPlanSourceFamily::Operational.into(),
            identity: "operational.main".into(),
            schema_version: "homeward.operational/1".into(),
            source_sha256: vec![1; 32],
            semantic_sha256: Some(vec![2; 32]),
            base: wire::ConfigurationPlanBase {
                kind: wire::ConfigurationPlanBaseKind::NoActiveDocument.into(),
                ..Default::default()
            }
            .into(),
            generation_revision: 1,
            manifest_sha256: vec![3; 32],
            observed_workspace_sha256: vec![4; 32],
            effect: wire::ConfigurationPlanEffect::Configuration.into(),
            changes: vec![wire::ConfigurationPlanChange {
                effect: wire::ConfigurationPlanEffect::Configuration.into(),
                operation: wire::ConfigurationPlanChangeOperation::Added.into(),
                field: "remote_calendar".into(),
                references_added: 1,
                ..Default::default()
            }],
            receipt: "a".repeat(80),
            ..Default::default()
        };
        let mut response = plan;
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_ok());
        let admitted_author = response.author.clone();
        for invalid_author in [
            "server-admitted-admin",
            "homes/test/sessions/admin",
            " homes/test/accounts/admin",
        ] {
            response.author = invalid_author.into();
            assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        }
        response.author = admitted_author;
        response.changes[0].field = "remote_calendar.url".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        response.changes[0].field = "remote_calendar.auth.bearer_secret".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = &mut response;
        plan.changes[0].field = "remote_calendar".into();
        plan.changes[0].effect = wire::ConfigurationPlanEffect::Dashboard.into();
        plan.effect = wire::ConfigurationPlanEffect::Dashboard.into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = &mut response;
        plan.family = wire::ConfigurationPlanSourceFamily::Dashboard.into();
        plan.identity = "dashboard.preview".into();
        plan.schema_version = "homeward-dashboard-1".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = &mut response;
        plan.family = wire::ConfigurationPlanSourceFamily::Operational.into();
        plan.identity = "operational.main".into();
        plan.schema_version = "homeward.operational/1".into();
        plan.changes[0].effect = wire::ConfigurationPlanEffect::Configuration.into();
        plan.effect = wire::ConfigurationPlanEffect::Configuration.into();
        plan.application_available = true;
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
    }
}

#[cfg(test)]
mod integration_selection_tests {
    use super::*;

    #[test]
    fn nha053_integration_selection_response_refuses_private_instances_and_cross_family_effect() {
        let plan = wire::ConfigurationPlan {
            name: "homes/synthetic/configurationPlans/p-synthetic".into(),
            etag: "synthetic-plan-etag".into(),
            create_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000000,
                ..Default::default()
            }
            .into(),
            update_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000000,
                ..Default::default()
            }
            .into(),
            expire_time: buffa_types::google::protobuf::Timestamp {
                seconds: 1700000600,
                ..Default::default()
            }
            .into(),

            state: wire::ConfigurationPlanState::Admitted.into(),
            author: "homes/test/accounts/admin".into(),
            family: wire::ConfigurationPlanSourceFamily::Operational.into(),
            identity: "operational.main".into(),
            schema_version: "homeward.operational/1".into(),
            source_sha256: vec![1; 32],
            semantic_sha256: Some(vec![2; 32]),
            base: wire::ConfigurationPlanBase {
                kind: wire::ConfigurationPlanBaseKind::NoActiveDocument.into(),
                ..Default::default()
            }
            .into(),
            generation_revision: 1,
            manifest_sha256: vec![3; 32],
            observed_workspace_sha256: vec![4; 32],
            effect: wire::ConfigurationPlanEffect::Configuration.into(),
            changes: vec![wire::ConfigurationPlanChange {
                effect: wire::ConfigurationPlanEffect::Configuration.into(),
                operation: wire::ConfigurationPlanChangeOperation::Added.into(),
                field: "disabled_integrations".into(),
                references_added: 1,
                ..Default::default()
            }],
            receipt: "a".repeat(80),
            ..Default::default()
        };
        let mut response = plan;
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_ok());
        let baseline = response.clone();
        for (family, schema, effect) in [
            (
                wire::ConfigurationPlanSourceFamily::DeclarativeV1,
                "ha-declarative-1",
                wire::ConfigurationPlanEffect::Configuration,
            ),
            (
                wire::ConfigurationPlanSourceFamily::DeclarativeV2,
                "ha-declarative-2",
                wire::ConfigurationPlanEffect::Configuration,
            ),
            (
                wire::ConfigurationPlanSourceFamily::Authorization,
                "homeward.authorization/1",
                wire::ConfigurationPlanEffect::HumanRights,
            ),
            (
                wire::ConfigurationPlanSourceFamily::Ownership,
                "homeward.heating-ownership/1",
                wire::ConfigurationPlanEffect::TechnicalOwnership,
            ),
            (
                wire::ConfigurationPlanSourceFamily::Dashboard,
                "homeward-dashboard-1",
                wire::ConfigurationPlanEffect::Dashboard,
            ),
            (
                wire::ConfigurationPlanSourceFamily::Automation,
                "ha-logic-1",
                wire::ConfigurationPlanEffect::RunTargets,
            ),
            (
                wire::ConfigurationPlanSourceFamily::Script,
                "ha-logic-1",
                wire::ConfigurationPlanEffect::RunTargets,
            ),
            (
                wire::ConfigurationPlanSourceFamily::Scene,
                "ha-scene-1",
                wire::ConfigurationPlanEffect::RunTargets,
            ),
        ] {
            let mut candidate = baseline.clone();
            candidate.family = family.into();
            candidate.schema_version = schema.into();
            candidate.effect = effect.into();
            candidate.changes[0].effect = effect.into();
            let other = candidate;
            assert!(admit_plan_response(PlanResponse::Build(&other)).is_err());
        }

        response.changes[0].field = "disabled_integrations[0]".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        response.changes[0].field = "disabled_integrations.nha053-private-selection-worker".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = &mut response;
        plan.changes[0].field = "disabled_integrations".into();
        plan.changes[0].effect = wire::ConfigurationPlanEffect::Dashboard.into();
        plan.effect = wire::ConfigurationPlanEffect::Dashboard.into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = &mut response;
        plan.family = wire::ConfigurationPlanSourceFamily::Dashboard.into();
        plan.identity = "dashboard.preview".into();
        plan.schema_version = "homeward-dashboard-1".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = &mut response;
        plan.family = wire::ConfigurationPlanSourceFamily::Operational.into();
        plan.identity = "operational.main".into();
        plan.schema_version = "homeward.operational/1".into();
        plan.changes[0].effect = wire::ConfigurationPlanEffect::Configuration.into();
        plan.effect = wire::ConfigurationPlanEffect::Configuration.into();
        plan.application_available = true;
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
    }
}

#[cfg(test)]
mod ecobee_tests {
    use super::*;

    #[test]
    fn native_integration_response_refuses_private_settings_and_cross_family_effect() {
        for native_field in ["ecobee", "sonos", "android_tv"] {
            let plan = wire::ConfigurationPlan {
                name: "homes/synthetic/configurationPlans/p-synthetic".into(),
                etag: "synthetic-plan-etag".into(),
                create_time: buffa_types::google::protobuf::Timestamp {
                    seconds: 1700000000,
                    ..Default::default()
                }
                .into(),
                update_time: buffa_types::google::protobuf::Timestamp {
                    seconds: 1700000000,
                    ..Default::default()
                }
                .into(),
                expire_time: buffa_types::google::protobuf::Timestamp {
                    seconds: 1700000600,
                    ..Default::default()
                }
                .into(),

                state: wire::ConfigurationPlanState::Admitted.into(),
                author: "homes/test/accounts/admin".into(),
                family: wire::ConfigurationPlanSourceFamily::Operational.into(),
                identity: "operational.main".into(),
                schema_version: "homeward.operational/1".into(),
                source_sha256: vec![1; 32],
                semantic_sha256: Some(vec![2; 32]),
                base: wire::ConfigurationPlanBase {
                    kind: wire::ConfigurationPlanBaseKind::NoActiveDocument.into(),
                    ..Default::default()
                }
                .into(),
                generation_revision: 1,
                manifest_sha256: vec![3; 32],
                observed_workspace_sha256: vec![4; 32],
                effect: wire::ConfigurationPlanEffect::Configuration.into(),
                changes: vec![wire::ConfigurationPlanChange {
                    effect: wire::ConfigurationPlanEffect::Configuration.into(),
                    operation: wire::ConfigurationPlanChangeOperation::Added.into(),
                    field: native_field.into(),
                    references_added: 1,
                    ..Default::default()
                }],
                receipt: "a".repeat(80),
                ..Default::default()
            };
            let mut response = plan;
            assert!(admit_plan_response(PlanResponse::Build(&response)).is_ok());
            let baseline = response.clone();
            for (family, schema, effect) in [
                (
                    wire::ConfigurationPlanSourceFamily::DeclarativeV1,
                    "ha-declarative-1",
                    wire::ConfigurationPlanEffect::Configuration,
                ),
                (
                    wire::ConfigurationPlanSourceFamily::DeclarativeV2,
                    "ha-declarative-2",
                    wire::ConfigurationPlanEffect::Configuration,
                ),
                (
                    wire::ConfigurationPlanSourceFamily::Authorization,
                    "homeward.authorization/1",
                    wire::ConfigurationPlanEffect::HumanRights,
                ),
                (
                    wire::ConfigurationPlanSourceFamily::Ownership,
                    "homeward.heating-ownership/1",
                    wire::ConfigurationPlanEffect::TechnicalOwnership,
                ),
                (
                    wire::ConfigurationPlanSourceFamily::Dashboard,
                    "homeward-dashboard-1",
                    wire::ConfigurationPlanEffect::Dashboard,
                ),
                (
                    wire::ConfigurationPlanSourceFamily::Automation,
                    "ha-logic-1",
                    wire::ConfigurationPlanEffect::RunTargets,
                ),
                (
                    wire::ConfigurationPlanSourceFamily::Script,
                    "ha-logic-1",
                    wire::ConfigurationPlanEffect::RunTargets,
                ),
                (
                    wire::ConfigurationPlanSourceFamily::Scene,
                    "ha-scene-1",
                    wire::ConfigurationPlanEffect::RunTargets,
                ),
            ] {
                let mut candidate = baseline.clone();
                candidate.family = family.into();
                candidate.schema_version = schema.into();
                candidate.effect = effect.into();
                candidate.changes[0].effect = effect.into();
                let other = candidate;
                assert!(admit_plan_response(PlanResponse::Build(&other)).is_err());
            }

            response.changes[0].field = "ecobee.credential_secret".into();
            assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
            response.changes[0].field = format!("{native_field}.address");
            assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
            let plan = &mut response;
            plan.changes[0].field = native_field.into();
            plan.changes[0].effect = wire::ConfigurationPlanEffect::Dashboard.into();
            plan.effect = wire::ConfigurationPlanEffect::Dashboard.into();
            assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
            let plan = &mut response;
            plan.family = wire::ConfigurationPlanSourceFamily::Dashboard.into();
            plan.identity = "dashboard.preview".into();
            plan.schema_version = "homeward-dashboard-1".into();
            assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
            let plan = &mut response;
            plan.family = wire::ConfigurationPlanSourceFamily::Operational.into();
            plan.identity = "operational.main".into();
            plan.schema_version = "homeward.operational/1".into();
            plan.changes[0].effect = wire::ConfigurationPlanEffect::Configuration.into();
            plan.effect = wire::ConfigurationPlanEffect::Configuration.into();
            plan.application_available = true;
            assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        }
    }
}
