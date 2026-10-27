use std::{
    sync::{
        Arc, Mutex as SyncMutex,
        atomic::{AtomicU64, Ordering},
    },
    time::{SystemTime, UNIX_EPOCH},
};

use buffa_types::google::protobuf::Timestamp;
use home_client_transport::{AccessToken, AuthTransport, Failure, FailureKind};
use home_wire_contract::{auth_contract, home::auth::v1 as auth};
use tokio::sync::{Mutex, RwLock, RwLockReadGuard, watch};
use tokio_util::{sync::CancellationToken, task::TaskTracker};
use zeroize::{Zeroize, Zeroizing};

use crate::{
    AccessLease, Generation, Phase, RecordFailure, RefreshState, SecretRecord, SecureStore,
    ServerRevocation, SessionFailure, SessionReaction, SessionView, api::AuthApi,
};

pub(crate) trait Clock: Send + Sync {
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
    identity_epoch: AtomicU64,
    epoch: SyncMutex<CancellationToken>,
    shutdown: CancellationToken,
    state: Mutex<State>,
    persistence: Arc<Mutex<()>>,
    operations: RwLock<()>,
    tasks: TaskTracker,
    view: watch::Sender<SessionView>,
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
                identity_epoch: AtomicU64::new(0),
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
        !self.inner.shutdown.is_cancelled() && self.generation() == generation
    }
    fn token_for(&self, generation: Generation) -> Result<CancellationToken, SessionFailure> {
        let epoch = self.inner.epoch.lock().expect("epoch lock");
        if !self.is_current(generation) {
            return Err(SessionFailure::Cancelled);
        }
        Ok(epoch.child_token())
    }
    fn advance(&self) -> Generation {
        let generation = Generation(self.inner.generation.fetch_add(1, Ordering::SeqCst) + 1);
        let mut epoch = self.inner.epoch.lock().expect("epoch lock");
        epoch.cancel();
        *epoch = self.inner.shutdown.child_token();
        generation
    }
    fn publish(&self, phase: Phase) {
        self.publish_failure(phase, None);
    }
    fn publish_failure(&self, phase: Phase, failure: Option<SessionFailure>) {
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
                SessionFailure::Cancelled | SessionFailure::Closed => Phase::SignedOut,
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
                        challenge_id: challenge.id,
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
                | RecordFailure::Expired
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

    /// Recheck absolute lifetime without a network call or an access refresh.
    /// Hosts call this while idle and on returning to the foreground so a
    /// previously displayed protected result cannot outlive its session.
    pub async fn revalidate_lifetime(&self) -> Result<(), SessionFailure> {
        let _operation = self.operation().await?;
        let generation = {
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
            if !record.session_expired(&now) {
                return Ok(());
            }
            let generation = self.advance();
            self.inner.identity_epoch.fetch_add(1, Ordering::SeqCst);
            state.record = None;
            state.challenge = None;
            state.phase = Phase::NeedsLogin;
            state.failure = Some(SessionFailure::NeedsLogin);
            self.publish_failure(Phase::NeedsLogin, Some(SessionFailure::NeedsLogin));
            generation
        };
        if let Err(error) = self.persist(generation, None).await {
            return Err(self.fail(generation, error).await);
        }
        Err(SessionFailure::NeedsLogin)
    }

    pub async fn access(&self, cancel: &CancellationToken) -> Result<AccessLease, SessionFailure> {
        let _operation = self.operation().await?;
        // A refresh may replace an access generation; logout/account switch
        // must instead cancel every waiter from the previous session identity.
        let identity_epoch = self.inner.identity_epoch.load(Ordering::SeqCst);
        let mut changes = self.subscribe();
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
            if state.phase == Phase::Ready {
                let record = state.record.clone().ok_or(SessionFailure::NeedsLogin)?;
                let now = self.inner.clock.now()?;
                if record.session_expired(&now) {
                    let generation = self.advance();
                    self.inner.identity_epoch.fetch_add(1, Ordering::SeqCst);
                    state.record = None;
                    state.phase = Phase::NeedsLogin;
                    self.publish(Phase::NeedsLogin);
                    drop(state);
                    if let Err(error) = self.persist(generation, None).await {
                        return Err(self.fail(generation, error).await);
                    }
                    return Err(SessionFailure::NeedsLogin);
                }
                if !record.access_expired(&now) {
                    let generation = self.generation();
                    return Ok(AccessLease {
                        generation,
                        cancellation: self.token_for(generation)?,
                        token: AccessToken::parse(&record.tokens.access_token)
                            .map_err(|_| SessionFailure::InvalidResponse)?,
                        account: auth_contract::AccountId::try_from(
                            &*record.tokens.session.account_id,
                        )
                        .map_err(|_| SessionFailure::InvalidResponse)?,
                        session: auth_contract::SessionId::try_from(&*record.tokens.session.id)
                            .map_err(|_| SessionFailure::InvalidResponse)?,
                    });
                }
                let generation = self.advance();
                state.phase = Phase::Refreshing;
                self.publish(Phase::Refreshing);
                let owner = self.clone();
                self.inner.tasks.spawn(async move {
                    owner.refresh(generation, record).await;
                });
            } else if state.phase != Phase::Refreshing {
                return Err(state.failure.unwrap_or(SessionFailure::NeedsLogin));
            }
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
            let consumed = SecretRecord {
                tokens: previous.tokens.clone(),
                admitted_at: previous.admitted_at.clone(),
                refresh: RefreshState::Consumed,
            };
            self.persist(generation, Some(consumed.encode())).await?;
            if !self.is_current(generation) {
                return Err(SessionFailure::Cancelled);
            }
            let response = self
                .inner
                .api
                .refresh(
                    auth::RefreshSessionRequest {
                        refresh_token: previous.tokens.refresh_token.clone(),
                        ..Default::default()
                    },
                    &self.token_for(generation)?,
                )
                .await
                .map_err(|error| SessionFailure::RefreshUncertain(error.kind()))?;
            let tokens = response
                .tokens
                .into_option()
                .ok_or(SessionFailure::InvalidResponse)?;
            if tokens.session.id != previous.tokens.session.id
                || tokens.session.account_id != previous.tokens.session.account_id
                || tokens.session.expires_at != previous.tokens.session.expires_at
                || tokens.session.created_at != previous.tokens.session.created_at
                || tokens.session.authenticated_at != previous.tokens.session.authenticated_at
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
                            session_id: previous.tokens.session.id.clone(),
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
        use auth::AuthorizationTerminationReason as Reason;
        let reason = match failure.authorization_termination() {
            Ok(Some(detail)) => Some(detail.reason.as_known()),
            Ok(None)
                if failure.kind()
                    == FailureKind::Rpc(home_client_transport::RpcCode::Unauthenticated) =>
            {
                Some(None)
            }
            Ok(None) => return Ok(SessionReaction::Unchanged),
            Err(_) => Some(None),
        };
        let mut state = self.inner.state.lock().await;
        if !self.is_current(generation) {
            return Ok(SessionReaction::IgnoredOldGeneration);
        }
        let next = self.advance();
        let reaction = match reason.flatten() {
            Some(Reason::AuthorityUnavailable) => {
                state.phase = Phase::AuthorityUnavailable;
                state.failure = Some(SessionFailure::AuthorityUnavailable);
                SessionReaction::ViewSuspended
            }
            Some(Reason::RightsChanged) => {
                state.phase = Phase::RightsChanged;
                state.failure = Some(SessionFailure::NeedsLogin);
                SessionReaction::ViewReset
            }
            Some(
                Reason::CredentialRevoked
                | Reason::CredentialExpired
                | Reason::AccountDisabled
                | Reason::Unspecified,
            )
            | None => {
                self.inner.identity_epoch.fetch_add(1, Ordering::SeqCst);
                state.record = None;
                state.challenge = None;
                state.phase = Phase::NeedsLogin;
                state.failure = Some(SessionFailure::NeedsLogin);
                SessionReaction::CredentialsDiscarded
            }
        };
        self.publish_failure(state.phase, state.failure);
        drop(state);
        if reaction == SessionReaction::CredentialsDiscarded
            && let Err(error) = self.persist(next, None).await
        {
            return Err(self.fail(next, error).await);
        }
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
