//! Account forms capture admitted identities. No row index, transport, or secret
//! survives an editor/session replacement. Other settings forms extend this owner.
use super::{Payload, ProAdminAction, State, render};
use crate::{
    ProAdminChoice, ProAdminField, ProAdminFieldKind,
    administration::{MutationState, Section},
};
use home_client_session::{Generation, SessionCoordinator};
use home_client_transport::{Connection, FailureKind, RpcCode};
use home_wire_contract::{
    auth_contract::{self, AccountId},
    home::auth::v1 as auth,
};
use std::time::Duration;
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
pub(super) enum Field {
    Login = 1,
    Name = 2,
    Password = 3,
    Role = 10,
    Status = 11,
}
#[derive(Clone, Copy)]
#[repr(i32)]
enum Choice {
    User = 1,
    Admin = 2,
    Active = 3,
    Disabled = 4,
}
impl Choice {
    fn parse(value: i32) -> Option<Self> {
        match value {
            1 => Some(Self::User),
            2 => Some(Self::Admin),
            3 => Some(Self::Active),
            4 => Some(Self::Disabled),
            _ => None,
        }
    }
}
impl Field {
    fn parse(value: i32) -> Option<Self> {
        match value {
            1 => Some(Self::Login),
            2 => Some(Self::Name),
            3 => Some(Self::Password),
            10 => Some(Self::Role),
            11 => Some(Self::Status),
            _ => None,
        }
    }
    fn label(self) -> &'static str {
        match self {
            Self::Login => "Identifiant de connexion",
            Self::Name => "Nom affiché",
            Self::Password => "Mot de passe initial",
            Self::Role => "Rôle",
            Self::Status => "État du compte",
        }
    }
    fn limit(self) -> usize {
        match self {
            Self::Login | Self::Name | Self::Role | Self::Status => 256,
            Self::Password => 4096,
        }
    }
}
#[derive(Clone)]
pub(super) enum Command {
    Create,
    Update(auth::Account),
    Delete(AccountId),
}
impl Command {
    pub fn action(&self) -> ProAdminAction {
        match self {
            Self::Create => ProAdminAction::Create,
            Self::Update(_) => ProAdminAction::Save,
            Self::Delete(_) => ProAdminAction::Remove,
        }
    }
    pub fn label(&self) -> &'static str {
        match self {
            Self::Create => "Créer un compte",
            Self::Update(_) => "Modifier le compte",
            Self::Delete(_) => "Supprimer le compte",
        }
    }
    pub fn destructive(&self) -> bool {
        matches!(self, Self::Delete(_))
    }
}
pub(super) fn available(state: &State) -> Vec<Command> {
    if state.section != Section::Accounts
        || state.stage != crate::administration::Stage::Ready
            && state.stage != crate::administration::Stage::Empty
        || !state
            .private
            .authority()
            .is_some_and(|value| value.administrator())
        || state.private.draft().is_some()
    {
        return Vec::new();
    }
    let mut result = vec![Command::Create];
    if let Some(render::Target::Account(id)) = &state.selected
        && let Some(Payload::Accounts(accounts)) = state
            .private
            .pane(Section::Accounts)
            .and_then(|pane| pane.content.as_ref())
        && let Some(account) = accounts
            .iter()
            .find(|account| account.id.value == id.as_str())
    {
        result.push(Command::Update(account.clone()));
        result.push(Command::Delete(id.clone()));
    }
    result
}
pub(super) struct Draft {
    command: Command,
    login: Zeroizing<String>,
    name: Zeroizing<String>,
    password: Zeroizing<String>,
    role: auth::AccountRole,
    status: auth::AccountStatus,
    pub state: MutationState,
    pub notice: &'static str,
}
impl Draft {
    pub fn new(command: Command) -> Self {
        let mut draft = Self {
            command,
            login: Zeroizing::new(String::new()),
            name: Zeroizing::new(String::new()),
            password: Zeroizing::new(String::new()),
            role: auth::AccountRole::User,
            status: auth::AccountStatus::Active,
            state: MutationState::Draft,
            notice: "Les droits sont appliqués par le serveur. Confirmez explicitement l’enregistrement.",
        };
        if let Command::Update(account) = &draft.command {
            draft.login = Zeroizing::new(account.login_name.clone());
            draft.name = Zeroizing::new(account.display_name.clone());
            draft.role = auth_contract::admit_role(account.role).expect("admitted role");
            draft.status = match account.status {
                buffa::EnumValue::Known(value) => value,
                buffa::EnumValue::Unknown(_) => unreachable!("admitted status"),
            };
        }
        if matches!(draft.command, Command::Delete(_)) {
            draft.notice =
                "Cette suppression est définitive. Le serveur protège le dernier administrateur.";
        }
        draft
    }
    pub fn title(&self) -> &'static str {
        self.command.label()
    }
    pub fn destructive(&self) -> bool {
        self.command.destructive()
    }
    pub fn editable(&self) -> bool {
        matches!(self.state, MutationState::Draft | MutationState::Rejected)
    }
    pub fn edit(&mut self, token: i32, value: String) {
        if !self.editable() {
            return;
        }
        let Some(field) = Field::parse(token) else {
            return;
        };
        if value.len() > field.limit() || value.contains('\0') {
            self.notice = "La valeur dépasse la limite autorisée.";
            return;
        }
        match (&self.command, field) {
            (Command::Create, Field::Login) => self.login = Zeroizing::new(value),
            (Command::Create | Command::Update(_), Field::Name) => {
                self.name = Zeroizing::new(value)
            }
            (Command::Create, Field::Password) => self.password = Zeroizing::new(value),
            (Command::Update(_), Field::Login | Field::Password)
            | (Command::Delete(_), Field::Login | Field::Name | Field::Password)
            | (
                Command::Create | Command::Update(_) | Command::Delete(_),
                Field::Role | Field::Status,
            ) => return,
        }
        self.state = MutationState::Draft;
        self.notice = "Brouillon modifié ; rien n’est envoyé avant confirmation.";
    }
    pub fn choose(&mut self, field: i32, option: i32) {
        if !self.editable() || matches!(self.command, Command::Delete(_)) {
            return;
        }
        let (Some(field), Some(option)) = (Field::parse(field), Choice::parse(option)) else {
            return;
        };
        match option {
            Choice::User => {
                if field != Field::Role {
                    return;
                }
                self.role = auth::AccountRole::User;
            }
            Choice::Admin => {
                if field != Field::Role {
                    return;
                }
                self.role = auth::AccountRole::Admin;
            }
            Choice::Active => {
                if field != Field::Status || !matches!(self.command, Command::Update(_)) {
                    return;
                }
                self.status = auth::AccountStatus::Active;
            }
            Choice::Disabled => {
                if field != Field::Status || !matches!(self.command, Command::Update(_)) {
                    return;
                }
                self.status = auth::AccountStatus::Disabled;
            }
        }
        self.state = MutationState::Draft;
        self.notice = "Brouillon modifié ; rien n’est envoyé avant confirmation.";
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        let fields = match &self.command {
            Command::Create => vec![
                (Field::Login, &self.login),
                (Field::Name, &self.name),
                (Field::Password, &self.password),
            ],
            Command::Update(_) => vec![(Field::Name, &self.name)],
            Command::Delete(_) => Vec::new(),
        };
        fields
            .into_iter()
            .map(|(field, value)| ProAdminField {
                token: field as i32,
                label: field.label().into(),
                description: if matches!(field, Field::Password) {
                    "Le mot de passe n’est jamais relu depuis le serveur."
                } else {
                    ""
                }
                .into(),
                value: value.as_str().into(),
                kind: if matches!(field, Field::Password) {
                    ProAdminFieldKind::Password
                } else {
                    ProAdminFieldKind::Plain
                },
                editable: self.editable(),
            })
            .collect()
    }
    pub fn choices(&self) -> Vec<ProAdminChoice> {
        if matches!(self.command, Command::Delete(_)) {
            return Vec::new();
        }
        let mut result = vec![
            ProAdminChoice {
                token: Choice::User as i32,
                field_token: Field::Role as i32,
                label: "Utilisateur".into(),
                selected: self.role == auth::AccountRole::User,
                enabled: self.editable(),
            },
            ProAdminChoice {
                token: Choice::Admin as i32,
                field_token: Field::Role as i32,
                label: "Administrateur".into(),
                selected: self.role == auth::AccountRole::Admin,
                enabled: self.editable(),
            },
        ];
        if matches!(self.command, Command::Update(_)) {
            result.extend([
                ProAdminChoice {
                    token: Choice::Active as i32,
                    field_token: Field::Status as i32,
                    label: "Compte actif".into(),
                    selected: self.status == auth::AccountStatus::Active,
                    enabled: self.editable(),
                },
                ProAdminChoice {
                    token: Choice::Disabled as i32,
                    field_token: Field::Status as i32,
                    label: "Compte désactivé".into(),
                    selected: self.status == auth::AccountStatus::Disabled,
                    enabled: self.editable(),
                },
            ]);
        }
        result
    }
    pub fn submit(&mut self) -> Option<Operation> {
        if self.state != MutationState::Draft {
            return None;
        }
        if !matches!(self.command, Command::Delete(_))
            && (self.name.len() > 256 || self.name.contains(['\r', '\n']))
        {
            self.notice = "Le nom affiché est invalide.";
            return None;
        }
        let operation = match &self.command {
            Command::Create => {
                if self.login.trim().is_empty()
                    || self.login.contains(['\r', '\n'])
                    || self.password.is_empty()
                {
                    self.notice = "Renseignez l’identifiant et le mot de passe initial.";
                    return None;
                }
                Operation::Create(auth::CreateAccountRequest {
                    login_name: self.login.to_string(),
                    display_name: self.name.to_string(),
                    initial_password: self.password.to_string(),
                    role: self.role.into(),
                    ..Default::default()
                })
            }
            Command::Update(account) => Operation::Update(auth::UpdateAccountRequest {
                account_id: account.id.clone(),
                display_name: Some(self.name.to_string()),
                role: Some(self.role.into()),
                status: Some(self.status.into()),
                ..Default::default()
            }),
            Command::Delete(id) => Operation::Delete(auth::DeleteAccountRequest {
                account_id: auth::AccountId {
                    value: id.as_str().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }),
        };
        self.state = MutationState::InFlight;
        self.notice = "Enregistrement en cours…";
        Some(operation)
    }
    pub fn complete(&mut self, state: MutationState) {
        self.state = state;
        self.password = Zeroizing::new(String::new());
        self.notice = match state {
            MutationState::Published => "Enregistrement confirmé par le serveur.",
            MutationState::Rejected => {
                "Le serveur a refusé la modification. Corrigez le brouillon avant de confirmer à nouveau."
            }
            MutationState::Conflict => {
                "Le compte a changé. Rechargez les données avant de préparer une nouvelle modification."
            }
            MutationState::Uncertain => {
                "Résultat inconnu : aucune nouvelle tentative automatique. Rechargez la liste pour vérifier le résultat."
            }
            MutationState::Draft
            | MutationState::InFlight
            | MutationState::Reconciling
            | MutationState::RestartRequired => "Rechargez les données avant de poursuivre.",
        };
    }
}
pub(super) enum Operation {
    Create(auth::CreateAccountRequest),
    Update(auth::UpdateAccountRequest),
    Delete(auth::DeleteAccountRequest),
}
pub(super) async fn execute(
    session: &SessionCoordinator,
    connection: &Connection,
    expected: Generation,
    operation: Operation,
    cancel: &CancellationToken,
) -> MutationState {
    let Ok(lease) = super::read::admitted_lease(session, expected, cancel).await else {
        return MutationState::Uncertain;
    };
    let client = connection.accounts();
    macro_rules! call {($method:ident,$request:expr)=>{{
        let response=tokio::select!{biased;()=lease.cancellation.cancelled()=>return MutationState::Uncertain,result=client.$method($request,lease.token(),Duration::from_secs(4),cancel)=>result};
        match response{Ok(value)=>value,Err(error)=>{let _=session.observe_failure(lease.generation,&error).await;return match error.kind(){
            // Connect's ErrorCode is explicitly open/non-exhaustive. Unknown
            // future codes are uncertain, never permission to replay a write.
            FailureKind::Rpc(code)=>if matches!(code,RpcCode::InvalidArgument|RpcCode::PermissionDenied|RpcCode::FailedPrecondition){MutationState::Rejected}else if matches!(code,RpcCode::AlreadyExists|RpcCode::Aborted){MutationState::Conflict}else{MutationState::Uncertain},
            FailureKind::InvalidConfiguration|FailureKind::UntrustedCertificate|FailureKind::WrongServerName|FailureKind::TlsRejected|FailureKind::NetworkInterrupted|FailureKind::Deadline|FailureKind::Cancelled=>MutationState::Uncertain,
        };}}
    }}}
    let valid = match operation {
        Operation::Create(request) => {
            let response = call!(create_account, request);
            response.__buffa_unknown_fields.is_empty()
                && response
                    .account
                    .as_option()
                    .is_some_and(|account| auth_contract::admit_account(account).is_ok())
        }
        Operation::Update(request) => {
            let id = request.account_id.value.clone();
            let response = call!(update_account, request);
            response.__buffa_unknown_fields.is_empty()
                && response.account.as_option().is_some_and(|account| {
                    auth_contract::admit_account(account).is_ok() && account.id.value == id
                })
        }
        Operation::Delete(request) => call!(delete_account, request)
            .__buffa_unknown_fields
            .is_empty(),
    };
    if valid {
        MutationState::Published
    } else {
        MutationState::Uncertain
    }
}
