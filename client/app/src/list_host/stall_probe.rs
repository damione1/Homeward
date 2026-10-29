//! Opt-in real TLS and Slint adapter proof. Only the testing UI loop stalls;
//! Tokio and the isolated Go server keep running. No native rendering claim.
use super::*;
use crate::{cache_view, dashboard::ProjectionRequest, device_view::DeviceStore};
use home_client_actions::{
    ActionController, NoticeKind, SubmissionState, WriteAccess, write_access,
};
use home_client_cache::{CacheOwner, Filter};
use home_client_model::EntityId;
use home_client_model::lists::ListId;
use home_client_session::{SecureStore, ServerRevocation, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::home::{actions::v1 as action, registry::v1 as registry};
use serde::{Deserialize, Serialize};
use slint::Model;
use std::{io::Write as _, sync::mpsc as blocking, time::Instant};
use tokio::sync::oneshot;
use zeroize::Zeroizing;

type Result<T> = std::result::Result<T, Box<dyn std::error::Error + Send + Sync>>;
fn admit<T, E: std::fmt::Debug>(value: std::result::Result<T, E>) -> Result<T> {
    value.map_err(|error| format!("stall qualification: {error:?}").into())
}
#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
struct Workload {
    list_id: String,
    item_count: usize,
    point_count: usize,
    action_entity: Option<String>,
}
#[derive(Deserialize)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    login_name: String,
    password: String,
    entity_count: usize,
    workload: Workload,
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
#[derive(Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "kebab-case")]
enum Control {
    Load,
    Snapshot,
}
#[derive(Deserialize, PartialEq, Eq)]
struct Stats {
    item_count: usize,
    completed_items: usize,
    list_revision: u64,
}
#[derive(Default)]
struct Controls(u64);
impl Controls {
    async fn send(&mut self, action: Control) -> Result<Stats> {
        self.0 += 1;
        println!(
            "NHF023_STALL_CONTROL {}",
            serde_json::json!({"sequence":self.0,"action":action})
        );
        std::io::stdout().flush()?;
        let line = tokio::task::spawn_blocking(|| {
            let mut line = String::new();
            std::io::stdin().read_line(&mut line).map(|_| line)
        })
        .await??;
        #[derive(Deserialize)]
        struct Ack {
            sequence: u64,
            action: Control,
            workload: Stats,
        }
        let ack: Ack = serde_json::from_str(&line)?;
        if ack.sequence != self.0 || ack.action != action {
            return Err("unbound workload acknowledgement".into());
        }
        Ok(ack.workload)
    }
}
async fn until(mut condition: impl FnMut() -> bool) -> Result<()> {
    tokio::time::timeout(Duration::from_secs(75), async {
        while !condition() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .map_err(|_| "stall qualification deadline".into())
}
fn items(state: &State) -> &[Arc<home_wire_contract::home::lists::v1::ItemSnapshot>] {
    match state.items.as_ref().map(|snapshot| &snapshot.rows) {
        Some(CapturedRows::Items { rows, .. }) => rows,
        Some(CapturedRows::Catalog(_)) | None => &[],
    }
}
fn ready(state: &State) -> bool {
    state.active
        && !state.busy()
        && state.catalog_phase == Freshness::Online
        && (state.selected_list.is_none() || state.item_phase == Freshness::Online)
}
async fn ui<T: Send + 'static>(
    window: &slint::Weak<HomewardWindow>,
    action: impl FnOnce(HomewardWindow) -> T + Send + 'static,
) -> Result<T> {
    let (send, receive) = oneshot::channel();
    window.upgrade_in_event_loop(move |window| {
        let _ = send.send(action(window));
    })?;
    Ok(tokio::time::timeout(Duration::from_secs(10), receive).await??)
}
async fn run(fixture: Fixture, lists: Lists, window: slint::Weak<HomewardWindow>) -> Result<()> {
    let Mode::SyntheticOnly = fixture.mode;
    let endpoint = url::Url::parse(&fixture.origin)?;
    if endpoint.scheme() != "https"
        || endpoint.host_str() != Some("127.0.0.1")
        || endpoint.port().is_none()
        || !endpoint.username().is_empty()
        || endpoint.password().is_some()
        || endpoint.path() != "/"
        || endpoint.query().is_some()
        || endpoint.fragment().is_some()
        || fixture.entity_count != 851
        || fixture.workload.item_count != 10000
        || fixture.workload.point_count != 20000
    {
        return Err("explicit isolated composite workload required".into());
    }
    let list_id: ListId = admit(fixture.workload.list_id.parse())?;
    let with_command = fixture.workload.action_entity.is_some();
    let action_entity: Option<EntityId> = fixture
        .workload
        .action_entity
        .map(|id| id.parse())
        .transpose()?;
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let store = Arc::new(MemoryStore::default());
    let session = SessionCoordinator::new(connection.auth(), store.clone());
    admit(
        session
            .login(fixture.login_name, Zeroizing::new(fixture.password))
            .await,
    )?;
    let cancel = CancellationToken::new();
    let (list_owner, list_session, list_api, list_cancel) = (
        lists.clone(),
        session.clone(),
        connection.lists(),
        cancel.child_token(),
    );
    let list_task =
        tokio::spawn(async move { list_owner.run(list_session, list_api, list_cancel).await });
    let observed = Arc::new(Mutex::new((0usize, 0u64)));
    let command_request = Arc::new(Mutex::new(None));
    let captured_command = command_request.clone();
    let projected = observed.clone();
    let devices = DeviceStore::default();
    let target = window.clone();
    let (cache, mut errors) = CacheOwner::start(
        session.clone(),
        connection.state(),
        Filter::all(),
        move |phase, view, viewport| {
            *captured_command.lock().unwrap() = action_entity.as_ref().and_then(|id| {
                let view = view?;
                let entity = view.get(id)?;
                if write_access(entity, "core.switch.v1", "relay") != WriteAccess::Allowed {
                    return None;
                }
                Some(action::CommandRequest {
                    admission_epoch: view.cursor().epoch.clone(),
                    config_revision: view.cursor().config_revision,
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
                })
            });
            *projected.lock().unwrap() = (
                view.map_or(0, |view| view.len()),
                view.map_or(0, |view| {
                    view.entities()
                        .filter_map(|(_, row)| {
                            row.state.as_option().and_then(|state| state.source_seq)
                        })
                        .max()
                        .unwrap_or(0)
                }),
            );
            cache_view::project_devices(
                phase,
                view,
                viewport,
                &ProjectionRequest::default(),
                &devices,
            )
        },
        move |ticket| {
            let _ = target.upgrade_in_event_loop(move |window| {
                ticket.deliver(|generation, projection| {
                    cache_view::render(&window, generation, projection)
                });
            });
        },
    );
    let events = tokio::spawn(async move {
        let mut seen = Vec::new();
        while let Some(event) = errors.recv().await {
            if seen.len() < 64 {
                seen.push(event);
            }
        }
        seen
    });
    let handle = cache.handle();
    let (actions, mut notices) = ActionController::start(session.clone(), connection.actions());
    let (release, released) = blocking::channel();
    let work: Result<serde_json::Value> = async {
        admit(handle.activate())?;
        until(|| observed.lock().unwrap().0 == 851 && ready(&lists.0.state.lock().unwrap())).await?;
        let (token, generation) = {
            let state = lists.0.state.lock().unwrap();
            let Some(CapturedRows::Catalog(rows)) = state.catalog.as_ref().map(|snapshot| &snapshot.rows) else { return Err("catalog missing".into()); };
            let index = rows.iter().position(|row| row.id.value == list_id.as_str()).ok_or("workload list missing")?;
            (state.catalog_tokens[index], state.generation)
        };
        ui(&window, move |window| window.invoke_lists_choose(token, generation)).await?;
        until(|| { let state=lists.0.state.lock().unwrap(); ready(&state) && items(&state).len()==10000 }).await?;
        let (token, generation) = { let state=lists.0.state.lock().unwrap(); (state.item_tokens[0], state.generation) };
        ui(&window, |window| {
            assert_eq!(window.get_lists_tasks().row_count(), 10000);
            assert!(!window.get_lists_tasks().row_data(0).unwrap().completed);
        }).await?;
        let (entered, frozen) = oneshot::channel();
        let (left, thawed) = oneshot::channel();
        window.upgrade_in_event_loop(move |_| {
            let started=Instant::now();
            let _=entered.send(());
            let unblocked=released.recv_timeout(Duration::from_secs(80)).is_ok();
            let _=left.send((unblocked,started.elapsed()));
        })?;
        frozen.await?;
        let mut controls=Controls::default();
        let initial=controls.send(Control::Load).await?;
        if initial != (Stats {item_count:10000,completed_items:0,list_revision:10001}) { return Err("unexpected initial task state".into()); }
        let command = if with_command {
            let request = command_request.lock().unwrap().clone().ok_or("authorized command target missing")?;
            let epoch = request.admission_epoch.clone();
            let revision = request.config_revision;
            let current = command_request.clone();
            Some(admit(actions.client().submit(request, Arc::new(move || {
                current.lock().unwrap().as_ref().is_some_and(|request| request.admission_epoch == epoch && request.config_revision == revision)
            })))?)
        } else { None };
        // The intent is programmatic in this adapter proof. Its genuine native
        // checkbox path is covered separately by the multi-domain UI scenario.
        lists.select_and_write(token,generation,Write::Complete(true));
        until(|| { let state=lists.0.state.lock().unwrap(); ready(&state) && state.notice.starts_with("Modification confirmée")
            && items(&state).first().is_some_and(|row| row.value.completed==Some(true)) }).await?;
        until(|| observed.lock().unwrap().1==1000).await?;
        if let Some(command) = &command {
            until(|| command.is_finished()).await?;
            let snapshot = command.snapshot();
            if snapshot.submission != SubmissionState::Replied || !snapshot.progress.as_ref().is_some_and(|point| point.status == action::ActionStatus::Confirmed) {
                return Err("actual command protocol confirmation missing".into());
            }
        }
        let cache_metrics=handle.metrics();
        let list_metrics=lists.0.mailbox.metrics();
        if cache_metrics.pending_notifications!=1 || list_metrics.pending_notifications!=1 || cache_metrics.coalesced<999 {
            return Err("stalled UI publication bounds not retained".into());
        }
        let final_stats=controls.send(Control::Snapshot).await?;
        if final_stats != (Stats {item_count:10000,completed_items:1,list_revision:10002}) { return Err("task confirmation missing or replayed".into()); }
        release.send(())?;
        let (unblocked,elapsed)=thawed.await?;
        if !unblocked || elapsed<Duration::from_secs(49) { return Err("UI stall interval not proved".into()); }
        ui(&window, |window| {
            assert_eq!(window.get_lists_tasks().row_count(),10000);
            assert!(window.get_lists_tasks().row_data(0).unwrap().completed);
            assert!(window.get_lists_notice().starts_with("Modification confirmée"));
            assert!(window.get_read_status().contains("851"));
        }).await?;
        if handle.metrics().pending_notifications!=0 || lists.0.mailbox.metrics().pending_notifications!=0 {
            return Err("visual queues did not drain after thaw".into());
        }
        // No command notice was consumed while the visual loop was frozen.
        // The reliable path must retain its exact terminal identity despite
        // coalescing 999 state projections on the separate visual path.
        let mut terminal_notice = false;
        if let Some(command) = &command {
            while let Ok(notice) = notices.try_recv() {
                if notice.id != *command.id() { return Err("foreign action notice".into()); }
                terminal_notice |= notice.kind == NoticeKind::Terminal;
            }
            if !terminal_notice || !command.snapshot().progress.as_ref().is_some_and(|point| point.status == action::ActionStatus::Confirmed) {
                return Err("command confirmation lost after visual thaw".into());
            }
        }
        Ok(serde_json::json!({"entities":851,"updates_observed":1000,"ui_stalled_seconds":elapsed.as_secs_f64(),
            "pending_state_notifications":cache_metrics.pending_notifications,"pending_list_notifications":list_metrics.pending_notifications,
            "state_coalesced":cache_metrics.coalesced,"checked_task_ui_after_thaw":true,"confirmation_ui_after_thaw":true,
            "list_revision":final_stats.list_revision,"task_mutations":1,"native_rendering":false,"device_actions":with_command,
            "terminal_notice_after_thaw":terminal_notice,"physical_device_actions":false}))
    }.await;
    // Always thaw before cleanup, including a failed assertion/control. Only this
    // test's event loop is affected; no real UI or process is stopped.
    if let Err(error) = &work {
        let state = lists.0.state.lock().unwrap();
        eprintln!(
            "NHF023_STALL_FAILURE {error}; observed={:?}; tasks={} ready={} notice={}",
            *observed.lock().unwrap(),
            items(&state).len(),
            ready(&state),
            state.notice
        );
    }
    let _ = release.send(());
    actions.close().await;
    cancel.cancel();
    let list_join = tokio::time::timeout(Duration::from_secs(10), list_task).await;
    cache.close().await;
    drop(handle);
    let seen_events = events.await?;
    let revoked = session.logout().await;
    session.close().await;
    list_join??;
    if !seen_events.is_empty() {
        eprintln!("NHF023_STALL_EVENTS {seen_events:?}");
    }
    let report = work?;
    if admit(revoked)? != ServerRevocation::Acknowledged
        || store.load()?.is_some()
        || !seen_events.is_empty()
    {
        return Err("owner shutdown/session cleanup failed (see recorded events)".into());
    }
    println!("NHF023_STALL_REPORT {report}");
    Ok(())
}
#[test]
#[ignore = "explicit isolated workload TLS fixture and one Slint testing loop"]
fn real_tls_ui_stall_keeps_latest_projection_and_task_confirmation() -> Result<()> {
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(std::env::var("HOMEWARD_NHF023_READY")?)?)?;
    i_slint_backend_testing::init_integration_test_with_system_time();
    let window = HomewardWindow::new()?;
    cache_view::bind(&window);
    let lists = Lists::bind(&window);
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()?;
    let weak = window.as_weak();
    struct QuitLoop;
    impl Drop for QuitLoop {
        fn drop(&mut self) {
            let _ = slint::quit_event_loop();
        }
    }
    let task = runtime.spawn(async move {
        let _quit = QuitLoop;
        run(fixture, lists, weak).await
    });
    let loop_result = slint::run_event_loop();
    runtime.block_on(task)??;
    loop_result?;
    Ok(())
}
