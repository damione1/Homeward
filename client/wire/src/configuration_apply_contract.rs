//! Structural activation admission; a plan receipt conveys no authorization.
use std::collections::BTreeSet;

use crate::{
    config_plans,
    configuration_contract::{self as configuration, ConfigurationAdmissionError as Error},
    home::{api::v2 as wire, api::v2 as plan},
};
use buffa::EnumValue;

pub enum ActivationRequest<'a> {
    Apply(&'a wire::ApplyConfigurationPlanRequest),
    Rollback(&'a wire::RollbackConfigurationRequest),
}
pub enum ActivationResponse<'a> {
    Apply(&'a crate::google::longrunning::Operation),
    Rollback(&'a crate::google::longrunning::Operation),
}

fn sources(values: &[wire::SourcePrecondition]) -> bool {
    let mut ids = BTreeSet::new();
    let mut paths = BTreeSet::new();
    values.len() <= 64
        && values.iter().all(|value| {
            value.__buffa_unknown_fields.is_empty()
                && value.id.as_option().is_some_and(configuration::id)
                && config_plans::label(&value.logical_path, 4096)
                && ids.insert(value.id.value.as_str())
                && paths.insert(value.logical_path.as_str())
                && match &value.sql_state {
                    Some(wire::source_precondition::SqlState::Existing(value)) => {
                        configuration::version(value)
                    }
                    Some(wire::source_precondition::SqlState::SqlAbsent(value)) => {
                        value.__buffa_unknown_fields.is_empty()
                    }
                    None => false,
                }
                && match &value.workspace_state {
                    Some(wire::source_precondition::WorkspaceState::WorkspaceSha256(value)) => {
                        configuration::hash(value)
                    }
                    Some(wire::source_precondition::WorkspaceState::WorkspaceAbsent(value)) => {
                        value.__buffa_unknown_fields.is_empty()
                    }
                    None => false,
                }
        })
}
fn history(value: &wire::RollbackConfigurationRequest) -> bool {
    if value.family == plan::ConfigurationPlanSourceFamily::Authorization {
        match (
            value.historical.as_option(),
            value.historical_policy_version.as_option(),
        ) {
            (Some(value), None) => configuration::generation(value),
            (None, Some(value)) => configuration::version(value),
            (Some(_), Some(_)) | (None, None) => false,
        }
    } else {
        !value.historical_policy_version.is_set()
            && value
                .historical
                .as_option()
                .is_some_and(configuration::generation)
    }
}
pub fn admit_activation_request(value: ActivationRequest<'_>) -> Result<(), Error> {
    let valid = match value {
        ActivationRequest::Apply(value) => {
            value.__buffa_unknown_fields.is_empty()
                && (value.request_id.is_empty()
                    || home_client_model::api::RequestId::try_from(value.request_id.as_str())
                        .is_ok())
                && configuration::resource_name(&value.name, "configurationPlans")
                && !value.etag.is_empty()
                && value.etag.len() <= 128
                && value
                    .candidate
                    .as_option()
                    .is_some_and(config_plans::candidate)
                && value
                    .expected
                    .as_option()
                    .is_some_and(configuration::version)
                && configuration::hash(&value.expected_workspace_sha256)
                && sources(&value.sources)
                && if value.candidate.family == plan::ConfigurationPlanSourceFamily::Operational {
                    value.sources.is_empty()
                } else {
                    value.sources.len() == value.candidate.files.len()
                }
        }
        ActivationRequest::Rollback(value) => {
            value.__buffa_unknown_fields.is_empty()
                && (value.request_id.is_empty()
                    || home_client_model::api::RequestId::try_from(value.request_id.as_str())
                        .is_ok())
                && configuration::document_name(&value.name)
                && value
                    .expected
                    .as_option()
                    .is_some_and(configuration::version)
                && configuration::hash(&value.expected_workspace_sha256)
                && history(value)
                && sources(&value.sources)
                && match value.family {
                    EnumValue::Known(plan::ConfigurationPlanSourceFamily::Unspecified) => {
                        value.sources.is_empty() && value.identity.is_empty()
                    }
                    EnumValue::Known(plan::ConfigurationPlanSourceFamily::Operational) => {
                        value.sources.is_empty() && value.identity == "operational.main"
                    }
                    EnumValue::Known(
                        plan::ConfigurationPlanSourceFamily::DeclarativeV1
                        | plan::ConfigurationPlanSourceFamily::DeclarativeV2
                        | plan::ConfigurationPlanSourceFamily::Authorization
                        | plan::ConfigurationPlanSourceFamily::Ownership
                        | plan::ConfigurationPlanSourceFamily::Dashboard
                        | plan::ConfigurationPlanSourceFamily::Automation
                        | plan::ConfigurationPlanSourceFamily::Script
                        | plan::ConfigurationPlanSourceFamily::Scene,
                    ) => !value.sources.is_empty() && config_plans::label(&value.identity, 255),
                    EnumValue::Unknown(_) => false,
                }
        }
    };
    if valid { Ok(()) } else { Err(Error::Invalid) }
}
pub fn admit_activation_response(value: ActivationResponse<'_>) -> Result<(), Error> {
    let (ActivationResponse::Apply(value) | ActivationResponse::Rollback(value)) = value;
    crate::configuration_operations_contract::admit_response(value)
        .map(|_| ())
        .map_err(|_| Error::Invalid)
}
