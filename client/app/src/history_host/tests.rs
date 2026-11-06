use super::*;

#[test]
fn native_filter_callbacks_cancel_old_period_and_navigation_removes_private_context() {
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    let owner = History::bind(&window);
    owner.0.state.lock().unwrap().active = true;
    owner.choose(Some(
        EntityName::try_from("homes/test/entities/synthetic-history").unwrap(),
    ));
    window.invoke_history_apply(
        "2026-10-26T00:00:00Z".into(),
        "2026-10-27T00:00:00Z".into(),
        HistoryStatistic::Observation,
    );
    let (old, stop) = {
        let state = owner.0.state.lock().unwrap();
        assert!(state.pending.is_some());
        (state.generation, state.cancel.clone())
    };
    window.invoke_history_draft_changed();
    assert!(stop.is_cancelled());
    assert!(!owner.0.state.lock().unwrap().admits(old));
    window.invoke_history_apply(
        "2026-10-25T00:00:00Z".into(),
        "2026-10-26T00:00:00Z".into(),
        HistoryStatistic::Mean,
    );
    let (new, stop) = {
        let state = owner.0.state.lock().unwrap();
        assert!(
            state
                .pending
                .as_ref()
                .is_some_and(|(_, stat)| *stat == Statistic::Mean)
        );
        (state.generation, state.cancel.clone())
    };
    assert_ne!(old, new);
    window.invoke_history_private_cleared();
    let state = owner.0.state.lock().unwrap();
    assert!(
        state.entity.is_none()
            && state.data.is_none()
            && state.frame.is_none()
            && state.pending.is_none()
    );
    assert!(stop.is_cancelled());
    assert!(!state.admits(new));
}
#[test]
fn invalid_or_too_large_window_is_an_explicit_local_reduction_without_an_rpc() {
    let owner = History::detached();
    owner.0.state.lock().unwrap().active = true;
    owner.choose(Some(
        EntityName::try_from("homes/test/entities/synthetic-history").unwrap(),
    ));
    for (start, end, statistic) in [
        (
            "2026-10-26T00:00:00Z",
            "2026-11-26T00:00:00Z",
            HistoryStatistic::Observation,
        ),
        (
            "2026-10-26T00:00:00Z",
            "2026-11-26T00:00:00Z",
            HistoryStatistic::Mean,
        ),
        (
            "2026-10-26T00:00:00.000000001Z",
            "2026-10-27T00:00:00Z",
            HistoryStatistic::Mean,
        ),
        (
            "2026-10-26T00:00:00-04:00",
            "2026-10-27T00:00:00-04:00",
            HistoryStatistic::Observation,
        ),
    ] {
        owner.apply(start, end, statistic);
        let state = owner.0.state.lock().unwrap();
        assert_eq!(state.status, Status::Failed(Failure::Query));
        assert!(state.pending.is_none());
    }
}
#[test]
fn suspension_and_generation_exhaustion_withdraw_every_held_intention() {
    let owner = History::detached();
    owner.0.state.lock().unwrap().active = true;
    owner.choose(Some(
        EntityName::try_from("homes/test/entities/synthetic-history").unwrap(),
    ));
    owner.apply(
        "2026-10-26T00:00:00Z",
        "2026-10-27T00:00:00Z",
        HistoryStatistic::Observation,
    );
    let generation = owner.0.state.lock().unwrap().generation;
    owner.foreground(false);
    owner.foreground(true);
    assert!(!owner.0.state.lock().unwrap().admits(generation));
    assert!(owner.0.state.lock().unwrap().entity.is_none());
    owner.0.state.lock().unwrap().generation = i32::MAX;
    owner.choose(Some(
        EntityName::try_from("homes/test/entities/other-synthetic").unwrap(),
    ));
    owner.apply(
        "2026-10-26T00:00:00Z",
        "2026-10-27T00:00:00Z",
        HistoryStatistic::Observation,
    );
    let state = owner.0.state.lock().unwrap();
    assert_eq!(state.generation, 0);
    assert!(state.pending.is_none());
}

#[test]
fn cohort_pause_preserves_historical_selection_but_rejects_every_old_intention() {
    let owner = History::detached();
    let entity = EntityName::try_from("homes/test/entities/synthetic-history").unwrap();
    owner.choose(Some(entity.clone()));
    let (old, cancel) = {
        let mut state = owner.0.state.lock().unwrap();
        state.active = true;
        state.cursor = Some(7);
        state.range = Some((0.2, 0.8));
        state.group = 2;
        state.status = Status::Ready;
        (state.generation, state.cancel.clone())
    };
    owner.suspend(Some(entity.clone()));
    owner.apply(
        "2026-10-26T00:00:00Z",
        "2026-10-27T00:00:00Z",
        HistoryStatistic::Observation,
    );
    owner.group(NavigationStep::Next);
    owner.draft();
    {
        let state = owner.0.state.lock().unwrap();
        assert!(state.paused && cancel.is_cancelled() && !state.admits(old));
        assert!(!state.admits(state.generation));
        assert_eq!(state.cursor, Some(7));
        assert_eq!(state.range, Some((0.2, 0.8)));
        assert_eq!(state.group, 2);
        assert!(state.pending.is_none() && state.expected_render.is_none());
    }
    owner.choose(Some(entity));
    {
        let state = owner.0.state.lock().unwrap();
        assert!(!state.paused && state.admits(state.generation));
        assert!(
            state.pending.is_none(),
            "a retained historical result is not a new query"
        );
        assert_eq!(state.range, Some((0.2, 0.8)));
    }
    owner.foreground(false);
    assert_eq!(owner.0.state.lock().unwrap().range, None);
}

