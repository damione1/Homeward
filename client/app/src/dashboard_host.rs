//! Versioned read/watch task. It shares session access and never submits actions.
use crate::dashboard::{DashboardId, LayoutState};
use crate::dashboard_catalogue::{Capture, CaptureOutcome};
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

type ReadReply = Result<Option<(DashboardId, wire::GetDashboardResponse)>, ReadFailure>;
fn accept_batch(
    capture: &mut Capture,
    replies: [ReadReply; 4],
) -> Result<CaptureOutcome, ReadFailure> {
    for reply in replies {
        if let Some((id, response)) = reply? {
            match capture
                .insert(&id, &response)
                .map_err(|_| ReadFailure::Contract)?
            {
                CaptureOutcome::Accepted => {}
                CaptureOutcome::Changed => return Ok(CaptureOutcome::Changed),
            }
        }
    }
    Ok(CaptureOutcome::Accepted)
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
    let read = async {
        loop {
            let list = transport
                .list(lease.token(), Duration::from_secs(15), &linked)
                .await
                .map_err(ReadFailure::Transport)?;
            if list.resume_token.is_empty() || list.resume_token.len() > 4096 {
                return Err(ReadFailure::Contract);
            }
            let preferred = choices.snapshot().1.selected;
            let mut catalogue = Catalogue::from_wire(&list, preferred.as_ref())
                .map_err(|_| ReadFailure::Contract)?;
            if catalogue.selected.is_none() {
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
            let mut capture = Capture::new(&list).map_err(|_| ReadFailure::Contract)?;
            let deadline = std::time::Instant::now() + Duration::from_secs(30);
            let mut changed_cohort = false;
            for chunk in list.dashboards.chunks(4) {
                let read = async |metadata: Option<&wire::Metadata>| {
                    let Some(metadata) = metadata else {
                        return Ok(None);
                    };
                    let id = metadata.id.as_option().ok_or(ReadFailure::Contract)?;
                    let id = DashboardId::parse(&id.value).map_err(|_| ReadFailure::Contract)?;
                    let remaining = deadline
                        .checked_duration_since(std::time::Instant::now())
                        .ok_or(ReadFailure::Empty)?;
                    transport
                        .get(
                            id.as_str().to_owned(),
                            lease.token(),
                            remaining.min(Duration::from_secs(15)),
                            &linked,
                        )
                        .await
                        .map(|response| Some((id, response)))
                        .map_err(ReadFailure::Transport)
                };
                let (a, b, c, d) = tokio::join!(
                    read(chunk.first()),
                    read(chunk.get(1)),
                    read(chunk.get(2)),
                    read(chunk.get(3))
                );
                match accept_batch(&mut capture, [a, b, c, d])? {
                    CaptureOutcome::Accepted => {}
                    CaptureOutcome::Changed => changed_cohort = true,
                }
                if changed_cohort {
                    break;
                }
            }
            if changed_cohort {
                publish(
                    lease.generation,
                    revision,
                    choices.hidden(),
                    LayoutState::Loading,
                );
                tokio::select! { biased; () = linked.cancelled() => return Ok(()), () = tokio::time::sleep(Duration::from_millis(100)) => {} }
                continue;
            }
            let snapshot = capture.finish().map_err(|_| ReadFailure::Contract)?;
            let layout = catalogue
                .selected
                .as_ref()
                .map_or(LayoutState::Unavailable, |id| snapshot.state(id));
            catalogue.snapshot = Some(snapshot);
            if !session.is_current(lease.generation) || linked.is_cancelled() {
                return Ok(());
            }
            publish(lease.generation, revision, catalogue, layout);
            let mut watch = transport
                .watch(
                    list.resume_token,
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
    fn a_changed_response_at_any_batch_position_discards_the_rest_without_using_a_poisoned_capture()
    {
        let replies: [wire::GetDashboardResponse; 4] = std::array::from_fn(|index| {
            let mut document = crate::dashboard::fixture_wire();
            document
                .metadata
                .as_option_mut()
                .unwrap()
                .id
                .as_option_mut()
                .unwrap()
                .value = format!("synthetic-{index}");
            wire::GetDashboardResponse {
                dashboard: document.into(),
                resume_token: "synthetic-cohort-a".into(),
                ..Default::default()
            }
        });
        let list = wire::ListDashboardsResponse {
            dashboards: replies
                .iter()
                .map(|response| {
                    response
                        .dashboard
                        .as_option()
                        .unwrap()
                        .metadata
                        .as_option()
                        .unwrap()
                        .clone()
                })
                .collect(),
            resume_token: "synthetic-cohort-a".into(),
            ..Default::default()
        };
        for changed in 0..replies.len() {
            let mut capture = Capture::new(&list).unwrap();
            let batch = std::array::from_fn(|index| {
                let mut response = replies[index].clone();
                if index == changed {
                    response.resume_token = "synthetic-cohort-b".into();
                }
                Ok(Some((
                    DashboardId::parse(&format!("synthetic-{index}")).unwrap(),
                    response,
                )))
            });
            assert!(
                matches!(
                    accept_batch(&mut capture, batch),
                    Ok(CaptureOutcome::Changed)
                ),
                "position {changed}"
            );
            assert!(capture.finish().is_err());
        }
    }
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
