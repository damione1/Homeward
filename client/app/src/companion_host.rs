//! A session-owned foreground companion. UI visibility only controls its
//! projection; sharing never depends on keeping the settings screen open.
mod native;
mod render;
mod state;
#[cfg(test)]
mod tests;
#[cfg(test)]
mod tls_probe;
mod worker;
#[derive(Clone, Copy)]
struct Ticket {
    scope: u64,
    fence: u64,
}

use crate::{HomewardWindow, ProAdminAction};
use home_client_session::{Phase, SessionCoordinator};
use home_client_transport::{CompanionTransport, Failure, FailureKind, RpcCode};
use home_wire_contract::{
    auth_contract::{AccountId, SessionId},
    companion_contract as admit,
    home::companion::v1 as wire,
};
use slint::ComponentHandle;
use state::{Draft, Gate, Identity, Intent, Job, Pending, Stage, State, Write};
use std::{
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio::{sync::Notify, task::JoinSet};
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(crate) struct NotificationParent {
    pub context: wire::Context,
    pub installation: wire::Installation,
    pub cancellation: CancellationToken,
}
#[derive(Clone, Copy)]
pub(crate) enum NotificationPresence {
    Visible,
    ForegroundService,
}
#[derive(Clone)]
pub(crate) struct Companion(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    changed: Notify,
    window: Option<slint::Weak<HomewardWindow>>,
    source: Arc<dyn native::Source>,
    session: Mutex<Option<(u64, SessionCoordinator)>>,
}
impl Companion {
    #[cfg(test)]
    pub fn detached() -> Self {
        Self::new(None, Arc::new(native::Native))
    }
    fn new(window: Option<slint::Weak<HomewardWindow>>, source: Arc<dyn native::Source>) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            changed: Notify::new(),
            window,
            source,
            session: Mutex::new(None),
        }))
    }
    fn authenticated(&self, state: &State) -> bool {
        self.0
            .session
            .lock()
            .expect("companion session")
            .as_ref()
            .is_some_and(|(scope, source)| {
                let view = *source.subscribe().borrow();
                *scope == state.scope
                    && state.validated == Some(view.generation)
                    && matches!(view.phase, Phase::Ready | Phase::Refreshing)
            })
    }
    pub(crate) fn notification_parent(&self) -> Option<NotificationParent> {
        self.notification_parent_for(NotificationPresence::Visible)
    }
    /// Installation metadata only: the notification owner must hold a live OS
    /// foreground-service lease and re-read this installation over TLS. This
    /// does not authorize companion collection while the UI is suspended.
    pub(crate) fn notification_parent_for(
        &self,
        presence: NotificationPresence,
    ) -> Option<NotificationParent> {
        let state = self.0.state.lock().expect("companion notification parent");
        let session_live = match presence {
            NotificationPresence::Visible => self.authenticated(&state),
            NotificationPresence::ForegroundService => self
                .0
                .session
                .lock()
                .expect("companion notification metadata session")
                .as_ref()
                .is_some_and(|(scope, session)| {
                    let phase = *session.subscribe().borrow();
                    *scope == state.scope && matches!(phase.phase, Phase::Ready | Phase::Refreshing)
                }),
        };
        if !state.active
            || (!state.foreground && matches!(presence, NotificationPresence::Visible))
            || matches!(&state.draft, Some(Draft::Revoke { .. }))
            || state.notification_retired
            || state.notification_cancel.is_cancelled()
            || !session_live
        {
            return None;
        }
        let installation = state.record.as_ref()?;
        if installation.state != wire::InstallationState::Active
            || installation.expires_at.seconds <= chrono::Utc::now().timestamp()
        {
            return None;
        }
        Some(NotificationParent {
            context: state.context.clone()?,
            installation: installation.clone(),
            cancellation: state.notification_cancel.clone(),
        })
    }
    #[cfg(test)]
    pub(crate) fn notification_fixture_action(&self, action: ProAdminAction, editor: bool) -> bool {
        let state = self.0.state.lock().expect("companion fixture action");
        if state.context.is_none() || state.running {
            return false;
        }
        let generation = state.generation;
        drop(state);
        self.action(generation, action, editor);
        true
    }
    pub fn bind(window: &HomewardWindow) -> Self {
        let this = Self::new(Some(window.as_weak()), Arc::new(native::Native));
        let owner = this.clone();
        window.on_companion_visible(move |visible| owner.visible(visible));
        let owner = this.clone();
        window.on_companion_action(move |_, generation, action| {
            owner.action(generation, action, false)
        });
        let owner = this.clone();
        window.on_companion_editor_action(move |_, generation, action| {
            owner.action(generation, action, true)
        });
        let owner = this.clone();
        window.on_companion_choose(move |_, generation, field, option| {
            let mut state = owner.0.state.lock().expect("companion state");
            state.choose(generation, field, option);
            owner.post(&state);
            drop(state);
            owner.0.changed.notify_one();
        });
        let owner = this.clone();
        window.on_companion_select(move |token, generation| {
            let mut state = owner.0.state.lock().expect("companion state");
            if generation == state.generation && (1..=14).contains(&token) {
                state.selected = (token - 1) as usize;
                owner.post(&state);
            }
        });
        let owner = this.clone();
        window.on_companion_navigate(move |step| {
            let mut state = owner.0.state.lock().expect("companion state");
            state.selected = match step {
                crate::NavigationStep::First => 0,
                crate::NavigationStep::Last => 13,
                crate::NavigationStep::Next => state.selected.saturating_add(1).min(13),
                crate::NavigationStep::Previous => state.selected.saturating_sub(1),
                crate::NavigationStep::PageNext => state.selected.saturating_add(5).min(13),
                crate::NavigationStep::PagePrevious => state.selected.saturating_sub(5),
            };
            owner.post(&state);
        });
        this
    }
    pub fn visible(&self, visible: bool) {
        let mut state = self.0.state.lock().expect("companion state");
        if state.visible == visible {
            return;
        }
        state.visible = visible;
        state.bump();
        if visible {
            state.requested = true;
        } else {
            state.draft = None;
        }
        self.post(&state);
        drop(state);
        self.0.changed.notify_one();
    }
    pub fn foreground(&self, foreground: bool) {
        let mut state = self.0.state.lock().expect("companion state");
        state.foreground(foreground);
        self.post(&state);
        drop(state);
        self.0.changed.notify_one();
    }
    pub fn clear(&self) {
        let mut state = self.0.state.lock().expect("companion state");
        state.clear();
        self.post(&state);
        drop(state);
        self.0.changed.notify_one();
    }
    fn action(&self, generation: i32, action: ProAdminAction, editor: bool) {
        let mut state = self.0.state.lock().expect("companion state");
        if !self.authenticated(&state) {
            return;
        }
        state.action(generation, action, editor);
        self.post(&state);
        drop(state);
        self.0.changed.notify_one();
    }
    fn post(&self, state: &State) {
        let Some(window) = &self.0.window else {
            return;
        };
        let projection = render::project(state);
        let (scope, fence, generation) = (state.scope, state.fence, state.generation);
        let owner = self.clone();
        let _ = window.upgrade_in_event_loop(move |window| {
            let state = owner.0.state.lock().expect("companion projection");
            if (state.scope, state.fence, state.generation) != (scope, fence, generation) {
                return;
            }
            // A queued model cannot outlive the native credential context.
            let allowed = owner.authenticated(&state);
            if state.active && !allowed {
                drop(state);
                render::apply(&window, render::empty(generation));
                return;
            }
            drop(state);
            render::apply(&window, projection);
        });
    }
    pub async fn run(
        &self,
        session: SessionCoordinator,
        api: CompanionTransport,
        cancel: CancellationToken,
    ) {
        let scope = {
            let mut state = self.0.state.lock().expect("companion start");
            state.clear();
            state.scope = state.scope.saturating_add(1);
            state.scope
        };
        *self.0.session.lock().expect("companion session") = Some((scope, session.clone()));
        let mut phases = session.subscribe();
        let mut tasks = JoinSet::new();
        let mut observed_generation = None;
        loop {
            let work = {
                let mut state = self.0.state.lock().expect("companion loop");
                if state.scope != scope {
                    break;
                }
                let phase = *phases.borrow();
                if observed_generation != Some(phase.generation) {
                    observed_generation = Some(phase.generation);
                    state.interrupt();
                    state.validated = None;
                    state.purge_sample();
                    state.need_read();
                    state.draft = None;
                    state.requested =
                        state.visible || state.record.is_some() || state.pending.is_some();
                    self.post(&state);
                }
                let active = matches!(phases.borrow().phase, Phase::Ready | Phase::Refreshing);
                if !active && state.active {
                    state.clear();
                    self.post(&state);
                }
                if active && !state.active && !state.exhausted {
                    state.active = true;
                    state.requested = state.visible;
                    state.stage = if state.visible {
                        Stage::Loading
                    } else {
                        Stage::Unregistered
                    };
                    self.post(&state);
                }
                if state.active
                    && state.foreground
                    && state.expire_values(chrono::Utc::now().timestamp())
                {
                    self.post(&state);
                }
                if tasks.is_empty() && phases.borrow().phase == Phase::Ready {
                    state
                        .plan(Instant::now())
                        .map(|job| (state.fence, state.cancel.clone(), job))
                } else {
                    None
                }
            };
            if let Some((fence, stop, job)) = work {
                let owner = self.clone();
                let source = session.clone();
                let transport = api.clone();
                tasks.spawn(async move {
                    owner.work(scope, fence, source, transport, stop, job).await;
                });
                let state = self.0.state.lock().expect("companion busy");
                self.post(&state);
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break,
                changed=phases.changed()=>{if changed.is_err(){break;}},
                ()=self.0.changed.notified()=>{},
                result=tasks.join_next(),if !tasks.is_empty()=>{if matches!(result,Some(Err(_))){let mut state=self.0.state.lock().expect("companion failed task");if state.scope==scope{state.invalid();self.post(&state);}}},
                ()=tokio::time::sleep(Duration::from_secs(1))=>{},
            }
        }
        {
            let state = self.0.state.lock().expect("companion stop");
            if state.scope == scope {
                state.cancel.cancel();
            }
        }
        while tasks.join_next().await.is_some() {}
        let mut state = self.0.state.lock().expect("companion close");
        if state.scope == scope {
            state.clear();
            self.post(&state);
        }
        let mut current = self.0.session.lock().expect("companion session");
        if current.as_ref().is_some_and(|(s, _)| *s == scope) {
            *current = None;
        }
    }
}
