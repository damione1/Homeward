//! Single-attempt CalendarService calls on the shared verified TLS connection.
//! The host owns revisions, admission, drafts and explicit conflict resolution.
use crate::{AccessToken, Failure, StateTransport, services::home::calendars::v1 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::calendars::v1 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct CalendarTransport {
    pub(crate) client: service::CalendarServiceClient<HttpClient>,
}

macro_rules! unary {
    ($name:ident, $method:ident, $request:ident, $response:ident) => {
        pub async fn $name(
            &self,
            request: wire::$request,
            token: &AccessToken,
            timeout: Duration,
            cancel: &CancellationToken,
        ) -> Result<wire::$response, Failure> {
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
impl CalendarTransport {
    unary!(
        list_calendars,
        list_calendars_with_options,
        ListCalendarsRequest,
        ListCalendarsResponse
    );
    unary!(
        get_calendar,
        get_calendar_with_options,
        GetCalendarRequest,
        GetCalendarResponse
    );
    unary!(
        mutate_calendar,
        mutate_calendar_with_options,
        MutateCalendarRequest,
        MutateCalendarResponse
    );
    unary!(
        get_event,
        get_event_with_options,
        GetEventRequest,
        GetEventResponse
    );
    unary!(
        mutate_event,
        mutate_event_with_options,
        MutateEventRequest,
        MutateEventResponse
    );
    unary!(
        query_occurrences,
        query_occurrences_with_options,
        QueryOccurrencesRequest,
        QueryOccurrencesResponse
    );
    unary!(
        resolve_local_time,
        resolve_local_time_with_options,
        ResolveLocalTimeRequest,
        ResolveLocalTimeResponse
    );

    /// Ready establishes the subscription; the host then reloads the page
    /// while keeping this stream alive. The protocol has no replay cursor.
    pub async fn watch_calendar(
        &self,
        request: wire::WatchCalendarRequest,
        token: &AccessToken,
        timeout: Duration,
        cancel: &CancellationToken,
    ) -> Result<CalendarWatch, Failure> {
        let options = StateTransport::options(token, timeout)?;
        tokio::select! { biased;
            () = cancel.cancelled() => Err(Failure::cancelled()),
            response = self.client.watch_calendar_with_options(request, options) => {
                response.map(|inner| CalendarWatch { inner: Some(inner), cancel: cancel.child_token() }).map_err(Failure::from)
            }
        }
    }
}

type WireStream = ServerStream<
    <HttpClient as ClientTransport>::ResponseBody,
    wire::__buffa::view::WatchCalendarResponseView<'static>,
>;
pub struct CalendarWatch {
    inner: Option<WireStream>,
    cancel: CancellationToken,
}
impl CalendarWatch {
    pub async fn next(&mut self) -> Result<Option<wire::WatchCalendarResponse>, Failure> {
        let Some(inner) = self.inner.as_mut() else {
            return Ok(None);
        };
        let result = tokio::select! { biased;
            () = self.cancel.cancelled() => Err(Failure::cancelled()),
            response = inner.message() => response.map(|value|value.map(|frame|frame.to_owned_message())).map_err(Failure::from),
        };
        if !matches!(result, Ok(Some(_))) {
            self.inner = None;
        }
        result
    }
}
impl Drop for CalendarWatch {
    fn drop(&mut self) {
        self.cancel.cancel();
    }
}
