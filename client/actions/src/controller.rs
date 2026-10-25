//! Session-shared, bounded action jobs. A consumed submission opportunity is
//! never recreated by recovery, stream interruption or credential rotation.
use crate::{ActionRecord, ActionTarget, CommandId, LiveState, SubmissionState, TrackingError};
use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_client_session::SessionCoordinator;
use home_client_transport::{ActionTransport, Failure, FailureKind};
use home_wire_contract::{
    actions_contract,
    home::{actions::v1 as wire, registry::v1::entity_ref::Reference},
};
use std::{
    collections::{BTreeMap, VecDeque},
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::sync::{mpsc, watch};
use tokio_util::{sync::CancellationToken, task::TaskTracker};

/// Minted by the cache/UI adapter from the exact identity, capability, write
/// permission and model generation captured by the gesture. Called again after
/// waiting for session access, immediately before consuming admission.
pub type AdmissionCheck = Arc<dyn Fn() -> bool + Send + Sync>;
const MAX_RECORDS: usize = 32;
const MAX_REQUEST_BYTES: usize = 64 * 1024;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ControllerError {
    Closed,
    Capacity,
    Busy,
    InvalidRequest,
    NotReady,
    IdentityUnavailable,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TrackingIssue {
    NoSession,
    NotSent,
    Transport(FailureKind),
    Protocol(TrackingError),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NoticeKind {
    SubmissionResult,
    SubmissionUncertain,
    NotSent,
    Terminal,
    TrackingUnavailable,
    TrackingInterrupted,
    Stopped,
}
#[derive(Clone)]
pub struct ActionNotice {
    pub id: CommandId,
    pub kind: NoticeKind,
}
#[derive(Clone)]
pub struct ActionSnapshot {
    pub id: CommandId,
    pub submission: SubmissionState,
    pub live: LiveState,
    pub response: Option<wire::CommandResponse>,
    pub failure: Option<wire::ActionOperationFailure>,
    pub durable: Option<wire::DurableActionResult>,
    pub progress: Option<wire::ActionProgress>,
    pub issue: Option<TrackingIssue>,
}
struct Entry {
    record: Mutex<ActionRecord>,
    issue: Mutex<Option<TrackingIssue>>,
    updates: watch::Sender<Arc<ActionSnapshot>>,
    stop: CancellationToken,
    finished: AtomicBool,
    changed: watch::Sender<()>,
}
impl Entry {
    fn snapshot(record: &ActionRecord, issue: Option<TrackingIssue>) -> Arc<ActionSnapshot> {
        Arc::new(ActionSnapshot {
            id: record.id().clone(),
            submission: record.submission(),
            live: record.live(),
            response: record.response().cloned(),
            failure: record.operation_failure().cloned(),
            durable: record.durable().cloned(),
            progress: record.progress().cloned(),
            issue,
        })
    }
    fn update(&self, issue: Option<TrackingIssue>, change: impl FnOnce(&mut ActionRecord)) {
        let mut record = self.record.lock().expect("action record mutex");
        change(&mut record);
        let mut last = self.issue.lock().expect("action issue mutex");
        if issue.is_some() {
            *last = issue;
        }
        self.updates.send_replace(Self::snapshot(&record, *last));
        self.changed.send_replace(());
    }
}
#[derive(Clone)]
pub struct ActionHandle {
    id: CommandId,
    entry: Arc<Entry>,
}
impl ActionHandle {
    pub fn is_finished(&self) -> bool {
        self.entry.finished.load(Ordering::SeqCst)
    }
    pub fn id(&self) -> &CommandId {
        &self.id
    }
    pub fn subscribe(&self) -> watch::Receiver<Arc<ActionSnapshot>> {
        self.entry.updates.subscribe()
    }
    pub fn snapshot(&self) -> Arc<ActionSnapshot> {
        self.entry.updates.borrow().clone()
    }
    /// Stops local tracking. It is not a cancellation of an admitted command.
    /// Before admission it also withdraws the still unsent local intention.
    pub fn stop_tracking(&self) {
        self.entry.stop.cancel();
        self.entry.update(None, |record| {
            if record.submission() == SubmissionState::Prepared {
                let _ = record.not_sent();
            }
            record.stop_tracking();
        });
    }
}
struct Registry {
    closed: bool,
    active: BTreeMap<ActionTarget, CommandId>,
    history: VecDeque<ActionHandle>,
}
struct Inner {
    registry: Mutex<Registry>,
    session: SessionCoordinator,
    transport: ActionTransport,
    cancel: CancellationToken,
    tasks: TaskTracker,
    notices: mpsc::Sender<ActionNotice>,
    changed: watch::Sender<()>,
}
pub struct ActionController {
    inner: Arc<Inner>,
}
#[derive(Clone)]
pub struct ActionClient {
    inner: Weak<Inner>,
}
impl ActionController {
    pub fn start(
        session: SessionCoordinator,
        transport: ActionTransport,
    ) -> (Self, mpsc::Receiver<ActionNotice>) {
        let (notices, receiver) = mpsc::channel(64);
        let (changed, _) = watch::channel(());
        let inner = Arc::new(Inner {
            registry: Mutex::new(Registry {
                closed: false,
                active: BTreeMap::new(),
                history: VecDeque::new(),
            }),
            session,
            transport,
            cancel: CancellationToken::new(),
            tasks: TaskTracker::new(),
            notices,
            changed,
        });
        (Self { inner }, receiver)
    }
    pub fn client(&self) -> ActionClient {
        ActionClient {
            inner: Arc::downgrade(&self.inner),
        }
    }
    /// Coalescible visual notifications. Results/errors also retain the
    /// separate reliable notice channel and their complete action snapshots.
    pub fn subscribe_updates(&self) -> watch::Receiver<()> {
        self.inner.changed.subscribe()
    }
    pub async fn close(self) {
        self.stop();
        self.inner.tasks.wait().await;
    }
    fn stop(&self) {
        self.inner
            .registry
            .lock()
            .expect("action registry mutex")
            .closed = true;
        self.inner.cancel.cancel();
        self.inner.tasks.close();
    }
}
impl Drop for ActionController {
    fn drop(&mut self) {
        self.stop();
    }
}
fn now_seconds() -> Result<i64, ControllerError> {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| ControllerError::InvalidRequest)?
            .as_secs(),
    )
    .map_err(|_| ControllerError::InvalidRequest)
}
fn not_expired(request: &wire::CommandRequest) -> bool {
    request.expires_at.as_option().is_some_and(|expiry| {
        (0..1_000_000_000).contains(&expiry.nanos)
            && now_seconds().is_ok_and(|now| expiry.seconds > now)
    })
}
impl ActionClient {
    pub fn submit(
        &self,
        mut request: wire::CommandRequest,
        valid: AdmissionCheck,
    ) -> Result<ActionHandle, ControllerError> {
        let inner = self.inner.upgrade().ok_or(ControllerError::Closed)?;
        if !valid() {
            return Err(ControllerError::NotReady);
        }
        let reference = request
            .target
            .as_option()
            .and_then(|target| target.reference.as_ref())
            .ok_or(ControllerError::InvalidRequest)?;
        let entity = match reference {
            Reference::EntityId(id) => id.parse().map_err(|_| ControllerError::InvalidRequest)?,
            Reference::HaAlias(_) => return Err(ControllerError::InvalidRequest),
        };
        if !request.command_id.is_empty()
            || !actions_contract::admission_epoch(&request.admission_epoch)
            || request.config_revision == 0
            || request.capability_schema_id.is_empty()
            || request.endpoint.is_empty()
            || request.encoded_len() as usize > MAX_REQUEST_BYTES
            || !request.__buffa_unknown_fields.is_empty()
            || request.arguments.as_option().is_none_or(|args| {
                args.arguments.is_none() || !args.__buffa_unknown_fields.is_empty()
            })
        {
            return Err(ControllerError::InvalidRequest);
        }
        if request.expires_at.as_option().is_none() {
            request.expires_at = Timestamp::from_unix_secs(
                now_seconds()?
                    .checked_add(30)
                    .ok_or(ControllerError::InvalidRequest)?,
            )
            .into();
        }
        if !not_expired(&request) {
            return Err(ControllerError::NotReady);
        }
        let slot = ActionTarget::new(entity, &request.capability_schema_id, &request.endpoint)
            .ok_or(ControllerError::InvalidRequest)?;
        let mut registry = inner.registry.lock().expect("action registry mutex");
        if registry.closed {
            return Err(ControllerError::Closed);
        }
        if registry.active.contains_key(&slot) {
            return Err(ControllerError::Busy);
        }
        if registry.history.len() == MAX_RECORDS {
            let retired = registry
                .history
                .iter()
                .position(|action| action.entry.finished.load(Ordering::SeqCst));
            match retired {
                Some(index) => {
                    registry.history.remove(index);
                }
                None => return Err(ControllerError::Capacity),
            }
        }
        let id = CommandId::generate().map_err(|_| ControllerError::IdentityUnavailable)?;
        request.command_id = id.as_str().to_owned();
        let record = ActionRecord::new(
            id.clone(),
            &request.admission_epoch,
            request.config_revision,
        )
        .map_err(|_| ControllerError::InvalidRequest)?;
        let (updates, _) = watch::channel(Entry::snapshot(&record, None));
        let entry = Arc::new(Entry {
            record: Mutex::new(record),
            issue: Mutex::new(None),
            updates,
            stop: CancellationToken::new(),
            finished: AtomicBool::new(false),
            changed: inner.changed.clone(),
        });
        let handle = ActionHandle {
            id: id.clone(),
            entry: entry.clone(),
        };
        registry.active.insert(slot.clone(), id.clone());
        registry.history.push_back(handle.clone());
        let owner = inner.clone();
        // Spawning is serialized with close, so close().wait() cannot miss a job.
        inner.tasks.spawn(async move {
            let _release = Release {
                inner: owner.clone(),
                entry: entry.clone(),
                slot,
                id,
            };
            execute(&owner, &entry, request, valid).await;
        });
        Ok(handle)
    }
}
struct Release {
    inner: Arc<Inner>,
    entry: Arc<Entry>,
    slot: ActionTarget,
    id: CommandId,
}
impl Drop for Release {
    fn drop(&mut self) {
        let mut registry = self.inner.registry.lock().expect("action registry mutex");
        if registry.active.get(&self.slot) == Some(&self.id) {
            registry.active.remove(&self.slot);
        }
        self.entry.finished.store(true, Ordering::SeqCst);
    }
}
async fn notice(inner: &Inner, entry: &Entry, kind: NoticeKind) {
    let id = entry
        .record
        .lock()
        .expect("action record mutex")
        .id()
        .clone();
    tokio::select! { biased; ()=inner.cancel.cancelled()=>{}, _=inner.notices.send(ActionNotice{id,kind})=>{} }
}
async fn execute(
    inner: &Inner,
    entry: &Entry,
    request: wire::CommandRequest,
    valid: AdmissionCheck,
) {
    let access = inner.session.access(&inner.cancel).await;
    let Ok(lease) = access else {
        entry.update(Some(TrackingIssue::NoSession), |record| {
            let _ = record.not_sent();
        });
        notice(inner, entry, NoticeKind::NotSent).await;
        return;
    };
    if inner.cancel.is_cancelled()
        || entry.stop.is_cancelled()
        || lease.cancellation.is_cancelled()
        || !inner.session.is_current(lease.generation)
        || !valid()
        || !not_expired(&request)
    {
        entry.update(Some(TrackingIssue::NotSent), |record| {
            let _ = record.not_sent();
        });
        notice(inner, entry, NoticeKind::NotSent).await;
        return;
    }
    let mut begun = false;
    entry.update(None, |record| {
        begun = record.begin_submission().is_ok();
    });
    if !begun {
        return;
    }
    let scope = lease.cancellation.child_token();
    let response = tokio::select! { biased;
        ()=inner.cancel.cancelled()=>{scope.cancel();return;},
        response=inner.transport.submit_action(wire::SubmitActionRequest{command:request.into(),..Default::default()},lease.token(),Duration::from_secs(15),&scope)=>response,
    };
    match response {
        Ok(response) => {
            let mut rejected = false;
            entry.update(None, |record| match record.submitted(response) {
                Ok(()) => {
                    rejected = record.response().is_some_and(|reply| {
                        reply.error.as_option().is_some()
                            && reply.status == wire::ActionStatus::Failed
                    })
                }
                Err(error) => {
                    let _ = record.submission_uncertain(None);
                    *entry.issue.lock().expect("action issue mutex") =
                        Some(TrackingIssue::Protocol(error));
                }
            });
            notice(inner, entry, NoticeKind::SubmissionResult).await;
            if rejected {
                return;
            }
        }
        Err(failure) => {
            let detail = failure.action_operation_failure().ok().flatten();
            entry.update(Some(TrackingIssue::Transport(failure.kind())), |record| {
                if record.submission_uncertain(detail).is_err() {
                    let _ = record.submission_uncertain(None);
                }
            });
            let _ = inner
                .session
                .observe_failure(lease.generation, &failure)
                .await;
            notice(inner, entry, NoticeKind::SubmissionUncertain).await;
        }
    }
    if entry.stop.is_cancelled() {
        notice(inner, entry, NoticeKind::Stopped).await;
        return;
    }
    let cancel = entry.stop.child_token();
    tokio::select! { biased; ()=inner.cancel.cancelled()=>{}, ()=entry.stop.cancelled()=>{}, ()=reconcile(inner,entry,&cancel)=>{} }
}
async fn read_failure(
    inner: &Inner,
    entry: &Entry,
    generation: home_client_session::Generation,
    failure: &Failure,
) -> bool {
    // A rotated credential invalidates its old read, not the command. Recover
    // through Get under the current lease, including an old-token rejection.
    if !inner.session.is_current(generation) {
        return true;
    }
    entry.update(Some(TrackingIssue::Transport(failure.kind())), |record| {
        record.interrupted()
    });
    let _ = inner.session.observe_failure(generation, failure).await;
    notice(inner, entry, NoticeKind::TrackingInterrupted).await;
    failure.may_resume_read()
}
async fn reconcile(inner: &Inner, entry: &Entry, cancel: &CancellationToken) {
    loop {
        if cancel.is_cancelled() {
            return;
        }
        let Ok(lease) = inner.session.access(cancel).await else {
            entry.update(Some(TrackingIssue::NoSession), |record| {
                record.interrupted()
            });
            return;
        };
        let scope = lease.cancellation.child_token();
        let id = entry
            .record
            .lock()
            .expect("action record mutex")
            .id()
            .as_str()
            .to_owned();
        let result = inner
            .transport
            .get_action(
                wire::GetActionRequest {
                    command_id: id.clone(),
                    ..Default::default()
                },
                lease.token(),
                Duration::from_secs(15),
                &scope,
            )
            .await;
        match result {
            Ok(response) => {
                if !inner.session.is_current(lease.generation) {
                    continue;
                }
                let mut error = None;
                entry.update(None, |record| {
                    error = record.got(response).err();
                });
                if let Some(error) = error {
                    entry.update(Some(TrackingIssue::Protocol(error)), |record| {
                        record.interrupted()
                    });
                    return;
                }
                let (terminal, unavailable) = {
                    let record = entry.record.lock().expect("action record mutex");
                    (record.terminal(), record.live() == LiveState::Unavailable)
                };
                if terminal {
                    notice(inner, entry, NoticeKind::Terminal).await;
                    return;
                }
                if unavailable {
                    notice(inner, entry, NoticeKind::TrackingUnavailable).await;
                    return;
                }
                entry.update(None, |record| {
                    let _ = record.begin_watch();
                });
                match inner
                    .transport
                    .watch_action(
                        wire::WatchActionRequest {
                            command_id: id,
                            ..Default::default()
                        },
                        lease.token(),
                        Duration::from_secs(300),
                        &scope,
                    )
                    .await
                {
                    Ok(mut watch) => loop {
                        let frame = watch.next().await;
                        if !inner.session.is_current(lease.generation) {
                            break;
                        }
                        match frame {
                            Ok(Some(frame)) => {
                                let mut error = None;
                                entry.update(None, |record| error = record.watched(frame).err());
                                if let Some(error) = error {
                                    entry.update(Some(TrackingIssue::Protocol(error)), |record| {
                                        record.interrupted()
                                    });
                                    return;
                                }
                                let (terminal, unavailable) = {
                                    let record = entry.record.lock().expect("action record mutex");
                                    (record.terminal(), record.live() == LiveState::Unavailable)
                                };
                                if terminal {
                                    notice(inner, entry, NoticeKind::Terminal).await;
                                    return;
                                }
                                if unavailable {
                                    notice(inner, entry, NoticeKind::TrackingUnavailable).await;
                                    return;
                                }
                            }
                            Ok(None) => break,
                            Err(failure) => {
                                if !read_failure(inner, entry, lease.generation, &failure).await {
                                    return;
                                }
                                break;
                            }
                        }
                    },
                    Err(failure) => {
                        if !read_failure(inner, entry, lease.generation, &failure).await {
                            return;
                        }
                    }
                }
            }
            Err(failure) => {
                if !read_failure(inner, entry, lease.generation, &failure).await {
                    return;
                }
            }
        }
        entry.update(None, |record| record.interrupted());
        tokio::select! { biased; ()=cancel.cancelled()=>return, ()=tokio::time::sleep(Duration::from_secs(1))=>{} }
    }
}
