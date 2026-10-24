//! Versioned read/watch task. It shares session access and never submits actions.
use crate::dashboard::{DashboardId, Layout, LayoutState};
use home_client_session::{Generation, Phase, SessionCoordinator};
use home_client_transport::{DashboardTransport, Failure, FailureKind, RpcCode};
use home_wire_contract::home::dashboard::v1 as wire;
use std::{sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;

enum ReadFailure {
    Transport(Failure),
    Contract,
    Empty,
}

async fn follow(
    transport: &DashboardTransport,
    session: &SessionCoordinator,
    cancel: &CancellationToken,
    publish: &impl Fn(Generation, LayoutState),
) -> Result<(), ReadFailure> {
    let lease = session
        .access(cancel)
        .await
        .map_err(|_| ReadFailure::Empty)?;
    let linked = lease.cancellation.child_token();
    let read = async {
        loop {
            let list = transport
                .list(lease.token(), Duration::from_secs(15), &linked)
                .await
                .map_err(ReadFailure::Transport)?;
            if !list.__buffa_unknown_fields.is_empty() || list.dashboards.len() > 128 {
                return Err(ReadFailure::Contract);
            }
            let Some(selected) = list.dashboards.first() else {
                return Err(ReadFailure::Empty);
            };
            let id =
                DashboardId::parse(&selected.id.as_option().ok_or(ReadFailure::Contract)?.value)
                    .map_err(|_| ReadFailure::Contract)?;
            let response = transport
                .get(
                    id.as_str().to_owned(),
                    lease.token(),
                    Duration::from_secs(15),
                    &linked,
                )
                .await
                .map_err(ReadFailure::Transport)?;
            if !response.__buffa_unknown_fields.is_empty()
                || response.resume_token.is_empty()
                || response.resume_token.len() > 4096
            {
                return Err(ReadFailure::Contract);
            }
            let layout = Layout::try_from(
                response
                    .dashboard
                    .as_option()
                    .ok_or(ReadFailure::Contract)?,
            )
            .map_err(|_| ReadFailure::Contract)?;
            if layout.id != id {
                return Err(ReadFailure::Contract);
            }
            if !session.is_current(lease.generation) || linked.is_cancelled() {
                return Ok(());
            }
            publish(lease.generation, LayoutState::Ready(Arc::new(layout)));
            let mut watch = transport
                .watch(
                    response.resume_token,
                    lease.token(),
                    Duration::from_secs(300),
                    &linked,
                )
                .await
                .map_err(ReadFailure::Transport)?;
            let frame = watch.next().await.map_err(ReadFailure::Transport)?;
            let Some(frame) = frame else {
                return Ok(());
            };
            if !frame.__buffa_unknown_fields.is_empty() {
                return Err(ReadFailure::Contract);
            }
            // Both notifications require a fresh authorized List/Get. A token
            // from Get closes the read/watch race; no local revision arithmetic.
            match frame.result {
                Some(wire::watch_dashboards_response::Result::Snapshot(_))
                | Some(wire::watch_dashboards_response::Result::ResyncRequired(_)) => {
                    publish(lease.generation, LayoutState::Loading);
                }
                None => return Err(ReadFailure::Contract),
            }
        }
    };
    tokio::select! { biased;
        () = cancel.cancelled() => { linked.cancel(); Ok(()) },
        result = read => result,
    }
}

pub(crate) async fn run(
    transport: DashboardTransport,
    session: SessionCoordinator,
    cancel: CancellationToken,
    retry: Arc<tokio::sync::Notify>,
    publish: impl Fn(Generation, LayoutState),
) {
    let mut phases = session.subscribe();
    loop {
        let view = *phases.borrow_and_update();
        if view.phase == Phase::Ready {
            let attempt = cancel.child_token();
            let result = tokio::select! { biased;
                () = cancel.cancelled() => return,
                () = retry.notified() => { attempt.cancel(); continue; },
                changed = phases.changed() => { attempt.cancel(); if changed.is_err() { return; } continue; },
                result = follow(&transport, &session, &attempt, &publish) => result,
            };
            match result {
                Err(ReadFailure::Transport(failure)) => {
                    let _ = session.observe_failure(view.generation, &failure).await;
                    publish(view.generation, LayoutState::Unavailable);
                    if !(failure.may_resume_read()
                        || failure.kind() == FailureKind::Rpc(RpcCode::FailedPrecondition)
                        || failure.kind() == FailureKind::Rpc(RpcCode::NotFound))
                    {
                        tokio::select! { biased;
                            () = cancel.cancelled() => return,
                            () = retry.notified() => {},
                            changed = phases.changed() => if changed.is_err() { return; },
                        }
                        continue;
                    }
                }
                Err(ReadFailure::Contract) => {
                    publish(view.generation, LayoutState::Invalid);
                }
                Err(ReadFailure::Empty) => {
                    publish(view.generation, LayoutState::Unavailable);
                }
                Ok(()) => {}
            }
            tokio::select! { biased;
                () = cancel.cancelled() => return,
                changed = phases.changed() => if changed.is_err() { return; },
                () = tokio::time::sleep(Duration::from_secs(3)) => {},
            }
        } else {
            tokio::select! { biased;
                () = cancel.cancelled() => return,
                changed = phases.changed() => if changed.is_err() { return; },
            }
        }
    }
}
