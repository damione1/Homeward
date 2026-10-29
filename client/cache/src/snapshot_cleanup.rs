//! Bounded, profile-owned pagination cleanup. No observations, persistent jobs,
//! credential refresh or device commands. Unconfirmed cleanup (lost replies,
//! full queue or profile exit) falls back to the server's existing60s TTL;
//! cleanup never retries or weakens its16-view bound.
use async_trait::async_trait;
use home_client_session::{AccessLease, SessionCoordinator};
use home_client_transport::{AccessToken, Failure, StateTransport};
use home_wire_contract::home::state::v1 as wire;
use std::{sync::Arc, time::Duration};
use tokio::{sync::mpsc, task::JoinHandle};
use tokio_util::sync::CancellationToken;

struct SnapshotToken(String);
impl SnapshotToken {
    fn parse(value: &str) -> Option<Self> {
        (value.len() == 64
            && value
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)))
        .then(|| Self(value.into()))
    }
}
struct Release {
    snapshot: SnapshotToken,
    token: AccessToken,
    cancel: CancellationToken,
    admitted: Box<dyn Fn() -> bool + Send + Sync>,
}
#[async_trait]
trait Api: Send + Sync + 'static {
    async fn release(&self, value: &Release, stop: &CancellationToken) -> Result<(), Failure>;
}
#[async_trait]
impl Api for StateTransport {
    async fn release(&self, value: &Release, stop: &CancellationToken) -> Result<(), Failure> {
        self.release_view(
            wire::ReleaseViewRequest {
                snapshot_token: value.snapshot.0.clone(),
                ..Default::default()
            },
            &value.token,
            Duration::from_secs(2),
            stop,
        )
        .await
    }
}

