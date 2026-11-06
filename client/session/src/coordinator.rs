use std::{
    sync::{
        Arc, Mutex as SyncMutex, Weak,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::{SystemTime, UNIX_EPOCH},
};

use buffa_types::google::protobuf::Timestamp;
use home_client_transport::{AccessToken, AuthTransport, Failure, FailureKind};
use home_wire_contract::{auth_contract, home::api::v2 as auth};
use tokio::sync::{Mutex, RwLock, RwLockReadGuard, watch};
use tokio_util::{sync::CancellationToken, task::TaskTracker};
use zeroize::{Zeroize, Zeroizing};

use crate::{
    AccessLease, Generation, Phase, RecordFailure, RefreshState, SecretRecord, SecureStore,
    ServerRevocation, SessionFailure, SessionReaction, SessionView, api::AuthApi,
};

pub trait Clock: Send + Sync {
    fn now(&self) -> Result<Timestamp, SessionFailure>;
}
struct WallClock;
impl Clock for WallClock {
    fn now(&self) -> Result<Timestamp, SessionFailure> {
        let value = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| SessionFailure::Clock)?;
        Ok(Timestamp {
            seconds: value
                .as_secs()
                .try_into()
                .map_err(|_| SessionFailure::Clock)?,
            nanos: value.subsec_nanos() as i32,
            ..Default::default()
        })
    }
}

struct State {
    record: Option<Arc<SecretRecord>>,
    challenge: Option<auth::FactorChallenge>,
    phase: Phase,
    failure: Option<SessionFailure>,
}
struct Inner {
    api: Arc<dyn AuthApi>,
    store: Arc<dyn SecureStore>,
    clock: Arc<dyn Clock>,
    generation: AtomicU64,
    generation_live: AtomicBool,
    identity_epoch: AtomicU64,
    presentation_epoch: AtomicU64,
    epoch: SyncMutex<CancellationToken>,
    shutdown: CancellationToken,
    state: Mutex<State>,
    persistence: Arc<Mutex<()>>,
    operations: RwLock<()>,
    tasks: TaskTracker,
    view: watch::Sender<SessionView>,
}
impl Inner {
    fn is_current(&self, generation: Generation) -> bool {
        !self.shutdown.is_cancelled()
            && self.generation_live.load(Ordering::SeqCst)
            && Generation(self.generation.load(Ordering::SeqCst)) == generation
    }
}

/// A publication fence without ownership of credentials or the OS store.
/// A closed or dropped coordinator always makes the fence invalid.
#[derive(Clone)]
pub struct GenerationGuard {
    inner: Weak<Inner>,
}
impl GenerationGuard {
    pub fn is_current(&self, generation: Generation) -> bool {
        self.inner
            .upgrade()
            .is_some_and(|inner| inner.is_current(generation))
    }
}

/// Weak provenance of an admitted credential. It contains no credential,
/// secure store or coordinator ownership. Presentation continuity is NOT an
/// authorization grant: only `is_current` may fence admitted work.
#[derive(Clone)]
pub struct SourceProof {
    inner: Weak<Inner>,
    generation: Generation,
    presentation_epoch: u64,
}
impl PartialEq for SourceProof {
    fn eq(&self, other: &Self) -> bool {
        Weak::ptr_eq(&self.inner, &other.inner)
            && self.generation == other.generation
            && self.presentation_epoch == other.presentation_epoch
    }
}
impl Eq for SourceProof {}
impl std::fmt::Debug for SourceProof {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("SourceProof([weak local provenance])")
    }
}
impl SourceProof {
    pub fn is_current(&self) -> bool {
        self.inner.upgrade().is_some_and(|inner| {
            inner.is_current(self.generation)
                && inner.presentation_epoch.load(Ordering::SeqCst) == self.presentation_epoch
        })
    }
    /// Only bounds already-displayed pixels at their original deadline.
    /// Every non-nominal transition permanently breaks this lineage, even
    /// when a watch subscriber misses the intermediate failure publication.
    pub fn presentation_continues(&self) -> bool {
        self.inner.upgrade().is_some_and(|inner| {
            !inner.shutdown.is_cancelled()
                && inner.generation_live.load(Ordering::SeqCst)
                && inner.presentation_epoch.load(Ordering::SeqCst) == self.presentation_epoch
        })
    }
    pub fn renewing(&self) -> bool {
        self.inner.upgrade().is_some_and(|inner| {
            !inner.shutdown.is_cancelled()
                && inner.generation_live.load(Ordering::SeqCst)
                && Generation(inner.generation.load(Ordering::SeqCst)) != self.generation
                && inner.presentation_epoch.load(Ordering::SeqCst) == self.presentation_epoch
        })
    }
    pub fn same_presentation(&self, other: &Self) -> bool {
        Weak::ptr_eq(&self.inner, &other.inner)
            && self.presentation_epoch == other.presentation_epoch
            && self.presentation_continues()
            && other.presentation_continues()
    }
    pub fn generation(&self) -> Generation {
        self.generation
    }
}

