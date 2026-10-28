use super::*;
use home_client_session::AccessLease;

enum Error {
    Transport(Failure),
    Cancelled,
    Invalid,
    Unsupported,
}
enum Received {
    Descriptor(wire::Context),
    Record(wire::Context, Box<wire::Installation>),
}

impl Companion {
    fn current(
        &self,
        scope: u64,
        fence: u64,
        session: &SessionCoordinator,
        lease: &AccessLease,
    ) -> bool {
        let state = self.0.state.lock().expect("companion admission");
        state.scope == scope
            && state.fence == fence
            && state.active
            && state.foreground
            && !state.cancel.is_cancelled()
            && state.validated == Some(lease.generation)
            && session.is_current(lease.generation)
            && !lease.cancellation.is_cancelled()
    }
    async fn read_once(
        &self,
        api: &CompanionTransport,
        lease: &AccessLease,
        stop: &CancellationToken,
    ) -> Result<Received, Error> {
        let request = {
            let state = self.0.state.lock().expect("companion read");
            state.pending.as_ref().map(Pending::selector).or_else(|| {
                state.record.as_ref().map(|r| wire::GetInstallationRequest {
                    selector: Some(wire::get_installation_request::Selector::Id(Box::new(
                        (*r.id).clone(),
                    ))),
                    ..Default::default()
                })
            })
        };
        if let Some(request) = request {
            admit::admit_request(admit::Request::Get(&request)).map_err(|_| Error::Invalid)?;
            let response = tokio::select! {biased;()=lease.cancellation.cancelled()=>return Err(Error::Cancelled),
            response=api.get_installation(request,lease.token(),Duration::from_secs(5),stop)=>response.map_err(Error::Transport)?};
            admit::admit_response(admit::Response::Get(&response)).map_err(|_| Error::Invalid)?;
            Ok(Received::Record(
                (*response.context).clone(),
                Box::new((*response.installation).clone()),
            ))
        } else {
            let response = tokio::select! {biased;()=lease.cancellation.cancelled()=>return Err(Error::Cancelled),
            response=api.describe_companion(wire::DescribeCompanionRequest::default(),lease.token(),Duration::from_secs(5),stop)=>response.map_err(Error::Transport)?};
            admit::admit_response(admit::Response::Describe(&response))
                .map_err(|_| Error::Invalid)?;
            Ok(Received::Descriptor((*response.context).clone()))
        }
    }
    async fn write_once(
        api: &CompanionTransport,
        lease: &AccessLease,
        stop: &CancellationToken,
        pending: &Pending,
    ) -> Result<Received, Error> {
        macro_rules! call {($method:ident,$r:ident,$request:ident,$response:ident)=>{{
            admit::admit_request(admit::Request::$request($r)).map_err(|_|Error::Invalid)?;
            let response=tokio::select! {biased;()=lease.cancellation.cancelled()=>return Err(Error::Cancelled),
                response=api.$method((**$r).clone(),lease.token(),Duration::from_secs(5),stop)=>response.map_err(Error::Transport)?};
            admit::admit_response(admit::Response::$response(&response)).map_err(|_|Error::Invalid)?;
            Ok(Received::Record((*response.context).clone(),Box::new((*response.installation).clone())))
        }};}
        match &pending.request {
            Write::Register(r) => call!(register_installation, r, Register, Register),
            Write::Consent(r) => call!(update_consent, r, Consent, Consent),
            Write::Publish(r) => call!(publish_telemetry, r, Publish, Publish),
            Write::Revoke(r) => call!(revoke_installation, r, Revoke, Revoke),
        }
    }
    fn accept_received(&self, scope: u64, fence: u64, value: Received, resolving: bool) -> bool {
        let mut state = self.0.state.lock().expect("companion response");
        if state.scope != scope || state.fence != fence || !state.active || !state.foreground {
            return false;
        }
        match value {
            Received::Descriptor(context) => {
                // Discovery is a read; it never grants collection or creates an
                // installation. A changed epoch retires old metadata.
                if state.context.as_ref().is_some_and(|c| c != &context) {
                    state.record = None;
                    state.pending = None;
                    state.draft = None;
                    state.need_read();
                }
                state.context = Some(context);
                state.stage = Stage::Unregistered;
                state.running = false;
            }
            Received::Record(context, record) => {
                if !state.accept(context, *record, resolving) {
                    state.invalid();
                    self.post(&state);
                    return false;
                }
            }
        }
        self.post(&state);
        true
    }
    async fn prepare(
        &self,
        scope: u64,
        fence: u64,
        session: &SessionCoordinator,
        lease: &AccessLease,
        stop: &CancellationToken,
        intent: Intent,
    ) -> Result<Pending, Error> {
        let request = match intent {
            Intent::Register(expected) => {
                let source = self.0.source.clone();
                let device = tokio::task::spawn_blocking(move || source.device())
                    .await
                    .map_err(|_| Error::Unsupported)?
                    .ok_or(Error::Unsupported)?;
                if !self.current(scope, fence, session, lease) || stop.is_cancelled() {
                    return Err(Error::Cancelled);
                }
                let nonce =
                    home_client_actions::CommandId::generate().map_err(|_| Error::Unsupported)?;
                let request =
                    admit::RequestId::parse(nonce.as_str()).map_err(|_| Error::Invalid)?;
                Write::Register(Box::new(wire::RegisterInstallationRequest {
                    request_id: request.as_str().into(),
                    device: device.into(),
                    expected: expected.into(),
                    ..Default::default()
                }))
            }
            Intent::Consent {
                context,
                prior,
                desired,
            } => Write::Consent(Box::new(wire::UpdateConsentRequest {
                expected: context.into(),
                id: prior.id.clone(),
                expected_revision: prior.revision,
                consent: desired.into(),
                ..Default::default()
            })),
            Intent::Revoke { context, prior } => {
                Write::Revoke(Box::new(wire::RevokeInstallationRequest {
                    expected: context.into(),
                    id: prior.id.clone(),
                    expected_revision: prior.revision,
                    ..Default::default()
                }))
            }
        };
        let state = self.0.state.lock().expect("companion prepared");
        Ok(Pending {
            request,
            prior: state.record.clone().map(Box::new),
            privacy: state.privacy,
            resolution_read: false,
        })
    }
    async fn sampled(
        &self,
        scope: u64,
        fence: u64,
        session: &SessionCoordinator,
        api: &CompanionTransport,
        lease: &AccessLease,
        stop: &CancellationToken,
    ) -> Result<Pending, Error> {
        // Always re-read the server grant before reading the OS. An expired,
        // revoked, withdrawn or foreign installation cannot admit a sample.
        let current = self.read_once(api, lease, stop).await?;
        if !self.current(scope, fence, session, lease)
            || !self.accept_received(scope, fence, current, false)
        {
            return Err(Error::Cancelled);
        }
        let (context, record, privacy) = {
            let mut state = self.0.state.lock().expect("companion sample grant");
            if state.gate != Gate::Open || state.stage != Stage::Ready {
                return Err(Error::Cancelled);
            }
            let Some(record) = &state.record else {
                return Err(Error::Invalid);
            };
            if !record.consent.battery && !record.consent.connectivity && !record.consent.activity {
                return Err(Error::Cancelled);
            }
            let result = (
                state.context.clone().ok_or(Error::Invalid)?,
                record.clone(),
                state.privacy,
            );
            state.running = true;
            result
        };
        let source = self.0.source.clone();
        let consent = (*record.consent).clone();
        let token = stop.clone();
        let sampled = tokio::task::spawn_blocking(move || source.sample(&consent, &token))
            .await
            .map_err(|_| Error::Unsupported)?;
        let (snapshot, cost) = sampled.ok_or_else(|| {
            if stop.is_cancelled() {
                Error::Cancelled
            } else {
                Error::Unsupported
            }
        })?;
        if !self.current(scope, fence, session, lease) {
            return Err(Error::Cancelled);
        }
        let mut state = self.0.state.lock().expect("companion sampled");
        if state.privacy != privacy
            || state.gate != Gate::Open
            || state.record.as_ref().is_none_or(|current| {
                current.revision != record.revision || current.id != record.id
            })
        {
            return Err(Error::Cancelled);
        }
        let sequence = record
            .last_sequence
            .checked_add(1)
            .filter(|s| *s < u64::MAX)
            .ok_or(Error::Invalid)?;
        state.samples = state.samples.saturating_add(1);
        state.sample_time = state.sample_time.saturating_add(cost);
        Ok(Pending {
            request: Write::Publish(Box::new(wire::PublishTelemetryRequest {
                expected: context.into(),
                id: record.id.clone(),
                expected_revision: record.revision,
                sequence,
                snapshot: snapshot.into(),
                ..Default::default()
            })),
            prior: Some(Box::new(record)),
            privacy,
            resolution_read: false,
        })
    }
    async fn execute(
        &self,
        ticket: Ticket,
        session: &SessionCoordinator,
        api: &CompanionTransport,
        lease: &AccessLease,
        stop: &CancellationToken,
        job: Job,
    ) -> Result<Received, Error> {
        let Ticket { scope, fence } = ticket;
        let prepared = match job {
            Job::Read => return self.read_once(api, lease, stop).await,
            Job::Intent(intent) => {
                self.prepare(scope, fence, session, lease, stop, intent)
                    .await?
            }
            Job::Sample => {
                self.sampled(scope, fence, session, api, lease, stop)
                    .await?
            }
        };
        if !self.current(scope, fence, session, lease) {
            return Err(Error::Cancelled);
        }
        {
            let mut state = self.0.state.lock().expect("companion submission");
            if matches!(&prepared.request, Write::Register(_)) {
                state.record = None;
            }
            state.pending = Some(prepared.clone());
            self.post(&state);
        }
        Self::write_once(api, lease, stop, &prepared).await
    }
    pub(super) async fn work(
        &self,
        scope: u64,
        fence: u64,
        session: SessionCoordinator,
        api: CompanionTransport,
        stop: CancellationToken,
        job: Job,
    ) {
        let lease = match session.access(&stop).await {
            Ok(lease) => lease,
            Err(_) => {
                let mut state = self.0.state.lock().expect("companion missing access");
                if state.scope == scope {
                    state.running = false;
                    if state.fence == fence {
                        state.need_read();
                        state.purge_sample();
                        state.stage = Stage::Offline;
                        self.post(&state);
                    }
                }
                return;
            }
        };
        {
            let mut state = self.0.state.lock().expect("companion native identity");
            if state.scope != scope
                || state.fence != fence
                || !state.identity(Identity {
                    account: lease.account_id().clone(),
                    session: lease.session_id().clone(),
                })
            {
                state.running = false;
                self.post(&state);
                return;
            }
        }
        {
            let mut state = self.0.state.lock().expect("companion verified subject");
            if state.scope != scope || state.fence != fence {
                return;
            }
            state.validated = Some(lease.generation);
        }
        let result = self
            .execute(Ticket { scope, fence }, &session, &api, &lease, &stop, job)
            .await;
        if let Err(Error::Transport(error)) = &result {
            let _ = session.observe_failure(lease.generation, error).await;
        }
        let still_native =
            session.is_current(lease.generation) && !lease.cancellation.is_cancelled();
        if !self.current(scope, fence, &session, &lease) {
            let mut state = self.0.state.lock().expect("companion late result");
            if state.scope == scope {
                state.running = false;
                if state.pending.is_some() {
                    state.requested = true;
                }
                if !still_native {
                    state.need_read();
                    state.purge_sample();
                }
                self.post(&state);
            }
            return;
        }
        match result {
            Ok(value) => {
                let resolving = self
                    .0
                    .state
                    .lock()
                    .expect("companion resolve")
                    .pending
                    .is_some();
                self.accept_received(scope, fence, value, resolving);
            }
            Err(error) => {
                let mut state = self.0.state.lock().expect("companion failure");
                state.running = false;
                state.purge_sample();
                match error {
                    Error::Invalid => state.invalid(),
                    Error::Unsupported => {
                        state.stage = Stage::Unsupported;
                        state.gate = Gate::UserStopped;
                        state.notice =
                            "Les services natifs nécessaires ne sont pas disponibles sur cet OS."
                                .into();
                    }
                    Error::Cancelled => {
                        if state.pending.is_some() {
                            state.stage = Stage::Uncertain;
                            state.requested = true;
                        }
                    }
                    Error::Transport(error) => {
                        let missing = error.kind() == FailureKind::Rpc(RpcCode::NotFound);
                        if missing
                            && state
                                .pending
                                .as_ref()
                                .is_some_and(|p| matches!(&p.request, Write::Register(_)))
                        {
                            state.pending = None;
                            state.record = None;
                            state.stage = Stage::Unregistered;
                            state.gate = Gate::UserStopped;
                            state.notice="Aucune inscription trouvée pour cette tentative. Aucune nouvelle inscription n’est envoyée automatiquement.".into();
                        } else if state.pending.is_some() {
                            state.stage = Stage::Uncertain;
                            state.need_read();
                            // Only the next job is a READ. If that read also
                            // fails, remain stopped until an explicit refresh.
                            state.requested =
                                state.pending.as_ref().is_some_and(|p| !p.resolution_read);
                            state.notice="Réponse incertaine : lecture de résolution, sans renvoi de mutation.".into();
                        } else {
                            state.need_read();
                            state.stage = match error.kind() {
                                FailureKind::Rpc(
                                    RpcCode::Unauthenticated | RpcCode::PermissionDenied,
                                ) => Stage::Denied,
                                FailureKind::Rpc(RpcCode::Unimplemented) => Stage::Unsupported,
                                FailureKind::Rpc(RpcCode::NotFound) => {
                                    state.record = None;
                                    Stage::Revoked
                                }
                                FailureKind::InvalidConfiguration
                                | FailureKind::UntrustedCertificate
                                | FailureKind::WrongServerName
                                | FailureKind::TlsRejected => Stage::Invalid,
                                FailureKind::NetworkInterrupted
                                | FailureKind::Deadline
                                | FailureKind::Cancelled
                                | FailureKind::Rpc(_) => Stage::Offline,
                            };
                            state.notice="Collecte arrêtée. Actualisez pour revérifier l’autorisation ; aucun échantillon n’est mis en attente.".into();
                        }
                    }
                }
                self.post(&state);
            }
        }
    }
}
