use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, AtomicI64, AtomicUsize, Ordering},
};

use async_trait::async_trait;
use buffa_types::google::protobuf::Timestamp;
use connectrpc::ConnectError;
use home_client_transport::Failure;
use home_wire_contract::home::auth::v1 as auth;
use tokio::sync::{Notify, Semaphore};
use tokio_util::sync::CancellationToken;
use zeroize::Zeroizing;

use crate::{
    Phase, RecordFailure, SecretRecord, SecureStore, SessionCoordinator, SessionFailure,
    StoreFailure, api::AuthApi, coordinator::Clock,
};

const BASE: i64 = 1_800_000_000;
struct TestClock(AtomicI64);
impl Clock for TestClock {
    fn now(&self) -> Result<Timestamp, SessionFailure> {
        Ok(timestamp(self.0.load(Ordering::SeqCst)))
    }
}
fn timestamp(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}

#[derive(Default)]
struct MemoryStore {
    record: Mutex<Option<Zeroizing<Vec<u8>>>>,
    fail_save: AtomicBool,
    clear_calls: AtomicUsize,
}
impl SecureStore for MemoryStore {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.record.lock().unwrap().clone())
    }
    fn save(&self, record: &[u8]) -> Result<(), StoreFailure> {
        if self.fail_save.load(Ordering::SeqCst) {
            return Err(StoreFailure::Unavailable);
        }
        *self.record.lock().unwrap() = Some(Zeroizing::new(record.to_vec()));
        Ok(())
    }
    fn clear(&self) -> Result<(), StoreFailure> {
        self.clear_calls.fetch_add(1, Ordering::SeqCst);
        *self.record.lock().unwrap() = None;
        Ok(())
    }
}

