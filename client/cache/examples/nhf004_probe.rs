//! Native TLS/cache qualification. MemoryStore is only an explicit test double;
//! this executable makes no claim about Keychain, Keystore, APKs or Slint frames.
#![deny(clippy::wildcard_enum_match_arm)]

use std::{
    error::Error,
    io::Write,
    sync::{Arc, Mutex},
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use buffa_types::google::protobuf::Timestamp;
use home_client_cache::{
    CacheEvent, CacheFailure, CacheOwner, CachePhase, ControlOutcome, DeliveryOutcome,
    DeliveryTicket, Filter, FrameEffect, LiveView, SnapshotCapture, WatchExit, read_snapshot,
    watch_once,
};
use home_client_model::EntityId;
use home_client_session::{SecureStore, ServerRevocation, SessionCoordinator, StoreFailure};
use home_client_transport::{Connection, StateTransport};
use home_wire_contract::home::state::v1 as wire;
use serde::{Deserialize, Serialize};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

type Result<T> = std::result::Result<T, Box<dyn Error + Send + Sync>>;

#[derive(Clone, Copy, Deserialize, Serialize)]
#[serde(rename_all = "kebab-case")]
enum Scenario {
    Load,
    Recovery,
    SlowStream,
    LastPageExpired,
    LastPageDenied,
}
impl std::str::FromStr for Scenario {
    type Err = serde_json::Error;
    fn from_str(input: &str) -> std::result::Result<Self, Self::Err> {
        serde_json::from_value(serde_json::Value::String(input.into()))
    }
}

#[derive(Clone, Copy, Serialize)]
#[serde(rename_all = "kebab-case")]
enum Action {
    Advance,
    Load,
    Burst,
    Outage,
    Restore,
    Deny,
    Allow,
    Withdraw,
    Restart,
}

#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum FixtureMode {
    SyntheticOnly,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: FixtureMode,
    origin: String,
    ca_pem: String,
    login_name: String,
    password: String,
    first_entity: String,
    entity_count: usize,
}

#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> std::result::Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, record: &[u8]) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(record.to_vec()));
        Ok(())
    }
    fn clear(&self) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}

struct Control(u64);
impl Control {
    async fn command(&mut self, action: Action) -> Result<u64> {
        self.0 = self.0.checked_add(1).ok_or("control exhausted")?;
        println!(
            "NHF004_CONTROL {}",
            serde_json::json!({"sequence":self.0,"action":action})
        );
        std::io::stdout().flush()?;
        let line = tokio::task::spawn_blocking(|| {
            let mut line = String::new();
            std::io::stdin().read_line(&mut line).map(|_| line)
        })
        .await??;
        let response: serde_json::Value = serde_json::from_str(&line)?;
        require(
            response["sequence"].as_u64() == Some(self.0),
            "control acknowledgement out of order",
        )?;
        response["published"]
            .as_u64()
            .ok_or_else(|| "missing publication barrier".into())
    }
}

