use super::{CalendarCapture, CalendarReadSnapshot};
use home_client_model::calendars::CalendarId;
use home_client_session::AccessLease;
use home_client_transport::{CalendarTransport, Failure};
use home_wire_contract::{calendars_contract as admit, home::calendars::v1 as wire};
use std::{future::Future, pin::Pin, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;

pub enum CalendarReadEvent {
    Invalidated,
    Published(Arc<CalendarReadSnapshot>),
}
pub enum CalendarReadExit {
    Cancelled,
    Ended,
    Resync,
    TimedOut,
    Contract(admit::AdmissionError),
    Transport(Failure),
}
type Pending<'a> =
    Pin<Box<dyn Future<Output = Result<Arc<CalendarReadSnapshot>, CalendarReadExit>> + Send + 'a>>;
const TIMEOUT: Duration = Duration::from_secs(10);
async fn capture(
    api: &CalendarTransport,
    lease: &AccessLease,
    query: &wire::QueryOccurrencesRequest,
) -> Result<Arc<CalendarReadSnapshot>, CalendarReadExit> {
    let before = api
        .get_calendar(
            wire::GetCalendarRequest {
                id: query.id.clone(),
                ..Default::default()
            },
            lease.token(),
            TIMEOUT,
            &lease.cancellation,
        )
        .await
        .map_err(CalendarReadExit::Transport)?;
    if !before.__buffa_unknown_fields.is_empty() {
        return Err(CalendarReadExit::Contract(admit::AdmissionError::Invalid));
    }
    let mut capture =
        CalendarCapture::new(&before.calendar, query).map_err(CalendarReadExit::Contract)?;
    while let Some(request) = capture.request() {
        let page = api
            .query_occurrences(request.clone(), lease.token(), TIMEOUT, &lease.cancellation)
            .await
            .map_err(CalendarReadExit::Transport)?;
        capture.push(&page).map_err(CalendarReadExit::Contract)?;
    }
    let after = api
        .get_calendar(
            wire::GetCalendarRequest {
                id: query.id.clone(),
                ..Default::default()
            },
            lease.token(),
            TIMEOUT,
            &lease.cancellation,
        )
        .await
        .map_err(CalendarReadExit::Transport)?;
    if !after.__buffa_unknown_fields.is_empty() {
        return Err(CalendarReadExit::Contract(admit::AdmissionError::Invalid));
    }
    capture
        .finish(&after.calendar)
        .map(Arc::new)
        .map_err(CalendarReadExit::Contract)
}
fn pending<'a>(
    api: &'a CalendarTransport,
    lease: &'a AccessLease,
    query: &'a wire::QueryOccurrencesRequest,
) -> Pending<'a> {
    Box::pin(async move {
        match tokio::time::timeout(Duration::from_secs(20), capture(api, lease, query)).await {
            Ok(result) => result,
            Err(_) => Err(CalendarReadExit::TimedOut),
        }
    })
}
/// One lease, one stream and at most one range capture. A queued invalidation
/// wins over a completed range. Resync restarts reads and never sends a mutation.
pub async fn read_calendar_once(
    api: &CalendarTransport,
    lease: &AccessLease,
    query: wire::QueryOccurrencesRequest,
    cancel: &CancellationToken,
    mut publish: impl FnMut(CalendarReadEvent) + Send,
) -> CalendarReadExit {
    let query = match admit::admit_query(&query) {
        Ok(value) => value,
        Err(error) => return CalendarReadExit::Contract(error),
    };
    if !query.wire().page_token.is_empty() {
        return CalendarReadExit::Contract(admit::AdmissionError::Sequence);
    }
    let id: CalendarId = match admit::calendar_id(&query.wire().id) {
        Some(id) => id,
        None => return CalendarReadExit::Contract(admit::AdmissionError::Invalid),
    };
    let opening = api.watch_calendar(
        wire::WatchCalendarRequest {
            id: query.wire().id.clone(),
            ..Default::default()
        },
        lease.token(),
        Duration::from_secs(300),
        &lease.cancellation,
    );
    let mut watch = tokio::select! {biased;
     ()=cancel.cancelled()=>return CalendarReadExit::Cancelled,
     ()=lease.cancellation.cancelled()=>return CalendarReadExit::Cancelled,
     opened=tokio::time::timeout(TIMEOUT,opening)=>match opened {Ok(Ok(watch))=>watch,Ok(Err(error))=>return CalendarReadExit::Transport(error),Err(_)=>return CalendarReadExit::TimedOut},
    };
    let first = tokio::select! {biased;
     ()=cancel.cancelled()=>return CalendarReadExit::Cancelled,
     ()=lease.cancellation.cancelled()=>return CalendarReadExit::Cancelled,
     next=tokio::time::timeout(TIMEOUT,watch.next())=>match next {Ok(result)=>result,Err(_)=>return CalendarReadExit::TimedOut},
    };
    let mut sequence = match first {
        Ok(Some(frame)) => {
            if let Err(error) = admit::admit_watch(&frame, &id, 0) {
                return CalendarReadExit::Contract(error);
            }
            match frame.frame {
                Some(wire::watch_calendar_response::Frame::Ready(_)) => frame.stream_sequence,
                Some(wire::watch_calendar_response::Frame::ResyncRequired(_)) => {
                    return CalendarReadExit::Resync;
                }
                Some(wire::watch_calendar_response::Frame::Changed(_)) | None => {
                    return CalendarReadExit::Contract(admit::AdmissionError::Sequence);
                }
            }
        }
        Ok(None) => return CalendarReadExit::Ended,
        Err(error) => return CalendarReadExit::Transport(error),
    };
    publish(CalendarReadEvent::Invalidated);
    let mut work = Some(pending(api, lease, query.wire()));
    let mut dirty = false;
    loop {
        tokio::select! {biased;
         ()=cancel.cancelled()=>return CalendarReadExit::Cancelled,
         ()=lease.cancellation.cancelled()=>return CalendarReadExit::Cancelled,
         next=watch.next()=>match next {
          Err(error)=>return CalendarReadExit::Transport(error),
          Ok(None)=>return CalendarReadExit::Ended,
          Ok(Some(frame))=>{
           if let Err(error)=admit::admit_watch(&frame,&id,sequence) {return CalendarReadExit::Contract(error)}
           sequence=frame.stream_sequence;
           match frame.frame {
            Some(wire::watch_calendar_response::Frame::Ready(_))|None=>return CalendarReadExit::Contract(admit::AdmissionError::Sequence),
            Some(wire::watch_calendar_response::Frame::Changed(_)|wire::watch_calendar_response::Frame::ResyncRequired(_))=>{
             publish(CalendarReadEvent::Invalidated);
             // Finish the in-flight unary read, discard its image, then read
             // again. Dropping it here can cancel server authority admission
             // and unnecessarily suspend every consumer of this session.
             if work.is_some() {dirty=true;} else {work=Some(pending(api,lease,query.wire()));}
            }
           }
          }
         },
         result=async {work.as_mut().expect("guarded calendar capture").await},if work.is_some()=>{
          work=None;
          if cancel.is_cancelled() || lease.cancellation.is_cancelled() {return CalendarReadExit::Cancelled}
          match result {
           Ok(snapshot)=>{
            if dirty {dirty=false;work=Some(pending(api,lease,query.wire()));}
            else {publish(CalendarReadEvent::Published(snapshot));}
           },
           Err(CalendarReadExit::Contract(admit::AdmissionError::StaleRevision))=>{
            dirty=false;work=Some(pending(api,lease,query.wire()));
           },
           Err(exit)=>return exit,
          }
         }
        }
    }
}