#[derive(Clone)]
pub struct SessionCoordinator {
    inner: Arc<Inner>,
}

impl SessionCoordinator {
    pub fn new(api: AuthTransport, store: Arc<dyn SecureStore>) -> Self {
        Self::with_parts(Arc::new(api), store, Arc::new(WallClock))
    }
    pub(crate) fn with_parts(
        api: Arc<dyn AuthApi>,
        store: Arc<dyn SecureStore>,
        clock: Arc<dyn Clock>,
    ) -> Self {
        let (view, _) = watch::channel(SessionView {
            generation: Generation(0),
            phase: Phase::SignedOut,
            failure: None,
        });
        Self {
            inner: Arc::new(Inner {
                api,
                store,
                clock,
                generation: AtomicU64::new(0),
                generation_live: AtomicBool::new(true),
                identity_epoch: AtomicU64::new(0),
                presentation_epoch: AtomicU64::new(0),
                epoch: SyncMutex::new(CancellationToken::new()),
                shutdown: CancellationToken::new(),
                state: Mutex::new(State {
                    record: None,
                    challenge: None,
                    phase: Phase::SignedOut,
                    failure: None,
                }),
                persistence: Arc::new(Mutex::new(())),
                operations: RwLock::new(()),
                tasks: TaskTracker::new(),
                view,
            }),
        }
    }
    pub fn subscribe(&self) -> watch::Receiver<SessionView> {
        self.inner.view.subscribe()
    }

