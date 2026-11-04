//! Configuration's finite Any vocabulary on the standard Operations envelope.
use buffa::{EnumValue, Message};
use home_client_model::api::{OperationName, RequestId};

use crate::{
    api_errors::{self, ApiFailure},
    api_operations, configuration_contract as configuration,
    google::longrunning::{self, operation::Result as OperationResult},
    home::api::v2 as wire,
};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InvalidConfigurationOperation;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Phase {
    Accepted,
    Committed,
    Published,
    Failed,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CommittedTarget {
    generation: String,
    revision: i64,
    manifest: [u8; 32],
    commit_time: (i64, i32),
}
impl CommittedTarget {
    pub fn generation(&self) -> &str {
        &self.generation
    }
    pub fn revision(&self) -> i64 {
        self.revision
    }
    pub fn manifest(&self) -> &[u8; 32] {
        &self.manifest
    }
    pub fn commit_time(&self) -> (i64, i32) {
        self.commit_time
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum State {
    Accepted,
    Committed(CommittedTarget),
    Published(CommittedTarget),
    Failed(ApiFailure),
}

/// This value contains no source bytes, diagnostics or permission to retry.
/// An accepted reservation carries no invented generation or commit evidence.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Publication {
    name: OperationName,
    request_id: Option<RequestId>,
    create_time: (i64, i32),
    state: State,
}
impl Publication {
    pub fn name(&self) -> &OperationName {
        &self.name
    }
    pub fn request_id(&self) -> Option<&RequestId> {
        self.request_id.as_ref()
    }
    pub fn phase(&self) -> Phase {
        match &self.state {
            State::Accepted => Phase::Accepted,
            State::Committed(_) => Phase::Committed,
            State::Published(_) => Phase::Published,
            State::Failed(_) => Phase::Failed,
        }
    }
    pub fn target(&self) -> Option<&CommittedTarget> {
        match &self.state {
            State::Committed(target) | State::Published(target) => Some(target),
            State::Accepted | State::Failed(_) => None,
        }
    }
    pub fn failure(&self) -> Option<&ApiFailure> {
        match &self.state {
            State::Failed(failure) => Some(failure),
            State::Accepted | State::Committed(_) | State::Published(_) => None,
        }
    }
    /// A reservation can acquire native commit evidence once. A terminal
    /// success or failure cannot regress, switch outcomes or change its target.
    pub fn follows(&self, previous: &Self) -> bool {
        self.name == previous.name
            && self.request_id == previous.request_id
            && self.create_time == previous.create_time
            && match &previous.state {
                State::Accepted => true,
                State::Committed(before) => match &self.state {
                    State::Committed(after) | State::Published(after) => before == after,
                    State::Accepted | State::Failed(_) => false,
                },
                State::Published(before) => match &self.state {
                    State::Published(after) => before == after,
                    State::Accepted | State::Committed(_) | State::Failed(_) => false,
                },
                State::Failed(before) => match &self.state {
                    State::Failed(after) => before == after,
                    State::Accepted | State::Committed(_) | State::Published(_) => false,
                },
            }
    }
}

pub fn admit_response(
    raw: &longrunning::Operation,
) -> Result<Publication, InvalidConfigurationOperation> {
    let expected =
        OperationName::try_from(raw.name.as_str()).map_err(|_| InvalidConfigurationOperation)?;
    admit_operation(raw, &expected)
}

pub fn admit_operation(
    raw: &longrunning::Operation,
    expected: &OperationName,
) -> Result<Publication, InvalidConfigurationOperation> {
    api_operations::admit_envelope(raw, expected).map_err(|_| InvalidConfigurationOperation)?;
    let Some((home, id)) = expected.as_str().split_once("/operations/") else {
        return Err(InvalidConfigurationOperation);
    };
    let Some(attempt) = id.strip_prefix("config-") else {
        return Err(InvalidConfigurationOperation);
    };
    if attempt.len() != 32
        || !attempt
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        || attempt.bytes().all(|b| b == b'0')
    {
        return Err(InvalidConfigurationOperation);
    }
    let metadata = raw
        .metadata
        .as_option()
        .ok_or(InvalidConfigurationOperation)?;
    if metadata.type_url != "type.googleapis.com/home.api.v2.ConfigurationOperationMetadata"
        || metadata.value.is_empty()
        || metadata.value.len() > 64 * 1024
    {
        return Err(InvalidConfigurationOperation);
    }
    let metadata = wire::ConfigurationOperationMetadata::decode_from_slice(&metadata.value)
        .map_err(|_| InvalidConfigurationOperation)?;
    if !metadata.__buffa_unknown_fields.is_empty()
        || !metadata
            .create_time
            .as_option()
            .is_some_and(configuration::timestamp)
    {
        return Err(InvalidConfigurationOperation);
    }
    let create_time = (metadata.create_time.seconds, metadata.create_time.nanos);
    let request_id = if metadata.request_id.is_empty() {
        None
    } else {
        Some(
            RequestId::try_from(metadata.request_id.as_str())
                .map_err(|_| InvalidConfigurationOperation)?,
        )
    };
    let phase = match metadata.phase {
        EnumValue::Known(wire::ConfigurationOperationPhase::Accepted) => Phase::Accepted,
        EnumValue::Known(wire::ConfigurationOperationPhase::Failed) => Phase::Failed,
        EnumValue::Known(wire::ConfigurationOperationPhase::Committed) => Phase::Committed,
        EnumValue::Known(wire::ConfigurationOperationPhase::Published) => Phase::Published,
        EnumValue::Known(wire::ConfigurationOperationPhase::Unspecified)
        | EnumValue::Unknown(_) => return Err(InvalidConfigurationOperation),
    };
    let state = match phase {
        Phase::Accepted | Phase::Failed => {
            if request_id.is_none()
                || metadata.commit_time.is_set()
                || !metadata.generation.is_empty()
                || metadata.generation_revision != 0
                || !metadata.manifest_sha256.is_empty()
                || raw.done != (phase == Phase::Failed)
            {
                return Err(InvalidConfigurationOperation);
            }
            if phase == Phase::Accepted {
                State::Accepted
            } else {
                let Some(OperationResult::Error(status)) = &raw.result else {
                    return Err(InvalidConfigurationOperation);
                };
                let failure =
                    api_errors::admit_status(status).map_err(|_| InvalidConfigurationOperation)?;
                if matches!(
                    failure.reason(),
                    wire::ApiErrorReason::CommitOutcomeUnknown
                        | wire::ApiErrorReason::EffectOutcomeUnknown
                ) {
                    return Err(InvalidConfigurationOperation);
                }
                State::Failed(failure)
            }
        }
        Phase::Committed | Phase::Published => {
            let manifest: [u8; 32] = metadata
                .manifest_sha256
                .as_slice()
                .try_into()
                .map_err(|_| InvalidConfigurationOperation)?;
            if !metadata
                .commit_time
                .as_option()
                .is_some_and(configuration::timestamp)
                || (metadata.commit_time.seconds, metadata.commit_time.nanos) < create_time
                || metadata.generation_revision <= 0
                || manifest == [0; 32]
                || metadata.generation
                    != format!(
                        "{home}/configurationGenerations/g-{}",
                        metadata.generation_revision
                    )
                || raw.done != (phase == Phase::Published)
            {
                return Err(InvalidConfigurationOperation);
            }
            let target = CommittedTarget {
                generation: metadata.generation.clone(),
                revision: metadata.generation_revision,
                manifest,
                commit_time: (metadata.commit_time.seconds, metadata.commit_time.nanos),
            };
            if phase == Phase::Committed {
                State::Committed(target)
            } else {
                let Some(OperationResult::Response(response)) = &raw.result else {
                    return Err(InvalidConfigurationOperation);
                };
                if response.type_url
                    != "type.googleapis.com/home.api.v2.ConfigurationActivationResult"
                    || response.value.is_empty()
                    || response.value.len() > 64 * 1024
                {
                    return Err(InvalidConfigurationOperation);
                }
                let result =
                    wire::ConfigurationActivationResult::decode_from_slice(&response.value)
                        .map_err(|_| InvalidConfigurationOperation)?;
                if !result.__buffa_unknown_fields.is_empty()
                    || result.generation != metadata.generation
                    || result.generation_revision != metadata.generation_revision
                    || result.manifest_sha256 != metadata.manifest_sha256
                {
                    return Err(InvalidConfigurationOperation);
                }
                State::Published(target)
            }
        }
    };
    Ok(Publication {
        name: expected.clone(),
        request_id,
        create_time,
        state,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn accepted() -> Publication {
        Publication {
            name: OperationName::try_from(
                "homes/synthetic/operations/config-11111111111111111111111111111111",
            )
            .unwrap(),
            request_id: Some(RequestId::try_from("018e363a-8e16-44a4-9aa6-f623ecb0b504").unwrap()),
            create_time: (100, 0),
            state: State::Accepted,
        }
    }

    #[test]
    fn native_commit_evidence_is_immutable_and_cannot_regress() {
        let accepted = accepted();
        assert!(accepted.target().is_none());
        let mut committed = accepted.clone();
        let target = CommittedTarget {
            generation: "homes/synthetic/configurationGenerations/g-1".into(),
            revision: 1,
            manifest: [1; 32],
            commit_time: (101, 0),
        };
        committed.state = State::Committed(target.clone());
        assert!(committed.follows(&accepted));
        assert!(!accepted.follows(&committed));
        let mut published = committed.clone();
        published.state = State::Published(target.clone());
        assert!(published.follows(&committed));
        assert!(!committed.follows(&published));
        let mut changed = published.clone();
        let mut changed_target = target;
        changed_target.commit_time = (102, 0);
        changed.state = State::Published(changed_target);
        assert!(!changed.follows(&published));
        changed = published.clone();
        changed.request_id =
            Some(RequestId::try_from("018e363a-8e16-44a4-9aa6-f623ecb0b505").unwrap());
        assert!(!changed.follows(&accepted));
        changed = published.clone();
        changed.create_time = (99, 0);
        assert!(!changed.follows(&accepted));
    }
}
