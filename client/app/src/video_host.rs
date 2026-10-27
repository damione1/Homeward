//! One camera-scoped native player. Private frames live only in the playing
//! variant. One owned TLS/decode job must join before its replacement starts.
mod read;
mod render;
#[cfg(test)]
mod tests;
use crate::{HomewardWindow, ProVideoAction};
use home_client_cache::{ProjectionMailbox, Publication};
use home_client_session::{Generation, Phase, SessionCoordinator};
use home_client_transport::CameraTransport;
use home_wire_contract::cameras_contract::Camera;
use slint::ComponentHandle;
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicU64, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};
use tokio::{
    sync::{Notify, watch},
    task::JoinSet,
};
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(super) struct Selection {
    pub camera: Camera,
    pub camera_context: u64,
    pub token: i32,
    pub camera_generation: i32,
    pub authority: Generation,
}
#[derive(Clone, Copy)]
pub(super) enum RetirementReason {
    Revoked,
    Invalid,
}
#[derive(Clone)]
pub(super) struct CameraRetirement {
    pub camera: home_client_model::cameras::CameraId,
    pub context: u64,
    pub authority: Generation,
    pub reason: RetirementReason,
}
pub(super) struct Picture {
    pixels: slint::SharedPixelBuffer<slint::Rgba8Pixel>,
    deadline: Instant,
    presentation_micros: i64,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PauseReason {
    User,
    OutsideViewport,
}
#[derive(Clone)]
enum Playback {
    Idle,
    Connecting,
    Playing(Arc<Picture>),
    Paused(PauseReason),
    Expired,
    Revoked,
    Offline,
    Unavailable,
    Unsupported,
    Invalid,
}
impl Playback {
    fn can_play(&self) -> bool {
        match self {
            Self::Idle
            | Self::Paused(_)
            | Self::Expired
            | Self::Offline
            | Self::Unavailable
            | Self::Invalid => true,
            Self::Connecting | Self::Playing(_) | Self::Revoked | Self::Unsupported => false,
        }
    }
    fn can_pause(&self) -> bool {
        match self {
            Self::Connecting | Self::Playing(_) => true,
            Self::Idle
            | Self::Paused(_)
            | Self::Expired
            | Self::Revoked
            | Self::Offline
            | Self::Unavailable
            | Self::Unsupported
            | Self::Invalid => false,
        }
    }
    fn deadline(&self) -> Option<Instant> {
        match self {
            Self::Playing(frame) => Some(frame.deadline),
            Self::Idle
            | Self::Connecting
            | Self::Paused(_)
            | Self::Expired
            | Self::Revoked
            | Self::Offline
            | Self::Unavailable
            | Self::Unsupported
            | Self::Invalid => None,
        }
    }
}
struct State {
    selection: Option<Selection>,
    source: Option<(SessionCoordinator, Generation)>,
    context: u64,
    generation: i32,
    phase: Playback,
    viewport: bool,
    requested: bool,
    cancel: CancellationToken,
}
impl Default for State {
    fn default() -> Self {
        Self {
            selection: None,
            source: None,
            context: 1,
            generation: 1,
            phase: Playback::Idle,
            viewport: false,
            requested: false,
            cancel: CancellationToken::new(),
        }
    }
}
impl State {
    fn current(&self) -> bool {
        self.context > 0
            && self.generation > 0
            && self.selection.as_ref().is_some_and(|selection| {
                self.source.as_ref().is_some_and(|(session, generation)| {
                    *generation == selection.authority && session.is_current(*generation)
                })
            })
    }
    fn retire(&mut self, phase: Playback) {
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.requested = false;
        self.context = self.context.checked_add(1).unwrap_or(0);
        self.generation = self.generation.checked_add(1).unwrap_or(0);
        self.phase = phase;
        if self.context == 0 || self.generation == 0 {
            self.selection = None;
            self.phase = Playback::Invalid;
        }
    }
    fn accepts(&self, context: u64, generation: Generation) -> bool {
        self.context == context
            && self.viewport
            && self.current()
            && !self.cancel.is_cancelled()
            && self
                .source
                .as_ref()
                .is_some_and(|(_, current)| *current == generation)
    }
}
#[derive(Default)]
struct Counters {
    workers_started: AtomicU64,
    workers_joined: AtomicU64,
    workers_live: AtomicUsize,
    max_live_workers: AtomicUsize,
    frames_decoded: AtomicU64,
    decode_nanos: AtomicU64,
    max_decode_nanos: AtomicU64,
}
struct Inner {
    state: Mutex<State>,
    changed: Notify,
    window: Option<slint::Weak<HomewardWindow>>,
    mailbox: ProjectionMailbox<render::Projection>,
    counters: Counters,
    retirements: watch::Sender<Option<CameraRetirement>>,
}

#[cfg(all(test, target_os = "macos"))]
pub(crate) struct ProbeSnapshot {
    pub phase: crate::ProVideoState,
    pub token: i32,
    pub generation: i32,
    pub fingerprint: Option<[u8; 32]>,
    pub workers_live: usize,
    pub workers_started: u64,
    pub workers_joined: u64,
    pub max_live_workers: usize,
    pub frames_decoded: u64,
    pub decode_nanos: u64,
    pub max_decode_nanos: u64,
}
#[cfg(all(test, target_os = "macos"))]
impl Videos {
    pub(crate) fn probe_snapshot(&self) -> ProbeSnapshot {
        use sha2::{Digest, Sha256};
        let (phase, token, generation, picture) = {
            let state = self.0.state.lock().unwrap();
            let picture = match &state.phase {
                Playback::Playing(frame) => Some(frame.clone()),
                Playback::Idle
                | Playback::Connecting
                | Playback::Paused(_)
                | Playback::Expired
                | Playback::Revoked
                | Playback::Offline
                | Playback::Unavailable
                | Playback::Unsupported
                | Playback::Invalid => None,
            };
            (
                render::state(&state.phase).0,
                state.selection.as_ref().map_or(0, |s| s.token),
                state.generation,
                picture,
            )
        };
        let counters = &self.0.counters;
        ProbeSnapshot {
            phase,
            token,
            generation,
            fingerprint: picture.map(|frame| Sha256::digest(frame.pixels.as_bytes()).into()),
            workers_live: counters.workers_live.load(Ordering::SeqCst),
            workers_started: counters.workers_started.load(Ordering::SeqCst),
            workers_joined: counters.workers_joined.load(Ordering::SeqCst),
            max_live_workers: counters.max_live_workers.load(Ordering::SeqCst),
            frames_decoded: counters.frames_decoded.load(Ordering::SeqCst),
            decode_nanos: counters.decode_nanos.load(Ordering::SeqCst),
            max_decode_nanos: counters.max_decode_nanos.load(Ordering::SeqCst),
        }
    }
    pub(crate) fn probe_viewport(&self, visible: bool) {
        self.viewport(visible);
    }
    pub(crate) fn probe_action(&self, t: i32, g: i32, a: ProVideoAction) -> Option<(i32, i32)> {
        self.action(t, g, a)
    }
}
#[derive(Clone)]
pub(crate) struct Videos(Arc<Inner>);
impl Videos {
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        let (retirements, _) = watch::channel(None);
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            changed: Notify::new(),
            window,
            mailbox: ProjectionMailbox::new(),
            counters: Counters::default(),
            retirements,
        }))
    }
    pub(super) fn subscribe_retirements(&self) -> watch::Receiver<Option<CameraRetirement>> {
        self.0.retirements.subscribe()
    }
    pub(crate) fn detached() -> Self {
        Self::new(None)
    }
    pub(crate) fn bind(window: &HomewardWindow) -> Self {
        let this = Self::new(Some(window.as_weak()));
        let owner = this.clone();
        let weak = window.as_weak();
        window.on_video_action(move |t, g, a| {
            if let Some((token, generation)) = owner.action(t, g, a)
                && let Some(window) = weak.upgrade()
            {
                window.invoke_cameras_snapshot(token, generation);
            }
        });
        let owner = this.clone();
        window.on_video_viewport(move |visible| owner.viewport(visible));
        let owner = this.clone();
        window.on_video_expire(move |generation| owner.expire(generation));
        render::clear(window);
        this
    }
    fn publish(&self, state: &State) {
        let projection = render::Projection::new(state);
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
    }
    pub(super) fn select(&self, selection: Option<Selection>) {
        let mut state = self.0.state.lock().expect("video state");
        let same = match (&state.selection, &selection) {
            (Some(old), Some(new)) => {
                old.camera.id == new.camera.id
                    && old.camera_context == new.camera_context
                    && old.authority == new.authority
            }
            (None, None) => true,
            (Some(_), None) | (None, Some(_)) => false,
        };
        if !same {
            state.retire(Playback::Idle);
        }
        state.selection = selection;
        if !same {
            self.retired(&state);
        } else {
            self.publish(&state);
        }
    }
    pub(super) fn open(&self) {
        let mut state = self.0.state.lock().expect("video state");
        if !state.current() {
            return;
        }
        state.retire(Playback::Connecting);
        state.requested = true;
        self.retired(&state);
    }
    pub(super) fn stop(&self) {
        let mut state = self.0.state.lock().expect("video state");
        state.retire(Playback::Idle);
        self.retired(&state);
    }
    fn action(&self, token: i32, generation: i32, action: ProVideoAction) -> Option<(i32, i32)> {
        let mut state = self.0.state.lock().expect("video state");
        if !state.current()
            || generation != state.generation
            || state.selection.as_ref().map(|s| s.token) != Some(token)
        {
            return None;
        }
        let mut snapshot = None;
        match action {
            ProVideoAction::Play => {
                if !state.phase.can_play() {
                    return None;
                }
                state.retire(Playback::Connecting);
                state.requested = true;
            }
            ProVideoAction::Pause => {
                if !state.phase.can_pause() {
                    return None;
                }
                state.retire(Playback::Paused(PauseReason::User));
            }
            ProVideoAction::Snapshot => {
                snapshot = state
                    .selection
                    .as_ref()
                    .map(|s| (s.token, s.camera_generation));
                state.retire(Playback::Idle);
            }
        }
        self.retired(&state);
        snapshot
    }
    fn viewport(&self, visible: bool) {
        let mut state = self.0.state.lock().expect("video state");
        if state.viewport == visible {
            return;
        }
        state.viewport = visible;
        if !visible && state.phase.can_pause() {
            state.retire(Playback::Paused(PauseReason::OutsideViewport));
            self.retired(&state);
        } else {
            self.0.changed.notify_waiters();
        }
    }
    fn expire(&self, generation: i32) {
        let mut state = self.0.state.lock().expect("video state");
        if generation == state.generation
            && state
                .phase
                .deadline()
                .is_some_and(|deadline| deadline <= Instant::now())
        {
            state.retire(Playback::Expired);
            self.retired(&state);
        }
    }
    fn failed(&self, context: u64, phase: Playback) {
        let mut state = self.0.state.lock().expect("video state");
        if state.context != context || state.cancel.is_cancelled() {
            return;
        }
        let reason = match &phase {
            Playback::Revoked => Some(RetirementReason::Revoked),
            Playback::Invalid => Some(RetirementReason::Invalid),
            Playback::Idle
            | Playback::Connecting
            | Playback::Playing(_)
            | Playback::Paused(_)
            | Playback::Expired
            | Playback::Offline
            | Playback::Unavailable
            | Playback::Unsupported => None,
        };
        if let Some(reason) = reason
            && let Some(selection) = &state.selection
        {
            self.0.retirements.send_replace(Some(CameraRetirement {
                camera: selection.camera.id.clone(),
                context: selection.camera_context,
                authority: selection.authority,
                reason,
            }));
        }
        state.retire(phase);
        self.retired(&state);
    }
    fn frame(&self, context: u64, generation: Generation, frame: Picture) {
        let mut state = self.0.state.lock().expect("video state");
        if !state.accepts(context, generation) || frame.deadline <= Instant::now() {
            return;
        }
        state.phase = Playback::Playing(Arc::new(frame));
        self.publish(&state);
    }
    pub(super) async fn run(
        &self,
        session: SessionCoordinator,
        api: CameraTransport,
        cancel: CancellationToken,
    ) {
        let mut phases = session.subscribe();
        let mut generation = phases.borrow().generation;
        let mut jobs = JoinSet::new();
        loop {
            let changed = self.0.changed.notified();
            tokio::pin!(changed);
            changed.as_mut().enable();
            let (next, deadline) = {
                let mut state = self.0.state.lock().expect("video state");
                let phase = *phases.borrow();
                let active = match phase.phase {
                    Phase::Ready | Phase::Refreshing => true,
                    Phase::SignedOut
                    | Phase::Busy
                    | Phase::Challenge { .. }
                    | Phase::EnrollmentRequired
                    | Phase::NeedsLogin
                    | Phase::CredentialRejected
                    | Phase::RateLimited
                    | Phase::NetworkUnavailable
                    | Phase::AuthorityUnavailable
                    | Phase::RightsChanged
                    | Phase::StorageUnavailable
                    | Phase::UnsupportedResponse
                    | Phase::Closed => false,
                };
                if phase.generation != generation || !active && state.source.is_some() {
                    generation = phase.generation;
                    state.retire(Playback::Idle);
                    state.selection = None;
                    state.source = None;
                    self.retired(&state);
                }
                if active {
                    state.source = Some((session.clone(), generation));
                }
                let next =
                    if jobs.is_empty() && state.current() && state.viewport && state.requested {
                        state.requested = false;
                        state
                            .selection
                            .as_ref()
                            .map(|s| (state.context, s.camera.id.clone(), state.cancel.clone()))
                    } else {
                        None
                    };
                (next, state.phase.deadline())
            };
            if let Some((context, id, token)) = next {
                let (this, session, api) = (self.clone(), session.clone(), api.clone());
                jobs.spawn(async move {
                    this.play(session, api, context, id, token).await;
                });
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break,
                result=phases.changed()=>{if result.is_err(){break}},
                ()=&mut changed=>{},
                result=jobs.join_next(),if !jobs.is_empty()=>{if result.is_some_and(|r|r.is_err()){let mut state=self.0.state.lock().expect("video state");state.retire(Playback::Invalid);self.retired(&state);}},
                _=tokio::time::sleep_until(tokio::time::Instant::from_std(deadline.unwrap_or_else(||Instant::now()+Duration::from_secs(60)))),if deadline.is_some()=>{let generation=self.0.state.lock().expect("video state").generation;self.expire(generation);},
            }
        }
        {
            let mut state = self.0.state.lock().expect("video state");
            state.retire(Playback::Idle);
            state.source = None;
            state.selection = None;
            self.retired(&state);
        }
        while jobs.join_next().await.is_some() {}
    }
}