    async fn operation(&self) -> Result<RwLockReadGuard<'_, ()>, SessionFailure> {
        let guard = self.inner.operations.read().await;
        if self.inner.shutdown.is_cancelled() {
            return Err(SessionFailure::Closed);
        }
        Ok(guard)
    }
    pub fn generation(&self) -> Generation {
        Generation(self.inner.generation.load(Ordering::SeqCst))
    }
    pub fn is_current(&self, generation: Generation) -> bool {
        self.inner.is_current(generation)
    }
    pub fn generation_guard(&self) -> GenerationGuard {
        GenerationGuard {
            inner: Arc::downgrade(&self.inner),
        }
    }
    fn token_for(&self, generation: Generation) -> Result<CancellationToken, SessionFailure> {
        let epoch = self.inner.epoch.lock().expect("epoch lock");
        if !self.is_current(generation) {
            return Err(SessionFailure::Cancelled);
        }
        Ok(epoch.child_token())
    }
    fn break_presentation(&self) {
        self.inner.presentation_epoch.fetch_add(1, Ordering::SeqCst);
    }
    fn advance(&self) -> Generation {
        self.break_presentation();
        self.advance_renewal()
    }
    fn advance_renewal(&self) -> Generation {
        let generation = Generation(self.inner.generation.fetch_add(1, Ordering::SeqCst) + 1);
        let mut epoch = self.inner.epoch.lock().expect("epoch lock");
        epoch.cancel();
        *epoch = self.inner.shutdown.child_token();
        self.inner.generation_live.store(true, Ordering::SeqCst);
        generation
    }
    fn publish(&self, phase: Phase) {
        self.publish_failure(phase, None);
    }
    fn publish_failure(&self, phase: Phase, failure: Option<SessionFailure>) {
        if !matches!(phase, Phase::Ready | Phase::Refreshing) || failure.is_some() {
            self.break_presentation();
        }
        self.inner.view.send_replace(SessionView {
            generation: self.generation(),
            phase,
            failure,
        });
    }

    async fn begin(&self) -> Result<Generation, SessionFailure> {
        let mut state = self.inner.state.lock().await;
        if self.inner.shutdown.is_cancelled() {
            return Err(SessionFailure::Closed);
        }
        if matches!(state.phase, Phase::Busy | Phase::Refreshing) {
            return Err(SessionFailure::Busy);
        }
        let generation = self.advance();
        self.inner.identity_epoch.fetch_add(1, Ordering::SeqCst);
        state.record = None;
        state.challenge = None;
        state.phase = Phase::Busy;
        state.failure = None;
        self.publish(Phase::Busy);
        Ok(generation)
    }

    // The blocking task OWNS the serialization guard. Dropping its awaiting
    // future cannot release the guard while an older OS write is still running.
    async fn persist(
        &self,
        generation: Generation,
        bytes: Option<Zeroizing<Vec<u8>>>,
    ) -> Result<(), SessionFailure> {
        let guard = self.inner.persistence.clone().lock_owned().await;
        let inner = self.inner.clone();
        self.inner
            .tasks
            .spawn_blocking(move || {
                let _guard = guard;
                if Generation(inner.generation.load(Ordering::SeqCst)) != generation
                    || (bytes.is_some() && inner.shutdown.is_cancelled())
                {
                    return Err(SessionFailure::Cancelled);
                }
                match bytes {
                    Some(bytes) => inner.store.save(&bytes),
                    None => inner.store.clear(),
                }
                .map_err(SessionFailure::Store)
            })
            .await
            .map_err(|_| SessionFailure::Store(crate::StoreFailure::Unavailable))?
    }

    async fn fail(&self, generation: Generation, failure: SessionFailure) -> SessionFailure {
        let mut state = self.inner.state.lock().await;
        if self.is_current(generation) {
            // Retire this generation without changing the failure delivered to
            // waiters of the same refresh attempt. Long-lived generation guards
            // reject even a queued Ready publication of this numeric generation.
            self.inner.generation_live.store(false, Ordering::SeqCst);
            self.inner.epoch.lock().expect("epoch lock").cancel();
            state.record = None;
            state.challenge = None;
            state.failure = Some(failure);
            state.phase = match failure {
                SessionFailure::Store(_) => Phase::StorageUnavailable,
                SessionFailure::Transport(FailureKind::Rpc(
                    home_client_transport::RpcCode::ResourceExhausted,
                )) => Phase::RateLimited,
                SessionFailure::Transport(FailureKind::Rpc(
                    home_client_transport::RpcCode::Unauthenticated,
                )) => Phase::CredentialRejected,
                SessionFailure::Transport(_) => Phase::NetworkUnavailable,
                SessionFailure::Cancelled
                | SessionFailure::RefreshSuperseded
                | SessionFailure::Closed => Phase::SignedOut,
                SessionFailure::NeedsLogin => Phase::NeedsLogin,
                SessionFailure::RefreshUncertain(FailureKind::Rpc(
                    home_client_transport::RpcCode::ResourceExhausted,
                )) => Phase::RateLimited,
                SessionFailure::RefreshUncertain(_) => Phase::NeedsLogin,
                SessionFailure::AuthorityUnavailable => Phase::AuthorityUnavailable,
                SessionFailure::Busy => Phase::Busy,
                SessionFailure::UnsupportedFactor
                | SessionFailure::InvalidResponse
                | SessionFailure::Clock => Phase::UnsupportedResponse,
            };
            self.publish_failure(state.phase, Some(failure));
        }
        failure
    }

    async fn install(
        &self,
        generation: Generation,
        record: SecretRecord,
    ) -> Result<(), SessionFailure> {
        AccessToken::parse(&record.tokens.access_token)
            .map_err(|_| SessionFailure::InvalidResponse)?;
        self.persist(generation, Some(record.encode())).await?;
        let mut state = self.inner.state.lock().await;
        if !self.is_current(generation) {
            return Err(SessionFailure::Cancelled);
        }
        state.record = Some(Arc::new(record));
        state.challenge = None;
        state.failure = None;
        state.phase = Phase::Ready;
        self.publish(Phase::Ready);
        Ok(())
    }

    async fn decision(
        &self,
        generation: Generation,
        mut decision: auth::LoginDecision,
    ) -> Result<(), SessionFailure> {
        let now = self.inner.clock.now()?;
        auth_contract::validate_received_login(&decision, &now)
            .map_err(|_| SessionFailure::InvalidResponse)?;
        match decision.outcome.take() {
            Some(auth::login_decision::Outcome::Authenticated(tokens)) => {
                let record =
                    SecretRecord::new(*tokens, now).map_err(|_| SessionFailure::InvalidResponse)?;
                self.install(generation, record).await
            }
            Some(auth::login_decision::Outcome::Challenge(challenge)) => {
                let recovery = challenge.options.iter().any(|option| {
                    matches!(
                        option.method,
                        Some(auth::factor_option::Method::RecoveryCode(_))
                    )
                });
                let fido2 = challenge.options.iter().any(|option| {
                    matches!(option.method, Some(auth::factor_option::Method::Fido2(_)))
                });
                let mut state = self.inner.state.lock().await;
                if !self.is_current(generation) {
                    return Err(SessionFailure::Cancelled);
                }
                state.challenge = Some(*challenge);
                state.phase = Phase::Challenge { recovery, fido2 };
                self.publish(state.phase);
                Ok(())
            }
            Some(auth::login_decision::Outcome::EnrollmentRequired(mut enrollment)) => {
                // There is no qualified FIDO2 enrollment provider yet. Retain
                // no grant and never turn this outcome into a data session.
                enrollment.enrollment_token.zeroize();
                let mut state = self.inner.state.lock().await;
                if !self.is_current(generation) {
                    return Err(SessionFailure::Cancelled);
                }
                state.phase = Phase::EnrollmentRequired;
                self.publish(state.phase);
                Ok(())
            }
            None => Err(SessionFailure::InvalidResponse),
        }
    }

    pub async fn login(
        &self,
        login_name: String,
        password: Zeroizing<String>,
    ) -> Result<(), SessionFailure> {
        let _operation = self.operation().await?;
        if login_name.is_empty()
            || login_name.len() > 256
            || password.is_empty()
            || password.len() > 4096
        {
            return Err(SessionFailure::InvalidResponse);
        }
        let generation = self.begin().await?;
        let result = async {
            self.persist(generation, None).await?;
            let response = self
                .inner
                .api
                .start_login(
                    auth::StartLoginRequest {
                        login_name,
                        password: password.to_string(),
                        ..Default::default()
                    },
                    &self.token_for(generation)?,
                )
                .await
                .map_err(|e| SessionFailure::Transport(e.kind()))?;
            self.decision(
                generation,
                response
                    .decision
                    .into_option()
                    .ok_or(SessionFailure::InvalidResponse)?,
            )
            .await
        }
        .await;
        if let Err(error) = result {
            return Err(self.fail(generation, error).await);
        }
        Ok(())
    }

    pub async fn complete_recovery(&self, code: Zeroizing<String>) -> Result<(), SessionFailure> {
        let _operation = self.operation().await?;
        let (generation, challenge) = {
            let mut state = self.inner.state.lock().await;
            if !matches!(state.phase, Phase::Challenge { recovery: true, .. }) {
                return Err(SessionFailure::UnsupportedFactor);
            }
            state.phase = Phase::Busy;
            self.publish(Phase::Busy);
            (
                self.generation(),
                state.challenge.take().ok_or(SessionFailure::NeedsLogin)?,
            )
        };
        let result = async {
            let response = self
                .inner
                .api
                .complete_login(
                    auth::CompleteLoginRequest {
                        challenge_id: challenge.challenge_id,
                        proof: auth::FactorProof {
                            proof: Some(auth::factor_proof::Proof::RecoveryCode(code.to_string())),
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    },
                    &self.token_for(generation)?,
                )
                .await
                .map_err(|e| SessionFailure::Transport(e.kind()))?;
            self.decision(
                generation,
                response
                    .decision
                    .into_option()
                    .ok_or(SessionFailure::InvalidResponse)?,
            )
            .await
        }
        .await;
        if let Err(error) = result {
            return Err(self.fail(generation, error).await);
        }
        Ok(())
    }

    pub async fn restore(&self) -> Result<(), SessionFailure> {
        let _operation = self.operation().await?;
        let generation = self.begin().await?;
        if let Err(error) = self.restore_record(generation).await {
            return Err(self.fail(generation, error).await);
        }
        Ok(())
    }

    async fn restore_record(&self, generation: Generation) -> Result<(), SessionFailure> {
        let guard = self.inner.persistence.clone().lock_owned().await;
        let store = self.inner.store.clone();
        let bytes = self
            .inner
            .tasks
            .spawn_blocking(move || {
                let _guard = guard;
                store.load()
            })
            .await
            .map_err(|_| SessionFailure::Store(crate::StoreFailure::Unavailable))?
            .map_err(SessionFailure::Store)?;
        let Some(bytes) = bytes else {
            let mut state = self.inner.state.lock().await;
            if !self.is_current(generation) {
                return Err(SessionFailure::Cancelled);
            }
            state.phase = Phase::SignedOut;
            state.failure = None;
            self.publish(Phase::SignedOut);
            return Ok(());
        };
        let record = match SecretRecord::decode(&bytes, &self.inner.clock.now()?) {
            Ok(record) => record,
            Err(
                RecordFailure::Invalid
                | RecordFailure::RefreshConsumed
                | RecordFailure::ClockMovedBackwards,
            ) => {
                self.persist(generation, None).await?;
                return Err(SessionFailure::NeedsLogin);
            }
        };
        let mut state = self.inner.state.lock().await;
        if !self.is_current(generation) {
            return Err(SessionFailure::Cancelled);
        }
        state.record = Some(Arc::new(record));
        state.failure = None;
        state.phase = Phase::Ready;
        self.publish(Phase::Ready);
        Ok(())
    }

    /// Invalidate protected access at the saved deadline, retaining the secure
    /// renewal operation. Only the server knows the current sliding deadline.
    pub async fn revalidate_lifetime(&self) -> Result<(), SessionFailure> {
        let _operation = self.operation().await?;
        {
            let mut state = self.inner.state.lock().await;
            if self.inner.shutdown.is_cancelled() {
                return Err(SessionFailure::Closed);
            }
            let Some(record) = state.record.as_ref() else {
                return Ok(());
            };
            let now = match self.inner.clock.now() {
                Ok(now) => now,
                Err(error) => {
                    let generation = self.generation();
                    drop(state);
                    return Err(self.fail(generation, error).await);
                }
            };
            if !record.session_expired(&now) || state.phase != Phase::Ready {
                return Ok(());
            }
            self.advance();
            state.phase = Phase::Offline;
            state.failure = Some(SessionFailure::Transport(FailureKind::Deadline));
            self.publish_failure(Phase::Offline, state.failure);
        }
        Ok(())
    }

    pub async fn access(&self, cancel: &CancellationToken) -> Result<AccessLease, SessionFailure> {
        let _operation = self.operation().await?;
        // A refresh may replace an access generation; logout/account switch
        // must instead cancel every waiter from the previous session identity.
        let identity_epoch = self.inner.identity_epoch.load(Ordering::SeqCst);
        let mut changes = self.subscribe();
        let mut first_attempt = true;
        let mut refresh_generation = None;
        loop {
            let mut state = self.inner.state.lock().await;
            if self.inner.shutdown.is_cancelled() {
                return Err(SessionFailure::Closed);
            }
            if cancel.is_cancelled() {
                return Err(SessionFailure::Cancelled);
            }
            if self.inner.identity_epoch.load(Ordering::SeqCst) != identity_epoch {
                return Err(SessionFailure::Cancelled);
            }
            if refresh_generation.is_some_and(|attempt| attempt != self.generation()) {
                // A later renewal superseded the attempt this waiter joined.
                // It must not silently join that new attempt after missing Offline.
                return Err(SessionFailure::RefreshSuperseded);
            }
            // A fresh read intention can retry a saved, unconsumed renewal.
            // Waiters on the previous shared attempt all receive its outcome.
            if first_attempt && state.phase == Phase::Offline && state.record.is_some() {
                state.phase = Phase::Ready;
                state.failure = None;
                self.publish(Phase::Ready);
            }
            first_attempt = false;
            if state.phase == Phase::Ready {
                let record = state.record.clone().ok_or(SessionFailure::NeedsLogin)?;
                let now = self.inner.clock.now()?;
                if !record.access_expired(&now) {
                    let generation = self.generation();
                    return Ok(AccessLease {
                        generation,
                        source: SourceProof {
                            inner: Arc::downgrade(&self.inner),
                            generation,
                            presentation_epoch: self
                                .inner
                                .presentation_epoch
                                .load(Ordering::SeqCst),
                        },
                        cancellation: self.token_for(generation)?,
                        token: AccessToken::parse(&record.tokens.access_token)
                            .map_err(|_| SessionFailure::InvalidResponse)?,
                        account: auth_contract::SessionName::try_from(
                            record.tokens.session.name.as_str(),
                        )
                        .map_err(|_| SessionFailure::InvalidResponse)?
                        .account(),
                        session: auth_contract::SessionName::try_from(
                            record.tokens.session.name.as_str(),
                        )
                        .map_err(|_| SessionFailure::InvalidResponse)?,
                    });
                }
                // Only an ordinary deadline renewal preserves display lineage.
                // Recovery after refusal, ambiguous dispatch or lifetime expiry
                // is a new admission and must discard old private presentation.
                let generation = if record.refresh == RefreshState::Available
                    && !record.session_expired(&now)
                    && state.failure.is_none()
                {
                    self.advance_renewal()
                } else {
                    self.advance()
                };
                state.phase = Phase::Refreshing;
                self.publish(Phase::Refreshing);
                let owner = self.clone();
                self.inner.tasks.spawn(async move {
                    owner.refresh(generation, record).await;
                });
            } else if state.phase != Phase::Refreshing {
                return Err(state.failure.unwrap_or(SessionFailure::NeedsLogin));
            }
            refresh_generation = Some(self.generation());
            drop(state);
            tokio::select! {
                biased;
                () = cancel.cancelled() => return Err(SessionFailure::Cancelled),
                () = self.inner.shutdown.cancelled() => return Err(SessionFailure::Closed),
                changed = changes.changed() => if changed.is_err() { return Err(SessionFailure::Closed); },
            }
        }
    }

    async fn refresh(&self, generation: Generation, previous: Arc<SecretRecord>) {
        let result = async {
            // A read-only reachability check avoids burning a refresh while a
            // reverse proxy is up but its backend is offline. Its success is
            // never used to reclassify a later ambiguous refresh failure.
            let access = AccessToken::parse(&previous.tokens.access_token)
                .map_err(|_| SessionFailure::InvalidResponse)?;
            if previous.refresh == RefreshState::Available
                && let Err(error) = self
                    .inner
                    .api
                    .preflight_refresh(
                        auth::ListSessionsRequest {
                            parent: auth_contract::SessionName::try_from(
                                previous.tokens.session.name.as_str(),
                            )
                            .map_err(|_| SessionFailure::InvalidResponse)?
                            .account()
                            .as_str()
                            .to_owned(),
                            page_size: 1,
                            ..Default::default()
                        },
                        &access,
                        &self.token_for(generation)?,
                    )
                    .await
            {
                // Access rejection cannot determine the independent refresh
                // family's validity. Only RefreshSession can decide that.
                let expired_access = match error.api_failure() {
                    Ok(Some(detail)) => {
                        detail.reason() == auth::ApiErrorReason::AuthenticationRequired
                    }
                    Ok(None) => {
                        error.kind()
                            == FailureKind::Rpc(home_client_transport::RpcCode::Unauthenticated)
                    }
                    Err(_) => false,
                };
                if !expired_access {
                    let reaction = self.observe_failure_inner(generation, &error).await?;
                    if reaction != SessionReaction::Unchanged {
                        return Ok(());
                    }
                    self.defer_refresh(generation, previous, error.kind())
                        .await?;
                    return Ok(());
                }
            }
            let renewal_key = match previous.refresh {
                RefreshState::Available | RefreshState::AccessRejected => {
                    let mut key = [0; 32];
                    getrandom::fill(&mut key).map_err(|_| SessionFailure::InvalidResponse)?;
                    if key == [0; 32] {
                        return Err(SessionFailure::InvalidResponse);
                    }
                    key
                }
                RefreshState::Pending(key) => key,
                RefreshState::Consumed => return Err(SessionFailure::NeedsLogin),
            };
            let pending = Arc::new(SecretRecord {
                tokens: previous.tokens.clone(),
                admitted_at: previous.admitted_at.clone(),
                refresh: RefreshState::Pending(renewal_key),
            });
            self.persist(generation, Some(pending.encode())).await?;
            {
                let mut state = self.inner.state.lock().await;
                if !self.is_current(generation) {
                    return Err(SessionFailure::Cancelled);
                }
                state.record = Some(pending.clone());
            }
            let response = match self
                .inner
                .api
                .refresh(
                    auth::RefreshSessionRequest {
                        refresh_token: previous.tokens.refresh_token.clone(),
                        renewal_key: renewal_key.to_vec(),
                        ..Default::default()
                    },
                    &self.token_for(generation)?,
                )
                .await
            {
                Ok(response) => response,
                Err(error) => {
                    match renewal_failure(&error)? {
                        RenewalFailure::Reauthenticate => {
                            self.reject_session(generation).await?;
                            return Ok(());
                        }
                        RenewalFailure::Retry => {
                            self.defer_refresh(generation, pending, error.kind())
                                .await?;
                            return Ok(());
                        }
                        RenewalFailure::Stop => {}
                    }
                    if self.observe_failure_inner(generation, &error).await?
                        != SessionReaction::Unchanged
                    {
                        return Ok(());
                    }
                    // A trust/protocol/permission refusal is not proof that
                    // the saved session expired. Keep its exact pending op;
                    // the read owner decides whether automatic retry is safe.
                    self.defer_refresh(generation, pending, error.kind())
                        .await?;
                    return Ok(());
                }
            };
            let tokens = response
                .tokens
                .into_option()
                .ok_or(SessionFailure::InvalidResponse)?;
            if tokens.session.name != previous.tokens.session.name
                || !same_persisted_instant(
                    tokens.session.create_time.as_option(),
                    previous.tokens.session.create_time.as_option(),
                )
                || !same_persisted_instant(
                    tokens.session.authentication_time.as_option(),
                    previous.tokens.session.authentication_time.as_option(),
                )
                || tokens.session.authentication_satisfaction
                    != previous.tokens.session.authentication_satisfaction
            {
                return Err(SessionFailure::InvalidResponse);
            }
            let record = SecretRecord::new(tokens, self.inner.clock.now()?)
                .map_err(|_| SessionFailure::InvalidResponse)?;
            self.install(generation, record).await
        }
        .await;
        if let Err(error) = result {
            self.fail(generation, error).await;
        }
    }

    async fn reject_session(&self, generation: Generation) -> Result<(), SessionFailure> {
        let mut state = self.inner.state.lock().await;
        if !self.is_current(generation) {
            return Err(SessionFailure::Cancelled);
        }
        let next = self.advance();
        self.inner.identity_epoch.fetch_add(1, Ordering::SeqCst);
        state.record = None;
        state.challenge = None;
        state.phase = Phase::Refreshing;
        state.failure = Some(SessionFailure::NeedsLogin);
        drop(state);
        if let Err(error) = self.persist(next, None).await {
            return Err(self.fail(next, error).await);
        }
        let mut state = self.inner.state.lock().await;
        if !self.is_current(next) {
            return Err(SessionFailure::Cancelled);
        }
        state.phase = Phase::NeedsLogin;
        self.publish_failure(Phase::NeedsLogin, state.failure);
        Ok(())
    }

    async fn defer_refresh(
        &self,
        generation: Generation,
        previous: Arc<SecretRecord>,
        failure: FailureKind,
    ) -> Result<(), SessionFailure> {
        self.persist(generation, Some(previous.encode())).await?;
        let mut state = self.inner.state.lock().await;
        if !self.is_current(generation) {
            return Err(SessionFailure::Cancelled);
        }
        state.record = Some(previous);
        state.challenge = None;
        state.phase = Phase::Offline;
        state.failure = Some(SessionFailure::Transport(failure));
        self.publish_failure(Phase::Offline, state.failure);
        Ok(())
    }

    pub async fn logout_local(&self) -> Result<(), SessionFailure> {
        self.logout_local_for(None).await.map(|_| ())
    }

    /// A completed mutation may retire its own credential. It must not clear a
    /// replacement login that started while its response was being delivered.
    pub async fn logout_local_if_current(
        &self,
        expected: Generation,
    ) -> Result<bool, SessionFailure> {
        self.logout_local_for(Some(expected)).await
    }

    async fn logout_local_for(&self, expected: Option<Generation>) -> Result<bool, SessionFailure> {
        let _operation = self.operation().await?;
        let generation = {
            let mut state = self.inner.state.lock().await;
            if expected.is_some_and(|expected| !self.is_current(expected)) {
                return Ok(false);
            }
            let generation = self.advance();
            self.inner.identity_epoch.fetch_add(1, Ordering::SeqCst);
            state.record = None;
            state.challenge = None;
            state.phase = Phase::SignedOut;
            state.failure = None;
            self.publish(Phase::SignedOut);
            generation
        };
        if let Err(error) = self.persist(generation, None).await {
            return Err(self.fail(generation, error).await);
        }
        Ok(true)
    }

    /// Local deletion is mandatory; server revocation is a single attempt and
    /// explicitly reports uncertainty. It never refreshes just to log out.
    pub async fn logout(&self) -> Result<ServerRevocation, SessionFailure> {
        let _operation = self.operation().await?;
        let (generation, previous) = {
            let mut state = self.inner.state.lock().await;
            let generation = self.advance();
            self.inner.identity_epoch.fetch_add(1, Ordering::SeqCst);
            let previous = state.record.take();
            state.challenge = None;
            state.failure = None;
            state.phase = Phase::SignedOut;
            self.publish(Phase::SignedOut);
            (generation, previous)
        };
        let local = self.persist(generation, None).await;
        let remote = if let Some(previous) = previous {
            match AccessToken::parse(&previous.tokens.access_token) {
                Ok(token) => match self
                    .inner
                    .api
                    .revoke(
                        auth::RevokeSessionRequest {
                            name: previous.tokens.session.name.clone(),
                            ..Default::default()
                        },
                        &token,
                        &self.inner.shutdown.child_token(),
                    )
                    .await
                {
                    Ok(()) => ServerRevocation::Acknowledged,
                    Err(_) => ServerRevocation::Uncertain,
                },
                Err(_) => ServerRevocation::Uncertain,
            }
        } else {
            ServerRevocation::NotRequired
        };
        if let Err(error) = local {
            return Err(self.fail(generation, error).await);
        }
        Ok(remote)
    }

    /// Process only a failure from the current access generation. A revoked old
    /// access after refresh cannot sign out its newly rotated replacement.
    pub async fn observe_failure(
        &self,
        generation: Generation,
        failure: &Failure,
    ) -> Result<SessionReaction, SessionFailure> {
        let _operation = self.operation().await?;
        self.observe_failure_inner(generation, failure).await
    }

    // Tracked refresh tasks already belong to this owner. They must not acquire
    // operations.read while close holds operations.write and joins those tasks.
    async fn observe_failure_inner(
        &self,
        generation: Generation,
        failure: &Failure,
    ) -> Result<SessionReaction, SessionFailure> {
        use auth::ApiErrorReason as Reason;
        // Only the renewal boundary can prove the refresh family invalid.
        let detail = match failure.api_failure() {
            Ok(detail) => detail,
            Err(_) => {
                return Err(self.fail(generation, SessionFailure::InvalidResponse).await);
            }
        };
        let reason = detail.as_ref().map(|detail| detail.reason());
        if reason == Some(Reason::AuthenticationRequired)
            || (reason.is_none()
                && failure.kind()
                    == FailureKind::Rpc(home_client_transport::RpcCode::Unauthenticated))
        {
            let mut state = self.inner.state.lock().await;
            if !self.is_current(generation) {
                return Ok(SessionReaction::IgnoredOldGeneration);
            }
            let previous = state.record.as_ref().ok_or(SessionFailure::NeedsLogin)?;
            let retained = SecretRecord {
                tokens: previous.tokens.clone(),
                admitted_at: previous.admitted_at.clone(),
                refresh: match previous.refresh {
                    RefreshState::Available | RefreshState::AccessRejected => {
                        RefreshState::AccessRejected
                    }
                    RefreshState::Pending(key) => RefreshState::Pending(key),
                    RefreshState::Consumed => return Err(SessionFailure::NeedsLogin),
                },
            };
            let next = self.advance();
            let bytes = retained.encode();
            state.record = Some(Arc::new(retained));
            state.phase = Phase::Offline;
            state.failure = Some(SessionFailure::Transport(failure.kind()));
            self.publish_failure(Phase::Offline, state.failure);
            drop(state);
            self.persist(next, Some(bytes)).await?;
            return Ok(SessionReaction::ViewSuspended);
        }
        if !matches!(
            reason,
            Some(Reason::AuthorityUnavailable | Reason::AccessDenied)
        ) {
            return Ok(SessionReaction::Unchanged);
        }
        let mut state = self.inner.state.lock().await;
        if !self.is_current(generation) {
            return Ok(SessionReaction::IgnoredOldGeneration);
        }
        self.advance();
        let reaction = match reason {
            Some(Reason::AuthorityUnavailable) => {
                state.phase = Phase::AuthorityUnavailable;
                state.failure = Some(SessionFailure::AuthorityUnavailable);
                SessionReaction::ViewSuspended
            }
            Some(Reason::AccessDenied) => {
                state.phase = Phase::RightsChanged;
                state.failure = Some(SessionFailure::NeedsLogin);
                SessionReaction::ViewReset
            }
            _ => unreachable!("only admitted suspension reasons reach this branch"),
        };
        self.publish_failure(state.phase, state.failure);
        drop(state);
        Ok(reaction)
    }

    /// Permit a fresh OpenView attempt after suspension/rights change. This
    /// never restores a cached view; that requires the cache's new snapshot.
    pub async fn allow_new_view_attempt(&self) -> Result<(), SessionFailure> {
        let _operation = self.operation().await?;
        let mut state = self.inner.state.lock().await;
        if !matches!(
            state.phase,
            Phase::AuthorityUnavailable | Phase::RightsChanged
        ) || state.record.is_none()
        {
            return Err(SessionFailure::NeedsLogin);
        }
        state.phase = Phase::Ready;
        state.failure = None;
        self.publish(Phase::Ready);
        Ok(())
    }

    pub async fn close(&self) {
        self.inner.shutdown.cancel();
        self.inner.epoch.lock().expect("epoch lock").cancel();
        // A public call may still be queued on persistence and has not yet
        // spawned its tracked OS task. Drain those calls before trusting an
        // empty TaskTracker; later calls observe Closed under the same barrier.
        let _operations = self.inner.operations.write().await;
        self.inner.tasks.close();
        self.inner.tasks.wait().await;
        let mut state = self.inner.state.lock().await;
        state.record = None;
        state.challenge = None;
        state.phase = Phase::Closed;
        self.publish(Phase::Closed);
    }
}