#[test]
fn cohort_suspend_keeps_the_prepared_historical_frame_and_query() {
    use home_client_cache::history::{Content, Pages, Query, Resolution, Snapshot};
    use home_wire_contract::home::api::v2 as wire;
    use slint_kit_pro::charts::{ChartWorker, PreparationResult};
    let stamp = |seconds| buffa_types::google::protobuf::Timestamp {
        seconds,
        ..Default::default()
    };
    let entity = EntityName::try_from("homes/test/entities/synthetic-history").unwrap();
    let query = Query::new(
        entity.clone(),
        wire::HistoryPeriod {
            start: stamp(1_792_540_800).into(),
            end: stamp(1_792_544_400).into(),
            ..Default::default()
        },
        Resolution::Observations,
    )
    .unwrap();
    let data = Arc::new(
        Data::from_snapshot(
            Arc::new(Snapshot {
                query,
                content: Content::Retained(Pages::Observations(
                    vec![wire::QueryObservationsResponse {
                        observations: vec![wire::HistoryObservation {
                            observation_id: format!("{:064x}", 1),
                            descriptor: Some(wire::HistoryNumericDescriptor {
                                unit: "°C".into(),
                                canonical_unit: "°C".into(),
                                scale: 1.,
                                ..Default::default()
                            })
                            .into(),
                            state: wire::HistoricalState {
                                receive_time: stamp(1_792_540_900).into(),
                                value: Some(wire::historical_state::Value::Measurement(Box::new(
                                    wire::HistoricalMeasurement {
                                        unit: "°C".into(),
                                        value: 21.5,
                                        ..Default::default()
                                    },
                                ))),
                                ..Default::default()
                            }
                            .into(),
                            ..Default::default()
                        }],
                        ..Default::default()
                    }]
                    .into(),
                )),
                encoded_bytes: 0,
                points: 1,
            }),
            Statistic::Observation,
        )
        .unwrap(),
    );
    let mut state = State {
        entity: Some(entity.clone()),
        active: true,
        data: Some(data.clone()),
        cursor: Some(0),
        range: Some((0.2, 0.8)),
        ..Default::default()
    };
    let (send, receive) = std::sync::mpsc::channel();
    let mut worker = ChartWorker::new(move |notice| {
        send.send(notice).unwrap();
    })
    .unwrap();
    let id = worker.submit(render::request(&state).unwrap()).unwrap();
    let Some(PreparationResult::Ready(frame)) = receive
        .recv_timeout(std::time::Duration::from_secs(5))
        .unwrap()
        .take_ready()
    else {
        panic!("prepared frame");
    };
    state.status = Status::Preparing;
    state.expected_render = Some((id, state.generation));
    assert!(state.complete_render(PreparationResult::Ready(frame.clone())));
    assert_eq!(state.status, Status::Ready);
    assert!(state.expected_render.is_none());
    let owner = History::new(None);
    *owner.0.state.lock().unwrap() = state;
    let old = owner.0.state.lock().unwrap().generation;
    owner.suspend(Some(entity.clone()));
    let mut state = owner.0.state.lock().unwrap();
    assert!(Arc::ptr_eq(state.data.as_ref().unwrap(), &data));
    assert!(Arc::ptr_eq(state.frame.as_ref().unwrap(), &frame));
    assert_eq!(state.cursor, Some(0));
    assert_eq!(state.range, Some((0.2, 0.8)));
    assert!(state.pending.is_none() && state.expected_render.is_none() && !state.render);
    assert!(!state.admits(old) && !state.admits(state.generation));
    assert!(
        !state.resume_render,
        "the completed worker is not interrupted work"
    );
    assert!(!state.complete_render(PreparationResult::Ready(frame.clone())));
    drop(state);
    owner.choose(Some(entity));
    let mut state = owner.0.state.lock().unwrap();
    assert!(state.admits(state.generation));
    assert!(!state.render && state.pending.is_none() && state.expected_render.is_none());
    assert!(Arc::ptr_eq(state.frame.as_ref().unwrap(), &frame));
    state.invalidate();
    assert!(state.data.is_none() && state.frame.is_none());
    worker.stop();
}

#[test]
fn suspension_does_not_resurrect_an_exhausted_history_generation() {
    for generation in [0, i32::MAX] {
        let mut state = State {
            generation,
            ..Default::default()
        };
        state.suspend();
        assert_eq!(state.generation, 0);
        assert!(!state.admits(0));
    }
}

#[test]
fn a_cancelled_history_read_requires_explicit_reload_instead_of_loading_forever() {
    let owner = History::new(None);
    let entity = EntityName::try_from("homes/test/entities/synthetic-history").unwrap();
    owner.choose(Some(entity.clone()));
    let (old, cancelled) = {
        let mut state = owner.0.state.lock().unwrap();
        state.active = true;
        state.status = Status::Loading;
        (state.generation, state.cancel.clone())
    };
    owner.suspend(Some(entity.clone()));
    assert!(cancelled.is_cancelled());
    owner.choose(Some(entity));
    let state = owner.0.state.lock().unwrap();
    assert!(!state.admits(old));
    assert!(state.admits(state.generation));
    assert_eq!(state.status, Status::Failed(Failure::Interrupted));
    assert!(state.pending.is_none() && !state.render);
    assert!(state.data.is_none() && state.frame.is_none());
}
