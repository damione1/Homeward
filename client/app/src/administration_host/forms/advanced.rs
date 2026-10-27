pub(super) mod integrations;
pub(in crate::administration_host) mod policy;
use super::{
    Completion, MutationState, ProAdminAction, ProAdminChoice, ProAdminField, ProAdminFieldKind,
    Section,
};
use home_client_session::{AccessLease, SessionCoordinator};
use home_client_transport::{Connection, FailureKind, RpcCode};
use home_wire_contract::{
    auth_contract::AccountId,
    home::{auth::v1 as auth, integrationadmin::v1 as integration},
};
use std::time::Duration;
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

#[derive(Clone)]
pub(in crate::administration_host) enum Command {
    Policy(policy::PolicyCommand),
    Integration(integrations::IntegrationCommand),
}
impl Command {
    pub fn action(&self) -> ProAdminAction {
        match self {
            Self::Policy(_) => ProAdminAction::Save,
            Self::Integration(value) => value.action(),
        }
    }
    pub fn section(&self) -> Section {
        match self {
            Self::Policy(_) => Section::Policies,
            Self::Integration(_) => Section::Integrations,
        }
    }
    pub fn label(&self) -> &'static str {
        match self {
            Self::Policy(_) => "Modifier les droits",
            Self::Integration(value) => value.label(),
        }
    }
    pub fn destructive(&self) -> bool {
        match self {
            Self::Policy(_) => true,
            Self::Integration(value) => {
                matches!(value, integrations::IntegrationCommand::Delete(_))
            }
        }
    }
}
#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
pub(in crate::administration_host) enum Field {
    PolicySource = 100,
    Identity = 101,
    Fragment = 102,
    References = 103,
    Schema = 104,
}
impl Field {
    fn parse(value: i32) -> Option<Self> {
        match value {
            100 => Some(Self::PolicySource),
            101 => Some(Self::Identity),
            102 => Some(Self::Fragment),
            103 => Some(Self::References),
            104 => Some(Self::Schema),
            _ => None,
        }
    }
}
#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
pub(in crate::administration_host) enum ChoiceField {
    PolicyPreset = 100,
    IntegrationType = 101,
}
impl ChoiceField {
    fn parse(value: i32) -> Option<Self> {
        match value {
            100 => Some(Self::PolicyPreset),
            101 => Some(Self::IntegrationType),
            _ => None,
        }
    }
}
pub(super) enum Draft {
    Policy(policy::PolicyDraft),
    Integration(integrations::IntegrationForm),
}
impl Draft {
    pub fn open(command: &Command) -> Result<Self, &'static str> {
        match command {
            Command::Policy(value) => policy::PolicyDraft::open(value).map(Self::Policy),
            Command::Integration(value) => {
                integrations::IntegrationForm::open(value).map(Self::Integration)
            }
        }
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        match self {
            Self::Policy(value) => value.fields(),
            Self::Integration(value) => value.fields(),
        }
    }
    pub fn choices(&self) -> Vec<ProAdminChoice> {
        match self {
            Self::Policy(value) => value.choices(),
            Self::Integration(value) => value.choices(),
        }
    }
    pub fn edit(&mut self, field: i32, value: String) -> Result<(), &'static str> {
        match self {
            Self::Policy(draft) => draft.edit(field, value),
            Self::Integration(draft) => draft.edit(field, value),
        }
    }
    pub fn choose(&mut self, field: i32, option: i32) -> Result<(), &'static str> {
        match self {
            Self::Policy(draft) => draft.choose(field, option),
            Self::Integration(draft) => draft.choose(field, option),
        }
    }
    pub fn submit(&self) -> Result<Operation, &'static str> {
        match self {
            Self::Policy(value) => value.submit(),
            Self::Integration(value) => value.submit(),
        }
    }
}
pub(in crate::administration_host) enum Operation {
    Policy(auth::ReplacePolicyRequest),
    Create(integration::CreateInstanceRequest),
    Update(integration::UpdateInstanceRequest),
    Delete(integration::DeleteInstanceRequest),
    SetEnabled(integration::SetInstanceEnabledRequest),
}
fn field(token: Field, label: &str, value: &str, multiline: bool, editable: bool) -> ProAdminField {
    ProAdminField {
        token: token as i32,
        label: label.into(),
        description: "Source et préconditions fournies par le serveur".into(),
        value: value.into(),
        kind: if multiline {
            ProAdminFieldKind::Multiline
        } else {
            ProAdminFieldKind::Plain
        },
        editable,
    }
}
fn choice(field: ChoiceField, token: i32, label: &str, selected: bool) -> ProAdminChoice {
    ProAdminChoice {
        token,
        field_token: field as i32,
        label: label.into(),
        selected,
        enabled: true,
    }
}
pub(super) async fn execute(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    operation: Operation,
    cancel: &CancellationToken,
) -> Completion {
    macro_rules! call {
        ($api:expr,$method:ident,$request:expr) => {{
            let api = $api;
            match api
                .$method($request, lease.token(), Duration::from_secs(4), cancel)
                .await
            {
                Ok(value) => value,
                Err(error) => {
                    let _ = session.observe_failure(lease.generation, &error).await;
                    return Completion::state(match error.kind() {
                        FailureKind::Rpc(code) => {
                            if matches!(code, RpcCode::Aborted | RpcCode::AlreadyExists) {
                                MutationState::Conflict
                            } else if matches!(
                                code,
                                RpcCode::InvalidArgument
                                    | RpcCode::PermissionDenied
                                    | RpcCode::FailedPrecondition
                            ) {
                                MutationState::Rejected
                            } else {
                                MutationState::Uncertain
                            }
                        }
                        FailureKind::InvalidConfiguration
                        | FailureKind::UntrustedCertificate
                        | FailureKind::WrongServerName
                        | FailureKind::TlsRejected
                        | FailureKind::NetworkInterrupted
                        | FailureKind::Deadline
                        | FailureKind::Cancelled => MutationState::Uncertain,
                    });
                }
            }
        }};
    }
    match operation {
        Operation::Policy(request) => {
            let account = request.account_id.value.clone();
            let submitted = sha2::Sha256::digest(&request.yaml_source);
            let response = call!(connection.policies(), replace_policy, request);
            Completion::published(
                home_wire_contract::authorization_contract::admit_replace_response(&response)
                    .is_ok()
                    && response.policy.account_id.value == account
                    && response.policy.version.source_sha256.as_slice() == &submitted[..]
                    && response.policy.version.source_sha256.as_slice()
                        == &sha2::Sha256::digest(&response.policy.yaml_source)[..],
            )
        }
        Operation::Create(request) => {
            let response = call!(connection.integrations(), create_instance, request);
            if response.__buffa_unknown_fields.is_empty() {
                integrations::activation(&response.activation, &response.document)
            } else {
                Completion::state(MutationState::Uncertain)
            }
        }
        Operation::Update(request) => {
            let response = call!(connection.integrations(), update_instance, request);
            if response.__buffa_unknown_fields.is_empty() {
                integrations::activation(&response.activation, &response.document)
            } else {
                Completion::state(MutationState::Uncertain)
            }
        }
        Operation::Delete(request) => {
            let response = call!(connection.integrations(), delete_instance, request);
            if response.__buffa_unknown_fields.is_empty() {
                integrations::activation(&response.activation, &response.document)
            } else {
                Completion::state(MutationState::Uncertain)
            }
        }
        Operation::SetEnabled(request) => {
            let response = call!(connection.integrations(), set_instance_enabled, request);
            if response.__buffa_unknown_fields.is_empty() {
                integrations::activation(&response.activation, &response.document)
            } else {
                Completion::state(MutationState::Uncertain)
            }
        }
    }
}
use sha2::Digest;
