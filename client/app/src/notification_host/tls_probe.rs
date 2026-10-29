//! Actual native-session owner over isolated Rust/Go-race/PG/TLS.
//! Native permission and presentation evidence are injected, never OS proof.
use super::*;
use home_client_session::{SecureStore, StoreFailure};
use serde::Deserialize;
use std::{
    path::Path,
    sync::{
        Condvar,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
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

struct Injected {
    settings: Mutex<os::Settings>,
    requests: AtomicUsize,
    presented: Mutex<Vec<os::PresentationId>>,
    removed: Mutex<Vec<os::PresentationId>>,
    block: AtomicBool,
    started: AtomicBool,
    release: Mutex<bool>,
    released: Condvar,
}
impl Default for Injected {
    fn default() -> Self {
        Self {
            settings: Mutex::new(os::Settings {
                permission: os::Permission::Authorized,
                alerts: os::Setting::Enabled,
                notification_center: os::Setting::Enabled,
            }),
            requests: AtomicUsize::new(0),
            presented: Mutex::new(Vec::new()),
            removed: Mutex::new(Vec::new()),
            block: AtomicBool::new(false),
            started: AtomicBool::new(false),
            release: Mutex::new(false),
            released: Condvar::new(),
        }
    }
}
impl native::Source for Injected {
    fn backend(&self) -> os::Backend {
        // The synthetic companion fixture declares macOS, on every test host.
        os::Backend::MacOs
    }
    fn settings(&self, request: bool) -> Result<os::Settings, os::Error> {
        if request {
            self.requests.fetch_add(1, Ordering::SeqCst);
        }
        Ok(*self.settings.lock().unwrap())
    }
    fn present(
        &self,
        scope: &os::PresentationScope,
        content: &os::Content,
    ) -> Result<os::Submission, os::Error> {
        if self.block.load(Ordering::SeqCst) {
            self.started.store(true, Ordering::SeqCst);
            let (released, _) = self
                .released
                .wait_timeout_while(self.release.lock().unwrap(), Duration::from_secs(10), |v| {
                    !*v
                })
                .unwrap();
            if !*released {
                return Err(os::Error::Timeout);
            }
        }
        if !scope.allows_presentation() {
            return Err(os::Error::ScopeClosed);
        }
        content.check_fresh(SystemTime::now())?;
        let mut presented = self.presented.lock().unwrap();
        assert!(
            !presented.contains(content.id()),
            "duplicate injected native operation"
        );
        presented.push(content.id().clone());
        Ok(os::Submission::AcceptedByOs)
    }
    fn evidence(&self, id: &os::PresentationId) -> Result<os::DeliveryEvidence, os::Error> {
        Ok(if self.presented.lock().unwrap().contains(id) {
            os::DeliveryEvidence::PresentInOsCenter
        } else {
            os::DeliveryEvidence::NotObserved
        })
    }
    fn remove(&self, id: &os::PresentationId) {
        self.removed.lock().unwrap().push(id.clone());
    }
}
async fn until(owner: &Notifications, label: &str, predicate: impl Fn(&State) -> bool) {
    tokio::time::timeout(Duration::from_secs(25), async {
        loop {
            if predicate(&owner.0.state.lock().unwrap()) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap_or_else(|_| {
        let s = owner.0.state.lock().unwrap();
        panic!(
            "{label}: {:?}, running {}, pending {}, stopped {}, notice {}",
            s.stage,
            s.running,
            s.pending.is_some(),
            s.local_stopped,
            s.notice
        );
    });
}
fn action(owner: &Notifications, action: NotificationAction) {
    let generation = owner.0.state.lock().unwrap().generation;
    owner.action(generation, action);
}
#[derive(Clone, Copy)]
enum Selection {
    Home,
    Entity,
}
fn row(owner: &Notifications, selection: Selection, action: NotificationRowAction) {
    let mut state = owner.0.state.lock().unwrap();
    let entry = state
        .entries
        .iter()
        .find(|entry| {
            entry.transport_acceptance == wire::TransportAcceptance::AcceptedForegroundLan
                && match selection {
                    Selection::Home => matches!(
                        entry.target.resource.as_ref(),
                        Some(wire::target::Resource::Home(_))
                    ),
                    Selection::Entity => matches!(
                        entry.target.resource.as_ref(),
                        Some(wire::target::Resource::Entity(_))
                    ),
                }
        })
        .unwrap();
    let id = admit::DeliveryId::parse(&entry.id.value).unwrap();
    let key = state.row_key(&id).unwrap();
    state.row_action(key.token, key.generation, action);
    drop(state);
    owner.0.changed.notify_one();
}
#[derive(Clone, Copy)]
enum Control {
    Nominal,
    Entity,
    Expiring,
    Withdraw,
    Account,
    LostRegister,
    LostEnable,
    LostReceipt,
}
async fn control(root: &Path, value: Control) {
    let (file, body, ack) = match value {
        Control::Nominal => (
            "seed-nominal",
            "explicit isolated fixture",
            "seed-nominal-done.json",
        ),
        Control::Entity => (
            "seed-entity",
            "explicit isolated fixture",
            "seed-entity-done.json",
        ),
        Control::Expiring => (
            "seed-expiring",
            "explicit isolated fixture",
            "seed-expiring-done.json",
        ),
        Control::Withdraw => (
            "withdraw-target",
            "explicit isolated fixture",
            "withdraw-target-done.json",
        ),
        Control::Account => (
            "revoke-account",
            "explicit isolated fixture",
            "account-revoked.json",
        ),
        Control::LostRegister => (
            "lose-next-reply",
            "/home.notifications.v1.NotificationService/RegisterDestination",
            "",
        ),
        Control::LostEnable => (
            "lose-next-reply",
            "/home.notifications.v1.NotificationService/UpdateDestination",
            "",
        ),
        Control::LostReceipt => (
            "lose-next-reply",
            "/home.notifications.v1.NotificationService/AcknowledgeDelivery",
            "",
        ),
    };
    if !ack.is_empty() {
        match std::fs::remove_file(root.join(ack)) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => panic!("fixture ack cleanup: {error}"),
        }
    }
    std::fs::write(root.join(file), body).unwrap();
    if !ack.is_empty() {
        tokio::time::timeout(Duration::from_secs(10), async {
            while !root.join(ack).is_file() {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
    }
}
async fn register_parent(parent: &crate::companion_host::Companion) {
    parent.visible(true);
    tokio::time::timeout(Duration::from_secs(20), async {
        while !parent.notification_fixture_action(crate::ProAdminAction::Create, false) {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    assert!(parent.notification_fixture_action(crate::ProAdminAction::Save, true));
    tokio::time::timeout(Duration::from_secs(20), async {
        while parent.notification_parent().is_none() {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
}
async fn register(owner: &Notifications) {
    until(owner, "discovery", |s| {
        matches!(
            s.stage,
            Stage::Unregistered | Stage::Revoked | Stage::Expired
        ) && !s.running
    })
    .await;
    action(owner, NotificationAction::Register);
    assert!(owner.0.state.lock().unwrap().draft.is_some());
    action(owner, NotificationAction::Confirm);
    until(owner, "registration", |s| {
        s.destination.is_some() && s.pending.is_none() && !s.running && s.stage == Stage::Ready
    })
    .await;
    assert!(owner.0.state.lock().unwrap().local_stopped);
    assert!(
        !owner
            .0
            .state
            .lock()
            .unwrap()
            .destination
            .as_ref()
            .unwrap()
            .native_delivery
    );
}
async fn enable(owner: &Notifications) {
    action(owner, NotificationAction::Enable);
    action(owner, NotificationAction::Confirm);
    until(owner, "enable", |s| {
        s.destination.as_ref().is_some_and(|d| d.native_delivery)
            && !s.local_stopped
            && s.pending.is_none()
            && !s.running
    })
    .await;
}
#[tokio::test]
#[ignore = "requires fresh actual isolated notification Go-race/PG/TLS fixture"]
async fn actual_notification_owner_inbox_native_reservations_and_revocation() {
    let path = std::env::var("HOMEWARD_NHF022_TLS_FIXTURE").unwrap();
    let root = Path::new(&path);
    assert!(root.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    assert!(matches!(fixture.mode, Mode::SyntheticOnly));
    assert!(!fixture.production_configuration && fixture.no_physical_providers);
    assert!(!fixture.admin.login_name.is_empty() && !fixture.admin.password.is_empty());
    let endpoint =
        crate::profile::TrustedEndpoint::parse(&fixture.origin, &fixture.ca_pem).unwrap();
    let connection = endpoint.connection();
    let session = SessionCoordinator::new(connection.auth(), Arc::new(Store::default()));
    session
        .login(
            fixture.user.login_name.clone(),
            Zeroizing::new(fixture.user.password.clone()),
        )
        .await
        .unwrap();
    let parent = crate::companion_host::Companion::detached();
    let source = Arc::new(Injected::default());
    let owner = Notifications::new(None, source.clone());
    let stop = CancellationToken::new();
    let mut tasks = JoinSet::new();
    {
        let p = parent.clone();
        let s = session.clone();
        let api = connection.companion();
        let c = stop.clone();
        tasks.spawn(async move {
            p.run(s, api, c).await;
        });
    }
    {
        let o = owner.clone();
        let s = session.clone();
        let api = connection.clone();
        let p = parent.clone();
        let c = stop.clone();
        tasks.spawn(async move {
            o.run(s, api, p, c).await;
        });
    }
    register_parent(&parent).await;
    owner.visible(true);
    until(&owner, "initial descriptor", |s| {
        matches!(
            s.stage,
            Stage::Unregistered | Stage::Revoked | Stage::Expired
        ) && !s.running
    })
    .await;
    assert_eq!(source.requests.load(Ordering::SeqCst), 0);
    assert!(source.presented.lock().unwrap().is_empty());
    control(root, Control::LostRegister).await;
    register(&owner).await;
    control(root, Control::LostEnable).await;
    enable(&owner).await;
    control(root, Control::Nominal).await;
    until(
        &owner,
        "native reservation, receipt and provider refusal",
        |s| {
            s.entries.len() == 2
                && s.entries
                    .iter()
                    .any(|e| e.presentation == wire::Presentation::PresentInOsCenter)
                && s.entries
                    .iter()
                    .all(|e| e.reception == wire::Reception::Received)
                && !s.running
        },
    )
    .await;
    assert_eq!(source.presented.lock().unwrap().len(), 1);
    owner.visible(false);
    tokio::time::sleep(Duration::from_millis(5600)).await;
    assert_eq!(
        source.presented.lock().unwrap().len(),
        1,
        "poll replayed an OS submission"
    );
    owner.visible(true);
    until(&owner, "idle after hidden inbox", |s| !s.running).await;
    row(&owner, Selection::Home, NotificationRowAction::Open);
    until(&owner, "home grant", |s| s.grant.is_some() && !s.running).await;
    let tag = owner.0.state.lock().unwrap().grant.as_ref().unwrap().tag;
    assert!(matches!(
        owner.take_navigation(tag),
        Some(NavigationTarget::Home)
    ));
    assert!(owner.take_navigation(tag).is_none());
    control(root, Control::LostReceipt).await;
    row(&owner, Selection::Home, NotificationRowAction::Read);
    until(&owner, "read receipt resolved once", |s| {
        s.pending.is_none()
            && !s.running
            && s.entries
                .iter()
                .any(|e| e.read_state == wire::ReadState::Read)
    })
    .await;
    row(&owner, Selection::Home, NotificationRowAction::Dismiss);
    until(&owner, "dismissed native and inbox entry", |s| {
        s.entries.len() == 1 && !s.running
    })
    .await;
    control(root, Control::Entity).await;
    until(&owner, "entity native receipt", |s| {
        !s.running
            && s.entries.iter().any(|e| {
                matches!(
                    e.target.resource.as_ref(),
                    Some(wire::target::Resource::Entity(_))
                ) && e.presentation == wire::Presentation::PresentInOsCenter
            })
    })
    .await;
    row(&owner, Selection::Entity, NotificationRowAction::Open);
    until(&owner, "entity grant", |s| s.grant.is_some() && !s.running).await;
    let tag = owner.0.state.lock().unwrap().grant.as_ref().unwrap().tag;
    assert!(matches!(
        owner.take_navigation(tag),
        Some(NavigationTarget::Entity(_))
    ));
    control(root, Control::Withdraw).await;
    row(&owner, Selection::Entity, NotificationRowAction::Open);
    until(&owner, "withdrawn target denied", |s| {
        s.stage == Stage::Denied && !s.running && s.grant.is_none()
    })
    .await;
    owner.foreground(false);
    parent.foreground(false);
    control(root, Control::Expiring).await;
    tokio::time::sleep(Duration::from_millis(2300)).await;
    parent.foreground(true);
    owner.foreground(true);
    until(&owner, "resume without expired replay", |s| {
        s.stage == Stage::Ready
            && !s.running
            && !s
                .entries
                .iter()
                .any(|e| e.expires_at.seconds - e.created_at.seconds <= 3)
    })
    .await;
    assert_eq!(source.presented.lock().unwrap().len(), 2);
    source.settings.lock().unwrap().permission = os::Permission::Denied;
    action(&owner, NotificationAction::Refresh);
    until(&owner, "OS withdrawal closes native path", |s| {
        !s.running
            && s.local_stopped
            && s.destination.as_ref().is_some_and(|d| {
                !d.native_delivery && d.permission.permission == wire::Permission::Denied
            })
    })
    .await;
    source.settings.lock().unwrap().permission = os::Permission::Authorized;
    action(&owner, NotificationAction::Refresh);
    until(&owner, "OS permission return does not enable", |s| {
        !s.running
            && s.local_stopped
            && s.destination.as_ref().is_some_and(|d| {
                !d.native_delivery && d.permission.permission == wire::Permission::Authorized
            })
    })
    .await;
    enable(&owner).await;
    action(&owner, NotificationAction::Revoke);
    action(&owner, NotificationAction::Confirm);
    until(&owner, "destination revoke", |s| {
        s.stage == Stage::Revoked && !s.running && s.entries.is_empty()
    })
    .await;
    assert!(parent.notification_parent().is_some());
    action(&owner, NotificationAction::Refresh);
    register(&owner).await;
    enable(&owner).await;
    source.block.store(true, Ordering::SeqCst);
    control(root, Control::Nominal).await;
    tokio::time::timeout(Duration::from_secs(20), async {
        while !source.started.load(Ordering::SeqCst) {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    let native = owner.0.state.lock().unwrap().native.clone().unwrap();
    assert!(native.allows_presentation());
    assert!(parent.notification_fixture_action(crate::ProAdminAction::Revoke, false));
    assert!(parent.notification_fixture_action(crate::ProAdminAction::Save, true));
    assert!(
        !native.allows_presentation(),
        "parent withdrawal did not fence an in-flight OS operation"
    );
    *source.release.lock().unwrap() = true;
    source.released.notify_all();
    until(&owner, "parent revoke purges child", |s| {
        !s.running && s.entries.is_empty() && s.native.is_none()
    })
    .await;
    assert_eq!(source.presented.lock().unwrap().len(), 2);
    tokio::time::timeout(Duration::from_secs(20), async {
        let lease = session.access(&stop).await.unwrap();
        let id = owner
            .0
            .state
            .lock()
            .unwrap()
            .destination
            .as_ref()
            .unwrap()
            .id
            .clone();
        loop {
            let response = connection
                .notifications()
                .get_destination(
                    wire::GetDestinationRequest {
                        selector: Some(wire::get_destination_request::Selector::Id(Box::new(
                            (*id).clone(),
                        ))),
                        ..Default::default()
                    },
                    lease.token(),
                    Duration::from_secs(5),
                    &stop,
                )
                .await
                .unwrap();
            if response.destination.state == wire::DestinationState::Revoked {
                break;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    // A fresh native login starts a new private context and requires both
    // registrations again; no previous destination is adopted automatically.
    owner.clear();
    parent.clear();
    source.block.store(false, Ordering::SeqCst);
    session
        .login(
            fixture.user.login_name.clone(),
            Zeroizing::new(fixture.user.password.clone()),
        )
        .await
        .unwrap();
    register_parent(&parent).await;
    owner.visible(true);
    register(&owner).await;
    enable(&owner).await;
    action(&owner, NotificationAction::Refresh);
    until(&owner, "fresh final native scope", |s| {
        !s.running && s.native.is_some()
    })
    .await;
    let native = owner.0.state.lock().unwrap().native.clone().unwrap();
    assert!(native.allows_presentation());
    control(root, Control::Account).await;
    action(&owner, NotificationAction::Refresh);
    until(
        &owner,
        "actual account withdrawal clears private state",
        |s| !s.active && s.entries.is_empty() && s.destination.is_none(),
    )
    .await;
    assert!(!native.allows_presentation());
    assert_eq!(source.presented.lock().unwrap().len(), 2);
    assert_eq!(
        source.requests.load(Ordering::SeqCst),
        0,
        "read/consent implicitly prompted the OS"
    );
    stop.cancel();
    while let Some(result) = tasks.join_next().await {
        result.unwrap();
    }
    println!("NHF022_NOTIFICATION_OWNER_TLS_PASS");
}
