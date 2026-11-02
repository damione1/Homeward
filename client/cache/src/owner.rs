//! One cancellable cache owner per server/account profile.
use std::{
    future::Future,
    pin::Pin,
    sync::{Arc, Mutex, Weak},
    time::Duration,
};

use home_client_session::{Phase, SessionCoordinator, SessionFailure};
use home_client_transport::{FailureKind, ReconnectPolicy, StateTransport};
use tokio::{
    sync::{mpsc, watch},
    task::JoinHandle,
};
use tokio_util::sync::CancellationToken;

use crate::{
    CacheFailure, CachedView, DeliveryTicket, Filter, ProjectionGeneration, ProjectionMailbox,
    Publication, SnapshotFailure, WatchExit, watch_once,
};
use crate::{
    snapshot_cleanup::{CleanupHandle, CleanupOwner},
    snapshot_reader::read_snapshot_owned,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CachePhase {
    Idle,
    Loading,
    Live,
    Offline,
    Resyncing,
    Suspended,
    Blocked,
    Invalid,
    Closed,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CacheEvent {
    Transport(FailureKind),
    Session(SessionFailure),
    Contract(CacheFailure),
    Resync(buffa::EnumValue<home_wire_contract::home::state::v1::ResyncReason>),
    StreamEnded,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ControlFailure {
    Closed,
    InvalidViewport,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ControlOutcome {
    Applied,
    Superseded,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Viewport {
    first: usize,
    count: usize,
}
impl Viewport {
    pub fn new(first: usize, count: usize) -> Result<Self, ControlFailure> {
        if first >= crate::MAX_ENTITIES || !(1..=64).contains(&count) {
            return Err(ControlFailure::InvalidViewport);
        }
        Ok(Self { first, count })
    }
    pub fn first(self) -> usize {
        self.first
    }
    pub fn count(self) -> usize {
        self.count
    }
}
impl Default for Viewport {
    fn default() -> Self {
        Self {
            first: 0,
            count: 64,
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Mode {
    Idle,
    Active,
    Suspended,
    Closed,
}
#[derive(Clone)]
struct Desired {
    sequence: u64,
    mode: Mode,
    generation: ProjectionGeneration,
    cancel: CancellationToken,
}

type Project<P> = dyn Fn(CachePhase, Option<&CachedView>, Viewport) -> P + Send + Sync;
type Notify<P> = dyn Fn(DeliveryTicket<P>) + Send + Sync;
type RunFuture = Pin<Box<dyn Future<Output = ()> + Send>>;
type Runner<P> = dyn Fn(RunContext<P>) -> RunFuture + Send + Sync;

struct Inner<P> {
    stopped: CancellationToken,
    gate: Mutex<()>,
    desired: watch::Sender<Desired>,
    applied: watch::Sender<u64>,
    viewport: watch::Sender<Viewport>,
    mailbox: ProjectionMailbox<P>,
    project: Arc<Project<P>>,
    notify: Arc<Notify<P>>,
    events: mpsc::Sender<CacheEvent>,
}

pub struct CacheHandle<P> {
    inner: Arc<Inner<P>>,
}

/// Admission captured synchronously with a user's read intention. Any later
/// invalidation, suspension, restart or close makes it obsolete. It keeps no
/// owner alive and cannot activate a different profile's cache.
pub struct ActivationPermit<P> {
    owner: Weak<Inner<P>>,
    sequence: u64,
}
impl<P> ActivationPermit<P> {
    /// A read-only, synchronous fence for an intention captured from this
    /// cache. It does not establish write permission or activate a view. The
    /// caller must separately validate the exact capability and server grant.
    pub fn is_active(&self) -> bool {
        let Some(owner) = self.owner.upgrade() else {
            return false;
        };
        let _gate = owner.gate.lock().expect("cache control gate poisoned");
        let desired = owner.desired.borrow();
        desired.mode == Mode::Active
            && desired.sequence == self.sequence
            && !desired.cancel.is_cancelled()
            && !owner.stopped.is_cancelled()
    }
}
impl<P> Clone for CacheHandle<P> {
    fn clone(&self) -> Self {
        Self {
            inner: self.inner.clone(),
        }
    }
}

impl<P: Send + 'static> CacheHandle<P> {
    fn set_mode(&self, mode: Mode, phase: CachePhase) -> Result<u64, ControlFailure> {
        self.set_mode_checked(mode, phase, None)
            .map(|sequence| sequence.expect("unconditional cache control"))
    }

    fn set_mode_checked(
        &self,
        mode: Mode,
        phase: CachePhase,
        permit: Option<ActivationPermit<P>>,
    ) -> Result<Option<u64>, ControlFailure> {
        let desired;
        {
            let _gate = self.inner.gate.lock().expect("cache control gate poisoned");
            let current = self.inner.desired.borrow().clone();
            if current.mode == Mode::Closed || self.inner.stopped.is_cancelled() {
                return Err(ControlFailure::Closed);
            }
            if let Some(permit) = permit
                && (permit.sequence != current.sequence
                    || !Weak::ptr_eq(&permit.owner, &Arc::downgrade(&self.inner)))
            {
                return Ok(None);
            }
            current.cancel.cancel();
            let next = self
                .inner
                .mailbox
                .invalidate()
                .ok()
                .zip(current.sequence.checked_add(1));
            let Some((generation, sequence)) = next else {
                self.inner.mailbox.close();
                self.inner.desired.send_replace(Desired {
                    mode: Mode::Closed,
                    ..current
                });
                return Err(ControlFailure::Closed);
            };
            desired = Desired {
                sequence,
                mode,
                generation,
                cancel: CancellationToken::new(),
            };
            self.inner.desired.send_replace(desired.clone());
        }
        // Do not hold the control gate across the consumer's notification callback.
        self.inner.emit(desired.generation, phase, None);
        Ok(Some(desired.sequence))
    }

    pub fn prepare_activation(&self) -> Result<ActivationPermit<P>, ControlFailure> {
        let _gate = self.inner.gate.lock().expect("cache control gate poisoned");
        let current = self.inner.desired.borrow();
        if current.mode == Mode::Closed || self.inner.stopped.is_cancelled() {
            return Err(ControlFailure::Closed);
        }
        Ok(ActivationPermit {
            owner: Arc::downgrade(&self.inner),
            sequence: current.sequence,
        })
    }

    pub fn activate_prepared(
        &self,
        permit: ActivationPermit<P>,
    ) -> Result<ControlOutcome, ControlFailure> {
        self.set_mode_checked(Mode::Active, CachePhase::Loading, Some(permit))
            .map(|sequence| match sequence {
                Some(_) => ControlOutcome::Applied,
                None => ControlOutcome::Superseded,
            })
    }

    pub fn activate(&self) -> Result<(), ControlFailure> {
        self.set_mode(Mode::Active, CachePhase::Loading).map(|_| ())
    }
    pub fn invalidate(&self) -> Result<(), ControlFailure> {
        self.set_mode(Mode::Idle, CachePhase::Idle).map(|_| ())
    }
    pub fn session_changed(&self, phase: Phase) -> Result<(), ControlFailure> {
        if self.inner.desired.borrow().mode == Mode::Closed || self.inner.stopped.is_cancelled() {
            return Err(ControlFailure::Closed);
        }
        match phase {
            Phase::Ready | Phase::Refreshing => Ok(()),
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
            | Phase::Closed => self.invalidate(),
        }
    }
    pub async fn suspend(&self) -> Result<ControlOutcome, ControlFailure> {
        let sequence = self.set_mode(Mode::Suspended, CachePhase::Suspended)?;
        self.wait_applied(sequence, Mode::Suspended).await
    }
    /// Join application of an already admitted control without issuing another
    /// invalidation. Useful when a native callback has synchronously cancelled
    /// publication and its worker may observe a newer foreground event meanwhile.
    pub async fn wait_for_control(&self) -> Result<ControlOutcome, ControlFailure> {
        let current = self.inner.desired.borrow().clone();
        if current.mode == Mode::Closed || self.inner.stopped.is_cancelled() {
            return Err(ControlFailure::Closed);
        }
        self.wait_applied(current.sequence, current.mode).await
    }
    pub fn resume(&self) -> Result<(), ControlFailure> {
        self.activate()
    }
    pub fn viewport(&self, viewport: Viewport) -> Result<(), ControlFailure> {
        // A renderer may request visible rows while holding the publication
        // barrier. Viewport requests never take the control/invalidation gate.
        if self.inner.desired.borrow().mode == Mode::Closed || self.inner.stopped.is_cancelled() {
            return Err(ControlFailure::Closed);
        }
        self.inner.viewport.send_replace(viewport);
        Ok(())
    }
    pub fn metrics(&self) -> crate::ProjectionMetrics {
        self.inner.mailbox.metrics()
    }
    async fn wait_applied(
        &self,
        sequence: u64,
        mode: Mode,
    ) -> Result<ControlOutcome, ControlFailure> {
        let mut applied = self.inner.applied.subscribe();
        loop {
            let actual = *applied.borrow_and_update();
            if actual >= sequence {
                let desired = self.inner.desired.borrow();
                return Ok(
                    if actual == sequence && desired.sequence == sequence && desired.mode == mode {
                        ControlOutcome::Applied
                    } else {
                        ControlOutcome::Superseded
                    },
                );
            }
            tokio::select! {
                biased;
                () = self.inner.stopped.cancelled() => return Err(ControlFailure::Closed),
                changed = applied.changed() => if changed.is_err() { return Err(ControlFailure::Closed); },
            }
        }
    }
    fn terminate(&self) {
        let _ = self.set_mode(Mode::Closed, CachePhase::Closed);
        self.inner.mailbox.close();
    }
}

impl<P: Send + 'static> Inner<P> {
    fn emit(&self, generation: ProjectionGeneration, phase: CachePhase, view: Option<&CachedView>) {
        let viewport = *self.viewport.borrow();
        let value = (self.project)(phase, view, viewport);
        match self.mailbox.publish(generation, value) {
            Publication::Schedule(ticket) => (self.notify)(ticket),
            Publication::Coalesced | Publication::Rejected(_) => {}
        }
    }
}

/// The owner must be closed and joined before replacing a profile. Cloned
/// handles cannot reopen it after close. Drop cancels as a fallback; close proves
/// that every cache future has actually returned.
pub struct CacheOwner<P: Send + 'static> {
    handle: CacheHandle<P>,
    task: Option<JoinHandle<()>>,
    cleanup: Option<CleanupOwner>,
}

impl<P: Send + 'static> CacheOwner<P> {
    pub fn start(
        session: SessionCoordinator,
        transport: StateTransport,
        filter: Filter,
        project: impl Fn(CachePhase, Option<&CachedView>, Viewport) -> P + Send + Sync + 'static,
        notify: impl Fn(DeliveryTicket<P>) + Send + Sync + 'static,
    ) -> (Self, mpsc::Receiver<CacheEvent>) {
        let (cleanup, releases) = CleanupOwner::start(session.clone(), transport.clone());
        let runner = Arc::new(move |context: RunContext<P>| -> RunFuture {
            let (session, transport, filter, releases) = (
                session.clone(),
                transport.clone(),
                filter.clone(),
                releases.clone(),
            );
            Box::pin(native_run(context, session, transport, filter, releases))
        });
        let (mut owner, events) = Self::build(Arc::new(project), Arc::new(notify), runner);
        owner.cleanup = Some(cleanup);
        (owner, events)
    }

    fn build(
        project: Arc<Project<P>>,
        notify: Arc<Notify<P>>,
        runner: Arc<Runner<P>>,
    ) -> (Self, mpsc::Receiver<CacheEvent>) {
        let mailbox = ProjectionMailbox::new();
        let initial = Desired {
            sequence: 0,
            mode: Mode::Idle,
            generation: mailbox.generation(),
            cancel: CancellationToken::new(),
        };
        let (desired, receiver) = watch::channel(initial);
        let (applied, _) = watch::channel(0);
        let (viewport, _) = watch::channel(Viewport::default());
        // Reliable lifecycle/read errors use backpressure, never visual coalescing.
        // Command confirmations are owned outside this read-only cache.
        let (events, event_rx) = mpsc::channel(32);
        let inner = Arc::new(Inner {
            stopped: CancellationToken::new(),
            gate: Mutex::new(()),
            desired,
            applied,
            viewport,
            mailbox,
            project,
            notify,
            events,
        });
        let handle = CacheHandle {
            inner: inner.clone(),
        };
        let task = tokio::spawn(owner_loop(inner, receiver, runner));
        (
            Self {
                handle,
                task: Some(task),
                cleanup: None,
            },
            event_rx,
        )
    }

    pub fn handle(&self) -> CacheHandle<P> {
        self.handle.clone()
    }
    pub async fn close(mut self) {
        self.handle.terminate();
        if let Some(task) = self.task.take() {
            let _ = task.await;
        }
        if let Some(cleanup) = self.cleanup.take() {
            cleanup.close().await;
        }
        self.handle.inner.stopped.cancel();
    }
}

impl<P: Send + 'static> Drop for CacheOwner<P> {
    fn drop(&mut self) {
        self.handle.terminate();
        if let Some(task) = self.task.take() {
            task.abort();
        }
        self.handle.inner.stopped.cancel();
    }
}

struct RunContext<P> {
    inner: Arc<Inner<P>>,
    desired: Desired,
}
struct WorkerStop<P: Send + 'static>(Arc<Inner<P>>);
impl<P: Send + 'static> Drop for WorkerStop<P> {
    fn drop(&mut self) {
        self.0.mailbox.close();
        self.0.stopped.cancel();
    }
}
async fn owner_loop<P: Send + 'static>(
    inner: Arc<Inner<P>>,
    mut desired: watch::Receiver<Desired>,
    runner: Arc<Runner<P>>,
) {
    let _stopped = WorkerStop(inner.clone());
    loop {
        let current = desired.borrow_and_update().clone();
        // The previous run and all of its pinned futures were dropped before
        // acknowledging this new control sequence.
        inner.applied.send_replace(current.sequence);
        match current.mode {
            Mode::Closed => break,
            Mode::Active => {
                let cancellation = current.cancel.clone();
                tokio::select! {
                    biased;
                    () = cancellation.cancelled() => {},
                    () = runner(RunContext { inner: inner.clone(), desired: current }) => {},
                }
                if desired.has_changed().unwrap_or(true) {
                    continue;
                }
            }
            Mode::Idle | Mode::Suspended => {}
        }
        if desired.changed().await.is_err() {
            break;
        }
    }
}

struct DisplayState {
    phase: CachePhase,
    view: Option<CachedView>,
}
struct Emitter<P> {
    inner: Arc<Inner<P>>,
    generation: ProjectionGeneration,
    state: Mutex<DisplayState>,
}
impl<P: Send + 'static> Emitter<P> {
    fn set(&self, phase: CachePhase, view: Option<&CachedView>) {
        *self.state.lock().expect("cache view mutex poisoned") = DisplayState {
            phase,
            view: view.cloned(),
        };
        self.inner.emit(self.generation, phase, view);
    }
    fn reproject(&self) {
        let (phase, view) = {
            let state = self.state.lock().expect("cache view mutex poisoned");
            (state.phase, state.view.clone())
        };
        self.inner.emit(self.generation, phase, view.as_ref());
    }
    async fn event(&self, event: CacheEvent, cancel: &CancellationToken) {
        tokio::select! { biased; () = cancel.cancelled() => {}, _ = self.inner.events.send(event) => {} }
    }
}

