//! Optional inert configuration plans: representation admission only. A receipt
//! does not authenticate its holder or grant activation/physical authority.
use crate::home::configurationplan::v1 as wire;
use buffa::EnumValue;
use std::collections::BTreeSet;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PlanAdmissionError {
    Invalid,
}

pub enum PlanRequest<'a> {
    Build(&'a wire::BuildPlanRequest),
    Revalidate(&'a wire::RevalidatePlanRequest),
}
pub enum PlanResponse<'a> {
    Build(&'a wire::BuildPlanResponse),
    Revalidate(&'a wire::RevalidatePlanResponse),
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
pub(crate) fn family(value: EnumValue<wire::SourceFamily>) -> Option<wire::SourceFamily> {
    match value {
        EnumValue::Known(
            value @ (wire::SourceFamily::Operational
            | wire::SourceFamily::DeclarativeV1
            | wire::SourceFamily::DeclarativeV2
            | wire::SourceFamily::Authorization
            | wire::SourceFamily::Ownership
            | wire::SourceFamily::Dashboard
            | wire::SourceFamily::Automation
            | wire::SourceFamily::Script
            | wire::SourceFamily::Scene),
        ) => Some(value),
        EnumValue::Known(wire::SourceFamily::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub(crate) fn candidate(value: &wire::Candidate) -> bool {
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
            v.__buffa_unknown_fields.is_empty() && v.candidate.as_option().is_some_and(candidate)
        }
        PlanRequest::Revalidate(v) => {
            v.__buffa_unknown_fields.is_empty()
                && v.candidate.as_option().is_some_and(candidate)
                && (44..=4200).contains(&v.receipt.len())
        }
    };
    if valid {
        Ok(AdmittedPlanRequest(value))
    } else {
        Err(PlanAdmissionError::Invalid)
    }
}
fn schema(family: wire::SourceFamily, value: &str) -> bool {
    match family {
        wire::SourceFamily::Operational => value == "homeward.operational/1",
        wire::SourceFamily::DeclarativeV1 => value == "ha-declarative-1",
        wire::SourceFamily::DeclarativeV2 => value == "ha-declarative-2",
        wire::SourceFamily::Authorization => value == "homeward.authorization/1",
        wire::SourceFamily::Ownership => value == "homeward.heating-ownership/1",
        wire::SourceFamily::Dashboard => value == "homeward-dashboard-1",
        wire::SourceFamily::Automation | wire::SourceFamily::Script => value == "ha-logic-1",
        wire::SourceFamily::Scene => value == "ha-scene-1",
        wire::SourceFamily::Unspecified => false,
    }
}
fn effect(family: wire::SourceFamily, value: EnumValue<wire::Effect>) -> bool {
    match value {
        EnumValue::Known(wire::Effect::None) => true,
        EnumValue::Known(wire::Effect::Configuration) => matches!(
            family,
            wire::SourceFamily::Operational
                | wire::SourceFamily::DeclarativeV1
                | wire::SourceFamily::DeclarativeV2
        ),
        EnumValue::Known(wire::Effect::HumanRights) => family == wire::SourceFamily::Authorization,
        EnumValue::Known(wire::Effect::TechnicalOwnership) => matches!(
            family,
            wire::SourceFamily::Ownership | wire::SourceFamily::Operational
        ),
        EnumValue::Known(wire::Effect::Dashboard) => family == wire::SourceFamily::Dashboard,
        EnumValue::Known(wire::Effect::RunTargets) => matches!(
            family,
            wire::SourceFamily::Automation | wire::SourceFamily::Script | wire::SourceFamily::Scene
        ),
        EnumValue::Known(wire::Effect::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn base(value: &wire::Base) -> bool {
    if !value.__buffa_unknown_fields.is_empty() {
        return false;
    }
    match value.kind {
        EnumValue::Known(wire::BaseKind::ActiveSql) => {
            value.revision.is_some_and(|v| v > 0)
                && value.source_sha256.as_ref().is_some_and(|v| hash(v))
        }
        EnumValue::Known(wire::BaseKind::ImmutableReference) => {
            value.revision.is_none() && value.source_sha256.as_ref().is_some_and(|v| hash(v))
        }
        EnumValue::Known(wire::BaseKind::NoActiveDocument) => {
            value.revision.is_none() && value.source_sha256.is_none()
        }
        EnumValue::Known(wire::BaseKind::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn field(value: &str) -> bool {
    matches!(
        value,
        "ecobee"
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
fn diagnostic(value: EnumValue<wire::DiagnosticCode>) -> bool {
    match value {
        EnumValue::Known(
            wire::DiagnosticCode::InvalidSchema
            | wire::DiagnosticCode::ExcludedReference
            | wire::DiagnosticCode::UnsupportedSource
            | wire::DiagnosticCode::Capacity
            | wire::DiagnosticCode::BaseUnavailable,
        ) => true,
        EnumValue::Known(wire::DiagnosticCode::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn plan(value: &wire::Plan) -> bool {
    let Some(family) = family(value.family) else {
        return false;
    };
    if !value.__buffa_unknown_fields.is_empty()
        || !label(&value.author, 255)
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
    let mut maximum: EnumValue<wire::Effect> = wire::Effect::None.into();
    for change in &value.changes {
        if !change.__buffa_unknown_fields.is_empty()
            || !field(&change.field)
            || ((change.field == "ecobee"
                || change.field == "remote_calendar"
                || change.field == "disabled_integrations")
                && family != wire::SourceFamily::Operational)
            || change.field.as_str() <= previous
            || !effect(family, change.effect)
            || change.effect == wire::Effect::None
            || change.effect.to_i32() > value.effect.to_i32()
            || change.references_added > 4096
            || change.references_removed > 4096
        {
            return false;
        }
        previous = &change.field;
        if change.effect.to_i32() > maximum.to_i32() {
            maximum = change.effect;
        }
        match change.operation {
            EnumValue::Known(
                wire::ChangeOperation::Added
                | wire::ChangeOperation::Removed
                | wire::ChangeOperation::Replaced,
            ) => {}
            EnumValue::Known(wire::ChangeOperation::Unspecified) | EnumValue::Unknown(_) => {
                return false;
            }
        }
    }
    if value.semantic_sha256.is_some() && value.effect != maximum {
        return false;
    }
    match value.state {
        EnumValue::Known(wire::PlanState::Admitted) => {
            value.semantic_sha256.as_ref().is_some_and(|v| hash(v))
                && value.diagnostics.is_empty()
                && (44..=4200).contains(&value.receipt.len())
                && (value.changes.is_empty() == (value.effect == wire::Effect::None))
        }
        EnumValue::Known(wire::PlanState::Rejected) => {
            value.semantic_sha256.is_none()
                && value.changes.is_empty()
                && !value.diagnostics.is_empty()
                && value.effect != wire::Effect::None
                && (44..=4200).contains(&value.receipt.len())
        }
        EnumValue::Known(wire::PlanState::Stale) => {
            value.receipt.is_empty()
                && ((value.semantic_sha256.as_ref().is_some_and(|v| hash(v))
                    && value.diagnostics.is_empty())
                    || (value.semantic_sha256.is_none() && !value.diagnostics.is_empty()))
        }
        EnumValue::Known(wire::PlanState::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
pub fn admit_plan_response(
    value: PlanResponse<'_>,
) -> Result<AdmittedPlanResponse<'_>, PlanAdmissionError> {
    let valid = match &value {
        PlanResponse::Build(v) => {
            v.__buffa_unknown_fields.is_empty()
                && v.plan
                    .as_option()
                    .is_some_and(|p| plan(p) && p.state != wire::PlanState::Stale)
        }
        PlanResponse::Revalidate(v) => {
            v.__buffa_unknown_fields.is_empty() && v.plan.as_option().is_some_and(plan)
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
        let candidate = wire::Candidate {
            family: wire::SourceFamily::Dashboard.into(),
            identity: "dashboard.preview".into(),
            entry: "candidate.yaml".into(),
            files: vec![wire::SourceFile {
                logical_path: "candidate.yaml".into(),
                exact_source: b"views: []".to_vec(),
                ..Default::default()
            }],
            ..Default::default()
        };
        let mut request = wire::BuildPlanRequest {
            candidate: candidate.into(),
            ..Default::default()
        };
        assert!(admit_plan_request(PlanRequest::Build(&request)).is_ok());
        request.candidate.as_option_mut().unwrap().family = EnumValue::Unknown(999);
        assert!(admit_plan_request(PlanRequest::Build(&request)).is_err());
    }
    #[test]
    fn nha053_absent_revision_is_not_zero_or_invented_active() {
        let mut value = wire::Base {
            kind: wire::BaseKind::NoActiveDocument.into(),
            ..Default::default()
        };
        assert!(base(&value));
        value.revision = Some(0);
        assert!(!base(&value));
        value.kind = wire::BaseKind::ActiveSql.into();
        value.source_sha256 = Some(vec![1; 32]);
        assert!(!base(&value));
        value.revision = Some(1);
        assert!(base(&value));
    }
    #[test]
    fn nha053_effect_families_never_collapse() {
        assert!(effect(
            wire::SourceFamily::Authorization,
            wire::Effect::HumanRights.into()
        ));
        assert!(!effect(
            wire::SourceFamily::Dashboard,
            wire::Effect::HumanRights.into()
        ));
        assert!(!effect(
            wire::SourceFamily::Automation,
            wire::Effect::Dashboard.into()
        ));
        assert!(!effect(
            wire::SourceFamily::Ownership,
            EnumValue::Unknown(999)
        ));
    }
}

#[cfg(test)]
mod remote_calendar_tests {
    use super::*;

    #[test]
    fn nha053_remote_calendar_response_refuses_private_paths_and_layout_effect() {
        let plan = wire::Plan {
            state: wire::PlanState::Admitted.into(),
            author: "server-admitted-admin".into(),
            family: wire::SourceFamily::Operational.into(),
            identity: "operational.main".into(),
            schema_version: "homeward.operational/1".into(),
            source_sha256: vec![1; 32],
            semantic_sha256: Some(vec![2; 32]),
            base: wire::Base {
                kind: wire::BaseKind::NoActiveDocument.into(),
                ..Default::default()
            }
            .into(),
            generation_revision: 1,
            manifest_sha256: vec![3; 32],
            observed_workspace_sha256: vec![4; 32],
            effect: wire::Effect::Configuration.into(),
            changes: vec![wire::Change {
                effect: wire::Effect::Configuration.into(),
                operation: wire::ChangeOperation::Added.into(),
                field: "remote_calendar".into(),
                references_added: 1,
                ..Default::default()
            }],
            receipt: "a".repeat(80),
            ..Default::default()
        };
        let mut response = wire::BuildPlanResponse {
            plan: plan.into(),
            ..Default::default()
        };
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_ok());
        response.plan.as_option_mut().unwrap().changes[0].field = "remote_calendar.url".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        response.plan.as_option_mut().unwrap().changes[0].field =
            "remote_calendar.auth.bearer_secret".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.changes[0].field = "remote_calendar".into();
        plan.changes[0].effect = wire::Effect::Dashboard.into();
        plan.effect = wire::Effect::Dashboard.into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.family = wire::SourceFamily::Dashboard.into();
        plan.identity = "dashboard.preview".into();
        plan.schema_version = "homeward-dashboard-1".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.family = wire::SourceFamily::Operational.into();
        plan.identity = "operational.main".into();
        plan.schema_version = "homeward.operational/1".into();
        plan.changes[0].effect = wire::Effect::Configuration.into();
        plan.effect = wire::Effect::Configuration.into();
        plan.application_available = true;
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
    }
}

#[cfg(test)]
mod integration_selection_tests {
    use super::*;

    #[test]
    fn nha053_integration_selection_response_refuses_private_instances_and_cross_family_effect() {
        let plan = wire::Plan {
            state: wire::PlanState::Admitted.into(),
            author: "server-admitted-admin".into(),
            family: wire::SourceFamily::Operational.into(),
            identity: "operational.main".into(),
            schema_version: "homeward.operational/1".into(),
            source_sha256: vec![1; 32],
            semantic_sha256: Some(vec![2; 32]),
            base: wire::Base {
                kind: wire::BaseKind::NoActiveDocument.into(),
                ..Default::default()
            }
            .into(),
            generation_revision: 1,
            manifest_sha256: vec![3; 32],
            observed_workspace_sha256: vec![4; 32],
            effect: wire::Effect::Configuration.into(),
            changes: vec![wire::Change {
                effect: wire::Effect::Configuration.into(),
                operation: wire::ChangeOperation::Added.into(),
                field: "disabled_integrations".into(),
                references_added: 1,
                ..Default::default()
            }],
            receipt: "a".repeat(80),
            ..Default::default()
        };
        let mut response = wire::BuildPlanResponse {
            plan: plan.into(),
            ..Default::default()
        };
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_ok());
        let baseline = response.plan.as_option().unwrap().clone();
        for (family, schema, effect) in [
            (
                wire::SourceFamily::DeclarativeV1,
                "ha-declarative-1",
                wire::Effect::Configuration,
            ),
            (
                wire::SourceFamily::DeclarativeV2,
                "ha-declarative-2",
                wire::Effect::Configuration,
            ),
            (
                wire::SourceFamily::Authorization,
                "homeward.authorization/1",
                wire::Effect::HumanRights,
            ),
            (
                wire::SourceFamily::Ownership,
                "homeward.heating-ownership/1",
                wire::Effect::TechnicalOwnership,
            ),
            (
                wire::SourceFamily::Dashboard,
                "homeward-dashboard-1",
                wire::Effect::Dashboard,
            ),
            (
                wire::SourceFamily::Automation,
                "ha-logic-1",
                wire::Effect::RunTargets,
            ),
            (
                wire::SourceFamily::Script,
                "ha-logic-1",
                wire::Effect::RunTargets,
            ),
            (
                wire::SourceFamily::Scene,
                "ha-scene-1",
                wire::Effect::RunTargets,
            ),
        ] {
            let mut candidate = baseline.clone();
            candidate.family = family.into();
            candidate.schema_version = schema.into();
            candidate.effect = effect.into();
            candidate.changes[0].effect = effect.into();
            let other = wire::BuildPlanResponse {
                plan: candidate.into(),
                ..Default::default()
            };
            assert!(admit_plan_response(PlanResponse::Build(&other)).is_err());
        }

        response.plan.as_option_mut().unwrap().changes[0].field = "disabled_integrations[0]".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        response.plan.as_option_mut().unwrap().changes[0].field =
            "disabled_integrations.nha053-private-selection-worker".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.changes[0].field = "disabled_integrations".into();
        plan.changes[0].effect = wire::Effect::Dashboard.into();
        plan.effect = wire::Effect::Dashboard.into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.family = wire::SourceFamily::Dashboard.into();
        plan.identity = "dashboard.preview".into();
        plan.schema_version = "homeward-dashboard-1".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.family = wire::SourceFamily::Operational.into();
        plan.identity = "operational.main".into();
        plan.schema_version = "homeward.operational/1".into();
        plan.changes[0].effect = wire::Effect::Configuration.into();
        plan.effect = wire::Effect::Configuration.into();
        plan.application_available = true;
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
    }
}

#[cfg(test)]
mod ecobee_tests {
    use super::*;

    #[test]
    fn nha053_ecobee_response_refuses_private_settings_and_cross_family_effect() {
        let plan = wire::Plan {
            state: wire::PlanState::Admitted.into(),
            author: "server-admitted-admin".into(),
            family: wire::SourceFamily::Operational.into(),
            identity: "operational.main".into(),
            schema_version: "homeward.operational/1".into(),
            source_sha256: vec![1; 32],
            semantic_sha256: Some(vec![2; 32]),
            base: wire::Base {
                kind: wire::BaseKind::NoActiveDocument.into(),
                ..Default::default()
            }
            .into(),
            generation_revision: 1,
            manifest_sha256: vec![3; 32],
            observed_workspace_sha256: vec![4; 32],
            effect: wire::Effect::Configuration.into(),
            changes: vec![wire::Change {
                effect: wire::Effect::Configuration.into(),
                operation: wire::ChangeOperation::Added.into(),
                field: "ecobee".into(),
                references_added: 1,
                ..Default::default()
            }],
            receipt: "a".repeat(80),
            ..Default::default()
        };
        let mut response = wire::BuildPlanResponse {
            plan: plan.into(),
            ..Default::default()
        };
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_ok());
        let baseline = response.plan.as_option().unwrap().clone();
        for (family, schema, effect) in [
            (
                wire::SourceFamily::DeclarativeV1,
                "ha-declarative-1",
                wire::Effect::Configuration,
            ),
            (
                wire::SourceFamily::DeclarativeV2,
                "ha-declarative-2",
                wire::Effect::Configuration,
            ),
            (
                wire::SourceFamily::Authorization,
                "homeward.authorization/1",
                wire::Effect::HumanRights,
            ),
            (
                wire::SourceFamily::Ownership,
                "homeward.heating-ownership/1",
                wire::Effect::TechnicalOwnership,
            ),
            (
                wire::SourceFamily::Dashboard,
                "homeward-dashboard-1",
                wire::Effect::Dashboard,
            ),
            (
                wire::SourceFamily::Automation,
                "ha-logic-1",
                wire::Effect::RunTargets,
            ),
            (
                wire::SourceFamily::Script,
                "ha-logic-1",
                wire::Effect::RunTargets,
            ),
            (
                wire::SourceFamily::Scene,
                "ha-scene-1",
                wire::Effect::RunTargets,
            ),
        ] {
            let mut candidate = baseline.clone();
            candidate.family = family.into();
            candidate.schema_version = schema.into();
            candidate.effect = effect.into();
            candidate.changes[0].effect = effect.into();
            let other = wire::BuildPlanResponse {
                plan: candidate.into(),
                ..Default::default()
            };
            assert!(admit_plan_response(PlanResponse::Build(&other)).is_err());
        }

        response.plan.as_option_mut().unwrap().changes[0].field = "ecobee.credential_secret".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        response.plan.as_option_mut().unwrap().changes[0].field = "ecobee.address".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.changes[0].field = "ecobee".into();
        plan.changes[0].effect = wire::Effect::Dashboard.into();
        plan.effect = wire::Effect::Dashboard.into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.family = wire::SourceFamily::Dashboard.into();
        plan.identity = "dashboard.preview".into();
        plan.schema_version = "homeward-dashboard-1".into();
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
        let plan = response.plan.as_option_mut().unwrap();
        plan.family = wire::SourceFamily::Operational.into();
        plan.identity = "operational.main".into();
        plan.schema_version = "homeward.operational/1".into();
        plan.changes[0].effect = wire::Effect::Configuration.into();
        plan.effect = wire::Effect::Configuration.into();
        plan.application_available = true;
        assert!(admit_plan_response(PlanResponse::Build(&response)).is_err());
    }
}
