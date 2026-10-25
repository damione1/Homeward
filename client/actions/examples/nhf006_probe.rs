//! Real TLS/controller proof. The in-memory store and loopback fake-device
//! profile are explicit test doubles, never production credential fallbacks.
#![deny(clippy::wildcard_enum_match_arm)]
use buffa_types::google::protobuf::Timestamp;
use home_client_actions::{
    ActionController, ActionHandle, LiveState, SubmissionState, WriteAccess, write_access,
};
use home_client_session::{SecureStore, SessionCoordinator, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::home::{
    actions::v1 as action, registry::v1 as registry, state::v1 as state,
};
use serde::Deserialize;
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;
type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;
#[derive(Clone, Copy, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Scenario {
    LostSubmit,
    AckOnly,
    Expire,
    Refusal,
    Withdrawn,
}
#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    login_name: String,
    password: String,
    scenario: Scenario,
}
#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> std::result::Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, data: &[u8]) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(data.to_vec()));
        Ok(())
    }
    fn clear(&self) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
async fn until(
    handle: &ActionHandle,
    check: impl Fn(&home_client_actions::ActionSnapshot) -> bool,
) -> Result<()> {
    let mut updates = handle.subscribe();
    tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            if check(&updates.borrow_and_update()) {
                return Ok(());
            }
            updates
                .changed()
                .await
                .map_err(|_| "action updates closed")?;
        }
    })
    .await
    .map_err(|_| "action predicate timed out")?
}
#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<()> {
    let path = std::env::args()
        .nth(1)
        .ok_or("private synthetic fixture required")?;
    let fixture: Fixture = serde_json::from_slice(&std::fs::read(path)?)?;
    let Mode::SyntheticOnly = fixture.mode;
    if !fixture.origin.starts_with("https://127.0.0.1:") {
        return Err("only loopback synthetic fixture accepted".into());
    }
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    session
        .login(fixture.login_name, Zeroizing::new(fixture.password))
        .await
        .map_err(|_| "native login failed")?;
    let cancel = CancellationToken::new();
    let lease = session.access(&cancel).await.map_err(|_| "no session")?;
    let page = connection
        .state()
        .open_view(
            state::OpenViewRequest {
                filter: Some(state::ViewFilter::default()).into(),
                page_size: 16,
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(10),
            &cancel,
        )
        .await?;
    if page.entities.len() != 1 {
        return Err("exact isolated view required".into());
    }
    let entity = &page.entities[0];
    let cursor = page.cursor.as_option().ok_or("no view cursor")?;
    if write_access(entity, "core.switch.v1", "relay") != WriteAccess::Allowed {
        return Err("server write permission required".into());
    }
    let mut request = action::CommandRequest {
        admission_epoch: cursor.epoch.clone(),
        config_revision: cursor.config_revision,
        target: registry::EntityRef {
            reference: Some(registry::entity_ref::Reference::EntityId(
                entity.entity_id.clone(),
            )),
            ..Default::default()
        }
        .into(),
        capability_schema_id: "core.switch.v1".into(),
        endpoint: "relay".into(),
        arguments: action::ActionArgs {
            arguments: Some(action::action_args::Arguments::SetSwitch(
                action::SetSwitch {
                    on: true,
                    ..Default::default()
                }
                .into(),
            )),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    match fixture.scenario {
        Scenario::Expire => {
            request.expires_at = Timestamp::from_unix_secs(
                std::time::SystemTime::now()
                    .duration_since(std::time::UNIX_EPOCH)?
                    .as_secs() as i64
                    + 3,
            )
            .into();
        }
        Scenario::Refusal => request.config_revision += 1,
        Scenario::LostSubmit | Scenario::AckOnly | Scenario::Withdrawn => {}
    }
    let (controller, mut notices) = ActionController::start(session.clone(), connection.actions());
    let drain = tokio::spawn(async move {
        let mut count = 0;
        while notices.recv().await.is_some() {
            count += 1;
        }
        count
    });
    let valid = Arc::new(AtomicBool::new(true));
    let check = valid.clone();
    let handle = controller
        .client()
        .submit(request, Arc::new(move || check.load(Ordering::SeqCst)))
        .map_err(|e| format!("local admission failed: {e:?}"))?;
    // On a current-thread runtime this revokes the captured generation before the
    // newly spawned command job gets a chance to wait for a token or dispatch.
    if matches!(fixture.scenario, Scenario::Withdrawn) {
        valid.store(false, Ordering::SeqCst);
    }
    let id = handle.id().as_str().to_owned();
    match fixture.scenario {
        Scenario::LostSubmit | Scenario::AckOnly => {
            until(&handle, |snapshot| {
                snapshot
                    .progress
                    .as_ref()
                    .is_some_and(|point| point.status == action::ActionStatus::Acknowledged)
            })
            .await?;
            let snapshot = handle.snapshot();
            assert_eq!(snapshot.live, LiveState::Available);
            match fixture.scenario {
                Scenario::LostSubmit => assert_eq!(snapshot.submission, SubmissionState::Uncertain),
                Scenario::AckOnly => assert_eq!(snapshot.submission, SubmissionState::Replied),
                Scenario::Expire | Scenario::Refusal | Scenario::Withdrawn => unreachable!(),
            }
            tokio::time::sleep(Duration::from_millis(150)).await;
            assert_eq!(
                handle.snapshot().progress.as_ref().unwrap().status,
                action::ActionStatus::Acknowledged
            );
            handle.stop_tracking();
            assert_eq!(handle.snapshot().live, LiveState::Stopped);
        }
        Scenario::Expire => {
            until(&handle, |snapshot| {
                snapshot
                    .progress
                    .as_ref()
                    .is_some_and(|point| point.status == action::ActionStatus::Indeterminate)
            })
            .await?;
        }
        Scenario::Refusal => {
            until(&handle, |snapshot| {
                snapshot.response.as_ref().is_some_and(|reply| {
                    reply.status == action::ActionStatus::Failed
                        && reply.error.as_option().is_some()
                })
            })
            .await?;
            assert!(handle.snapshot().progress.is_none());
        }
        Scenario::Withdrawn => {
            until(&handle, |snapshot| {
                snapshot.submission == SubmissionState::NotSent
            })
            .await?;
            assert!(handle.snapshot().response.is_none());
        }
    }
    // Observations stay untouched even when a command is ACKed or expires.
    let observed = connection
        .state()
        .open_view(
            state::OpenViewRequest {
                filter: Some(state::ViewFilter::default()).into(),
                page_size: 16,
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(10),
            &cancel,
        )
        .await?;
    assert!(matches!(
        observed.entities[0]
            .state
            .as_option()
            .and_then(|s| s.value.as_ref()),
        Some(state::observed_state::Value::SwitchOn(false))
    ));
    controller.close().await;
    drop(handle);
    drain.await?;
    session.close().await;
    println!(
        "NHF006_RESULT {}",
        serde_json::json!({"pass":true,"command_id":id,"observed_unchanged":true})
    );
    Ok(())
}
