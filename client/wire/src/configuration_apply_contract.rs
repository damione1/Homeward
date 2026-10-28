//! Structural activation admission; a plan receipt conveys no authorization.
use std::collections::BTreeSet;

use crate::{
    config_plans,
    configuration_contract::{self as configuration, ConfigurationAdmissionError as Error},
    home::{
        configuration::v1 as document, configurationapply::v1 as wire,
        configurationplan::v1 as plan,
    },
};
use buffa::EnumValue;

pub enum ActivationRequest<'a> {
    Apply(&'a wire::ApplyRequest),
    Rollback(&'a wire::RollbackRequest),
}
pub enum ActivationResponse<'a> {
    Apply(&'a wire::ApplyResponse),
    Rollback(&'a wire::RollbackResponse),
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
fn history(value: &wire::RollbackRequest) -> bool {
    if value.family == plan::SourceFamily::Authorization {
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
                && value
                    .candidate
                    .as_option()
                    .is_some_and(config_plans::candidate)
                && value
                    .receipt
                    .as_ref()
                    .is_none_or(|value| (44..=4200).contains(&value.len()))
                && value
                    .expected
                    .as_option()
                    .is_some_and(configuration::version)
                && configuration::hash(&value.expected_workspace_sha256)
                && sources(&value.sources)
                && if value.candidate.family == plan::SourceFamily::Operational {
                    value.sources.is_empty()
                } else {
                    value.sources.len() == value.candidate.files.len()
                }
        }
        ActivationRequest::Rollback(value) => {
            value.__buffa_unknown_fields.is_empty()
                && value.id.as_option().is_some_and(configuration::id)
                && value
                    .expected
                    .as_option()
                    .is_some_and(configuration::version)
                && configuration::hash(&value.expected_workspace_sha256)
                && history(value)
                && sources(&value.sources)
                && match value.family {
                    EnumValue::Known(plan::SourceFamily::Unspecified) => {
                        value.sources.is_empty() && value.identity.is_empty()
                    }
                    EnumValue::Known(plan::SourceFamily::Operational) => {
                        value.sources.is_empty() && value.identity == "operational.main"
                    }
                    EnumValue::Known(
                        plan::SourceFamily::DeclarativeV1
                        | plan::SourceFamily::DeclarativeV2
                        | plan::SourceFamily::Authorization
                        | plan::SourceFamily::Ownership
                        | plan::SourceFamily::Dashboard
                        | plan::SourceFamily::Automation
                        | plan::SourceFamily::Script
                        | plan::SourceFamily::Scene,
                    ) => !value.sources.is_empty() && config_plans::label(&value.identity, 255),
                    EnumValue::Unknown(_) => false,
                }
        }
    };
    if valid { Ok(()) } else { Err(Error::Invalid) }
}
pub fn admit_activation_response(value: ActivationResponse<'_>) -> Result<(), Error> {
    let (known, activation, current): (_, _, Option<&document::ReadDocumentResponse>) = match value
    {
        ActivationResponse::Apply(value) => (
            value.__buffa_unknown_fields.is_empty(),
            value.activation.as_option(),
            value.current.as_option(),
        ),
        ActivationResponse::Rollback(value) => (
            value.__buffa_unknown_fields.is_empty(),
            value.activation.as_option(),
            value.current.as_option(),
        ),
    };
    if !known
        || !activation
            .is_some_and(|value| configuration::admit_configuration_activation(value).is_ok())
        || !current.is_none_or(|value| configuration::admit_configuration_read(value).is_ok())
    {
        return Err(Error::Invalid);
    }
    Ok(())
}
