//! Versioned read/watch task. It shares session access and never submits actions.
use crate::dashboard::{DashboardId, Layout, LayoutFailure, LayoutState};
use crate::dashboard_selection::{Catalogue, Choices};
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

fn changed(frame: wire::WatchDashboardsResponse) -> Result<(), ReadFailure> {
    if !frame.__buffa_unknown_fields.is_empty() {
        return Err(ReadFailure::Contract);
    }
    match frame.result {
        Some(wire::watch_dashboards_response::Result::Snapshot(_))
        | Some(wire::watch_dashboards_response::Result::ResyncRequired(_)) => Ok(()),
        None => Err(ReadFailure::Contract),
    }
}

async fn follow(
    transport: &DashboardTransport,
    session: &SessionCoordinator,
    cancel: &CancellationToken,
    choices: &Choices,
    revision: u64,
    publish: &impl Fn(Generation, u64, Catalogue, LayoutState),
) -> Result<(), ReadFailure> {
    let lease = session
        .access(cancel)
        .await
        .map_err(|_| ReadFailure::Empty)?;
    let linked = lease.cancellation.child_token();
    let mut preferred = choices.snapshot().1.selected;
    let read = async {
        loop {
            let list = transport
                .list(lease.token(), Duration::from_secs(15), &linked)
                .await
                .map_err(ReadFailure::Transport)?;
            if list.resume_token.is_empty() || list.resume_token.len() > 4096 {
                return Err(ReadFailure::Contract);
            }
            let catalogue = Catalogue::from_wire(&list, preferred.as_ref())
                .map_err(|_| ReadFailure::Contract)?;
            preferred = catalogue.selected.clone();
            let Some(id) = catalogue.selected.as_ref() else {
                publish(
                    lease.generation,
                    revision,
                    catalogue,
                    LayoutState::Unavailable,
                );
                // Even an empty catalogue must be watched: a later addition
                // must not require the user to restart or press refresh.
                let mut watch = transport
                    .watch(
                        list.resume_token,
                        lease.token(),
                        Duration::from_secs(300),
                        &linked,
                    )
                    .await
                    .map_err(ReadFailure::Transport)?;
                let Some(frame) = watch.next().await.map_err(ReadFailure::Transport)? else {
                    return Ok(());
                };
                changed(frame)?;
                continue;
            };
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
            if response.resume_token != list.resume_token {
                publish(
                    lease.generation,
                    revision,
                    choices.hidden(),
                    LayoutState::Loading,
                );
                tokio::select! { biased; () = linked.cancelled() => return Ok(()), () = tokio::time::sleep(Duration::from_millis(100)) => {} }
                continue;
            }
            let document = response
                .dashboard
                .as_option()
                .ok_or(ReadFailure::Contract)?;
            let returned = DashboardId::parse(
                &document
                    .metadata
                    .as_option()
                    .ok_or(ReadFailure::Contract)?
                    .id
                    .as_option()
                    .ok_or(ReadFailure::Contract)?
                    .value,
            )
            .map_err(|_| ReadFailure::Contract)?;
            if returned != *id {
                return Err(ReadFailure::Contract);
            }
            let layout = match Layout::try_from(document) {
                Ok(layout) => LayoutState::Ready(Arc::new(layout)),
                Err(LayoutFailure::Unsupported) => LayoutState::unsupported(document),
                Err(LayoutFailure::Invalid | LayoutFailure::Budget) => {
                    return Err(ReadFailure::Contract);
                }
            };
            if !session.is_current(lease.generation) || linked.is_cancelled() {
                return Ok(());
            }
            publish(lease.generation, revision, catalogue, layout);
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
            changed(frame)?;
            // A fresh authorized List/Get follows either kind of notification.
            publish(
                lease.generation,
                revision,
                choices.hidden(),
                LayoutState::Loading,
            );
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
    choices: Choices,
    publish: impl Fn(Generation, u64, Catalogue, LayoutState),
) {
    let mut phases = session.subscribe();
    loop {
        let view = *phases.borrow_and_update();
        if view.phase == Phase::Ready {
            let attempt = cancel.child_token();
            let revision = choices.snapshot().0;
            let result = tokio::select! { biased;
                () = cancel.cancelled() => return,
                () = retry.notified() => { attempt.cancel(); continue; },
                changed = phases.changed() => { attempt.cancel(); if changed.is_err() { return; } continue; },
                result = follow(&transport, &session, &attempt, &choices, revision, &publish) => result,
            };
            match result {
                Err(ReadFailure::Transport(failure)) => {
                    let _ = session.observe_failure(view.generation, &failure).await;
                    publish(
                        view.generation,
                        revision,
                        choices.hidden(),
                        LayoutState::Unavailable,
                    );
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
                    publish(
                        view.generation,
                        revision,
                        choices.hidden(),
                        LayoutState::Invalid,
                    );
                }
                Err(ReadFailure::Empty) => {
                    publish(
                        view.generation,
                        revision,
                        choices.hidden(),
                        LayoutState::Unavailable,
                    );
                }
                Ok(()) => {}
            }
            tokio::select! { biased;
                () = cancel.cancelled() => return,
                changed = phases.changed() => if changed.is_err() { return; },
                () = retry.notified() => {},
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

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn empty_and_populated_catalogues_use_the_same_strict_watch_frame_boundary() {
        assert!(changed(wire::WatchDashboardsResponse::default()).is_err());
        assert!(
            changed(wire::WatchDashboardsResponse {
                result: Some(wire::watch_dashboards_response::Result::Snapshot(
                    wire::ListDashboardsResponse::default().into()
                )),
                ..Default::default()
            })
            .is_ok()
        );
        assert!(
            changed(wire::WatchDashboardsResponse {
                result: Some(wire::watch_dashboards_response::Result::ResyncRequired(
                    home_wire_contract::home::configuration::v1::ResyncRequired::default().into()
                )),
                ..Default::default()
            })
            .is_ok()
        );
    }
}
