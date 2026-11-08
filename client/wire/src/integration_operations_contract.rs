//! Integration receipts share the standard envelope, never configuration's
//! success vocabulary. NoChange is a validated original capture, not a commit.
use crate::{
    api_errors, api_operations, configuration_contract as configuration,
    google::longrunning::{self, operation::Result as OperationResult},
    home::api::v2 as wire,
};
use buffa::{EnumValue, Message};
use home_client_model::api::{OperationName, RequestId};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InvalidIntegrationOperation;

#[derive(Debug, Clone, PartialEq)]
pub struct Receipt {
    name: OperationName,
    metadata: wire::IntegrationOperationMetadata,
    result: Option<OperationResult>,
}
impl Receipt {
    pub fn name(&self) -> &OperationName {
        &self.name
    }
    pub fn phase(&self) -> wire::IntegrationOperationPhase {
        match self.metadata.phase {
            EnumValue::Known(p) => p,
            EnumValue::Unknown(_) => unreachable!(),
        }
    }
    /// Accepted may gain a target once Create has obtained its durable name.
    /// A committed proof and every terminal result are immutable.
    pub fn follows(&self, previous: &Self) -> bool {
        use wire::IntegrationOperationPhase as P;
        if self.name != previous.name
            || self.metadata.kind != previous.metadata.kind
            || self.metadata.request_id != previous.metadata.request_id
            || self.metadata.create_time != previous.metadata.create_time
            || (!previous.metadata.target.is_empty()
                && self.metadata.target != previous.metadata.target)
        {
            return false;
        }
        match previous.phase() {
            P::Accepted => true,
            P::Committed => {
                matches!(self.phase(), P::Committed | P::Published)
                    && self.metadata.commit_time == previous.metadata.commit_time
                    && self.metadata.generation == previous.metadata.generation
                    && self.metadata.generation_revision == previous.metadata.generation_revision
                    && self.metadata.manifest_sha256 == previous.metadata.manifest_sha256
            }
            P::Published | P::NoChange | P::Failed => self == previous,
            P::Unspecified => false,
        }
    }
}

fn resource(value: &str, home: &str, collection: &str) -> bool {
    let prefix = format!("{home}/{collection}/");
    value.strip_prefix(&prefix).is_some_and(|id| {
        !id.is_empty()
            && id.len() <= 63
            && !id.starts_with('-')
            && !id.ends_with('-')
            && id
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
    })
}
fn native_id(value: &str) -> bool {
    !value.trim().is_empty() && value.len() <= 256 && !value.contains(['\0', '\r', '\n'])
}
fn definition(v: &wire::IntegrationInstance, home: &str) -> bool {
    if !v.__buffa_unknown_fields.is_empty()
        || !native_id(&v.instance_key)
        || !resource(&v.name, home, "integrationInstances")
        || !resource(&v.configuration_document, home, "configurationDocuments")
        || v.observed_health.is_some()
        || v.observed_supervision.is_some()
        || !matches!(
            v.desired_state,
            EnumValue::Known(
                wire::IntegrationDesiredState::Enabled | wire::IntegrationDesiredState::Disabled
            )
        )
        || v.etag.len() > 256
        || v.etag.len() <= 4
        || !v.etag.starts_with("W/\"")
        || !v.etag.ends_with('"')
        || v.etag[3..v.etag.len() - 1]
            .chars()
            .any(|c| c < '\u{21}' || c == '\u{7f}' || c == '"')
    {
        return false;
    }
    use wire::integration_instance::Configuration;
    let (secrets, opaque) = match &v.configuration {
        Some(Configuration::Editable(c)) => {
            if !c.__buffa_unknown_fields.is_empty()
                || !resource(&v.integration_type, home, "integrationTypes")
                || !native_id(&c.schema_version)
                || c.yaml_source.is_empty()
                || c.yaml_source.len() > 64 * 1024
                || std::str::from_utf8(&c.yaml_source).is_err()
            {
                return false;
            }
            (&c.secrets, false)
        }
        Some(Configuration::Opaque(c)) => {
            if !c.__buffa_unknown_fields.is_empty() || !v.integration_type.is_empty() {
                return false;
            }
            (&c.secrets, true)
        }
        None => return false,
    };
    let mut seen = std::collections::BTreeSet::new();
    secrets.len() <= 32
        && secrets.iter().all(|s| {
            s.__buffa_unknown_fields.is_empty()
                && native_id(&s.field)
                && native_id(&s.secret_key)
                && s.instance_key == v.instance_key
                && (!opaque || s.field == "secret_names")
                && seen.insert(if opaque {
                    s.secret_key.as_str()
                } else {
                    s.field.as_str()
                })
        })
}

