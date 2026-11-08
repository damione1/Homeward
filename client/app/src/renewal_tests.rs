//! Real session + cache-owner + Slint composition, with synthetic read I/O only.
use async_trait::async_trait;
use buffa_types::google::protobuf::Timestamp;
use home_client_cache::{CacheOwner, CachedView, FeedSource, SnapshotFailure, WatchExit};
use home_client_session::testing::{AuthApi, Clock};
use home_client_session::{AccessLease, SecureStore, SessionFailure, StoreFailure};
use home_client_transport::{AccessToken, Failure};
use home_wire_contract::home::api::v2 as wire;
use slint::{ComponentHandle, Model};
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicI64, Ordering},
};
use std::time::Duration;
use tokio::sync::{Notify, Semaphore, mpsc};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

struct Time(AtomicI64);
impl Clock for Time {
    fn now(&self) -> Result<Timestamp, SessionFailure> {
        Ok(Timestamp::from_unix_secs(self.0.load(Ordering::SeqCst)))
    }
}
struct Memory;
impl SecureStore for Memory {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(None)
    }
    fn save(&self, _: &[u8]) -> Result<(), StoreFailure> {
        Ok(())
    }
    fn clear(&self) -> Result<(), StoreFailure> {
        Ok(())
    }
}
struct Auth {
    clock: Arc<Time>,
    base: i64,
    refreshing: Notify,
    release: Semaphore,
}
impl Auth {
    fn tokens(&self, renewed: bool) -> wire::SessionTokens {
        let now = self.clock.0.load(Ordering::SeqCst);
        wire::SessionTokens {
            session: wire::Session {
                name: "homes/synthetic/accounts/test/sessions/test".into(),
                create_time: Timestamp::from_unix_secs(self.base - 1).into(),
                expire_time: Timestamp::from_unix_secs(now + 3600).into(),
                authentication_time: Timestamp::from_unix_secs(self.base - 1).into(),
                authentication_satisfaction: wire::AuthenticationSatisfaction::PasswordOnly.into(),
                ..Default::default()
            }
            .into(),
            audience: wire::CredentialAudience::Interactive.into(),
            access_token: if renewed {
                "synthetic-new"
            } else {
                "synthetic-old"
            }
            .into(),
            refresh_token: if renewed {
                "renewal-new"
            } else {
                "renewal-old"
            }
            .into(),
            access_expire_time: Timestamp::from_unix_secs(now + 120).into(),
            ..Default::default()
        }
    }
}
#[async_trait]
impl AuthApi for Auth {
    async fn revoke(
        &self,
        _: wire::RevokeSessionRequest,
        _: &AccessToken,
        _: &CancellationToken,
    ) -> Result<(), Failure> {
        panic!("no revoke I/O")
    }
    async fn start_login(
        &self,
        _: wire::StartLoginRequest,
        _: &CancellationToken,
    ) -> Result<wire::StartLoginResponse, Failure> {
        Ok(wire::StartLoginResponse {
            decision: wire::LoginDecision {
                outcome: Some(wire::login_decision::Outcome::Authenticated(Box::new(
                    self.tokens(false),
                ))),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })
    }
    async fn complete_login(
        &self,
        _: wire::CompleteLoginRequest,
        _: &CancellationToken,
    ) -> Result<wire::CompleteLoginResponse, Failure> {
        panic!("no factor I/O")
    }
    async fn preflight_refresh(
        &self,
        _: wire::ListSessionsRequest,
        _: &AccessToken,
        _: &CancellationToken,
    ) -> Result<(), Failure> {
        Ok(())
    }
    async fn refresh(
        &self,
        _: wire::RefreshSessionRequest,
        cancel: &CancellationToken,
    ) -> Result<wire::RefreshSessionResponse, Failure> {
        self.refreshing.notify_one();
        tokio::select! {
            () = cancel.cancelled() => return Err(connectrpc::ConnectError::canceled("cancelled").into()),
            permit = self.release.acquire() => permit.unwrap().forget(),
        }
        Ok(wire::RefreshSessionResponse {
            tokens: self.tokens(true).into(),
            ..Default::default()
        })
    }
}
struct Reads {
    captures: tokio::sync::Mutex<mpsc::UnboundedReceiver<CachedView>>,
    watching: Notify,
}
#[async_trait]
impl FeedSource for Reads {
    async fn capture(
        &self,
        lease: &AccessLease,
        cancel: &CancellationToken,
    ) -> Result<CachedView, SnapshotFailure> {
        tokio::select! {
            () = cancel.cancelled() => Err(SnapshotFailure::Cancelled),
            () = lease.cancellation.cancelled() => Err(SnapshotFailure::Cancelled),
            view = async { self.captures.lock().await.recv().await } => Ok(view.unwrap()),
        }
    }
    async fn watch(
        &self,
        lease: &AccessLease,
        _view: CachedView,
        cancel: &CancellationToken,
        _: &mut (dyn for<'view> FnMut(&'view CachedView) + Send),
    ) -> WatchExit {
        self.watching.notify_one();
        tokio::select! { () = cancel.cancelled() => {}, () = lease.cancellation.cancelled() => {} }
        WatchExit::Cancelled
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum ControlFixture {
    Power,
    Dimmer,
    Alarm,
}
fn capture(control: ControlFixture) -> CachedView {
    use crate::device_view::fixture;
    if control == ControlFixture::Alarm {
        return fixture::capture(vec![fixture::entry(
            "homes/synthetic/entities/private-entity-0002",
            "Alarm",
            vec![fixture::capability(
                "homes/synthetic/entities/private-entity-0002/capabilities/power",
                wire::capability::Trait::Alarm(Box::new(wire::AlarmCapability {
                    definition: wire::AlarmDefinition {
                        operations: vec![
                            wire::AlarmOperation::Disarm.into(),
                            wire::AlarmOperation::ArmAway.into(),
                        ],
                        code_required_operations: vec![wire::AlarmOperation::Disarm.into()],
                        observed_states: vec![wire::AlarmState::Disarmed.into()],
                        ..Default::default()
                    }
                    .into(),
                    observation: wire::AlarmObservation {
                        value: wire::AlarmValue {
                            state: wire::AlarmState::Disarmed.into(),
                            ..Default::default()
                        }
                        .into(),
                        quality: fixture::quality().into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                })),
            )],
        )]);
    }
    if control == ControlFixture::Dimmer {
        return fixture::capture(vec![fixture::entry(
            "homes/synthetic/entities/private-entity-0002",
            "Lamp",
            vec![fixture::capability(
                "homes/synthetic/entities/private-entity-0002/capabilities/power",
                wire::capability::Trait::LightControl(Box::new(wire::LightControlCapability {
                    supports_power: true,
                    brightness_percent: wire::NumberBounds {
                        minimum: 0.,
                        maximum: 100.,
                        step: Some(1.),
                        ..Default::default()
                    }
                    .into(),
                    observation: wire::LightControlObservation {
                        on: Some(true),
                        brightness_percent: Some(75.),
                        quality: fixture::quality().into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                })),
            )],
        )]);
    }
    let mut power = fixture::power(
        "homes/synthetic/entities/private-entity-0002/capabilities/power",
        Some(wire::PowerValue::Off),
    );
    let Some(wire::capability::Trait::Power(control)) = power.r#trait.as_mut() else {
        unreachable!()
    };
    control.supports_set = true;
    fixture::capture(vec![fixture::entry(
        "homes/synthetic/entities/private-entity-0002",
        "Lamp",
        vec![power],
    )])
}

fn reader_capture(entity: &str, expires: Timestamp) -> CachedView {
    use home_client_cache::{Filter, SnapshotCapture};
    let name = home_client_model::api::EntityName::try_from(entity).unwrap();
    let now = Timestamp::from_unix_secs(chrono::Utc::now().timestamp());
    let mut capture = SnapshotCapture::begin(
        name.home(),
        Filter::all(),
        256,
        wire::EntityView {
            name: format!("{}/entityViews/reader-fixture", name.home().as_str()),
            create_time: now.clone().into(),
            expire_time: expires.into(),
            ..Default::default()
        },
        &now,
    )
    .unwrap();
    capture
        .push(
            wire::QueryEntityViewResponse {
                entries: vec![crate::device_view::fixture::entry(
                    entity,
                    "Synthetic reader",
                    vec![],
                )],
                resume_token: "synthetic-reader-checkpoint".into(),
                ..Default::default()
            },
            &now,
        )
        .unwrap();
    capture.finish(&now).unwrap()
}

/// Real credential coordinator shared by the reader boundary regressions.
pub(crate) struct Credentials {
    pub session: home_client_session::SessionCoordinator,
    clock: Arc<Time>,
    auth: Arc<Auth>,
}
impl Credentials {
    pub async fn new() -> Self {
        let base = chrono::Utc::now().timestamp();
        let clock = Arc::new(Time(AtomicI64::new(base)));
        let auth = Arc::new(Auth {
            clock: clock.clone(),
            base,
            refreshing: Notify::new(),
            release: Semaphore::new(0),
        });
        let session = home_client_session::testing::coordinator(
            auth.clone(),
            Arc::new(Memory),
            clock.clone(),
        );
        session
            .login("synthetic".into(), Zeroizing::new("synthetic".into()))
            .await
            .unwrap();
        Self {
            session,
            clock,
            auth,
        }
    }
    pub fn expire_access(&self) {
        self.clock.0.fetch_add(121, Ordering::SeqCst);
    }
    pub async fn wait_refresh(&self) {
        tokio::time::timeout(Duration::from_secs(5), self.auth.refreshing.notified())
            .await
            .unwrap();
    }
    pub fn release_refresh(&self) {
        self.auth.release.add_permits(1);
    }
    pub async fn renew(&self) {
        self.expire_access();
        self.release_refresh();
        self.session
            .access(&CancellationToken::new())
            .await
            .unwrap();
    }
    /// Bind through the same cache owner as production, then drop the capture
    /// and owner. The returned object contains only weak presentation metadata.
    pub async fn source(&self, entity: &str) -> home_client_cache::PresentationSource {
        let view = self
            .captured(reader_capture(
                entity,
                Timestamp::from_unix_secs(chrono::Utc::now().timestamp() + 60),
            ))
            .await;
        home_client_cache::PresentationSource::from_view(&view).unwrap()
    }
    pub async fn captured(&self, view: CachedView) -> CachedView {
        let (sender, receiver) = mpsc::unbounded_channel();
        sender.send(view).unwrap_or_else(|_| panic!("source alive"));
        let reads = Arc::new(Reads {
            captures: tokio::sync::Mutex::new(receiver),
            watching: Notify::new(),
        });
        let (sender, mut receiver) = mpsc::unbounded_channel();
        let (owner, _) = CacheOwner::start_test_source(
            self.session.clone(),
            reads,
            |_, view, _, _| view.cloned(),
            move |ticket| {
                let _ = sender.send(ticket);
            },
        );
        owner.handle().activate().unwrap();
        let mut source = None;
        tokio::time::timeout(Duration::from_secs(5), async {
            while source.is_none() {
                receiver
                    .recv()
                    .await
                    .unwrap()
                    .deliver(|_, value| source = value);
            }
        })
        .await
        .unwrap();
        owner.close().await;
        source.unwrap()
    }
}

/// An isolated socket counts even attempted TLS connections. It serves no API
/// and is never a real installation; zero accepts proves no reader RPC began.
pub(crate) async fn counted_transport() -> (
    home_client_transport::Connection,
    Arc<std::sync::atomic::AtomicUsize>,
    tokio::task::JoinHandle<()>,
) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let connection = home_client_transport::Connection::new(
        &format!("https://{}", listener.local_addr().unwrap()),
        include_bytes!("../tests/fixtures/trust-a.pem"),
    )
    .unwrap();
    let count = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let observed = count.clone();
    let task = tokio::spawn(async move {
        while let Ok((stream, _)) = listener.accept().await {
            observed.fetch_add(1, Ordering::SeqCst);
            drop(stream);
        }
    });
    (connection, count, task)
}

#[tokio::test]
async fn renewal_owner_expires_without_ui_delivery_and_never_extends_the_old_deadline() {
    let credentials = Credentials::new().await;
    let expires = chrono::Utc::now() + chrono::Duration::seconds(2);
    let deadline = (expires.timestamp(), expires.timestamp_subsec_nanos() as i32);
    let view = reader_capture(
        "homes/synthetic/entities/reader",
        Timestamp {
            seconds: deadline.0,
            nanos: deadline.1,
            ..Default::default()
        },
    );
    let (sender, receiver) = mpsc::unbounded_channel();
    sender.send(view).unwrap_or_else(|_| panic!("source alive"));
    let reads = Arc::new(Reads {
        captures: tokio::sync::Mutex::new(receiver),
        watching: Notify::new(),
    });
    let (events, mut projected) = mpsc::unbounded_channel();
    let (owner, _) = CacheOwner::start_test_source(
        credentials.session.clone(),
        reads.clone(),
        move |phase, view, _, renewal| {
            let _ = events.send((phase, view.is_some(), renewal.map(|p| p.deadline())));
        },
        |_ticket| { /* No UI delivery at all. */ },
    );
    owner.handle().activate().unwrap();
    tokio::time::timeout(Duration::from_secs(2), reads.watching.notified())
        .await
        .unwrap();
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let (phase, captured, retained) = projected.recv().await.unwrap();
            if phase == home_client_cache::CachePhase::Live {
                assert!(captured && retained.is_none());
                break;
            }
            assert_eq!(phase, home_client_cache::CachePhase::Loading);
            assert!(!captured && retained.is_none());
        }
    })
    .await
    .unwrap();
    credentials.renew().await;
    loop {
        let (phase, captured, retained) =
            tokio::time::timeout(Duration::from_secs(2), projected.recv())
                .await
                .unwrap()
                .unwrap();
        if phase == home_client_cache::CachePhase::Renewing {
            assert!(!captured);
            assert_eq!(retained, Some(deadline));
            break;
        }
    }
    credentials.renew().await;
    tokio::time::timeout(Duration::from_secs(4), async {
        loop {
            let (phase, captured, retained) = projected.recv().await.unwrap();
            assert!(!captured);
            if phase == home_client_cache::CachePhase::Renewing {
                assert_eq!(retained, Some(deadline), "another renewal grants no grace");
            } else if phase == home_client_cache::CachePhase::Loading {
                assert!(retained.is_none());
                assert!(chrono::Utc::now() >= expires);
                break;
            }
        }
    })
    .await
    .unwrap();
    owner.close().await;
    credentials.session.close().await;
}

