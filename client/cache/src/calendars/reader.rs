use super::{CalendarCapture, CalendarReadSnapshot};

use home_client_session::AccessLease;
use home_client_transport::{CalendarTransport, Failure, FailureKind, RpcCode};
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};
use std::{future::Future, pin::Pin, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;

pub enum CalendarReadEvent {
    Invalidated,
    NotMaterialized(home_client_model::api_calendars::CalendarName),
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
                name: admit::query_parent(&query.parent)
                    .map_err(CalendarReadExit::Contract)?
                    .0
                    .as_str()
                    .into(),
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
    let mut capture = CalendarCapture::new(&before, query).map_err(CalendarReadExit::Contract)?;
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
                name: admit::query_parent(&query.parent)
                    .map_err(CalendarReadExit::Contract)?
                    .0
                    .as_str()
                    .into(),
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
        .finish(&after)
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
    let id = match admit::query_parent(&query.wire().parent) {
        Ok((id, _)) => id,
        Err(error) => return CalendarReadExit::Contract(error),
    };
    let opening = api.watch_calendar(
        wire::WatchCalendarRequest {
            name: id.as_str().into(),
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
    let mut sequence: u64 = match first {
        Ok(Some(frame)) => match admit::admit_watch(&frame) {
            Ok((1, admit::WatchFrame::Ready)) => 1,
            Ok((_, admit::WatchFrame::Resync)) => return CalendarReadExit::Resync,
            Ok(_) => return CalendarReadExit::Contract(admit::AdmissionError::Sequence),
            Err(error) => return CalendarReadExit::Contract(error),
        },
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
           let (next,kind)=match admit::admit_watch(&frame) { Ok(value)=>value,Err(error)=>return CalendarReadExit::Contract(error) };
           if sequence.checked_add(1)!=Some(next){return CalendarReadExit::Contract(admit::AdmissionError::Sequence)}
           sequence=next;
           match kind {
            admit::WatchFrame::Ready=>return CalendarReadExit::Contract(admit::AdmissionError::Sequence),
            admit::WatchFrame::Resync=>return CalendarReadExit::Resync,
            admit::WatchFrame::Changed(_)=>{
             publish(CalendarReadEvent::Invalidated);
             // Let an in-flight unary read finish; its result is discarded.
             if work.is_some(){dirty=true;}else{work=Some(pending(api,lease,query.wire()));}
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
           Err(CalendarReadExit::Transport(error)) if error.kind()==FailureKind::Rpc(RpcCode::NotFound)=>{
             dirty=false;publish(CalendarReadEvent::NotMaterialized(id.clone()));
           },
           Err(CalendarReadExit::Contract(admit::AdmissionError::StaleVersion))=>{
            dirty=false;work=Some(pending(api,lease,query.wire()));
           },
           Err(exit)=>return exit,
          }
         }
        }
    }
}