pub fn admit_response(
    raw: &longrunning::Operation,
) -> Result<Receipt, InvalidIntegrationOperation> {
    let invalid = InvalidIntegrationOperation;
    let name = OperationName::try_from(raw.name.as_str()).map_err(|_| invalid)?;
    api_operations::admit_envelope(raw, &name).map_err(|_| invalid)?;
    let (home, id) = name.as_str().split_once("/operations/").ok_or(invalid)?;
    let attempt = id.strip_prefix("config-").ok_or(invalid)?;
    if attempt.len() != 32
        || attempt.bytes().all(|b| b == b'0')
        || !attempt
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        || raw.metadata.type_url != "type.googleapis.com/home.api.v2.IntegrationOperationMetadata"
    {
        return Err(invalid);
    }
    let m = wire::IntegrationOperationMetadata::decode_from_slice(&raw.metadata.value)
        .map_err(|_| invalid)?;
    if !m.__buffa_unknown_fields.is_empty()
        || !m
            .create_time
            .as_option()
            .is_some_and(configuration::timestamp)
        || (!m.request_id.is_empty() && RequestId::try_from(m.request_id.as_str()).is_err())
    {
        return Err(invalid);
    }
    use wire::{IntegrationMutationKind as K, IntegrationOperationPhase as P};
    let kind = match m.kind {
        EnumValue::Known(K::Create) => K::Create,
        EnumValue::Known(K::Update) => K::Update,
        EnumValue::Known(K::Delete) => K::Delete,
        EnumValue::Known(K::Unspecified) | EnumValue::Unknown(_) => return Err(invalid),
    };
    if (m.target.is_empty() && kind != K::Create)
        || (!m.target.is_empty() && !resource(&m.target, home, "integrationInstances"))
    {
        return Err(invalid);
    }
    let no_commit = !m.commit_time.is_set()
        && m.generation.is_empty()
        && m.generation_revision == 0
        && m.manifest_sha256.is_empty();
    let created = (m.create_time.seconds, m.create_time.nanos);
    let phase = match m.phase {
        EnumValue::Known(p) => p,
        _ => return Err(invalid),
    };
    match phase {
        P::Accepted | P::Failed => {
            if !no_commit || m.validation_time.is_set() || raw.done != (phase == P::Failed) {
                return Err(invalid);
            }
            if phase == P::Failed {
                let Some(OperationResult::Error(status)) = &raw.result else {
                    return Err(invalid);
                };
                let failure = api_errors::admit_status(status).map_err(|_| invalid)?;
                if matches!(
                    failure.reason(),
                    wire::ApiErrorReason::CommitOutcomeUnknown
                        | wire::ApiErrorReason::EffectOutcomeUnknown
                ) {
                    return Err(invalid);
                }
            }
        }
        P::NoChange => {
            if !no_commit
                || !raw.done
                || kind != K::Update
                || !m
                    .validation_time
                    .as_option()
                    .is_some_and(configuration::timestamp)
                || (m.validation_time.seconds, m.validation_time.nanos) < created
            {
                return Err(invalid);
            }
        }
        P::Committed | P::Published => {
            if m.target.is_empty()
                || m.validation_time.is_set()
                || !m
                    .commit_time
                    .as_option()
                    .is_some_and(configuration::timestamp)
                || (m.commit_time.seconds, m.commit_time.nanos) < created
                || m.generation_revision < 1
                || m.manifest_sha256.len() != 32
                || m.manifest_sha256.iter().all(|b| *b == 0)
                || m.generation
                    != format!(
                        "{home}/configurationGenerations/g-{}",
                        m.generation_revision
                    )
                || raw.done != (phase == P::Published)
            {
                return Err(invalid);
            }
        }
        P::Unspecified => return Err(invalid),
    }
    if matches!(phase, P::Published | P::NoChange) {
        let Some(OperationResult::Response(result)) = &raw.result else {
            return Err(invalid);
        };
        if kind == K::Delete {
            if result.type_url != "type.googleapis.com/google.protobuf.Empty"
                || !buffa_types::google::protobuf::Empty::decode_from_slice(&result.value)
                    .map_err(|_| invalid)?
                    .__buffa_unknown_fields
                    .is_empty()
            {
                return Err(invalid);
            }
        } else {
            if result.type_url != "type.googleapis.com/home.api.v2.IntegrationInstance"
                || result.value.len() > 128 * 1024
            {
                return Err(invalid);
            }
            let result =
                wire::IntegrationInstance::decode_from_slice(&result.value).map_err(|_| invalid)?;
            if !definition(&result, home) || result.name != m.target {
                return Err(invalid);
            }
        }
    }
    Ok(Receipt {
        name,
        metadata: m,
        result: raw.result.clone(),
    })
}
