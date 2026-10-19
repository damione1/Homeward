//! Structural admission for policy RPCs. This module never evaluates rights.
use crate::auth_contract::{AccountId, AdmissionError};
use crate::home::auth::v1 as auth;
use buffa::EnumValue;

pub const POLICY_SCHEMA: &str = "homeward.authorization/1";
pub const POLICY_SYNTAX_PROFILE: &str = "homeward.authorization-yaml/1";
pub const MAX_SOURCE_BYTES: usize = 1 << 20;

pub struct Admitted<T> {
    wire: T,
}
impl<T> Admitted<T> {
    pub fn wire(&self) -> &T {
        &self.wire
    }
}

fn source(value: &[u8]) -> bool {
    !value.is_empty() && value.len() <= MAX_SOURCE_BYTES && std::str::from_utf8(value).is_ok()
}
fn version(value: &auth::PolicyVersion) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.revision > 0
        && value.revision <= i64::MAX as u64
        && value.source_sha256.len() == 32
        && value.source_sha256.iter().any(|b| *b != 0)
}
fn snapshot(value: &auth::PolicySnapshot) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value
            .account_id
            .as_option()
            .is_some_and(|v| AccountId::try_from(v).is_ok())
        && source(&value.yaml_source)
        && value.schema == POLICY_SCHEMA
        && value.syntax_profile == POLICY_SYNTAX_PROFILE
        && value.version.as_option().is_some_and(version)
        && value.updated_at.as_option().is_some_and(|v| {
            v.__buffa_unknown_fields.is_empty()
                && (-62_135_596_800..=253_402_300_799).contains(&v.seconds)
                && (0..1_000_000_000).contains(&v.nanos)
        })
}
pub fn admit_get_request(value: &auth::GetPolicyRequest) -> Result<(), AdmissionError> {
    if value.__buffa_unknown_fields.is_empty()
        && value
            .account_id
            .as_option()
            .is_some_and(|v| AccountId::try_from(v).is_ok())
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_get_response(
    value: &auth::GetPolicyResponse,
) -> Result<Admitted<auth::GetPolicyResponse>, AdmissionError> {
    use auth::get_policy_response::Result;
    let valid = match &value.result {
        Some(Result::Present(v)) => snapshot(v),
        Some(Result::Absent(v)) => v.__buffa_unknown_fields.is_empty(),
        None => false,
    };
    if valid && value.__buffa_unknown_fields.is_empty() {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_replace_request(value: &auth::ReplacePolicyRequest) -> Result<(), AdmissionError> {
    use auth::replace_policy_request::Precondition;
    let valid = match &value.precondition {
        Some(Precondition::Expected(v)) => version(v),
        Some(Precondition::ExpectAbsent(v)) => v.__buffa_unknown_fields.is_empty(),
        None => false,
    };
    if valid
        && value.__buffa_unknown_fields.is_empty()
        && source(&value.yaml_source)
        && value
            .account_id
            .as_option()
            .is_some_and(|v| AccountId::try_from(v).is_ok())
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_replace_response(
    value: &auth::ReplacePolicyResponse,
) -> Result<Admitted<auth::ReplacePolicyResponse>, AdmissionError> {
    let valid = match value.rights_change {
        EnumValue::Known(auth::PolicyRightsChange::Changed)
        | EnumValue::Known(auth::PolicyRightsChange::Unchanged) => true,
        EnumValue::Known(auth::PolicyRightsChange::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if valid
        && value.__buffa_unknown_fields.is_empty()
        && value.policy.as_option().is_some_and(snapshot)
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_error(
    value: &auth::AuthorizationPolicyError,
) -> Result<Admitted<auth::AuthorizationPolicyError>, AdmissionError> {
    let path = value.field_path.len() <= 128
        && value
            .field_path
            .bytes()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || b"_.[]".contains(&c));
    let valid = match value.code {
        EnumValue::Known(auth::AuthorizationPolicyErrorCode::VersionConflict)
        | EnumValue::Known(auth::AuthorizationPolicyErrorCode::ScopeDenied)
        | EnumValue::Known(auth::AuthorizationPolicyErrorCode::AuthorityUnavailable) => {
            value.field_path.is_empty()
        }
        EnumValue::Known(auth::AuthorizationPolicyErrorCode::InvalidSource)
        | EnumValue::Known(auth::AuthorizationPolicyErrorCode::InvalidVersion) => {
            !value.field_path.is_empty()
        }
        EnumValue::Known(auth::AuthorizationPolicyErrorCode::Unspecified)
        | EnumValue::Unknown(_) => false,
    };
    if valid && path && value.__buffa_unknown_fields.is_empty() {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
/// Every admitted termination requires discarding the complete cached view.
pub fn admit_termination(
    value: &auth::AuthorizationTermination,
) -> Result<Admitted<auth::AuthorizationTermination>, AdmissionError> {
    let valid = match value.reason {
        EnumValue::Known(auth::AuthorizationTerminationReason::CredentialRevoked)
        | EnumValue::Known(auth::AuthorizationTerminationReason::CredentialExpired)
        | EnumValue::Known(auth::AuthorizationTerminationReason::AccountDisabled)
        | EnumValue::Known(auth::AuthorizationTerminationReason::RightsChanged)
        | EnumValue::Known(auth::AuthorizationTerminationReason::AuthorityUnavailable) => true,
        EnumValue::Known(auth::AuthorizationTerminationReason::Unspecified)
        | EnumValue::Unknown(_) => false,
    };
    if valid && value.__buffa_unknown_fields.is_empty() {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
