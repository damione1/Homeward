//! Actual owner/controller over fresh Go/PG/native auth/TLS. OS collector
//! refusals are explicitly injected; native rendering/physical OS are separate.
use super::*;
use home_client_platform::telemetry as os;
use home_client_session::{SecureStore, StoreFailure};
use serde::Deserialize;
use std::{
    path::Path,
    sync::atomic::{AtomicBool, AtomicUsize, Ordering},
    time::SystemTime,
};
use zeroize::Zeroizing;

#[derive(Default)]
struct Store(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for Store {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, record: &[u8]) -> Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(record.to_vec()));
        Ok(())
    }
    fn clear(&self) -> Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Credentials {
    login_name: String,
    password: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    user: Credentials,
    admin: Credentials,
    production_configuration: bool,
    no_physical_providers: bool,
}

#[derive(Default)]
struct Injected {
    devices: AtomicUsize,
    samples: AtomicUsize,
    network_available: AtomicBool,
}
impl native::Source for Injected {
    fn device(&self) -> Option<wire::DeviceDescription> {
        self.devices.fetch_add(1, Ordering::SeqCst);
        Some(wire::DeviceDescription {
            platform: wire::Platform::MacOs.into(),
            model: "Injected desktop without battery".into(),
            os_version: "synthetic-1".into(),
            capabilities: native::capabilities(os::Platform::MacOs),
            ..Default::default()
        })
    }
    fn sample(
        &self,
        consent: &wire::Consent,
        cancel: &CancellationToken,
    ) -> Option<(wire::Snapshot, Duration)> {
        if cancel.is_cancelled() {
            return None;
        }
        self.samples.fetch_add(1, Ordering::SeqCst);
        let snapshot = os::Snapshot {
            captured_at: SystemTime::now(),
            battery: if consent.battery {
                os::Observation::NoHardware
            } else {
                os::Observation::Disabled
            },
            connectivity: if !consent.connectivity {
                os::Observation::Disabled
            } else if self.network_available.load(Ordering::SeqCst) {
                os::Observation::Value(os::Connectivity::LocallyReachable)
            } else {
                os::Observation::PermissionDenied
            },
            activity: if consent.activity {
                os::Observation::Value(os::AppActivity::Foreground)
            } else {
                os::Observation::Disabled
            },
        };
        Some((native::encode(snapshot)?, Duration::from_micros(7)))
    }
}
#[derive(Clone, Copy)]
enum Control {
    LoseRegister,
    LosePublish,
    RejectRead,
    RevokeAccount,
}
fn control(root: &Path, action: Control) {
    let (file, value) = match action {
        Control::LoseRegister => (
            "lose-next-reply",
            "/home.companion.v1.CompanionService/RegisterInstallation",
        ),
        Control::LosePublish => (
            "lose-next-reply",
            "/home.companion.v1.CompanionService/PublishTelemetry",
        ),
        Control::RejectRead => ("reject-next-read", "explicit isolated read rejection"),
        Control::RevokeAccount => ("revoke-account", "explicit isolated account withdrawal"),
    };
    std::fs::write(root.join(file), value).unwrap();
}
async fn until(owner: &Companion, label: &str, seconds: u64, predicate: impl Fn(&State) -> bool) {
    tokio::time::timeout(Duration::from_secs(seconds), async {
        loop {
            if predicate(&owner.0.state.lock().unwrap()) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap_or_else(|_| {
        let s = owner.0.state.lock().unwrap();
        panic!(
            "owner timed out {label}: {:?}, running {}, pending {}, gate {:?}",
            s.stage,
            s.running,
            s.pending.is_some(),
            s.gate
        )
    });
}
fn action(owner: &Companion, action: ProAdminAction, editor: bool) {
    let generation = owner.0.state.lock().unwrap().generation;
    owner.action(generation, action, editor);
}
fn choose(owner: &Companion, field: state::Field, choice: i32) {
    let mut s = owner.0.state.lock().unwrap();
    let generation = s.generation;
    s.choose(generation, field as i32, choice);
    drop(s);
    owner.0.changed.notify_one();
}

#[tokio::test]
#[ignore = "requires explicit fresh companion Go/PG/TLS fixture"]
async fn actual_companion_owner_private_consent_lifecycle_and_revocation() {
    let directory =
        std::env::var("HOMEWARD_NHF021_TLS_FIXTURE").expect("explicit isolated fixture");
    let root = Path::new(&directory);
    assert!(root.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    assert!(matches!(fixture.mode, Mode::SyntheticOnly));
    assert!(!fixture.production_configuration && fixture.no_physical_providers);
    assert!(!fixture.admin.login_name.is_empty() && !fixture.admin.password.is_empty());
    let endpoint =
        crate::profile::TrustedEndpoint::parse(&fixture.origin, &fixture.ca_pem).unwrap();
    let source = Arc::new(Injected::default());
    let owner = Companion::new(None, source.clone());
    let connection = endpoint.connection();
    let session = SessionCoordinator::new(connection.auth(), Arc::new(Store::default()));
    session
        .login(
            fixture.user.login_name.clone(),
            Zeroizing::new(fixture.user.password.clone()),
        )
        .await
        .unwrap();
    let cancel = CancellationToken::new();
    let task_owner = owner.clone();
    let task_session = session.clone();
    let api = connection.companion();
    let stop = cancel.clone();
    let task = tokio::spawn(async move {
        task_owner.run(task_session, api, stop).await;
    });
    owner.visible(true);
    until(&owner, "discovery only", 20, |s| {
        s.context.is_some() && s.stage == Stage::Unregistered && !s.running
    })
    .await;
    assert_eq!(source.devices.load(Ordering::SeqCst), 0);
    assert_eq!(source.samples.load(Ordering::SeqCst), 0);
    action(&owner, ProAdminAction::Create, false);
    assert!(owner.0.state.lock().unwrap().draft.is_some());
    action(&owner, ProAdminAction::Cancel, true);
    assert!(owner.0.state.lock().unwrap().record.is_none());
    assert_eq!(source.devices.load(Ordering::SeqCst), 0);
    control(root, Control::LoseRegister);
    action(&owner, ProAdminAction::Create, false);
    action(&owner, ProAdminAction::Save, true);
    until(&owner, "registration resolved without replay", 20, |s| {
        s.record.is_some() && s.pending.is_none() && !s.running && s.stage == Stage::Ready
    })
    .await;
    assert_eq!(source.devices.load(Ordering::SeqCst), 1);
    assert_eq!(source.samples.load(Ordering::SeqCst), 0);
    {
        let s = owner.0.state.lock().unwrap();
        let r = s.record.as_ref().unwrap();
        assert!(!r.consent.battery && !r.consent.connectivity && !r.consent.activity);
    }
    action(&owner, ProAdminAction::Enable, false);
    choose(&owner, state::Field::Battery, 1);
    choose(&owner, state::Field::Connectivity, 1);
    choose(&owner, state::Field::Activity, 1);
    choose(&owner, state::Field::Cadence, 1);
    assert_eq!(source.samples.load(Ordering::SeqCst), 0);
    control(root, Control::LosePublish);
    action(&owner, ProAdminAction::Save, true);
    until(
        &owner,
        "first sample original sequence resolution",
        20,
        |s| {
            s.record.as_ref().is_some_and(|r| r.last_sequence == 1)
                && s.pending.is_none()
                && !s.running
        },
    )
    .await;
    assert_eq!(source.samples.load(Ordering::SeqCst), 1);
    {
        let s = owner.0.state.lock().unwrap();
        let r = s.record.as_ref().unwrap();
        assert!(
            matches!(r.latest.battery.observation,Some(wire::battery_observation::Observation::NoData(n)) if n==wire::NoData::NoHardware)
        );
        assert!(
            matches!(r.latest.connectivity.observation,Some(wire::connectivity_observation::Observation::NoData(n)) if n==wire::NoData::PermissionDenied)
        );
    }
    // Real elapsed cadence, while settings is hidden. The injected refusal
    // becoming a value is collector evidence only, not an actual OS grant.
    let first = Instant::now();
    owner.visible(false);
    source.network_available.store(true, Ordering::SeqCst);
    until(
        &owner,
        "foreground dashboard sharing continues at30s",
        45,
        |s| {
            s.record.as_ref().is_some_and(|r| r.last_sequence == 2)
                && s.pending.is_none()
                && !s.running
        },
    )
    .await;
    assert!(first.elapsed() >= Duration::from_secs(29));
    assert_eq!(source.samples.load(Ordering::SeqCst), 2);
    {
        let s = owner.0.state.lock().unwrap();
        assert!(
            matches!(s.record.as_ref().unwrap().latest.connectivity.observation,Some(wire::connectivity_observation::Observation::Value(v)) if v==wire::Connectivity::LocallyReachable)
        );
    }
    owner.foreground(false);
    assert!(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .record
            .as_ref()
            .unwrap()
            .latest
            .as_option()
            .is_none()
    );
    tokio::time::sleep(Duration::from_millis(1100)).await;
    assert_eq!(source.samples.load(Ordering::SeqCst), 2);
    owner.foreground(true);
    owner.visible(true);
    until(&owner, "resume fresh read without catch-up", 20, |s| {
        s.stage == Stage::Ready && !s.running && s.gate == Gate::Open
    })
    .await;
    assert_eq!(source.samples.load(Ordering::SeqCst), 2);
    action(&owner, ProAdminAction::Disable, false);
    assert_eq!(owner.0.state.lock().unwrap().gate, Gate::UserStopped);
    control(root, Control::RejectRead);
    action(&owner, ProAdminAction::Refresh, false);
    until(&owner, "failed read cannot clear stop", 20, |s| {
        s.stage == Stage::Offline && !s.running
    })
    .await;
    assert_eq!(owner.0.state.lock().unwrap().gate, Gate::UserStopped);
    action(&owner, ProAdminAction::Refresh, false);
    until(&owner, "successful read cannot clear stop", 20, |s| {
        s.stage == Stage::Ready && !s.running
    })
    .await;
    {
        let mut s = owner.0.state.lock().unwrap();
        assert_eq!(s.gate, Gate::UserStopped);
        s.next_sample = Instant::now();
    }
    owner.0.changed.notify_one();
    tokio::time::sleep(Duration::from_millis(1100)).await;
    assert_eq!(source.samples.load(Ordering::SeqCst), 2);
    action(&owner, ProAdminAction::Revoke, false);
    action(&owner, ProAdminAction::Cancel, true);
    assert_eq!(
        owner.0.state.lock().unwrap().record.as_ref().unwrap().state,
        wire::InstallationState::Active
    );
    action(&owner, ProAdminAction::Revoke, false);
    action(&owner, ProAdminAction::Save, true);
    until(
        &owner,
        "explicit revoke purges only this installation",
        20,
        |s| s.stage == Stage::Revoked && s.pending.is_none() && !s.running,
    )
    .await;
    assert!(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .record
            .as_ref()
            .unwrap()
            .latest
            .as_option()
            .is_none()
    );
    let previous = session.generation();
    session.logout_local().await.unwrap();
    session
        .login(
            fixture.user.login_name,
            Zeroizing::new(fixture.user.password),
        )
        .await
        .unwrap();
    assert_ne!(previous, session.generation());
    until(
        &owner,
        "new native session drops old installation without registration",
        20,
        |s| {
            s.active
                && s.record.is_none()
                && s.pending.is_none()
                && s.stage == Stage::Unregistered
                && s.context.is_some()
                && !s.running
                && s.validated == Some(session.generation())
        },
    )
    .await;
    assert_eq!(source.devices.load(Ordering::SeqCst), 1);
    assert_eq!(source.samples.load(Ordering::SeqCst), 2);
    control(root, Control::RevokeAccount);
    tokio::time::timeout(Duration::from_secs(10), async {
        while !root.join("account-revoked.json").is_file() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
    action(&owner, ProAdminAction::Refresh, false);
    until(
        &owner,
        "native authority withdrawal purges metadata",
        20,
        |s| !s.active && s.identity.is_none() && s.record.is_none() && s.pending.is_none(),
    )
    .await;
    assert!(matches!(
        session.subscribe().borrow().phase,
        Phase::NeedsLogin | Phase::CredentialRejected
    ));
    assert_eq!(source.devices.load(Ordering::SeqCst), 1);
    assert_eq!(source.samples.load(Ordering::SeqCst), 2);
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(10), task)
        .await
        .unwrap()
        .unwrap();
    session.close().await;
    println!("NHF021_COMPANION_OWNER_TLS_PASS");
}
