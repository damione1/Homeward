//! Session-owned bounded forms. Protocol identifiers are captured from admitted
//! data, every mutation is attempted once, and one-time secrets stay private.
pub(super) mod advanced;
pub(super) mod scopes;
use super::{Payload, ProAdminAction, State, render};
use crate::{
    ProAdminChoice, ProAdminField, ProAdminFieldKind,
    administration::{MutationState, SecretKind, Section, Stage},
};
use home_client_session::{Generation, SessionCoordinator};
use home_client_transport::{Connection, FailureKind, RpcCode};
use home_wire_contract::{
    auth_contract::{self, AccountId, FactorCredentialId, InvitationId, McpKeyId, SessionId},
    home::auth::v1 as auth,
};
use scopes::ScopeChoice;
use std::time::Duration;
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
pub(super) enum Field {
    Login = 1,
    Name = 2,
    Password = 3,
    ClientName = 4,
    Days = 5,
}
impl Field {
    fn parse(value: i32) -> Option<Self> {
        match value {
            1 => Some(Self::Login),
            2 => Some(Self::Name),
            3 => Some(Self::Password),
            4 => Some(Self::ClientName),
            5 => Some(Self::Days),
            _ => None,
        }
    }
    fn label(self) -> &'static str {
        match self {
            Self::Login => "Identifiant de connexion",
            Self::Name => "Nom affiché",
            Self::Password => "Mot de passe initial",
            Self::ClientName => "Nom du client MCP",
            Self::Days => "Validité en jours",
        }
    }
    fn limit(self) -> usize {
        match self {
            Self::Login | Self::Name => 256,
            Self::ClientName => 128,
            Self::Password => 4096,
            Self::Days => 3,
        }
    }
}
#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(i32)]
pub(super) enum ChoiceField {
    Role = 10,
    Status = 11,
    Scope = 12,
    RequireMfa = 13,
}
impl ChoiceField {
    fn parse(value: i32) -> Option<Self> {
        match value {
            10 => Some(Self::Role),
            11 => Some(Self::Status),
            12 => Some(Self::Scope),
            13 => Some(Self::RequireMfa),
            _ => None,
        }
    }
}
#[derive(Clone, Copy)]
#[repr(i32)]
pub(super) enum Choice {
    User = 1,
    Admin = 2,
    Active = 3,
    Disabled = 4,
    RequireMfa = 5,
    OptionalMfa = 6,
}
impl Choice {
    fn parse(value: i32) -> Option<Self> {
        match value {
            1 => Some(Self::User),
            2 => Some(Self::Admin),
            3 => Some(Self::Active),
            4 => Some(Self::Disabled),
            5 => Some(Self::RequireMfa),
            6 => Some(Self::OptionalMfa),
            _ => None,
        }
    }
}

