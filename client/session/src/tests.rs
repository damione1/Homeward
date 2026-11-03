use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, AtomicI32, AtomicI64, AtomicUsize, Ordering},
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
    Phase, RefreshState, SecretRecord, SecureStore, SessionCoordinator, SessionFailure,
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
    fail_clear: AtomicBool,
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
        if self.fail_clear.load(Ordering::SeqCst) {
            return Err(StoreFailure::Unavailable);
        }
        *self.record.lock().unwrap() = None;
        Ok(())
    }
}

struct TestApi {
    clock: Arc<TestClock>,
    store: Arc<MemoryStore>,
    refresh_calls: AtomicUsize,
    renewal_keys: Mutex<Vec<Vec<u8>>>,
    refresh_error: Mutex<Option<auth::AuthError>>,
    bare_refresh_unauthenticated: AtomicBool,
    started: Notify,
    release: Semaphore,
    lost_reply: AtomicBool,
    ignore_cancel: AtomicBool,
    rate_limit_refresh: AtomicBool,
    server_clock_offset: AtomicI64,
    access_lifetime: AtomicI64,
    issued_nanos: AtomicI32,
    rotated_nanos: AtomicI32,
    refused_connect: AtomicBool,
    gateway_offline: AtomicBool,
    bare_preflight_unauthenticated: AtomicBool,
    preflight_reason: Mutex<Option<auth::AuthorizationTerminationReason>>,
    block_preflight: AtomicBool,
    preflight_started: Notify,
    preflight_cancelled: Notify,
    preflight_release: Semaphore,
}
impl TestApi {
    fn tokens(&self, account: &str, rotated: bool) -> auth::SessionTokens {
        let offset = self.server_clock_offset.load(Ordering::SeqCst);
        let now = self.clock.0.load(Ordering::SeqCst) + offset;
        let nanos = if rotated {
            self.rotated_nanos.load(Ordering::SeqCst)
        } else {
            self.issued_nanos.load(Ordering::SeqCst)
        };
        let precise = |seconds| Timestamp {
            seconds,
            nanos,
            ..Default::default()
        };
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
                created_at: precise(BASE - 1 + offset).into(),
                expires_at: precise(now + 3600).into(),
                authentication_satisfaction: auth::AuthenticationSatisfaction::PasswordOnly.into(),
                authenticated_at: precise(BASE - 1 + offset).into(),
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
    async fn preflight_refresh(
        &self,
        _: auth::ListSessionsRequest,
        _: &home_client_transport::AccessToken,
        cancel: &CancellationToken,
    ) -> Result<(), Failure> {
        if self.block_preflight.load(Ordering::SeqCst) {
            self.preflight_started.notify_one();
            cancel.cancelled().await;
            self.preflight_cancelled.notify_one();
            self.preflight_release.acquire().await.unwrap().forget();
            return Err(ConnectError::canceled("synthetic preflight cancelled").into());
        }
        let reason = *self.preflight_reason.lock().unwrap();
        if let Some(reason) = reason {
            return Err(termination(reason));
        }
        if self.gateway_offline.load(Ordering::SeqCst) {
            return Err(ConnectError::unavailable("synthetic proxy backend unavailable").into());
        }
        if self.bare_preflight_unauthenticated.load(Ordering::SeqCst) {
            return Err(ConnectError::unauthenticated(
                "synthetic expired access without terminal detail",
            )
            .into());
        }
        Ok(())
    }
    async fn refresh(
        &self,
        request: auth::RefreshSessionRequest,
        cancel: &CancellationToken,
    ) -> Result<auth::RefreshSessionResponse, Failure> {
        self.refresh_calls.fetch_add(1, Ordering::SeqCst);
        let saved = self.store.load().unwrap().unwrap();
        let persisted = SecretRecord::decode(&saved, &self.clock.now().unwrap()).unwrap();
        assert_eq!(request.renewal_key.len(), 32);
        assert_ne!(request.renewal_key.as_slice(), &[0; 32]);
        assert_eq!(
            persisted.refresh,
            RefreshState::Pending(request.renewal_key.as_slice().try_into().unwrap()),
            "dispatch preceded durable renewal identity"
        );
        self.renewal_keys
            .lock()
            .unwrap()
            .push(request.renewal_key.to_vec());
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
        if self.refused_connect.load(Ordering::SeqCst) {
            let socket = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let origin = format!("https://{}", socket.local_addr().unwrap());
            drop(socket);
            return home_client_transport::Connection::new(&origin, &[])
                .unwrap()
                .auth()
                .refresh_session(
                    auth::RefreshSessionRequest {
                        refresh_token: "synthetic-unstarted-only".into(),
                        ..Default::default()
                    },
                    std::time::Duration::from_secs(2),
                    cancel,
                )
                .await;
        }
        if self.lost_reply.load(Ordering::SeqCst) {
            return Err(ConnectError::unavailable("synthetic lost reply").into());
        }
        if let Some(detail) = self.refresh_error.lock().unwrap().clone() {
            use buffa::MessageName;
            return Err(
                ConnectError::permission_denied("synthetic terminal renewal refusal")
                    .with_detail(connectrpc::ErrorDetail::from_message(
                        auth::AuthError::FULL_NAME,
                        &detail,
                    ))
                    .into(),
            );
        }
        if self.bare_refresh_unauthenticated.load(Ordering::SeqCst) {
            return Err(ConnectError::unauthenticated("synthetic untyped proxy refusal").into());
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
        renewal_keys: Mutex::new(Vec::new()),
        refresh_error: Mutex::new(None),
        bare_refresh_unauthenticated: AtomicBool::new(false),
        started: Notify::new(),
        release: Semaphore::new(0),
        lost_reply: AtomicBool::new(false),
        ignore_cancel: AtomicBool::new(false),
        rate_limit_refresh: AtomicBool::new(false),
        server_clock_offset: AtomicI64::new(0),
        access_lifetime: AtomicI64::new(120),
        issued_nanos: AtomicI32::new(0),
        rotated_nanos: AtomicI32::new(0),
        refused_connect: AtomicBool::new(false),
        gateway_offline: AtomicBool::new(false),
        bare_preflight_unauthenticated: AtomicBool::new(false),
        preflight_reason: Mutex::new(None),
        block_preflight: AtomicBool::new(false),
        preflight_started: Notify::new(),
        preflight_cancelled: Notify::new(),
        preflight_release: Semaphore::new(0),
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
    assert_eq!(restored.revalidate_lifetime().await, Ok(()));
    assert_eq!(restored.subscribe().borrow().phase, Phase::Ready);
    assert_eq!(
        api.refresh_calls.load(Ordering::SeqCst),
        1,
        "the saved old deadline cannot expire an extended session"
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
async fn refresh_rate_limit_preserves_the_pending_operation_for_backoff_recovery() {
    let (owner, api, clock, _) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.rate_limit_refresh.store(true, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    api.rate_limit_refresh.store(false, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(owner.access(&CancellationToken::new()).await.is_ok());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 2);
    {
        let keys = api.renewal_keys.lock().unwrap();
        assert_eq!(keys[0], keys[1]);
    }
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
async fn ten_expired_access_requests_share_one_durably_identified_refresh() {
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
async fn lost_refresh_response_recovers_the_same_operation_after_restart() {
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
    restarted.restore().await.unwrap();
    api.lost_reply.store(false, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(restarted.access(&CancellationToken::new()).await.is_ok());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 2);
    {
        let keys = api.renewal_keys.lock().unwrap();
        assert_eq!(
            keys[0], keys[1],
            "restart must recover the original renewal"
        );
    }
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
    let alice = owner.access(&CancellationToken::new()).await.unwrap();
    assert_eq!(alice.account_id().as_str(), "account-alice");
    assert_eq!(alice.session_id().as_str(), "session-alice");
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
    assert!(alice.cancellation.is_cancelled());
    let bob = owner.access(&CancellationToken::new()).await.unwrap();
    assert_eq!(bob.account_id().as_str(), "account-bob");
    assert_eq!(bob.session_id().as_str(), "session-bob");
    let saved = store.load().unwrap().unwrap();
    let current = SecretRecord::decode(&saved, &clock.now().unwrap()).unwrap();
    assert_eq!(current.tokens.session.account_id.value, "account-bob");
    owner.close().await;
}

#[tokio::test]
async fn saved_expiry_suspends_access_but_preserves_server_authoritative_recovery() {
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
    assert_eq!(owner.revalidate_lifetime().await, Ok(()));
    assert!(lease.cancellation.is_cancelled());
    assert!(store.load().unwrap().is_some());
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
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

#[tokio::test]
async fn rotation_accepts_sql_timestamp_precision_but_rejects_another_microsecond() {
    for (rotated_nanos, accepted) in [(159_008_000, true), (159_009_000, false)] {
        let (owner, api, clock, _) = fixture();
        api.issued_nanos.store(159_008_138, Ordering::SeqCst);
        api.rotated_nanos.store(rotated_nanos, Ordering::SeqCst);
        owner
            .login("alice".into(), Zeroizing::new("synthetic password".into()))
            .await
            .unwrap();
        clock.0.store(BASE + 121, Ordering::SeqCst);
        api.release.add_permits(1);
        assert_eq!(
            owner.access(&CancellationToken::new()).await.is_ok(),
            accepted
        );
        assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
        owner.close().await;
    }
}

#[tokio::test]
async fn proven_unstarted_refresh_survives_restart_then_renews_without_login() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.refused_connect.store(true, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(matches!(
        owner.access(&CancellationToken::new()).await,
        Err(SessionFailure::Transport(_))
    ));
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    let bytes = store.load().unwrap().unwrap();
    assert!(SecretRecord::decode(&bytes, &clock.now().unwrap()).is_ok());
    assert_eq!(
        store.clear_calls.load(Ordering::SeqCst),
        1,
        "only initial login cleared storage"
    );
    owner.close().await;
    api.refused_connect.store(false, Ordering::SeqCst);
    api.release.add_permits(1);
    let restarted = SessionCoordinator::with_parts(api.clone(), store, clock);
    restarted.restore().await.unwrap();
    assert!(restarted.access(&CancellationToken::new()).await.is_ok());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 2);
    restarted.close().await;
}

#[tokio::test]
async fn logout_after_unstarted_refresh_cannot_be_revived_by_a_new_read() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.refused_connect.store(true, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    owner.logout_local().await.unwrap();
    assert!(store.load().unwrap().is_none());
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    owner.close().await;
}

#[tokio::test]
async fn delayed_waiter_cannot_join_a_new_attempt_after_missing_offline() {
    use std::{
        future::{Future, poll_fn},
        task::Poll,
    };
    let (owner, api, clock, _) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.refused_connect.store(true, Ordering::SeqCst);
    let a = owner.clone();
    let first = tokio::spawn(async move { a.access(&CancellationToken::new()).await });
    api.started.notified().await;
    let cancel = CancellationToken::new();
    let mut delayed = Box::pin(owner.access(&cancel));
    poll_fn(|cx| match delayed.as_mut().poll(cx) {
        Poll::Pending => Poll::Ready(()),
        Poll::Ready(_) => panic!("waiter must join blocked first refresh"),
    })
    .await;
    api.release.add_permits(1);
    assert!(first.await.unwrap().is_err());
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    api.refused_connect.store(false, Ordering::SeqCst);
    let c = owner.clone();
    let next = tokio::spawn(async move { c.access(&CancellationToken::new()).await });
    api.started.notified().await;
    assert!(matches!(
        tokio::time::timeout(std::time::Duration::from_millis(100), delayed)
            .await
            .unwrap(),
        Err(SessionFailure::RefreshSuperseded)
    ));
    api.release.add_permits(1);
    assert!(next.await.unwrap().is_ok());
    owner.close().await;
}

#[tokio::test]
async fn proxy_backend_outage_preserves_unburned_session_before_refresh_dispatch() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.gateway_offline.store(true, Ordering::SeqCst);
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    assert_eq!(
        api.refresh_calls.load(Ordering::SeqCst),
        0,
        "read-only preflight must not dispatch renewal"
    );
    assert!(SecretRecord::decode(&store.load().unwrap().unwrap(), &clock.now().unwrap()).is_ok());
    api.gateway_offline.store(false, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(owner.access(&CancellationToken::new()).await.is_ok());
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    owner.close().await;
}

#[tokio::test]
async fn closing_during_blocked_preflight_joins_without_operation_lock_deadlock() {
    let (owner, api, clock, _) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.block_preflight.store(true, Ordering::SeqCst);
    let reader = owner.clone();
    let pending = tokio::spawn(async move { reader.access(&CancellationToken::new()).await });
    api.preflight_started.notified().await;
    let closing_owner = owner.clone();
    let closing = tokio::spawn(async move { closing_owner.close().await });
    api.preflight_cancelled.notified().await;
    assert!(pending.await.unwrap().is_err());
    tokio::task::yield_now().await;
    assert!(
        owner.inner.operations.try_read().is_err(),
        "close owns the operation barrier while joining refresh"
    );
    api.preflight_release.add_permits(1);
    tokio::time::timeout(std::time::Duration::from_millis(500), closing)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 0);
    assert_eq!(owner.subscribe().borrow().phase, Phase::Closed);
}

#[tokio::test]
async fn preflight_distinguishes_expired_access_from_revocation_and_unavailable_authority() {
    use auth::AuthorizationTerminationReason as Reason;
    for reason in [
        Reason::CredentialExpired,
        Reason::CredentialRevoked,
        Reason::AuthorityUnavailable,
    ] {
        let (owner, api, clock, store) = fixture();
        owner
            .login("alice".into(), Zeroizing::new("synthetic password".into()))
            .await
            .unwrap();
        clock.0.store(BASE + 121, Ordering::SeqCst);
        *api.preflight_reason.lock().unwrap() = Some(reason);
        api.release.add_permits(1);
        let result = owner.access(&CancellationToken::new()).await;
        match reason {
            Reason::CredentialExpired => {
                assert!(result.is_ok());
                assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
            }
            Reason::CredentialRevoked => {
                assert!(result.is_err());
                assert_eq!(owner.subscribe().borrow().phase, Phase::NeedsLogin);
                assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 0);
                assert!(store.load().unwrap().is_none());
            }
            Reason::AuthorityUnavailable => {
                assert!(result.is_err());
                assert_eq!(
                    owner.subscribe().borrow().phase,
                    Phase::AuthorityUnavailable
                );
                assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 0);
                assert!(
                    SecretRecord::decode(&store.load().unwrap().unwrap(), &clock.now().unwrap())
                        .is_ok()
                );
            }
            Reason::Unspecified | Reason::RightsChanged | Reason::AccountDisabled => {
                unreachable!("not an exercised case")
            }
        }
        owner.close().await;
    }
}

#[tokio::test]
async fn pending_renewal_survives_multiple_outages_and_saved_expiry_without_preflight() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.lost_reply.store(true, Ordering::SeqCst);
    api.release.add_permits(5);
    for _ in 0..5 {
        assert!(owner.access(&CancellationToken::new()).await.is_err());
        assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    }
    owner.close().await;
    // The server may already have extended expiry in the lost response. Its
    // old access is invalid, so a preflight must not discard the pending op.
    clock.0.store(BASE + 4000, Ordering::SeqCst);
    api.gateway_offline.store(true, Ordering::SeqCst);
    api.lost_reply.store(false, Ordering::SeqCst);
    let restarted = SessionCoordinator::with_parts(api.clone(), store.clone(), clock.clone());
    restarted.restore().await.unwrap();
    api.release.add_permits(1);
    restarted.access(&CancellationToken::new()).await.unwrap();
    assert_eq!(restarted.subscribe().borrow().phase, Phase::Ready);
    assert_eq!(store.clear_calls.load(Ordering::SeqCst), 1);
    {
        let keys = api.renewal_keys.lock().unwrap();
        assert_eq!(keys.len(), 6);
        assert!(keys.iter().all(|key| key == &keys[0]));
    }
    restarted.close().await;
}

#[tokio::test]
async fn typed_definitive_renewal_failures_clear_credentials_instead_of_retrying() {
    use auth::{
        AuthErrorCode, AuthenticationFailure, ExpiredResource, PermissionFailure,
        auth_error::Detail,
    };
    for detail in [
        auth::AuthError {
            code: AuthErrorCode::Unauthenticated.into(),
            detail: Some(Detail::Authentication(
                AuthenticationFailure::Revoked.into(),
            )),
            ..Default::default()
        },
        auth::AuthError {
            code: AuthErrorCode::Expired.into(),
            detail: Some(Detail::Expired(ExpiredResource::Session.into())),
            ..Default::default()
        },
        auth::AuthError {
            code: AuthErrorCode::PermissionDenied.into(),
            detail: Some(Detail::Permission(
                PermissionFailure::AccountDisabled.into(),
            )),
            ..Default::default()
        },
    ] {
        let (owner, api, clock, store) = fixture();
        owner
            .login("alice".into(), Zeroizing::new("synthetic password".into()))
            .await
            .unwrap();
        clock.0.store(BASE + 121, Ordering::SeqCst);
        *api.refresh_error.lock().unwrap() = Some(detail);
        api.release.add_permits(1);
        assert!(owner.access(&CancellationToken::new()).await.is_err());
        assert_eq!(owner.subscribe().borrow().phase, Phase::NeedsLogin);
        assert!(store.load().unwrap().is_none());
        assert!(owner.access(&CancellationToken::new()).await.is_err());
        assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
        owner.close().await;
    }
}

#[tokio::test]
async fn bare_access_refusal_asks_the_refresh_authority_instead_of_losing_the_session() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    clock.0.store(BASE + 121, Ordering::SeqCst);
    api.bare_preflight_unauthenticated
        .store(true, Ordering::SeqCst);
    api.release.add_permits(1);
    owner.access(&CancellationToken::new()).await.unwrap();
    assert_eq!(owner.subscribe().borrow().phase, Phase::Ready);
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    assert!(store.load().unwrap().is_some());
    owner.close().await;
}

#[tokio::test]
async fn untyped_refusal_preserves_the_session_and_recovers_the_same_pending_renewal() {
    let (owner, api, clock, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    let access = owner.access(&CancellationToken::new()).await.unwrap();
    let refusal = Failure::from(ConnectError::unauthenticated(
        "synthetic missing auth details",
    ));
    assert_eq!(
        owner
            .observe_failure(access.generation, &refusal)
            .await
            .unwrap(),
        crate::SessionReaction::ViewSuspended
    );
    assert!(access.cancellation.is_cancelled());
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    assert!(store.load().unwrap().is_some());
    // The old access remains locally unexpired: refusal must force renewal.
    api.bare_refresh_unauthenticated
        .store(true, Ordering::SeqCst);
    api.release.add_permits(1);
    assert!(owner.access(&CancellationToken::new()).await.is_err());
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    owner.close().await;
    let restored = SessionCoordinator::with_parts(api.clone(), store.clone(), clock);
    restored.restore().await.unwrap();
    api.bare_refresh_unauthenticated
        .store(false, Ordering::SeqCst);
    api.release.add_permits(1);
    restored.access(&CancellationToken::new()).await.unwrap();
    {
        let keys = api.renewal_keys.lock().unwrap();
        assert_eq!(keys.len(), 2);
        assert_eq!(keys[0], keys[1]);
    }
    assert_eq!(store.clear_calls.load(Ordering::SeqCst), 1);
    restored.close().await;
}

#[tokio::test]
async fn failed_secure_clear_publishes_storage_failure_under_the_new_generation() {
    use auth::{AuthErrorCode, AuthenticationFailure, auth_error::Detail};
    use buffa::MessageName;
    for during_refresh in [true, false] {
        let (owner, api, clock, store) = fixture();
        owner
            .login("alice".into(), Zeroizing::new("synthetic password".into()))
            .await
            .unwrap();
        let lease = owner.access(&CancellationToken::new()).await.unwrap();
        let detail = auth::AuthError {
            code: AuthErrorCode::Unauthenticated.into(),
            detail: Some(Detail::Authentication(
                AuthenticationFailure::Revoked.into(),
            )),
            ..Default::default()
        };
        store.fail_clear.store(true, Ordering::SeqCst);
        if during_refresh {
            clock.0.store(BASE + 121, Ordering::SeqCst);
            *api.refresh_error.lock().unwrap() = Some(detail);
            api.release.add_permits(1);
            let result = tokio::time::timeout(
                std::time::Duration::from_secs(2),
                owner.access(&CancellationToken::new()),
            )
            .await;
            assert!(result.unwrap().is_err());
        } else {
            let error = Failure::from(
                ConnectError::unauthenticated("synthetic terminal refusal").with_detail(
                    connectrpc::ErrorDetail::from_message(auth::AuthError::FULL_NAME, &detail),
                ),
            );
            assert_eq!(
                owner.observe_failure(lease.generation, &error).await,
                Err(SessionFailure::Store(StoreFailure::Unavailable))
            );
        }
        assert!(lease.cancellation.is_cancelled());
        assert_eq!(owner.subscribe().borrow().phase, Phase::StorageUnavailable);
        assert_eq!(owner.subscribe().borrow().generation, owner.generation());
        // Recovery must not remain Busy/Refreshing. Once secure storage works,
        // an explicit fresh login is admitted without restarting the process.
        store.fail_clear.store(false, Ordering::SeqCst);
        owner
            .login("bob".into(), Zeroizing::new("synthetic password".into()))
            .await
            .unwrap();
        assert_eq!(owner.subscribe().borrow().phase, Phase::Ready);
        owner.close().await;
    }
}

#[tokio::test]
async fn an_expired_stream_access_lease_renews_the_session_instead_of_signing_out() {
    let (owner, api, _, store) = fixture();
    owner
        .login("alice".into(), Zeroizing::new("synthetic password".into()))
        .await
        .unwrap();
    let lease = owner.access(&CancellationToken::new()).await.unwrap();
    let expired_access = termination(auth::AuthorizationTerminationReason::CredentialExpired);
    assert_eq!(
        owner
            .observe_failure(lease.generation, &expired_access)
            .await
            .unwrap(),
        crate::SessionReaction::ViewSuspended
    );
    assert!(lease.cancellation.is_cancelled());
    assert_eq!(owner.subscribe().borrow().phase, Phase::Offline);
    assert!(store.load().unwrap().is_some());
    api.release.add_permits(1);
    owner.access(&CancellationToken::new()).await.unwrap();
    assert_eq!(owner.subscribe().borrow().phase, Phase::Ready);
    assert_eq!(api.refresh_calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        store.clear_calls.load(Ordering::SeqCst),
        1,
        "only initial login cleared storage"
    );
    owner.close().await;
}
