use crate::administration::{self, Authority, Identity, IntegrationId, MAX_ROWS, Section};
use home_client_session::{AccessLease, SessionCoordinator, SessionFailure};
use home_client_transport::{Connection, FailureKind, RpcCode};
use home_wire_contract::{
    auth_contract::{self, AccountId},
    authorization_contract, configuration_contract,
    home::{
        auth::v1 as auth, integrationadmin::v1 as integration, observability::v1 as observation,
    },
    observability_contract,
};
use sha2::{Digest, Sha256};
use std::{collections::BTreeSet, time::Duration};
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(super) enum Selection {
    Account(AccountId),
    Integration(IntegrationId),
}
#[derive(Clone)]
pub(super) struct PageToken(String);
impl PageToken {
    pub fn parse(value: &str) -> Result<Self, ReadError> {
        if value.is_empty() || value.len() > 512 || value.contains(['\0', '\r', '\n']) {
            return Err(ReadError::Invalid);
        }
        Ok(Self(value.into()))
    }
}
#[derive(Clone)]
pub(super) enum Payload {
    Personal {
        profile: Option<auth::Account>,
    },
    Sessions(Vec<auth::Session>),
    Factors(Vec<auth::FactorCredential>),
    MfaPolicy(auth::SecondFactorPolicy),
    Keys(Vec<auth::McpKey>),
    Accounts(Vec<auth::Account>),
    Invitations(Vec<auth::Invitation>),
    Policy {
        account: AccountId,
        response: auth::GetPolicyResponse,
    },
    Integrations(Box<IntegrationList>),
    Integration(Box<IntegrationDetail>),
    Health(Box<observation::GetHealthResponse>),
    Metrics(Box<observation::GetMetricsResponse>),
    Logs(Box<observation::QueryRuntimeLogsResponse>),
    Audits(Box<observation::QueryAuditsResponse>),
}
#[derive(Clone)]
pub(super) struct IntegrationList {
    pub types: Vec<integration::IntegrationType>,
    pub response: integration::ListInstancesResponse,
}
#[derive(Clone)]
pub(super) struct IntegrationDetail {
    pub response: integration::GetInstanceResponse,
    pub kind: Result<integration::IntegrationType, ReadError>,
    pub health: Result<observation::GetIntegrationHealthResponse, ReadError>,
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
const TIMEOUT: Duration = Duration::from_secs(4);

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
    if [lease.account_id().as_str(), lease.session_id().as_str()]
        .iter()
        .any(|value| value.len() > 128 || value.contains(['\0', '\r', '\n']))
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
        account: lease.account_id().clone(),
        session: lease.session_id().clone(),
    };
    let api = connection.accounts();
    let profile = tokio::select! {biased;
        ()=cancel.cancelled()=>return Err(ReadError::Cancelled),
        ()=lease.cancellation.cancelled()=>return Err(ReadError::Cancelled),
        result=api.get_account(auth::GetAccountRequest{account_id:auth::AccountId{value:identity.account.as_str().into(),..Default::default()}.into(),..Default::default()},lease.token(),TIMEOUT,cancel)=>result,
    };
    let authority = match profile {
        Ok(response) => {
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            Authority::admit(
                identity,
                response.account.into_option().ok_or(ReadError::Invalid)?,
            )
            .map_err(|_| ReadError::Invalid)?
        }
        Err(error) => {
            if !matches!(error.authorization_termination(), Ok(None))
                || error.kind() == FailureKind::Rpc(RpcCode::Unauthenticated)
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
        || IntegrationId::parse(&value.type_id).is_err()
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
    let own = auth::AccountId {
        value: lease.account_id().as_str().into(),
        ..Default::default()
    };
    match section {
        Section::MfaPolicy => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.accounts(),
                get_second_factor_policy,
                auth::GetSecondFactorPolicyRequest::default()
            );
            auth_contract::admit_get_second_factor_policy_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::MfaPolicy(
                response.policy.into_option().ok_or(ReadError::Invalid)?,
            ))
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
                    account_id: own.into(),
                    ..Default::default()
                }
            );
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            metadata(&response.sessions, |row| {
                auth_contract::admit_session_metadata(row).is_ok()
                    && row.account_id.value == lease.account_id().as_str()
            })?;
            distinct(response.sessions.iter().map(|row| row.id.value.as_str()))?;
            Ok(Payload::Sessions(response.sessions))
        }
        Section::Factors => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.auth(),
                list_factors,
                auth::ListFactorsRequest {
                    account_id: own.into(),
                    ..Default::default()
                }
            );
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            metadata(&response.factors, |row| {
                auth_contract::admit_factor_metadata(row).is_ok()
                    && row.account_id.value == lease.account_id().as_str()
            })?;
            distinct(response.factors.iter().map(|row| row.id.value.as_str()))?;
            Ok(Payload::Factors(response.factors))
        }
        Section::McpKeys => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.mcp_keys(),
                list_mcp_keys,
                auth::ListMcpKeysRequest {
                    account_id: own.into(),
                    ..Default::default()
                }
            );
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            metadata(&response.keys, |row| {
                auth_contract::admit_mcp_key_metadata(row).is_ok()
                    && row.account_id.value == lease.account_id().as_str()
            })?;
            distinct(response.keys.iter().map(|row| row.id.value.as_str()))?;
            Ok(Payload::Keys(response.keys))
        }
        Section::Accounts => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.accounts(),
                list_accounts,
                auth::ListAccountsRequest::default()
            );
            Ok(Payload::Accounts(
                administration::admit_accounts(response).map_err(|_| ReadError::Invalid)?,
            ))
        }
        Section::Invitations => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.accounts(),
                list_invitations,
                auth::ListInvitationsRequest::default()
            );
            if !response.__buffa_unknown_fields.is_empty() {
                return Err(ReadError::Invalid);
            }
            metadata(&response.invitations, |row| {
                auth_contract::admit_invitation_metadata(row).is_ok()
            })?;
            distinct(response.invitations.iter().map(|row| row.id.value.as_str()))?;
            Ok(Payload::Invitations(response.invitations))
        }
        Section::Policies => match selection {
            Some(Selection::Account(account)) => {
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.policies(),
                    get_policy,
                    auth::GetPolicyRequest {
                        account_id: auth::AccountId {
                            value: account.as_str().into(),
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    }
                );
                authorization_contract::admit_get_response(&response)
                    .map_err(|_| ReadError::Invalid)?;
                if let Some(auth::get_policy_response::Result::Present(value)) = &response.result
                    && (value.account_id.value != account.as_str()
                        || value.yaml_source.contains(&0)
                        || value.version.source_sha256.as_slice()
                            != &Sha256::digest(&value.yaml_source)[..])
                {
                    return Err(ReadError::Invalid);
                }
                Ok(Payload::Policy { account, response })
            }
            Some(Selection::Integration(_)) | None => {
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.accounts(),
                    list_accounts,
                    auth::ListAccountsRequest::default()
                );
                Ok(Payload::Accounts(
                    administration::admit_accounts(response).map_err(|_| ReadError::Invalid)?,
                ))
            }
        },
        Section::Integrations => match selection {
            Some(Selection::Integration(id)) => {
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.integrations(),
                    get_instance,
                    integration::GetInstanceRequest {
                        integration_instance_id: id.as_str().into(),
                        ..Default::default()
                    }
                );
                if !response.__buffa_unknown_fields.is_empty()
                    || response.instance.integration_instance_id != id.as_str()
                {
                    return Err(ReadError::Invalid);
                }
                administration::admit_integration_instance(
                    response.instance.as_option().ok_or(ReadError::Invalid)?,
                )
                .map_err(|_| ReadError::Invalid)?;
                configuration_contract::admit_configuration_read(
                    response.document.as_option().ok_or(ReadError::Invalid)?,
                )
                .map_err(|_| ReadError::Invalid)?;
                let kind = (async {
                    if response.instance.type_id.is_empty() {
                        return Err(ReadError::Unsupported);
                    }
                    let kind_response = call!(
                        session,
                        lease,
                        cancel,
                        connection.integrations(),
                        get_type,
                        integration::GetIntegrationTypeRequest {
                            type_id: response.instance.type_id.clone(),
                            ..Default::default()
                        }
                    );
                    if !kind_response.__buffa_unknown_fields.is_empty() {
                        return Err(ReadError::Invalid);
                    }
                    let kind = kind_response
                        .r#type
                        .into_option()
                        .ok_or(ReadError::Invalid)?;
                    self::kind(&kind)?;
                    if kind.type_id != response.instance.type_id {
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
                            integration_instance_id: id.as_str().into(),
                            ..Default::default()
                        }
                    );
                    observability_contract::admit_integration_health_response(&health)
                        .map_err(|_| ReadError::Invalid)?;
                    Ok(health)
                })
                .await;
                Ok(Payload::Integration(Box::new(IntegrationDetail {
                    response,
                    kind,
                    health,
                })))
            }
            Some(Selection::Account(_)) | None => {
                let types = call!(
                    session,
                    lease,
                    cancel,
                    connection.integrations(),
                    list_types,
                    integration::ListIntegrationTypesRequest::default()
                );
                if !types.__buffa_unknown_fields.is_empty() {
                    return Err(ReadError::Invalid);
                }
                metadata(&types.types, |row| kind(row).is_ok())?;
                distinct(types.types.iter().map(|row| row.type_id.as_str()))?;
                let response = call!(
                    session,
                    lease,
                    cancel,
                    connection.integrations(),
                    list_instances,
                    integration::ListInstancesRequest::default()
                );
                if !response.__buffa_unknown_fields.is_empty() {
                    return Err(ReadError::Invalid);
                }
                metadata(&response.instances, |row| {
                    administration::admit_integration_instance(row).is_ok()
                })?;
                distinct(
                    response
                        .instances
                        .iter()
                        .map(|row| row.integration_instance_id.as_str()),
                )?;
                configuration_contract::admit_configuration_read(
                    response.document.as_option().ok_or(ReadError::Invalid)?,
                )
                .map_err(|_| ReadError::Invalid)?;
                Ok(Payload::Integrations(Box::new(IntegrationList {
                    types: types.types,
                    response,
                })))
            }
        },
        Section::Health => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.observability(),
                get_health,
                observation::GetHealthRequest::default()
            );
            observability_contract::admit_health_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::Health(Box::new(response)))
        }
        Section::Metrics => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.observability(),
                get_metrics,
                observation::GetMetricsRequest::default()
            );
            observability_contract::admit_metrics_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::Metrics(Box::new(response)))
        }
        Section::Logs => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.observability(),
                query_runtime_logs,
                observation::QueryRuntimeLogsRequest {
                    page_size: Some(100),
                    page_token: page.map(|value| value.0),
                    ..Default::default()
                }
            );
            observability_contract::admit_runtime_logs_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::Logs(Box::new(response)))
        }
        Section::Audits | Section::SecretAudits => {
            let response = call!(
                session,
                lease,
                cancel,
                connection.observability(),
                query_audits,
                observation::QueryAuditsRequest {
                    source: section.audit_source().expect("audit section").into(),
                    page_size: Some(100),
                    page_token: page.map(|value| value.0),
                    ..Default::default()
                }
            );
            observability_contract::admit_audits_response(&response)
                .map_err(|_| ReadError::Invalid)?;
            Ok(Payload::Audits(Box::new(response)))
        }
    }
}
