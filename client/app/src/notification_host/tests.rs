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

#[test]
fn row_intents_follow_message_identity_across_reorder_and_reject_recycled_or_changed_rows() {
    fn message(n: u128) -> wire::Delivery {
        wire::Delivery {
            id: wire::DeliveryId {
                value: format!("{n:032x}"),
                ..Default::default()
            }
            .into(),
            revision: 1,
            created_at: Timestamp {
                seconds: 1,
                ..Default::default()
            }
            .into(),
            expires_at: Timestamp {
                seconds: 2_000_000_000,
                ..Default::default()
            }
            .into(),
            read_state: wire::ReadState::Unread.into(),
            ..Default::default()
        }
    }
    let first = admit::DeliveryId::parse(&format!("{:032x}", 1)).unwrap();
    let replacement = admit::DeliveryId::parse(&format!("{:032x}", 3)).unwrap();
    let mut state = State {
        active: true,
        visible: true,
        entries: vec![message(1), message(2)],
        ..Default::default()
    };
    state.bump();
    let original = state.row_key(&first).unwrap();
    state.entries.reverse();
    state.bump();
    state.bump();
    let reordered = state.row_key(&first).unwrap();
    assert_eq!(
        (reordered.token, reordered.generation),
        (original.token, original.generation)
    );
    state.row_action(
        original.token,
        original.generation,
        NotificationRowAction::Open,
    );
    assert!(
        matches!(state.intent.take(),Some(Intent::Row(id,NotificationRowAction::Open)) if id==first)
    );
    state
        .entries
        .iter_mut()
        .find(|e| e.id.value == first.as_str())
        .unwrap()
        .revision = 2;
    state.bump();
    state.row_action(
        original.token,
        original.generation,
        NotificationRowAction::Dismiss,
    );
    assert!(state.intent.is_none());
    state.entries.retain(|e| e.id.value != first.as_str());
    state.entries.push(message(3));
    state.bump();
    assert_ne!(state.row_key(&replacement).unwrap().token, original.token);
    state.row_action(
        original.token,
        original.generation,
        NotificationRowAction::Open,
    );
    assert!(state.intent.is_none());
    let current = state.row_key(&replacement).unwrap();
    state.interrupt();
    state.bump();
    state.row_action(
        current.token,
        current.generation,
        NotificationRowAction::Read,
    );
    assert!(state.intent.is_none());
    assert_ne!(state.row_key(&replacement).unwrap().token, current.token);
}

#[test]
fn missing_native_services_are_valid_unavailable_snapshots_not_protocol_failures() {
    for error in [
        os::Error::BundleRequired,
        os::Error::Unsupported,
        os::Error::PendingQualification,
        os::Error::Timeout,
        os::Error::NativeUnavailable,
    ] {
        let snapshot = native::permission(Err(error));
        assert!(admit::valid_permission(&snapshot));
        assert!(!admit::can_present(&snapshot));
        assert_eq!(snapshot.permission, wire::Permission::Unknown);
        assert_eq!(snapshot.alerts, wire::Setting::Unsupported);
        assert_eq!(snapshot.notification_center, wire::Setting::Unsupported);
    }
}

#[test]
fn native_read_intent_survives_only_a_short_same_session_activity_pause() {
    let mut state = State {
        active: true,
        local_stopped: false,
        scope: 1,
        identity: Some((
            AccountId::parse("synthetic-account").unwrap(),
            SessionId::parse("synthetic-session").unwrap(),
        )),
        parent: Some(companion::Installation {
            id: companion::InstallationId {
                value: "a".repeat(32),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }),
        destination: Some(wire::Destination {
            id: wire::DestinationId {
                value: "b".repeat(32),
                ..Default::default()
            }
            .into(),
            state: wire::DestinationState::Active.into(),
            native_delivery: true,
            ..Default::default()
        }),
        ..Default::default()
    };
    let binding = state.open_binding().unwrap();
    let id = admit::DeliveryId::parse(&"c".repeat(32)).unwrap();
    let now = Instant::now();
    state.foreground = false;
    state.interrupt();
    assert!(state.capture_native_open(binding.clone(), id.clone(), now));
    assert!(state.pending_native_open(now).is_none());
    state.foreground = true;
    assert_eq!(state.pending_native_open(now), Some(id.clone()));
    state.foreground = false;
    state.interrupt();
    assert!(state.pending_native_open(now).is_none());
    state.foreground = true;
    assert_eq!(state.pending_native_open(now), Some(id.clone()));
    state.finish_native_open(&id);
    assert!(state.pending_native_open(now).is_none());
    assert!(state.capture_native_open(binding.clone(), id.clone(), now));
    assert!(
        state
            .pending_native_open(now + os::MAX_SCOPE_FRESHNESS)
            .is_none()
    );
    state.local_stopped = true;
    assert!(!state.capture_native_open(binding.clone(), id.clone(), now));
    state.local_stopped = false;
    state.identity = Some((
        AccountId::parse("another-account").unwrap(),
        SessionId::parse("another-session").unwrap(),
    ));
    assert!(!state.capture_native_open(binding, id, now));
}
