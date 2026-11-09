//! Session-owned bounded capability jobs. Each admitted gesture sends exactly
//! once; only standard operation reads follow. Local stop never cancels a
//! physical command, and neither transport failure nor uncertainty recreates it.
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
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CapabilityControllerError {
    Closed,
    Capacity,
    Busy,
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
#[derive(Clone, Debug)]
pub struct CapabilitySnapshot {
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
    fn finished(&self) -> bool {
        let snapshot = self.updates.borrow();
        matches!(
            snapshot.submission,
            CapabilitySubmission::NotSent
                | CapabilitySubmission::Rejected
                | CapabilitySubmission::Closed
        ) || matches!(
            snapshot.tracking,
            CapabilityTracking::Complete | CapabilityTracking::Stopped
        )
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
    pub fn request_id(&self) -> RequestId {
        self.entry.updates.borrow().request_id.clone()
    }
    pub fn snapshot(&self) -> Arc<CapabilitySnapshot> {
        self.entry.updates.borrow().clone()
    }
    pub fn subscribe(&self) -> watch::Receiver<Arc<CapabilitySnapshot>> {
        self.entry.updates.subscribe()
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
struct Shared {
    session: SessionCoordinator,
    capabilities: CapabilityTransport,
    operations: OperationsTransport,
    cancel: CancellationToken,
    tasks: TaskTracker,
    entries: Mutex<VecDeque<Arc<Entry>>>,
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
            .any(|entry| entry.target == *command.target() && !entry.finished())
        {
            return Err(CapabilityControllerError::Busy);
        }
        if entries
            .iter()
            .any(|entry| entry.updates.borrow().request_id == id)
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
            request_id: id,
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
            updates,
            stop: shared.cancel.child_token(),
        });
        entries.push_back(entry.clone());
        // Register the job under the same lock that closes submission during shutdown.
        shared.tasks.spawn(run(
            shared.clone(),
            entry.clone(),
            command,
            admission,
            alarm_code,
        ));
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
async fn run(
    shared: Arc<Shared>,
    entry: Arc<Entry>,
    command: Command,
    admission: AdmissionCheck,
    alarm_code: Option<AlarmCode>,
) {
    if tokio::time::timeout(
        Duration::from_secs(90),
        run_once(shared, entry.clone(), command, admission, alarm_code),
    )
    .await
    .is_err()
    {
        entry.update(|value| {
            if value.submission == CapabilitySubmission::Sending {
                value.submission = CapabilitySubmission::Uncertain;
            }
            value.tracking = CapabilityTracking::Interrupted;
            value.issue = Some(CapabilityIssue::TrackingDeadline);
        });
    }
}
async fn run_once(
    shared: Arc<Shared>,
    entry: Arc<Entry>,
    command: Command,
    admission: AdmissionCheck,
    alarm_code: Option<AlarmCode>,
) {
    if entry.stop.is_cancelled() {
        not_sent(&entry, CapabilityIssue::Cancelled);
        return;
    }
    let lease = tokio::select! {biased;()=entry.stop.cancelled()=>{not_sent(&entry,CapabilityIssue::Cancelled);return},result=shared.session.access(&entry.stop)=>match result{Ok(lease)=>lease,Err(_)=>{not_sent(&entry,CapabilityIssue::NoSession);return}}};
    if entry.stop.is_cancelled()
        || lease.cancellation.is_cancelled()
        || !shared.session.is_current(lease.generation)
        || !admission()
    {
        not_sent(&entry, CapabilityIssue::AdmissionChanged);
        return;
    }
    if !entry.begin_submission() {
        return;
    }
    entry.update(|value| value.submission = CapabilitySubmission::Sending);
    // Attach only after the final session/admission check. The logical command
    // retained for operation recovery contains no secret; recovery only reads.
    let mut request = encode_command(&command);
    if let Some(code) = alarm_code {
        request.ephemeral_alarm_code.extend_from_slice(&code.0);
    }
    let response = tokio::select! {biased;
        ()=entry.stop.cancelled()=>{entry.update(|value|{value.submission=CapabilitySubmission::Uncertain;value.issue=Some(CapabilityIssue::Cancelled);value.tracking=CapabilityTracking::Stopped});return},
        ()=lease.cancellation.cancelled()=>{entry.update(|value|{value.submission=CapabilitySubmission::Uncertain;value.issue=Some(CapabilityIssue::NoSession);value.tracking=CapabilityTracking::Interrupted});return},
        response=shared.capabilities.execute_capability(request,lease.token(),Duration::from_secs(10),&entry.stop)=>response,
    };
    let operation = match response {
        Err(error) => {
            if private_failure(&error) {
                entry.redact();
                observe_failure(&shared, lease.generation, &error).await;
                return;
            }
            observe_failure(&shared, lease.generation, &error).await;
            let failure = error.api_failure().ok().flatten().cloned();
            // A canonical pre-admission refusal can be displayed as rejected.
            // Canonical uncertainty and undecodable/transport errors remain uncertain.
            let rejected = failure.as_ref().is_some_and(rejected_failure);
            let handle = failure
                .as_ref()
                .and_then(|value| value.operation().cloned());
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
            let Some(handle) = handle else {
                return;
            };
            if rejected || !shared.session.is_current(lease.generation) {
                return;
            }
            // ResourceInfo is only an authorized read handle. Reconcile with
            // GetOperation; never resubmit ExecuteCapability after uncertainty.
            let request = wire::GetOperationRequest {
                name: handle.as_str().to_owned(),
                ..Default::default()
            };
            let read = tokio::select! { biased;
                ()=entry.stop.cancelled()=>return,
                ()=lease.cancellation.cancelled()=>return,
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
                            interrupted(&entry, CapabilityIssue::Contract(error));
                            return;
                        }
                    }
                }
                Ok(_) => {
                    interrupted(&entry, CapabilityIssue::AdmissionChanged);
                    return;
                }
                Err(error) => {
                    if private_failure(&error) {
                        entry.redact();
                    } else {
                        interrupted_read(
                            &entry,
                            CapabilityIssue::Transport(error.kind()),
                            error.api_failure().ok().flatten().cloned(),
                            &handle,
                        );
                    }
                    observe_failure(&shared, lease.generation, &error).await;
                    return;
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
                return;
            }
            match admit_command_operation(&raw, &command, None) {
                Ok(operation) => operation,
                Err(error) => {
                    entry.update(|value| {
                        value.submission = CapabilitySubmission::Uncertain;
                        value.tracking = CapabilityTracking::Unavailable;
                        value.issue = Some(CapabilityIssue::Contract(error))
                    });
                    return;
                }
            }
        }
    };
    let mut operation = operation;
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
            return;
        }
        // Standard WaitOperation is best effort; a short response never becomes
        // a tight loop or an additional physical command.
        tokio::select! {biased;()=entry.stop.cancelled()=>return,()=tokio::time::sleep(Duration::from_millis(200))=>{}}
        let lease = tokio::select! {biased;()=entry.stop.cancelled()=>return,result=shared.session.access(&entry.stop)=>match result{Ok(lease)=>lease,Err(_)=>{interrupted(&entry,CapabilityIssue::NoSession);return}}};
        let request = wire::WaitOperationRequest {
            name: operation.name().as_str().to_owned(),
            timeout: buffa_types::google::protobuf::Duration {
                seconds: 10,
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        let response = tokio::select! {biased;()=entry.stop.cancelled()=>return,()=lease.cancellation.cancelled()=>{interrupted(&entry,CapabilityIssue::NoSession);return},response=shared.operations.wait_operation(request,lease.token(),Duration::from_secs(12),&entry.stop)=>response};
        match response {
            Ok(raw) => {
                if !shared.session.is_current(lease.generation) {
                    interrupted(&entry, CapabilityIssue::NoSession);
                    return;
                }
                match admit_command_operation(&raw, &command, Some(&operation)) {
                    Ok(next) => operation = next,
                    Err(error) => {
                        interrupted(&entry, CapabilityIssue::Contract(error));
                        return;
                    }
                }
            }
            Err(error) => {
                if private_failure(&error) {
                    entry.redact();
                    observe_failure(&shared, lease.generation, &error).await;
                    return;
                }
                observe_failure(&shared, lease.generation, &error).await;
                interrupted_read(
                    &entry,
                    CapabilityIssue::Transport(error.kind()),
                    error.api_failure().ok().flatten().cloned(),
                    operation.name(),
                );
                return;
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
    #[tokio::test]
    async fn physical_controller_requires_and_preserves_the_callers_uuid() {
        let connection = Connection::new("https://127.0.0.1:1", &[]).unwrap();
        let session = SessionCoordinator::new(connection.auth(), Arc::new(NoCredentialStore));
        let cancel = CancellationToken::new();
        let controller = CapabilityController::new(
            session,
            connection.capabilities(),
            connection.operations(),
            &cancel,
        );
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
            Err(CapabilityControllerError::Busy) | Err(CapabilityControllerError::InvalidCommand)
        ));
        tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if handle.snapshot().submission == CapabilitySubmission::NotSent {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        assert_eq!(handle.snapshot().request_id, id);
        assert!(handle.snapshot().operation.is_none());
        assert!(matches!(
            controller.submit(command(Some(id)), Arc::new(|| true)),
            Err(CapabilityControllerError::InvalidCommand)
        ));
        cancel.cancel();
        controller.shutdown().await;
    }
    #[test]
    fn lost_response_and_local_stop_never_rearm_the_consumed_intention() {
        let id = crate::new_request_id().unwrap();
        let command = command(Some(id.clone()));
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
            updates,
            stop: CancellationToken::new(),
        });
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
    #[test]
    fn reconciliation_fault_keeps_uuid_and_typed_cause_without_terminal_operation_or_replay() {
        use buffa_types::google::protobuf::Any;
        use home_client_model::api::OperationName;
        use home_wire_contract::{api_errors, google::rpc, home::api::v2 as api};
        fn detail<T: Message>(name: &str, value: T) -> Any {
            Any {
                type_url: format!("type.googleapis.com/{name}"),
                value: value.encode_to_vec().into(),
                ..Default::default()
            }
        }
        let fault = |name: &str| {
            api_errors::admit_status(&rpc::Status {
                code: rpc::Code::UNKNOWN as i32,
                details: vec![
                    detail(
                        "google.rpc.ErrorInfo",
                        rpc::ErrorInfo {
                            domain: api_errors::DOMAIN.into(),
                            reason: "EFFECT_OUTCOME_UNKNOWN".into(),
                            ..Default::default()
                        },
                    ),
                    detail(
                        "home.api.v2.ExecutionUncertainty",
                        api::ExecutionUncertainty {
                            phase: api::UncertaintyPhase::Effect.into(),
                            disposition: api::ReconciliationDisposition::ReconcileOnly.into(),
                            ..Default::default()
                        },
                    ),
                    detail(
                        "google.rpc.ResourceInfo",
                        rpc::ResourceInfo {
                            resource_type: "longrunning.googleapis.com/Operation".into(),
                            resource_name: name.into(),
                            ..Default::default()
                        },
                    ),
                ],
                ..Default::default()
            })
            .unwrap()
        };
        let id = crate::new_request_id().unwrap();
        let command = command(Some(id.clone()));
        let (updates, _) = watch::channel(Arc::new(CapabilitySnapshot {
            request_id: id.clone(),
            submission: CapabilitySubmission::Accepted,
            tracking: CapabilityTracking::Live,
            operation: None,
            failure: None,
            issue: None,
        }));
        let entry = Entry {
            redacted: AtomicBool::new(false),
            consumed: AtomicBool::new(true),
            target: command.target().clone(),
            updates,
            stop: CancellationToken::new(),
        };
        let expected = OperationName::try_from("homes/synthetic/operations/command-one").unwrap();
        interrupted_read(
            &entry,
            CapabilityIssue::Cancelled,
            Some(fault(expected.as_str())),
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
        interrupted_read(&entry, CapabilityIssue::Cancelled, None, &expected);
        assert_eq!(
            entry.updates.borrow().failure.as_ref().unwrap().operation(),
            Some(&expected),
            "an untyped read failure must not erase known execution uncertainty"
        );
        interrupted_read(
            &entry,
            CapabilityIssue::Cancelled,
            Some(fault("homes/synthetic/operations/other")),
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
