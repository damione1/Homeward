//! Private administration data and edit preconditions, independent of Slint.
//! Server permissions remain authoritative; a locally admitted role only gates UI.
use std::collections::{BTreeMap, BTreeSet};

use buffa::EnumValue;
use home_wire_contract::{
    auth_contract::{self, AccountId, SessionId},
    configuration_contract,
    home::{auth::v1 as auth, configuration::v1 as config, integrationadmin::v1 as integration},
    integration_admin_contract::{self, IntegrationAdminRequest},
    observability_contract,
};
use zeroize::Zeroizing;

#[cfg(test)]
mod tests;

pub const MAX_ROWS: usize = 1024;
pub const MAX_FRAGMENT_BYTES: usize = 65_536;

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Section {
    Account,
    Sessions,
    Factors,
    McpKeys,
    Accounts,
    Invitations,
    Policies,
    Integrations,
    Health,
    Metrics,
    Logs,
    Audits,
}
impl Section {
    pub const ALL: [Self; 12] = [
        Self::Account,
        Self::Sessions,
        Self::Factors,
        Self::McpKeys,
        Self::Accounts,
        Self::Invitations,
        Self::Policies,
        Self::Integrations,
        Self::Health,
        Self::Metrics,
        Self::Logs,
        Self::Audits,
    ];
    pub fn label(self) -> &'static str {
        match self {
            Self::Account => "Mon compte",
            Self::Sessions => "Sessions",
            Self::Factors => "Sécurité",
            Self::McpKeys => "Clés MCP",
            Self::Accounts => "Comptes",
            Self::Invitations => "Invitations",
            Self::Policies => "Droits",
            Self::Integrations => "Intégrations",
            Self::Health => "Santé",
            Self::Metrics => "Métriques",
            Self::Logs => "Journaux",
            Self::Audits => "Audits",
        }
    }
    pub fn administrative(self) -> bool {
        match self {
            Self::Account | Self::Sessions | Self::Factors | Self::McpKeys => false,
            Self::Accounts
            | Self::Invitations
            | Self::Policies
            | Self::Integrations
            | Self::Health
            | Self::Metrics
            | Self::Logs
            | Self::Audits => true,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Refusal {
    Invalid,
    Unsupported,
    ReadOnly,
    Stale,
    Denied,
    Exhausted,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Identity {
    pub account: AccountId,
    pub session: SessionId,
}

pub struct Authority {
    pub identity: Identity,
    account: Option<auth::Account>,
}
impl Authority {
    pub fn admit(identity: Identity, account: auth::Account) -> Result<Self, Refusal> {
        auth_contract::admit_account(&account).map_err(|_| Refusal::Invalid)?;
        let received = AccountId::try_from(&*account.id).map_err(|_| Refusal::Invalid)?;
        if received != identity.account || account.status != auth::AccountStatus::Active {
            return Err(Refusal::Denied);
        }
        Ok(Self {
            identity,
            account: Some(account),
        })
    }
    /// Personal session/factor/MCP routes remain available when the server
    /// refuses AccountService, whose public transport is admin-only.
    pub fn session_only(identity: Identity) -> Self {
        Self {
            identity,
            account: None,
        }
    }
    pub fn account(&self) -> Option<&auth::Account> {
        self.account.as_ref()
    }
    pub fn administrator(&self) -> bool {
        self.account
            .as_ref()
            .is_some_and(|account| account.role == auth::AccountRole::Admin)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Context(u64);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SecretKind {
    Invitation,
    McpKey,
    RecoveryCodes,
}
impl SecretKind {
    fn section(self) -> Section {
        match self {
            Self::Invitation => Section::Invitations,
            Self::McpKey => Section::McpKeys,
            Self::RecoveryCodes => Section::Factors,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Stage {
    Loading,
    Ready,
    Empty,
    Denied,
    Offline,
    Unavailable,
    Unsupported,
    Invalid,
}

/// An independent pane failure cannot replace another service's result.
pub struct Pane<T> {
    pub stage: Stage,
    pub content: Option<T>,
}

/// One session context owns all protected panes and drafts. Contexts are never reused.
/// Dropping the one-time secret clears its owned buffer; it is never part of rows.
pub struct PrivateState<T, D> {
    context: Context,
    authority: Option<Authority>,
    panes: BTreeMap<Section, Pane<T>>,
    draft: Option<D>,
    one_time_secret: Option<Zeroizing<String>>,
}
impl<T, D> Default for PrivateState<T, D> {
    fn default() -> Self {
        Self {
            context: Context(0),
            authority: None,
            panes: BTreeMap::new(),
            draft: None,
            one_time_secret: None,
        }
    }
}
impl<T, D> PrivateState<T, D> {
    pub fn context(&self) -> Context {
        self.context
    }
    pub fn authority(&self) -> Option<&Authority> {
        self.authority.as_ref()
    }
    pub fn pane(&self, section: Section) -> Option<&Pane<T>> {
        self.panes.get(&section)
    }
    pub fn draft(&self) -> Option<&D> {
        self.draft.as_ref()
    }
    pub fn draft_mut(&mut self) -> Option<&mut D> {
        self.draft.as_mut()
    }
    pub fn dismiss_draft(&mut self) {
        self.draft = None;
    }
    pub fn retire(&mut self) -> Result<(), Refusal> {
        self.authority = None;
        self.panes.clear();
        self.draft = None;
        self.one_time_secret = None;
        self.context = Context(self.context.0.checked_add(1).ok_or(Refusal::Exhausted)?);
        Ok(())
    }
    pub fn install(&mut self, authority: Authority) -> Result<Context, Refusal> {
        self.retire()?;
        self.authority = Some(authority);
        Ok(self.context)
    }
    fn admit(&self, context: Context, section: Section) -> Result<(), Refusal> {
        if context != self.context {
            return Err(Refusal::Stale);
        }
        let authority = self.authority.as_ref().ok_or(Refusal::Denied)?;
        if section.administrative() && !authority.administrator() {
            return Err(Refusal::Denied);
        }
        Ok(())
    }
    pub fn publish(
        &mut self,
        context: Context,
        section: Section,
        pane: Pane<T>,
    ) -> Result<(), Refusal> {
        self.admit(context, section)?;
        self.panes.insert(section, pane);
        Ok(())
    }
    pub fn edit(&mut self, context: Context, section: Section, draft: D) -> Result<(), Refusal> {
        self.admit(context, section)?;
        self.draft = Some(draft);
        Ok(())
    }
    pub fn show_secret(
        &mut self,
        context: Context,
        kind: SecretKind,
        value: Zeroizing<String>,
    ) -> Result<(), Refusal> {
        self.admit(context, kind.section())?;
        if value.is_empty() || value.len() > 4096 || value.contains(['\0', '\r', '\n']) {
            return Err(Refusal::Invalid);
        }
        self.one_time_secret = Some(value);
        Ok(())
    }
    pub fn secret(&self) -> Option<&str> {
        self.one_time_secret.as_ref().map(|value| value.as_str())
    }
    pub fn dismiss_secret(&mut self) {
        self.one_time_secret = None;
    }
}

/// An uncertain write is never represented as a retryable draft.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum MutationState {
    Draft,
    InFlight,
    Conflict,
    Uncertain,
    Published,
    Reconciling,
    RestartRequired,
    Rejected,
}
impl MutationState {
    pub fn activation(value: &config::ActivationResult) -> Result<Self, Refusal> {
        configuration_contract::admit_configuration_activation(value)
            .map_err(|_| Refusal::Invalid)?;
        match value.outcome {
            EnumValue::Known(config::ActivationOutcome::Published) => Ok(Self::Published),
            EnumValue::Known(config::ActivationOutcome::CommittedReconciling) => {
                Ok(Self::Reconciling)
            }
            EnumValue::Known(config::ActivationOutcome::CommitUnknown) => Ok(Self::Uncertain),
            EnumValue::Known(config::ActivationOutcome::RestartRequired) => {
                Ok(Self::RestartRequired)
            }
            EnumValue::Known(
                config::ActivationOutcome::RejectedBeforeCommit
                | config::ActivationOutcome::NotCommitted,
            ) => Ok(Self::Rejected),
            EnumValue::Known(config::ActivationOutcome::Unspecified) | EnumValue::Unknown(_) => {
                Err(Refusal::Unsupported)
            }
        }
    }
    pub fn submit_allowed(self) -> bool {
        self == Self::Draft
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct IntegrationId(String);
impl IntegrationId {
    pub fn parse(value: &str) -> Result<Self, Refusal> {
        if !bounded_text(value, 256) {
            return Err(Refusal::Invalid);
        }
        Ok(Self(value.into()))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
fn bounded_text(value: &str, limit: usize) -> bool {
    !value.trim().is_empty() && value.len() <= limit && !value.contains(['\0', '\r', '\n'])
}
fn references(values: &[integration::SecretReference], id: &IntegrationId) -> bool {
    let mut fields = BTreeSet::new();
    values.len() <= 32
        && values.iter().all(|value| {
            value.__buffa_unknown_fields.is_empty()
                && bounded_text(&value.field, 256)
                && bounded_text(&value.name, 256)
                && value.integration_instance_id == id.as_str()
                && fields.insert(value.field.as_str())
        })
}

pub fn admit_integration_instance(value: &integration::IntegrationInstance) -> Result<(), Refusal> {
    if !value.__buffa_unknown_fields.is_empty() || !bounded_text(&value.type_id, 256) {
        return Err(Refusal::Invalid);
    }
    let id = IntegrationId::parse(&value.integration_instance_id)?;
    match value.desired_state {
        EnumValue::Known(
            integration::DesiredState::Enabled | integration::DesiredState::Disabled,
        ) => (),
        EnumValue::Known(integration::DesiredState::Unspecified) | EnumValue::Unknown(_) => {
            return Err(Refusal::Unsupported);
        }
    }
    match value.configuration.as_ref() {
        Some(integration::integration_instance::Configuration::Editable(source)) => {
            if !source.__buffa_unknown_fields.is_empty()
                || !bounded_text(&source.schema_version, 256)
                || source.yaml_source.is_empty()
                || source.yaml_source.len() > MAX_FRAGMENT_BYTES
                || std::str::from_utf8(&source.yaml_source).is_err()
                || source.yaml_source.contains(&0)
                || !references(&source.secrets, &id)
            {
                return Err(Refusal::Invalid);
            }
        }
        Some(integration::integration_instance::Configuration::Opaque(source)) => {
            if !source.__buffa_unknown_fields.is_empty() || !references(&source.secrets, &id) {
                return Err(Refusal::Invalid);
            }
        }
        None => return Err(Refusal::Invalid),
    }
    admit_integration_health(
        &home_wire_contract::home::observability::v1::GetIntegrationHealthResponse {
            report: value.report.clone(),
            supervision: value.supervision.clone(),
            ..Default::default()
        },
    )
}

/// The editor retains the exact server secret references outside the editable YAML.
/// Opaque configurations remain read-only. No global redacted document is rebuilt.
pub struct IntegrationDraft {
    id: IntegrationId,
    type_id: String,
    source: integration::PublicConfiguration,
    expected: integration::MutationPrecondition,
    state: MutationState,
}
fn mutation_precondition(
    document: &config::ReadDocumentResponse,
) -> Result<integration::MutationPrecondition, Refusal> {
    configuration_contract::admit_configuration_read(document).map_err(|_| Refusal::Invalid)?;
    let metadata = document.document.as_option().ok_or(Refusal::Invalid)?;
    if metadata.id.value != "operational.main"
        || metadata.kind != config::DocumentKind::OperationalYaml
    {
        return Err(Refusal::Invalid);
    }
    let workspace = match metadata.workspace_state.as_ref() {
        Some(config::document::WorkspaceState::Workspace(value)) => value,
        Some(config::document::WorkspaceState::WorkspaceUnobserved(_)) => {
            return Err(Refusal::ReadOnly);
        }
        None => return Err(Refusal::Invalid),
    };
    Ok(integration::MutationPrecondition {
        expected: metadata.version.clone(),
        expected_workspace_sha256: workspace.source_sha256.clone(),
        ..Default::default()
    })
}
impl IntegrationDraft {
    pub fn open(
        instance: &integration::IntegrationInstance,
        document: &config::ReadDocumentResponse,
    ) -> Result<Self, Refusal> {
        Self::from_parts(instance, mutation_precondition(document)?)
    }
    fn from_parts(
        instance: &integration::IntegrationInstance,
        expected: integration::MutationPrecondition,
    ) -> Result<Self, Refusal> {
        if !instance.__buffa_unknown_fields.is_empty() || !bounded_text(&instance.type_id, 256) {
            return Err(Refusal::Invalid);
        }
        let id = IntegrationId::parse(&instance.integration_instance_id)?;
        match instance.desired_state {
            EnumValue::Known(
                integration::DesiredState::Enabled | integration::DesiredState::Disabled,
            ) => (),
            EnumValue::Known(integration::DesiredState::Unspecified) | EnumValue::Unknown(_) => {
                return Err(Refusal::Unsupported);
            }
        }
        let source = match instance.configuration.as_ref() {
            Some(integration::integration_instance::Configuration::Editable(value)) => {
                value.as_ref().clone()
            }
            Some(integration::integration_instance::Configuration::Opaque(_)) => {
                return Err(Refusal::ReadOnly);
            }
            None => return Err(Refusal::Invalid),
        };
        if !references(&source.secrets, &id) {
            return Err(Refusal::Invalid);
        }
        let draft = Self {
            id,
            type_id: instance.type_id.clone(),
            source,
            expected,
            state: MutationState::Draft,
        };
        draft.prepare_update()?;
        Ok(draft)
    }
    pub fn id(&self) -> &IntegrationId {
        &self.id
    }
    pub fn state(&self) -> MutationState {
        self.state
    }
    pub fn yaml(&self) -> &str {
        std::str::from_utf8(&self.source.yaml_source).expect("admitted UTF-8 fragment")
    }
    pub fn secret_references(&self) -> &[integration::SecretReference] {
        &self.source.secrets
    }
    pub fn edit_yaml(&mut self, value: &str) -> Result<(), Refusal> {
        if !self.state.submit_allowed() {
            return Err(Refusal::Stale);
        }
        if value.is_empty() || value.len() > MAX_FRAGMENT_BYTES || value.contains('\0') {
            return Err(Refusal::Invalid);
        }
        self.source.yaml_source = value.as_bytes().to_vec();
        Ok(())
    }
    pub fn prepare_update(&self) -> Result<integration::UpdateInstanceRequest, Refusal> {
        if !self.state.submit_allowed() {
            return Err(Refusal::Stale);
        }
        let source = std::str::from_utf8(&self.source.yaml_source).map_err(|_| Refusal::Invalid)?;
        if source.contains('\0') {
            return Err(Refusal::Invalid);
        }
        let request = integration::UpdateInstanceRequest {
            type_id: self.type_id.clone(),
            integration_instance_id: self.id.as_str().into(),
            configuration: self.source.clone().into(),
            precondition: self.expected.clone().into(),
            ..Default::default()
        };
        integration_admin_contract::admit_integration_admin_request(
            IntegrationAdminRequest::Update(&request),
        )
        .map_err(|_| Refusal::Invalid)?;
        Ok(request)
    }
    pub fn begin_update(&mut self) -> Result<integration::UpdateInstanceRequest, Refusal> {
        let request = self.prepare_update()?;
        self.state = MutationState::InFlight;
        Ok(request)
    }
    pub fn complete(&mut self, outcome: MutationState) -> Result<(), Refusal> {
        if self.state != MutationState::InFlight
            || matches!(outcome, MutationState::Draft | MutationState::InFlight)
        {
            return Err(Refusal::Stale);
        }
        self.state = outcome;
        Ok(())
    }
    pub fn set_enabled(
        &self,
        state: integration::DesiredState,
    ) -> Result<integration::SetInstanceEnabledRequest, Refusal> {
        if !self.state.submit_allowed() {
            return Err(Refusal::Stale);
        }
        let request = integration::SetInstanceEnabledRequest {
            integration_instance_id: self.id.as_str().into(),
            desired_state: state.into(),
            precondition: self.expected.clone().into(),
            ..Default::default()
        };
        integration_admin_contract::admit_integration_admin_request(
            IntegrationAdminRequest::SetEnabled(&request),
        )
        .map_err(|_| Refusal::Invalid)?;
        Ok(request)
    }
}

/// Opaque source stays read-only, but the server still exposes desired-state
/// changes and deletion with the same exact document/workspace preconditions.
pub fn prepare_enabled(
    instance: &integration::IntegrationInstance,
    document: &config::ReadDocumentResponse,
    state: integration::DesiredState,
) -> Result<integration::SetInstanceEnabledRequest, Refusal> {
    if !instance.__buffa_unknown_fields.is_empty() || !bounded_text(&instance.type_id, 256) {
        return Err(Refusal::Invalid);
    }
    match instance.desired_state {
        EnumValue::Known(
            integration::DesiredState::Enabled | integration::DesiredState::Disabled,
        ) => (),
        EnumValue::Known(integration::DesiredState::Unspecified) | EnumValue::Unknown(_) => {
            return Err(Refusal::Unsupported);
        }
    }
    let id = IntegrationId::parse(&instance.integration_instance_id)?;
    let request = integration::SetInstanceEnabledRequest {
        integration_instance_id: id.as_str().into(),
        desired_state: state.into(),
        precondition: mutation_precondition(document)?.into(),
        ..Default::default()
    };
    integration_admin_contract::admit_integration_admin_request(
        IntegrationAdminRequest::SetEnabled(&request),
    )
    .map_err(|_| Refusal::Invalid)?;
    Ok(request)
}

/// Admit an entire bounded account capture before any row is published.
pub fn admit_accounts(response: auth::ListAccountsResponse) -> Result<Vec<auth::Account>, Refusal> {
    if !response.__buffa_unknown_fields.is_empty() || response.accounts.len() > MAX_ROWS {
        return Err(Refusal::Invalid);
    }
    let mut identities = BTreeSet::new();
    for account in &response.accounts {
        auth_contract::admit_account(account).map_err(|_| Refusal::Invalid)?;
        if !bounded_text(&account.login_name, 256)
            || !bounded_text(&account.id.value, 128)
            || account.display_name.len() > 256
            || !identities.insert(account.id.value.as_str())
        {
            return Err(Refusal::Invalid);
        }
    }
    Ok(response.accounts)
}

pub fn admit_integration_health(
    value: &home_wire_contract::home::observability::v1::GetIntegrationHealthResponse,
) -> Result<(), Refusal> {
    observability_contract::admit_integration_health_response(value)
        .map(|_| ())
        .map_err(|_| Refusal::Invalid)
}
