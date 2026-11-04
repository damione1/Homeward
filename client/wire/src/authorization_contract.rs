//! Structural admission of native-owned public policy resources; never evaluates rights.
use crate::auth_contract::AdmissionError;
use crate::home::api::v2 as auth;
use buffa::EnumValue;
use home_client_model::auth::AuthorizationPolicyName;

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
fn etag(value: &str) -> bool {
    !value.is_empty() && value.len() <= 128
}
fn snapshot(value: &auth::AuthorizationPolicy) -> bool {
    let stored = match value.origin {
        EnumValue::Known(auth::AuthorizationPolicyOrigin::Default) => {
            value.etag.is_empty() && !value.update_time.is_set()
        }
        EnumValue::Known(auth::AuthorizationPolicyOrigin::Stored) => {
            etag(&value.etag)
                && value.update_time.as_option().is_some_and(|v| {
                    v.__buffa_unknown_fields.is_empty()
                        && (-62_135_596_800..=253_402_300_799).contains(&v.seconds)
                        && (0..1_000_000_000).contains(&v.nanos)
                })
        }
        EnumValue::Known(auth::AuthorizationPolicyOrigin::Unspecified) | EnumValue::Unknown(_) => {
            false
        }
    };
    stored
        && value.__buffa_unknown_fields.is_empty()
        && AuthorizationPolicyName::try_from(value.name.as_str()).is_ok()
        && source(&value.yaml_source)
        && value.schema == POLICY_SCHEMA
        && value.syntax_profile == POLICY_SYNTAX_PROFILE
}
pub fn admit_get_request(
    value: &auth::GetAuthorizationPolicyRequest,
) -> Result<(), AdmissionError> {
    if value.__buffa_unknown_fields.is_empty()
        && AuthorizationPolicyName::try_from(value.name.as_str()).is_ok()
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_get_response(
    value: &auth::AuthorizationPolicy,
) -> Result<Admitted<auth::AuthorizationPolicy>, AdmissionError> {
    if snapshot(value) {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_replace_request(
    value: &auth::ReplaceAuthorizationPolicyRequest,
) -> Result<(), AdmissionError> {
    use auth::replace_authorization_policy_request::Precondition;
    let valid = match &value.precondition {
        Some(Precondition::ExpectedEtag(v)) => etag(v),
        Some(Precondition::ExpectAbsent(v)) => v.__buffa_unknown_fields.is_empty(),
        None => false,
    };
    if valid
        && value.__buffa_unknown_fields.is_empty()
        && source(&value.yaml_source)
        && AuthorizationPolicyName::try_from(value.name.as_str()).is_ok()
    {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}
pub fn admit_replace_response(
    value: &auth::ReplaceAuthorizationPolicyResponse,
) -> Result<Admitted<auth::ReplaceAuthorizationPolicyResponse>, AdmissionError> {
    let valid = match value.rights_change {
        EnumValue::Known(auth::PolicyRightsChange::Changed)
        | EnumValue::Known(auth::PolicyRightsChange::Unchanged) => true,
        EnumValue::Known(auth::PolicyRightsChange::Unspecified) | EnumValue::Unknown(_) => false,
    };
    if valid
        && value.__buffa_unknown_fields.is_empty()
        && value.policy.as_option().is_some_and(|policy| {
            snapshot(policy) && policy.origin == auth::AuthorizationPolicyOrigin::Stored
        })
    {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
