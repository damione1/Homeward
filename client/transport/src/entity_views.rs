//! Single-attempt bounded captures on the shared verified TLS connection.
use crate::{AccessToken, Failure, services::home::api::v2 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::api::v2 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct EntityViewTransport {
    pub(crate) client: service::EntityViewServiceClient<HttpClient>,
}
macro_rules! unary {
 ($name:ident,$method:ident,$request:ident,$response:ident)=>{
  pub async fn $name(&self,request:wire::$request,token:&AccessToken,timeout:Duration,cancel:&CancellationToken)->Result<wire::$response,Failure>{
   let options=token.options(timeout)?;
   tokio::select!{biased;
    ()=cancel.cancelled()=>Err(Failure::cancelled()),
    result=self.client.$method(request,options)=>result.map(|value|value.into_owned()).map_err(Failure::from),
   }
  }
 };
}
impl EntityViewTransport {
    unary!(
        create_entity_view,
        create_entity_view_with_options,
        CreateEntityViewRequest,
        EntityView
    );
    unary!(
        get_entity_view,
        get_entity_view_with_options,
        GetEntityViewRequest,
        EntityView
    );
    unary!(
        list_entity_views,
        list_entity_views_with_options,
        ListEntityViewsRequest,
        ListEntityViewsResponse
    );
    unary!(
        query_entity_view,
        query_entity_view_with_options,
        QueryEntityViewRequest,
        QueryEntityViewResponse
    );
    pub async fn delete_entity_view(
        &self,
        request: wire::DeleteEntityViewRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<(), Failure> {
        let options = token.options(timeout)?;
        tokio::select! {biased;
         ()=cancel.cancelled()=>Err(Failure::cancelled()),
         result=self.client.delete_entity_view_with_options(request,options)=>result.map(|_|()).map_err(Failure::from),
        }
    }
    pub async fn watch_entity_view(
        &self,
        request: wire::WatchEntityViewRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<EntityViewWatch, Failure> {
        let options = token.options(timeout)?;
        tokio::select! {biased;
         ()=cancel.cancelled()=>Err(Failure::cancelled()),
         result=self.client.watch_entity_view_with_options(request,options)=>result.map(|inner|EntityViewWatch{inner:Some(inner),cancel:cancel.child_token()}).map_err(Failure::from),
        }
    }
}
type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    wire::__buffa::view::WatchEntityViewResponseView<'static>,
>;
pub struct EntityViewWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}
impl EntityViewWatch {
    pub async fn next(&mut self) -> Result<Option<wire::WatchEntityViewResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! {biased;
         ()=self.cancel.cancelled()=>Err(Failure::cancelled()),
         result=inner.message()=>result.map(|value|value.map(|message|message.to_owned_message())).map_err(Failure::from),
        };
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}
impl Drop for EntityViewWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
