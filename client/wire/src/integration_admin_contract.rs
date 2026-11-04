//! Administrative boundary only. No source store, driver or transport.
use crate::{configuration_contract, home::api::v2 as wire};
use buffa::EnumValue;
use home_client_model::api::RequestId;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IntegrationAdminAdmissionError {
    Invalid,
}
pub enum IntegrationAdminRequest<'a> {
    Types(&'a wire::ListIntegrationTypesRequest),
    Type(&'a wire::GetIntegrationTypeRequest),
    Instances(&'a wire::ListIntegrationInstancesRequest),
    Instance(&'a wire::GetIntegrationInstanceRequest),
    Create(&'a wire::CreateIntegrationInstanceRequest),
    Update(&'a wire::UpdateIntegrationInstanceRequest),
    Delete(&'a wire::DeleteIntegrationInstanceRequest),
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
fn valid_segment(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 63
        && !value.starts_with('-')
        && !value.ends_with('-')
        && value
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}
fn parse_resource<'a>(value: &'a str, collection: &str) -> Option<&'a str> {
    let (home, id_seg) = value.split_once(&format!("/{collection}/"))?;
    if configuration_contract::home_name(home) && valid_segment(id_seg) {
        Some(home)
    } else {
        None
    }
}
fn etag(value: &str) -> bool {
    value.len() > 4
        && value.len() <= 256
        && value.starts_with("W/\"")
        && value.ends_with('"')
        && value[3..value.len() - 1]
            .chars()
            .all(|c| c >= '\u{21}' && c != '\u{7f}' && c != '"')
}
fn request_id(value: &str) -> bool {
    value.is_empty() || RequestId::try_from(value).is_ok()
}
fn page(page_size: i32, page_token: &str) -> bool {
    page_size >= 0 && page_token.len() <= 2048 && !page_token.contains(['\0', '\r', '\n'])
}
fn state(value: EnumValue<wire::IntegrationDesiredState>) -> bool {
    match value {
        EnumValue::Known(
            wire::IntegrationDesiredState::Enabled | wire::IntegrationDesiredState::Disabled,
        ) => true,
        EnumValue::Known(wire::IntegrationDesiredState::Unspecified) | EnumValue::Unknown(_) => {
            false
        }
    }
}
fn configuration(value: &wire::IntegrationConfiguration, instance_key: &str) -> bool {
    if !value.__buffa_unknown_fields.is_empty()
        || !id(&value.schema_version)
        || !(1..=65_536).contains(&value.yaml_source.len())
        || std::str::from_utf8(&value.yaml_source).is_err()
        || value.yaml_source.contains(&0)
        || value.secrets.len() > 32
    {
        return false;
    }
    let mut seen = std::collections::BTreeSet::new();
    value.secrets.iter().all(|reference| {
        reference.__buffa_unknown_fields.is_empty()
            && id(&reference.field)
            && reference.instance_key == instance_key
            && id(&reference.secret_key)
            && seen.insert(reference.field.as_str())
    })
}
fn editable_instance(value: &wire::IntegrationInstance, home: &str) -> bool {
    if !value.__buffa_unknown_fields.is_empty()
        || !id(&value.instance_key)
        || parse_resource(&value.integration_type, "integrationTypes") != Some(home)
    {
        return false;
    }
    match value.configuration.as_ref() {
        Some(wire::integration_instance::Configuration::Editable(cfg)) => {
            configuration(cfg, &value.instance_key)
        }
        Some(wire::integration_instance::Configuration::Opaque(_)) | None => false,
    }
}
pub fn admit_integration_admin_request(
    value: IntegrationAdminRequest<'_>,
) -> Result<AdmittedIntegrationAdminRequest<'_>, IntegrationAdminAdmissionError> {
    let valid = match &value {
        IntegrationAdminRequest::Types(v) => {
            v.__buffa_unknown_fields.is_empty()
                && configuration_contract::home_name(&v.parent)
                && page(v.page_size, &v.page_token)
        }
        IntegrationAdminRequest::Type(v) => {
            v.__buffa_unknown_fields.is_empty()
                && parse_resource(&v.name, "integrationTypes").is_some()
        }
        IntegrationAdminRequest::Instances(v) => {
            v.__buffa_unknown_fields.is_empty()
                && configuration_contract::home_name(&v.parent)
                && page(v.page_size, &v.page_token)
        }
        IntegrationAdminRequest::Instance(v) => {
            v.__buffa_unknown_fields.is_empty()
                && parse_resource(&v.name, "integrationInstances").is_some()
        }
        IntegrationAdminRequest::Create(v) => {
            configuration_contract::requested_resource_id(&v.integration_instance_id)
                && v.__buffa_unknown_fields.is_empty()
                && configuration_contract::home_name(&v.parent)
                && request_id(&v.request_id)
                && v.integration_instance.as_option().is_some_and(|inst| {
                    state(inst.desired_state) && editable_instance(inst, &v.parent)
                })
        }
        IntegrationAdminRequest::Update(v) => {
            v.__buffa_unknown_fields.is_empty()
                && request_id(&v.request_id)
                && v.integration_instance
                    .as_option()
                    .zip(v.update_mask.as_option())
                    .is_some_and(|(inst, mask)| {
                        if !inst.__buffa_unknown_fields.is_empty()
                            || !mask.__buffa_unknown_fields.is_empty()
                            || mask.paths.is_empty()
                            || mask.paths.len() > 8
                            || !etag(&inst.etag)
                        {
                            return false;
                        }
                        let Some(home) = parse_resource(&inst.name, "integrationInstances") else {
                            return false;
                        };
                        let mut seen = std::collections::BTreeSet::new();
                        let mut want_state = false;
                        let mut want_editable = false;
                        for path in &mask.paths {
                            if !seen.insert(path.as_str()) {
                                return false;
                            }
                            match path.as_str() {
                                "*" => {
                                    want_state = true;
                                    want_editable = true;
                                }
                                "desired_state" => want_state = true,
                                "editable" => want_editable = true,
                                _ => return false,
                            }
                        }
                        (!want_state || state(inst.desired_state))
                            && (!want_editable || editable_instance(inst, home))
                    })
        }
        IntegrationAdminRequest::Delete(v) => {
            v.__buffa_unknown_fields.is_empty()
                && parse_resource(&v.name, "integrationInstances").is_some()
                && etag(&v.etag)
                && request_id(&v.request_id)
        }
    };
    if valid {
        Ok(AdmittedIntegrationAdminRequest { wire: value })
    } else {
        Err(IntegrationAdminAdmissionError::Invalid)
    }
}
