use super::*;

#[test]
fn native_filter_callbacks_cancel_old_period_and_navigation_removes_private_context() {
    i_slint_backend_testing::init_no_event_loop();
    let window = HomewardWindow::new().unwrap();
    let owner = History::bind(&window);
    owner.0.state.lock().unwrap().active = true;
    owner.choose(Some("synthetic-history".parse().unwrap()));
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
    owner.choose(Some("synthetic-history".parse().unwrap()));
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
    owner.choose(Some("synthetic-history".parse().unwrap()));
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
    owner.choose(Some("other-synthetic".parse().unwrap()));
    owner.apply(
        "2026-10-26T00:00:00Z",
        "2026-10-27T00:00:00Z",
        HistoryStatistic::Observation,
    );
    let state = owner.0.state.lock().unwrap();
    assert_eq!(state.generation, 0);
    assert!(state.pending.is_none());
}