pub(crate) struct CleanupOwner {
    stop: CancellationToken,
    task: Option<JoinHandle<()>>,
}
#[derive(Clone)]
pub(crate) struct CleanupHandle {
    sender: mpsc::Sender<Release>,
    session: SessionCoordinator,
}
pub(crate) struct ReleaseGuard {
    sender: mpsc::Sender<Release>,
    value: Option<Release>,
}
impl Drop for ReleaseGuard {
    fn drop(&mut self) {
        if let Some(value) = self.value.take() {
            // Nonblocking even during synchronous future cancellation. A full
            // or closed cleanup queue leaves only the existing bounded TTL.
            let _ = self.sender.try_send(value);
        }
    }
}
impl CleanupHandle {
    pub(crate) fn track(&self, snapshot: &str, lease: &AccessLease) -> Option<ReleaseGuard> {
        let snapshot = SnapshotToken::parse(snapshot)?;
        let session = self.session.clone();
        let generation = lease.generation;
        let cancel = lease.cancellation.clone();
        let admission = cancel.clone();
        Some(ReleaseGuard {
            sender: self.sender.clone(),
            value: Some(Release {
                snapshot,
                token: lease.token().clone(),
                cancel,
                admitted: Box::new(move || {
                    session.is_current(generation) && !admission.is_cancelled()
                }),
            }),
        })
    }
}
impl CleanupOwner {
    pub(crate) fn start(
        session: SessionCoordinator,
        transport: StateTransport,
    ) -> (Self, CleanupHandle) {
        let (owner, sender) = Self::build(Arc::new(transport));
        (owner, CleanupHandle { sender, session })
    }
    fn build(api: Arc<dyn Api>) -> (Self, mpsc::Sender<Release>) {
        let (sender, mut receiver) = mpsc::channel::<Release>(8);
        let stop = CancellationToken::new();
        let cancelled = stop.clone();
        let task = tokio::spawn(async move {
            loop {
                let value = tokio::select! { biased;
                    () = cancelled.cancelled() => break,
                    value = receiver.recv() => match value { Some(value) => value, None => break },
                };
                if !(value.admitted)() || value.cancel.is_cancelled() {
                    continue;
                }
                tokio::select! { biased;
                    () = cancelled.cancelled() => break,
                    () = value.cancel.cancelled() => {},
                    _ = api.release(&value, &cancelled) => {},
                }
            }
        });
        (
            Self {
                stop,
                task: Some(task),
            },
            sender,
        )
    }
    pub(crate) async fn close(mut self) {
        self.stop.cancel();
        if let Some(task) = self.task.take() {
            let _ = task.await;
        }
    }
}
impl Drop for CleanupOwner {
    fn drop(&mut self) {
        self.stop.cancel();
        if let Some(task) = self.task.take() {
            task.abort();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Mutex,
        atomic::{AtomicBool, Ordering},
    };
    use tokio::sync::Notify;

    #[derive(Default)]
    struct Fake {
        calls: Mutex<Vec<String>>,
        entered: Notify,
        first_gate: Mutex<Option<Arc<Notify>>>,
    }
    #[async_trait]
    impl Api for Fake {
        async fn release(&self, value: &Release, _stop: &CancellationToken) -> Result<(), Failure> {
            self.calls.lock().unwrap().push(value.snapshot.0.clone());
            self.entered.notify_one();
            let gate = self.first_gate.lock().unwrap().take();
            if let Some(gate) = gate {
                gate.notified().await;
            }
            Ok(())
        }
    }
    fn value(index: u64) -> Release {
        Release {
            snapshot: SnapshotToken::parse(&format!("{index:064x}")).unwrap(),
            token: AccessToken::parse("synthetic-access").unwrap(),
            cancel: CancellationToken::new(),
            admitted: Box::new(|| true),
        }
    }
    async fn entered(api: &Fake) {
        tokio::time::timeout(Duration::from_secs(1), api.entered.notified())
            .await
            .unwrap();
    }
    #[tokio::test]
    async fn dropping_a_finished_or_abandoned_guard_releases_once() {
        let api = Arc::new(Fake::default());
        let (owner, sender) = CleanupOwner::build(api.clone());
        drop(ReleaseGuard {
            sender,
            value: Some(value(1)),
        });
        entered(&api).await;
        owner.close().await;
        assert_eq!(*api.calls.lock().unwrap(), vec![format!("{:064x}", 1)]);
    }
    #[tokio::test]
    async fn queued_cleanup_rechecks_admission_and_access_cancellation() {
        let gate = Arc::new(Notify::new());
        let api = Arc::new(Fake {
            first_gate: Mutex::new(Some(gate.clone())),
            ..Default::default()
        });
        let (owner, sender) = CleanupOwner::build(api.clone());
        sender.send(value(1)).await.ok().unwrap();
        entered(&api).await;
        let admitted = Arc::new(AtomicBool::new(true));
        let current = admitted.clone();
        let mut stale = value(2);
        stale.admitted = Box::new(move || current.load(Ordering::SeqCst));
        sender.send(stale).await.ok().unwrap();
        let cancelled = value(3);
        cancelled.cancel.cancel();
        sender.send(cancelled).await.ok().unwrap();
        sender.send(value(4)).await.ok().unwrap();
        admitted.store(false, Ordering::SeqCst);
        gate.notify_one();
        entered(&api).await;
        owner.close().await;
        assert_eq!(
            *api.calls.lock().unwrap(),
            vec![format!("{:064x}", 1), format!("{:064x}", 4)]
        );
    }
    #[tokio::test]
    async fn close_joins_an_inflight_call_and_the_pending_queue_stays_bounded() {
        let api = Arc::new(Fake {
            first_gate: Mutex::new(Some(Arc::new(Notify::new()))),
            ..Default::default()
        });
        let (owner, sender) = CleanupOwner::build(api.clone());
        sender.send(value(1)).await.ok().unwrap();
        entered(&api).await;
        for index in 2..=9 {
            sender.try_send(value(index)).ok().unwrap();
        }
        assert_eq!(sender.capacity(), 0);
        drop(ReleaseGuard {
            sender: sender.clone(),
            value: Some(value(10)),
        });
        assert_eq!(sender.capacity(), 0);
        tokio::time::timeout(Duration::from_secs(1), owner.close())
            .await
            .unwrap();
        assert!(sender.is_closed());
        assert_eq!(api.calls.lock().unwrap().len(), 1);
    }
}
