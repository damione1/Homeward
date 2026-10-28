//! Administrative RPCs share the verified connection and attempt each call once.
//! The host owns permission checks, preconditions, bounded admission and secrets.
use std::time::Duration;

use connectrpc::client::{CallOptions, HttpClient};
use home_wire_contract::home::{
    auth::v1 as auth, integrationadmin::v1 as integration, observability::v1 as observation,
};
use tokio_util::sync::CancellationToken;

use crate::{AccessToken, Failure, StateTransport, services::home};

macro_rules! unary {
    ($name:ident, $method:ident, $request:ty, $response:ty) => {
        pub async fn $name(
            &self,
            request: $request,
            token: &AccessToken,
            timeout: Duration,
            cancel: &CancellationToken,
        ) -> Result<$response, Failure> {
            let options = StateTransport::options(token, timeout)?;
            tokio::select! { biased;
                () = cancel.cancelled() => Err(Failure::cancelled()),
                response = self.client.$method(request, options) => {
                    response.map(|value| value.into_owned()).map_err(Failure::from)
                }
            }
        }
    };
}

#[derive(Clone)]
pub struct AccountTransport {
    pub(crate) client: home::auth::v1::AccountServiceClient<HttpClient>,
}
impl AccountTransport {
    unary!(
        create_account,
        create_account_with_options,
        auth::CreateAccountRequest,
        auth::CreateAccountResponse
    );
    unary!(
        get_account,
        get_account_with_options,
        auth::GetAccountRequest,
        auth::GetAccountResponse
    );
    unary!(
        list_accounts,
        list_accounts_with_options,
        auth::ListAccountsRequest,
        auth::ListAccountsResponse
    );
    unary!(
        update_account,
        update_account_with_options,
        auth::UpdateAccountRequest,
        auth::UpdateAccountResponse
    );
    unary!(
        delete_account,
        delete_account_with_options,
        auth::DeleteAccountRequest,
        auth::DeleteAccountResponse
    );
    unary!(
        create_invitation,
        create_invitation_with_options,
        auth::CreateInvitationRequest,
        auth::CreateInvitationResponse
    );
    unary!(
        list_invitations,
        list_invitations_with_options,
        auth::ListInvitationsRequest,
        auth::ListInvitationsResponse
    );
    unary!(
        revoke_invitation,
        revoke_invitation_with_options,
        auth::RevokeInvitationRequest,
        auth::RevokeInvitationResponse
    );
    unary!(
        get_second_factor_policy,
        get_second_factor_policy_with_options,
        auth::GetSecondFactorPolicyRequest,
        auth::GetSecondFactorPolicyResponse
    );
    unary!(
        set_second_factor_policy,
        set_second_factor_policy_with_options,
        auth::SetSecondFactorPolicyRequest,
        auth::SetSecondFactorPolicyResponse
    );

    /// Acceptance authenticates with the invitation, before a native session exists.
    /// Its token/password and the one-time invitation response must remain private.
    pub async fn accept_invitation(
        &self,
        request: auth::AcceptInvitationRequest,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<auth::AcceptInvitationResponse, Failure> {
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.accept_invitation_with_options(
                request, CallOptions::default().with_timeout(timeout)
            ) => response.map(|value| value.into_owned()).map_err(Failure::from),
        }
    }
}

#[derive(Clone)]
pub struct PolicyTransport {
    pub(crate) client: home::auth::v1::AuthorizationPolicyServiceClient<HttpClient>,
}
impl PolicyTransport {
    unary!(
        get_policy,
        get_policy_with_options,
        auth::GetPolicyRequest,
        auth::GetPolicyResponse
    );
    unary!(
        replace_policy,
        replace_policy_with_options,
        auth::ReplacePolicyRequest,
        auth::ReplacePolicyResponse
    );
}

#[derive(Clone)]
pub struct McpKeyTransport {
    pub(crate) client: home::auth::v1::McpKeyServiceClient<HttpClient>,
}
impl McpKeyTransport {
    unary!(
        create_mcp_key,
        create_mcp_key_with_options,
        auth::CreateMcpKeyRequest,
        auth::CreateMcpKeyResponse
    );
    unary!(
        list_mcp_keys,
        list_mcp_keys_with_options,
        auth::ListMcpKeysRequest,
        auth::ListMcpKeysResponse
    );
    unary!(
        revoke_mcp_key,
        revoke_mcp_key_with_options,
        auth::RevokeMcpKeyRequest,
        auth::RevokeMcpKeyResponse
    );
}

#[derive(Clone)]
pub struct IntegrationTransport {
    pub(crate) client: home::integrationadmin::v1::IntegrationServiceClient<HttpClient>,
}
impl IntegrationTransport {
    unary!(
        list_types,
        list_integration_types_with_options,
        integration::ListIntegrationTypesRequest,
        integration::ListIntegrationTypesResponse
    );
    unary!(
        get_type,
        get_integration_type_with_options,
        integration::GetIntegrationTypeRequest,
        integration::GetIntegrationTypeResponse
    );
    unary!(
        list_instances,
        list_instances_with_options,
        integration::ListInstancesRequest,
        integration::ListInstancesResponse
    );
    unary!(
        get_instance,
        get_instance_with_options,
        integration::GetInstanceRequest,
        integration::GetInstanceResponse
    );
    unary!(
        create_instance,
        create_instance_with_options,
        integration::CreateInstanceRequest,
        integration::CreateInstanceResponse
    );
    unary!(
        update_instance,
        update_instance_with_options,
        integration::UpdateInstanceRequest,
        integration::UpdateInstanceResponse
    );
    unary!(
        delete_instance,
        delete_instance_with_options,
        integration::DeleteInstanceRequest,
        integration::DeleteInstanceResponse
    );
    unary!(
        set_instance_enabled,
        set_instance_enabled_with_options,
        integration::SetInstanceEnabledRequest,
        integration::SetInstanceEnabledResponse
    );
}

#[derive(Clone)]
pub struct ObservabilityTransport {
    pub(crate) client: home::observability::v1::ObservabilityServiceClient<HttpClient>,
}
impl ObservabilityTransport {
    unary!(
        get_health,
        get_health_with_options,
        observation::GetHealthRequest,
        observation::GetHealthResponse
    );
    unary!(
        get_metrics,
        get_metrics_with_options,
        observation::GetMetricsRequest,
        observation::GetMetricsResponse
    );
    unary!(
        get_integration_health,
        get_integration_health_with_options,
        observation::GetIntegrationHealthRequest,
        observation::GetIntegrationHealthResponse
    );
    unary!(
        query_runtime_logs,
        query_runtime_logs_with_options,
        observation::QueryRuntimeLogsRequest,
        observation::QueryRuntimeLogsResponse
    );
    unary!(
        query_audits,
        query_audits_with_options,
        observation::QueryAuditsRequest,
        observation::QueryAuditsResponse
    );
    unary!(
        read_run_trace,
        read_run_trace_with_options,
        observation::ReadRunTraceRequest,
        observation::ReadRunTraceResponse
    );
}
