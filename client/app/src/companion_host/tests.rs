use super::*;
use home_client_platform::telemetry as os;

fn context() -> wire::Context {
    wire::Context {
        deployment: wire::Deployment::SyntheticOnly.into(),
        epoch: "a".repeat(32),
        ..Default::default()
    }
}
fn device() -> wire::DeviceDescription {
    wire::DeviceDescription {
        platform: wire::Platform::MacOs.into(),
        model: "Injected desktop model".into(),
        os_version: "synthetic-1".into(),
        capabilities: native::capabilities(os::Platform::MacOs),
        ..Default::default()
    }
}
fn record() -> wire::Installation {
    wire::Installation {
        id: wire::InstallationId {
            value: "b".repeat(32),
            ..Default::default()
        }
        .into(),
        device_id: wire::DeviceId {
            value: "c".repeat(32),
            ..Default::default()
        }
        .into(),
        account_id: home_wire_contract::home::auth::v1::AccountId {
            value: "synthetic-account".into(),
            ..Default::default()
        }
        .into(),
        session_id: home_wire_contract::home::auth::v1::SessionId {
            value: "synthetic-session".into(),
            ..Default::default()
        }
        .into(),
        device: device().into(),
        consent: wire::Consent {
            battery: true,
            connectivity: false,
            activity: false,
            cadence: wire::Cadence::Minute1.into(),
            ..Default::default()
        }
        .into(),
        state: wire::InstallationState::Active.into(),
        revision: 2,
        registration_request_id: "d".repeat(32),
        expires_at: buffa_types::google::protobuf::Timestamp {
            seconds: 2_000_000_000,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn ready() -> State {
    let mut state = State {
        active: true,
        visible: true,
        identity: Some(Identity {
            account: AccountId::parse("synthetic-account").unwrap(),
            session: SessionId::parse("synthetic-session").unwrap(),
        }),
        ..Default::default()
    };
    assert!(state.accept(context(), record(), false));
    state
}
#[test]
fn reading_or_opening_a_draft_never_confirms_an_effect() {
    let mut state = ready();
    let generation = state.generation;
    state.action(generation, ProAdminAction::Enable, false);
    assert!(state.draft.is_some());
    assert!(state.intent.is_none());
    assert!(state.pending.is_none());
    let generation = state.generation;
    state.action(generation, ProAdminAction::Cancel, true);
    assert!(state.draft.is_none());
    assert!(state.intent.is_none());
    assert!(state.record.as_ref().unwrap().consent.battery);
}
#[test]
fn restored_installation_cannot_restart_collection_from_saved_server_consent() {
    let owner = Companion::detached();
    let (scope, fence) = {
        let mut state = owner.0.state.lock().unwrap();
        state.active = true;
        state.identity = Some(Identity {
            account: AccountId::parse("synthetic-account").unwrap(),
            session: SessionId::parse("synthetic-session").unwrap(),
        });
        (state.scope, state.fence)
    };
    assert!(owner.accept_received(
        scope,
        fence,
        worker::Received::Recovered(context(), Box::new(record())),
        false
    ));
    let mut state = owner.0.state.lock().unwrap();
    assert_eq!(state.gate, Gate::UserStopped);
    assert!(state.record.as_ref().unwrap().consent.battery);
    state.requested = false;
    assert!(
        state
            .plan(Instant::now() + Duration::from_secs(600))
            .is_none()
    );
    state.need_read();
    assert!(state.accept(context(), record(), false));
    assert_eq!(state.gate, Gate::UserStopped);
}
#[test]
fn withdrawal_stops_locally_before_confirmation_and_old_callback_cannot_enable_it() {
    let mut state = ready();
    let old = state.generation;
    state.action(old, ProAdminAction::Enable, false);
    let generation = state.generation;
    state.choose(generation, 1, 2);
    assert_eq!(state.gate, Gate::UserStopped);
    assert!(state.record.as_ref().unwrap().consent.battery);
    assert!(state.intent.is_none());
    state.action(old, ProAdminAction::Save, true);
    assert!(state.intent.is_none());
    let generation = state.generation;
    state.action(generation, ProAdminAction::Cancel, true);
    assert_eq!(state.gate, Gate::UserStopped);
}
#[test]
fn hiding_settings_does_not_stop_foreground_collection_but_suspend_requires_read() {
    let mut state = ready();
    state.visible = false;
    assert!(matches!(state.plan(Instant::now()), Some(Job::Sample)));
    state.running = false;
    state.foreground(false);
    assert_eq!(state.stage, Stage::Suspended);
    assert!(state.plan(Instant::now()).is_none());
    state.foreground(true);
    assert_eq!(state.gate, Gate::NeedsRead);
    assert!(matches!(state.plan(Instant::now()), Some(Job::Read)));
}
#[test]
fn ambiguous_consent_resolves_by_read_and_a_later_local_stop_wins() {
    let mut state = ready();
    let prior = state.record.as_ref().unwrap().clone();
    let mut desired = (*prior.consent).clone();
    desired.activity = true;
    state.pending = Some(Pending {
        request: Write::Consent(Box::new(wire::UpdateConsentRequest {
            expected: context().into(),
            id: prior.id.clone(),
            expected_revision: prior.revision,
            consent: desired.clone().into(),
            ..Default::default()
        })),
        prior: Some(Box::new(prior.clone())),
        privacy: state.privacy,
        resolution_read: false,
    });
    state.stop();
    state.running = false;
    assert!(matches!(state.plan(Instant::now()), Some(Job::Read)));
    assert!(state.pending.as_ref().unwrap().resolution_read);
    let mut received = prior;
    received.revision += 1;
    received.consent = desired.into();
    assert!(state.accept(context(), received, true));
    assert_eq!(state.gate, Gate::UserStopped);
    assert!(state.pending.is_none());
    assert!(state.intent.is_none());
}
#[test]
fn native_identity_revision_regression_and_counter_exhaustion_fail_closed() {
    let mut state = ready();
    let mut regressed = record();
    regressed.revision = 1;
    assert!(!state.accept(context(), regressed, false));
    assert!(!state.identity(Identity {
        account: AccountId::parse("other-account").unwrap(),
        session: SessionId::parse("other-session").unwrap()
    }));
    assert!(state.record.is_none());
    assert!(state.pending.is_none());
    state.generation = i32::MAX;
    assert!(!state.bump());
    assert!(state.exhausted);
    assert!(state.plan(Instant::now()).is_none());
}
#[test]
fn adapter_keeps_absence_refusal_and_stale_distinct_from_values() {
    let snapshot = os::Snapshot {
        captured_at: std::time::SystemTime::now(),
        battery: os::Observation::NoHardware,
        connectivity: os::Observation::PermissionDenied,
        activity: os::Observation::Stale,
    };
    let encoded = native::encode(snapshot).unwrap();
    assert!(admit::valid_snapshot(&encoded));
    assert!(
        matches!(encoded.battery.observation,Some(wire::battery_observation::Observation::NoData(n)) if n==wire::NoData::NoHardware)
    );
    assert!(
        matches!(encoded.connectivity.observation,Some(wire::connectivity_observation::Observation::NoData(n)) if n==wire::NoData::PermissionDenied)
    );
    assert!(
        matches!(encoded.activity.observation,Some(wire::activity_observation::Observation::NoData(n)) if n==wire::NoData::Stale)
    );
}

#[test]
fn read_failure_refresh_or_old_credential_cannot_clear_a_local_stop() {
    let mut state = ready();
    state.stop();
    state.need_read();
    assert_eq!(state.gate, Gate::UserStopped);
    assert!(state.accept(context(), record(), false));
    assert_eq!(state.gate, Gate::UserStopped);
    state.next_sample = Instant::now();
    assert!(state.plan(Instant::now()).is_none());
}
#[test]
fn wall_clock_gap_expires_values_even_if_monotonic_deadline_has_not_elapsed() {
    let mut state = ready();
    let snapshot = native::encode(os::Snapshot {
        captured_at: std::time::SystemTime::UNIX_EPOCH + Duration::from_secs(1_700_000_000),
        battery: os::Observation::NoHardware,
        connectivity: os::Observation::Disabled,
        activity: os::Observation::Disabled,
    })
    .unwrap();
    state.record.as_mut().unwrap().latest = snapshot.into();
    state.next_sample = Instant::now() + Duration::from_secs(300);
    assert!(state.expire_values(1_700_000_120));
    assert!(state.record.as_ref().unwrap().latest.as_option().is_none());
    assert_eq!(state.gate, Gate::NeedsRead);
    assert!(matches!(state.plan(Instant::now()), Some(Job::Read)));
}

#[test]
fn suspension_cancels_telemetry_without_withdrawing_independent_notification_metadata() {
    let mut state = State::default();
    let collection = state.cancel.clone();
    let notification = state.notification_cancel.clone();
    state.foreground(false);
    assert!(collection.is_cancelled());
    assert!(!notification.is_cancelled());
    assert!(state.plan(Instant::now()).is_none());
    state.clear();
    assert!(notification.is_cancelled());
}
