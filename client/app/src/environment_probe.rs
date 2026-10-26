//! Opt-in real Rust TLS/cache/host proof against the isolated Go fixture.
use crate::{
    dashboard::{DashboardId, LayoutState, ProjectionRequest},
    dashboard_catalogue::{Capture, CaptureOutcome},
    device_view::DeviceStore,
    environment_view::ProfileZone,
};
use home_client_cache::{
    CacheOwner, CachePhase, CachedView, DeliveryTicket, Filter, read_snapshot,
};
use home_client_session::{SecureStore, SessionCoordinator, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::home::{environment::v1 as env, state::v1 as state};
use serde::{Deserialize, Serialize};
use std::{
    collections::VecDeque,
    io::Write,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;
fn admit<T, E: std::fmt::Debug>(value: std::result::Result<T, E>) -> Result<T> {
    value.map_err(|e| format!("admission: {e:?}").into())
}
#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Reader {
    login_name: String,
    password: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    entity_count: usize,
    member: Reader,
    guest: Reader,
}
#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> std::result::Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, value: &[u8]) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(value.to_vec()));
        Ok(())
    }
    fn clear(&self) -> std::result::Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
#[derive(Clone, Copy, Serialize)]
#[serde(rename_all = "kebab-case")]
enum Control {
    Stale,
    Unknown,
    Absent,
    Nominal,
    Missing,
    RevokeDetails,
    AllowDetails,
    ProfileParis,
    ProfileToronto,
    Outage,
    RestoreNetwork,
}
#[derive(Default)]
struct Controls(u64);
impl Controls {
    async fn send(&mut self, action: Control) -> Result<()> {
        self.0 = self.0.checked_add(1).ok_or("control exhausted")?;
        println!(
            "NHF010_CONTROL {}",
            serde_json::json!({"sequence":self.0,"action":action})
        );
        std::io::stdout().flush()?;
        let line = tokio::task::spawn_blocking(|| {
            let mut line = String::new();
            std::io::stdin().read_line(&mut line).map(|_| line)
        })
        .await??;
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Ack {
            sequence: u64,
        }
        let ack: Ack = serde_json::from_str(&line)?;
        if ack.sequence != self.0 {
            return Err("out of order control acknowledgement".into());
        }
        Ok(())
    }
}
struct Snapshot {
    phase: CachePhase,
    view: Option<CachedView>,
}
#[derive(Default)]
struct Queue {
    tickets: Mutex<VecDeque<DeliveryTicket<Snapshot>>>,
    notify: Notify,
}
impl Queue {
    async fn until(&self, check: impl Fn(&Snapshot) -> bool) -> Result<Snapshot> {
        tokio::time::timeout(Duration::from_secs(20), async {
            loop {
                let notified = self.notify.notified();
                let ticket = self.tickets.lock().unwrap().pop_front();
                if let Some(ticket) = ticket {
                    let mut snapshot = None;
                    ticket.deliver(|_, value| snapshot = Some(value));
                    if let Some(snapshot) = snapshot
                        && check(&snapshot)
                    {
                        return Ok(snapshot);
                    }
                } else {
                    notified.await;
                }
            }
        })
        .await
        .map_err(|_| "cache projection timed out")?
    }
}
fn private(view: &CachedView) -> bool {
    view.entities().any(|(_, entity)| {
        matches!(
            entity.state.value,
            Some(state::observed_state::Value::PresenceDetails(_))
        )
    })
}
fn weather(view: &CachedView) -> Option<&env::WeatherValue> {
    view.entities().find_map(|(_, entity)| {
        if let Some(state::observed_state::Value::Weather(value)) = entity.state.value.as_ref() {
            Some(value.as_ref())
        } else {
            None
        }
    })
}
fn solar_instant(view: &CachedView) -> Option<(i64, i32)> {
    view.entities().find_map(|(_, entity)| {
        if let Some(state::observed_state::Value::SolarEvent(value)) = entity.state.value.as_ref()
            && let Some(home_wire_contract::home::solar::v1::solar_occurrence::Occurrence::At(at)) =
                value.occurrence.occurrence.as_ref()
        {
            Some((at.seconds, at.nanos))
        } else {
            None
        }
    })
}
async fn snapshot(
    connection: &Connection,
    session: &SessionCoordinator,
    cancel: &CancellationToken,
) -> Result<CachedView> {
    let lease = admit(session.access(cancel).await)?;
    Ok(read_snapshot(&connection.state(), &lease, Filter::all(), 1, cancel).await?)
}
async fn layout(
    connection: &Connection,
    session: &SessionCoordinator,
    cancel: &CancellationToken,
) -> Result<ProjectionRequest> {
    let lease = admit(session.access(cancel).await)?;
    let transport = connection.dashboard();
    let list = transport
        .list(lease.token(), Duration::from_secs(10), cancel)
        .await?;
    assert_eq!(list.dashboards.len(), 1);
    let id = admit(DashboardId::parse(&list.dashboards[0].id.value))?;
    let mut capture = admit(Capture::new(&list))?;
    let response = transport
        .get(
            id.as_str().into(),
            lease.token(),
            Duration::from_secs(10),
            cancel,
        )
        .await?;
    assert!(matches!(
        admit(capture.insert(&id, &response))?,
        CaptureOutcome::Accepted
    ));
    let catalogue = admit(capture.finish())?;
    let layout = catalogue
        .layout(&id)
        .ok_or("configured environment layout refused")?;
    assert!(layout.native.is_some());
    Ok(ProjectionRequest {
        dashboard: LayoutState::Ready(layout),
        catalogue: Some(catalogue),
        ..Default::default()
    })
}
async fn run(fixture: Fixture) -> Result<()> {
    let Mode::SyntheticOnly = fixture.mode;
    let origin = url::Url::parse(&fixture.origin)?;
    if origin.scheme() != "https"
        || origin.host_str() != Some("127.0.0.1")
        || origin.port().is_none()
    {
        return Err("only isolated loopback TLS accepted".into());
    }
    assert_eq!(fixture.entity_count, 4);
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let cancel = CancellationToken::new();
    let guest = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    admit(
        guest
            .login(
                fixture.guest.login_name,
                Zeroizing::new(fixture.guest.password),
            )
            .await,
    )?;
    let guest_view = snapshot(&connection, &guest, &cancel).await?;
    assert_eq!(guest_view.len(), 3);
    assert!(!private(&guest_view));
    let guest_layout = layout(&connection, &guest, &cancel).await?;
    let guest_projection =
        DeviceStore::default().project(CachePhase::Live, Some(&guest_view), &guest_layout);
    assert_eq!(guest_projection.native_devices.len(), 3);
    admit(guest.logout().await)?;
    guest.close().await;

    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    admit(
        session
            .login(
                fixture.member.login_name,
                Zeroizing::new(fixture.member.password),
            )
            .await,
    )?;
    let initial = snapshot(&connection, &session, &cancel).await?;
    assert_eq!(initial.len(), 4);
    assert!(private(&initial));
    assert_eq!(
        initial.display_profile().unwrap().time_zone,
        "America/Toronto"
    );
    let request = layout(&connection, &session, &cancel).await?;
    let store = DeviceStore::default();
    let projected = store.project(CachePhase::Live, Some(&initial), &request);
    assert_eq!(projected.native_devices.len(), 4);
    let mut kinds = Vec::new();
    for row in projected.native_devices.values() {
        kinds.push(row.ui(None).environment_kind);
    }
    assert!(kinds.contains(&crate::ProEnvironmentKind::Weather));
    assert!(kinds.contains(&crate::ProEnvironmentKind::Solar));
    assert!(kinds.contains(&crate::ProEnvironmentKind::Presence));
    let known = weather(&initial).ok_or("weather missing")?;
    assert_eq!(known.temperature.value, Some(0.));
    assert_eq!(known.temperature.unit, env::TemperatureUnit::Celsius);

    let queue = Arc::new(Queue::default());
    let target = queue.clone();
    let (owner, mut events) = CacheOwner::start(
        session.clone(),
        connection.state(),
        Filter::all(),
        |phase, view, _| Snapshot {
            phase,
            view: view.cloned(),
        },
        move |ticket| {
            target.tickets.lock().unwrap().push_back(ticket);
            target.notify.notify_one();
        },
    );
    let drain = tokio::spawn(async move { while events.recv().await.is_some() {} });
    let handle = owner.handle();
    admit(handle.activate())?;
    queue
        .until(|p| p.phase == CachePhase::Live && p.view.as_ref().is_some_and(private))
        .await?;
    let mut controls = Controls::default();
    controls.send(Control::Stale).await?;
    let stale =
        queue
            .until(|p| {
                p.view.as_ref().and_then(weather).is_some_and(|v| {
                    v.forecast.valid_until.seconds < chrono::Utc::now().timestamp()
                }) && p.view.as_ref().is_some_and(|v| {
                    v.entities()
                        .all(|(_, e)| e.state.quality.freshness == state::Freshness::Stale)
                })
            })
            .await?;
    assert!(
        stale
            .view
            .unwrap()
            .entities()
            .all(|(_, e)| e.state.quality.freshness == state::Freshness::Stale)
    );
    controls.send(Control::Unknown).await?;
    queue
        .until(|p| {
            p.view
                .as_ref()
                .and_then(weather)
                .is_some_and(|v| v.condition == Some(env::WeatherCondition::Unknown.into()))
        })
        .await?;
    controls.send(Control::Absent).await?;
    let absent=queue.until(|p|p.view.as_ref().is_some_and(|v|v.entities().any(|(_,e)|if let Some(state::observed_state::Value::SolarEvent(value))=e.state.value.as_ref(){matches!(value.occurrence.occurrence,Some(home_wire_contract::home::solar::v1::solar_occurrence::Occurrence::Absent(_)))}else{false}))).await?;
    let absent_projection = store.project(CachePhase::Live, absent.view.as_ref(), &request);
    let solar = absent_projection
        .native_devices
        .values()
        .map(|row| row.ui(None))
        .find(|row| row.environment_kind == crate::ProEnvironmentKind::Solar)
        .ok_or("solar card missing after absent event")?;
    assert!(
        slint::Model::row_data(&solar.solar.events, 0)
            .unwrap()
            .value
            .contains("Absent dans l’horizon")
    );
    controls.send(Control::Missing).await?;
    queue
        .until(|p| {
            p.view
                .as_ref()
                .is_some_and(|v| v.entities().all(|(_, e)| e.state.value.is_none()))
        })
        .await?;
    controls.send(Control::Nominal).await?;
    queue
        .until(|p| p.phase == CachePhase::Live && p.view.as_ref().is_some_and(private))
        .await?;
    controls.send(Control::ProfileParis).await?;
    // The real owner must retire the old cohort, then obtain the new profile.
    let paris = queue
        .until(|p| {
            p.view
                .as_ref()
                .and_then(CachedView::display_profile)
                .is_some_and(|v| v.time_zone == "Europe/Paris")
        })
        .await?;
    let paris = paris.view.unwrap();
    assert!(matches!(
        ProfileZone::from_profile(paris.display_profile()),
        ProfileZone::Known(_)
    ));
    controls.send(Control::ProfileToronto).await?;
    queue
        .until(|p| {
            p.view
                .as_ref()
                .and_then(CachedView::display_profile)
                .is_some_and(|v| v.time_zone == "America/Toronto")
        })
        .await?;
    controls.send(Control::RevokeDetails).await?;
    // The actual owner must publish a private clear or a newly authorized view
    // after authority termination/resync; old details cannot remain projected.
    queue
        .until(|p| p.view.is_none() || p.view.as_ref().is_some_and(|v| !private(v)))
        .await?;
    admit(handle.activate())?;
    let restricted = queue
        .until(|p| {
            p.phase == CachePhase::Live
                && p.view.as_ref().is_some_and(|v| v.len() == 3 && !private(v))
        })
        .await?;
    assert!(!private(restricted.view.as_ref().unwrap()));
    controls.send(Control::AllowDetails).await?;
    admit(handle.activate())?;
    let restored = queue
        .until(|p| p.phase == CachePhase::Live && p.view.as_ref().is_some_and(private))
        .await?;
    let before_suspend = solar_instant(restored.view.as_ref().unwrap())
        .ok_or("solar instant missing before suspend")?;
    admit(handle.suspend().await)?;
    queue
        .until(|p| p.phase == CachePhase::Suspended && p.view.is_none())
        .await?;
    admit(handle.resume())?;
    let resumed = queue
        .until(|p| p.phase == CachePhase::Live && p.view.as_ref().is_some_and(private))
        .await?;
    assert_eq!(
        solar_instant(resumed.view.as_ref().unwrap()),
        Some(before_suspend)
    );
    assert!(resumed.view.as_ref().unwrap().entities().any(|(_, e)| {
        if let Some(state::observed_state::Value::Presence(value)) = e.state.value.as_ref() {
            value.residence == env::Residence::Home
        } else {
            false
        }
    }));
    controls.send(Control::Outage).await?;
    let offline = queue
        .until(|p| p.phase == CachePhase::Offline && p.view.as_ref().is_some_and(private))
        .await?;
    assert_eq!(
        solar_instant(offline.view.as_ref().unwrap()),
        Some(before_suspend)
    );
    controls.send(Control::RestoreNetwork).await?;
    controls.send(Control::Nominal).await?;
    queue
        .until(|p| {
            p.phase == CachePhase::Live
                && p.view.as_ref().and_then(weather).is_some_and(|v| {
                    v.forecast.valid_until.seconds > chrono::Utc::now().timestamp()
                })
        })
        .await?;
    // A retained CacheHandle also retains the reliable event sender. Release
    // the probe's handle before waiting for that receiver to finish draining.
    drop(handle);
    tokio::time::timeout(Duration::from_secs(10), owner.close()).await?;
    tokio::time::timeout(Duration::from_secs(5), drain).await??;
    admit(session.logout().await)?;
    session.close().await;
    println!(
        "NHF010_RESULT {}",
        serde_json::json!({"pass":true,"guest_entities":3,"member_entities":4,"typed_host_cards":true,"ordered_watch":true,"profile_hot_reload":true,"private_purge":true,"suspension_keeps_absolute_events":true,"offline_reconnect":true,"controls":controls.0,"no_real_devices":true})
    );
    Ok(())
}

#[test]
#[ignore = "requires the explicit isolated Go NHF010 server and private stdin control harness"]
fn real_tls_environment_profiles_and_private_cache() {
    let path =
        std::env::var_os("HOMEWARD_NHF010_READY").expect("explicit synthetic fixture required");
    let fixture: Fixture = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(run(fixture)).unwrap();
}
