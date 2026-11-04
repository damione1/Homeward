use super::super::super::read::{IntegrationDetail, IntegrationList};
use super::*;
use crate::administration::{self, IntegrationDraft, IntegrationId};
use home_wire_contract::{
    google::longrunning,
    home::api::v2 as integration,
    integration_admin_contract::{self, IntegrationAdminRequest},
};

#[derive(Clone, Copy)]
pub(in crate::administration_host) enum DesiredState {
    Enabled,
    Disabled,
}
impl DesiredState {
    fn wire(self) -> integration::IntegrationDesiredState {
        match self {
            Self::Enabled => integration::IntegrationDesiredState::Enabled,
            Self::Disabled => integration::IntegrationDesiredState::Disabled,
        }
    }
}
#[derive(Clone, PartialEq, Eq)]
pub(in crate::administration_host::forms) struct IntegrationTypeId(IntegrationId);

#[derive(Clone)]
pub(in crate::administration_host) enum IntegrationCommand {
    Create(Box<IntegrationList>),
    Update(Box<IntegrationDetail>),
    Delete(Box<IntegrationDetail>),
    SetEnabled {
        detail: Box<IntegrationDetail>,
        state: DesiredState,
    },
}
impl IntegrationCommand {
    pub fn action(&self) -> ProAdminAction {
        match self {
            Self::Create(_) => ProAdminAction::Create,
            Self::Update(_) => ProAdminAction::Save,
            Self::Delete(_) => ProAdminAction::Remove,
            Self::SetEnabled { state, .. } => match *state {
                DesiredState::Enabled => ProAdminAction::Enable,
                DesiredState::Disabled => ProAdminAction::Disable,
            },
        }
    }
    pub fn label(&self) -> &'static str {
        match self {
            Self::Create(_) => "Create Disabled Integration",
            Self::Update(_) => "Edit Public Settings",
            Self::Delete(_) => "Delete Integration",
            Self::SetEnabled { state, .. } => match *state {
                DesiredState::Enabled => "Enable Integration",
                DesiredState::Disabled => "Disable Integration",
            },
        }
    }
}
pub(in crate::administration_host::forms) enum IntegrationForm {
    Create {
        parent: String,
        types: Vec<integration::IntegrationType>,
        selected: Option<IntegrationTypeId>,
        id: String,
        source: Zeroizing<String>,
        references: Zeroizing<String>,
    },
    Update(IntegrationDraft),
    Delete {
        name: IntegrationId,
        instance_key: String,
        etag: String,
    },
    SetEnabled {
        instance_key: String,
        request: integration::UpdateIntegrationInstanceRequest,
    },
}
impl IntegrationForm {
    pub fn open(command: &IntegrationCommand) -> Result<Self, &'static str> {
        match command {
            IntegrationCommand::Create(value) => Ok(Self::Create {
                parent: value.parent.clone(),
                types: value.types.clone(),
                selected: None,
                id: String::new(),
                source: Zeroizing::new(String::new()),
                references: Zeroizing::new(String::new()),
            }),
            IntegrationCommand::Update(value) => IntegrationDraft::open(&value.instance)
                .map(Self::Update)
                .map_err(|_| "This configuration is read-only."),
            IntegrationCommand::Delete(value) => {
                administration::admit_integration_instance(&value.instance)
                    .map_err(|_| "The workspace version is unavailable for writing.")?;
                Ok(Self::Delete {
                    name: IntegrationId::parse(&value.instance.name)
                        .map_err(|_| "Invalid identity.")?,
                    instance_key: value.instance.instance_key.clone(),
                    etag: value.instance.etag.clone(),
                })
            }
            IntegrationCommand::SetEnabled { detail, state } => {
                let request = administration::prepare_enabled(&detail.instance, state.wire())
                    .map_err(|_| "The workspace version is unavailable for writing.")?;
                Ok(Self::SetEnabled {
                    instance_key: detail.instance.instance_key.clone(),
                    request,
                })
            }
        }
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        match self {
            Self::Create {
                types,
                selected,
                id,
                source,
                references,
                ..
            } => {
                let mut fields = vec![
                    field(Field::Identity, "Instance ID", id, false, true),
                    field(Field::Fragment, "Public Settings YAML", source, true, true),
                    field(
                        Field::References,
                        "Secret References · field=name",
                        references,
                        true,
                        true,
                    ),
                ];
                if let Some(kind) = selected
                    .as_ref()
                    .and_then(|id| types.iter().find(|kind| kind.name == id.0.as_str()))
                {
                    fields.push(field(
                        Field::Schema,
                        "Schema Version",
                        &kind.schema_version,
                        false,
                        false,
                    ));
                }
                fields
            }
            Self::Update(value) => {
                let mut fields = vec![
                    field(
                        Field::Identity,
                        "Affected Instance",
                        value.instance_key(),
                        false,
                        false,
                    ),
                    field(
                        Field::Fragment,
                        "Public Settings YAML",
                        value.yaml(),
                        true,
                        true,
                    ),
                ];
                for reference in value.secret_references() {
                    fields.push(field(
                        Field::References,
                        &format!("Retained reference · {}", reference.field),
                        &reference.secret_key,
                        false,
                        false,
                    ));
                }
                fields
            }
            Self::Delete { instance_key, .. } => vec![field(
                Field::Identity,
                "Instance to Delete",
                instance_key,
                false,
                false,
            )],
            Self::SetEnabled { instance_key, .. } => vec![field(
                Field::Identity,
                "Affected Instance",
                instance_key,
                false,
                false,
            )],
        }
    }
    pub fn choices(&self) -> Vec<ProAdminChoice> {
        match self {
            Self::Create {
                types, selected, ..
            } => types
                .iter()
                .enumerate()
                .map(|(index, kind)| {
                    choice(
                        ChoiceField::IntegrationType,
                        i32::try_from(index + 1).expect("bounded catalogue"),
                        &kind.type_key,
                        selected
                            .as_ref()
                            .is_some_and(|id| id.0.as_str() == kind.name),
                    )
                })
                .collect(),
            Self::Update(_) | Self::Delete { .. } | Self::SetEnabled { .. } => Vec::new(),
        }
    }
    pub fn edit(&mut self, token: i32, value: String) -> Result<(), &'static str> {
        let Some(field) = Field::parse(token) else {
            return Ok(());
        };
        match self {
            Self::Create {
                id,
                source,
                references,
                ..
            } => match field {
                Field::Identity => {
                    if value.len() > 256 || value.contains(['\0', '\r', '\n']) {
                        return Err("The identity must be valid and at most 256 bytes.");
                    }
                    *id = value;
                }
                Field::Fragment => {
                    fragment(&value)?;
                    *source = Zeroizing::new(value);
                }
                Field::References => {
                    if value.len() > 16384 || value.contains('\0') {
                        return Err("The reference list is too long.");
                    }
                    *references = Zeroizing::new(value);
                }
                Field::PolicySource | Field::Schema => (),
            },
            Self::Update(draft) => {
                if field == Field::Fragment {
                    draft
                        .edit_yaml(&value)
                        .map_err(|_| "The public fragment is invalid or no longer editable.")?;
                }
            }
            Self::Delete { .. } | Self::SetEnabled { .. } => (),
        }
        Ok(())
    }
    pub fn choose(&mut self, field: i32, option: i32) -> Result<(), &'static str> {
        if ChoiceField::parse(field) != Some(ChoiceField::IntegrationType) {
            return Ok(());
        }
        if let Self::Create {
            types, selected, ..
        } = self
            && let Some(index) = usize::try_from(option)
                .ok()
                .and_then(|value| value.checked_sub(1))
            && index < types.len()
        {
            *selected = Some(IntegrationTypeId(
                IntegrationId::parse(&types[index].name)
                    .map_err(|_| "Invalid integration type.")?,
            ));
        }
        Ok(())
    }
    pub fn submit(&self) -> Result<Operation, &'static str> {
        match self {
            Self::Create {
                parent,
                types,
                selected,
                id,
                source,
                references,
            } => {
                let kind = selected
                    .as_ref()
                    .and_then(|id| types.iter().find(|kind| kind.name == id.0.as_str()))
                    .ok_or("Select a server-provided integration type.")?;
                IntegrationId::parse(id).map_err(|_| "Enter a valid instance ID.")?;
                fragment(source)?;
                let mut fields = std::collections::BTreeSet::new();
                let mut secrets = Vec::new();
                for line in references.lines().filter(|line| !line.trim().is_empty()) {
                    let (field, name) = line.split_once('=').ok_or(
                        "Each reference must use field=name format, without a secret value.",
                    )?;
                    let (field, name) = (field.trim(), name.trim());
                    if field.is_empty()
                        || name.is_empty()
                        || field.len() > 256
                        || name.len() > 256
                        || field.contains(['\r', '\n', '\0'])
                        || name.contains(['\r', '\n', '\0'])
                        || !fields.insert(field)
                    {
                        return Err("A reference is empty, duplicated or invalid.");
                    }
                    secrets.push(integration::IntegrationSecretReference {
                        field: field.into(),
                        secret_key: name.into(),
                        instance_key: id.clone(),
                        ..Default::default()
                    });
                }
                let request = integration::CreateIntegrationInstanceRequest {
                    parent: parent.clone(),
                    integration_instance: integration::IntegrationInstance {
                        integration_type: kind.name.clone(),
                        instance_key: id.clone(),
                        desired_state: integration::IntegrationDesiredState::Disabled.into(),
                        configuration: Some(
                            integration::integration_instance::Configuration::Editable(Box::new(
                                integration::IntegrationConfiguration {
                                    schema_version: kind.schema_version.clone(),
                                    yaml_source: source.as_bytes().to_vec(),
                                    secrets,
                                    ..Default::default()
                                },
                            )),
                        ),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                };
                integration_admin_contract::admit_integration_admin_request(
                    IntegrationAdminRequest::Create(&request),
                )
                .map_err(|_| "The fragment or its references do not meet the server contract.")?;
                Ok(Operation::Create(request))
            }
            Self::Update(draft) => draft
                .prepare_update()
                .map(Operation::Update)
                .map_err(|_| "The public fragment or its references are invalid."),
            Self::Delete { name, etag, .. } => {
                let request = integration::DeleteIntegrationInstanceRequest {
                    name: name.as_str().into(),
                    etag: etag.clone(),
                    ..Default::default()
                };
                integration_admin_contract::admit_integration_admin_request(
                    IntegrationAdminRequest::Delete(&request),
                )
                .map_err(|_| "Invalid deletion preconditions.")?;
                Ok(Operation::Delete(request))
            }
            Self::SetEnabled { request, .. } => Ok(Operation::Update(request.clone())),
        }
    }
}
fn fragment(value: &str) -> Result<(), &'static str> {
    if value.len() > administration::MAX_FRAGMENT_BYTES || value.contains('\0') {
        Err("The public fragment is limited to 64 KiB of UTF-8 text.")
    } else {
        Ok(())
    }
}
pub(super) fn operation(value: &longrunning::Operation) -> Completion {
    Completion::state(MutationState::integration_operation(value).unwrap_or(MutationState::Uncertain))
}