struct TestApi {
    clock: Arc<TestClock>,
    store: Arc<MemoryStore>,
    refresh_calls: AtomicUsize,
    started: Notify,
    release: Semaphore,
    lost_reply: AtomicBool,
    ignore_cancel: AtomicBool,
    rate_limit_refresh: AtomicBool,
    server_clock_offset: AtomicI64,
    access_lifetime: AtomicI64,
}
impl TestApi {
    fn tokens(&self, account: &str, rotated: bool) -> auth::SessionTokens {
        let offset = self.server_clock_offset.load(Ordering::SeqCst);
        let now = self.clock.0.load(Ordering::SeqCst) + offset;
        auth::SessionTokens {
            session: auth::Session {
                id: auth::SessionId {
                    value: format!("session-{account}"),
                    ..Default::default()
                }
                .into(),
                account_id: auth::AccountId {
                    value: format!("account-{account}"),
                    ..Default::default()
                }
                .into(),
                created_at: timestamp(BASE - 1 + offset).into(),
                expires_at: timestamp(BASE + 3600 + offset).into(),
                authentication_satisfaction: auth::AuthenticationSatisfaction::PasswordOnly.into(),
                authenticated_at: timestamp(BASE - 1 + offset).into(),
                ..Default::default()
            }
            .into(),
            audience: auth::CredentialAudience::Interactive.into(),
            access_token: if rotated {
                "access-rotated"
            } else {
                "access-initial"
            }
            .into(),
            refresh_token: if rotated {
                "refresh-rotated"
            } else {
                "refresh-initial"
            }
            .into(),
            access_expires_at: timestamp(now + self.access_lifetime.load(Ordering::SeqCst)).into(),
            ..Default::default()
        }
    }
}
#[async_trait]
impl AuthApi for TestApi {
    async fn revoke(
        &self,
        _: auth::RevokeSessionRequest,
        _: &home_client_transport::AccessToken,
        _: &CancellationToken,
    ) -> Result<(), Failure> {
        Ok(())
    }
    async fn start_login(
        &self,
        request: auth::StartLoginRequest,
        _: &CancellationToken,
    ) -> Result<auth::StartLoginResponse, Failure> {
        Ok(auth::StartLoginResponse {
            decision: auth::LoginDecision {
                outcome: Some(auth::login_decision::Outcome::Authenticated(Box::new(
                    self.tokens(&request.login_name, false),
                ))),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })
    }
    async fn complete_login(
        &self,
        _: auth::CompleteLoginRequest,
        _: &CancellationToken,
    ) -> Result<auth::CompleteLoginResponse, Failure> {
        Err(ConnectError::unimplemented("not used").into())
    }
    async fn refresh(
        &self,
        _: auth::RefreshSessionRequest,
        cancel: &CancellationToken,
    ) -> Result<auth::RefreshSessionResponse, Failure> {
        self.refresh_calls.fetch_add(1, Ordering::SeqCst);
        let saved = self.store.load().unwrap().unwrap();
        assert!(
            matches!(
                SecretRecord::decode(&saved, &self.clock.now().unwrap()),
                Err(RecordFailure::RefreshConsumed)
            ),
            "dispatch preceded durable burn"
        );
        self.started.notify_one();
        if self.ignore_cancel.load(Ordering::SeqCst) {
            self.release.acquire().await.unwrap().forget();
        } else {
            tokio::select! {
                biased;
                () = cancel.cancelled() => return Err(ConnectError::canceled("synthetic cancellation").into()),
                permit = self.release.acquire() => permit.unwrap().forget(),
            }
        }
        if self.lost_reply.load(Ordering::SeqCst) {
            return Err(ConnectError::unavailable("synthetic lost reply").into());
        }
        if self.rate_limit_refresh.load(Ordering::SeqCst) {
            return Err(ConnectError::resource_exhausted("synthetic limit").into());
        }
        Ok(auth::RefreshSessionResponse {
            tokens: self.tokens("alice", true).into(),
            ..Default::default()
        })
    }
}

fn fixture() -> (
    SessionCoordinator,
    Arc<TestApi>,
    Arc<TestClock>,
    Arc<MemoryStore>,
) {
    let clock = Arc::new(TestClock(AtomicI64::new(BASE)));
    let store = Arc::new(MemoryStore::default());
    let api = Arc::new(TestApi {
        clock: clock.clone(),
        store: store.clone(),
        refresh_calls: AtomicUsize::new(0),
        started: Notify::new(),
        release: Semaphore::new(0),
        lost_reply: AtomicBool::new(false),
        ignore_cancel: AtomicBool::new(false),
        rate_limit_refresh: AtomicBool::new(false),
        server_clock_offset: AtomicI64::new(0),
        access_lifetime: AtomicI64::new(120),
    });
    (
        SessionCoordinator::with_parts(api.clone(), store.clone(), clock.clone()),
        api,
        clock,
        store,
    )
}

fn termination(reason: auth::AuthorizationTerminationReason) -> Failure {
    use buffa::MessageName;
    ConnectError::unauthenticated("synthetic termination")
        .with_detail(connectrpc::ErrorDetail::from_message(
            auth::AuthorizationTermination::FULL_NAME,
            &auth::AuthorizationTermination {
                reason: reason.into(),
                ..Default::default()
            },
        ))
        .into()
}

#[tokio::test]
async fn independent_clocks_restore_normally_and_refresh_before_the_server_deadline() {
    let (owner, api, clock, store) = fixture();
    api.server_clock_offset.store(1, Ordering::SeqCst);
    api.access_lifetime.store(900, Ordering::SeqCst);
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    assert!(owner.access(&CancellationToken::new()).await.is_ok());
    owner.close().await;
    // Admission records the real local time, not a future adjusted timestamp.
    let restored = SessionCoordinator::with_parts(api.clone(), store, clock.clone());
    restored.restore().await.unwrap();
    assert!(restored.access(&CancellationToken::new()).await.is_ok());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 0);
    clock.0.store(BASE + 871, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(restored.access(&CancellationToken::new()).await.is_ok());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    clock.0.store(BASE + 3571, Ordering::SeqCst);
    assert_eq!(
        restored.revalidate_lifetime().await,
        Err(SessionFailure::NeedsLogin)
    );
    assert_eq!(restored.subscribe().borrow().phase, Phase::NeedsLogin);
    assert_eq!(
        api.refresh_calls.load(Ordering::SeqCst),
        1,
        "absolute expiry must not refresh"
    );
    restored.close().await;
}

#[tokio::test]
async fn close_joins_a_restore_waiting_to_spawn_its_os_task_and_refuses_later_writes() {
    let (owner, _, _, store) = fixture();
    let persistence = owner.inner.persistence.clone().lock_owned().await;
    let mut changes = owner.subscribe();
    let restore = {
        let owner = owner.clone();
        tokio::spawn(async move { owner.restore().await })
    };
    while changes.borrow().phase != Phase::Busy {
        changes.changed().await.unwrap();
    }
    let mut closing = {
        let owner = owner.clone();
        tokio::spawn(async move { owner.close().await })
    };
    assert!(
        tokio::time::timeout(std::time::Duration::from_millis(20), &mut closing)
            .await
            .is_err(),
        "close returned before the queued OS operation joined"
    );
    drop(persistence);
    assert_eq!(restore.await.unwrap(), Err(SessionFailure::Cancelled));
    closing.await.unwrap();
    assert_eq!(owner.subscribe().borrow().phase, Phase::Closed);
    assert_eq!(owner.logout_local().await, Err(SessionFailure::Closed));
    assert_eq!(owner.logout().await, Err(SessionFailure::Closed));
    assert_eq!(owner.restore().await, Err(SessionFailure::Closed));
    assert_eq!(
        store.clear_calls.load(Ordering::SeqCst),
        0,
        "a closed coordinator performed an OS write"
    );
}

#[tokio::test]
async fn network_failure_keeps_session_authority_failure_suspends_and_revocation_deletes() {
    let (owner, _, _, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    let initial = owner.access(&CancellationToken::new()).await.unwrap();
    let network = Failure::from(ConnectError::unavailable("synthetic network failure"));
    assert_eq!(
        owner
            .observe_failure(initial.generation, &network)
            .await
            .unwrap(),
        crate::SessionReaction::Unchanged
    );
    assert_eq!(owner.subscribe().borrow().phase, Phase::Ready);
    assert_eq!(
        owner
            .observe_failure(
                initial.generation,
                &termination(auth::AuthorizationTerminationReason::AuthorityUnavailable)
            )
            .await
            .unwrap(),
        crate::SessionReaction::ViewSuspended
    );
    assert!(initial.cancellation.is_cancelled());
    assert!(store.load().unwrap().is_some());
    assert!(matches!(
        owner.access(&CancellationToken::new()).await,
        Err(SessionFailure::AuthorityUnavailable)
    ));
    owner.allow_new_view_attempt().await.unwrap();
    let resumed = owner.access(&CancellationToken::new()).await.unwrap();
    assert_eq!(
        owner
            .observe_failure(
                initial.generation,
                &termination(auth::AuthorizationTerminationReason::CredentialRevoked)
            )
            .await
            .unwrap(),
        crate::SessionReaction::IgnoredOldGeneration
    );
    assert_eq!(owner.subscribe().borrow().phase, Phase::Ready);
    assert_eq!(
        owner
            .observe_failure(
                resumed.generation,
                &termination(auth::AuthorizationTerminationReason::CredentialRevoked)
            )
            .await
            .unwrap(),
        crate::SessionReaction::CredentialsDiscarded
    );
    assert!(resumed.cancellation.is_cancelled());
    assert!(store.load().unwrap().is_none());
    assert_eq!(owner.subscribe().borrow().phase, Phase::NeedsLogin);
    owner.close().await;
}

#[tokio::test]
async fn rights_change_resets_the_view_without_revoking_the_native_session() {
    let (owner, _, _, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    let initial = owner.access(&CancellationToken::new()).await.unwrap();
    assert_eq!(
        owner
            .observe_failure(
                initial.generation,
                &termination(auth::AuthorizationTerminationReason::RightsChanged)
            )
            .await
            .unwrap(),
        crate::SessionReaction::ViewReset
    );
    assert!(initial.cancellation.is_cancelled());
    assert!(store.load().unwrap().is_some());
    assert_eq!(owner.subscribe().borrow().phase, Phase::RightsChanged);
    owner.close().await;
}

#[tokio::test]
async fn refresh_rate_limit_has_a_distinct_state_without_a_retry_loop() {
    let (owner, api, clock, _) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.rate_limit_refresh.store(true, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert_eq!(owner.subscribe().borrow().phase, Phase::RateLimited);
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    owner.close().await;
}

#[tokio::test]
async fn cancelling_one_waiter_does_not_cancel_the_shared_refresh() {
    let (owner, api, clock, _) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    let cancel = CancellationToken::new();
    let first = {
        let owner = owner.clone();
        let cancel = cancel.clone();
        tokio::spawn(async move { owner.access(&cancel).await })
    };
    api.started.notified().await;
    let second = {
        let owner = owner.clone();
        tokio::spawn(async move { owner.access(&CancellationToken::new()).await })
    };
    cancel.cancel();
    assert!(matches!(
        first.await.unwrap(),
        Err(SessionFailure::Cancelled)
    ));
    api.release.add_permits(1);
    second.await.unwrap().unwrap();
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    owner.close().await;
}

#[tokio::test]
async fn ten_expired_access_requests_share_one_durably_burned_refresh() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    let initial = owner.access(&CancellationToken::new()).await.unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    let mut readers = Vec::new();
    for _ in 0..10 {
        let owner = owner.clone();
        readers.push(tokio::spawn(async move {
            owner.access(&CancellationToken::new()).await
        }));
    }
    api.started.notified().await;
    assert!(initial.cancellation.is_cancelled());
    api.release.add_permits(1);
    for reader in readers {
        let lease = reader.await.unwrap().unwrap();
        assert!(owner.is_current(lease.generation));
    }
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    let saved = store.load().unwrap().unwrap();
    assert!(SecretRecord::decode(&saved, &clock.now().unwrap()).is_ok());
    owner.close().await;
}

#[tokio::test]
async fn lost_refresh_response_never_replays_after_restart() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.lost_reply.store(true, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    owner.close().await;
    let restarted = SessionCoordinator::with_parts(api.clone(), store, clock);
    assert_eq!(restarted.restore().await, Err(SessionFailure::NeedsLogin));
    assert!(restarted.access(&CancellationToken::new()).await.is_err());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    restarted.close().await;
}

#[tokio::test]
async fn failed_durable_burn_prevents_refresh_dispatch() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    store.fail_save.store(true, Ordering::SeqCst);
    assert!(matches!(
        owner.access(&CancellationToken::new()).await,
        Err(SessionFailure::Store(StoreFailure::Unavailable))
    ));
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 0);
    assert_eq!(owner.subscribe().borrow().phase, Phase::StorageUnavailable);
    owner.close().await;
}

#[tokio::test]
async fn late_refresh_cannot_restore_the_previous_account_after_logout_and_login() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.ignore_cancel.store(true, Ordering::SeqCst);
    let waiter = {
        let owner = owner.clone();
        tokio::spawn(async move { owner.access(&CancellationToken::new()).await })
    };
    api.started.notified().await;
    owner.logout_local().await.unwrap();
    owner
        .login("bob".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    let generation = owner.generation();
    api.release.add_permits(1);
    assert!(matches!(
        waiter.await.unwrap(),
        Err(SessionFailure::Cancelled)
    ));
    // Wait for all tracked work without closing the active session first.
    owner.inner.tasks.close();
    owner.inner.tasks.wait().await;
    assert_eq!(owner.generation(), generation);
    let saved = store.load().unwrap().unwrap();
    let current = SecretRecord::decode(&saved, &clock.now().unwrap()).unwrap();
    assert_eq!(current.tokens.session.account_id.value, "account-bob");
    owner.close().await;
}

#[tokio::test]
async fn idle_lifetime_check_clears_absolute_expiry_without_dispatching_a_refresh() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    let lease = owner.access(&CancellationToken::new()).await.unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    assert_eq!(owner.revalidate_lifetime().await, Ok(()));
    assert!(store.load().unwrap().is_some());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 0);
    clock.0.store(BASE + 3600, Ordering::SeqCst);
    assert_eq!(
        owner.revalidate_lifetime().await,
        Err(SessionFailure::NeedsLogin)
    );
    assert!(lease.cancellation.is_cancelled());
    assert!(store.load().unwrap().is_none());
    assert_eq!(owner.subscribe().borrow().phase, Phase::NeedsLogin);
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 0);
    owner.close().await;
}

#[tokio::test]
async fn first_launch_with_an_empty_os_store_is_signed_out_without_a_refusal() {
    let (owner, api, _, store) = fixture();
    assert_eq!(owner.restore().await, Ok(()));
    assert_eq!(owner.subscribe().borrow().phase, Phase::SignedOut);
    assert_eq!(owner.subscribe().borrow().failure, None);
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert!(store.load().unwrap().is_none());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 0);
    owner.close().await;
}