#[derive(Clone)]
pub(super) enum Command {
    Create,
    Update(auth::Account),
    Delete(AccountId),
    Invite,
    RevokeInvitation(InvitationId),
    CreateMcp,
    RevokeMcp(McpKeyId),
    RevokeSession(SessionId),
    RemoveFactor(FactorCredentialId),
    RecoveryCodes(AccountId),
    MfaPolicy(auth::SecondFactorPolicy),
    Advanced(Box<advanced::Command>),
}
impl Command {
    pub fn section(&self) -> Section {
        match self {
            Self::Create | Self::Update(_) | Self::Delete(_) => Section::Accounts,
            Self::Invite | Self::RevokeInvitation(_) => Section::Invitations,
            Self::CreateMcp | Self::RevokeMcp(_) => Section::McpKeys,
            Self::RevokeSession(_) => Section::Sessions,
            Self::RemoveFactor(_) | Self::RecoveryCodes(_) => Section::Factors,
            Self::MfaPolicy(_) => Section::MfaPolicy,
            Self::Advanced(value) => value.section(),
        }
    }
    pub fn action(&self) -> ProAdminAction {
        match self {
            Self::Advanced(value) => value.action(),
            Self::Create | Self::Invite | Self::CreateMcp | Self::RecoveryCodes(_) => {
                ProAdminAction::Create
            }
            Self::Update(_) | Self::MfaPolicy(_) => ProAdminAction::Save,
            Self::Delete(_) => ProAdminAction::Remove,
            Self::RevokeInvitation(_)
            | Self::RevokeMcp(_)
            | Self::RevokeSession(_)
            | Self::RemoveFactor(_) => ProAdminAction::Revoke,
        }
    }
    pub fn label(&self) -> &'static str {
        match self {
            Self::Advanced(value) => value.label(),
            Self::Create => "Créer un compte",
            Self::Update(_) => "Modifier le compte",
            Self::Delete(_) => "Supprimer le compte",
            Self::Invite => "Créer une invitation",
            Self::RevokeInvitation(_) => "Révoquer l’invitation",
            Self::CreateMcp => "Créer une clé MCP",
            Self::RevokeMcp(_) => "Révoquer la clé MCP",
            Self::RevokeSession(_) => "Révoquer cette session",
            Self::RemoveFactor(_) => "Retirer ce facteur",
            Self::RecoveryCodes(_) => "Remplacer les codes de secours",
            Self::MfaPolicy(_) => "Modifier la politique MFA",
        }
    }
    pub fn destructive(&self) -> bool {
        if let Self::Advanced(value) = self {
            return value.destructive();
        }
        matches!(
            self,
            Self::Delete(_)
                | Self::RevokeInvitation(_)
                | Self::RevokeMcp(_)
                | Self::RevokeSession(_)
                | Self::RemoveFactor(_)
                | Self::RecoveryCodes(_)
        )
    }
    fn input_fields(&self) -> &'static [Field] {
        match self {
            Self::Advanced(_) => &[],
            Self::Create => &[Field::Login, Field::Name, Field::Password],
            Self::Update(_) => &[Field::Name],
            Self::Invite => &[Field::Login, Field::Days],
            Self::CreateMcp => &[Field::ClientName, Field::Days],
            Self::Delete(_)
            | Self::RevokeInvitation(_)
            | Self::RevokeMcp(_)
            | Self::RevokeSession(_)
            | Self::RemoveFactor(_)
            | Self::RecoveryCodes(_)
            | Self::MfaPolicy(_) => &[],
        }
    }
    fn role_choices(&self) -> bool {
        matches!(self, Self::Create | Self::Update(_) | Self::Invite)
    }
}
pub(super) fn available(state: &State) -> Vec<Command> {
    if !matches!(state.stage, Stage::Ready | Stage::Empty)
        || state.private.draft().is_some()
        || state.private.secret().is_some()
    {
        return Vec::new();
    }
    let Some(authority) = state.private.authority() else {
        return Vec::new();
    };
    if state.section.administrative() && !authority.administrator() {
        return Vec::new();
    }
    let mut result = Vec::new();
    match state.section {
        Section::Accounts => {
            result.push(Command::Create);
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
        }
        Section::Invitations => {
            result.push(Command::Invite);
            if let Some(render::Target::Invitation(id)) = &state.selected
                && let Some(Payload::Invitations(rows)) = state
                    .private
                    .pane(Section::Invitations)
                    .and_then(|pane| pane.content.as_ref())
                && rows.iter().any(|row| {
                    row.id.value == id.as_str()
                        && !row.revoked_at.is_set()
                        && !row.accepted_at.is_set()
                })
            {
                result.push(Command::RevokeInvitation(id.clone()));
            }
        }
        Section::McpKeys => {
            result.push(Command::CreateMcp);
            if let Some(render::Target::Key(id)) = &state.selected
                && let Some(Payload::Keys(rows)) = state
                    .private
                    .pane(Section::McpKeys)
                    .and_then(|pane| pane.content.as_ref())
                && rows
                    .iter()
                    .any(|row| row.id.value == id.as_str() && !row.revoked_at.is_set())
            {
                result.push(Command::RevokeMcp(id.clone()));
            }
        }
        Section::Sessions => {
            if let Some(render::Target::Session(id)) = &state.selected
                && let Some(Payload::Sessions(rows)) = state
                    .private
                    .pane(Section::Sessions)
                    .and_then(|pane| pane.content.as_ref())
                && rows
                    .iter()
                    .any(|row| row.id.value == id.as_str() && !row.revoked_at.is_set())
            {
                result.push(Command::RevokeSession(id.clone()));
            }
        }
        Section::Factors => {
            if let Some(Payload::Factors(rows)) = state
                .private
                .pane(Section::Factors)
                .and_then(|pane| pane.content.as_ref())
            {
                if !rows.is_empty() {
                    result.push(Command::RecoveryCodes(authority.identity.account.clone()));
                }
                if let Some(render::Target::Factor(id)) = &state.selected
                    && rows.iter().any(|row| row.id.value == id.as_str())
                {
                    result.push(Command::RemoveFactor(id.clone()));
                }
            }
        }
        Section::MfaPolicy => {
            if let Some(Payload::MfaPolicy(value)) = state
                .private
                .pane(Section::MfaPolicy)
                .and_then(|pane| pane.content.as_ref())
            {
                result.push(Command::MfaPolicy(value.clone()));
            }
        }
        Section::Policies => {
            if let Some(Payload::Policy { account, response }) = state
                .private
                .pane(Section::Policies)
                .and_then(|pane| pane.content.as_ref())
            {
                result.push(Command::Advanced(Box::new(advanced::Command::Policy(
                    advanced::policy::PolicyCommand {
                        account: account.clone(),
                        response: response.clone(),
                    },
                ))));
            }
        }
        Section::Integrations => {
            use advanced::integrations::{DesiredState, IntegrationCommand};
            match state
                .private
                .pane(Section::Integrations)
                .and_then(|pane| pane.content.as_ref())
            {
                Some(Payload::Integrations(value)) => result.push(Command::Advanced(Box::new(
                    advanced::Command::Integration(IntegrationCommand::Create(value.clone())),
                ))),
                Some(Payload::Integration(value)) => {
                    if matches!(value.response.instance.configuration,Some(home_wire_contract::home::integrationadmin::v1::integration_instance::Configuration::Editable(_))){result.push(Command::Advanced(Box::new(advanced::Command::Integration(IntegrationCommand::Update(value.clone())))));}
                    result.push(Command::Advanced(Box::new(advanced::Command::Integration(
                        IntegrationCommand::Delete(value.clone()),
                    ))));
                    let desired = if value.response.instance.desired_state
                        == home_wire_contract::home::integrationadmin::v1::DesiredState::Enabled
                    {
                        DesiredState::Disabled
                    } else {
                        DesiredState::Enabled
                    };
                    result.push(Command::Advanced(Box::new(advanced::Command::Integration(
                        IntegrationCommand::SetEnabled {
                            detail: value.clone(),
                            state: desired,
                        },
                    ))));
                }
                Some(
                    Payload::Personal { .. }
                    | Payload::Sessions(_)
                    | Payload::Factors(_)
                    | Payload::MfaPolicy(_)
                    | Payload::Keys(_)
                    | Payload::Accounts(_)
                    | Payload::Invitations(_)
                    | Payload::Policy { .. }
                    | Payload::Health(_)
                    | Payload::Metrics(_)
                    | Payload::Logs(_)
                    | Payload::Audits(_)
                    | Payload::Configuration(_),
                )
                | None => (),
            }
        }
        Section::Account
        | Section::Configuration
        | Section::Health
        | Section::Metrics
        | Section::Logs
        | Section::Audits
        | Section::SecretAudits => {}
    }
    result
}
pub(super) struct Draft {
    command: Command,
    login: Zeroizing<String>,
    name: Zeroizing<String>,
    password: Zeroizing<String>,
    client_name: Zeroizing<String>,
    days: Zeroizing<String>,
    role: auth::AccountRole,
    status: auth::AccountStatus,
    scopes: Vec<ScopeChoice>,
    require_admin_factor: bool,
    advanced: Option<Box<advanced::Draft>>,
    pub state: MutationState,
    pub notice: &'static str,
}
impl Draft {
    pub fn new(command: Command) -> Self {
        let mut value = Self {
            command,
            login: Zeroizing::new(String::new()),
            name: Zeroizing::new(String::new()),
            password: Zeroizing::new(String::new()),
            client_name: Zeroizing::new(String::new()),
            days: Zeroizing::new("7".into()),
            role: auth::AccountRole::User,
            status: auth::AccountStatus::Active,
            scopes: Vec::new(),
            require_admin_factor: false,
            advanced: None,
            state: MutationState::Draft,
            notice: "Rien n’est envoyé avant votre confirmation. Les droits sont appliqués par le serveur.",
        };
        if let Command::Update(account) = &value.command {
            value.login = Zeroizing::new(account.login_name.clone());
            value.name = Zeroizing::new(account.display_name.clone());
            value.role = auth_contract::admit_role(account.role).expect("admitted role");
            value.status = match account.status {
                buffa::EnumValue::Known(value) => value,
                buffa::EnumValue::Unknown(_) => unreachable!("admitted status"),
            };
        }
        if value.command.destructive() {
            value.notice = "Cette révocation ou suppression doit être confirmée. Le serveur protège les invariants d’accès.";
        }
        if matches!(value.command, Command::CreateMcp) {
            value.days = Zeroizing::new("30".into());
            value.notice = "Sélectionnez les permissions maximales de cette clé. Elles n’ajoutent aucun droit à votre compte. Le secret sera affiché une seule fois.";
        }
        if let Command::MfaPolicy(policy) = &value.command {
            value.require_admin_factor = policy.require_admin_factor;
            value.notice = "La politique utilise la version exacte du serveur. Un changement peut révoquer les sessions et demander une nouvelle authentification.";
        }
        if matches!(value.command, Command::RecoveryCodes(_)) {
            value.notice = "Les anciens codes seront invalidés et vos sessions révoquées. Les nouveaux codes seront affichés une seule fois après confirmation.";
        }
        if let Command::Advanced(command) = &value.command {
            match advanced::Draft::open(command) {
                Ok(draft) => {
                    value.advanced = Some(Box::new(draft));
                    value.notice = "Les préconditions exactes du serveur sont conservées. Les choix rapides de droits remplacent toutes les règles du brouillon ; rien n’est enregistré sans confirmation.";
                }
                Err(message) => {
                    value.state = MutationState::Rejected;
                    value.notice = message;
                }
            }
        }
        value
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
    fn value(&self, field: Field) -> &str {
        match field {
            Field::Login => &self.login,
            Field::Name => &self.name,
            Field::Password => &self.password,
            Field::ClientName => &self.client_name,
            Field::Days => &self.days,
        }
    }
    fn modified(&mut self) {
        self.state = MutationState::Draft;
        self.notice = "Brouillon modifié ; rien n’est envoyé avant confirmation.";
    }
    pub fn edit(&mut self, token: i32, value: String) {
        if !self.editable() {
            return;
        }
        if let Some(draft) = &mut self.advanced {
            match draft.edit(token, value) {
                Ok(()) => self.modified(),
                Err(message) => self.notice = message,
            };
            return;
        }
        let Some(field) = Field::parse(token) else {
            return;
        };
        if !self.command.input_fields().contains(&field) {
            return;
        }
        if value.len() > field.limit() || value.contains('\0') {
            self.notice = "La valeur dépasse la limite autorisée.";
            return;
        }
        let value = Zeroizing::new(value);
        match field {
            Field::Login => self.login = value,
            Field::Name => self.name = value,
            Field::Password => self.password = value,
            Field::ClientName => self.client_name = value,
            Field::Days => self.days = value,
        }
        self.modified();
    }
    pub fn choose(&mut self, field: i32, option: i32) {
        if !self.editable() {
            return;
        }
        if let Some(draft) = &mut self.advanced {
            match draft.choose(field, option) {
                Ok(()) => self.modified(),
                Err(message) => self.notice = message,
            };
            return;
        }
        let Some(field) = ChoiceField::parse(field) else {
            return;
        };
        match field {
            ChoiceField::Scope => {
                if !matches!(self.command, Command::CreateMcp) {
                    return;
                }
                let Some(value) = ScopeChoice::parse(option) else {
                    return;
                };
                if let Some(index) = self.scopes.iter().position(|scope| *scope == value) {
                    self.scopes.remove(index);
                } else {
                    self.scopes.push(value);
                }
            }
            ChoiceField::RequireMfa => {
                if !matches!(self.command, Command::MfaPolicy(_)) {
                    return;
                }
                match Choice::parse(option) {
                    Some(Choice::RequireMfa) => self.require_admin_factor = true,
                    Some(Choice::OptionalMfa) => self.require_admin_factor = false,
                    Some(Choice::User | Choice::Admin | Choice::Active | Choice::Disabled)
                    | None => return,
                }
            }
            ChoiceField::Role | ChoiceField::Status => {
                let Some(option) = Choice::parse(option) else {
                    return;
                };
                match option {
                    Choice::User => {
                        if field != ChoiceField::Role || !self.command.role_choices() {
                            return;
                        }
                        self.role = auth::AccountRole::User;
                    }
                    Choice::Admin => {
                        if field != ChoiceField::Role || !self.command.role_choices() {
                            return;
                        }
                        self.role = auth::AccountRole::Admin;
                    }
                    Choice::Active => {
                        if field != ChoiceField::Status
                            || !matches!(self.command, Command::Update(_))
                        {
                            return;
                        }
                        self.status = auth::AccountStatus::Active;
                    }
                    Choice::Disabled => {
                        if field != ChoiceField::Status
                            || !matches!(self.command, Command::Update(_))
                        {
                            return;
                        }
                        self.status = auth::AccountStatus::Disabled;
                    }
                    Choice::RequireMfa | Choice::OptionalMfa => return,
                }
            }
        }
        self.modified();
    }
    pub fn fields(&self) -> Vec<ProAdminField> {
        if let Some(draft) = &self.advanced {
            return draft.fields();
        }
        self.command
            .input_fields()
            .iter()
            .map(|&field| ProAdminField {
                token: field as i32,
                label: field.label().into(),
                description: match field {
                    Field::Password => "Le mot de passe n’est jamais relu depuis le serveur.",
                    Field::Days => {
                        if matches!(self.command, Command::CreateMcp) {
                            "De 1 à 365 jours."
                        } else {
                            "De 1 à 30 jours."
                        }
                    }
                    Field::Login | Field::Name | Field::ClientName => "",
                }
                .into(),
                value: self.value(field).into(),
                kind: if field == Field::Password {
                    ProAdminFieldKind::Password
                } else {
                    ProAdminFieldKind::Plain
                },
                editable: self.editable(),
            })
            .collect()
    }
    pub fn choices(&self) -> Vec<ProAdminChoice> {
        if let Some(draft) = &self.advanced {
            return draft.choices();
        }
        let mut result = Vec::new();
        if self.command.role_choices() {
            result.extend(
                [
                    (
                        Choice::User,
                        "Utilisateur",
                        self.role == auth::AccountRole::User,
                    ),
                    (
                        Choice::Admin,
                        "Administrateur",
                        self.role == auth::AccountRole::Admin,
                    ),
                ]
                .map(|(choice, label, selected)| ProAdminChoice {
                    token: choice as i32,
                    field_token: ChoiceField::Role as i32,
                    label: label.into(),
                    selected,
                    enabled: self.editable(),
                }),
            );
        }
        if matches!(self.command, Command::Update(_)) {
            result.extend(
                [
                    (
                        Choice::Active,
                        "Compte actif",
                        self.status == auth::AccountStatus::Active,
                    ),
                    (
                        Choice::Disabled,
                        "Compte désactivé",
                        self.status == auth::AccountStatus::Disabled,
                    ),
                ]
                .map(|(choice, label, selected)| ProAdminChoice {
                    token: choice as i32,
                    field_token: ChoiceField::Status as i32,
                    label: label.into(),
                    selected,
                    enabled: self.editable(),
                }),
            );
        }
        if matches!(self.command, Command::CreateMcp) {
            result.extend(ScopeChoice::all().map(|(token, scope)| ProAdminChoice {
                token,
                field_token: ChoiceField::Scope as i32,
                label: scope.label().into(),
                selected: self.scopes.contains(&scope),
                enabled: self.editable(),
            }));
        }
        if matches!(self.command, Command::MfaPolicy(_)) {
            result.extend(
                [
                    (
                        Choice::RequireMfa,
                        "Facteur obligatoire pour les administrateurs",
                        self.require_admin_factor,
                    ),
                    (
                        Choice::OptionalMfa,
                        "Facteur administrateur facultatif",
                        !self.require_admin_factor,
                    ),
                ]
                .map(|(choice, label, selected)| ProAdminChoice {
                    token: choice as i32,
                    field_token: ChoiceField::RequireMfa as i32,
                    label: label.into(),
                    selected,
                    enabled: self.editable(),
                }),
            );
        }
        result
    }
    pub fn submit(&mut self) -> Option<Operation> {
        if self.state != MutationState::Draft {
            return None;
        }
        if let Some(draft) = &self.advanced {
            return match draft.submit() {
                Ok(operation) => {
                    self.state = MutationState::InFlight;
                    self.notice = "Enregistrement en cours…";
                    Some(Operation::Advanced(Box::new(operation)))
                }
                Err(message) => {
                    self.notice = message;
                    None
                }
            };
        }
        if self.command.input_fields().iter().any(|&field| {
            self.value(field).contains('\0') || self.value(field).len() > field.limit()
        }) {
            return None;
        }
        if self.name.contains(['\r', '\n'])
            || self.login.contains(['\r', '\n'])
            || self.client_name.contains(['\r', '\n'])
        {
            self.notice = "La valeur contient un retour à la ligne interdit.";
            return None;
        }
        let days = if matches!(self.command, Command::Invite | Command::CreateMcp) {
            let maximum = if matches!(self.command, Command::Invite) {
                30
            } else {
                365
            };
            let Ok(days) = self.days.parse::<i64>() else {
                self.notice = "Saisissez une validité en jours entiers.";
                return None;
            };
            if !(1..=maximum).contains(&days) {
                self.notice = "La validité est hors de la plage autorisée.";
                return None;
            }
            days
        } else {
            0
        };
        let operation = match &self.command {
            Command::Advanced(_) => return None,
            Command::Create => {
                if self.login.trim().is_empty() || self.password.is_empty() {
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
            Command::Invite => {
                if self.login.trim().is_empty() {
                    self.notice = "Renseignez l’identifiant du compte invité.";
                    return None;
                }
                Operation::Invite(auth::CreateInvitationRequest {
                    login_name: self.login.to_string(),
                    role: self.role.into(),
                    expires_at: buffa_types::google::protobuf::Timestamp {
                        seconds: chrono::Utc::now().timestamp() + days * 86400,
                        nanos: 0,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                })
            }
            Command::RevokeInvitation(id) => {
                Operation::RevokeInvitation(auth::RevokeInvitationRequest {
                    invitation_id: auth::InvitationId {
                        value: id.as_str().into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                })
            }
            Command::CreateMcp => {
                if self.client_name.trim().is_empty() || self.scopes.is_empty() {
                    self.notice =
                        "Renseignez le client et sélectionnez au moins une permission maximale.";
                    return None;
                }
                let request = auth::CreateMcpKeyRequest {
                    client_name: self.client_name.to_string(),
                    scopes: self.scopes.iter().copied().map(ScopeChoice::wire).collect(),
                    lifetime: buffa_types::google::protobuf::Duration {
                        seconds: days * 86400,
                        nanos: 0,
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                };
                if auth_contract::admit_mcp_create_request(&request).is_err() {
                    self.notice = "Le plafond de permissions ou la validité est invalide.";
                    return None;
                }
                Operation::CreateMcp(request)
            }
            Command::RevokeMcp(id) => Operation::RevokeMcp(auth::RevokeMcpKeyRequest {
                key_id: auth::McpKeyId {
                    value: id.as_str().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }),
            Command::RevokeSession(id) => Operation::RevokeSession(auth::RevokeSessionRequest {
                session_id: auth::SessionId {
                    value: id.as_str().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }),
            Command::RemoveFactor(id) => Operation::RemoveFactor(auth::RemoveFactorRequest {
                factor_id: auth::FactorCredentialId {
                    value: id.as_str().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }),
            Command::RecoveryCodes(id) => {
                Operation::RecoveryCodes(auth::ReplaceRecoveryCodesRequest {
                    account_id: auth::AccountId {
                        value: id.as_str().into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                })
            }
            Command::MfaPolicy(policy) => {
                let request = auth::SetSecondFactorPolicyRequest {
                    require_admin_factor: self.require_admin_factor,
                    expected: policy.version.clone(),
                    ..Default::default()
                };
                if auth_contract::admit_set_second_factor_policy(&request).is_err() {
                    return None;
                }
                Operation::MfaPolicy(request)
            }
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
                "Les données ont changé. Rechargez-les avant de préparer une nouvelle modification."
            }
            MutationState::Uncertain => {
                "Résultat inconnu : aucune nouvelle tentative automatique. Rechargez la liste pour vérifier le résultat."
            }
            MutationState::Reconciling => {
                "Modification enregistrée. Le rapprochement du runtime est en cours ; rechargez l’état sans renvoyer l’écriture."
            }
            MutationState::RestartRequired => {
                "Modification enregistrée. Un redémarrage du serveur est nécessaire pour l’appliquer ; ne renvoyez pas l’écriture."
            }
            MutationState::Draft | MutationState::InFlight => {
                "Rechargez les données avant de poursuivre."
            }
        };
    }
}
pub(super) enum Operation {
    Create(auth::CreateAccountRequest),
    Update(auth::UpdateAccountRequest),
    Delete(auth::DeleteAccountRequest),
    Invite(auth::CreateInvitationRequest),
    RevokeInvitation(auth::RevokeInvitationRequest),
    CreateMcp(auth::CreateMcpKeyRequest),
    RevokeMcp(auth::RevokeMcpKeyRequest),
    RevokeSession(auth::RevokeSessionRequest),
    RemoveFactor(auth::RemoveFactorRequest),
    RecoveryCodes(auth::ReplaceRecoveryCodesRequest),
    MfaPolicy(auth::SetSecondFactorPolicyRequest),
    Advanced(Box<advanced::Operation>),
}
pub(super) struct Completion {
    pub state: MutationState,
    pub secret: Option<(SecretKind, Zeroizing<String>)>,
    pub recovery_generation: Option<Generation>,
}
impl Completion {
    fn state(state: MutationState) -> Self {
        Self {
            state,
            secret: None,
            recovery_generation: None,
        }
    }
    fn published(valid: bool) -> Self {
        Self::state(if valid {
            MutationState::Published
        } else {
            MutationState::Uncertain
        })
    }
    fn secret(kind: SecretKind, value: Zeroizing<String>) -> Self {
        if value.is_empty() || value.len() > 4096 || value.contains(['\0', '\r', '\n']) {
            return Self::state(MutationState::Uncertain);
        }
        Self {
            state: MutationState::Published,
            secret: Some((kind, value)),
            recovery_generation: None,
        }
    }
}
pub(super) async fn execute(
    session: &SessionCoordinator,
    connection: &Connection,
    expected: Generation,
    operation: Operation,
    cancel: &CancellationToken,
    receipt: &super::receipt::Permit,
) -> Completion {
    let Ok(lease) = super::read::admitted_lease(session, expected, cancel).await else {
        return Completion::state(MutationState::Uncertain);
    };
    macro_rules! call {
        ($client:expr,$method:ident,$request:expr)=>{call!($client,$method,$request,cancel,true)};
        ($client:expr,$method:ident,$request:expr,$token:expr,$observe:expr)=>{{
            let client=$client;
            let response=tokio::select!{biased;()=lease.cancellation.cancelled(),if $observe=>return Completion::state(MutationState::Uncertain),result=client.$method($request,lease.token(),Duration::from_secs(4),$token)=>result};
            match response{Ok(value)=>value,Err(error)=>{let _=session.observe_failure(lease.generation,&error).await;return Completion::state(match error.kind(){
                FailureKind::Rpc(code)=>if matches!(code,RpcCode::InvalidArgument|RpcCode::PermissionDenied|RpcCode::FailedPrecondition){MutationState::Rejected}else if matches!(code,RpcCode::AlreadyExists|RpcCode::Aborted){MutationState::Conflict}else{MutationState::Uncertain},
                FailureKind::InvalidConfiguration|FailureKind::UntrustedCertificate|FailureKind::WrongServerName|FailureKind::TlsRejected|FailureKind::NetworkInterrupted|FailureKind::Deadline|FailureKind::Cancelled=>MutationState::Uncertain,
            });}}
        }}
    }
    match operation {
        Operation::Advanced(value) => {
            advanced::execute(session, connection, &lease, *value, &receipt.cancel).await
        }
        Operation::Create(request) => {
            let login = request.login_name.clone();
            let response = call!(connection.accounts(), create_account, request);
            Completion::published(
                response.__buffa_unknown_fields.is_empty()
                    && response.account.as_option().is_some_and(|account| {
                        auth_contract::admit_account(account).is_ok() && account.login_name == login
                    }),
            )
        }
        Operation::Update(request) => {
            let id = request.account_id.value.clone();
            let response = call!(connection.accounts(), update_account, request);
            Completion::published(
                response.__buffa_unknown_fields.is_empty()
                    && response.account.as_option().is_some_and(|account| {
                        auth_contract::admit_account(account).is_ok() && account.id.value == id
                    }),
            )
        }
        Operation::Delete(request) => Completion::published(
            call!(connection.accounts(), delete_account, request)
                .__buffa_unknown_fields
                .is_empty(),
        ),
        Operation::Invite(request) => {
            let login = request.login_name.clone();
            let role = request.role;
            let response = call!(connection.accounts(), create_invitation, request);
            let secret = Zeroizing::new(response.invitation_token);
            if !response.__buffa_unknown_fields.is_empty()
                || !response.invitation.as_option().is_some_and(|invitation| {
                    auth_contract::admit_invitation_metadata(invitation).is_ok()
                        && invitation.login_name == login
                        && invitation.role == role
                        && !invitation.accepted_at.is_set()
                        && !invitation.revoked_at.is_set()
                })
            {
                return Completion::state(MutationState::Uncertain);
            }
            Completion::secret(SecretKind::Invitation, secret)
        }
        Operation::RevokeInvitation(request) => Completion::published(
            call!(connection.accounts(), revoke_invitation, request)
                .__buffa_unknown_fields
                .is_empty(),
        ),
        Operation::CreateMcp(request) => {
            let name = request.client_name.clone();
            let scopes = request.scopes.clone();
            let response = call!(connection.mcp_keys(), create_mcp_key, request);
            let secret = Zeroizing::new(response.secret);
            let now = chrono::Utc::now();
            let now = buffa_types::google::protobuf::Timestamp {
                seconds: now.timestamp(),
                nanos: now.timestamp_subsec_nanos() as i32,
                ..Default::default()
            };
            if !response.__buffa_unknown_fields.is_empty()
                || !response.key.as_option().is_some_and(|key| {
                    auth_contract::admit_mcp_key(key, &now).is_ok()
                        && auth_contract::admit_mcp_key_metadata(key).is_ok()
                        && key.account_id.value == lease.account_id().as_str()
                        && key.client_name == name
                        && key.scopes.len() == scopes.len()
                        && scopes.iter().all(|scope| {
                            key.scopes.iter().any(|value| {
                                value.family == scope.family && value.operation == scope.operation
                            })
                        })
                })
            {
                return Completion::state(MutationState::Uncertain);
            }
            Completion::secret(SecretKind::McpKey, secret)
        }
        Operation::RevokeMcp(request) => Completion::published(
            call!(connection.mcp_keys(), revoke_mcp_key, request)
                .__buffa_unknown_fields
                .is_empty(),
        ),
        Operation::RevokeSession(request) => {
            let current = request.session_id.value == lease.session_id().as_str();
            let () = call!(
                connection.auth(),
                revoke_session,
                request,
                &receipt.cancel,
                false
            );
            let valid = true;
            if valid && current && !receipt.cancel.is_cancelled() {
                let _ = retire(session, lease.generation).await;
            }
            Completion::published(valid)
        }
        Operation::RemoveFactor(request) => {
            let response = call!(
                connection.auth(),
                remove_factor,
                request,
                &receipt.cancel,
                false
            );
            let valid = response.__buffa_unknown_fields.is_empty();
            if valid && !receipt.cancel.is_cancelled() {
                let _ = retire(session, lease.generation).await;
            }
            Completion::published(valid)
        }
        Operation::MfaPolicy(request) => {
            let requested = request.require_admin_factor;
            let response = call!(
                connection.accounts(),
                set_second_factor_policy,
                request,
                &receipt.cancel,
                false
            );
            if auth_contract::admit_set_second_factor_policy_response(&response).is_err()
                || response.policy.require_admin_factor != requested
            {
                return Completion::state(MutationState::Uncertain);
            }
            match response.continuation {
                buffa::EnumValue::Known(auth::SecondFactorSessionContinuation::Continues) => (),
                buffa::EnumValue::Known(
                    auth::SecondFactorSessionContinuation::ReauthenticationRequired,
                ) => {
                    if !receipt.cancel.is_cancelled() {
                        let _ = retire(session, lease.generation).await;
                    }
                }
                buffa::EnumValue::Known(auth::SecondFactorSessionContinuation::Unspecified)
                | buffa::EnumValue::Unknown(_) => {
                    return Completion::state(MutationState::Uncertain);
                }
            }
            Completion::published(true)
        }
        Operation::RecoveryCodes(request) => {
            if request.account_id.value != lease.account_id().as_str() {
                return Completion::state(MutationState::Rejected);
            }
            let response = call!(
                connection.auth(),
                replace_recovery_codes,
                request,
                &receipt.cancel,
                false
            );
            let codes = Zeroizing::new(response.recovery_codes);
            let mut unique = std::collections::BTreeSet::new();
            if !response.__buffa_unknown_fields.is_empty()
                || codes.len() != 10
                || codes
                    .iter()
                    .any(|code| !recovery_code(code) || !unique.insert(code.as_str()))
            {
                return Completion::state(MutationState::Uncertain);
            }
            let value = Zeroizing::new(codes.join("\n"));
            if receipt.cancel.is_cancelled() {
                return Completion::state(MutationState::Uncertain);
            }
            let Some(generation) = retire(session, lease.generation).await else {
                return Completion::state(MutationState::Uncertain);
            };
            Completion {
                state: MutationState::Published,
                secret: Some((SecretKind::RecoveryCodes, value)),
                recovery_generation: Some(generation),
            }
        }
    }
}

async fn retire(session: &SessionCoordinator, expected: Generation) -> Option<Generation> {
    let view = *session.subscribe().borrow();
    if view.generation != expected
        && matches!(
            view.phase,
            home_client_session::Phase::Ready
                | home_client_session::Phase::Refreshing
                | home_client_session::Phase::Busy
                | home_client_session::Phase::Challenge { .. }
                | home_client_session::Phase::EnrollmentRequired
        )
    {
        return None;
    }
    match session.logout_local_if_current(view.generation).await {
        Ok(true) => Some(session.generation()),
        Ok(false) => None,
        Err(_) => None,
    }
}
fn recovery_code(value: &str) -> bool {
    let bytes = value.as_bytes();
    bytes.len() == 30
        && bytes.iter().enumerate().all(|(index, &value)| {
            if matches!(index, 5 | 11 | 17 | 23) {
                value == b'-'
            } else {
                value.is_ascii_uppercase() || (b'2'..=b'7').contains(&value)
            }
        })
}