#[tokio::test]
async fn credential_renewal_keeps_home_pixels_through_the_real_owner() {
    renewal_composition(false, ControlFixture::Power).await;
}
#[tokio::test]
async fn credential_renewal_keeps_detail_pixels_through_the_real_owner() {
    renewal_composition(true, ControlFixture::Power).await;
}
#[tokio::test]
async fn credential_renewal_keeps_one_light_dimmer_at_observed_seventy_five_percent() {
    renewal_composition(true, ControlFixture::Dimmer).await;
}
#[tokio::test]
async fn keypad_survives_credential_renewal_and_unjoined_capture_through_real_owner() {
    renewal_composition(true, ControlFixture::Alarm).await;
}
async fn renewal_composition(detail: bool, control: ControlFixture) {
    let dimmer = control == ControlFixture::Dimmer;
    let alarm = control == ControlFixture::Alarm;
    slint::platform::set_platform(Box::new(i_slint_backend_testing::TestingBackend::new(
        i_slint_backend_testing::TestingBackendOptions {
            mock_time: true,
            renderer_name: Some("software".into()),
            ..Default::default()
        },
    )))
    .unwrap();
    let window = crate::create_window().unwrap();
    crate::cache_view::bind(&window);
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    window
        .window()
        .set_size(slint::LogicalSize::new(1100., 1600.));
    window.show().unwrap();
    let base = chrono::Utc::now().timestamp();
    let clock = Arc::new(Time(AtomicI64::new(base)));
    let auth = Arc::new(Auth {
        clock: clock.clone(),
        base,
        refreshing: Notify::new(),
        release: Semaphore::new(0),
    });
    let session =
        home_client_session::testing::coordinator(auth.clone(), Arc::new(Memory), clock.clone());
    session
        .login("synthetic".into(), Zeroizing::new("synthetic".into()))
        .await
        .unwrap();
    crate::session_host::render_view(&window, *session.subscribe().borrow());
    let old_access = tokio::time::timeout(
        Duration::from_secs(5),
        session.access(&CancellationToken::new()),
    )
    .await
    .unwrap()
    .unwrap();
    let first = capture(control);
    let capture_expiry = first.expires().seconds;
    let request = Arc::new(Mutex::new(crate::dashboard::ProjectionRequest {
        dashboard: crate::dashboard::LayoutState::Ready(Arc::new(
            crate::dashboard::joined_fixture(&first),
        )),
        ..Default::default()
    }));
    let devices = crate::device_view::DeviceStore::default();
    let retained = devices.clone();
    let _navigation = crate::navigation::bind_devices(
        &window,
        request.clone(),
        || {},
        move || Some(retained.clone()),
    );
    let (sender, receiver) = mpsc::unbounded_channel();
    let reads = Arc::new(Reads {
        captures: tokio::sync::Mutex::new(receiver),
        watching: Notify::new(),
    });
    sender
        .send(first)
        .unwrap_or_else(|_| panic!("read source available"));
    let (tickets, mut publications) = mpsc::unbounded_channel();
    let admitted = Arc::new(Mutex::new(None::<CachedView>));
    let projected_capture = admitted.clone();
    let projected = devices.clone();
    let projected_request = request.clone();
    let (owner, _events) = CacheOwner::start_test_source(
        session.clone(),
        reads.clone(),
        move |phase, view, viewport, renewal| {
            let mut current = projected_request.lock().unwrap();
            if let Some(view) = view {
                *projected_capture.lock().unwrap() = Some(view.clone());
                if current
                    .dashboard
                    .layout()
                    .is_some_and(|layout| layout.source.is_none())
                {
                    current.dashboard = crate::dashboard::LayoutState::Ready(Arc::new(
                        crate::dashboard::joined_fixture(view),
                    ));
                }
            }
            crate::cache_view::project_owner(phase, view, viewport, &current, &projected, renewal)
        },
        move |ticket| {
            tickets.send(ticket).unwrap_or_else(|_| panic!("UI alive"));
        },
    );
    owner.handle().activate().unwrap();
    tokio::time::timeout(Duration::from_secs(5), reads.watching.notified())
        .await
        .unwrap();
    publications
        .recv()
        .await
        .unwrap()
        .deliver(|generation, projection| {
            crate::cache_view::render(&window, generation, projection);
        });
    slint::platform::update_timers_and_animations();
    if detail {
        let row = window
            .get_home_layout_nodes()
            .iter()
            .find(|node| {
                node.device.can_power || alarm && node.device.kind == crate::ProDeviceKind::Alarm
            })
            .unwrap()
            .device;
        window.invoke_device_details(row.token, row.generation);
        assert_eq!(window.get_page(), crate::ViewPage::Detail);
        owner
            .handle()
            .viewport(home_client_cache::Viewport::default())
            .unwrap();
        tokio::time::timeout(Duration::from_secs(5), publications.recv())
            .await
            .unwrap()
            .unwrap()
            .deliver(|generation, projection| {
                crate::cache_view::render(&window, generation, projection);
            });
        slint::platform::update_timers_and_animations();
    }
    snapshot(
        &window,
        if dimmer {
            "light75-before"
        } else if detail {
            "detail-before"
        } else {
            "before"
        },
    );
    if dimmer {
        assert_light75(&window);
    }
    let panels = window.get_detail_panels();
    let detail_title = window.get_detail_title();
    if detail {
        assert!(panels.row_count() > 0);
    }
    let nodes = window.get_home_layout_nodes();
    let count = nodes.row_count();
    let title = window.get_dashboard_title();
    let row = if detail {
        window
            .get_device_cards()
            .iter()
            .find(|row| row.can_power || alarm && row.kind == crate::ProDeviceKind::Alarm)
            .unwrap()
    } else {
        nodes
            .iter()
            .find(|node| {
                node.device.can_power || alarm && node.device.kind == crate::ProDeviceKind::Alarm
            })
            .unwrap()
            .device
    };
    let old_intent = devices
        .prepare(
            row.token,
            row.generation,
            window.get_projection_version(),
            if alarm {
                crate::device_view::DeviceIntent::Alarm(wire::AlarmOperation::ArmAway)
            } else {
                crate::device_view::DeviceIntent::Power(if dimmer {
                    crate::device_view::Power::Off
                } else {
                    crate::device_view::Power::On
                })
            },
        )
        .unwrap();
    assert!((old_intent.guard)());
    if alarm {
        devices
            .alarm_digit(
                row.token,
                row.generation,
                window.get_projection_version(),
                Some(0),
            )
            .unwrap();
        owner
            .handle()
            .viewport(home_client_cache::Viewport::default())
            .unwrap();
        publications
            .recv()
            .await
            .unwrap()
            .deliver(|generation, projection| {
                crate::cache_view::render(&window, generation, projection);
            });
        assert_eq!(
            window
                .get_detail_panels()
                .row_data(0)
                .unwrap()
                .alarm
                .entered_count,
            1
        );
    }
    let panels = window.get_detail_panels();
    assert!(
        count > 0,
        "real Home composition starts with server-defined nodes"
    );
    assert!(
        capture_expiry > chrono::Utc::now().timestamp(),
        "credential clock does not age the capture"
    );
    clock.0.store(base + 121, Ordering::SeqCst);
    let refreshing = session.clone();
    let renewal = tokio::spawn(async move { refreshing.access(&CancellationToken::new()).await });
    tokio::time::timeout(Duration::from_secs(5), auth.refreshing.notified())
        .await
        .unwrap();
    crate::session_host::render_view(&window, *session.subscribe().borrow());
    assert!(old_access.cancellation.is_cancelled());
    assert!(
        !(old_intent.guard)(),
        "old intent is revoked before delivering any UI ticket"
    );
    let ticket = tokio::time::timeout(Duration::from_secs(5), publications.recv())
        .await
        .unwrap()
        .unwrap();
    ticket.deliver(|generation, projection| {
        crate::cache_view::render(&window, generation, projection);
    });
    slint::platform::update_timers_and_animations();
    snapshot(
        &window,
        if dimmer {
            "light75-renewing"
        } else if detail {
            "detail-renewing"
        } else {
            "renewing"
        },
    );
    if detail {
        assert_eq!(window.get_page(), crate::ViewPage::Detail);
        assert_eq!(window.get_detail_title(), detail_title);
        assert_eq!(window.get_detail_panels(), panels);
    }
    assert_eq!(
        window.get_home_layout_nodes().row_count(),
        count,
        "credential renewal must not replace Home with Loading states"
    );
    assert!(!window.get_commands_ready());
    if dimmer {
        assert_light75(&window);
    }
    assert_eq!(window.get_dashboard_title(), title);
    assert!(window.get_has_configured_layout());
    assert!(std::ptr::eq(
        nodes.as_any(),
        window.get_home_layout_nodes().as_any()
    ));
    auth.release.add_permits(1);
    renewal.await.unwrap().unwrap();
    crate::session_host::render_view(&window, *session.subscribe().borrow());
    assert!(
        !(old_intent.guard)(),
        "a new credential does not rehabilitate an old intent"
    );
    while let Ok(ticket) = publications.try_recv() {
        ticket.deliver(|generation, projection| {
            crate::cache_view::render(&window, generation, projection);
        });
        assert_eq!(window.get_home_layout_nodes().row_count(), count);
    }
    let next = capture(control);
    sender
        .send(next.clone())
        .unwrap_or_else(|_| panic!("read source available"));
    tokio::time::timeout(Duration::from_secs(5), reads.watching.notified())
        .await
        .unwrap();
    publications
        .recv()
        .await
        .unwrap()
        .deliver(|generation, projection| {
            crate::cache_view::render(&window, generation, projection);
        });
    assert_eq!(window.get_home_layout_nodes().row_count(), count);
    assert!(
        !window.get_commands_ready(),
        "capture without its fresh catalogue is still inert"
    );
    request.lock().unwrap().dashboard = crate::dashboard::LayoutState::Ready(Arc::new(
        crate::dashboard::joined_fixture(admitted.lock().unwrap().as_ref().unwrap()),
    ));
    owner
        .handle()
        .viewport(home_client_cache::Viewport::default())
        .unwrap();
    tokio::time::timeout(Duration::from_secs(5), publications.recv())
        .await
        .unwrap()
        .unwrap()
        .deliver(|generation, projection| {
            crate::cache_view::render(&window, generation, projection);
        });
    snapshot(
        &window,
        if dimmer {
            "light75-rejoined"
        } else if detail {
            "detail-rejoined"
        } else {
            "rejoined"
        },
    );
    if detail {
        assert_eq!(window.get_page(), crate::ViewPage::Detail);
        assert_eq!(window.get_detail_title(), detail_title);
        assert_eq!(window.get_detail_panels().row_count(), panels.row_count());
    }
    assert_eq!(window.get_home_layout_nodes().row_count(), count);
    assert_eq!(window.get_dashboard_title(), title);
    assert!(window.get_commands_ready());
    if dimmer {
        assert_light75(&window);
    }
    assert!(!(old_intent.guard)());
    if alarm {
        let panel = window.get_detail_panels().row_data(0).unwrap();
        assert_eq!(
            panel.alarm.entered_count, 1,
            "nominal renewal must preserve the masked draft"
        );
        assert!(
            panel.alarm.can_confirm,
            "protected operation selection must survive too"
        );
        let row = panel.device;
        devices
            .alarm_digit(
                row.token,
                row.generation,
                window.get_projection_version(),
                Some(7),
            )
            .unwrap();
        assert!(
            devices
                .alarm_confirm(row.token, row.generation, window.get_projection_version())
                .is_ok()
        );
    }
    owner.close().await;
    session.close().await;
}

