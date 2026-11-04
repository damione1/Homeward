//! Configuration calls use the verified TLS pool and make one attempt only.
//! Admission, source ownership, CAS and user confirmation belong to the host.
use std::time::Duration;

use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::{
    api::v2 as document, api::v2 as activation, api::v2 as plan, api::v2 as simulation,
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
pub struct ConfigurationTransport {
    pub(crate) client: home::api::v2::ConfigurationServiceClient<HttpClient>,
}
impl ConfigurationTransport {
    unary!(
        read_document,
        get_configuration_document_with_options,
        document::GetConfigurationDocumentRequest,
        document::ConfigurationDocument
    );
    unary!(
        list_documents,
        list_configuration_documents_with_options,
        document::ListConfigurationDocumentsRequest,
        document::ListConfigurationDocumentsResponse
    );
    unary!(
        list_versions,
        list_configuration_revisions_with_options,
        document::ListConfigurationRevisionsRequest,
        document::ListConfigurationRevisionsResponse
    );
    unary!(
        list_generations,
        list_configuration_generations_with_options,
        document::ListConfigurationGenerationsRequest,
        document::ListConfigurationGenerationsResponse
    );
    unary!(
        list_schemas,
        list_configuration_schemas_with_options,
        document::ListConfigurationSchemasRequest,
        document::ListConfigurationSchemasResponse
    );
    unary!(
        write_document,
        publish_configuration_document_with_options,
        document::PublishConfigurationDocumentRequest,
        home_wire_contract::google::longrunning::Operation
    );
    unary!(
        restore_document,
        rollback_configuration_document_with_options,
        document::RollbackConfigurationDocumentRequest,
        home_wire_contract::google::longrunning::Operation
    );

    pub async fn watch(
        &self,
        request: document::WatchConfigurationRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<ConfigurationWatch, Failure> {
        let options = token.options(timeout)?;
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.watch_configuration_with_options(request, options) => {
                response.map(|inner| ConfigurationWatch {
                    inner: Some(inner), cancel: cancel.child_token(),
                }).map_err(Failure::from)
            }
        }
    }
}

type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    document::__buffa::view::WatchConfigurationResponseView<'static>,
>;

pub struct ConfigurationWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}
impl ConfigurationWatch {
    pub async fn next(&mut self) -> Result<Option<document::WatchConfigurationResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! { biased;
            () = self.cancel.cancelled() => Err(Failure::cancelled()),
            response = inner.message() => response
                .map(|value| value.map(|message| message.to_owned_message()))
                .map_err(Failure::from),
        };
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}
impl Drop for ConfigurationWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}

#[derive(Clone)]
pub struct ConfigurationPlanTransport {
    pub(crate) client: home::api::v2::ConfigurationPlanServiceClient<HttpClient>,
}
impl ConfigurationPlanTransport {
    unary!(
        get_plan,
        get_configuration_plan_with_options,
        plan::GetConfigurationPlanRequest,
        plan::ConfigurationPlan
    );
    unary!(
        list_plans,
        list_configuration_plans_with_options,
        plan::ListConfigurationPlansRequest,
        plan::ListConfigurationPlansResponse
    );
    unary!(
        build_plan,
        create_configuration_plan_with_options,
        plan::CreateConfigurationPlanRequest,
        plan::ConfigurationPlan
    );
    unary!(
        revalidate_plan,
        revalidate_configuration_plan_with_options,
        plan::RevalidateConfigurationPlanRequest,
        plan::ConfigurationPlan
    );
}

#[derive(Clone)]
pub struct ConfigurationSimulationTransport {
    pub(crate) client: home::api::v2::ConfigurationSimulationServiceClient<HttpClient>,
}
impl ConfigurationSimulationTransport {
    unary!(
        simulate_plan,
        simulate_configuration_plan_with_options,
        simulation::SimulateConfigurationPlanRequest,
        simulation::SimulateConfigurationPlanResponse
    );
}

#[derive(Clone)]
pub struct ConfigurationApplyTransport {
    pub(crate) client: home::api::v2::ConfigurationApplyServiceClient<HttpClient>,
}
impl ConfigurationApplyTransport {
    unary!(
        apply,
        apply_configuration_plan_with_options,
        activation::ApplyConfigurationPlanRequest,
        home_wire_contract::google::longrunning::Operation
    );
    unary!(
        rollback,
        rollback_configuration_with_options,
        activation::RollbackConfigurationRequest,
        home_wire_contract::google::longrunning::Operation
    );
}
