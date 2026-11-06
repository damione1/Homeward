//! Versioned read/watch task. It shares session access and never submits actions.
use crate::dashboard::{DashboardName, LayoutState};
use crate::dashboard_catalogue::{Capture, CaptureOutcome};
use crate::dashboard_selection::{Catalogue, Choices};
use home_client_session::{Generation, Phase, SessionCoordinator};
use home_client_transport::{DashboardTransport, Failure, FailureKind, FibonacciBackoff, RpcCode};
use home_wire_contract::home::api::v2 as wire;
use std::{sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;

enum ReadFailure {
    Transport(Failure),
    Contract,
    Empty,
}

type ReadReply = Result<Option<(DashboardName, wire::QueryDashboardResponse)>, ReadFailure>;
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
    if frame.invalidated {
        Ok(())
    } else {
        Err(ReadFailure::Contract)
    }
}

async fn follow(
    transport: &DashboardTransport,
    session: &SessionCoordinator,
    cancel: &CancellationToken,
    choices: &Choices,
    revision: u64,
    view: Arc<home_client_cache::CachedView>,
    publish: &impl Fn(Generation, u64, Catalogue, LayoutState),
) -> Result<(), ReadFailure> {
    let lease = session
        .access(cancel)
        .await
        .map_err(|_| ReadFailure::Empty)?;
    // access() may itself renew. Never query an old capture with the new
    // credential, even if the Ready watch publication overtook cache teardown.
    if !view
        .source_proof()
        .is_some_and(|proof| proof.is_current() && proof.generation() == lease.generation)
    {
        return Ok(());
    }
    let linked = lease.cancellation.child_token();
    let read = async {
        loop {
            use buffa::Message;
            let mut list = wire::ListDashboardsResponse::default();
            let mut token = String::new();
            let mut seen = std::collections::BTreeSet::new();
            let mut bytes = 0u64;
            loop {
                let page = transport
                    .list_dashboards(
                        wire::ListDashboardsRequest {
                            parent: view.name().home().as_str().to_owned(),
                            page_size: 64,
                            page_token: token.clone(),
                            ..Default::default()
                        },
                        lease.token(),
                        Duration::from_secs(15),
                        &linked,
                    )
                    .await
                    .map_err(ReadFailure::Transport)?;
                bytes = bytes.saturating_add(u64::from(page.encoded_len()));
                if bytes > 16 * 1024 * 1024 || !page.__buffa_unknown_fields.is_empty() {
                    return Err(ReadFailure::Contract);
                };
                if !page.next_page_token.is_empty()
                    && (!page.resume_token.is_empty() || !seen.insert(page.next_page_token.clone()))
                {
                    return Err(ReadFailure::Contract);
                };
                token = page.next_page_token;
                list.dashboards.extend(page.dashboards);
                if list.dashboards.len() > 128 {
                    return Err(ReadFailure::Contract);
                };
                if token.is_empty() {
                    list.resume_token = page.resume_token;
                    break;
                }
            }
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
                    .watch_dashboards(
                        wire::WatchDashboardsRequest {
                            parent: view.name().home().as_str().to_owned(),
                            resume_token: list.resume_token.clone(),
                            ..Default::default()
                        },
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
            let mut capture =
                Capture::new(&list, view.clone()).map_err(|_| ReadFailure::Contract)?;
            let deadline = std::time::Instant::now() + Duration::from_secs(30);
            let mut changed_cohort = false;
            for chunk in list.dashboards.chunks(4) {
                let read = async |metadata: Option<&wire::Dashboard>| {
                    let Some(metadata) = metadata else {
                        return Ok(None);
                    };
                    let id = DashboardName::try_from(metadata.name.as_str())
                        .map_err(|_| ReadFailure::Contract)?;
                    let remaining = deadline
                        .checked_duration_since(std::time::Instant::now())
                        .ok_or(ReadFailure::Empty)?;
                    transport
                        .query_dashboard(
                            wire::QueryDashboardRequest {
                                name: id.as_str().to_owned(),
                                entity_view: view.name().as_str().to_owned(),
                                resume_token: view.checkpoint().to_owned(),
                                ..Default::default()
                            },
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
            if !lease.source_proof().is_current() || linked.is_cancelled() {
                return Ok(());
            }
            publish(lease.generation, revision, catalogue, layout);
            let mut watch = transport
                .watch_dashboards(
                    wire::WatchDashboardsRequest {
                        parent: view.name().home().as_str().to_owned(),
                        resume_token: list.resume_token.clone(),
                        ..Default::default()
                    },
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
    mut captures: tokio::sync::watch::Receiver<Option<Arc<home_client_cache::CachedView>>>,
    publish: impl Fn(Generation, u64, Catalogue, LayoutState),
) {
    let mut phases = session.subscribe();
    let mut backoff = FibonacciBackoff::default();
    loop {
        let view = *phases.borrow_and_update();
        let capture = captures.borrow_and_update().clone().filter(|capture| {
            capture
                .source_proof()
                .is_some_and(|proof| proof.is_current() && proof.generation() == view.generation)
        });
        if view.phase == Phase::Ready && capture.is_some() {
            let capture = capture.expect("checked capture");
            let attempt = cancel.child_token();
            let revision = choices.snapshot().0;
            let succeeded = std::sync::atomic::AtomicBool::new(false);
            let admit = |generation, revision, catalogue: Catalogue, layout: LayoutState| {
                if !matches!(layout, LayoutState::Loading) {
                    succeeded.store(true, std::sync::atomic::Ordering::Relaxed);
                }
                publish(generation, revision, catalogue, layout);
            };
            let result = tokio::select! { biased;
                () = cancel.cancelled() => return,
                () = retry.notified() => { attempt.cancel(); continue; },
                changed = phases.changed() => { attempt.cancel(); if changed.is_err() { return; } continue; },
                changed = captures.changed() => { attempt.cancel(); if changed.is_err() {return;} continue; },
                result = follow(&transport, &session, &attempt, &choices, revision, capture, &admit) => result,
            };
            if succeeded.load(std::sync::atomic::Ordering::Relaxed) {
                backoff.reset();
            }
            match result {
                Err(ReadFailure::Transport(failure)) => {
                    let _ = session.observe_failure(view.generation, &failure).await;
                    // A transient read interruption does not revoke a coherent
                    // server catalogue already on screen. Entity freshness and
                    // command admission still follow the cache's offline state.
                    // Authority failures (including malformed typed details)
                    // never take this retention path.
                    let resumable = failure.may_resume_read();
                    if !resumable || choices.snapshot().1.snapshot.is_none() {
                        publish(
                            view.generation,
                            revision,
                            choices.hidden(),
                            LayoutState::Unavailable,
                        );
                    }
                    if !(resumable
                        || failure.kind() == FailureKind::Rpc(RpcCode::FailedPrecondition)
                        || failure.kind() == FailureKind::Rpc(RpcCode::NotFound))
                    {
                        tokio::select! { biased;
                                    () = cancel.cancelled() => return,
                                    () = retry.notified() => {},
                                    changed = phases.changed() => if changed.is_err() { return; },
                        changed = captures.changed() => if changed.is_err() { return; },
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
                changed = captures.changed() => if changed.is_err() { return; },
                () = retry.notified() => {},
                () = tokio::time::sleep(backoff.next_delay()) => {},
            }
        } else {
            tokio::select! { biased;
                () = cancel.cancelled() => return,
                changed = phases.changed() => if changed.is_err() { return; },
                changed = captures.changed() => if changed.is_err() { return; },
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn a_foreign_checkpoint_at_any_batch_position_discards_the_rest_and_poisons_capture() {
        let view = Arc::new(crate::dashboard::capture_fixture(vec![]));
        let replies: [wire::QueryDashboardResponse; 4] = std::array::from_fn(|i| {
            let mut document = crate::dashboard::fixture_wire();
            document.name = format!("homes/synthetic/dashboards/synthetic-{i}");
            crate::dashboard::query_fixture(document, &view)
        });
        let list = wire::ListDashboardsResponse {
            dashboards: replies
                .iter()
                .map(|r| r.dashboard.as_option().unwrap().clone())
                .collect(),
            resume_token: "catalogue-checkpoint".into(),
            ..Default::default()
        };
        for changed in 0..4 {
            let mut capture = Capture::new(&list, view.clone()).unwrap();
            let batch = std::array::from_fn(|i| {
                let mut response = replies[i].clone();
                if i == changed {
                    response.resume_token = "foreign-checkpoint".into();
                }
                Ok(Some((
                    DashboardName::try_from(response.dashboard.name.as_str()).unwrap(),
                    response,
                )))
            });
            assert!(matches!(
                accept_batch(&mut capture, batch),
                Err(ReadFailure::Contract)
            ));
            assert!(capture.finish().is_err());
        }
    }
    #[test]
    fn empty_and_populated_catalogues_use_the_same_strict_invalidation_boundary() {
        use buffa::Message;
        assert!(changed(wire::WatchDashboardsResponse::default()).is_err());
        let frame = wire::WatchDashboardsResponse {
            invalidated: true,
            ..Default::default()
        };
        assert!(changed(frame.clone()).is_ok());
        let mut bytes = frame.encode_to_vec();
        bytes.extend_from_slice(&[0x98, 0x06, 0x01]);
        assert!(
            changed(wire::WatchDashboardsResponse::decode_from_slice(&bytes).unwrap()).is_err()
        );
    }
}
