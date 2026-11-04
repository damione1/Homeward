//! Administrative RPCs share the verified connection and attempt each call once.
//! The host owns permission checks, preconditions, bounded admission and secrets.
use std::time::Duration;

use connectrpc::client::{CallOptions, HttpClient};
use home_wire_contract::{
    google::longrunning,
    home::{api::v2 as auth, api::v2 as integration, api::v2 as observation},
};
use tokio_util::sync::CancellationToken;

use crate::{AccessToken, Failure, services::home};

macro_rules! unary {
    ($name:ident, $method:ident, $request:ty, $response:ty) => {
        pub async fn $name(
            &self,
            request: $request,
            token: &AccessToken,
            timeout: Duration,
            cancel: &CancellationToken,
        ) -> Result<$response, Failure> {
            let options = token.options(timeout)?;
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
    pub(crate) client: home::api::v2::AccountServiceClient<HttpClient>,
}
impl AccountTransport {
    unary!(
        create_account,
        create_account_with_options,
        auth::CreateAccountRequest,
        auth::Account
    );
    unary!(
        get_account,
        get_account_with_options,
        auth::GetAccountRequest,
        auth::Account
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
        auth::Account
    );
    unary!(
        delete_account,
        delete_account_with_options,
        auth::DeleteAccountRequest,
        buffa_types::google::protobuf::Empty
    );
    unary!(
        issue_invitation,
        issue_invitation_with_options,
        auth::IssueInvitationRequest,
        auth::IssueInvitationResponse
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
        auth::SecondFactorPolicy
    );
    unary!(
        change_second_factor_policy,
        change_second_factor_policy_with_options,
        auth::ChangeSecondFactorPolicyRequest,
        auth::ChangeSecondFactorPolicyResponse
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
    pub(crate) client: home::api::v2::AuthorizationPolicyServiceClient<HttpClient>,
}
impl PolicyTransport {
    unary!(
        get_authorization_policy,
        get_authorization_policy_with_options,
        auth::GetAuthorizationPolicyRequest,
        auth::AuthorizationPolicy
    );
    unary!(
        replace_authorization_policy,
        replace_authorization_policy_with_options,
        auth::ReplaceAuthorizationPolicyRequest,
        auth::ReplaceAuthorizationPolicyResponse
    );
}

#[derive(Clone)]
pub struct McpKeyTransport {
    pub(crate) client: home::api::v2::McpKeyServiceClient<HttpClient>,
}
impl McpKeyTransport {
    unary!(
        issue_mcp_key,
        issue_mcp_key_with_options,
        auth::IssueMcpKeyRequest,
        auth::IssueMcpKeyResponse
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
    pub(crate) client: home::api::v2::IntegrationServiceClient<HttpClient>,
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
        integration::IntegrationType
    );
    unary!(
        list_instances,
        list_integration_instances_with_options,
        integration::ListIntegrationInstancesRequest,
        integration::ListIntegrationInstancesResponse
    );
    unary!(
        get_instance,
        get_integration_instance_with_options,
        integration::GetIntegrationInstanceRequest,
        integration::IntegrationInstance
    );
    unary!(
        create_instance,
        create_integration_instance_with_options,
        integration::CreateIntegrationInstanceRequest,
        longrunning::Operation
    );
    unary!(
        update_instance,
        update_integration_instance_with_options,
        integration::UpdateIntegrationInstanceRequest,
        longrunning::Operation
    );
    unary!(
        delete_instance,
        delete_integration_instance_with_options,
        integration::DeleteIntegrationInstanceRequest,
        longrunning::Operation
    );
}

#[derive(Clone)]
pub struct ObservabilityTransport {
    pub(crate) client: home::api::v2::ObservabilityServiceClient<HttpClient>,
}
impl ObservabilityTransport {
    unary!(
        get_health,
        get_health_with_options,
        observation::GetHealthRequest,
        observation::Health
    );
    unary!(
        get_metrics,
        get_metrics_with_options,
        observation::GetMetricsRequest,
        observation::Metrics
    );
    unary!(
        get_integration_health,
        get_integration_health_with_options,
        observation::GetIntegrationHealthRequest,
        observation::IntegrationHealth
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
        query_run_trace,
        query_run_trace_with_options,
        observation::QueryRunTraceRequest,
        observation::QueryRunTraceResponse
    );
    unary!(
        query_recorder_coverage,
        query_recorder_coverage_with_options,
        observation::QueryRecorderCoverageRequest,
        observation::QueryRecorderCoverageResponse
    );
}
