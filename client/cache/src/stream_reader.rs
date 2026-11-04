use crate::{CacheFailure, CachedView, FrameEffect, LiveView, time_key, wall_now};
use home_client_session::AccessLease;
use home_client_transport::{EntityViewTransport, Failure};
use home_wire_contract::home::api::v2 as wire;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

/// Any resync, invalid frame or authority failure withdraws the projection.
pub enum WatchExit {
    Ended(CachedView),
    Transport {
        failure: Failure,
        retained: Option<CachedView>,
    },
    Resync(wire::ApiErrorReason),
    Invalid(CacheFailure),
    Cancelled,
}
fn transport_exit(failure: Failure, view: CachedView) -> WatchExit {
    if failure
        .api_failure()
        .ok()
        .flatten()
        .is_some_and(|value| value.reason() == wire::ApiErrorReason::ResyncRequired)
    {
        return WatchExit::Resync(wire::ApiErrorReason::ResyncRequired);
    }
    let unexpired = wall_now().is_ok_and(|now| time_key(&now) < time_key(&view.expires));
    let retain = failure.may_resume_read() && unexpired;
    WatchExit::Transport {
        failure,
        retained: retain.then_some(view),
    }
}
pub async fn watch_once(
    transport: &EntityViewTransport,
    lease: &AccessLease,
    view: CachedView,
    timeout: Duration,
    cancel: &CancellationToken,
    mut on_applied: impl FnMut(&CachedView) + Send,
) -> WatchExit {
    if cancel.is_cancelled() || lease.cancellation.is_cancelled() {
        return WatchExit::Cancelled;
    }
    let request = wire::WatchEntityViewRequest {
        name: view.name().as_str().to_owned(),
        resume_token: view.checkpoint().to_owned(),
        ..Default::default()
    };
    let stream = tokio::select! {biased;
     ()=cancel.cancelled()=>return WatchExit::Cancelled,
     ()=lease.cancellation.cancelled()=>return WatchExit::Cancelled,
     result=transport.watch_entity_view(request,lease.token(),timeout,&lease.cancellation)=>result,
    };
    let mut stream = match stream {
        Ok(value) => value,
        Err(error) => return transport_exit(error, view),
    };
    let mut live = LiveView::new(view);
    loop {
        let response = tokio::select! {biased;
         ()=cancel.cancelled()=>return WatchExit::Cancelled,
         ()=lease.cancellation.cancelled()=>return WatchExit::Cancelled,
         result=stream.next()=>result,
        };
        if cancel.is_cancelled() || lease.cancellation.is_cancelled() {
            return WatchExit::Cancelled;
        }
        match response {
            Ok(Some(frame)) => match live.apply(frame) {
                Ok(FrameEffect::Applied) => {
                    on_applied(live.current().expect("admitted change retains view"))
                }
                Err(error) => return WatchExit::Invalid(error),
            },
            Ok(None) => {
                let current = live.into_current().expect("live view before terminal");
                if !wall_now().is_ok_and(|now| time_key(&now) < time_key(&current.expires)) {
                    return WatchExit::Invalid(CacheFailure::Expired);
                }
                return WatchExit::Ended(current);
            }
            Err(error) => {
                return transport_exit(
                    error,
                    live.into_current().expect("live view before failure"),
                );
            }
        }
    }
}