async fn native_run<P: Send + 'static>(
    context: RunContext<P>,
    session: SessionCoordinator,
    transport: StateTransport,
    filter: Filter,
    releases: CleanupHandle,
) {
    let emitter = Emitter {
        inner: context.inner.clone(),
        generation: context.desired.generation,
        state: Mutex::new(DisplayState {
            phase: CachePhase::Loading,
            view: None,
        }),
    };
    let cancel = &context.desired.cancel;
    let mut viewport = context.inner.viewport.subscribe();
    // Pin both futures across viewport changes. A scroll never drops/restarts an
    // in-flight decoder or a shared credential-refresh waiter.
    let feed = feed(&emitter, &session, &transport, filter, cancel, &releases);
    let maintenance = maintain_access(&emitter, &session, cancel);
    tokio::pin!(feed, maintenance);
    let mut ended = false;
    loop {
        tokio::select! {
            biased;
            () = cancel.cancelled() => break,
            changed = viewport.changed() => { if changed.is_err() { break; } emitter.reproject(); },
            () = &mut feed, if !ended => { ended = true; },
            () = &mut maintenance => break,
        }
    }
}

async fn maintain_access<P: Send + 'static>(
    emitter: &Emitter<P>,
    session: &SessionCoordinator,
    cancel: &CancellationToken,
) {
    let mut tick = tokio::time::interval(Duration::from_secs(1));
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! { biased; () = cancel.cancelled() => return, _ = tick.tick() => {} }
        let phase = session.subscribe().borrow().phase;
        if matches!(phase, Phase::Ready | Phase::Refreshing)
            && let Err(error) = session.access(cancel).await
            && !cancel.is_cancelled()
        {
            emitter.set(CachePhase::Blocked, None);
            emitter.event(CacheEvent::Session(error), cancel).await;
        }
    }
}

