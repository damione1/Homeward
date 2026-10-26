//! Opt-in host proof over the real TLS facade and isolated Go fake driver.
//! Compiled only in the test executable; no product endpoint or credentials.
use super::*;
use crate::{
    dashboard::{DashboardId, LayoutState},
    dashboard_catalogue::{Capture, CaptureOutcome},
};
use home_client_actions::{ActionController, ActionHandle, ActionSnapshot, SubmissionState};
use home_client_cache::{Filter, FrameEffect, LiveView, read_snapshot};
use home_client_session::{SecureStore, SessionCoordinator, StoreFailure};
use home_client_transport::Connection;
use home_wire_contract::home::controls::v1 as control;
use serde::{Deserialize, Serialize};
use std::{io::Write, time::Duration};
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
#[derive(Clone, Copy, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Scenario {
    AckOnly,
    LostSubmit,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    login_name: String,
    password: String,
    entity_count: usize,
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
#[derive(Clone, Copy, Serialize)]
#[serde(rename_all = "kebab-case")]
enum Command {
    ConfirmLast,
    ObserveLast,
    WithdrawCover,
    Rediscover,
    DenyWrite,
    AllowWrite,
}
#[derive(Default)]
struct Controls(u64);
impl Controls {
    async fn command(&mut self, action: Command, id: Option<&str>) -> Result<()> {
        self.0 = self.0.checked_add(1).ok_or("control exhausted")?;
        let mut value = serde_json::json!({"sequence":self.0,"action":action});
        if let Some(id) = id {
            value["command_id"] = id.into();
        }
        println!("NHF009_CONTROL {value}");
        std::io::stdout().flush()?;
        let input = tokio::task::spawn_blocking(|| {
            let mut line = String::new();
            std::io::stdin().read_line(&mut line).map(|_| line)
        })
        .await??;
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Ack {
            sequence: u64,
        }
        let ack: Ack = serde_json::from_str(&input)?;
        if ack.sequence != self.0 {
            return Err("out of order harness acknowledgement".into());
        };
        Ok(())
    }
}
async fn until(handle: &ActionHandle, check: impl Fn(&ActionSnapshot) -> bool) -> Result<()> {
    let mut updates = handle.subscribe();
    tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            if check(&updates.borrow_and_update()) {
                return Ok(());
            };
            updates
                .changed()
                .await
                .map_err(|_| "action stream closed")?;
        }
    })
    .await
    .map_err(|_| "action predicate timed out")?
}
async fn snapshot(
    connection: &Connection,
    session: &SessionCoordinator,
    cancel: &CancellationToken,
) -> Result<CachedView> {
    let lease = admit(session.access(cancel).await)?;
    Ok(read_snapshot(&connection.state(), &lease, Filter::all(), 2, cancel).await?)
}
async fn read_request(
    connection: &Connection,
    session: &SessionCoordinator,
    cancel: &CancellationToken,
) -> Result<ProjectionRequest> {
    let lease = admit(session.access(cancel).await)?;
    let transport = connection.dashboard();
    let list = transport
        .list(lease.token(), Duration::from_secs(10), cancel)
        .await?;
    assert_eq!(list.dashboards.len(), 2);
    let id = admit(DashboardId::parse(&list.dashboards[0].id.value))?;
    let mut capture = admit(Capture::new(&list))?;
    for metadata in &list.dashboards {
        assert_eq!(
            metadata.profile,
            home_client_model::dashboard::Profile::Recipes.name()
        );
        let requested = admit(DashboardId::parse(&metadata.id.value))?;
        let response = transport
            .get(
                requested.as_str().into(),
                lease.token(),
                Duration::from_secs(10),
                cancel,
            )
            .await?;
        assert!(matches!(
            admit(capture.insert(&requested, &response))?,
            CaptureOutcome::Accepted
        ));
    }
    let catalogue = admit(capture.finish())?;
    let layout = catalogue.layout(&id).ok_or("profile2 layout refused")?;
    assert!(layout.mode == crate::dashboard::ServingMode::Synthetic && layout.native.is_some());
    Ok(ProjectionRequest {
        dashboard: LayoutState::Ready(layout),
        catalogue: Some(catalogue),
        ..Default::default()
    })
}
fn row(
    store: &DeviceStore,
    view: &CachedView,
    request: &ProjectionRequest,
    kind: ProDeviceKind,
) -> Result<CardRow> {
    let projection = store.project(CachePhase::Live, Some(view), request);
    projection
        .rows
        .into_iter()
        .find(|row| row.kind == kind && (kind != ProDeviceKind::Cover || !row.values.is_empty()))
        .ok_or_else(|| "expected typed host card missing".into())
}
#[derive(Clone, Copy, Serialize)]
#[serde(rename_all = "kebab-case")]
enum Operation {
    CoverOpen,
    CoverClose,
    CoverStop,
    CoverPosition,
    Lock,
    Unlock,
    AlarmHome,
    AlarmAway,
    AlarmDisarm,
}
impl Operation {
    fn kind(self) -> ProDeviceKind {
        match self {
            Self::CoverOpen | Self::CoverClose | Self::CoverStop | Self::CoverPosition => {
                ProDeviceKind::Cover
            }
            Self::Lock | Self::Unlock => ProDeviceKind::Lock,
            Self::AlarmHome | Self::AlarmAway | Self::AlarmDisarm => ProDeviceKind::Alarm,
        }
    }
    fn intent(self) -> DeviceIntent {
        match self {
            Self::CoverOpen => DeviceIntent::Cover(controls::CoverIntent::Operation(
                control::CoverOperation::Open,
            )),
            Self::CoverClose => DeviceIntent::Cover(controls::CoverIntent::Operation(
                control::CoverOperation::Close,
            )),
            Self::CoverStop => DeviceIntent::Cover(controls::CoverIntent::Operation(
                control::CoverOperation::Stop,
            )),
            Self::CoverPosition => DeviceIntent::Cover(controls::CoverIntent::Position(
                number::NumericInput::Text("40".into()),
            )),
            Self::Lock => DeviceIntent::Lock(control::LockOperation::Lock),
            Self::Unlock => DeviceIntent::Lock(control::LockOperation::Unlock),
            Self::AlarmHome => DeviceIntent::Alarm(control::AlarmOperation::ArmHome),
            Self::AlarmAway => DeviceIntent::Alarm(control::AlarmOperation::ArmAway),
            Self::AlarmDisarm => DeviceIntent::Alarm(control::AlarmOperation::Disarm),
        }
    }
}
async fn run(fixture: Fixture) -> Result<()> {
    let Mode::SyntheticOnly = fixture.mode;
    let origin = url::Url::parse(&fixture.origin)?;
    if origin.scheme() != "https"
        || origin.host_str() != Some("127.0.0.1")
        || origin.port().is_none()
    {
        return Err("only loopback TLS synthetic fixture accepted".into());
    }
    assert_eq!(fixture.entity_count, 4);
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes())?;
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    admit(
        session
            .login(fixture.login_name, Zeroizing::new(fixture.password))
            .await,
    )?;
    let cancel = CancellationToken::new();
    let mut request = read_request(&connection, &session, &cancel).await?;
    let store = DeviceStore::default();
    let mut view = snapshot(&connection, &session, &cancel).await?;
    assert_eq!(view.len(), 4);
    let projection = store.project(CachePhase::Live, Some(&view), &request);
    assert!(
        !projection
            .native_nodes
            .iter()
            .any(|node| node.kind == crate::dashboard_layout::Kind::Popup)
    );
    // Popup descendants are intentionally absent from the home surface; the exact
    // body remains in the decoded configured tree and is reached by navigation.
    assert!(
        request
            .dashboard
            .layout()
            .unwrap()
            .native
            .as_ref()
            .unwrap()
            .nodes
            .values()
            .any(|node| node.kind == crate::dashboard_layout::Kind::Popup)
    );
    // The streaming response may not flush headers before its first frame.
    // Keep that read independent from actions, as the actual cache owner does.
    let watch_cancel = cancel.child_token();
    let state_transport = connection.state();
    let state_session = session.clone();
    let stream_cursor = view.cursor().clone();
    let stream_filter = view.filter().to_wire();
    let worker_cancel = watch_cancel.clone();
    let (frames, mut state_frames) = tokio::sync::mpsc::channel(16);
    let watch_job = tokio::spawn(async move {
        let lease = admit(state_session.access(&worker_cancel).await)?;
        let mut watch = state_transport
            .watch_view(
                state::WatchViewRequest {
                    cursor: stream_cursor.into(),
                    filter: stream_filter.into(),
                    ..Default::default()
                },
                lease.token(),
                Duration::from_secs(90),
                &worker_cancel,
            )
            .await?;
        loop {
            let frame = tokio::select! {biased;()=worker_cancel.cancelled()=>return Ok::<(),Box<dyn std::error::Error+Send+Sync>>(()),result=watch.next()=>result?};
            let Some(frame) = frame else { return Ok(()) };
            tokio::select! {biased;()=worker_cancel.cancelled()=>return Ok(()),result=frames.send(frame)=>result.map_err(|_|"state frame consumer closed")?};
        }
    });
    let mut live = LiveView::new(view.clone());
    let (controller, mut notices) = ActionController::start(session.clone(), connection.actions());
    let drain = tokio::spawn(async move { while notices.recv().await.is_some() {} });
    let mut controls = Controls::default();
    let mut submitted = Vec::new();
    for (index, operation) in [
        Operation::CoverOpen,
        Operation::CoverClose,
        Operation::CoverStop,
        Operation::CoverPosition,
        Operation::Lock,
        Operation::Unlock,
        Operation::AlarmHome,
        Operation::AlarmAway,
        Operation::AlarmDisarm,
    ]
    .into_iter()
    .enumerate()
    {
        request.native = None;
        if operation.kind() == ProDeviceKind::Lock {
            let home = store.project(CachePhase::Live, Some(&view), &request);
            let mut popup = None;
            for recipe in home.recipes.values() {
                if let Ok(prepared) = store.prepare_recipe(
                    recipe.token,
                    recipe.generation,
                    0,
                    home_client_model::dashboard::Gesture::Tap,
                ) {
                    assert!(prepared.is_current());
                    match prepared.intent {
                        NativeIntent::Navigate(destination)
                            if destination.presentation
                                == home_client_model::dashboard::Presentation::Popup =>
                        {
                            popup = Some(destination);
                            break;
                        }
                        NativeIntent::Navigate(_)
                        | NativeIntent::MoreInfo(_)
                        | NativeIntent::Command(_) => {}
                    }
                }
            }
            request.native = Some(popup.ok_or("configured popup gesture missing")?);
        }
        let row = row(&store, &view, &request, operation.kind())?;
        let prepared = admit(store.prepare(row.token, row.generation, 0, operation.intent()))?;
        let target = prepared.target.entity().clone();
        let observed = view.get(&target).unwrap().state.value.clone();
        let handle = admit(controller.client().submit(prepared.request, prepared.guard))?;
        store.remember(prepared.target, handle.clone());
        until(&handle, |snapshot| {
            snapshot
                .progress
                .as_ref()
                .is_some_and(|p| p.status == actions::ActionStatus::Acknowledged)
        })
        .await?;
        if index == 0 {
            match fixture.scenario {
                Scenario::AckOnly => {
                    assert_eq!(handle.snapshot().submission, SubmissionState::Replied)
                }
                Scenario::LostSubmit => {
                    assert_eq!(handle.snapshot().submission, SubmissionState::Uncertain)
                }
            }
        }
        let audit = snapshot(&connection, &session, &cancel).await?;
        assert_eq!(
            audit.get(&target).unwrap().state.value,
            observed,
            "ACK changed the observation"
        );
        let id = handle.id().as_str().to_owned();
        let previous = view.get(&target).unwrap().state.revision;
        controls.command(Command::ObserveLast, Some(&id)).await?;
        tokio::time::timeout(Duration::from_secs(10), async {
            loop {
                let frame = state_frames.recv().await.ok_or("state watch ended")?;
                match admit(live.apply(frame))? {
                    FrameEffect::Applied => {}
                    FrameEffect::Resync(_) => return Err("unexpected nominal state resync".into()),
                }
                if live
                    .current()
                    .and_then(|view| view.get(&target))
                    .is_some_and(|entity| entity.state.revision > previous)
                {
                    return Ok::<(), Box<dyn std::error::Error + Send + Sync>>(());
                }
            }
        })
        .await??;
        view = live.current().ok_or("state cache abandoned")?.clone();
        let lease = admit(session.access(&cancel).await)?;
        let progress = connection
            .actions()
            .get_action(
                actions::GetActionRequest {
                    command_id: id.clone(),
                    ..Default::default()
                },
                lease.token(),
                Duration::from_secs(10),
                &cancel,
            )
            .await?;
        assert!(
            matches!(progress.live,Some(actions::get_action_response::Live::Progress(point)) if point.status==actions::ActionStatus::Acknowledged),
            "correlated observation must not confirm a secure control"
        );
        controls.command(Command::ConfirmLast, Some(&id)).await?;
        until(&handle, |snapshot| {
            snapshot
                .progress
                .as_ref()
                .is_some_and(|p| p.status == actions::ActionStatus::Confirmed)
        })
        .await?;
        tokio::time::timeout(Duration::from_secs(5), async {
            while !handle.is_finished() {
                tokio::task::yield_now().await
            }
        })
        .await?;
        submitted.push(serde_json::json!({"operation":operation,"id":id,"confirmed":true,"observation_did_not_confirm":true}));
    }
    // A real server capability withdrawal invalidates the actual host guard.
    let cover = row(&store, &view, &request, ProDeviceKind::Cover)?;
    let held = admit(store.prepare(
        cover.token,
        cover.generation,
        0,
        Operation::CoverOpen.intent(),
    ))?;
    controls.command(Command::WithdrawCover, None).await?;
    let frame = tokio::time::timeout(Duration::from_secs(10), state_frames.recv())
        .await?
        .ok_or("withdrawal watch ended without resync")?;
    assert!(matches!(admit(live.apply(frame))?, FrameEffect::Resync(_)));
    assert!(live.current().is_none());
    watch_cancel.cancel();
    watch_job.await??;

    view = snapshot(&connection, &session, &cancel).await?;
    store.project(CachePhase::Live, Some(&view), &request);
    assert!(!(held.guard)());
    assert!(
        controller
            .client()
            .submit(held.request, held.guard)
            .is_err()
    );
    controls.command(Command::Rediscover, None).await?;
    view = snapshot(&connection, &session, &cancel).await?;
    let restored = row(&store, &view, &request, ProDeviceKind::Cover)?;
    assert_ne!(restored.generation, cover.generation);
    let held = admit(store.prepare(
        restored.token,
        restored.generation,
        0,
        Operation::CoverOpen.intent(),
    ))?;
    controls.command(Command::DenyWrite, None).await?;
    request = read_request(&connection, &session, &cancel).await?;
    view = snapshot(&connection, &session, &cancel).await?;
    let denied = row(&store, &view, &request, ProDeviceKind::Cover)?;
    assert_eq!(denied.access, ProControlAccess::Denied);
    assert!(!(held.guard)());
    assert!(matches!(
        store.prepare(
            denied.token,
            denied.generation,
            0,
            Operation::CoverOpen.intent()
        ),
        Err(Refusal::ReadOnly)
    ));
    // Deliberately bypass local UI admission to prove the ordinary authenticated
    // server also refuses this typed command. It must never reach the fake driver.
    let mut forged = held.request;
    let denied_id = admit(home_client_actions::CommandId::generate())?
        .as_str()
        .to_owned();
    forged.command_id = denied_id.clone();
    let lease = admit(session.access(&cancel).await)?;
    let refused = connection
        .actions()
        .submit_action(
            actions::SubmitActionRequest {
                command: forged.into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(10),
            &cancel,
        )
        .await
        .expect_err("server admitted an unauthorized typed command");
    assert!(matches!(
        refused.kind(),
        home_client_transport::FailureKind::Rpc(home_client_transport::RpcCode::PermissionDenied)
    ));
    controls.command(Command::AllowWrite, None).await?;
    controller.close().await;
    drain.await?;
    session.close().await;
    println!(
        "NHF009_RESULT {}",
        serde_json::json!({"pass":true,"profile":home_client_model::dashboard::Profile::Recipes.name(),"commands":submitted,"capability_withdrawal":true,"readonly_host_and_server_refusal":true,"denied_command_id":denied_id,"no_real_devices":true})
    );
    Ok(())
}
#[test]
#[ignore = "requires the explicit isolated Go NHF009 fixture and private stdin control harness"]
fn real_tls_secure_controls_and_profile2_catalogue() {
    let path =
        std::env::var_os("HOMEWARD_NHF009_READY").expect("explicit synthetic fixture required");
    let fixture: Fixture = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(run(fixture)).unwrap();
}
