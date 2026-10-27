//! Private, session-owned camera screen. One lease, watch, image and bounded
//! event capture at a time; every HTTP/decode job is joined before retirement.
mod read;
mod render;
#[cfg(test)]
mod tests;
#[cfg(test)]
mod tls_probe;
use crate::{
    HomewardWindow, NavigationStep,
    camera::{Events, State, Status},
};
use home_client_cache::{ProjectionMailbox, Publication};
use home_client_model::cameras::ImageKey;
use home_client_session::{Generation, Phase, SessionCoordinator};
use home_client_transport::CameraTransport;
use slint::ComponentHandle;
use std::{
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio::{sync::Notify, task::JoinSet};
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(crate) struct Cameras(Arc<Inner>);
struct Inner {
    videos: crate::video_host::Videos,
    state: Mutex<State>,
    source: Mutex<Option<(SessionCoordinator, Generation)>>,
    window: Option<slint::Weak<HomewardWindow>>,
    changed: Notify,
    mailbox: ProjectionMailbox<render::Projection>,
}
#[derive(Clone, Copy)]
enum Job {
    Catalog,
    Camera,
}
impl Cameras {
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        let videos = match window.as_ref().and_then(slint::Weak::upgrade) {
            Some(window) => crate::video_host::Videos::bind(&window),
            None => crate::video_host::Videos::detached(),
        };
        Self(Arc::new(Inner {
            videos,
            state: Mutex::new(State::default()),
            source: Mutex::new(None),
            window,
            changed: Notify::new(),
            mailbox: ProjectionMailbox::new(),
        }))
    }
    #[cfg(test)]
    pub fn detached() -> Self {
        Self::new(None)
    }
    pub fn bind(window: &HomewardWindow) -> Self {
        let owner = Self::new(Some(window.as_weak()));
        let this = owner.clone();
        window.on_cameras_choose(move |t, g| this.choose(t, g));
        let this = owner.clone();
        window.on_cameras_event(move |t, g| this.event(t, g));
        let this = owner.clone();
        window.on_cameras_snapshot(move |t, g| this.snapshot(t, g));
        let this = owner.clone();
        window.on_cameras_open_video(move |t, g| this.open_video(t, g));
        let this = owner.clone();
        window.on_cameras_reload(move |t, g| this.reload_preview(t, g));
        let this = owner.clone();
        window.on_cameras_reload_catalog(move |g| this.reload_catalog(g));
        let this = owner.clone();
        window.on_cameras_catalog_navigate(move |step| this.navigate_catalog(step));
        let this = owner.clone();
        window.on_cameras_interactive(move |active| this.interactive(active));
        let this = owner.clone();
        window.on_cameras_viewport(move |visible| this.viewport(visible));
        let this = owner.clone();
        window.on_cameras_navigate(move |step| this.navigate(step));
        let this = owner.clone();
        window.on_cameras_expire(move |g| this.expire(g));
        render::clear(window);
        owner
    }
    fn publish(&self, state: &State) {
        let source = self.0.source.lock().expect("camera source").clone();
        let selection = if state.active && state.foreground && state.interactive {
            state.selected.as_ref().and_then(|id| {
                let (index, camera) = state
                    .catalog
                    .iter()
                    .enumerate()
                    .find(|(_, c)| &c.id == id)?;
                let (_, authority) = source.as_ref()?;
                Some(crate::video_host::Selection {
                    camera: camera.clone(),
                    camera_context: state.context,
                    token: i32::try_from(index + 1).ok()?,
                    camera_generation: state.generation,
                    authority: *authority,
                })
            })
        } else {
            None
        };
        self.0.videos.select(selection);
        let projection = render::Projection::new(state, source);
        if let Publication::Schedule(ticket) = self
            .0
            .mailbox
            .publish(self.0.mailbox.generation(), projection)
            && let Some(window) = &self.0.window
        {
            let _ = window.upgrade_in_event_loop(move |window| {
                ticket.deliver(|_, value| render::render(&window, value));
            });
        }
        self.0.changed.notify_waiters();
    }
    fn retired(&self, state: &State) {
        let _ = self.0.mailbox.invalidate();
        self.publish(state);
        self.0.changed.notify_waiters();
    }
    pub fn clear(&self) {
        let mut state = self.0.state.lock().expect("camera state");
        state.clear_private();
        *self.0.source.lock().expect("camera source") = None;
        self.retired(&state);
    }
    pub fn foreground(&self, active: bool) {
        let mut state = self.0.state.lock().expect("camera state");
        if state.foreground == active {
            return;
        };
        state.foreground = active;
        state.clear_private();
        self.retired(&state);
    }
    fn interactive(&self, active: bool) {
        let mut state = self.0.state.lock().expect("camera state");
        if state.interactive == active {
            return;
        };
        state.interactive = active;
        state.clear_private();
        self.retired(&state);
    }
    fn viewport(&self, visible: bool) {
        let mut state = self.0.state.lock().expect("camera state");
        if state.preview_visible == visible {
            return;
        };
        state.preview_visible = visible;
        if visible {
            state.image_cancel = state.cancel.child_token();
            match state.status {
                Status::Hidden | Status::Loading | Status::Ready => {
                    state.image_requested = state.target.is_some();
                    state.status = Status::Loading;
                }
                Status::Missing
                | Status::Expired
                | Status::Revoked
                | Status::WrongCamera
                | Status::Offline
                | Status::Unavailable
                | Status::Invalid
                | Status::Unsupported => {
                    state.image_requested = false;
                }
            }
            state.bump();
        } else {
            state.hide_preview();
        }
        self.retired(&state);
    }
    fn admits(&self, state: &State, generation: i32) -> bool {
        state.active
            && state.foreground
            && state.interactive
            && generation == state.generation
            && generation > 0
            && self
                .0
                .source
                .lock()
                .expect("camera source")
                .as_ref()
                .is_some_and(|(s, g)| s.is_current(*g))
    }
    fn choose(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("camera state");
        if !self.admits(&state, generation) {
            return;
        }
        let Some(id) = usize::try_from(token)
            .ok()
            .and_then(|t| t.checked_sub(1))
            .and_then(|i| state.catalog.get(i))
            .map(|v| v.id.clone())
        else {
            return;
        };
        if state.selected.as_ref() == Some(&id) {
            return;
        };
        state.select(id);
        self.retired(&state);
    }
    fn event(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("camera state");
        if !self.admits(&state, generation) {
            return;
        }
        let Some(key) = state.events.key(token).cloned() else {
            return;
        };
        if state.selected.as_ref() != Some(key.camera()) {
            return;
        };
        self.0.videos.stop();
        state.set_target(ImageKey::EventThumbnail(key));
        self.retired(&state);
    }
    fn snapshot(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("camera state");
        if !self.admits(&state, generation) {
            return;
        }
        let Some(id) = state.selected.clone() else {
            return;
        };
        if state
            .catalog
            .iter()
            .position(|c| c.id == id)
            .and_then(|i| i32::try_from(i + 1).ok())
            != Some(token)
        {
            return;
        }
        match state.status {
            Status::Expired
            | Status::Revoked
            | Status::WrongCamera
            | Status::Offline
            | Status::Unavailable
            | Status::Invalid
            | Status::Unsupported => state.select(id),
            Status::Hidden | Status::Loading | Status::Ready | Status::Missing => {
                state.set_target(ImageKey::Snapshot(id));
            }
        }
        self.0.videos.stop();
        self.retired(&state);
    }
    fn open_video(&self, token: i32, generation: i32) {
        let state = self.0.state.lock().expect("camera state");
        if !self.admits(&state, generation) {
            return;
        }
        let expected = state
            .selected
            .as_ref()
            .and_then(|id| state.catalog.iter().position(|c| &c.id == id))
            .and_then(|index| i32::try_from(index + 1).ok());
        if expected == Some(token) {
            self.0.videos.open();
        }
    }
    fn reload(&self, generation: i32) {
        let mut state = self.0.state.lock().expect("camera state");
        if !state.active || !state.interactive || generation != state.generation || generation <= 0
        {
            return;
        }
        if let Some(id) = state.selected.clone() {
            state.select(id);
        } else {
            state.clear_private();
        }
        self.retired(&state);
    }
    fn reload_preview(&self, token: i32, generation: i32) {
        let valid = {
            let state = self.0.state.lock().expect("camera state");
            self.admits(&state, generation)
                && state
                    .selected
                    .as_ref()
                    .and_then(|id| state.catalog.iter().position(|c| &c.id == id))
                    .and_then(|i| i32::try_from(i + 1).ok())
                    == Some(token)
        };
        if valid {
            self.reload(generation);
        }
    }
    fn reload_catalog(&self, generation: i32) {
        let mut state = self.0.state.lock().expect("camera state");
        if !state.active || !state.interactive || generation != state.generation || generation <= 0
        {
            return;
        }
        state.clear_private();
        self.retired(&state);
    }
    fn navigate_catalog(&self, step: NavigationStep) {
        let mut state = self.0.state.lock().expect("camera state");
        if !self.admits(&state, state.generation) || state.catalog.is_empty() {
            return;
        }
        let current = state
            .selected
            .as_ref()
            .and_then(|id| state.catalog.iter().position(|c| &c.id == id))
            .unwrap_or(0);
        let index = match step {
            NavigationStep::Previous => current.saturating_sub(1),
            NavigationStep::Next => (current + 1).min(state.catalog.len() - 1),
            NavigationStep::First => 0,
            NavigationStep::Last => state.catalog.len() - 1,
            NavigationStep::PagePrevious => current.saturating_sub(5),
            NavigationStep::PageNext => (current + 5).min(state.catalog.len() - 1),
        };
        let id = state.catalog[index].id.clone();
        if state.selected.as_ref() == Some(&id) {
            return;
        }
        state.select(id);
        self.retired(&state);
    }
    fn navigate(&self, step: NavigationStep) {
        let mut state = self.0.state.lock().expect("camera state");
        if !self.admits(&state, state.generation) || state.events.rows.is_empty() {
            return;
        }
        self.0.videos.stop();
        let current = state
            .target
            .as_ref()
            .and_then(|key| match key {
                ImageKey::Snapshot(_) => None,
                ImageKey::EventThumbnail(event) => {
                    state.events.rows.iter().position(|r| &r.key == event)
                }
            })
            .unwrap_or(0);
        let index = match step {
            NavigationStep::Previous => current.saturating_sub(1),
            NavigationStep::Next => (current + 1).min(state.events.rows.len() - 1),
            NavigationStep::First => 0,
            NavigationStep::Last => state.events.rows.len() - 1,
            NavigationStep::PagePrevious => current.saturating_sub(5),
            NavigationStep::PageNext => (current + 5).min(state.events.rows.len() - 1),
        };
        let key = state.events.rows[index].key.clone();
        state.set_target(ImageKey::EventThumbnail(key));
        self.retired(&state);
    }
    fn expire(&self, generation: i32) {
        let mut state = self.0.state.lock().expect("camera state");
        if generation == state.generation
            && state
                .picture
                .as_ref()
                .is_some_and(|p| p.deadline <= Instant::now())
        {
            state.picture = None;
            state.status = Status::Expired;
            state.bump();
            self.retired(&state);
        }
    }
    fn failed(&self, context: u64, status: Status) {
        let mut state = self.0.state.lock().expect("camera state");
        if state.context != context {
            return;
        }
        state.image_cancel.cancel();
        state.picture = None;
        state.events = Events::default();
        if matches!(
            status,
            Status::Revoked | Status::WrongCamera | Status::Invalid
        ) {
            state.catalog = Arc::default();
            state.selected = None;
            state.target = None;
            state.catalog_status = status;
        }
        state.status = status;
        state.bump();
        self.retired(&state);
    }
    pub async fn run(
        &self,
        session: SessionCoordinator,
        api: CameraTransport,
        cancel: CancellationToken,
    ) {
        let sibling = cancel.child_token();
        let camera = async {
            self.run_cameras(session.clone(), api.clone(), sibling.clone())
                .await;
            sibling.cancel();
        };
        let video = async {
            self.0
                .videos
                .run(session.clone(), api.clone(), sibling.clone())
                .await;
            sibling.cancel();
        };
        tokio::join!(camera, video);
    }
    async fn run_cameras(
        &self,
        session: SessionCoordinator,
        api: CameraTransport,
        cancel: CancellationToken,
    ) {
        let mut phases = session.subscribe();
        let mut video_retirements = self.0.videos.subscribe_retirements();
        let mut generation = phases.borrow().generation;
        let mut jobs = JoinSet::new();
        let (mut catalog_busy, mut camera_busy) = (false, false);
        loop {
            // Both the screen and its lease task observe this notification.
            // Register before inspecting state so a viewport/session change
            // cannot land between inspection and waiting.
            let changed = self.0.changed.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            let (catalog, camera, deadline) = {
                let mut state = self.0.state.lock().expect("camera state");
                let phase = *phases.borrow();
                if phase.generation != generation {
                    generation = phase.generation;
                    state.clear_private();
                    self.retired(&state);
                }
                let active = matches!(phase.phase, Phase::Ready | Phase::Refreshing)
                    && state.foreground
                    && state.interactive
                    && state.generation > 0;
                if !active && state.active {
                    state.clear_private();
                    self.retired(&state);
                }
                if active && !state.active {
                    state.active = true;
                    state.catalog_status = Status::Loading;
                    self.publish(&state);
                }
                let catalog = if active && !catalog_busy && state.catalog_status == Status::Loading
                {
                    Some((state.context, state.cancel.clone()))
                } else {
                    None
                };
                let camera = if active && !camera_busy && state.read_requested {
                    state.selected.clone().map(|id| {
                        state.read_requested = false;
                        (state.context, id, state.cancel.clone())
                    })
                } else {
                    None
                };
                (catalog, camera, state.picture.as_ref().map(|p| p.deadline))
            };
            if let Some((context, token)) = catalog {
                catalog_busy = true;
                let (this, session, api) = (self.clone(), session.clone(), api.clone());
                jobs.spawn(async move {
                    this.catalog(session, api, context, token).await;
                    Job::Catalog
                });
            }
            if let Some((context, id, token)) = camera {
                camera_busy = true;
                let (this, session, api) = (self.clone(), session.clone(), api.clone());
                jobs.spawn(async move {
                    this.camera(session, api, context, id, token).await;
                    Job::Camera
                });
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break,
                result=video_retirements.changed()=>{
                    if result.is_err(){break}
                    let retirement=video_retirements.borrow().clone();
                    if let Some(retirement)=retirement {
                        let current={let state=self.0.state.lock().expect("camera state");
                            state.context==retirement.context&&state.selected.as_ref()==Some(&retirement.camera)
                                &&self.0.source.lock().expect("camera source").as_ref().is_some_and(|(_,generation)|*generation==retirement.authority)};
                        if current {self.failed(retirement.context,match retirement.reason {crate::video_host::RetirementReason::Revoked=>Status::Revoked,crate::video_host::RetirementReason::Invalid=>Status::Invalid});}
                    }
                },
                result=phases.changed()=>{if result.is_err(){break}},
                ()=&mut changed=>{},
                result=jobs.join_next(),if !jobs.is_empty()=>{
                    match result {
                        Some(Ok(Job::Catalog))=>catalog_busy=false,
                        Some(Ok(Job::Camera))=>camera_busy=false,
                        Some(Err(_))=>{self.clear();break},
                        None=>{},
                    }
                },
                _=tokio::time::sleep_until(tokio::time::Instant::from_std(deadline.unwrap_or_else(||Instant::now()+Duration::from_secs(60)))),if deadline.is_some()=>{let generation=self.0.state.lock().expect("camera state").generation;self.expire(generation);},
            }
        }
        self.clear();
        while jobs.join_next().await.is_some() {}
        *self.0.source.lock().expect("camera source") = None;
    }
}
