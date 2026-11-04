use crate::administration::{self, Authority, Identity, IntegrationId, MAX_ROWS, Section};
use home_client_session::{AccessLease, SessionCoordinator, SessionFailure};
use home_client_transport::{Connection, FailureKind, RpcCode};
use home_wire_contract::{
    auth_contract::{self, AccountName},
    authorization_contract,
    home::{api::v2 as auth, api::v2 as integration, api::v2 as observation},
    observability_contract,
};
use sha2::{Digest, Sha256};
use std::{collections::BTreeSet, time::Duration};
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(super) enum Selection {
    Account(AccountName),
    Integration(IntegrationId),
    Configuration(super::configuration::Selection),
}
#[derive(Clone)]
pub(super) struct PageToken(String);
impl PageToken {
    pub(super) fn configuration(value: &str) -> Result<Self, ReadError> {
        if value.is_empty() || value.len() > 1200 || value.contains(['\0', '\r', '\n']) {
            return Err(ReadError::Invalid);
        }
        Ok(Self(value.into()))
    }
    pub(super) fn as_str(&self) -> &str {
        &self.0
    }
    pub fn parse(value: &str) -> Result<Self, ReadError> {
        if value.is_empty() || value.len() > 2048 || value.contains(['\0', '\r', '\n']) {
            return Err(ReadError::Invalid);
        }
        Ok(Self(value.into()))
    }
}
#[derive(Clone)]
pub(super) struct ResourcePage<T> {
    rows: Vec<T>,
    pub next: Option<PageToken>,
}
impl<T> ResourcePage<T> {
    fn new(rows: Vec<T>, next: String) -> Result<Self, ReadError> {
        if rows.len() > 100 {
            return Err(ReadError::Invalid);
        }
        Ok(Self {
            rows,
            next: if next.is_empty() {
                None
            } else {
                Some(PageToken::parse(&next)?)
            },
        })
    }
}
impl<T> std::ops::Deref for ResourcePage<T> {
    type Target = [T];
    fn deref(&self) -> &[T] {
        &self.rows
    }
}
impl<'a, T> IntoIterator for &'a ResourcePage<T> {
    type Item = &'a T;
    type IntoIter = std::slice::Iter<'a, T>;
    fn into_iter(self) -> Self::IntoIter {
        self.rows.iter()
    }
}
#[derive(Clone)]
pub(super) enum Payload {
    Personal {
        profile: Option<auth::Account>,
    },
    Sessions(ResourcePage<auth::Session>),
    Factors(ResourcePage<auth::FactorCredential>),
    MfaPolicy(auth::SecondFactorPolicy),
    Keys(ResourcePage<auth::McpKey>),
    Accounts(ResourcePage<auth::Account>),
    Invitations(ResourcePage<auth::Invitation>),
    Policy {
        account: AccountName,
        response: auth::AuthorizationPolicy,
    },
    Integrations(Box<IntegrationList>),
    Integration(Box<IntegrationDetail>),
    Configuration(Box<super::configuration::Payload>),
    Health(Box<observation::Health>),
    Metrics(Box<observation::Metrics>),
    Logs(Box<observation::QueryRuntimeLogsResponse>),
    Audits(Box<observation::QueryAuditsResponse>),
}
#[derive(Clone)]
pub(super) struct IntegrationList {
    pub parent: String,
    pub types: Vec<integration::IntegrationType>,
    pub response: integration::ListIntegrationInstancesResponse,
}
#[derive(Clone)]
pub(super) struct IntegrationDetail {
    pub instance: integration::IntegrationInstance,
    pub kind: Result<integration::IntegrationType, ReadError>,
    pub health: Result<observation::IntegrationHealth, ReadError>,
}
#[derive(Clone, Copy, Debug)]
pub(super) enum ReadError {
    Session(SessionFailure),
    Transport(FailureKind),
    Invalid,
    Unsupported,
    Cancelled,
}
pub(super) struct Loaded {
    pub authority: Authority,
    pub section: Section,
    pub payload: Result<Payload, ReadError>,
    pub session_generation: home_client_session::Generation,
}
pub(super) const TIMEOUT: Duration = Duration::from_secs(4);

