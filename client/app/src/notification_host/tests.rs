use super::*;
#[test]
fn registration_and_permission_require_a_separate_confirmation() {
    let mut state = State {
        active: true,
        visible: true,
        ..Default::default()
    };
    state.action(state.generation, NotificationAction::Permission);
    assert!(state.draft.is_some());
    assert!(state.intent.is_none());
    let old = state.generation;
    state.action(state.generation, NotificationAction::Cancel);
    assert!(state.draft.is_none());
    state.action(old, NotificationAction::Confirm);
    assert!(state.intent.is_none());
    state.action(state.generation, NotificationAction::Permission);
    state.action(state.generation, NotificationAction::Confirm);
    assert!(matches!(state.intent, Some(Intent::Permission)));
}
#[test]
fn local_stop_closes_native_scope_even_during_an_ambiguous_write() {
    let scope = os::PresentationScope::new(|| true);
    let mut state = State {
        active: true,
        visible: true,
        running: true,
        local_stopped: false,
        native: Some(scope.clone()),
        ..Default::default()
    };
    let at = SystemTime::now();
    scope
        .refresh_until(at + Duration::from_secs(5), at)
        .unwrap();
    let fence = state.fence;
    state.action(state.generation, NotificationAction::Disable);
    assert!(state.local_stopped);
    assert!(state.native.is_none());
    assert!(state.fence > fence);
    assert_eq!(
        scope.refresh_until(at + Duration::from_secs(5), at),
        Err(os::Error::ScopeClosed)
    );
    assert!(matches!(state.intent, Some(Intent::Disable)));
}
#[test]
fn malformed_or_stale_row_callbacks_cannot_create_an_intent() {
    let mut state = State {
        active: true,
        visible: true,
        ..Default::default()
    };
    for token in [i32::MIN, -1, 0, 1, i32::MAX] {
        state.row_action(token, state.generation, NotificationRowAction::Open);
        assert!(state.intent.is_none());
    }
}
#[test]
fn cleanup_runs_without_foreground_or_native_credentials() {
    let id = os::PresentationId::parse("00000000000000000000000000000001").unwrap();
    let mut state = State {
        foreground: false,
        cleanup: vec![id.clone()],
        ..Default::default()
    };
    assert!(matches!(state.plan(),Some(Job::Cleanup(ids)) if ids==vec![id]));
    assert!(state.plan().is_none());
}
