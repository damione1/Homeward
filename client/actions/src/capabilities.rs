//! Session-owned bounded capability jobs. Each admitted gesture sends exactly
//! once; only standard operation reads follow. Local stop never cancels a
//! physical command, and neither transport failure nor uncertainty recreates it.
//!
//! One capability has at most one job in flight. A later gesture for the same
//! capability waits in a single queued slot (latest wins) and is sent when the
//! in-flight job reaches a terminal admission state. A pre-admission "route
//! busy" refusal defers the same gesture under a fresh request id; an uncertain
//! write is never replayed.
use crate::AdmissionCheck;
use buffa::Message;
use home_client_model::api::{CapabilityName, Command, RequestId};
use home_client_session::SessionCoordinator;
use home_client_transport::{CapabilityTransport, Failure, FailureKind, OperationsTransport};
use home_wire_contract::{
    api_command_operations::{CommandOperation, admit_command_operation},
    api_contract::encode_command,
    api_errors::ApiFailure,
    api_operations::InvalidOperation,
    google::longrunning as wire,
};
use std::{
    collections::VecDeque,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tokio::sync::watch;
use tokio_util::{sync::CancellationToken, task::TaskTracker};
/// One-use alarm input. Never part of a command identity, snapshot or durable state.
/// Deliberately neither Clone nor Debug; dropping an unsent value erases it.
pub struct AlarmCode(zeroize::Zeroizing<Vec<u8>>);
impl AlarmCode {
    pub fn new(value: zeroize::Zeroizing<Vec<u8>>) -> Option<Self> {
        (!value.is_empty() && value.len() <= 32 && value.iter().all(u8::is_ascii_digit))
            .then_some(Self(value))
    }
}
const MAX_JOBS: usize = 32;
const MAX_REQUEST_BYTES: usize = 64 * 1024;
/// Upper bound of local observation for one sent gesture.
const TRACKING_DEADLINE: Duration = Duration::from_secs(90);
/// Delay before each standard operation read: 200 ms, then 1 s, then 2 s.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct ReadBackoff(Duration);
impl ReadBackoff {
    const INITIAL: Self = Self(Duration::from_millis(200));
    const SECOND: Self = Self(Duration::from_secs(1));
    const CEILING: Self = Self(Duration::from_secs(2));
    fn next(self) -> Self {
        if self.0 < Self::SECOND.0 {
            Self::SECOND
        } else {
            Self::CEILING
        }
    }
}
/// Bounded re-send budget after a pre-admission route-busy refusal. Each
/// attempt is a new explicit send with a fresh request id, never a replay.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct DeferralAttempt(u8);
impl DeferralAttempt {
    const FIRST: Self = Self(1);
    const LIMIT: u8 = 6;
    fn delay(self) -> Duration {
        match self.0 {
            0 | 1 => Duration::from_millis(500),
            2 => Duration::from_secs(1),
            _ => Duration::from_secs(2),
        }
    }
    fn next(self) -> Option<Self> {
        (self.0 < Self::LIMIT).then_some(Self(self.0 + 1))
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CapabilityControllerError {
    Closed,
    Capacity,
    NotReady,
    InvalidCommand,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CapabilitySubmission {
    Prepared,
    Sending,
    NotSent,
    Accepted,
    Rejected,
    Uncertain,
    /// A later gesture for the same capability replaced this one before it was sent.
    Superseded,
    /// The server refused the route as busy before admission; a fresh send is queued.
    Deferred,
    Closed,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CapabilityTracking {
    Waiting,
    Live,
    Unavailable,
    Interrupted,
    Complete,
    Stopped,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CapabilityIssue {
    NoSession,
    AdmissionChanged,
    Cancelled,
    TrackingDeadline,
    Transport(FailureKind),
    Contract(InvalidOperation),
}
/// Admission view of one job. A terminal job never blocks a new gesture, even
/// when its physical outcome is unknown; it still renders as not confirmed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum JobState {
    Pending(PendingStage),
    Terminal(TerminalCause),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PendingStage {
    Preparing,
    Sending,
    Deferred,
    Tracking,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TerminalCause {
    NotSent,
    Rejected,
    Superseded,
    Closed,
    Complete,
    Stopped,
    Uncertain,
    Interrupted,
    Unavailable,
}
enum SubmissionClass {
    Settled(TerminalCause),
    Open(PendingStage),
    Uncertain,
}
fn classify_submission(submission: CapabilitySubmission) -> SubmissionClass {
    match submission {
        CapabilitySubmission::NotSent => SubmissionClass::Settled(TerminalCause::NotSent),
        CapabilitySubmission::Rejected => SubmissionClass::Settled(TerminalCause::Rejected),
        CapabilitySubmission::Superseded => SubmissionClass::Settled(TerminalCause::Superseded),
        CapabilitySubmission::Closed => SubmissionClass::Settled(TerminalCause::Closed),
        CapabilitySubmission::Prepared => SubmissionClass::Open(PendingStage::Preparing),
        CapabilitySubmission::Sending => SubmissionClass::Open(PendingStage::Sending),
        CapabilitySubmission::Deferred => SubmissionClass::Open(PendingStage::Deferred),
        CapabilitySubmission::Accepted => SubmissionClass::Open(PendingStage::Tracking),
        CapabilitySubmission::Uncertain => SubmissionClass::Uncertain,
    }
}
fn terminal_tracking(
    tracking: CapabilityTracking,
    operation: Option<&CommandOperation>,
) -> Option<TerminalCause> {
    match tracking {
        CapabilityTracking::Complete => Some(TerminalCause::Complete),
        CapabilityTracking::Stopped => Some(TerminalCause::Stopped),
        CapabilityTracking::Interrupted => Some(TerminalCause::Interrupted),
        CapabilityTracking::Unavailable => {
            // Reads still follow an accepted operation without a live feed.
            let polling = operation.is_some_and(|operation| !operation.done());
            (!polling).then_some(TerminalCause::Unavailable)
        }
        CapabilityTracking::Waiting | CapabilityTracking::Live => None,
    }
}
pub fn job_state(snapshot: &CapabilitySnapshot) -> JobState {
    let open = match classify_submission(snapshot.submission) {
        SubmissionClass::Settled(cause) => return JobState::Terminal(cause),
        SubmissionClass::Open(stage) => Some(stage),
        SubmissionClass::Uncertain => None,
    };
    if let Some(cause) = terminal_tracking(snapshot.tracking, snapshot.operation.as_ref()) {
        return JobState::Terminal(cause);
    }
    match open {
        Some(stage) => JobState::Pending(stage),
        None => JobState::Terminal(TerminalCause::Uncertain),
    }
}
#[derive(Clone, Debug)]
pub struct CapabilitySnapshot {
    /// Request id currently on the wire. A deferred re-send carries a fresh id.
    pub request_id: RequestId,
    pub submission: CapabilitySubmission,
    pub tracking: CapabilityTracking,
    pub operation: Option<CommandOperation>,
    pub failure: Option<ApiFailure>,
    pub issue: Option<CapabilityIssue>,
}
struct Entry {
    redacted: AtomicBool,
    consumed: AtomicBool,
    target: CapabilityName,
    /// The caller's gesture id; stable across a deferred re-send.
    gesture: RequestId,
    updates: watch::Sender<Arc<CapabilitySnapshot>>,
    stop: CancellationToken,
}
impl Entry {
    fn begin_submission(&self) -> bool {
        !self.redacted.load(Ordering::SeqCst)
            && !self.stop.is_cancelled()
            && self
                .consumed
                .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                .is_ok()
    }
    /// Only a canonical pre-admission refusal re-arms a send. The server never
    /// admitted the previous request id, so nothing can be replayed.
    fn rearm_after_deferral(&self) {
        self.consumed.store(false, Ordering::SeqCst);
    }
    fn update(&self, change: impl FnOnce(&mut CapabilitySnapshot)) {
        self.updates.send_modify(|current| {
            if self.redacted.load(Ordering::SeqCst) {
                return;
            }
            let mut next = (**current).clone();
            change(&mut next);
            #[cfg(debug_assertions)]
            {
                // Closed enums and booleans only: never format the snapshot,
                // operation, resource identity or an external error message.
                let diagnostic = |value: &CapabilitySnapshot| {
                    (
                        value.submission,
                        value.tracking,
                        value.issue,
                        value.operation.is_some(),
                        value.operation.as_ref().is_some_and(|operation| operation.done()),
                        value.operation.as_ref().is_some_and(|operation| operation.live_progress_available()),
                    )
                };
                let before = diagnostic(current.as_ref());
                let after = diagnostic(&next);
                if before != after {
                    eprintln!(
                        "homeward-command: submission={:?} tracking={:?} issue={:?} operation_present={} done={} live={}",
                        after.0, after.1, after.2, after.3, after.4, after.5
                    );
                }
            }
            *current = Arc::new(next)
        });
    }
    fn state(&self) -> JobState {
        job_state(&self.updates.borrow())
    }
    fn finished(&self) -> bool {
        matches!(self.state(), JobState::Terminal(_))
    }
    fn redact(&self) {
        self.stop.cancel();
        self.redacted.store(true, Ordering::SeqCst);
        self.updates.send_modify(|current| {
            let mut value = (**current).clone();
            value.submission = CapabilitySubmission::Closed;
            value.tracking = CapabilityTracking::Stopped;
            value.operation = None;
            value.failure = None;
            value.issue = None;
            *current = Arc::new(value);
        });
    }
}
#[derive(Clone)]
pub struct CapabilityHandle {
    entry: Arc<Entry>,
}
impl CapabilityHandle {
    /// The id of the caller's gesture. The snapshot carries the wire id.
    pub fn request_id(&self) -> RequestId {
        self.entry.gesture.clone()
    }
    pub fn snapshot(&self) -> Arc<CapabilitySnapshot> {
        self.entry.updates.borrow().clone()
    }
    pub fn subscribe(&self) -> watch::Receiver<Arc<CapabilitySnapshot>> {
        self.entry.updates.subscribe()
    }
    pub fn state(&self) -> JobState {
        self.entry.state()
    }
    pub fn is_finished(&self) -> bool {
        self.entry.finished()
    }
    /// Withdraws an unsent opportunity or stops observation of an admitted one.
    /// This never invokes CancelOperation or replays ExecuteCapability.
    pub fn stop_tracking(&self) {
        self.entry.stop.cancel();
        self.entry
            .update(|value| value.tracking = CapabilityTracking::Stopped);
    }
}
/// A gesture waiting for its capability slot, or a deferred re-send.
struct QueuedJob {
    entry: Arc<Entry>,
    command: Command,
    admission: AdmissionCheck,
    alarm_code: Option<AlarmCode>,
    not_before: Option<tokio::time::Instant>,
    deferral: Option<DeferralAttempt>,
}
/// One capability's in-flight job and the single queued successor (latest wins).
struct InFlight {
    job: Arc<Entry>,
    queued: Option<QueuedJob>,
}
struct Shared {
    session: SessionCoordinator,
    capabilities: CapabilityTransport,
    operations: OperationsTransport,
    cancel: CancellationToken,
    tasks: TaskTracker,
    entries: Mutex<VecDeque<Arc<Entry>>>,
    slots: Mutex<Vec<(CapabilityName, InFlight)>>,
}
struct Owner {
    shared: Arc<Shared>,
}
impl Drop for Owner {
    fn drop(&mut self) {
        self.shared.cancel.cancel();
        self.shared.tasks.close();
        for entry in self
            .shared
            .entries
            .lock()
            .expect("capability jobs")
            .drain(..)
        {
            entry.redact();
        }
    }
}
#[derive(Clone)]
pub struct CapabilityController {
    owner: Arc<Owner>,
}
impl CapabilityController {
    pub fn new(
        session: SessionCoordinator,
        capabilities: CapabilityTransport,
        operations: OperationsTransport,
        cancel: &CancellationToken,
    ) -> Self {
        Self {
            owner: Arc::new(Owner {
                shared: Arc::new(Shared {
                    session,
                    capabilities,
                    operations,
                    cancel: cancel.child_token(),
                    tasks: TaskTracker::new(),
                    entries: Mutex::new(VecDeque::new()),
                    slots: Mutex::new(Vec::new()),
                }),
            }),
        }
    }
    pub fn submit(
        &self,
        command: Command,
        admission: AdmissionCheck,
    ) -> Result<CapabilityHandle, CapabilityControllerError> {
        self.submit_with_alarm_code(command, admission, None)
    }
    pub fn submit_with_alarm_code(
        &self,
        command: Command,
        admission: AdmissionCheck,
        alarm_code: Option<AlarmCode>,
    ) -> Result<CapabilityHandle, CapabilityControllerError> {
        if alarm_code.is_some()
            && !matches!(command.intent(), home_client_model::api::Intent::Alarm(_))
        {
            return Err(CapabilityControllerError::InvalidCommand);
        }
        let shared = &self.owner.shared;
        if shared.cancel.is_cancelled() || shared.tasks.is_closed() {
            return Err(CapabilityControllerError::Closed);
        }
        if !admission() {
            return Err(CapabilityControllerError::NotReady);
        }
        let id = command
            .request_id()
            .cloned()
            .ok_or(CapabilityControllerError::InvalidCommand)?;
        if command.definition_etag().is_none()
            || encode_command(&command).encoded_len() as usize > MAX_REQUEST_BYTES
        {
            return Err(CapabilityControllerError::InvalidCommand);
        }
        let mut entries = shared.entries.lock().expect("capability jobs");
        if shared.cancel.is_cancelled() || shared.tasks.is_closed() {
            return Err(CapabilityControllerError::Closed);
        }
        if entries
            .iter()
            .any(|entry| entry.gesture == id || entry.updates.borrow().request_id == id)
        {
            return Err(CapabilityControllerError::InvalidCommand);
        }
        if entries.len() >= MAX_JOBS {
            entries.retain(|entry| !entry.finished());
        }
        if entries.len() >= MAX_JOBS {
            return Err(CapabilityControllerError::Capacity);
        }
        let (updates, _) = watch::channel(Arc::new(CapabilitySnapshot {
            request_id: id.clone(),
            submission: CapabilitySubmission::Prepared,
            tracking: CapabilityTracking::Waiting,
            operation: None,
            failure: None,
            issue: None,
        }));
        let entry = Arc::new(Entry {
            redacted: AtomicBool::new(false),
            consumed: AtomicBool::new(false),
            target: command.target().clone(),
            gesture: id,
            updates,
            stop: shared.cancel.child_token(),
        });
        entries.push_back(entry.clone());
        let job = QueuedJob {
            entry: entry.clone(),
            command,
            admission,
            alarm_code,
            not_before: None,
            deferral: None,
        };
        // Register the job under the same lock that closes submission during shutdown.
        let mut slots = shared.slots.lock().expect("capability slots");
        match slots.iter_mut().find(|(target, _)| target == &entry.target) {
            Some((_, inflight)) => {
                // Latest wins: an unsent earlier gesture is withdrawn, never sent.
                if let Some(previous) = inflight.queued.replace(job) {
                    superseded(&previous.entry);
                }
            }
            None => {
                slots.push((
                    entry.target.clone(),
                    InFlight {
                        job: entry.clone(),
                        queued: None,
                    },
                ));
                shared.tasks.spawn(run(shared.clone(), job));
            }
        }
        drop(slots);
        drop(entries);
        Ok(CapabilityHandle { entry })
    }
    /// The session owner joins this on shutdown. Cancellation only ends local
    /// work; no server cancellation or submission replay is implied.
    pub async fn shutdown(&self) {
        let shared = &self.owner.shared;
        {
            let entries = shared.entries.lock().expect("capability jobs");
            shared.cancel.cancel();
            shared.tasks.close();
            for entry in entries.iter() {
                entry.redact();
            }
        }
        shared.tasks.wait().await;
        for entry in shared.entries.lock().expect("capability jobs").drain(..) {
            entry.redact();
        }
    }
}
fn not_sent(entry: &Entry, issue: CapabilityIssue) {
    entry.update(|value| {
        value.submission = CapabilitySubmission::NotSent;
        value.tracking = CapabilityTracking::Stopped;
        value.issue = Some(issue)
    });
}
fn superseded(entry: &Entry) {
    entry.stop.cancel();
    entry.update(|value| {
        value.submission = CapabilitySubmission::Superseded;
        value.tracking = CapabilityTracking::Stopped;
    });
}
fn interrupted(entry: &Entry, issue: CapabilityIssue) {
    entry.update(|value| {
        value.tracking = CapabilityTracking::Interrupted;
        value.issue = Some(issue)
    });
}
fn interrupted_read(
    entry: &Entry,
    issue: CapabilityIssue,
    failure: Option<ApiFailure>,
    expected: &home_client_model::api::OperationName,
) {
    if failure
        .as_ref()
        .and_then(ApiFailure::operation)
        .is_some_and(|name| name != expected)
    {
        interrupted(
            entry,
            CapabilityIssue::Contract(InvalidOperation::Inconsistent),
        );
        return;
    }
    entry.update(|value| {
        value.tracking = CapabilityTracking::Interrupted;
        value.issue = Some(issue);
        if let Some(failure) = failure {
            value.failure = Some(failure);
        }
    });
}

fn rejected_failure(failure: &ApiFailure) -> bool {
    if failure.uncertainty().is_some() {
        return false;
    }
    use home_wire_contract::home::api::v2::ApiErrorReason as R;
    match failure.reason() {
        R::ValidationFailed
        | R::EtagMismatch
        | R::ResourceNotFound
        | R::AccessDenied
        | R::AuthenticationRequired
        | R::SourceReadOnly
        | R::CapabilityUnavailable
        | R::TopologyChanged
        | R::ConfigChanged
        | R::RateLimited
        | R::QueryBudgetExceeded
        | R::RequestTooLarge
        | R::RequestIdReused
        | R::ResourceAlreadyExists
        | R::StatePreconditionFailed
        | R::ConfigRestartRequired => true,
        R::CommitOutcomeUnknown
        | R::EffectOutcomeUnknown
        | R::ServiceUnavailable
        | R::AuthorityUnavailable
        | R::RequestCancelled
        | R::RequestDeadlineExceeded
        | R::InternalFailure
        | R::StoredDataIntegrity
        | R::ResyncRequired
        | R::StreamCapacity
        | R::MethodNotSupported
        | R::Unspecified => false,
    }
}
/// A route-busy refusal is canonical and precedes admission: the server holds
/// the endpoint for another dispatch and never saw this request as a command.
fn deferrable_failure(failure: &ApiFailure) -> bool {
    failure.uncertainty().is_none()
        && failure.reason() == home_wire_contract::home::api::v2::ApiErrorReason::RateLimited
}
/// A standard read may be retried within the same job when the server or the
/// network is momentarily unavailable. The job deadline bounds the retries.
fn retryable_read(error: &Failure) -> bool {
    if error.may_resume_read() {
        return true;
    }
    use home_wire_contract::home::api::v2::ApiErrorReason as R;
    error.api_failure().ok().flatten().is_some_and(|failure| {
        failure.uncertainty().is_none()
            && matches!(
                failure.reason(),
                R::RateLimited | R::ServiceUnavailable | R::StreamCapacity | R::QueryBudgetExceeded
            )
    })
}
fn private_failure(error: &Failure) -> bool {
    if error.api_failure().is_err() {
        return true;
    }
    error.api_failure().ok().flatten().is_some_and(|failure| {
        matches!(
            failure.reason(),
            home_wire_contract::home::api::v2::ApiErrorReason::AccessDenied
                | home_wire_contract::home::api::v2::ApiErrorReason::AuthenticationRequired
                | home_wire_contract::home::api::v2::ApiErrorReason::AuthorityUnavailable
        )
    })
}
async fn observe_failure(
    shared: &Shared,
    generation: home_client_session::Generation,
    error: &Failure,
) {
    let _ = shared.session.observe_failure(generation, error).await;
}
/// How one job left its slot.
enum Outcome {
    Settled,
    /// Refused before admission as route busy; the same gesture is re-sent later.
    Deferred {
        command: Box<Command>,
        admission: AdmissionCheck,
        attempt: DeferralAttempt,
    },
}
/// Runs one slot: the in-flight job, then each queued successor in turn.
async fn run(shared: Arc<Shared>, mut job: QueuedJob) {
    loop {
        let entry = job.entry.clone();
        let target = entry.target.clone();
        let outcome = run_job(&shared, job).await;
        match release_slot(&shared, &target, &entry, outcome) {
            Some(next) => job = next,
            None => return,
        }
    }
}
/// Frees the capability slot of a finished job and hands over the queued
/// successor. A queued user gesture always wins over a deferred re-send.
fn release_slot(
    shared: &Shared,
    target: &CapabilityName,
    entry: &Arc<Entry>,
    outcome: Outcome,
) -> Option<QueuedJob> {
    let mut slots = shared.slots.lock().expect("capability slots");
    let index = slots
        .iter()
        .position(|(slot, inflight)| slot == target && Arc::ptr_eq(&inflight.job, entry))?;
    let (_, mut inflight) = slots.remove(index);
    let next = match outcome {
        Outcome::Settled => inflight.queued.take(),
        Outcome::Deferred {
            command,
            admission,
            attempt,
        } => match inflight.queued.take() {
            Some(gesture) => {
                superseded(entry);
                Some(gesture)
            }
            None => Some(QueuedJob {
                entry: entry.clone(),
                command: *command,
                admission,
                alarm_code: None,
                not_before: Some(tokio::time::Instant::now() + attempt.delay()),
                deferral: Some(attempt),
            }),
        },
    };
    if let Some(job) = &next {
        slots.push((
            target.clone(),
            InFlight {
                job: job.entry.clone(),
                queued: None,
            },
        ));
    }
    next
}
async fn run_job(shared: &Arc<Shared>, job: QueuedJob) -> Outcome {
    let entry = job.entry.clone();
    if let Some(not_before) = job.not_before {
        tokio::select! {biased;
            ()=entry.stop.cancelled()=>{not_sent(&entry,CapabilityIssue::Cancelled);return Outcome::Settled},
            ()=tokio::time::sleep_until(not_before)=>{}
        }
    }
    match tokio::time::timeout(TRACKING_DEADLINE, run_once(shared, job)).await {
        Ok(outcome) => outcome,
        Err(_) => {
            entry.update(|value| {
                value.submission = match value.submission {
                    CapabilitySubmission::Prepared | CapabilitySubmission::Deferred => {
                        CapabilitySubmission::NotSent
                    }
                    CapabilitySubmission::Sending => CapabilitySubmission::Uncertain,
                    CapabilitySubmission::NotSent
                    | CapabilitySubmission::Accepted
                    | CapabilitySubmission::Rejected
                    | CapabilitySubmission::Uncertain
                    | CapabilitySubmission::Superseded
                    | CapabilitySubmission::Closed => value.submission,
                };
                value.tracking = CapabilityTracking::Interrupted;
                value.issue = Some(CapabilityIssue::TrackingDeadline);
            });
            Outcome::Settled
        }
    }
}
/// Builds the explicit re-send of a gesture the server never admitted.
fn deferred_command(command: &Command) -> Option<Command> {
    let id = crate::new_request_id().ok()?;
    let etag = command.definition_etag()?.clone();
    Some(
        Command::new(
            command.target().clone(),
            command.intent(),
            Some(id),
            command.deadline(),
        )
        .with_definition_etag(etag),
    )
}
async fn run_once(shared: &Arc<Shared>, job: QueuedJob) -> Outcome {
    let QueuedJob {
        entry,
        command,
        admission,
        alarm_code,
        not_before: _,
        deferral,
    } = job;
    if entry.stop.is_cancelled() {
        not_sent(&entry, CapabilityIssue::Cancelled);
        return Outcome::Settled;
    }
    let lease = tokio::select! {biased;()=entry.stop.cancelled()=>{not_sent(&entry,CapabilityIssue::Cancelled);return Outcome::Settled},result=shared.session.access(&entry.stop)=>match result{Ok(lease)=>lease,Err(_)=>{not_sent(&entry,CapabilityIssue::NoSession);return Outcome::Settled}}};
    if entry.stop.is_cancelled()
        || lease.cancellation.is_cancelled()
        || !shared.session.is_current(lease.generation)
        || !admission()
    {
        not_sent(&entry, CapabilityIssue::AdmissionChanged);
        return Outcome::Settled;
    }
    if deferral.is_some() {
        entry.rearm_after_deferral();
    }
    if !entry.begin_submission() {
        return Outcome::Settled;
    }
    let wire_id = command.request_id().cloned();
    entry.update(|value| {
        value.submission = CapabilitySubmission::Sending;
        if let Some(id) = wire_id {
            value.request_id = id;
        }
    });
    // Attach only after the final session/admission check. The logical command
    // retained for operation recovery contains no secret; recovery only reads.
    let mut request = encode_command(&command);
    let alarm = alarm_code.is_some();
    if let Some(code) = alarm_code {
        request.ephemeral_alarm_code.extend_from_slice(&code.0);
    }
    let response = tokio::select! {biased;
        ()=entry.stop.cancelled()=>{entry.update(|value|{value.submission=CapabilitySubmission::Uncertain;value.issue=Some(CapabilityIssue::Cancelled);value.tracking=CapabilityTracking::Stopped});return Outcome::Settled},
        ()=lease.cancellation.cancelled()=>{entry.update(|value|{value.submission=CapabilitySubmission::Uncertain;value.issue=Some(CapabilityIssue::NoSession);value.tracking=CapabilityTracking::Interrupted});return Outcome::Settled},
        response=shared.capabilities.execute_capability(request,lease.token(),Duration::from_secs(10),&entry.stop)=>response,
    };
    let operation = match response {
        Err(error) => {
            if private_failure(&error) {
                entry.redact();
                observe_failure(shared, lease.generation, &error).await;
                return Outcome::Settled;
            }
            observe_failure(shared, lease.generation, &error).await;
            let failure = error.api_failure().ok().flatten().cloned();
            // A route held by another dispatch refuses before admission. The
            // same gesture is re-sent later under a fresh id, within a budget.
            // An alarm code is one-use and is never re-sent.
            if !alarm && failure.as_ref().is_some_and(deferrable_failure) {
                let attempt = match deferral {
                    None => Some(DeferralAttempt::FIRST),
                    Some(previous) => previous.next(),
                };
                if let Some(attempt) = attempt
                    && let Some(resend) = deferred_command(&command)
                {
                    entry.update(|value| {
                        value.submission = CapabilitySubmission::Deferred;
                        value.tracking = CapabilityTracking::Waiting;
                        value.failure = failure;
                        value.issue = Some(CapabilityIssue::Transport(error.kind()));
                    });
                    return Outcome::Deferred {
                        command: Box::new(resend),
                        admission,
                        attempt,
                    };
                }
            }
            // A canonical pre-admission refusal can be displayed as rejected.
            // Canonical uncertainty and undecodable/transport errors remain uncertain.
            let rejected = failure.as_ref().is_some_and(rejected_failure);
            let handle = failure
                .as_ref()
                .and_then(|value| value.operation().cloned());
            let Some(handle) =
                handle.filter(|_| !rejected && shared.session.is_current(lease.generation))
            else {
                entry.update(|value| {
                    value.submission = if rejected {
                        CapabilitySubmission::Rejected
                    } else {
                        CapabilitySubmission::Uncertain
                    };
                    value.tracking = CapabilityTracking::Unavailable;
                    value.failure = failure;
                    value.issue = Some(CapabilityIssue::Transport(error.kind()));
                });
                return Outcome::Settled;
            };
            // ResourceInfo is only an authorized read handle. Reconcile with
            // GetOperation; never resubmit ExecuteCapability after uncertainty.
            // The job stays Sending until the read settles it.
            entry.update(|value| {
                value.failure = failure;
                value.issue = Some(CapabilityIssue::Transport(error.kind()));
            });
            let request = wire::GetOperationRequest {
                name: handle.as_str().to_owned(),
                ..Default::default()
            };
            let read = tokio::select! { biased;
                ()=entry.stop.cancelled()=>{entry.update(|value|{value.submission=CapabilitySubmission::Uncertain;value.tracking=CapabilityTracking::Stopped});return Outcome::Settled},
                ()=lease.cancellation.cancelled()=>{entry.update(|value|{value.submission=CapabilitySubmission::Uncertain;value.tracking=CapabilityTracking::Interrupted;value.issue=Some(CapabilityIssue::NoSession)});return Outcome::Settled},
                response=shared.operations.get_operation(request, lease.token(), Duration::from_secs(10), &entry.stop)=>response,
            };
            match read {
                Ok(raw)
                    if shared.session.is_current(lease.generation)
                        && raw.name == handle.as_str() =>
                {
                    match admit_command_operation(&raw, &command, None) {
                        Ok(operation) => operation,
                        Err(error) => {
                            entry
                                .update(|value| value.submission = CapabilitySubmission::Uncertain);
                            interrupted(&entry, CapabilityIssue::Contract(error));
                            return Outcome::Settled;
                        }
                    }
                }
                Ok(_) => {
                    entry.update(|value| value.submission = CapabilitySubmission::Uncertain);
                    interrupted(&entry, CapabilityIssue::AdmissionChanged);
                    return Outcome::Settled;
                }
                Err(error) => {
                    if private_failure(&error) {
                        entry.redact();
                    } else {
                        entry.update(|value| value.submission = CapabilitySubmission::Uncertain);
                        interrupted_read(
                            &entry,
                            CapabilityIssue::Transport(error.kind()),
                            error.api_failure().ok().flatten().cloned(),
                            &handle,
                        );
                    }
                    observe_failure(shared, lease.generation, &error).await;
                    return Outcome::Settled;
                }
            }
        }
        Ok(raw) => {
            if !shared.session.is_current(lease.generation)
                || entry.stop.is_cancelled()
                || lease.cancellation.is_cancelled()
            {
                entry.update(|value| {
                    value.submission = CapabilitySubmission::Uncertain;
                    value.tracking = CapabilityTracking::Interrupted;
                    value.issue = Some(CapabilityIssue::AdmissionChanged)
                });
                return Outcome::Settled;
            }
            match admit_command_operation(&raw, &command, None) {
                Ok(operation) => operation,
                Err(error) => {
                    entry.update(|value| {
                        value.submission = CapabilitySubmission::Uncertain;
                        value.tracking = CapabilityTracking::Unavailable;
                        value.issue = Some(CapabilityIssue::Contract(error))
                    });
                    return Outcome::Settled;
                }
            }
        }
    };
    let mut operation = operation;
    let mut backoff = ReadBackoff::INITIAL;
    loop {
        entry.update(|value| {
            value.submission = CapabilitySubmission::Accepted;
            value.tracking = if operation.done() {
                CapabilityTracking::Complete
            } else if operation.live_progress_available() {
                CapabilityTracking::Live
            } else {
                CapabilityTracking::Unavailable
            };
            value.operation = Some(operation.clone());
        });
        if operation.done() {
            return Outcome::Settled;
        }
        // Standard WaitOperation is best effort; a short response never becomes
        // a tight loop or an additional physical command. Reads back off.
        tokio::select! {biased;()=entry.stop.cancelled()=>return Outcome::Settled,()=tokio::time::sleep(backoff.0)=>{}}
        backoff = backoff.next();
        let lease = tokio::select! {biased;()=entry.stop.cancelled()=>return Outcome::Settled,result=shared.session.access(&entry.stop)=>match result{Ok(lease)=>lease,Err(_)=>{interrupted(&entry,CapabilityIssue::NoSession);return Outcome::Settled}}};
        let request = wire::WaitOperationRequest {
            name: operation.name().as_str().to_owned(),
            timeout: buffa_types::google::protobuf::Duration {
                seconds: 10,
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let response = tokio::select! {biased;()=entry.stop.cancelled()=>return Outcome::Settled,()=lease.cancellation.cancelled()=>{interrupted(&entry,CapabilityIssue::NoSession);return Outcome::Settled},response=shared.operations.wait_operation(request,lease.token(),Duration::from_secs(12),&entry.stop)=>response};
        match response {
            Ok(raw) => {
                if !shared.session.is_current(lease.generation) {
                    interrupted(&entry, CapabilityIssue::NoSession);
                    return Outcome::Settled;
                }
                match admit_command_operation(&raw, &command, Some(&operation)) {
                    Ok(next) => operation = next,
                    Err(error) => {
                        interrupted(&entry, CapabilityIssue::Contract(error));
                        return Outcome::Settled;
                    }
                }
            }
            Err(error) => {
                if private_failure(&error) {
                    entry.redact();
                    observe_failure(shared, lease.generation, &error).await;
                    return Outcome::Settled;
                }
                observe_failure(shared, lease.generation, &error).await;
                if retryable_read(&error) {
                    // A momentary read failure does not end observation; the
                    // job deadline bounds the retries.
                    let kind = error.kind();
                    entry.update(|value| value.issue = Some(CapabilityIssue::Transport(kind)));
                    continue;
                }
                interrupted_read(
                    &entry,
                    CapabilityIssue::Transport(error.kind()),
                    error.api_failure().ok().flatten().cloned(),
                    operation.name(),
                );
                return Outcome::Settled;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use home_client_model::api::{Deadline, DefinitionEtag, Intent, Power};
    use home_client_session::{SecureStore, StoreFailure};
    use home_client_transport::Connection;
    use zeroize::Zeroizing;

    struct NoCredentialStore;
    impl SecureStore for NoCredentialStore {
        fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
            panic!("test must not load credentials")
        }
        fn save(&self, _: &[u8]) -> Result<(), StoreFailure> {
            panic!("test must not save credentials")
        }
        fn clear(&self) -> Result<(), StoreFailure> {
            panic!("test must not clear credentials")
        }
    }
    fn command(id: Option<RequestId>) -> Command {
        Command::new(
            CapabilityName::try_from("homes/synthetic/entities/relay/capabilities/power").unwrap(),
            Intent::Power(Power::On),
            id,
            Deadline::new(2000000000, 0).unwrap(),
        )
        .with_definition_etag(DefinitionEtag::try_from("W/\"definition-one\"").unwrap())
    }
    fn entry(
        command: &Command,
        submission: CapabilitySubmission,
        tracking: CapabilityTracking,
    ) -> Arc<Entry> {
        let id = command.request_id().cloned().unwrap();
        let (updates, _) = watch::channel(Arc::new(CapabilitySnapshot {
            request_id: id.clone(),
            submission,
            tracking,
            operation: None,
            failure: None,
            issue: None,
        }));
        Arc::new(Entry {
            redacted: AtomicBool::new(false),
            consumed: AtomicBool::new(false),
            target: command.target().clone(),
            gesture: id,
            updates,
            stop: CancellationToken::new(),
        })
    }
    fn controller() -> (CapabilityController, CancellationToken) {
        let connection = Connection::new("https://127.0.0.1:1", &[]).unwrap();
        let session = SessionCoordinator::new(connection.auth(), Arc::new(NoCredentialStore));
        let cancel = CancellationToken::new();
        let controller = CapabilityController::new(
            session,
            connection.capabilities(),
            connection.operations(),
            &cancel,
        );
        (controller, cancel)
    }
    async fn settle(handle: &CapabilityHandle) {
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if handle.is_finished() {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
    }
    #[tokio::test]
    async fn physical_controller_requires_and_preserves_the_callers_uuid() {
        let (controller, cancel) = controller();
        assert!(matches!(
            controller.submit(command(None), Arc::new(|| true)),
            Err(CapabilityControllerError::InvalidCommand)
        ));
        let id = crate::new_request_id().unwrap();
        let handle = controller
            .submit(command(Some(id.clone())), Arc::new(|| true))
            .unwrap();
        assert_eq!(handle.request_id(), id);
        assert!(matches!(
            controller.submit(command(Some(id.clone())), Arc::new(|| true)),
            Err(CapabilityControllerError::InvalidCommand)
        ));
        settle(&handle).await;
        assert_eq!(handle.snapshot().submission, CapabilitySubmission::NotSent);
        assert_eq!(handle.snapshot().request_id, id);
        assert!(handle.snapshot().operation.is_none());
        assert!(matches!(
            controller.submit(command(Some(id)), Arc::new(|| true)),
            Err(CapabilityControllerError::InvalidCommand)
        ));
        cancel.cancel();
        controller.shutdown().await;
    }
    #[tokio::test]
    async fn later_gestures_queue_behind_the_in_flight_job_and_the_latest_wins() {
        let (controller, cancel) = controller();
        // Hold the slot: a never-admitted job stays Prepared until released.
        let held = controller
            .submit(
                command(Some(crate::new_request_id().unwrap())),
                Arc::new(|| true),
            )
            .unwrap();
        let second = controller
            .submit(
                command(Some(crate::new_request_id().unwrap())),
                Arc::new(|| true),
            )
            .expect("a second gesture is queued, never refused as busy");
        let third = controller
            .submit(
                command(Some(crate::new_request_id().unwrap())),
                Arc::new(|| true),
            )
            .unwrap();
        settle(&second).await;
        assert_eq!(
            second.snapshot().submission,
            CapabilitySubmission::Superseded,
            "an unsent queued gesture is withdrawn by the next one"
        );
        assert_eq!(
            second.state(),
            JobState::Terminal(TerminalCause::Superseded)
        );
        settle(&held).await;
        settle(&third).await;
        assert_eq!(held.snapshot().submission, CapabilitySubmission::NotSent);
        assert_eq!(
            third.snapshot().submission,
            CapabilitySubmission::NotSent,
            "the queued gesture ran once the slot was released"
        );
        assert!(controller.owner.shared.slots.lock().unwrap().is_empty());
        cancel.cancel();
        controller.shutdown().await;
    }
    #[test]
    fn unknown_outcomes_are_terminal_for_admission_but_pending_reads_are_not() {
        let command = command(Some(crate::new_request_id().unwrap()));
        let snapshot = |submission, tracking| CapabilitySnapshot {
            request_id: command.request_id().cloned().unwrap(),
            submission,
            tracking,
            operation: None,
            failure: None,
            issue: None,
        };
        use CapabilitySubmission as S;
        use CapabilityTracking as T;
        for (submission, tracking, expected) in [
            (
                S::Uncertain,
                T::Unavailable,
                JobState::Terminal(TerminalCause::Unavailable),
            ),
            (
                S::Uncertain,
                T::Waiting,
                JobState::Terminal(TerminalCause::Uncertain),
            ),
            (
                S::Accepted,
                T::Interrupted,
                JobState::Terminal(TerminalCause::Interrupted),
            ),
            (
                S::Sending,
                T::Interrupted,
                JobState::Terminal(TerminalCause::Interrupted),
            ),
            (
                S::Accepted,
                T::Unavailable,
                JobState::Terminal(TerminalCause::Unavailable),
            ),
            (
                S::Rejected,
                T::Unavailable,
                JobState::Terminal(TerminalCause::Rejected),
            ),
            (
                S::Superseded,
                T::Stopped,
                JobState::Terminal(TerminalCause::Superseded),
            ),
            (
                S::Prepared,
                T::Waiting,
                JobState::Pending(PendingStage::Preparing),
            ),
            (
                S::Sending,
                T::Waiting,
                JobState::Pending(PendingStage::Sending),
            ),
            (
                S::Deferred,
                T::Waiting,
                JobState::Pending(PendingStage::Deferred),
            ),
            (
                S::Accepted,
                T::Live,
                JobState::Pending(PendingStage::Tracking),
            ),
            (
                S::Accepted,
                T::Complete,
                JobState::Terminal(TerminalCause::Complete),
            ),
        ] {
            assert_eq!(job_state(&snapshot(submission, tracking)), expected);
        }
        let entry = entry(&command, S::Accepted, T::Unavailable);
        assert!(
            entry.finished(),
            "an unavailable feed without an operation never blocks"
        );
    }
    #[test]
    fn read_backoff_and_deferral_budget_are_bounded() {
        let mut backoff = ReadBackoff::INITIAL;
        let mut observed = Vec::new();
        for _ in 0..4 {
            observed.push(backoff.0);
            backoff = backoff.next();
        }
        assert_eq!(
            observed,
            [
                Duration::from_millis(200),
                Duration::from_secs(1),
                Duration::from_secs(2),
                Duration::from_secs(2)
            ]
        );
        let mut attempt = Some(DeferralAttempt::FIRST);
        let mut delays = Vec::new();
        while let Some(current) = attempt {
            delays.push(current.delay());
            attempt = current.next();
        }
        assert_eq!(delays.len(), usize::from(DeferralAttempt::LIMIT));
        assert_eq!(delays[0], Duration::from_millis(500));
        assert_eq!(delays[1], Duration::from_secs(1));
        assert!(
            delays[2..]
                .iter()
                .all(|delay| *delay == Duration::from_secs(2))
        );
        assert!(delays.iter().sum::<Duration>() <= Duration::from_secs(10));
    }
    #[test]
    fn lost_response_and_local_stop_never_rearm_the_consumed_intention() {
        let id = crate::new_request_id().unwrap();
        let command = command(Some(id.clone()));
        let entry = entry(
            &command,
            CapabilitySubmission::Prepared,
            CapabilityTracking::Waiting,
        );
        assert!(entry.begin_submission());
        entry.update(|value| value.submission = CapabilitySubmission::Uncertain);
        let handle = CapabilityHandle {
            entry: entry.clone(),
        };
        handle.stop_tracking();
        assert_eq!(handle.request_id(), id);
        assert_eq!(
            handle.snapshot().submission,
            CapabilitySubmission::Uncertain
        );
        assert_eq!(handle.snapshot().tracking, CapabilityTracking::Stopped);
        assert!(
            handle.snapshot().operation.is_none(),
            "no synthetic acknowledgement"
        );
        assert!(
            !entry.begin_submission(),
            "local stop never permits a second effect"
        );
        entry.redact();
        entry.update(|value| value.submission = CapabilitySubmission::Accepted);
        assert_eq!(handle.snapshot().submission, CapabilitySubmission::Closed);
        assert!(
            !entry.begin_submission(),
            "a late read cannot revive closed private state"
        );
    }
    fn fault(
        code: home_wire_contract::google::rpc::Code,
        reason: &str,
        uncertain: bool,
        name: Option<&str>,
    ) -> ApiFailure {
        use buffa_types::google::protobuf::Any;
        use home_wire_contract::{api_errors, google::rpc, home::api::v2 as api};
        fn detail<T: Message>(name: &str, value: T) -> Any {
            Any {
                type_url: format!("type.googleapis.com/{name}"),
                value: value.encode_to_vec().into(),
                ..Default::default()
            }
        }
        let mut details = vec![detail(
            "google.rpc.ErrorInfo",
            rpc::ErrorInfo {
                domain: api_errors::DOMAIN.into(),
                reason: reason.into(),
                ..Default::default()
            },
        )];
        if uncertain {
            details.push(detail(
                "home.api.v2.ExecutionUncertainty",
                api::ExecutionUncertainty {
                    phase: api::UncertaintyPhase::Effect.into(),
                    disposition: api::ReconciliationDisposition::ReconcileOnly.into(),
                    ..Default::default()
                },
            ));
        }
        if let Some(name) = name {
            details.push(detail(
                "google.rpc.ResourceInfo",
                rpc::ResourceInfo {
                    resource_type: "longrunning.googleapis.com/Operation".into(),
                    resource_name: name.into(),
                    ..Default::default()
                },
            ));
        }
        api_errors::admit_status(&rpc::Status {
            code: code as i32,
            details,
            ..Default::default()
        })
        .unwrap()
    }
    #[test]
    fn only_a_canonical_route_busy_refusal_defers_and_a_deferred_send_gets_a_fresh_id() {
        use home_wire_contract::google::rpc::Code;
        assert!(deferrable_failure(&fault(
            Code::RESOURCE_EXHAUSTED,
            "RATE_LIMITED",
            false,
            None
        )));
        assert!(
            !deferrable_failure(&fault(Code::UNKNOWN, "EFFECT_OUTCOME_UNKNOWN", true, None)),
            "uncertainty is never re-sent"
        );
        assert!(!deferrable_failure(&fault(
            Code::UNAVAILABLE,
            "SERVICE_UNAVAILABLE",
            false,
            None
        )));
        assert!(!deferrable_failure(&fault(
            Code::INVALID_ARGUMENT,
            "VALIDATION_FAILED",
            false,
            None
        )));
        let original = command(Some(crate::new_request_id().unwrap()));
        let resend = deferred_command(&original).unwrap();
        assert_ne!(resend.request_id(), original.request_id());
        assert_eq!(resend.target(), original.target());
        assert_eq!(resend.intent(), original.intent());
        assert_eq!(resend.definition_etag(), original.definition_etag());
        let entry = entry(
            &original,
            CapabilitySubmission::Prepared,
            CapabilityTracking::Waiting,
        );
        assert!(entry.begin_submission());
        assert!(!entry.begin_submission());
        entry.rearm_after_deferral();
        assert!(
            entry.begin_submission(),
            "a deferred gesture sends again exactly once"
        );
        assert!(!entry.begin_submission());
    }
    #[test]
    fn reconciliation_fault_keeps_uuid_and_typed_cause_without_terminal_operation_or_replay() {
        use home_client_model::api::OperationName;
        let id = crate::new_request_id().unwrap();
        let command = command(Some(id.clone()));
        let entry = entry(
            &command,
            CapabilitySubmission::Accepted,
            CapabilityTracking::Live,
        );
        entry.consumed.store(true, Ordering::SeqCst);
        let expected = OperationName::try_from("homes/synthetic/operations/command-one").unwrap();
        interrupted_read(
            &entry,
            CapabilityIssue::Cancelled,
            Some(fault(
                home_wire_contract::google::rpc::Code::UNKNOWN,
                "EFFECT_OUTCOME_UNKNOWN",
                true,
                Some(expected.as_str()),
            )),
            &expected,
        );
        let current = entry.updates.borrow().clone();
        assert_eq!(current.request_id, id);
        assert_eq!(current.submission, CapabilitySubmission::Accepted);
        assert_eq!(current.tracking, CapabilityTracking::Interrupted);
        assert_eq!(
            current.failure.as_ref().unwrap().operation(),
            Some(&expected)
        );
        assert!(
            current.operation.is_none(),
            "RPC uncertainty is not a fabricated terminal Operation"
        );
        assert!(
            !entry.begin_submission(),
            "reconciliation cannot rearm an admitted intention"
        );
        assert_eq!(
            job_state(&current),
            JobState::Terminal(TerminalCause::Interrupted),
            "an interrupted read never blocks the next gesture"
        );
        interrupted_read(&entry, CapabilityIssue::Cancelled, None, &expected);
        assert_eq!(
            entry.updates.borrow().failure.as_ref().unwrap().operation(),
            Some(&expected),
            "an untyped read failure must not erase known execution uncertainty"
        );
        interrupted_read(
            &entry,
            CapabilityIssue::Cancelled,
            Some(fault(
                home_wire_contract::google::rpc::Code::UNKNOWN,
                "EFFECT_OUTCOME_UNKNOWN",
                true,
                Some("homes/synthetic/operations/other"),
            )),
            &expected,
        );
        assert_eq!(
            entry.updates.borrow().issue,
            Some(CapabilityIssue::Contract(InvalidOperation::Inconsistent))
        );
        assert_eq!(
            entry.updates.borrow().failure.as_ref().unwrap().operation(),
            Some(&expected),
            "foreign handle cannot replace the exact known cause"
        );
    }
}
