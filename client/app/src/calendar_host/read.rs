use super::*;
use home_client_cache::calendars::{CalendarReadEvent, CalendarReadExit, read_calendar_once};
use home_client_session::Generation;
use home_client_transport::{Failure, FailureKind, RpcCode};
use std::time::Duration;
use tokio::task::JoinSet;
const TIMEOUT: Duration = Duration::from_secs(10);

fn failure_status(error: &Failure) -> Status {
    if !matches!(error.authorization_termination(), Ok(None)) {
        return Status::Denied;
    }
    match error.kind() {
        FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated) => Status::Denied,
        FailureKind::Rpc(RpcCode::Unimplemented) => Status::Unsupported,
        FailureKind::Rpc(RpcCode::InvalidArgument | RpcCode::OutOfRange | RpcCode::DataLoss) => {
            Status::Invalid
        }
        FailureKind::Rpc(
            RpcCode::Canceled
            | RpcCode::Unknown
            | RpcCode::DeadlineExceeded
            | RpcCode::NotFound
            | RpcCode::AlreadyExists
            | RpcCode::ResourceExhausted
            | RpcCode::FailedPrecondition
            | RpcCode::Aborted
            | RpcCode::Internal
            | RpcCode::Unavailable,
        )
        | FailureKind::InvalidConfiguration
        | FailureKind::UntrustedCertificate
        | FailureKind::WrongServerName
        | FailureKind::TlsRejected
        | FailureKind::NetworkInterrupted
        | FailureKind::Deadline
        | FailureKind::Cancelled => Status::Offline,
        // connectrpc declares its RPC code non-exhaustive. Unknown wire values
        // cannot establish a usable, commandable calendar projection.
        FailureKind::Rpc(_) => Status::Invalid,
    }
}
impl Calendars {
    fn job_failed(&self, context: u64, status: Status) {
        {
            let mut state = self.0.state.lock().expect("calendar state");
            if state.context != context {
                return;
            }
            state.busy = false;
        }
        self.failed(context, status);
    }
    fn failed(&self, context: u64, status: Status) {
        let mut state = self.0.state.lock().expect("calendar state");
        if state.context != context {
            return;
        }
        match status {
            Status::Denied | Status::Unsupported | Status::Invalid => {
                state.clear_private();
                let _ = self.0.mailbox.invalidate();
            }
            Status::Offline => {
                state.ready = false;
                state.bump();
            }
            Status::TooLarge => {
                state.snapshot = None;
                state.ready = false;
                state.draft = None;
                state.busy = false;
                state.bump();
            }
            Status::Loading | Status::Ready => return,
        }
        state.status = status;
        state.notice = match status {
            Status::Denied => "Accès retiré : calendriers et brouillons effacés.",
            Status::Unsupported => "Le serveur ne propose pas encore cette interface calendrier.",
            Status::Invalid => "Réponse calendrier non admise ; données retirées.",
            Status::TooLarge => {
                "Cette plage dépasse la limite de présentation. Réduisez les dates pour la relire."
            }
            Status::Offline => "Hors ligne · copie datée conservée · aucune écriture rejouée.",
            Status::Loading | Status::Ready => "",
        }
        .into();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    async fn catalog(
        &self,
        session: SessionCoordinator,
        api: CalendarTransport,
        context: u64,
        cancel: CancellationToken,
    ) {
        let lease = match session.access(&cancel).await {
            Ok(lease) => lease,
            Err(_) => {
                self.failed(context, Status::Denied);
                return;
            }
        };
        let response = api
            .list_calendars(
                wire::ListCalendarsRequest::default(),
                lease.token(),
                TIMEOUT,
                &cancel,
            )
            .await;
        if cancel.is_cancelled()
            || lease.cancellation.is_cancelled()
            || !session.is_current(lease.generation)
        {
            return;
        }
        let catalog = match response {
            Ok(response) => match admit::admit_catalog(&response) {
                Ok(catalog) => catalog.wire().calendars.clone(),
                Err(_) => {
                    self.failed(context, Status::Invalid);
                    return;
                }
            },
            Err(error) => {
                let status = failure_status(&error);
                let _ = session.observe_failure(lease.generation, &error).await;
                self.failed(context, status);
                return;
            }
        };
        let mut state = self.0.state.lock().expect("calendar state");
        if state.context != context
            || cancel.is_cancelled()
            || !session.is_current(lease.generation)
        {
            return;
        }
        state.catalog = catalog;
        state.source = Some(lease.generation);
        state.catalog_needed = false;
        state.status = Status::Ready;
        if !state
            .catalog
            .iter()
            .any(|calendar| admit::calendar_id(&calendar.id).as_ref() == state.selected.as_ref())
        {
            state.selected = state
                .catalog
                .first()
                .and_then(|calendar| admit::calendar_id(&calendar.id));
            state.snapshot = None;
            state.draft = None;
            state.reset_reads();
        }
        if state.catalog.is_empty() {
            state.notice = "Aucun calendrier autorisé fourni par le serveur.".into();
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    async fn watch(
        &self,
        session: SessionCoordinator,
        api: CalendarTransport,
        context: u64,
        query: wire::QueryOccurrencesRequest,
        cancel: CancellationToken,
    ) {
        let lease = match session.access(&cancel).await {
            Ok(lease) => lease,
            Err(_) => {
                self.failed(context, Status::Denied);
                return;
            }
        };
        let exit = read_calendar_once(&api, &lease, query, &cancel, |event| {
            let mut state = self.0.state.lock().expect("calendar state");
            if state.context != context
                || cancel.is_cancelled()
                || lease.cancellation.is_cancelled()
                || !session.is_current(lease.generation)
            {
                return;
            }
            match event {
                CalendarReadEvent::Invalidated => {
                    state.ready = false;
                }
                CalendarReadEvent::Published(snapshot) => {
                    state.snapshot(snapshot, lease.generation)
                }
            }
            self.publish(&state);
        })
        .await;
        if cancel.is_cancelled()
            || lease.cancellation.is_cancelled()
            || !session.is_current(lease.generation)
        {
            return;
        }
        match exit {
            CalendarReadExit::Transport(error) => {
                let status = failure_status(&error);
                let _ = session.observe_failure(lease.generation, &error).await;
                self.failed(context, status);
            }
            CalendarReadExit::Contract(admit::AdmissionError::Capacity) => {
                self.failed(context, Status::TooLarge)
            }
            CalendarReadExit::Contract(
                admit::AdmissionError::Invalid
                | admit::AdmissionError::ForeignResource
                | admit::AdmissionError::StaleRevision
                | admit::AdmissionError::Sequence,
            ) => self.failed(context, Status::Invalid),
            CalendarReadExit::Cancelled => {}
            CalendarReadExit::Ended | CalendarReadExit::Resync | CalendarReadExit::TimedOut => {
                self.failed(context, Status::Offline)
            }
        }
    }
    async fn execute(
        &self,
        session: SessionCoordinator,
        api: CalendarTransport,
        context: u64,
        generation: i32,
        job: Job,
        cancel: CancellationToken,
    ) {
        match job {
            Job::Write(prepared) => {
                let owner = self.clone();
                let guard: home_client_actions::AdmissionCheck = Arc::new(move || {
                    let state = owner.0.state.lock().expect("calendar state");
                    state.owns_write(context) && state.generation == generation && state.editable()
                });
                let owner = self.clone();
                let receipt_current: home_client_actions::AdmissionCheck = Arc::new(move || {
                    owner
                        .0
                        .state
                        .lock()
                        .expect("calendar state")
                        .owns_write(context)
                });
                let outcome = prepared
                    .submit(&api, &session, &cancel, guard, receipt_current)
                    .await;
                let mut state = self.0.state.lock().expect("calendar state");
                if state.context != context {
                    return;
                }
                state.busy = false;
                let phase = match outcome {
                    CalendarWriteOutcome::Committed(_) => {
                        state.draft = None;
                        state.catalog_needed = true;
                        state.reset_reads();
                        state.notice = "Modification enregistrée ; relecture du serveur.".into();
                        None
                    }
                    CalendarWriteOutcome::Conflict => {
                        state.notice="Conflit : le brouillon reste intact. Relisez ou résolvez explicitement avant d’enregistrer.".into();
                        Some(EditPhase::Conflict)
                    }
                    CalendarWriteOutcome::CommitUnknown(_) => {
                        state.uncertain = true;
                        state.notice="Résultat du commit inconnu · aucun renvoi automatique. Relisez explicitement avant une nouvelle intention.".into();
                        Some(EditPhase::Unknown)
                    }
                    CalendarWriteOutcome::CommittedResponseWithheld => {
                        state.clear_private();
                        state.status = Status::Denied;
                        state.notice =
                            "Modification admise, réponse retirée après changement de droits."
                                .into();
                        Some(EditPhase::Withheld)
                    }
                    CalendarWriteOutcome::AuthorityTerminated => {
                        state.clear_private();
                        state.status = Status::Denied;
                        state.notice = "Autorité retirée ; données privées effacées.".into();
                        None
                    }
                    CalendarWriteOutcome::NotSent(_) => {
                        state.notice = "Intention non envoyée : le contexte a changé.".into();
                        Some(EditPhase::Ready)
                    }
                    CalendarWriteOutcome::Rejected(_) => {
                        state.notice = "Modification refusée par le serveur ; aucun renvoi.".into();
                        Some(EditPhase::Ready)
                    }
                };
                if let Some(phase) = phase
                    && let Some(draft) = &mut state.draft
                {
                    draft.phase = phase;
                }
                self.publish(&state);
                self.0.changed.notify_one();
            }
            Job::Open(key, mode) => {
                let lease = match session.access(&cancel).await {
                    Ok(lease) => lease,
                    Err(_) => {
                        self.failed(context, Status::Denied);
                        return;
                    }
                };
                let response = api
                    .get_event(
                        wire::GetEventRequest {
                            key: event_key(&key).into(),
                            ..Default::default()
                        },
                        lease.token(),
                        TIMEOUT,
                        &cancel,
                    )
                    .await;
                if cancel.is_cancelled()
                    || lease.cancellation.is_cancelled()
                    || !session.is_current(lease.generation)
                {
                    return;
                }
                let value = match response {
                    Ok(response) if response.__buffa_unknown_fields.is_empty() => {
                        match admit::admit_event(&response.event, &key) {
                            Ok(value) => value,
                            Err(_) => {
                                self.failed(context, Status::Invalid);
                                return;
                            }
                        }
                    }
                    Ok(_) => {
                        self.failed(context, Status::Invalid);
                        return;
                    }
                    Err(error) => {
                        let status = failure_status(&error);
                        let _ = session.observe_failure(lease.generation, &error).await;
                        self.job_failed(context, status);
                        return;
                    }
                };
                let mut state = self.0.state.lock().expect("calendar state");
                if !state.finish_read_job(context, generation) {
                    self.publish(&state);
                    return;
                }
                if state
                    .calendar()
                    .is_none_or(|calendar| calendar.revision != value.wire().calendar_revision)
                {
                    state.notice = "La révision du calendrier a changé ; relisez la plage.".into();
                    state.reset_reads();
                } else {
                    let current = Draft::from_server(&value);
                    state.draft = match mode {
                        Open::Replace => current,
                        Open::Rebase => match (state.draft.take(), current) {
                            (Some(mut draft), Some(latest)) if draft.key == latest.key => {
                                draft.revision = latest.revision;
                                draft.original.recurrence_rule = latest.original.recurrence_rule;
                                draft.original.additions = latest.original.additions;
                                draft.original.exclusions = latest.original.exclusions;
                                draft.original.exceptions = latest.original.exceptions;
                                draft.phase = EditPhase::Ready;
                                Some(draft)
                            }
                            (Some(_) | None, latest) => latest,
                        },
                    };
                    state.bump();
                    state.notice = if state.draft.is_some() {
                        "Révision de l’événement relue. Aucun enregistrement automatique."
                    } else {
                        "Cet événement est absent ou supprimé."
                    }
                    .into();
                }
                self.publish(&state);
            }
            Job::Resolve(boundary, request) => {
                let lease = match session.access(&cancel).await {
                    Ok(lease) => lease,
                    Err(_) => {
                        self.failed(context, Status::Denied);
                        return;
                    }
                };
                let response = api
                    .resolve_local_time(request.clone(), lease.token(), TIMEOUT, &cancel)
                    .await;
                if cancel.is_cancelled()
                    || lease.cancellation.is_cancelled()
                    || !session.is_current(lease.generation)
                {
                    return;
                }
                match response {
                    Ok(response) => {
                        let mut state = self.0.state.lock().expect("calendar state");
                        if !state.finish_read_job(context, generation) {
                            self.publish(&state);
                            return;
                        }
                        if state.draft.as_mut().is_none_or(|draft| {
                            draft.resolve(boundary, &request, &response).is_err()
                        }) {
                            state.notice =
                                "Résolution temporelle refusée ou brouillon modifié.".into();
                        }
                        self.publish(&state);
                    }
                    Err(error) => {
                        let status = failure_status(&error);
                        let _ = session.observe_failure(lease.generation, &error).await;
                        self.job_failed(context, status);
                    }
                }
            }
        }
    }
    pub async fn run(
        &self,
        session: SessionCoordinator,
        api: CalendarTransport,
        cancel: CancellationToken,
    ) {
        *self.0.session.lock().expect("calendar session") = Some(session.clone());
        let mut phases = session.subscribe();
        let mut source = phases.borrow().generation;
        let mut context: Option<(Generation, u64, Option<CalendarId>)> = None;
        let mut tasks = JoinSet::new();
        let mut attempt = cancel.child_token();
        let mut clock = tokio::time::interval(Duration::from_secs(2));
        clock.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            if self.0.retry.swap(false, Ordering::SeqCst) {
                let _ = session.allow_new_view_attempt().await;
            }
            let (next, catalog, query, job) = {
                let mut state = self.0.state.lock().expect("calendar state");
                let view = *phases.borrow();
                if view.generation != source {
                    source = view.generation;
                    state.clear_private();
                    state.status = Status::Loading;
                    let _ = self.0.mailbox.invalidate();
                }
                let permitted = match state.status {
                    Status::Denied | Status::Unsupported | Status::Invalid | Status::TooLarge => {
                        false
                    }
                    Status::Loading | Status::Ready | Status::Offline => true,
                };
                let active = matches!(view.phase, Phase::Ready | Phase::Refreshing)
                    && state.foreground
                    && state.interactive
                    && state.generation > 0
                    && state.privacy_generation > 0
                    && state.context > 0
                    && permitted;
                if !active
                    && state.active
                    && !matches!(view.phase, Phase::Ready | Phase::Refreshing)
                {
                    state.clear_private();
                    let _ = self.0.mailbox.invalidate();
                }
                state.active = active;
                let next = active.then(|| (view.generation, state.context, state.selected.clone()));
                let catalog = state.catalog_needed;
                let query = state.query();
                let job = if active {
                    state
                        .job
                        .take()
                        .map(|job| (state.context, state.generation, job))
                } else {
                    None
                };
                self.publish(&state);
                (next, catalog, query, job)
            };
            if context != next {
                attempt.cancel();
                while tasks.join_next().await.is_some() {}
                context = next;
                if let Some((_, context, _)) = &context {
                    attempt = cancel.child_token();
                    if catalog {
                        let (owner, session, api, stop, id) = (
                            self.clone(),
                            session.clone(),
                            api.clone(),
                            attempt.clone(),
                            *context,
                        );
                        tasks.spawn(async move {
                            owner.catalog(session, api, id, stop).await;
                        });
                    }
                    if let Some(query) = query {
                        let (owner, session, api, stop, id) = (
                            self.clone(),
                            session.clone(),
                            api.clone(),
                            attempt.clone(),
                            *context,
                        );
                        tasks.spawn(async move {
                            owner.watch(session, api, id, query, stop).await;
                        });
                    }
                }
            }
            if let Some((context, generation, job)) = job {
                let (owner, session, api, stop) =
                    (self.clone(), session.clone(), api.clone(), attempt.clone());
                tasks.spawn(async move {
                    owner
                        .execute(session, api, context, generation, job, stop)
                        .await;
                });
            }
            tokio::select! {biased;
                ()=cancel.cancelled()=>break,
                change=phases.changed()=>{if change.is_err(){break;}},
                ()=self.0.changed.notified()=>{},
                _=tasks.join_next(),if !tasks.is_empty()=>{},
                _=clock.tick()=>{let mut state=self.0.state.lock().expect("calendar state");if state.active&&state.status==Status::Offline{state.reset_reads();}self.publish(&state);},
            }
        }
        attempt.cancel();
        self.clear();
        while tasks.join_next().await.is_some() {}
        *self.0.session.lock().expect("calendar session") = None;
    }
}