fn require(value: bool, message: &'static str) -> Result<()> {
    if value { Ok(()) } else { Err(message.into()) }
}
fn now() -> Timestamp {
    let elapsed = SystemTime::now().duration_since(UNIX_EPOCH).unwrap();
    Timestamp {
        seconds: elapsed.as_secs() as i64,
        nanos: elapsed.subsec_nanos() as i32,
        ..Default::default()
    }
}
async fn until(mut condition: impl FnMut() -> bool, seconds: u64) -> Result<()> {
    tokio::time::timeout(Duration::from_secs(seconds), async {
        while !condition() {
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .map_err(|_| "cache condition deadline exceeded".into())
}

#[derive(Clone, Copy)]
struct Projection {
    phase: CachePhase,
    count: usize,
    sequence: u64,
}
struct Observed {
    latest: Projection,
    epoch: String,
    order: Vec<u64>,
    errors: Vec<CacheEvent>,
    first_capabilities: usize,
}
impl Default for Observed {
    fn default() -> Self {
        Self {
            latest: Projection {
                phase: CachePhase::Idle,
                count: 0,
                sequence: 0,
            },
            epoch: String::new(),
            order: Vec::new(),
            errors: Vec::new(),
            first_capabilities: 0,
        }
    }
}

async fn owner_case(
    scenario: Scenario,
    session: &SessionCoordinator,
    transport: StateTransport,
    fixture: &Fixture,
    control: &mut Control,
) -> Result<serde_json::Value> {
    let observed = Arc::new(Mutex::new(Observed::default()));
    let queued = Arc::new(Mutex::new(None::<DeliveryTicket<Projection>>));
    let observed_worker = observed.clone();
    let queued_worker = queued.clone();
    let first_id: EntityId = fixture.first_entity.parse()?;
    let (owner, mut errors) = CacheOwner::start(
        session.clone(),
        transport.clone(),
        Filter::all(),
        move |phase, view, _| {
            let mut observed = observed_worker.lock().unwrap();
            let sequence = view.map_or(0, |view| {
                view.entities()
                    .filter_map(|(_, entity)| {
                        entity.state.as_option().and_then(|state| state.source_seq)
                    })
                    .max()
                    .unwrap_or(0)
            });
            let projection = Projection {
                phase,
                count: view.map_or(0, |view| view.len()),
                sequence,
            };
            if let Some(view) = view {
                let previous = observed.order.last().copied().unwrap_or(0);
                if sequence > previous && observed.order.len() < 20_000 {
                    observed.order.push(sequence);
                }
                observed.epoch = view.cursor().epoch.clone();
                observed.first_capabilities = view
                    .get(&first_id)
                    .map_or(0, |entity| entity.capabilities.len());
            }
            observed.latest = projection;
            projection
        },
        move |ticket| {
            let mut queued = queued_worker.lock().unwrap();
            assert!(
                queued.is_none(),
                "more than one pending visual notification"
            );
            *queued = Some(ticket);
        },
    );
    let handle = owner.handle();
    let observed_errors = observed.clone();
    let error_reader = tokio::spawn(async move {
        while let Some(error) = errors.recv().await {
            let mut observed = observed_errors.lock().unwrap();
            assert!(
                observed.errors.len() < 256,
                "unbounded read-error production"
            );
            observed.errors.push(error);
        }
    });
    let work = async {
        handle.activate().map_err(|e| format!("activate: {e:?}"))?;
        until(|| observed.lock().unwrap().latest.count == 850, 20).await?;
        require(handle.metrics().pending_notifications == 1, "initial UI notification missing")?;
        let details = match scenario {
            Scenario::Load => {
                control.command(Action::Load).await?;
                until(|| observed.lock().unwrap().latest.sequence == 1000, 70).await?;
                require(observed.lock().unwrap().order == (1..=1000).collect::<Vec<_>>(), "a native state update was skipped, reordered or duplicated")?;
                let metrics = handle.metrics();
                require(metrics.pending_notifications == 1 && metrics.coalesced >= 1000 && metrics.delivered == 0, "slow UI queue exceeded its bound or consumed raw frames")?;
                let ticket = queued.lock().unwrap().take().ok_or("missing coalesced delivery")?;
                let mut rendered = None;
                require(ticket.deliver(|_, value| rendered = Some(value)) == DeliveryOutcome::Delivered, "latest projection not delivered")?;
                require(rendered.is_some_and(|p| p.count == 850 && p.sequence == 1000 && p.phase == CachePhase::Live), "slow UI received an old projection")?;
                require(handle.suspend().await.map_err(|e| format!("suspend: {e:?}"))? == ControlOutcome::Applied, "suspension did not join old readers")?;
                let paused_order = observed.lock().unwrap().order.len();
                let next = control.command(Action::Advance).await?;
                tokio::time::sleep(Duration::from_millis(250)).await;
                require({ let s = observed.lock().unwrap(); s.order.len() == paused_order && s.latest.count == 0 }, "suspended reader republished")?;
                handle.resume().map_err(|e| format!("resume: {e:?}"))?;
                until(|| observed.lock().unwrap().latest.sequence == next, 10).await?;
                serde_json::json!({"entities":850,"ordered_updates":1000,"pending_ui_max":1,"coalesced":metrics.coalesced,"suspend_joined":true,"resume_fresh":true})
            }
            Scenario::Recovery => {
                let next = control.command(Action::Advance).await?;
                until(|| observed.lock().unwrap().latest.sequence == next, 5).await?;
                control.command(Action::Outage).await?;
                until(|| observed.lock().unwrap().latest.phase == CachePhase::Offline, 5).await?;
                require(observed.lock().unwrap().latest.count == 850, "ordinary outage erased the dated cache")?;
                let next = control.command(Action::Advance).await?;
                control.command(Action::Restore).await?;
                until(|| { let s = observed.lock().unwrap(); s.latest.sequence == next && s.latest.phase == CachePhase::Live }, 12).await?;
                let old_epoch = observed.lock().unwrap().epoch.clone();
                control.command(Action::Restart).await?;
                until(|| { let s = observed.lock().unwrap(); s.latest.count == 850 && s.epoch != old_epoch }, 25).await?;
                require(observed.lock().unwrap().errors.iter().any(|error| matches!(error, CacheEvent::Resync(reason) if *reason == wire::ResyncReason::EpochChanged)), "server restart did not resync the old epoch")?;
                require(handle.suspend().await.map_err(|e| format!("suspend: {e:?}"))? == ControlOutcome::Applied, "replay barrier not joined")?;
                let lease = session.access(&CancellationToken::new()).await?;
                let saved = read_snapshot(&transport, &lease, Filter::all(), 256, &CancellationToken::new()).await?;
                control.command(Action::Burst).await?;
                let mut stale_callbacks = 0;
                let exit = watch_once(&transport, &lease, saved, Duration::from_secs(10), &CancellationToken::new(), |_| stale_callbacks += 1).await;
                require(matches!(exit, WatchExit::Resync(reason) if reason == wire::ResyncReason::ReplayExpired) && stale_callbacks == 0, "expired replay published stale data")?;
                handle.resume().map_err(|e| format!("resume: {e:?}"))?;
                until(|| observed.lock().unwrap().latest.count == 850, 10).await?;
                control.command(Action::Deny).await?;
                until(|| observed.lock().unwrap().latest.count == 0, 10).await?;
                // Let any queued old producer run; the weak UI ticket must see only the invalidation.
                tokio::time::sleep(Duration::from_millis(250)).await;
                let mut disclosed = false;
                if let Some(ticket) = queued.lock().unwrap().take() { ticket.deliver(|_, p| disclosed = p.count != 0); }
                require(!disclosed, "old UI ticket disclosed data after rights withdrawal")?;
                control.command(Action::Allow).await?;
                if matches!(session.subscribe().borrow().phase, home_client_session::Phase::AuthorityUnavailable | home_client_session::Phase::RightsChanged) {
                    session.allow_new_view_attempt().await?;
                }
                handle.activate().map_err(|e| format!("reactivate: {e:?}"))?;
                until(|| observed.lock().unwrap().latest.count == 850, 10).await?;
                control.command(Action::Withdraw).await?;
                until(|| { let s=observed.lock().unwrap(); s.latest.count > 0 && s.first_capabilities == 0 }, 10).await?;
                serde_json::json!({"outage_keeps_dated_view":true,"cursor_reconnect":true,"new_epoch_resync":true,"expired_replay_resync":true,"rights_clear_pending_ui":true,"capability_withdrawal":true})
            }
            Scenario::LastPageExpired | Scenario::LastPageDenied | Scenario::SlowStream => return Err("wrong owner scenario".into()),
        };
        Ok(details)
    }.await;
    owner.close().await;
    let controls_closed = handle.activate().is_err();
    // Closed handles still own their Inner; release the last sender before
    // joining the reliable error consumer. Weak visual tickets retain no sender.
    drop(handle);
    error_reader.await?;
    if work.is_err() {
        let observed = observed.lock().unwrap();
        eprintln!(
            "NHF004_FAILURE phase={:?} entities={} events={:?}",
            observed.latest.phase, observed.latest.count, observed.errors
        );
    }
    require(controls_closed, "closed owner left usable controls")?;
    if let Some(ticket) = queued.lock().unwrap().take() {
        let mut rendered = false;
        let outcome = ticket.deliver(|_, _| rendered = true);
        require(
            !rendered
                && matches!(
                    outcome,
                    DeliveryOutcome::Closed | DeliveryOutcome::OwnerGone
                ),
            "closed owner delivered an old projection",
        )?;
    }
    work
}

async fn last_page_case(
    scenario: Scenario,
    session: &SessionCoordinator,
    transport: &StateTransport,
    control: &mut Control,
) -> Result<serde_json::Value> {
    let lease = session.access(&CancellationToken::new()).await?;
    let first = transport
        .open_view(
            wire::OpenViewRequest {
                filter: wire::ViewFilter::default().into(),
                page_size: 85,
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(10),
            &lease.cancellation,
        )
        .await?;
    let expires = first
        .expires_at
        .as_option()
        .ok_or("missing server expiry")?
        .clone();
    let mut capture = SnapshotCapture::begin(Filter::all(), 85, first, &now())?;
    // Nine of ten real pages (765 entities), with the last page still provisional.
    for _ in 1..9 {
        let request = capture
            .next_request()
            .ok_or("capture ended before last-page fault")?;
        let page = transport
            .read_page(
                request,
                lease.token(),
                Duration::from_secs(10),
                &lease.cancellation,
            )
            .await?;
        capture.push(page.page.into_option().ok_or("missing page")?, &now())?;
    }
    let request = capture.next_request().ok_or("missing last page")?;
    match scenario {
        Scenario::LastPageExpired => {
            let expiry =
                UNIX_EPOCH + Duration::new(expires.seconds.try_into()?, expires.nanos.try_into()?);
            let delay = expiry.duration_since(SystemTime::now()).unwrap_or_default()
                + Duration::from_millis(100);
            require(
                delay <= Duration::from_secs(61),
                "unexpected expiry interval",
            )?;
            println!("NHF004_WAIT_EXPIRY {}", delay.as_secs());
            std::io::stdout().flush()?;
            tokio::time::sleep(delay).await;
        }
        Scenario::LastPageDenied => {
            control.command(Action::Deny).await?;
        }
        Scenario::Load | Scenario::Recovery | Scenario::SlowStream => {
            return Err("wrong last-page scenario".into());
        }
    }
    let response = transport
        .read_page(
            request,
            lease.token(),
            Duration::from_secs(10),
            &lease.cancellation,
        )
        .await;
    require(
        response.is_err(),
        "real server admitted the faulted final page",
    )?;
    capture.abandon();
    require(
        capture.finish(&now()).err() == Some(CacheFailure::Abandoned),
        "partial view survived refusal",
    )?;
    Ok(
        serde_json::json!({"provisional_entities":765,"published_entities":0,"server_refused_last_page":true,"whole_capture_abandoned":true,"failure":response.err().map(|e| format!("{:?}",e.kind()))}),
    )
}

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<_> = std::env::args().collect();
    require(
        args.len() == 3,
        "private fixture and typed scenario required",
    )?;
    let scenario: Scenario = args[2].parse()?;
    let fixture: Fixture = serde_json::from_slice(&std::fs::read(&args[1])?)?;
    require(
        matches!(fixture.mode, FixtureMode::SyntheticOnly)
            && fixture.entity_count == 850
            && fixture.origin.starts_with("https://127.0.0.1:"),
        "only the isolated 850-entity fixture is accepted",
    )?;
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let store = Arc::new(MemoryStore::default());
    let session = SessionCoordinator::new(connection.auth(), store.clone());
    session
        .login(
            fixture.login_name.clone(),
            Zeroizing::new(fixture.password.clone()),
        )
        .await?;
    let mut control = Control(0);
    let result = match scenario {
        Scenario::Load | Scenario::Recovery => {
            owner_case(
                scenario,
                &session,
                connection.state(),
                &fixture,
                &mut control,
            )
            .await
        }
        Scenario::LastPageExpired | Scenario::LastPageDenied => {
            last_page_case(scenario, &session, &connection.state(), &mut control).await
        }
        Scenario::SlowStream => slow_stream_case(&session, &connection.state(), &mut control).await,
    };
    // A failed outage assertion must restore only this fixture's circuit so
    // cleanup can revoke its synthetic credential and preserve the primary error.
    if matches!(scenario, Scenario::Recovery) {
        control.command(Action::Restore).await?;
    }
    let revocation = session.logout().await;
    session.close().await;
    let details = result?;
    require(
        revocation? == ServerRevocation::Acknowledged && store.load()?.is_none(),
        "synthetic session not revoked and deleted",
    )?;
    println!(
        "NHF004_RESULT {}",
        serde_json::json!({"scenario":scenario,"os":std::env::consts::OS,"secure_store":"test-double-only","owner_closed":matches!(scenario,Scenario::Load | Scenario::Recovery),"session_revoked":true,"details":details})
    );
    Ok(())
}

async fn slow_stream_case(
    session: &SessionCoordinator,
    transport: &StateTransport,
    control: &mut Control,
) -> Result<serde_json::Value> {
    let lease = session.access(&CancellationToken::new()).await?;
    let view = read_snapshot(
        transport,
        &lease,
        Filter::all(),
        256,
        &CancellationToken::new(),
    )
    .await?;
    control.command(Action::Advance).await?;
    let mut stream = transport
        .watch_view(
            wire::WatchViewRequest {
                cursor: view.cursor().clone().into(),
                filter: view.filter().to_wire().into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(60),
            &lease.cancellation,
        )
        .await?;
    let mut live = LiveView::new(view);
    let first = stream
        .next()
        .await?
        .ok_or("slow-client stream did not establish")?;
    require(
        live.apply(first)? == FrameEffect::Applied,
        "slow-client seed did not apply",
    )?;
    // Stop consuming the real HTTP body while the fixture overflows its bounded
    // live subscription. No raw-frame buffer or detached reader is added here.
    control.command(Action::Burst).await?;
    let mut before_resync = 1;
    loop {
        let frame = stream
            .next()
            .await?
            .ok_or("slow client ended without resync")?;
        match live.apply(frame)? {
            FrameEffect::Applied => {
                before_resync += 1;
                require(before_resync <= 17001, "unbounded slow-client replay")?;
            }
            FrameEffect::Resync(reason) => {
                require(
                    reason == wire::ResyncReason::ClientSlow && live.current().is_none(),
                    "slow client retained an invalid view",
                )?;
                return Ok(
                    serde_json::json!({"real_http_body_paused":true,"client_slow_resync":true,"old_view_erased":true,"frames_before_resync":before_resync}),
                );
            }
        }
    }
}
