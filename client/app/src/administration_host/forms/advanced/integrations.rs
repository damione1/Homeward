use super::super::super::read::{IntegrationDetail, IntegrationList};
use super::*;
use crate::administration::{self, IntegrationDraft, IntegrationId};
use home_wire_contract::{
    home::{configuration::v1 as config, integrationadmin::v1 as integration},
    integration_admin_contract::{self, IntegrationAdminRequest},
};

#[derive(Clone, Copy)]
pub(in crate::administration_host) enum DesiredState {
    Enabled,
    Disabled,
}
impl DesiredState {
    fn wire(self) -> integration::DesiredState {
        match self {
            Self::Enabled => integration::DesiredState::Enabled,
            Self::Disabled => integration::DesiredState::Disabled,
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
        types: Vec<integration::IntegrationType>,
        selected: Option<IntegrationTypeId>,
        id: String,
        source: Zeroizing<String>,
        references: Zeroizing<String>,
        expected: integration::MutationPrecondition,
    },
    Update(IntegrationDraft),
    Delete {
        id: IntegrationId,
        expected: integration::MutationPrecondition,
    },
    SetEnabled(integration::SetInstanceEnabledRequest),
}
impl IntegrationForm {
    pub fn open(command: &IntegrationCommand) -> Result<Self, &'static str> {
        match command {
            IntegrationCommand::Create(value) => Ok(Self::Create {
                types: value.types.clone(),
                selected: None,
                id: String::new(),
                source: Zeroizing::new(String::new()),
                references: Zeroizing::new(String::new()),
                expected: administration::mutation_precondition(&value.response.document)
                    .map_err(|_| "The workspace version is unavailable for writing.")?,
            }),
            IntegrationCommand::Update(value) => {
                IntegrationDraft::open(&value.response.instance, &value.response.document)
                    .map(Self::Update)
                    .map_err(|_| "This configuration is read-only.")
            }
            IntegrationCommand::Delete(value) => Ok(Self::Delete {
                id: IntegrationId::parse(&value.response.instance.integration_instance_id)
                    .map_err(|_| "Invalid identity.")?,
                expected: administration::mutation_precondition(&value.response.document)
                    .map_err(|_| "The workspace version is unavailable for writing.")?,
            }),
            IntegrationCommand::SetEnabled { detail, state } => administration::prepare_enabled(
                &detail.response.instance,
                &detail.response.document,
                state.wire(),
            )
            .map(Self::SetEnabled)
            .map_err(|_| "The workspace version is unavailable for writing."),
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
                    .and_then(|id| types.iter().find(|kind| kind.type_id == id.0.as_str()))
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
                        value.id().as_str(),
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
                        &reference.name,
                        false,
                        false,
                    ));
                }
                fields
            }
            Self::Delete { id, .. } => vec![field(
                Field::Identity,
                "Instance to Delete",
                id.as_str(),
                false,
                false,
            )],
            Self::SetEnabled(request) => vec![field(
                Field::Identity,
                "Affected Instance",
                &request.integration_instance_id,
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
                        &kind.type_id,
                        selected
                            .as_ref()
                            .is_some_and(|id| id.0.as_str() == kind.type_id),
                    )
                })
                .collect(),
            Self::Update(_) | Self::Delete { .. } | Self::SetEnabled(_) => Vec::new(),
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
            Self::Delete { .. } | Self::SetEnabled(_) => (),
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
                IntegrationId::parse(&types[index].type_id)
                    .map_err(|_| "Invalid integration type.")?,
            ));
        }
        Ok(())
    }
    pub fn submit(&self) -> Result<Operation, &'static str> {
        match self {
            Self::Create {
                types,
                selected,
                id,
                source,
                references,
                expected,
            } => {
                let kind = selected
                    .as_ref()
                    .and_then(|id| types.iter().find(|kind| kind.type_id == id.0.as_str()))
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
                    secrets.push(integration::SecretReference {
                        field: field.into(),
                        name: name.into(),
                        integration_instance_id: id.clone(),
                        ..Default::default()
                    });
                }
                let request = integration::CreateInstanceRequest {
                    type_id: kind.type_id.clone(),
                    integration_instance_id: id.clone(),
                    desired_state: integration::DesiredState::Disabled.into(),
                    configuration: integration::PublicConfiguration {
                        schema_version: kind.schema_version.clone(),
                        yaml_source: source.as_bytes().to_vec(),
                        secrets,
                        ..Default::default()
                    }
                    .into(),
                    precondition: expected.clone().into(),
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
            Self::Delete { id, expected } => {
                let request = integration::DeleteInstanceRequest {
                    integration_instance_id: id.as_str().into(),
                    precondition: expected.clone().into(),
                    ..Default::default()
                };
                integration_admin_contract::admit_integration_admin_request(
                    IntegrationAdminRequest::Delete(&request),
                )
                .map_err(|_| "Invalid deletion preconditions.")?;
                Ok(Operation::Delete(request))
            }
            Self::SetEnabled(request) => Ok(Operation::SetEnabled(request.clone())),
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
pub(super) fn activation(
    value: &config::ActivationResult,
    document: &config::ReadDocumentResponse,
) -> Completion {
    let state =
        if home_wire_contract::configuration_contract::admit_configuration_read(document).is_ok() {
            MutationState::activation(value).unwrap_or(MutationState::Uncertain)
        } else {
            MutationState::Uncertain
        };
    Completion::state(state)
}
