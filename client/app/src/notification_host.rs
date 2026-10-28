//! Native-session-owned inbox. Server receipts reserve native attempts before
//! touching the OS; lost writes resolve by reads and never enter a retry queue.
mod native;
mod render;
mod state;
#[cfg(test)]
mod tests;
#[cfg(test)]
mod tls_probe;
mod worker;

use crate::{HomewardWindow, NotificationAction, NotificationRowAction};
use buffa_types::google::protobuf::Timestamp;
use home_client_platform::notifications as os;
use home_client_session::{AccessLease, Phase, SessionCoordinator};
use home_client_transport::{Connection, Failure, FailureKind, RpcCode};
use home_wire_contract::{
    auth_contract::{AccountId, SessionId},
    companion_contract as parent_admit,
    home::{companion::v1 as companion, notifications::v1 as wire},
    notifications_contract as admit,
};
use slint::ComponentHandle;
use state::{Grant, Intent, Job, Pending, Stage, State};
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant, SystemTime},
};
use tokio::{sync::Notify, task::JoinSet};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Copy)]
struct Ticket {
    scope: u64,
    fence: u64,
}
#[derive(Clone)]
pub(crate) struct Notifications(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    changed: Notify,
    projection_queued: AtomicBool,
    window: Option<slint::Weak<HomewardWindow>>,
    source: Arc<dyn native::Source>,
    session: Mutex<Option<(u64, SessionCoordinator)>>,
}
pub(crate) enum NavigationTarget {
    Home,
    Entity(home_client_model::EntityId),
}
fn now() -> Timestamp {
    let at = chrono::Utc::now();
    Timestamp {
        seconds: at.timestamp(),
        nanos: at.timestamp_subsec_nanos() as i32,
        ..Default::default()
    }
}
fn system(at: &Timestamp) -> Option<SystemTime> {
    SystemTime::UNIX_EPOCH.checked_add(Duration::new(
        at.seconds.try_into().ok()?,
        at.nanos.try_into().ok()?,
    ))
}
fn nonce() -> Result<String, worker::Error> {
    home_client_actions::CommandId::generate()
        .map(|id| id.as_str().to_owned())
        .map_err(|_| worker::Error::Invalid)
}
impl Notifications {
    fn new(window: Option<slint::Weak<HomewardWindow>>, source: Arc<dyn native::Source>) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            changed: Notify::new(),
            projection_queued: AtomicBool::new(false),
            window,
            source,
            session: Mutex::new(None),
        }))
    }
    #[cfg(test)]
    pub fn detached() -> Self {
        Self::new(None, Arc::new(native::Native))
    }
    pub fn bind(window: &HomewardWindow) -> Self {
        let this = Self::new(Some(window.as_weak()), Arc::new(native::Native));
        let owner = this.clone();
        window.on_notification_visible(move |visible| owner.visible(visible));
        let owner = this.clone();
        window.on_notification_action(move |generation, action| owner.action(generation, action));
        let owner = this.clone();
        window.on_notification_row_action(move |token, generation, action| {
            let mut state = owner.0.state.lock().expect("notification row action");
            if owner.authenticated(&state) {
                state.row_action(token, generation, action);
                owner.post(&state);
            }
            drop(state);
            owner.0.changed.notify_one();
        });
        this
    }
    fn authenticated(&self, state: &State) -> bool {
        self.0
            .session
            .lock()
            .expect("notification session")
            .as_ref()
            .is_some_and(|(scope, session)| {
                let phase = *session.subscribe().borrow();
                *scope == state.scope
                    && state.validated == Some(phase.generation)
                    && matches!(phase.phase, Phase::Ready | Phase::Refreshing)
            })
    }
    pub fn visible(&self, visible: bool) {
        let mut state = self.0.state.lock().expect("notification visibility");
        if state.visible != visible {
            state.visible = visible;
            state.draft = None;
            state.requested |= visible;
            state.bump();
            self.post(&state);
        }
        drop(state);
        self.0.changed.notify_one();
    }
    pub fn foreground(&self, foreground: bool) {
        let mut state = self.0.state.lock().expect("notification foreground");
        if state.foreground != foreground {
            state.foreground = foreground;
            state.interrupt();
            state.intent = None;
            state.requested = foreground;
            state.resolution_attempted = false;
            if !foreground {
                state.entries.clear();
                state.stage = Stage::Suspended;
            }
            self.post(&state);
        }
        drop(state);
        self.0.changed.notify_one();
    }
    pub fn clear(&self) {
        let mut state = self.0.state.lock().expect("notification clear");
        state.clear();
        self.post(&state);
        drop(state);
        self.0.changed.notify_one();
    }
    fn action(&self, generation: i32, action: NotificationAction) {
        let mut state = self.0.state.lock().expect("notification action");
        if self.authenticated(&state) {
            state.action(generation, action);
            self.post(&state);
        }
        drop(state);
        self.0.changed.notify_one();
    }
    fn post(&self, _state: &State) {
        let Some(window) = &self.0.window else {
            return;
        };
        if self.0.projection_queued.swap(true, Ordering::AcqRel) {
            return;
        }
        let owner = self.clone();
        if window
            .upgrade_in_event_loop(move |window| {
                let state = owner.0.state.lock().expect("notification projection");
                let projection = if state.active && !owner.authenticated(&state) {
                    render::empty(state.generation)
                } else {
                    render::project(&state)
                };
                // Read the latest complete state, not a snapshot per receipt. Reset
                // while holding its lock so a subsequent change queues one frame.
                owner.0.projection_queued.store(false, Ordering::Release);
                drop(state);
                render::apply(&window, projection);
            })
            .is_err()
        {
            self.0.projection_queued.store(false, Ordering::Release);
        }
    }
    pub fn take_navigation(&self, generation: i32) -> Option<NavigationTarget> {
        let mut state = self.0.state.lock().expect("notification navigation grant");
        #[cfg(debug_assertions)]
        eprintln!("homeward: notification grant UI callback");
        if state
            .grant
            .as_ref()
            .is_none_or(|grant| grant.tag != generation)
            || !state.active
            || !state.foreground
            || !self.authenticated(&state)
        {
            return None;
        }
        let grant = state.grant.take()?;
        if grant.parent_stop.is_cancelled() || state.validated != Some(grant.native_generation) {
            return None;
        }
        admit::bind_open_elapsed(
            &grant.response,
            state.context.as_ref()?,
            state.destination.as_ref()?,
            &grant.message,
            grant.requested_at.elapsed(),
        )
        .ok()?;
        let target = match grant.response.authorized_target.resource.as_ref()? {
            wire::target::Resource::Home(_) => NavigationTarget::Home,
            wire::target::Resource::Entity(id) => NavigationTarget::Entity(id.value.parse().ok()?),
        };
        // A pause or lost read response must not consume the short-lived native
        // intention. Clear it only when a freshly bound grant reaches the UI.
        state.finish_native_open(&admit::DeliveryId::parse(&grant.message.id.value).ok()?);
        Some(target)
    }
    pub async fn run(
        &self,
        session: SessionCoordinator,
        connection: Connection,
        parent: crate::companion_host::Companion,
        cancel: CancellationToken,
    ) {
        let scope = {
            let mut state = self.0.state.lock().expect("notification start");
            state.clear();
            state.scope = state.scope.saturating_add(1);
            state.scope
        };
        *self.0.session.lock().expect("notification session") = Some((scope, session.clone()));
        let mut phases = session.subscribe();
        let mut observed = None;
        let mut tasks = JoinSet::new();
        loop {
            let parent_binding = parent.notification_parent();
            let work = {
                let mut state = self.0.state.lock().expect("notification loop");
                if state.scope != scope {
                    break;
                }
                let phase = *phases.borrow();
                if observed != Some(phase.generation) {
                    observed = Some(phase.generation);
                    state.interrupt();
                    state.validated = None;
                    state.entries.clear();
                    state.requested =
                        state.visible || state.destination.is_some() || state.pending.is_some();
                    state.resolution_attempted = false;
                }
                let active = matches!(phase.phase, Phase::Ready | Phase::Refreshing);
                if !active && state.active {
                    state.clear();
                    self.post(&state);
                }
                if active && !state.active {
                    state.active = true;
                    state.stage = Stage::WaitingParent;
                    state.requested = state.visible;
                    self.post(&state);
                }
                if state.active && state.foreground {
                    if parent_binding.as_ref().is_some_and(|parent| {
                        state.destination.as_ref().is_some_and(|destination| {
                            destination.installation_id != parent.installation.id
                        })
                    }) {
                        state.interrupt();
                        state.destination = None;
                        state.parent = None;
                        state.context = None;
                        state.entries.clear();
                        state.inbox_revision = 0;
                        state.pending = None;
                        state.intent = None;
                        state.local_stopped = true;
                        state.requested = state.visible;
                    }
                    if state
                        .native_parent_stop
                        .as_ref()
                        .is_some_and(CancellationToken::is_cancelled)
                    {
                        state.stop_native();
                    }
                    if parent_binding.is_none()
                        && (state.native.is_some() || !state.entries.is_empty())
                    {
                        state.stop_native();
                        state.entries.clear();
                        state.grant = None;
                        state.stage = Stage::WaitingParent;
                        state.bump();
                        self.post(&state);
                    }
                    if state.visible && state.parent.is_none() && parent_binding.is_some() {
                        state.requested = true;
                    }
                    if state.expire() {
                        self.post(&state);
                    }
                }
                if tasks.is_empty() && (phase.phase == Phase::Ready || !state.cleanup.is_empty()) {
                    state.plan().map(|job| {
                        (
                            Ticket {
                                scope,
                                fence: state.fence,
                            },
                            state.cancel.clone(),
                            job,
                        )
                    })
                } else {
                    None
                }
            };
            if let Some((ticket, stop, job)) = work {
                let owner = self.clone();
                let source = session.clone();
                let api = connection.clone();
                let link = parent_binding.clone();
                tasks.spawn(async move {
                    owner.work(ticket, source, api, link, stop, job).await;
                });
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break,
                change=phases.changed()=>{if change.is_err(){break;}},
                ()=self.0.changed.notified()=>{},
                task=tasks.join_next(),if !tasks.is_empty()=>{if matches!(task,Some(Err(_))){let mut state=self.0.state.lock().expect("notification worker panic");if state.scope==scope{state.running=false;state.invalid();self.post(&state);}}},
                ()=tokio::time::sleep(Duration::from_millis(250))=>{},
            }
        }
        {
            let mut state = self.0.state.lock().expect("notification stop");
            if state.scope == scope {
                state.clear();
                self.post(&state);
            }
        }
        while tasks.join_next().await.is_some() {}
        let cleanup = {
            let mut state = self.0.state.lock().expect("notification cleanup");
            std::mem::take(&mut state.cleanup)
        };
        self.cleanup(cleanup).await;
        let mut current = self.0.session.lock().expect("notification session");
        if current.as_ref().is_some_and(|(id, _)| *id == scope) {
            *current = None;
        }
    }
    async fn cleanup(&self, ids: Vec<os::PresentationId>) {
        if ids.is_empty() {
            return;
        }
        let source = self.0.source.clone();
        let _ = tokio::task::spawn_blocking(move || {
            for id in ids {
                source.remove(&id);
            }
        })
        .await;
    }
}
