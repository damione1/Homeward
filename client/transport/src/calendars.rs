//! Single-attempt CalendarService calls on the shared verified TLS connection.
//! The host owns revisions, admission, drafts and explicit conflict resolution.
use crate::{AccessToken, Failure, services::home::api::v2 as service};
use connectrpc::client::{ClientTransport, HttpClient, ServerStream};
use home_wire_contract::home::api::v2 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub struct CalendarTransport {
    pub(crate) homes: crate::HomeTransport,
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
        ) -> Result<responses::$response, Failure> {
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
mod responses {
    pub use buffa_types::google::protobuf::Empty;
    pub use home_wire_contract::home::api::v2::*;
}
impl CalendarTransport {
    pub fn home(&self) -> &crate::HomeTransport {
        &self.homes
    }
    unary!(
        query_calendar_sources,
        query_calendar_sources_with_options,
        QueryCalendarSourcesRequest,
        QueryCalendarSourcesResponse
    );
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
        Calendar
    );
    unary!(
        create_calendar,
        create_calendar_with_options,
        CreateCalendarRequest,
        Calendar
    );
    unary!(
        update_calendar,
        update_calendar_with_options,
        UpdateCalendarRequest,
        Calendar
    );
    unary!(
        delete_calendar,
        delete_calendar_with_options,
        DeleteCalendarRequest,
        Empty
    );
    unary!(
        list_calendar_events,
        list_calendar_events_with_options,
        ListCalendarEventsRequest,
        ListCalendarEventsResponse
    );
    unary!(
        get_calendar_event,
        get_calendar_event_with_options,
        GetCalendarEventRequest,
        CalendarEvent
    );
    unary!(
        create_calendar_event,
        create_calendar_event_with_options,
        CreateCalendarEventRequest,
        CalendarEvent
    );
    unary!(
        update_calendar_event,
        update_calendar_event_with_options,
        UpdateCalendarEventRequest,
        CalendarEvent
    );
    unary!(
        delete_calendar_event,
        delete_calendar_event_with_options,
        DeleteCalendarEventRequest,
        Empty
    );
    unary!(
        query_occurrences,
        query_occurrences_with_options,
        QueryOccurrencesRequest,
        QueryOccurrencesResponse
    );
    unary!(
        resolve_calendar_time,
        resolve_calendar_time_with_options,
        ResolveCalendarTimeRequest,
        ResolveCalendarTimeResponse
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
        let options = token.options(timeout)?;
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
