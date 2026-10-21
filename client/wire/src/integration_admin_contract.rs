//! Administrative boundary only. No source store, driver or transport.
use crate::home::{configuration::v1 as config, integrationadmin::v1 as wire};
use buffa::EnumValue;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IntegrationAdminAdmissionError {
    Invalid,
}
pub enum IntegrationAdminRequest<'a> {
    Types(&'a wire::ListIntegrationTypesRequest),
    Type(&'a wire::GetIntegrationTypeRequest),
    Instances(&'a wire::ListInstancesRequest),
    Instance(&'a wire::GetInstanceRequest),
    Create(&'a wire::CreateInstanceRequest),
    Update(&'a wire::UpdateInstanceRequest),
    Delete(&'a wire::DeleteInstanceRequest),
    SetEnabled(&'a wire::SetInstanceEnabledRequest),
}
pub struct AdmittedIntegrationAdminRequest<'a> {
    wire: IntegrationAdminRequest<'a>,
}
impl<'a> AdmittedIntegrationAdminRequest<'a> {
    pub fn wire(&self) -> &IntegrationAdminRequest<'a> {
        &self.wire
    }
}
fn id(value: &str) -> bool {
    !value.trim().is_empty() && value.len() <= 256 && !value.contains(['\0', '\r', '\n'])
}
fn hash(value: &[u8]) -> bool {
    value.len() == 32 && value.iter().any(|byte| *byte != 0)
}
fn version(value: &config::DocumentVersion) -> bool {
    value.__buffa_unknown_fields.is_empty() && value.revision > 0 && hash(&value.source_sha256)
}
fn expected(value: &wire::MutationPrecondition) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.expected.as_option().is_some_and(version)
        && hash(&value.expected_workspace_sha256)
}
fn state(value: EnumValue<wire::DesiredState>) -> bool {
    match value {
        EnumValue::Known(wire::DesiredState::Enabled | wire::DesiredState::Disabled) => true,
        EnumValue::Known(wire::DesiredState::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn configuration(value: &wire::PublicConfiguration, instance: &str) -> bool {
    if !value.__buffa_unknown_fields.is_empty()
        || !id(&value.schema_version)
        || !(1..=65_536).contains(&value.yaml_source.len())
        || value.secrets.len() > 32
    {
        return false;
    }
    let mut seen = std::collections::BTreeSet::new();
    value.secrets.iter().all(|reference| {
        reference.__buffa_unknown_fields.is_empty()
            && id(&reference.field)
            && reference.integration_instance_id == instance
            && id(&reference.name)
            && seen.insert(reference.field.as_str())
    })
}
pub fn admit_integration_admin_request(
    value: IntegrationAdminRequest<'_>,
) -> Result<AdmittedIntegrationAdminRequest<'_>, IntegrationAdminAdmissionError> {
    let valid = match &value {
        IntegrationAdminRequest::Types(v) => v.__buffa_unknown_fields.is_empty(),
        IntegrationAdminRequest::Type(v) => v.__buffa_unknown_fields.is_empty() && id(&v.type_id),
        IntegrationAdminRequest::Instances(v) => v.__buffa_unknown_fields.is_empty(),
        IntegrationAdminRequest::Instance(v) => {
            v.__buffa_unknown_fields.is_empty() && id(&v.integration_instance_id)
        }
        IntegrationAdminRequest::Create(v) => {
            v.__buffa_unknown_fields.is_empty()
                && id(&v.type_id)
                && id(&v.integration_instance_id)
                && state(v.desired_state)
                && v.configuration
                    .as_option()
                    .is_some_and(|c| configuration(c, &v.integration_instance_id))
                && v.precondition.as_option().is_some_and(expected)
        }
        IntegrationAdminRequest::Update(v) => {
            v.__buffa_unknown_fields.is_empty()
                && id(&v.type_id)
                && id(&v.integration_instance_id)
                && v.configuration
                    .as_option()
                    .is_some_and(|c| configuration(c, &v.integration_instance_id))
                && v.precondition.as_option().is_some_and(expected)
        }
        IntegrationAdminRequest::Delete(v) => {
            v.__buffa_unknown_fields.is_empty()
                && id(&v.integration_instance_id)
                && v.precondition.as_option().is_some_and(expected)
        }
        IntegrationAdminRequest::SetEnabled(v) => {
            v.__buffa_unknown_fields.is_empty()
                && id(&v.integration_instance_id)
                && state(v.desired_state)
                && v.precondition.as_option().is_some_and(expected)
        }
    };
    if valid {
        Ok(AdmittedIntegrationAdminRequest { wire: value })
    } else {
        Err(IntegrationAdminAdmissionError::Invalid)
    }
}