// Read retries count consecutive attempts without an admitted cursor advance
// or a sustained watch retaining that admitted view. A new pagination token or
// admission proof is not evidence of progress.
#[derive(Default)]
struct ReadProgress {
    cursor: Option<home_wire_contract::home::state::v1::ViewCursor>,
    failures: u8,
}
impl ReadProgress {
    fn observe(&mut self, cursor: &home_wire_contract::home::state::v1::ViewCursor) {
        if self.cursor.as_ref().is_none_or(|previous| {
            !crate::same_generation(previous, cursor)
                || cursor.global_sequence > previous.global_sequence
        }) {
            self.cursor = Some(cursor.clone());
            self.failures = 0;
        }
    }
    fn failed(&mut self) -> u8 {
        self.failures = self.failures.saturating_add(1);
        self.failures
    }
    fn watch_ended(&mut self, elapsed: Duration, exit: &WatchExit) {
        // Quiet homes need not advance the cursor before an ordinary server
        // stream rotation. Only a long EOF/deadline with an admitted retained
        // view earns a fresh reconnect budget. Resyncs, invalid frames and
        // authority failures never earn it merely by taking a long time.
        let retained_rotation = match exit {
            WatchExit::Ended(_) => true,
            WatchExit::Transport { failure, retained } => {
                failure.kind() == FailureKind::Deadline
                    && failure.may_resume_read()
                    && retained.is_some()
            }
            WatchExit::Resync(_) | WatchExit::Invalid(_) | WatchExit::Cancelled => false,
        };
        if elapsed >= Duration::from_secs(30) && retained_rotation {
            self.failures = 0;
        }
    }
}
async fn resume_read<P: Send + 'static>(
    emitter: &Emitter<P>,
    policy: ReconnectPolicy,
    progress: &mut ReadProgress,
    cancel: &CancellationToken,
) -> bool {
    if policy
        .wait_after(progress.failed(), cancel)
        .await
        .unwrap_or(false)
    {
        return true;
    }
    if !cancel.is_cancelled() {
        // No worker will attempt another read. Leave an actionable terminal
        // state, rather than indefinitely displaying a resync in progress.
        emitter.set(CachePhase::Blocked, None);
    }
    false
}

