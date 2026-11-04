//! Current source/session content RPCs, each single attempt on the shared TLS
//! connection. Provider streams belong to an exact caller-bound MediaSession.
use crate::{AccessToken, Failure, services::home::api::v2 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::api::v2 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;
#[derive(Clone)]
pub struct MediaTransport {
    pub(crate) client: service::MediaServiceClient<HttpClient>,
    pub(crate) homes: crate::HomeTransport,
}
macro_rules! unary {($name:ident,$request:ident,$response:ty,$method:ident)=>{
    pub async fn $name(&self,request:wire::$request,token:&AccessToken,timeout:Duration,cancel:&CancellationToken)->Result<$response,Failure>{
        let options=token.options(timeout)?;
        tokio::select!{biased;()=cancel.cancelled()=>Err(Failure::cancelled()),response=self.client.$method(request,options)=>response.map(|value|value.into_owned()).map_err(Failure::from)}
    }
};}
impl MediaTransport {
    pub fn home(&self) -> &crate::HomeTransport {
        &self.homes
    }
    unary!(
        list_media_sources,
        ListMediaSourcesRequest,
        wire::ListMediaSourcesResponse,
        list_media_sources_with_options
    );
    unary!(
        get_media_source,
        GetMediaSourceRequest,
        wire::MediaSource,
        get_media_source_with_options
    );
    unary!(
        create_media_session,
        CreateMediaSessionRequest,
        wire::MediaSession,
        create_media_session_with_options
    );
    unary!(
        get_media_session,
        GetMediaSessionRequest,
        wire::MediaSession,
        get_media_session_with_options
    );
    unary!(
        list_media_sessions,
        ListMediaSessionsRequest,
        wire::ListMediaSessionsResponse,
        list_media_sessions_with_options
    );
    unary!(
        delete_media_session,
        DeleteMediaSessionRequest,
        buffa_types::google::protobuf::Empty,
        delete_media_session_with_options
    );
    unary!(
        read_media_session,
        ReadMediaSessionRequest,
        wire::ReadMediaSessionResponse,
        read_media_session_with_options
    );
    unary!(
        query_media_session_events,
        QueryMediaSessionEventsRequest,
        wire::QueryMediaSessionEventsResponse,
        query_media_session_events_with_options
    );
    pub async fn watch_media_session(
        &self,
        request: wire::WatchMediaSessionRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<MediaSessionWatch, Failure> {
        let options = token.options(timeout)?;
        tokio::select! {biased;()=cancel.cancelled()=>Err(Failure::cancelled()),response=self.client.watch_media_session_with_options(request,options)=>response.map(|inner|MediaSessionWatch{inner:Some(inner),cancel:cancel.child_token()}).map_err(Failure::from)}
    }
    pub async fn stream_media_session(
        &self,
        request: wire::StreamMediaSessionRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<MediaVideoWatch, Failure> {
        let options = token.options(timeout)?;
        tokio::select! {biased;()=cancel.cancelled()=>Err(Failure::cancelled()),response=self.client.stream_media_session_with_options(request,options)=>response.map(|inner|MediaVideoWatch{inner:Some(inner),cancel:cancel.child_token()}).map_err(Failure::from)}
    }
}
pub struct MediaSessionWatch {
    inner: Option<
        ServerStream<
            <HttpClient as ClientTransport>::ResponseBody,
            wire::__buffa::view::WatchMediaSessionResponseView<'static>,
        >,
    >,
    cancel: CancellationToken,
}
impl MediaSessionWatch {
    pub async fn next(&mut self) -> Result<Option<wire::WatchMediaSessionResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! {biased;()=self.cancel.cancelled()=>Err(Failure::cancelled()),response=inner.message()=>response.map(|value|value.map(|frame|frame.to_owned_message())).map_err(Failure::from)};
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}
impl Drop for MediaSessionWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
pub struct MediaVideoWatch {
    inner: Option<
        ServerStream<
            <HttpClient as ClientTransport>::ResponseBody,
            wire::__buffa::view::StreamMediaSessionResponseView<'static>,
        >,
    >,
    cancel: CancellationToken,
}
impl MediaVideoWatch {
    pub async fn next(&mut self) -> Result<Option<wire::StreamMediaSessionResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! {biased;()=self.cancel.cancelled()=>Err(Failure::cancelled()),response=inner.message()=>response.map(|value|value.map(|frame|frame.to_owned_message())).map_err(Failure::from)};
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}
impl Drop for MediaVideoWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