macro_rules! call {
    ($session:expr,$lease:expr,$cancel:expr,$client:expr,$method:ident,$request:expr) => {{
        let client = $client;
        let response = tokio::select! {biased;
            ()=$cancel.cancelled()=>return Err(ReadError::Cancelled),
            ()=$lease.cancellation.cancelled()=>return Err(ReadError::Cancelled),
            result=client.$method($request,$lease.token(),TIMEOUT,$cancel)=>result,
        };
        match response {
            Ok(value) => value,
            Err(error) => {
                let _ = $session.observe_failure($lease.generation, &error).await;
                return Err(ReadError::Transport(error.kind()));
            }
        }
    }};
}
pub(super) use call;

pub(super) async fn admitted_lease(
    session: &SessionCoordinator,
    expected: home_client_session::Generation,
    cancel: &CancellationToken,
) -> Result<AccessLease, ReadError> {
    if !session.is_current(expected) {
        return Err(ReadError::Cancelled);
    }
    let lease = session.access(cancel).await.map_err(ReadError::Session)?;
    if lease.generation != expected || !session.is_current(expected) {
        return Err(ReadError::Cancelled);
    }
    if [lease.account_name().as_str(), lease.session_name().as_str()]
        .iter()
        .any(|value| value.len() > 256 || value.contains(['\0', '\r', '\n']))
    {
        return Err(ReadError::Invalid);
    }
    Ok(lease)
}
async fn authority(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    cancel: &CancellationToken,
) -> Result<Authority, ReadError> {
    let identity = Identity {
        account: lease.account_name().clone(),
        session: lease.session_name().clone(),
    };
    let api = connection.accounts();
    let profile = tokio::select! {biased;
        ()=cancel.cancelled()=>return Err(ReadError::Cancelled),
        ()=lease.cancellation.cancelled()=>return Err(ReadError::Cancelled),
        result=api.get_account(auth::GetAccountRequest{name:identity.account.as_str().into(),..Default::default()},lease.token(),TIMEOUT,cancel)=>result,
    };
    let authority = match profile {
        Ok(response) => {
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            Authority::admit(identity, response).map_err(|_| ReadError::Invalid)?
        }
        Err(error) => {
            if match error.api_failure() {
                Err(_) => true,
                Ok(Some(detail)) => matches!(
                    detail.reason(),
                    home_wire_contract::home::api::v2::ApiErrorReason::AuthorityUnavailable
                        | home_wire_contract::home::api::v2::ApiErrorReason::AuthenticationRequired
                        | home_wire_contract::home::api::v2::ApiErrorReason::AccessDenied
                ),
                Ok(None) => false,
            } || error.kind() == FailureKind::Rpc(RpcCode::Unauthenticated)
            {
                let _ = session.observe_failure(lease.generation, &error).await;
                return Err(ReadError::Transport(error.kind()));
            }
            // No privileged profile data survives a refused/unavailable probe.
            // Personal endpoints independently authorize the same native lease.
            Authority::session_only(identity)
        }
    };
    Ok(authority)
}
pub(super) async fn probe(
    session: &SessionCoordinator,
    connection: &Connection,
    expected: home_client_session::Generation,
    cancel: &CancellationToken,
) -> Result<Authority, ReadError> {
    let lease = admitted_lease(session, expected, cancel).await?;
    authority(session, connection, &lease, cancel).await
}
pub(super) async fn load(
    session: &SessionCoordinator,
    connection: &Connection,
    expected: home_client_session::Generation,
    requested: Section,
    selection: Option<Selection>,
    page: Option<PageToken>,
    cancel: &CancellationToken,
) -> Result<Loaded, ReadError> {
    let lease = admitted_lease(session, expected, cancel).await?;
    let authority = authority(session, connection, &lease, cancel).await?;
    let section = if requested.administrative() && !authority.administrator() {
        Section::Account
    } else {
        requested
    };
    let mut payload = read_section(
        session, connection, &lease, section, selection, page, cancel,
    )
    .await;
    if let Ok(Payload::Personal { profile }) = &mut payload {
        *profile = authority.account().cloned();
    }
    Ok(Loaded {
        authority,
        section,
        payload,
        session_generation: lease.generation,
    })
}