fn assert_light75(window: &crate::HomewardWindow) {
    let panels = window.get_detail_panels();
    assert_eq!(
        panels.row_count(),
        1,
        "one cohesive light panel, not three editors"
    );
    let panel = panels.row_data(0).unwrap();
    assert!(panel.device.can_brightness && panel.light.brightness_known);
    assert_eq!(panel.light.brightness, 75.);
    assert!(panel.device.power_known && panel.device.power_on);
}

fn snapshot(window: &crate::HomewardWindow, name: &str) {
    let pixels = window.window().take_snapshot().unwrap();
    if let Some(directory) = std::env::var_os("HOMEWARD_RENEWAL_RENDER_DIR") {
        let directory = std::path::PathBuf::from(directory);
        std::fs::create_dir_all(&directory).unwrap();
        let file = std::fs::File::create(directory.join(format!("{name}.png"))).unwrap();
        let mut encoder = png::Encoder::new(file, pixels.width(), pixels.height());
        encoder.set_color(png::ColorType::Rgba);
        encoder.set_depth(png::BitDepth::Eight);
        encoder
            .write_header()
            .unwrap()
            .write_image_data(pixels.as_bytes())
            .unwrap();
    }
}

#[tokio::test]
async fn keypad_suspension_erases_input_before_coalesced_resume_publications() {
    use crate::{
        dashboard::{LayoutState, ProjectionRequest},
        device_view::DeviceStore,
    };
    let credentials = Credentials::new().await;
    let first = capture(ControlFixture::Alarm);
    let request = Arc::new(Mutex::new(ProjectionRequest {
        dashboard: LayoutState::Ready(Arc::new(crate::dashboard::joined_fixture(&first))),
        ..Default::default()
    }));
    let devices = DeviceStore::default();
    let projected = devices.clone();
    let (sender, receiver) = mpsc::unbounded_channel();
    sender
        .send(first)
        .unwrap_or_else(|_| panic!("synthetic source alive"));
    let reads = Arc::new(Reads {
        captures: tokio::sync::Mutex::new(receiver),
        watching: Notify::new(),
    });
    let (tickets, mut publications) = mpsc::unbounded_channel();
    let (owner, _) = CacheOwner::start_test_source(
        credentials.session.clone(),
        reads.clone(),
        move |phase, view, _, renewal| {
            let mut request = request.lock().unwrap();
            if let Some(view) = view {
                request.dashboard =
                    LayoutState::Ready(Arc::new(crate::dashboard::joined_fixture(view)));
            }
            projected.project_owner(phase, view, &request, renewal)
        },
        move |ticket| {
            tickets
                .send(ticket)
                .unwrap_or_else(|_| panic!("publication receiver alive"));
        },
    );
    owner.handle().activate().unwrap();
    tokio::time::timeout(Duration::from_secs(5), reads.watching.notified())
        .await
        .unwrap();
    let mut before = None;
    while before.is_none() {
        tokio::time::timeout(Duration::from_secs(5), publications.recv())
            .await
            .unwrap()
            .unwrap()
            .deliver(|_, value| {
                before = value
                    .rows
                    .into_iter()
                    .find(|row| row.kind == crate::ProDeviceKind::Alarm);
            });
    }
    let before = before.unwrap();
    devices
        .alarm_digit(before.token, before.generation, 0, Some(0))
        .unwrap();
    assert!(
        devices
            .alarm_confirm(before.token, before.generation, 0)
            .is_ok()
    );
    // LifecycleBridge uses this synchronous invalidation before enqueueing the
    // native event. Resume immediately, without consuming a single UI ticket.
    owner.handle().invalidate().unwrap();
    assert!(
        devices
            .alarm_confirm(before.token, before.generation, 0)
            .is_err()
    );
    owner.handle().activate().unwrap();
    sender
        .send(capture(ControlFixture::Alarm))
        .unwrap_or_else(|_| panic!("source alive"));
    tokio::time::timeout(Duration::from_secs(5), reads.watching.notified())
        .await
        .unwrap();
    let mut after = None;
    while after.is_none() {
        tokio::time::timeout(Duration::from_secs(5), publications.recv())
            .await
            .unwrap()
            .unwrap()
            .deliver(|_, value| {
                after = value
                    .rows
                    .into_iter()
                    .find(|row| row.kind == crate::ProDeviceKind::Alarm);
            });
    }
    let after = after.unwrap();
    assert_ne!(after.generation, before.generation);
    assert!(
        devices
            .alarm_confirm(after.token, after.generation, 0)
            .is_err(),
        "missed Idle publication cannot resurrect the code"
    );
    owner.close().await;
    credentials.session.close().await;
}
