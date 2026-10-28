//! Read-only subscription recovery. No mutation is stored or retried here.
use super::wire;
use crate::administration::{Section, Stage};
use crate::administration_host::{Administration, read, render};
use buffa::EnumValue;
use home_client_session::{Generation, SessionCoordinator};
use home_client_transport::{Connection, Failure, FailureKind, RpcCode};
use home_wire_contract::configuration_contract::admit_configuration_watch;
use std::time::Duration;
use tokio_util::sync::CancellationToken;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(in crate::administration_host) struct Permit(u64);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Phase {
    Idle,
    Starting(Permit),
    Live(Permit),
    Stopped,
}
pub(super) struct Cohort {
    serial: Option<u64>,
    phase: Phase,
    cancel: CancellationToken,
}
impl Default for Cohort {
    fn default() -> Self {
        Self {
            serial: Some(0),
            phase: Phase::Idle,
            cancel: CancellationToken::new(),
        }
    }
}
pub(in crate::administration_host) struct Job {
    pub permit: Permit,
    cancel: CancellationToken,
}
impl Cohort {
    pub fn restart(&mut self) {
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.phase = Phase::Idle;
    }
    pub fn start(&mut self) -> Option<Job> {
        if self.phase != Phase::Idle {
            return None;
        }
        self.serial = self.serial.and_then(|value| value.checked_add(1));
        let Some(serial) = self.serial else {
            self.phase = Phase::Stopped;
            return None;
        };
        let permit = Permit(serial);
        self.phase = Phase::Starting(permit);
        Some(Job {
            permit,
            cancel: self.cancel.child_token(),
        })
    }
    pub fn admits(&self, permit: Permit) -> bool {
        match self.phase {
            Phase::Starting(current) | Phase::Live(current) => {
                current == permit && !self.cancel.is_cancelled()
            }
            Phase::Idle | Phase::Stopped => false,
        }
    }
    pub fn syncing(&mut self, permit: Permit) -> bool {
        if !self.admits(permit) {
            return false;
        }
        self.phase = Phase::Starting(permit);
        true
    }
    pub fn ready(&mut self, permit: Permit) -> bool {
        if !self.admits(permit) {
            return false;
        }
        self.phase = Phase::Live(permit);
        true
    }
    pub fn live(&self) -> bool {
        matches!(self.phase, Phase::Live(_))
    }
    pub fn stop(&mut self, permit: Permit) -> bool {
        if !self.admits(permit) {
            return false;
        }
        self.cancel.cancel();
        self.phase = Phase::Stopped;
        true
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(in crate::administration_host) enum Stop {
    Unavailable,
    Denied,
    Invalid,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Event {
    Ready,
    Refresh,
    Recapture,
}
#[derive(Clone, Copy)]
enum Cycle {
    Refresh,
    Recapture,
    Retired,
}
struct Protocol {
    cursor: String,
    ready: bool,
}
impl Protocol {
    fn new(cursor: String) -> Self {
        Self {
            cursor,
            ready: false,
        }
    }
    fn admit(&mut self, value: &wire::WatchConfigurationResponse) -> Result<Event, Stop> {
        admit_configuration_watch(value).map_err(|_| Stop::Invalid)?;
        match &value.result {
            Some(wire::watch_configuration_response::Result::Ready(value)) => {
                if self.ready || value.resume_token != self.cursor {
                    return Err(Stop::Invalid);
                }
                self.ready = true;
                Ok(Event::Ready)
            }
            Some(wire::watch_configuration_response::Result::Changes(_)) => {
                if !self.ready {
                    return Err(Stop::Invalid);
                }
                // A change is an invalidation, never a source or snapshot.
                Ok(Event::Refresh)
            }
            Some(wire::watch_configuration_response::Result::ResyncRequired(value)) => {
                match value.reason {
                    EnumValue::Known(wire::ResyncReason::AuthorityChanged) => Err(Stop::Denied),
                    EnumValue::Known(wire::ResyncReason::ActivationSignal) => Ok(if self.ready {
                        Event::Refresh
                    } else {
                        Event::Recapture
                    }),
                    EnumValue::Known(
                        wire::ResyncReason::EpochChanged
                        | wire::ResyncReason::TokenInvalid
                        | wire::ResyncReason::HistoryLost
                        | wire::ResyncReason::Closed,
                    ) => Ok(Event::Recapture),
                    EnumValue::Known(wire::ResyncReason::Unspecified) | EnumValue::Unknown(_) => {
                        Err(Stop::Invalid)
                    }
                }
            }
            None => Err(Stop::Invalid),
        }
    }
}
fn read_stop(value: read::ReadError) -> Stop {
    match render::failure(value) {
        Stage::Denied => Stop::Denied,
        Stage::Offline | Stage::Unavailable | Stage::Loading => Stop::Unavailable,
        Stage::Invalid | Stage::Unsupported | Stage::Ready | Stage::Empty => Stop::Invalid,
    }
}
fn transport_stop(value: &Failure) -> Stop {
    if !matches!(value.authorization_termination(), Ok(None))
        || matches!(
            value.kind(),
            FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated)
        )
    {
        return Stop::Denied;
    }
    if value.may_resume_read() {
        Stop::Unavailable
    } else {
        Stop::Invalid
    }
}

pub(in crate::administration_host) async fn run(
    owner: Administration,
    session: SessionCoordinator,
    connection: Connection,
    generation: Generation,
    selection: Option<read::Selection>,
    page: Option<read::PageToken>,
    job: Job,
) {
    let result = run_inner(
        &owner,
        &session,
        &connection,
        generation,
        selection,
        page,
        &job,
    )
    .await;
    if !job.cancel.is_cancelled()
        && let Err(stop) = result
    {
        owner.configuration_watch_stopped(job.permit, stop);
    }
}
async fn run_inner(
    owner: &Administration,
    session: &SessionCoordinator,
    connection: &Connection,
    generation: Generation,
    selection: Option<read::Selection>,
    page: Option<read::PageToken>,
    job: &Job,
) -> Result<(), Stop> {
    let mut budget = Budget::default();
    let mut page = page;
    loop {
        if job.cancel.is_cancelled() || !owner.configuration_watch_syncing(job.permit) {
            return Ok(());
        }
        let started = std::time::Instant::now();
        let round = subscription(
            owner,
            session,
            connection,
            generation,
            selection.clone(),
            page.clone(),
            job,
        )
        .await;
        if job.cancel.is_cancelled() {
            return Ok(());
        }
        match round {
            Ok(Cycle::Refresh) => {
                page = None;
                continue;
            }
            Ok(Cycle::Retired) => return Ok(()),
            Ok(Cycle::Recapture) | Err(Stop::Unavailable) => {
                page = None;
                let Some(delay) = budget.delay(started.elapsed() >= Duration::from_secs(30)) else {
                    return Err(Stop::Unavailable);
                };
                if !owner.configuration_watch_syncing(job.permit) {
                    return Ok(());
                }
                tokio::select! { biased; ()=job.cancel.cancelled()=>return Ok(()), ()=tokio::time::sleep(delay)=>{} }
            }
            Err(Stop::Denied) => return Err(Stop::Denied),
            Err(Stop::Invalid) => return Err(Stop::Invalid),
        }
    }
}
async fn subscription(
    owner: &Administration,
    session: &SessionCoordinator,
    connection: &Connection,
    generation: Generation,
    selection: Option<read::Selection>,
    page: Option<read::PageToken>,
    job: &Job,
) -> Result<Cycle, Stop> {
    let loaded = read::load(
        session,
        connection,
        generation,
        Section::Configuration,
        selection,
        page.clone(),
        &job.cancel,
    )
    .await
    .map_err(read_stop)?;
    if !loaded.authority.administrator() || loaded.section != Section::Configuration {
        return Err(Stop::Denied);
    }
    let cursor = match &loaded.payload {
        Ok(read::Payload::Configuration(value)) => value.root.resume_token().to_owned(),
        Ok(
            read::Payload::Personal { .. }
            | read::Payload::Sessions(_)
            | read::Payload::Factors(_)
            | read::Payload::MfaPolicy(_)
            | read::Payload::Keys(_)
            | read::Payload::Accounts(_)
            | read::Payload::Invitations(_)
            | read::Payload::Policy { .. }
            | read::Payload::Integrations(_)
            | read::Payload::Integration(_)
            | read::Payload::Health(_)
            | read::Payload::Metrics(_)
            | read::Payload::Logs(_)
            | read::Payload::Audits(_),
        ) => return Err(Stop::Invalid),
        Err(value) => return Err(read_stop(*value)),
    };
    if !owner.configuration_snapshot(job.permit, loaded, page)? {
        return Ok(Cycle::Retired);
    }
    let lease = read::admitted_lease(session, generation, &job.cancel)
        .await
        .map_err(read_stop)?;
    let api = connection.configuration();
    let opening = tokio::time::timeout(
        Duration::from_secs(4),
        api.watch(
            wire::WatchConfigurationRequest {
                resume_token: cursor.clone(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(300),
            &job.cancel,
        ),
    )
    .await;
    let mut stream = match opening {
        Ok(Ok(stream)) => stream,
        Ok(Err(error)) => {
            let stop = transport_stop(&error);
            let _ = session.observe_failure(lease.generation, &error).await;
            return Err(stop);
        }
        Err(_) => return Err(Stop::Unavailable),
    };
    let mut protocol = Protocol::new(cursor);
    loop {
        let value = tokio::select! { biased;
            ()=job.cancel.cancelled()=>return Ok(Cycle::Retired),
            ()=lease.cancellation.cancelled()=>return Err(Stop::Unavailable),
            ()=tokio::time::sleep(if protocol.ready { Duration::from_secs(300) } else { Duration::from_secs(4) })=>return Err(Stop::Unavailable),
            result=stream.next()=>result,
        };
        let value = match value {
            Ok(Some(value)) => value,
            Ok(None) => return Err(Stop::Unavailable),
            Err(error) => {
                let result = transport_stop(&error);
                let _ = session.observe_failure(lease.generation, &error).await;
                return Err(result);
            }
        };
        match protocol.admit(&value)? {
            Event::Ready => {
                if !owner.configuration_watch_ready(job.permit) {
                    return Ok(Cycle::Retired);
                }
            }
            Event::Refresh => return Ok(Cycle::Refresh),
            Event::Recapture => return Ok(Cycle::Recapture),
        }
    }
}

#[derive(Default)]
struct Budget {
    failures: u8,
}
impl Budget {
    fn delay(&mut self, stable: bool) -> Option<Duration> {
        if stable {
            self.failures = 0;
        }
        self.failures = self.failures.checked_add(1)?;
        if self.failures > 3 {
            return None;
        }
        Some(Duration::from_millis(200u64 << (self.failures - 1)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn ready(cursor: &str) -> wire::WatchConfigurationResponse {
        wire::WatchConfigurationResponse {
            result: Some(wire::watch_configuration_response::Result::Ready(Box::new(
                wire::WatchReady {
                    resume_token: cursor.into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
    }
    fn resync(reason: wire::ResyncReason) -> wire::WatchConfigurationResponse {
        wire::WatchConfigurationResponse {
            result: Some(wire::watch_configuration_response::Result::ResyncRequired(
                Box::new(wire::ResyncRequired {
                    reason: reason.into(),
                    ..Default::default()
                }),
            )),
            ..Default::default()
        }
    }
    #[test]
    fn readiness_requires_the_exact_cursor_and_is_not_repeatable() {
        let cursor = "a".repeat(118);
        let mut protocol = Protocol::new(cursor.clone());
        assert_eq!(protocol.admit(&ready(&"b".repeat(118))), Err(Stop::Invalid));
        assert_eq!(protocol.admit(&ready(&cursor)), Ok(Event::Ready));
        assert_eq!(protocol.admit(&ready(&cursor)), Err(Stop::Invalid));
        assert_eq!(
            protocol.admit(&resync(wire::ResyncReason::AuthorityChanged)),
            Err(Stop::Denied)
        );
        assert_eq!(
            protocol.admit(&resync(wire::ResyncReason::ActivationSignal)),
            Ok(Event::Refresh)
        );
    }
    #[test]
    fn subscription_replacement_retires_old_readiness_and_stopped_work_never_restarts_itself() {
        let mut cohort = Cohort::default();
        let old = cohort.start().unwrap();
        assert!(!cohort.live());
        assert!(cohort.start().is_none());
        assert!(cohort.ready(old.permit));
        cohort.restart();
        let new = cohort.start().unwrap();
        assert!(old.cancel.is_cancelled());
        assert_ne!(old.permit, new.permit);
        assert!(!cohort.ready(old.permit));
        assert!(!cohort.stop(old.permit));
        assert!(cohort.ready(new.permit));
        assert!(cohort.syncing(new.permit));
        assert!(!cohort.live());
        assert!(cohort.stop(new.permit));
        assert!(cohort.start().is_none());
        assert!(!cohort.ready(new.permit));
        cohort.restart();
        assert!(cohort.start().is_some());
    }
    #[test]
    fn an_exhausted_subscription_identity_is_never_reused() {
        let mut cohort = Cohort {
            serial: Some(u64::MAX),
            ..Default::default()
        };
        assert!(cohort.start().is_none());
        cohort.restart();
        assert!(cohort.start().is_none());
    }
    #[test]
    fn changes_before_ready_are_refused_and_early_resyncs_consume_a_finite_budget() {
        let mut protocol = Protocol::new("a".repeat(118));
        let change = wire::WatchConfigurationResponse {
            result: Some(wire::watch_configuration_response::Result::Changes(
                Box::new(wire::ChangeBatch {
                    resume_token: "b".repeat(118),
                    changes: vec![wire::DocumentChange {
                        id: wire::DocumentId {
                            value: "operational.main".into(),
                            ..Default::default()
                        }
                        .into(),
                        version: wire::DocumentVersion {
                            revision: 7,
                            source_sha256: vec![1; 32],
                            ..Default::default()
                        }
                        .into(),
                        dirty: wire::DirtyState::Unvalidated.into(),
                        observed_workspace_sha256: Some(vec![2; 32]),
                        ..Default::default()
                    }],
                    ..Default::default()
                }),
            )),
            ..Default::default()
        };
        assert_eq!(protocol.admit(&change), Err(Stop::Invalid));
        assert_eq!(
            protocol.admit(&resync(wire::ResyncReason::ActivationSignal)),
            Ok(Event::Recapture)
        );
        protocol.admit(&ready(&"a".repeat(118))).unwrap();
        assert_eq!(protocol.admit(&change), Ok(Event::Refresh));
        let mut budget = Budget::default();
        for expected in [200, 400, 800] {
            assert_eq!(budget.delay(false), Some(Duration::from_millis(expected)));
        }
        assert_eq!(budget.delay(false), None);
        assert_eq!(budget.delay(true), Some(Duration::from_millis(200)));
    }
}