fn metadata<T>(rows: &[T], valid: impl Fn(&T) -> bool) -> Result<(), ReadError> {
    if rows.len() > MAX_ROWS || rows.iter().any(|row| !valid(row)) {
        Err(ReadError::Invalid)
    } else {
        Ok(())
    }
}
fn distinct<'a>(values: impl Iterator<Item = &'a str>) -> Result<(), ReadError> {
    let mut ids = BTreeSet::new();
    for value in values {
        if value.is_empty() || value.len() > 256 || !ids.insert(value) {
            return Err(ReadError::Invalid);
        }
    }
    Ok(())
}
fn kind(value: &integration::IntegrationType) -> Result<(), ReadError> {
    if !value.__buffa_unknown_fields.is_empty()
        || IntegrationId::parse(&value.name).is_err()
        || IntegrationId::parse(&value.type_key).is_err()
        || value.manifest_version.is_empty()
        || value.manifest_version.len() > 256
        || value.schema_version.is_empty()
        || value.schema_version.len() > 256
        || value.json_schema.is_empty()
        || value.json_schema.len() > 65536
        || value.schema_sha256.as_slice() != &Sha256::digest(&value.json_schema)[..]
        || value.capability_schema_ids.len() > 128
        || value
            .capability_schema_ids
            .iter()
            .any(|id| id.is_empty() || id.len() > 256)
    {
        return Err(ReadError::Invalid);
    }
    let json: serde_json::Value =
        serde_json::from_slice(&value.json_schema).map_err(|_| ReadError::Invalid)?;
    if !json.is_object() {
        return Err(ReadError::Unsupported);
    }
    Ok(())
}
async fn read_section(
    session: &SessionCoordinator,
    connection: &Connection,
    lease: &AccessLease,
    section: Section,
    selection: Option<Selection>,
    page: Option<PageToken>,
    cancel: &CancellationToken,
) -> Result<Payload, ReadError> {
    let own = lease.account_name().as_str().to_owned();
    let home = lease.account_name().home().as_str().to_owned();
    let page_token = page.as_ref().map_or("", PageToken::as_str).to_owned();
    match section {
        Section::Configuration => {
            let selection = match selection {
                Some(Selection::Configuration(value)) => value,
                Some(Selection::Account(_) | Selection::Integration(_)) | None => {
                    super::configuration::Selection::Documents
                }
            };
            super::configuration::load(session, connection, lease, selection, page, cancel)
                .await
                .map(|value| Payload::Configuration(Box::new(value)))
        }
        Section::MfaPolicy => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.accounts(),
                get_second_factor_policy,
                auth::GetSecondFactorPolicyRequest {
                    name: format!("{home}/secondFactorPolicy"),
                    ..Default::default()
                }
            );
            auth_contract::admit_second_factor_policy(&response).map_err(|_| ReadError::Invalid)?;
            Ok(Payload::MfaPolicy(response))
        }
        Section::Account => Ok(Payload::Personal { profile: None }),
        Section::Sessions => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.auth(),
                list_sessions,
                auth::ListSessionsRequest {
                    parent: own.clone(),
                    page_size: 100,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            metadata(&response.sessions, |row| {
                auth_contract::admit_session_metadata(row).is_ok()
                    && auth_contract::SessionName::try_from(row.name.as_str())
                        .is_ok_and(|name| &name.account() == lease.account_name())
            })?;
            distinct(response.sessions.iter().map(|row| row.name.as_str()))?;
            Ok(Payload::Sessions(ResourcePage::new(
                response.sessions,
                response.next_page_token,
            )?))
        }
        Section::Factors => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.auth(),
                list_factor_credentials,
                auth::ListFactorCredentialsRequest {
                    parent: own.clone(),
                    page_size: 100,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            metadata(&response.factor_credentials, |row| {
                auth_contract::admit_factor_metadata(row).is_ok()
                    && auth_contract::FactorCredentialName::try_from(row.name.as_str())
                        .is_ok_and(|name| &name.account() == lease.account_name())
            })?;
            distinct(
                response
                    .factor_credentials
                    .iter()
                    .map(|row| row.name.as_str()),
            )?;
            Ok(Payload::Factors(ResourcePage::new(
                response.factor_credentials,
                response.next_page_token,
            )?))
        }
        Section::McpKeys => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.mcp_keys(),
                list_mcp_keys,
                auth::ListMcpKeysRequest {
                    parent: own.clone(),
                    page_size: 100,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            metadata(&response.mcp_keys, |row| {
                auth_contract::admit_mcp_key_metadata(row).is_ok()
                    && auth_contract::McpKeyName::try_from(row.name.as_str())
                        .is_ok_and(|name| &name.account() == lease.account_name())
            })?;
            distinct(response.mcp_keys.iter().map(|row| row.name.as_str()))?;
            Ok(Payload::Keys(ResourcePage::new(
                response.mcp_keys,
                response.next_page_token,
            )?))
        }
        Section::Accounts => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.accounts(),
                list_accounts,
                auth::ListAccountsRequest {
                    parent: home.clone(),
                    page_size: 100,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            Ok(Payload::Accounts(ResourcePage::new(
                administration::admit_accounts(response.clone()).map_err(|_| ReadError::Invalid)?,
                response.next_page_token,
            )?))
        }
        Section::Invitations => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.accounts(),
                list_invitations,
                auth::ListInvitationsRequest {
                    parent: home.clone(),
                    page_size: 100,
                    page_token: page_token.clone(),
                    ..Default::default()
                }
            );
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            metadata(&response.invitations, |row| {
                auth_contract::admit_invitation_metadata(row).is_ok()
            })?;
            distinct(response.invitations.iter().map(|row| row.name.as_str()))?;
            Ok(Payload::Invitations(ResourcePage::new(
                response.invitations,
                response.next_page_token,
            )?))
        }
        Section::Policies => match selection {
            Some(Selection::Account(account)) => {
                let name = format!("{}/authorizationPolicy", account.as_str());
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.policies(),
                    get_authorization_policy,
                    auth::GetAuthorizationPolicyRequest {
                        name: name.clone(),
                        ..Default::default()
                    }
                );
                authorization_contract::admit_get_response(&response)
                    .map_err(|_| ReadError::Invalid)?;
                if response.name != name || response.yaml_source.contains(&0) {
                    return Err(ReadError::Invalid);
                }
                Ok(Payload::Policy { account, response })
            }
            Some(Selection::Integration(_) | Selection::Configuration(_)) | None => {
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.accounts(),
                    list_accounts,
                    auth::ListAccountsRequest {
                        parent: home.clone(),
                        page_size: 100,
                        page_token: page_token.clone(),
                        ..Default::default()
                    }
                );
                Ok(Payload::Accounts(ResourcePage::new(
                    administration::admit_accounts(response.clone())
                        .map_err(|_| ReadError::Invalid)?,
                    response.next_page_token,
                )?))
            }
        },
        Section::Integrations => match selection {
            Some(Selection::Integration(id)) => {
                let instance = call!(
                    session,
                    lease,
                    cancel,
                    connection.integrations(),
                    get_instance,
                    integration::GetIntegrationInstanceRequest {
                        name: id.as_str().into(),
                        ..Default::default()
                    }
                );
                if instance.name != id.as_str() {
                    return Err(ReadError::Invalid);
                }
                administration::admit_integration_instance(&instance)
                    .map_err(|_| ReadError::Invalid)?;
                let kind = (async {
                    if instance.integration_type.is_empty() {
                        return Err(ReadError::Unsupported);
                    }
                    let kind = call!(
                        session,
                        lease,
                        cancel,
                        connection.integrations(),
                        get_type,
                        integration::GetIntegrationTypeRequest {
                            name: instance.integration_type.clone(),
                            ..Default::default()
                        }
                    );
                    self::kind(&kind)?;
                    if kind.name != instance.integration_type {
                        return Err(ReadError::Invalid);
                    }
                    Ok(kind)
                })
                .await;
                let health = (async {
                    let health = call!(
                        session,
                        lease,
                        cancel,
                        connection.observability(),
                        get_integration_health,
                        observation::GetIntegrationHealthRequest {
                            name: format!("{}/integrationHealth", instance.name),
                            ..Default::default()
                        }
                    );
                    if health.name != format!("{}/integrationHealth", instance.name) {
                        return Err(ReadError::Invalid);
                    }
                    observability_contract::admit_integration_health_response(&health)
                        .map_err(|_| ReadError::Invalid)?;
                    Ok(health)
                })
                .await;
                Ok(Payload::Integration(Box::new(IntegrationDetail {
                    instance,
                    kind,
                    health,
                })))
            }
            Some(Selection::Account(_) | Selection::Configuration(_)) | None => {
                let parent =
                    super::configuration::home_name(session, connection, lease, cancel).await?;
                let types = call!(
                    session,
                    lease,
                    cancel,
                    connection.integrations(),
                    list_types,
                    integration::ListIntegrationTypesRequest {
                        parent: parent.clone(),
                        page_size: 64,
                        ..Default::default()
                    }
                );
                if !types.__buffa_unknown_fields.is_empty() || !types.next_page_token.is_empty() {
                    return Err(ReadError::Invalid);
                }
                metadata(&types.integration_types, |row| kind(row).is_ok())?;
                distinct(types.integration_types.iter().map(|row| row.name.as_str()))?;
                distinct(
                    types
                        .integration_types
                        .iter()
                        .map(|row| row.type_key.as_str()),
                )?;
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.integrations(),
                    list_instances,
                    integration::ListIntegrationInstancesRequest {
                        parent: parent.clone(),
                        page_size: 100,
                        ..Default::default()
                    }
                );
                if !response.__buffa_unknown_fields.is_empty() {
                    return Err(ReadError::Invalid);
                }
                metadata(&response.integration_instances, |row| {
                    administration::admit_integration_instance(row).is_ok()
                })?;
                distinct(
                    response
                        .integration_instances
                        .iter()
                        .map(|row| row.name.as_str()),
                )?;
                distinct(
                    response
                        .integration_instances
                        .iter()
                        .map(|row| row.instance_key.as_str()),
                )?;
                Ok(Payload::Integrations(Box::new(IntegrationList {
                    parent,
                    types: types.integration_types,
                    response,
                })))
            }
        },
        Section::Health => {
            let parent =
                super::configuration::home_name(session, connection, lease, cancel).await?;
            let response = call!(
                session,
                lease,
                cancel,
                connection.observability(),
                get_health,
                observation::GetHealthRequest {
                    name: format!("{parent}/health"),
                    ..Default::default()
                }
            );
            if response.name != format!("{parent}/health") {
                return Err(ReadError::Invalid);
            }
            observability_contract::admit_health_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::Health(Box::new(response)))
        }
        Section::Metrics => {
            let parent =
                super::configuration::home_name(session, connection, lease, cancel).await?;
            let response = call!(
                session,
                lease,
                cancel,
                connection.observability(),
                get_metrics,
                observation::GetMetricsRequest {
                    name: format!("{parent}/metrics"),
                    ..Default::default()
                }
            );
            if response.name != format!("{parent}/metrics") {
                return Err(ReadError::Invalid);
            }
            observability_contract::admit_metrics_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::Metrics(Box::new(response)))
        }
        Section::Logs => {
            let parent =
                super::configuration::home_name(session, connection, lease, cancel).await?;
            let response = call!(
                session,
                lease,
                cancel,
                connection.observability(),
                query_runtime_logs,
                observation::QueryRuntimeLogsRequest {
                    parent,
                    page_size: 100,
                    page_token: page.map(|value| value.0).unwrap_or_default(),
                    ..Default::default()
                }
            );
            observability_contract::admit_runtime_logs_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::Logs(Box::new(response)))
        }
        Section::Audits | Section::SecretAudits => {
            let parent =
                super::configuration::home_name(session, connection, lease, cancel).await?;
            let response = call!(
                session,
                lease,
                cancel,
                connection.observability(),
                query_audits,
                observation::QueryAuditsRequest {
                    parent,
                    source: section.audit_source().expect("audit section").into(),
                    page_size: 100,
                    page_token: page.map(|value| value.0).unwrap_or_default(),
                    ..Default::default()
                }
            );
            observability_contract::admit_audits_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::Audits(Box::new(response)))
        }
    }
}
