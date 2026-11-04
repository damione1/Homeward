//! Private administration data and edit preconditions, independent of Slint.
//! Server permissions remain authoritative; a locally admitted role only gates UI.
use std::collections::{BTreeMap, BTreeSet};

use buffa::EnumValue;
use buffa_types::google::protobuf::FieldMask;
use home_wire_contract::{
    auth_contract::{self, AccountName, SessionName},
    configuration_contract,
    google::longrunning,
    home::api::v2 as auth,
    home::api::v2 as config,
    integration_admin_contract::{self, IntegrationAdminRequest},
    integration_operations_contract, observability_contract,
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
    MfaPolicy,
    McpKeys,
    Accounts,
    Invitations,
    Policies,
    Integrations,
    Configuration,
    Health,
    Metrics,
    Logs,
    Audits,
    SecretAudits,
}
impl Section {
    pub const ALL: [Self; 15] = [
        Self::Account,
        Self::Sessions,
        Self::Factors,
        Self::MfaPolicy,
        Self::McpKeys,
        Self::Accounts,
        Self::Invitations,
        Self::Policies,
        Self::Integrations,
        Self::Configuration,
        Self::Health,
        Self::Metrics,
        Self::Logs,
        Self::Audits,
        Self::SecretAudits,
    ];
    pub fn label(self) -> &'static str {
        match self {
            Self::Account => "My Account",
            Self::Sessions => "Sessions",
            Self::Factors => "Security",
            Self::MfaPolicy => "MFA Policy",
            Self::McpKeys => "MCP Keys",
            Self::Accounts => "Accounts",
            Self::Invitations => "Invitations",
            Self::Policies => "Permissions",
            Self::Integrations => "Integrations",
            Self::Configuration => "Configuration",
            Self::Health => "Health",
            Self::Metrics => "Metrics",
            Self::Logs => "Logs",
            Self::Audits => "Access Audits",
            Self::SecretAudits => "Secret Audits",
        }
    }
    pub fn audit_source(self) -> Option<home_wire_contract::home::api::v2::ObservationAuditSource> {
        use home_wire_contract::home::api::v2::ObservationAuditSource as AuditSource;
        match self {
            Self::Audits => Some(AuditSource::Authentication),
            Self::SecretAudits => Some(AuditSource::SecretAccess),
            Self::Account
            | Self::Sessions
            | Self::Factors
            | Self::MfaPolicy
            | Self::McpKeys
            | Self::Accounts
            | Self::Invitations
            | Self::Policies
            | Self::Integrations
            | Self::Configuration
            | Self::Health
            | Self::Metrics
            | Self::Logs => None,
        }
    }
    pub fn administrative(self) -> bool {
        match self {
            Self::Account | Self::Sessions | Self::Factors | Self::McpKeys => false,
            Self::Accounts
            | Self::MfaPolicy
            | Self::Invitations
            | Self::Policies
            | Self::Integrations
            | Self::Configuration
            | Self::Health
            | Self::Metrics
            | Self::Logs
            | Self::Audits
            | Self::SecretAudits => true,
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
    pub account: AccountName,
    pub session: SessionName,
}

pub struct Authority {
    pub identity: Identity,
    account: Option<auth::Account>,
}
impl Authority {
    pub fn admit(identity: Identity, account: auth::Account) -> Result<Self, Refusal> {
        auth_contract::admit_account(&account).map_err(|_| Refusal::Invalid)?;
        let received =
            AccountName::try_from(account.name.as_str()).map_err(|_| Refusal::Invalid)?;
        if received != identity.account || account.state != auth::AccountState::Active {
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
    secret_kind: Option<SecretKind>,
}
impl<T, D> Default for PrivateState<T, D> {
    fn default() -> Self {
        Self {
            context: Context(0),
            authority: None,
            panes: BTreeMap::new(),
            draft: None,
            one_time_secret: None,
            secret_kind: None,
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
        self.secret_kind = None;
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
        self.secret_kind = Some(kind);
        Ok(())
    }
    pub fn secret(&self) -> Option<&str> {
        self.one_time_secret.as_ref().map(|value| value.as_str())
    }
    pub fn secret_kind(&self) -> Option<SecretKind> {
        self.secret_kind
    }
    pub fn dismiss_secret(&mut self) {
        self.one_time_secret = None;
        self.secret_kind = None;
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
    pub fn integration_operation(value: &longrunning::Operation) -> Result<Self, Refusal> {
        let receipt =
            integration_operations_contract::admit_response(value).map_err(|_| Refusal::Invalid)?;
        match receipt.phase() {
            config::IntegrationOperationPhase::Published
            | config::IntegrationOperationPhase::NoChange => Ok(Self::Published),
            config::IntegrationOperationPhase::Accepted
            | config::IntegrationOperationPhase::Committed => Ok(Self::Reconciling),
            config::IntegrationOperationPhase::Failed => Ok(Self::Rejected),
            config::IntegrationOperationPhase::Unspecified => Err(Refusal::Unsupported),
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
fn resource_home<'a>(value: &'a str, collection: &str) -> Option<&'a str> {
    let (home, id) = value.split_once(&format!("/{collection}/"))?;
    if configuration_contract::home_name(home)
        && !id.is_empty()
        && id.len() <= 63
        && !id.starts_with('-')
        && !id.ends_with('-')
        && id
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
    {
        Some(home)
    } else {
        None
    }
}
fn valid_etag(value: &str) -> bool {
    value.len() > 4
        && value.len() <= 256
        && value.starts_with("W/\"")
        && value.ends_with('"')
        && value[3..value.len() - 1]
            .chars()
            .all(|c| c >= '\u{21}' && c != '\u{7f}' && c != '"')
}
fn references(
    values: &[config::IntegrationSecretReference],
    instance_key: &str,
    opaque: bool,
) -> bool {
    let mut fields = BTreeSet::new();
    values.len() <= 32
        && values.iter().all(|value| {
            value.__buffa_unknown_fields.is_empty()
                && bounded_text(&value.field, 256)
                && bounded_text(&value.secret_key, 256)
                && value.instance_key == instance_key
                && (!opaque || value.field == "secret_names")
                && fields.insert(if opaque {
                    value.secret_key.as_str()
                } else {
                    value.field.as_str()
                })
        })
}

pub fn admit_integration_instance(value: &config::IntegrationInstance) -> Result<(), Refusal> {
    if !value.__buffa_unknown_fields.is_empty()
        || !bounded_text(&value.instance_key, 256)
        || !valid_etag(&value.etag)
    {
        return Err(Refusal::Invalid);
    }
    let home = resource_home(&value.name, "integrationInstances").ok_or(Refusal::Invalid)?;
    if resource_home(&value.configuration_document, "configurationDocuments") != Some(home) {
        return Err(Refusal::Invalid);
    }
    match value.desired_state {
        EnumValue::Known(
            config::IntegrationDesiredState::Enabled | config::IntegrationDesiredState::Disabled,
        ) => (),
        EnumValue::Known(config::IntegrationDesiredState::Unspecified) | EnumValue::Unknown(_) => {
            return Err(Refusal::Unsupported);
        }
    }
    match value.configuration.as_ref() {
        Some(config::integration_instance::Configuration::Editable(source)) => {
            if resource_home(&value.integration_type, "integrationTypes") != Some(home)
                || !source.__buffa_unknown_fields.is_empty()
                || !bounded_text(&source.schema_version, 256)
                || source.yaml_source.is_empty()
                || source.yaml_source.len() > MAX_FRAGMENT_BYTES
                || std::str::from_utf8(&source.yaml_source).is_err()
                || source.yaml_source.contains(&0)
                || !references(&source.secrets, &value.instance_key, false)
            {
                return Err(Refusal::Invalid);
            }
        }
        Some(config::integration_instance::Configuration::Opaque(source)) => {
            if !value.integration_type.is_empty()
                || !source.__buffa_unknown_fields.is_empty()
                || !references(&source.secrets, &value.instance_key, true)
            {
                return Err(Refusal::Invalid);
            }
        }
        None => return Err(Refusal::Invalid),
    }
    if let Some(health) = &value.observed_health {
        match health {
            config::integration_instance::ObservedHealth::NoHealthReport(empty) => {
                if !empty.__buffa_unknown_fields.is_empty() {
                    return Err(Refusal::Invalid);
                }
            }
            config::integration_instance::ObservedHealth::HealthReport(report) => {
                if !report.__buffa_unknown_fields.is_empty() {
                    return Err(Refusal::Invalid);
                }
                match report.status {
                    EnumValue::Known(
                        config::IntegrationHealthStatus::Ready
                        | config::IntegrationHealthStatus::Degraded
                        | config::IntegrationHealthStatus::Unavailable,
                    ) => (),
                    EnumValue::Known(config::IntegrationHealthStatus::Unspecified)
                    | EnumValue::Unknown(_) => return Err(Refusal::Unsupported),
                }
                match &report.admission {
                    Some(config::integration_health_report::Admission::AdmissionTime(ts)) => {
                        if !configuration_contract::timestamp(ts) {
                            return Err(Refusal::Invalid);
                        }
                    }
                    Some(config::integration_health_report::Admission::AdmissionNotRecorded(
                        empty,
                    )) => {
                        if !empty.__buffa_unknown_fields.is_empty() {
                            return Err(Refusal::Invalid);
                        }
                    }
                    None => return Err(Refusal::Invalid),
                }
            }
        }
    }
    if let Some(supervision) = &value.observed_supervision {
        match supervision {
            config::integration_instance::ObservedSupervision::NotSupervised(empty) => {
                if !empty.__buffa_unknown_fields.is_empty() {
                    return Err(Refusal::Invalid);
                }
            }
            config::integration_instance::ObservedSupervision::Supervision(snapshot) => {
                if !snapshot.__buffa_unknown_fields.is_empty()
                    || snapshot.attempts < 0
                    || snapshot.tasks < 0
                {
                    return Err(Refusal::Invalid);
                }
                match snapshot.lifecycle {
                    EnumValue::Known(
                        config::IntegrationLifecycle::Starting
                        | config::IntegrationLifecycle::Running
                        | config::IntegrationLifecycle::Retrying
                        | config::IntegrationLifecycle::Degraded
                        | config::IntegrationLifecycle::Stopped,
                    ) => (),
                    EnumValue::Known(config::IntegrationLifecycle::Unspecified)
                    | EnumValue::Unknown(_) => return Err(Refusal::Unsupported),
                }
            }
        }
    }
    Ok(())
}

/// The editor retains the exact server secret references outside the editable YAML.
/// Opaque configurations remain read-only. No global redacted document is rebuilt.
pub struct IntegrationDraft {
    id: IntegrationId,
    instance_key: String,
    integration_type: String,
    etag: String,
    source: config::IntegrationConfiguration,
    state: MutationState,
}
impl IntegrationDraft {
    pub fn open(instance: &config::IntegrationInstance) -> Result<Self, Refusal> {
        admit_integration_instance(instance)?;
        let id = IntegrationId::parse(&instance.name)?;
        let source = match instance.configuration.as_ref() {
            Some(config::integration_instance::Configuration::Editable(value)) => {
                value.as_ref().clone()
            }
            Some(config::integration_instance::Configuration::Opaque(_)) => {
                return Err(Refusal::ReadOnly);
            }
            None => return Err(Refusal::Invalid),
        };
        let draft = Self {
            id,
            instance_key: instance.instance_key.clone(),
            integration_type: instance.integration_type.clone(),
            etag: instance.etag.clone(),
            source,
            state: MutationState::Draft,
        };
        draft.prepare_update()?;
        Ok(draft)
    }
    pub fn id(&self) -> &IntegrationId {
        &self.id
    }
    pub fn instance_key(&self) -> &str {
        &self.instance_key
    }
    pub fn state(&self) -> MutationState {
        self.state
    }
    pub fn yaml(&self) -> &str {
        std::str::from_utf8(&self.source.yaml_source).expect("admitted UTF-8 fragment")
    }
    pub fn secret_references(&self) -> &[config::IntegrationSecretReference] {
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
    pub fn prepare_update(&self) -> Result<config::UpdateIntegrationInstanceRequest, Refusal> {
        if !self.state.submit_allowed() {
            return Err(Refusal::Stale);
        }
        let source = std::str::from_utf8(&self.source.yaml_source).map_err(|_| Refusal::Invalid)?;
        if source.contains('\0') {
            return Err(Refusal::Invalid);
        }
        let request = config::UpdateIntegrationInstanceRequest {
            integration_instance: config::IntegrationInstance {
                name: self.id.as_str().into(),
                instance_key: self.instance_key.clone(),
                integration_type: self.integration_type.clone(),
                etag: self.etag.clone(),
                configuration: Some(config::integration_instance::Configuration::Editable(
                    Box::new(self.source.clone()),
                )),
                ..Default::default()
            }
            .into(),
            update_mask: FieldMask {
                paths: vec!["editable".into()],
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        integration_admin_contract::admit_integration_admin_request(
            IntegrationAdminRequest::Update(&request),
        )
        .map_err(|_| Refusal::Invalid)?;
        Ok(request)
    }
    pub fn begin_update(&mut self) -> Result<config::UpdateIntegrationInstanceRequest, Refusal> {
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
        state: config::IntegrationDesiredState,
    ) -> Result<config::UpdateIntegrationInstanceRequest, Refusal> {
        if !self.state.submit_allowed() {
            return Err(Refusal::Stale);
        }
        let request = config::UpdateIntegrationInstanceRequest {
            integration_instance: config::IntegrationInstance {
                name: self.id.as_str().into(),
                desired_state: state.into(),
                etag: self.etag.clone(),
                ..Default::default()
            }
            .into(),
            update_mask: FieldMask {
                paths: vec!["desired_state".into()],
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        integration_admin_contract::admit_integration_admin_request(
            IntegrationAdminRequest::Update(&request),
        )
        .map_err(|_| Refusal::Invalid)?;
        Ok(request)
    }
}

/// Opaque source stays read-only, but the server still exposes desired-state
/// changes and deletion with the same exact resource etag precondition.
pub fn prepare_enabled(
    instance: &config::IntegrationInstance,
    state: config::IntegrationDesiredState,
) -> Result<config::UpdateIntegrationInstanceRequest, Refusal> {
    admit_integration_instance(instance)?;
    let id = IntegrationId::parse(&instance.name)?;
    let request = config::UpdateIntegrationInstanceRequest {
        integration_instance: config::IntegrationInstance {
            name: id.as_str().into(),
            desired_state: state.into(),
            etag: instance.etag.clone(),
            ..Default::default()
        }
        .into(),
        update_mask: FieldMask {
            paths: vec!["desired_state".into()],
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    integration_admin_contract::admit_integration_admin_request(IntegrationAdminRequest::Update(
        &request,
    ))
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
            || !bounded_text(&account.name, 256)
            || account.display_name.len() > 256
            || !identities.insert(account.name.as_str())
        {
            return Err(Refusal::Invalid);
        }
    }
    Ok(response.accounts)
}

pub fn admit_integration_health(
    value: &home_wire_contract::home::api::v2::IntegrationHealth,
) -> Result<(), Refusal> {
    observability_contract::admit_integration_health_response(value)
        .map(|_| ())
        .map_err(|_| Refusal::Invalid)
}