async fn feed<P: Send + 'static>(
    emitter: &Emitter<P>,
    session: &SessionCoordinator,
    transport: &StateTransport,
    filter: Filter,
    cancel: &CancellationToken,
    releases: &CleanupHandle,
) {
    let policy = ReconnectPolicy::new(4, Duration::from_millis(250), Duration::from_secs(4))
        .expect("fixed read retry policy");
    let mut retained = None;
    let mut access_generation = None;
    let mut progress = ReadProgress::default();
    loop {
        if cancel.is_cancelled() {
            return;
        }
        let lease = match session.access(cancel).await {
            Ok(lease) => lease,
            Err(error) => {
                emitter.set(CachePhase::Blocked, None);
                emitter.event(CacheEvent::Session(error), cancel).await;
                return;
            }
        };
        if access_generation != Some(lease.generation) {
            retained = None;
            access_generation = Some(lease.generation);
            emitter.set(CachePhase::Loading, None);
        }
        let view = match retained.take() {
            Some(view) => view,
            None => {
                match read_snapshot_owned(transport, &lease, filter.clone(), 256, cancel, releases)
                    .await
                {
                    Ok(view) => {
                        if !session.is_current(lease.generation) || cancel.is_cancelled() {
                            continue;
                        }
                        emitter.set(CachePhase::Live, Some(&view));
                        view
                    }
                    Err(SnapshotFailure::Cancelled) => {
                        if cancel.is_cancelled() {
                            return;
                        }
                        continue;
                    }
                    Err(SnapshotFailure::Transport(failure)) => {
                        if !session.is_current(lease.generation) {
                            continue;
                        }
                        emitter.set(CachePhase::Offline, None);
                        let _ = session.observe_failure(lease.generation, &failure).await;
                        emitter
                            .event(CacheEvent::Transport(failure.kind()), cancel)
                            .await;
                        let retryable = failure.may_resume_read();
                        if retryable && resume_read(emitter, policy, &mut progress, cancel).await {
                            continue;
                        }
                        emitter.set(CachePhase::Blocked, None);
                        return;
                    }
                    Err(SnapshotFailure::Contract(error)) => {
                        emitter.set(CachePhase::Invalid, None);
                        emitter.event(CacheEvent::Contract(error), cancel).await;
                        return;
                    }
                }
            }
        };
        progress.observe(view.cursor());
        let watch_started = tokio::time::Instant::now();
        let exit = watch_once(
            transport,
            &lease,
            view,
            Duration::from_secs(3600),
            cancel,
            |view| {
                progress.observe(view.cursor());
                emitter.set(CachePhase::Live, Some(view));
            },
        )
        .await;
        if cancel.is_cancelled() {
            return;
        }
        if !session.is_current(lease.generation) {
            retained = None;
            emitter.set(CachePhase::Loading, None);
            continue;
        }
        progress.watch_ended(watch_started.elapsed(), &exit);
        match exit {
            WatchExit::Ended(view) => {
                emitter.set(CachePhase::Offline, Some(&view));
                retained = Some(view);
                emitter.event(CacheEvent::StreamEnded, cancel).await;
            }
            WatchExit::Transport {
                failure,
                retained: view,
            } => {
                emitter.set(
                    if view.is_some() {
                        CachePhase::Offline
                    } else {
                        CachePhase::Blocked
                    },
                    view.as_ref(),
                );
                let _ = session.observe_failure(lease.generation, &failure).await;
                emitter
                    .event(CacheEvent::Transport(failure.kind()), cancel)
                    .await;
                retained = view;
                if retained.is_none() {
                    return;
                }
            }
            WatchExit::Resync(reason) => {
                emitter.set(CachePhase::Resyncing, None);
                emitter.event(CacheEvent::Resync(reason), cancel).await;
                retained = None;
            }
            WatchExit::Invalid(error) => {
                emitter.set(CachePhase::Invalid, None);
                emitter.event(CacheEvent::Contract(error), cancel).await;
                return;
            }
            WatchExit::Cancelled => {
                retained = None;
                emitter.set(CachePhase::Loading, None);
                continue;
            }
        }
        if !resume_read(emitter, policy, &mut progress, cancel).await {
            return;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::DeliveryOutcome;
    use std::sync::atomic::{AtomicUsize, Ordering};

    #[derive(Default)]
    struct Runs {
        active: AtomicUsize,
        peak: AtomicUsize,
        ended: AtomicUsize,
        started: tokio::sync::Notify,
    }
    struct ActiveRun(Arc<Runs>);
    impl Drop for ActiveRun {
        fn drop(&mut self) {
            self.0.active.fetch_sub(1, Ordering::SeqCst);
            self.0.ended.fetch_add(1, Ordering::SeqCst);
        }
    }

    struct Rig {
        owner: CacheOwner<CachePhase>,
        events: mpsc::Receiver<CacheEvent>,
        runs: Arc<Runs>,
        queued: Arc<Mutex<Vec<DeliveryTicket<CachePhase>>>>,
    }
    impl Rig {
        fn new() -> Self {
            let runs = Arc::new(Runs::default());
            let queued = Arc::new(Mutex::new(Vec::new()));
            let observed = runs.clone();
            let runner = Arc::new(move |context: RunContext<CachePhase>| -> RunFuture {
                let runs = observed.clone();
                Box::pin(async move {
                    let active = runs.active.fetch_add(1, Ordering::SeqCst) + 1;
                    runs.peak.fetch_max(active, Ordering::SeqCst);
                    let _run = ActiveRun(runs.clone());
                    context
                        .inner
                        .emit(context.desired.generation, CachePhase::Live, None);
                    runs.started.notify_one();
                    context.desired.cancel.cancelled().await;
                })
            });
            let queue = queued.clone();
            let (owner, events) = CacheOwner::build(
                Arc::new(|phase, _, _| phase),
                Arc::new(move |ticket| queue.lock().unwrap().push(ticket)),
                runner,
            );
            Self {
                owner,
                events,
                runs,
                queued,
            }
        }
        async fn started(&self) {
            tokio::time::timeout(Duration::from_secs(2), self.runs.started.notified())
                .await
                .unwrap();
        }
    }

    fn captured_cursor(config: u64, sequence: u64, proof: u8, token: &str) -> CachedView {
        use buffa_types::google::protobuf::Timestamp;
        use home_wire_contract::home::state::v1 as state;
        let filter = Filter::all();
        let response = state::OpenViewResponse {
            snapshot_token: token.into(),
            cursor: state::ViewCursor {
                epoch: "read-recovery-epoch".into(),
                config_revision: config,
                authorization_revision: 1,
                global_sequence: sequence,
                filter_sha256: filter.digest().to_vec(),
                config_sha256: vec![1; 32],
                grant_sha256: vec![2; 32],
                admission_proof: vec![proof; 40],
                ..Default::default()
            }
            .into(),
            expires_at: Timestamp::from_unix_secs(60).into(),
            ..Default::default()
        };
        crate::SnapshotCapture::begin(filter, 256, response, &Timestamp::from_unix_secs(0))
            .unwrap()
            .finish(&Timestamp::from_unix_secs(0))
            .unwrap()
    }
    fn recovery_emitter(rig: &Rig) -> Emitter<CachePhase> {
        Emitter {
            inner: rig.owner.handle.inner.clone(),
            generation: rig.owner.handle.inner.mailbox.generation(),
            state: Mutex::new(DisplayState {
                phase: CachePhase::Loading,
                view: None,
            }),
        }
    }
    #[tokio::test]
    async fn coherent_new_config_captures_recover_beyond_the_retry_budget_without_watch_deltas() {
        let rig = Rig::new();
        let emitter = recovery_emitter(&rig);
        let policy =
            ReconnectPolicy::new(4, Duration::from_millis(1), Duration::from_millis(4)).unwrap();
        let cancel = CancellationToken::new();
        let mut progress = ReadProgress::default();
        for config in 1..=8 {
            // Every new active configuration forces a resync before any delta.
            // Pagination storage and proof happen to be fresh too, but are not
            // the reason this coherent capture earns another bounded budget.
            let view = captured_cursor(config, 10, config as u8, &format!("capture-{config}"));
            progress.observe(view.cursor());
            emitter.set(CachePhase::Live, Some(&view));
            emitter.set(CachePhase::Resyncing, None);
            assert!(resume_read(&emitter, policy, &mut progress, &cancel).await);
            assert_eq!(progress.failures, 1);
        }
        let view = captured_cursor(8, 11, 9, "advanced-global-sequence");
        progress.observe(view.cursor());
        assert!(resume_read(&emitter, policy, &mut progress, &cancel).await);
        assert_eq!(progress.failures, 1);
        rig.owner.close().await;
    }
    #[tokio::test]
    async fn identical_recaptures_with_new_tokens_and_proofs_are_bounded_and_publish_blocked() {
        let rig = Rig::new();
        let emitter = recovery_emitter(&rig);
        let policy =
            ReconnectPolicy::new(4, Duration::from_millis(1), Duration::from_millis(4)).unwrap();
        let cancel = CancellationToken::new();
        let mut progress = ReadProgress::default();
        for attempt in 1..=4 {
            let view = captured_cursor(1, 10, attempt, &format!("different-token-{attempt}"));
            progress.observe(view.cursor());
            emitter.set(CachePhase::Live, Some(&view));
            emitter.set(CachePhase::Resyncing, None);
            assert_eq!(
                resume_read(&emitter, policy, &mut progress, &cancel).await,
                attempt < 4
            );
        }
        {
            let display = emitter.state.lock().unwrap();
            assert_eq!(display.phase, CachePhase::Blocked);
            assert!(display.view.is_none());
        }
        let ticket = rig.queued.lock().unwrap().pop().unwrap();
        assert_eq!(
            ticket.deliver(|_, phase| assert_eq!(phase, CachePhase::Blocked)),
            DeliveryOutcome::Delivered
        );
        rig.owner.close().await;
    }
    #[tokio::test]
    async fn quiet_long_watch_rotations_reconnect_without_exhausting_the_retry_budget() {
        let rig = Rig::new();
        let emitter = recovery_emitter(&rig);
        let policy =
            ReconnectPolicy::new(4, Duration::from_millis(1), Duration::from_millis(4)).unwrap();
        let cancel = CancellationToken::new();
        let view = captured_cursor(1, 10, 1, "quiet-view");
        let mut progress = ReadProgress::default();
        progress.observe(view.cursor());
        for rotation in 0..8 {
            // No data changes, new snapshot tokens or refreshed proofs are
            // needed: an idle retained stream may rotate repeatedly.
            let exit = if rotation % 2 == 0 {
                WatchExit::Ended(view.clone())
            } else {
                WatchExit::Transport {
                    failure: connectrpc::ConnectError::new(
                        connectrpc::ErrorCode::DeadlineExceeded,
                        "synthetic watch deadline",
                    )
                    .into(),
                    retained: Some(view.clone()),
                }
            };
            progress.watch_ended(Duration::from_secs(30), &exit);
            emitter.set(CachePhase::Offline, Some(&view));
            assert!(resume_read(&emitter, policy, &mut progress, &cancel).await);
            assert_eq!(progress.failures, 1);
        }
        // A healthy rotation does not create an unbounded budget for the next
        // rapid failure sequence on that same cursor.
        for attempt in 2..=4 {
            progress.watch_ended(Duration::from_millis(1), &WatchExit::Ended(view.clone()));
            assert_eq!(
                resume_read(&emitter, policy, &mut progress, &cancel).await,
                attempt < 4
            );
        }
        assert_eq!(emitter.state.lock().unwrap().phase, CachePhase::Blocked);
        rig.owner.close().await;
    }

    #[test]
    fn long_resyncs_invalidations_and_authority_failures_never_reset_watch_failures() {
        let view = captured_cursor(1, 10, 1, "unchanged-view");
        let mut progress = ReadProgress::default();
        progress.observe(view.cursor());
        progress.failures = 3;
        let exits = [
            WatchExit::Ended(view.clone()),
            WatchExit::Transport {
                failure: connectrpc::ConnectError::new(
                    connectrpc::ErrorCode::DeadlineExceeded,
                    "synthetic short deadline",
                )
                .into(),
                retained: Some(view),
            },
        ];
        for exit in exits {
            progress.watch_ended(Duration::from_millis(29_999), &exit);
            assert_eq!(progress.failures, 3, "short watches remain bounded");
        }
        let exits = [
            WatchExit::Resync(buffa::EnumValue::Unknown(255)),
            WatchExit::Invalid(CacheFailure::Expired),
            WatchExit::Cancelled,
            WatchExit::Transport {
                failure: connectrpc::ConnectError::new(
                    connectrpc::ErrorCode::Unauthenticated,
                    "synthetic authority failure",
                )
                .into(),
                retained: None,
            },
            WatchExit::Transport {
                failure: connectrpc::ConnectError::new(
                    connectrpc::ErrorCode::DeadlineExceeded,
                    "synthetic deadline without an admitted view",
                )
                .into(),
                retained: None,
            },
        ];
        for exit in exits {
            progress.watch_ended(Duration::from_secs(3600), &exit);
            assert_eq!(progress.failures, 3, "duration cannot restore authority");
        }
    }
    #[test]
    fn only_admitted_cohort_changes_or_forward_positions_reset_read_failures() {
        let mut progress = ReadProgress::default();
        let original = captured_cursor(1, 10, 1, "original");
        progress.observe(original.cursor());
        progress.failed();
        let older = captured_cursor(1, 9, 2, "older-position");
        progress.observe(older.cursor());
        assert_eq!(progress.failures, 1);
        let same = captured_cursor(1, 10, 3, "same-position");
        progress.observe(same.cursor());
        assert_eq!(progress.failures, 1);
        for change in 0..6 {
            let mut cursor = original.cursor().clone();
            match change {
                0 => cursor.epoch = "restarted-epoch".into(),
                1 => cursor.config_revision += 1,
                2 => cursor.authorization_revision += 1,
                3 => cursor.config_sha256 = vec![7; 32],
                4 => cursor.grant_sha256 = vec![8; 32],
                5 => cursor.global_sequence += 1,
                _ => unreachable!(),
            }
            progress.cursor = Some(original.cursor().clone());
            progress.failures = 3;
            progress.observe(&cursor);
            assert_eq!(progress.failures, 0, "progress case {change}");
        }
    }

    #[tokio::test]
    async fn a_captured_active_fence_is_withdrawn_synchronously_and_never_revives() {
        let rig = Rig::new();
        let handle = rig.owner.handle();
        assert!(!handle.prepare_activation().unwrap().is_active());
        handle.activate().unwrap();
        let before = handle.prepare_activation().unwrap();
        assert!(before.is_active());
        handle.invalidate().unwrap();
        assert!(
            !before.is_active(),
            "does not wait for a worker or UI callback"
        );
        handle.activate().unwrap();
        assert!(
            !before.is_active(),
            "a later activation cannot revive an old gesture"
        );
        let current = handle.prepare_activation().unwrap();
        assert!(current.is_active());
        rig.owner.close().await;
        assert!(!current.is_active());
    }

    #[tokio::test]
    async fn rapid_restarts_stay_single_owner_and_suspend_acknowledges_after_the_old_future_is_gone()
     {
        let rig = Rig::new();
        let handle = rig.owner.handle();
        handle.activate().unwrap();
        rig.started().await;
        for _ in 0..100 {
            handle.activate().unwrap();
            tokio::task::yield_now().await;
        }
        assert_eq!(handle.suspend().await, Ok(ControlOutcome::Applied));
        assert_eq!(rig.runs.active.load(Ordering::SeqCst), 0);
        assert_eq!(rig.runs.peak.load(Ordering::SeqCst), 1);
        assert!(rig.runs.ended.load(Ordering::SeqCst) > 0);
        assert_eq!(handle.metrics().pending_notifications, 1);
        let ticket = rig.queued.lock().unwrap().pop().unwrap();
        assert_eq!(
            ticket.deliver(|_, phase| assert_eq!(phase, CachePhase::Suspended)),
            DeliveryOutcome::Delivered
        );
        rig.owner.close().await;
        assert_eq!(handle.activate(), Err(ControlFailure::Closed));
        assert_eq!(handle.resume(), Err(ControlFailure::Closed));
        assert_eq!(
            handle.session_changed(Phase::Ready),
            Err(ControlFailure::Closed)
        );
        assert_eq!(
            handle.viewport(Viewport::default()),
            Err(ControlFailure::Closed)
        );
        assert_eq!(handle.suspend().await, Err(ControlFailure::Closed));
    }

    #[tokio::test]
    async fn an_obsolete_run_cannot_republish_after_invalidation_or_profile_close() {
        let rig = Rig::new();
        let handle = rig.owner.handle();
        handle.activate().unwrap();
        rig.started().await;
        let old = handle.inner.desired.borrow().generation;
        handle.suspend().await.unwrap();
        handle.inner.emit(old, CachePhase::Live, None);
        let ticket = rig.queued.lock().unwrap().pop().unwrap();
        ticket.deliver(|_, phase| assert_eq!(phase, CachePhase::Suspended));
        handle.resume().unwrap();
        // Drain any old stored Notify permit before waiting for this new run.
        tokio::time::timeout(Duration::from_secs(2), async {
            while rig.runs.active.load(Ordering::SeqCst) == 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        assert_eq!(rig.runs.active.load(Ordering::SeqCst), 1);
        rig.owner.close().await;
        assert_eq!(rig.runs.active.load(Ordering::SeqCst), 0);
        handle.inner.emit(old, CachePhase::Live, None);
        for ticket in rig.queued.lock().unwrap().drain(..) {
            assert_eq!(
                ticket.deliver(|_, _| panic!("closed profile republished")),
                DeliveryOutcome::Closed
            );
        }
    }

    #[tokio::test]
    async fn a_read_admitted_before_suspend_cannot_reactivate_after_its_acknowledgement() {
        let rig = Rig::new();
        let handle = rig.owner.handle();
        handle.activate().unwrap();
        rig.started().await;
        let permit = handle.prepare_activation().unwrap();
        let delayed = handle.clone();
        let (release, waiting) = tokio::sync::oneshot::channel();
        let read = tokio::spawn(async move {
            waiting.await.unwrap();
            delayed.activate_prepared(permit)
        });
        assert_eq!(handle.suspend().await, Ok(ControlOutcome::Applied));
        release.send(()).unwrap();
        assert_eq!(read.await.unwrap(), Ok(ControlOutcome::Superseded));
        assert_eq!(rig.runs.active.load(Ordering::SeqCst), 0);
        let ticket = rig.queued.lock().unwrap().pop().unwrap();
        ticket.deliver(|_, phase| assert_eq!(phase, CachePhase::Suspended));

        // A fresh explicit intention remains usable after the transition.
        let permit = handle.prepare_activation().unwrap();
        assert_eq!(
            handle.activate_prepared(permit),
            Ok(ControlOutcome::Applied)
        );
        rig.started().await;
        let before_logout = handle.prepare_activation().unwrap();
        handle.session_changed(Phase::SignedOut).unwrap();
        assert_eq!(
            handle.activate_prepared(before_logout),
            Ok(ControlOutcome::Superseded)
        );
        let sequence = handle.inner.desired.borrow().sequence;
        handle.wait_applied(sequence, Mode::Idle).await.unwrap();
        assert_eq!(rig.runs.active.load(Ordering::SeqCst), 0);
        rig.owner.close().await;
    }

    #[tokio::test]
    async fn a_prepared_read_is_bound_to_one_owner_and_cannot_survive_close() {
        let first = Rig::new();
        let second = Rig::new();
        let a = first.owner.handle();
        let b = second.owner.handle();
        assert_eq!(
            b.activate_prepared(a.prepare_activation().unwrap()),
            Ok(ControlOutcome::Superseded)
        );
        let admitted = a.prepare_activation().unwrap();
        first.owner.close().await;
        assert_eq!(a.activate_prepared(admitted), Err(ControlFailure::Closed));
        assert!(a.prepare_activation().is_err());
        assert_eq!(second.runs.active.load(Ordering::SeqCst), 0);
        second.owner.close().await;
    }

    #[tokio::test]
    async fn session_termination_cancels_the_current_run_but_refresh_does_not_reset_the_owner() {
        let rig = Rig::new();
        let handle = rig.owner.handle();
        handle.activate().unwrap();
        rig.started().await;
        let before = handle.inner.desired.borrow().clone();
        handle.session_changed(Phase::Refreshing).unwrap();
        assert!(!before.cancel.is_cancelled());
        assert_eq!(handle.inner.desired.borrow().sequence, before.sequence);
        handle.session_changed(Phase::RightsChanged).unwrap();
        assert!(before.cancel.is_cancelled());
        let sequence = handle.inner.desired.borrow().sequence;
        handle.wait_applied(sequence, Mode::Idle).await.unwrap();
        assert_eq!(rig.runs.active.load(Ordering::SeqCst), 0);
        let ticket = rig.queued.lock().unwrap().pop().unwrap();
        ticket.deliver(|_, phase| assert_eq!(phase, CachePhase::Idle));
        rig.owner.close().await;
    }

    #[tokio::test]
    async fn read_errors_are_ordered_and_backpressured_on_a_separate_bounded_channel() {
        let mut rig = Rig::new();
        let emitter = Emitter {
            inner: rig.owner.handle.inner.clone(),
            generation: rig.owner.handle.inner.mailbox.generation(),
            state: Mutex::new(DisplayState {
                phase: CachePhase::Idle,
                view: None,
            }),
        };
        let expected: Vec<_> = (0usize..32)
            .map(|i| {
                if i.is_multiple_of(2) {
                    CacheEvent::Transport(FailureKind::Deadline)
                } else {
                    CacheEvent::Contract(CacheFailure::Expired)
                }
            })
            .collect();
        for event in &expected {
            emitter.inner.events.try_send(*event).unwrap();
        }
        let cancel = CancellationToken::new();
        let send = emitter.event(CacheEvent::StreamEnded, &cancel);
        tokio::pin!(send);
        tokio::select! {
            biased;
            () = &mut send => panic!("full event channel dropped or overwrote an event"),
            () = tokio::task::yield_now() => {},
        }
        assert_eq!(rig.events.len(), 32);
        assert_eq!(rig.events.recv().await, Some(expected[0]));
        send.await;
        for event in &expected[1..] {
            assert_eq!(rig.events.recv().await, Some(*event));
        }
        assert_eq!(rig.events.recv().await, Some(CacheEvent::StreamEnded));
        assert_eq!(rig.events.try_recv(), Err(mpsc::error::TryRecvError::Empty));
        assert_eq!(rig.owner.handle().metrics().scheduled, 0);
        rig.owner.close().await;
    }

    #[tokio::test]
    async fn a_panicking_worker_fails_closed_without_leaving_control_waiters_hanging() {
        let (owner, _events) = CacheOwner::<CachePhase>::build(
            Arc::new(|phase, _, _| phase),
            Arc::new(drop),
            Arc::new(|_| Box::pin(async { panic!("fixture worker failure") })),
        );
        let handle = owner.handle();
        handle.activate().unwrap();
        tokio::time::timeout(Duration::from_secs(2), handle.inner.stopped.cancelled())
            .await
            .unwrap();
        assert_eq!(handle.activate(), Err(ControlFailure::Closed));
        assert_eq!(handle.suspend().await, Err(ControlFailure::Closed));
        owner.close().await;
    }

    #[tokio::test]
    async fn exhausting_a_control_sequence_closes_and_joins_instead_of_wrapping() {
        let rig = Rig::new();
        let handle = rig.owner.handle();
        let current = handle.inner.desired.borrow().clone();
        handle.inner.desired.send_replace(Desired {
            sequence: u64::MAX,
            ..current
        });
        assert_eq!(handle.activate(), Err(ControlFailure::Closed));
        tokio::time::timeout(Duration::from_secs(2), rig.owner.close())
            .await
            .unwrap();
        assert!(handle.inner.stopped.is_cancelled());
    }

    #[tokio::test]
    async fn a_later_resume_supersedes_a_pending_suspend_receipt() {
        let rig = Rig::new();
        let handle = rig.owner.handle();
        handle.activate().unwrap();
        rig.started().await;
        let suspended = handle
            .set_mode(Mode::Suspended, CachePhase::Suspended)
            .unwrap();
        handle.resume().unwrap();
        assert_eq!(
            handle.wait_applied(suspended, Mode::Suspended).await,
            Ok(ControlOutcome::Superseded)
        );
        rig.owner.close().await;
    }
}
