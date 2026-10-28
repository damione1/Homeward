//! Configuration calls use the verified TLS pool and make one attempt only.
//! Admission, source ownership, CAS and user confirmation belong to the host.
use std::time::Duration;

use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::{
    configuration::v1 as document, configurationapply::v1 as activation,
    configurationplan::v1 as plan, configurationsimulation::v1 as simulation,
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
pub struct ConfigurationTransport {
    pub(crate) client: home::configuration::v1::ConfigurationServiceClient<HttpClient>,
}
impl ConfigurationTransport {
    unary!(
        read_document,
        read_document_with_options,
        document::ReadDocumentRequest,
        document::ReadDocumentResponse
    );
    unary!(
        list_documents,
        list_documents_with_options,
        document::ListDocumentsRequest,
        document::ListDocumentsResponse
    );
    unary!(
        list_versions,
        list_versions_with_options,
        document::ListVersionsRequest,
        document::ListVersionsResponse
    );
    unary!(
        list_generations,
        list_generations_with_options,
        document::ListGenerationsRequest,
        document::ListGenerationsResponse
    );
    unary!(
        list_schemas,
        list_schemas_with_options,
        document::ListSchemasRequest,
        document::ListSchemasResponse
    );
    unary!(
        write_document,
        write_document_with_options,
        document::WriteDocumentRequest,
        document::WriteDocumentResponse
    );
    unary!(
        restore_document,
        restore_document_with_options,
        document::RestoreDocumentRequest,
        document::RestoreDocumentResponse
    );

    pub async fn watch(
        &self,
        request: document::WatchConfigurationRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<ConfigurationWatch, Failure> {
        let options = StateTransport::options(token, timeout)?;
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
    pub(crate) client: home::configurationplan::v1::ConfigurationPlanServiceClient<HttpClient>,
}
impl ConfigurationPlanTransport {
    unary!(
        build_plan,
        build_plan_with_options,
        plan::BuildPlanRequest,
        plan::BuildPlanResponse
    );
    unary!(
        revalidate_plan,
        revalidate_plan_with_options,
        plan::RevalidatePlanRequest,
        plan::RevalidatePlanResponse
    );
}

#[derive(Clone)]
pub struct ConfigurationSimulationTransport {
    pub(crate) client:
        home::configurationsimulation::v1::ConfigurationSimulationServiceClient<HttpClient>,
}
impl ConfigurationSimulationTransport {
    unary!(
        simulate_plan,
        simulate_plan_with_options,
        simulation::SimulatePlanRequest,
        simulation::SimulatePlanResponse
    );
}

#[derive(Clone)]
pub struct ConfigurationApplyTransport {
    pub(crate) client: home::configurationapply::v1::ConfigurationApplyServiceClient<HttpClient>,
}
impl ConfigurationApplyTransport {
    unary!(
        apply,
        apply_with_options,
        activation::ApplyRequest,
        activation::ApplyResponse
    );
    unary!(
        rollback,
        rollback_with_options,
        activation::RollbackRequest,
        activation::RollbackResponse
    );
}