#[cfg(test)]
#[path = "tests.rs"]
mod tests;

// PostgreSQL timestamps retain microseconds. Older servers returned the original
// nanoseconds at login, then the stored precision at rotation. Only these two
// immutable timestamps use that resolution; all other identity checks stay exact.
fn same_persisted_instant(a: Option<&Timestamp>, b: Option<&Timestamp>) -> bool {
    match (a, b) {
        (Some(a), Some(b)) => {
            (0..1_000_000_000).contains(&a.nanos)
                && (0..1_000_000_000).contains(&b.nanos)
                && a.seconds == b.seconds
                && a.nanos / 1000 == b.nanos / 1000
        }
        (Some(_), None) | (None, Some(_)) | (None, None) => false,
    }
}

// Map the protobuf error exactly once at the renewal boundary. A permission
// error about another resource must not silently erase a valid session.
enum RenewalFailure {
    Retry,
    Reauthenticate,
    Stop,
}
fn renewal_failure(failure: &Failure) -> Result<RenewalFailure, SessionFailure> {
    let canonical = failure
        .api_failure()
        .map_err(|_| SessionFailure::InvalidResponse)?;
    if canonical.is_some_and(|detail| {
        detail.reason() == auth::ApiErrorReason::AuthenticationRequired
            || detail.reason() == auth::ApiErrorReason::AccessDenied
    }) {
        return Ok(RenewalFailure::Reauthenticate);
    }
    Ok(if failure.may_recover_renewal() {
        RenewalFailure::Retry
    } else {
        RenewalFailure::Stop
    })
}
