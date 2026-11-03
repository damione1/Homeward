//! Admitted canonical error values. Human messages/descriptions are discarded;
//! no error detail confers authority or permission to retry a mutation.
use crate::{google::rpc, home::api::v2 as api};
use buffa::{EnumValue, Enumeration, Message};
use std::collections::BTreeSet;

pub const MAX_DETAILS_BYTES: usize = 16 * 1024;
pub const MAX_VIOLATIONS: usize = 32;
pub const DOMAIN: &str = "homeward.damiengoehrig.ca";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InvalidApiError {
    Malformed,
    Duplicate,
    Unsupported,
    Limit,
    Inconsistent,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct FieldViolation {
    field: String,
    rule: api::ValidationReason,
}
impl FieldViolation {
    pub fn field(&self) -> &str {
        &self.field
    }
    pub fn rule(&self) -> api::ValidationReason {
        self.rule
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ApiFailure {
    code: rpc::Code,
    reason: api::ApiErrorReason,
    violations: Vec<FieldViolation>,
    truncated: bool,
    uncertainty: Option<api::UncertaintyPhase>,
    support_reference: Option<String>,
}
impl ApiFailure {
    pub fn code(&self) -> rpc::Code {
        self.code
    }
    pub fn reason(&self) -> api::ApiErrorReason {
        self.reason
    }
    pub fn violations(&self) -> &[FieldViolation] {
        &self.violations
    }
    pub fn violations_truncated(&self) -> bool {
        self.truncated
    }
    pub fn uncertainty(&self) -> Option<api::UncertaintyPhase> {
        self.uncertainty
    }
    pub fn support_reference(&self) -> Option<&str> {
        self.support_reference.as_deref()
    }
}

fn field_path(path: &str) -> bool {
    if path.len() > 256 {
        return false;
    }
    if path.is_empty() {
        return true;
    }
    path.split('.').all(|part| {
        let (name, index) = match part.split_once('[') {
            Some((name, tail)) => (name, Some(tail)),
            None => (part, None),
        };
        !name.is_empty()
            && name.as_bytes()[0].is_ascii_lowercase()
            && name
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_')
            && index.is_none_or(|tail| {
                tail.strip_suffix(']').is_some_and(|value| {
                    !value.is_empty()
                        && value.len() <= 5
                        && value.bytes().all(|b| b.is_ascii_digit())
                        && value
                            .parse::<u16>()
                            .is_ok_and(|index| index.to_string() == value)
                })
            })
    })
}

fn decode<T: Message + Default>(bytes: &[u8]) -> Result<T, InvalidApiError> {
    T::decode_from_slice(bytes).map_err(|_| InvalidApiError::Malformed)
}

pub fn admit_status(status: &rpc::Status) -> Result<ApiFailure, InvalidApiError> {
    if status.details.len() > 10
        || status.compute_size(&mut buffa::SizeCache::default()) as usize > MAX_DETAILS_BYTES
    {
        return Err(InvalidApiError::Limit);
    }
    if !status.__buffa_unknown_fields.is_empty() {
        return Err(InvalidApiError::Unsupported);
    }
    let code = rpc::Code::from_i32(status.code)
        .filter(|code| *code != rpc::Code::OK)
        .ok_or(InvalidApiError::Unsupported)?;
    let mut reason = None;
    let mut violations = Vec::new();
    let mut truncated = false;
    let mut uncertainty = None;
    let mut support_reference = None;
    let mut seen = BTreeSet::new();
    for detail in &status.details {
        if !detail.__buffa_unknown_fields.is_empty() {
            return Err(InvalidApiError::Unsupported);
        }
        let kind = detail
            .type_url
            .strip_prefix("type.googleapis.com/")
            .unwrap_or(&detail.type_url);
        if !seen.insert(kind) {
            return Err(InvalidApiError::Duplicate);
        }
        match kind {
            "google.rpc.ErrorInfo" => {
                let value: rpc::ErrorInfo = decode(&detail.value)?;
                if !value.__buffa_unknown_fields.is_empty() || value.domain != DOMAIN {
                    return Err(InvalidApiError::Unsupported);
                }
                if !value
                    .metadata
                    .iter()
                    .all(|(key, value)| key == "violationsTruncated" && value == "true")
                {
                    return Err(InvalidApiError::Unsupported);
                }
                truncated = !value.metadata.is_empty();
                let parsed = api::ApiErrorReason::from_proto_name(&format!(
                    "API_ERROR_REASON_{}",
                    value.reason
                ))
                .filter(|value| *value != api::ApiErrorReason::Unspecified)
                .ok_or(InvalidApiError::Unsupported)?;
                reason = Some(parsed);
            }
            "google.rpc.BadRequest" => {
                let value: rpc::BadRequest = decode(&detail.value)?;
                if !value.__buffa_unknown_fields.is_empty() {
                    return Err(InvalidApiError::Unsupported);
                }
                if value.field_violations.len() > MAX_VIOLATIONS {
                    return Err(InvalidApiError::Limit);
                }
                for issue in value.field_violations {
                    if !issue.__buffa_unknown_fields.is_empty() || !field_path(&issue.field) {
                        return Err(InvalidApiError::Malformed);
                    }
                    let rule = api::ValidationReason::from_proto_name(&format!(
                        "VALIDATION_REASON_{}",
                        issue.reason
                    ))
                    .filter(|value| *value != api::ValidationReason::Unspecified)
                    .ok_or(InvalidApiError::Unsupported)?;
                    let admitted = FieldViolation {
                        field: issue.field,
                        rule,
                    };
                    if violations.last().is_some_and(|last: &FieldViolation| {
                        (&last.field, last.rule.proto_name())
                            >= (&admitted.field, admitted.rule.proto_name())
                    }) {
                        return Err(InvalidApiError::Inconsistent);
                    }
                    violations.push(admitted);
                }
            }
            "home.api.v2.ExecutionUncertainty" => {
                let value: api::ExecutionUncertainty = decode(&detail.value)?;
                if !value.__buffa_unknown_fields.is_empty() {
                    return Err(InvalidApiError::Unsupported);
                }
                if value.disposition
                    != EnumValue::Known(api::ReconciliationDisposition::ReconcileOnly)
                {
                    return Err(InvalidApiError::Unsupported);
                }
                uncertainty = Some(match value.phase {
                    EnumValue::Known(
                        phase @ (api::UncertaintyPhase::Commit | api::UncertaintyPhase::Effect),
                    ) => phase,
                    EnumValue::Known(api::UncertaintyPhase::Unspecified)
                    | EnumValue::Unknown(_) => return Err(InvalidApiError::Unsupported),
                });
            }
            "google.rpc.RequestInfo" => {
                let value: rpc::RequestInfo = decode(&detail.value)?;
                if !value.__buffa_unknown_fields.is_empty()
                    || !value.serving_data.is_empty()
                    || value.request_id.len() != 32
                    || !value.request_id.bytes().all(|b| b.is_ascii_hexdigit())
                {
                    return Err(InvalidApiError::Malformed);
                }
                support_reference = Some(value.request_id);
            }
            "google.rpc.PreconditionFailure" => {
                let _: rpc::PreconditionFailure = decode(&detail.value)?;
            }
            "google.rpc.QuotaFailure" => {
                let _: rpc::QuotaFailure = decode(&detail.value)?;
            }
            "google.rpc.RetryInfo" => {
                let _: rpc::RetryInfo = decode(&detail.value)?;
            }
            "google.rpc.LocalizedMessage" => {
                let _: rpc::LocalizedMessage = decode(&detail.value)?;
            }
            "google.rpc.Help" => {
                let _: rpc::Help = decode(&detail.value)?;
            }
            "google.rpc.ResourceInfo" => {
                let _: rpc::ResourceInfo = decode(&detail.value)?;
            }
            _ => return Err(InvalidApiError::Unsupported),
        }
    }
    let reason = reason.ok_or(InvalidApiError::Malformed)?;
    use api::ApiErrorReason as R;
    let expected = match reason {
        R::AuthenticationRequired => rpc::Code::UNAUTHENTICATED,
        R::AccessDenied => rpc::Code::PERMISSION_DENIED,
        R::ValidationFailed => rpc::Code::INVALID_ARGUMENT,
        R::ResourceNotFound => rpc::Code::NOT_FOUND,
        R::ResourceAlreadyExists | R::RequestIdReused => rpc::Code::ALREADY_EXISTS,
        R::EtagMismatch => rpc::Code::ABORTED,
        R::ConfigChanged
        | R::TopologyChanged
        | R::CapabilityUnavailable
        | R::SourceReadOnly
        | R::ResyncRequired => rpc::Code::FAILED_PRECONDITION,
        R::RateLimited | R::RequestTooLarge | R::StreamCapacity | R::QueryBudgetExceeded => {
            rpc::Code::RESOURCE_EXHAUSTED
        }
        R::ServiceUnavailable => rpc::Code::UNAVAILABLE,
        R::RequestCancelled => rpc::Code::CANCELLED,
        R::RequestDeadlineExceeded => rpc::Code::DEADLINE_EXCEEDED,
        R::CommitOutcomeUnknown | R::EffectOutcomeUnknown => rpc::Code::UNKNOWN,
        R::InternalFailure => rpc::Code::INTERNAL,
        R::StoredDataIntegrity => rpc::Code::DATA_LOSS,
        R::MethodNotSupported => rpc::Code::UNIMPLEMENTED,
        R::Unspecified => return Err(InvalidApiError::Unsupported),
    };
    if code != expected {
        return Err(InvalidApiError::Inconsistent);
    }
    match reason {
        api::ApiErrorReason::CommitOutcomeUnknown => {
            if uncertainty != Some(api::UncertaintyPhase::Commit) || code != rpc::Code::UNKNOWN {
                return Err(InvalidApiError::Inconsistent);
            }
        }
        api::ApiErrorReason::EffectOutcomeUnknown => {
            if uncertainty != Some(api::UncertaintyPhase::Effect) || code != rpc::Code::UNKNOWN {
                return Err(InvalidApiError::Inconsistent);
            }
        }
        api::ApiErrorReason::Unspecified => return Err(InvalidApiError::Unsupported),
        api::ApiErrorReason::AuthenticationRequired
        | api::ApiErrorReason::AccessDenied
        | api::ApiErrorReason::ValidationFailed
        | api::ApiErrorReason::ResourceNotFound
        | api::ApiErrorReason::ResourceAlreadyExists
        | api::ApiErrorReason::RequestIdReused
        | api::ApiErrorReason::EtagMismatch
        | api::ApiErrorReason::ConfigChanged
        | api::ApiErrorReason::TopologyChanged
        | api::ApiErrorReason::CapabilityUnavailable
        | api::ApiErrorReason::SourceReadOnly
        | api::ApiErrorReason::ResyncRequired
        | api::ApiErrorReason::RateLimited
        | api::ApiErrorReason::RequestTooLarge
        | api::ApiErrorReason::StreamCapacity
        | api::ApiErrorReason::QueryBudgetExceeded
        | api::ApiErrorReason::ServiceUnavailable
        | api::ApiErrorReason::RequestCancelled
        | api::ApiErrorReason::RequestDeadlineExceeded
        | api::ApiErrorReason::InternalFailure
        | api::ApiErrorReason::StoredDataIntegrity
        | api::ApiErrorReason::MethodNotSupported => {
            if uncertainty.is_some() {
                return Err(InvalidApiError::Inconsistent);
            }
        }
    }
    if truncated && violations.len() != MAX_VIOLATIONS {
        return Err(InvalidApiError::Inconsistent);
    }
    if !violations.is_empty() && reason != api::ApiErrorReason::ValidationFailed {
        return Err(InvalidApiError::Inconsistent);
    }
    Ok(ApiFailure {
        code,
        reason,
        violations,
        truncated,
        uncertainty,
        support_reference,
    })
}
