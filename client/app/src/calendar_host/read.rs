use super::*;
use home_client_cache::calendars::{CalendarReadEvent, CalendarReadExit, read_calendar_once};
use home_client_model::{api::HomeName, calendars::CalendarId};
use home_client_session::Generation;
use home_client_transport::{Failure, FailureKind, RpcCode};
use std::time::Duration;
use tokio::task::JoinSet;
const TIMEOUT: Duration = Duration::from_secs(10);

fn failure_status(error: &Failure) -> Status {
    match error.api_failure() {
        Err(_) => Status::Invalid,
        Ok(Some(error)) => {
            use wire::ApiErrorReason as R;
            match error.reason() {
                R::AccessDenied | R::AuthenticationRequired => Status::Denied,
                R::MethodNotSupported => Status::Unsupported,
                R::ServiceUnavailable | R::RequestDeadlineExceeded | R::RequestCancelled => {
                    Status::Offline
                }
                R::ResourceNotFound
                | R::CapabilityUnavailable
                | R::ResyncRequired
                | R::ConfigChanged
                | R::TopologyChanged
                | R::StatePreconditionFailed
                | R::RateLimited
                | R::RequestTooLarge
                | R::StreamCapacity
                | R::QueryBudgetExceeded => Status::Offline,
                R::AuthorityUnavailable
                | R::ValidationFailed
                | R::ResourceAlreadyExists
                | R::RequestIdReused
                | R::EtagMismatch
                | R::SourceReadOnly
                | R::ConfigRestartRequired
                | R::CommitOutcomeUnknown
                | R::EffectOutcomeUnknown
                | R::InternalFailure
                | R::StoredDataIntegrity
                | R::Unspecified => Status::Invalid,
            }
        }
        Ok(None) => {
            if matches!(
                error.kind(),
                FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated)
            ) {
                Status::Denied
            } else if error.may_resume_read() {
                Status::Offline
            } else {
                Status::Invalid
            }
        }
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
    pub(super) fn failed(&self, context: u64, status: Status) {
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
        let notice = match status {
            Status::Denied => "Access removed: calendars and drafts cleared.",
            Status::Unsupported => "The server does not yet provide this calendar interface.",
            Status::Invalid => "Calendar response rejected; data removed.",
            Status::TooLarge => {
                "This range exceeds the display limit. Shorten the date range and reload it."
            }
            // Read availability is projected from Status. Keep any independent
            // mutation warning so recovery cannot erase a conflict/unknown commit.
            Status::Offline => &state.notice,
            Status::Loading | Status::Ready => "",
        };
        state.notice = notice.into();
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
        let discovery = api
            .home()
            .list_homes(
                wire::ListHomesRequest {
                    page_size: 2,
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
        let home = match discovery {
            Ok(homes)
                if homes.__buffa_unknown_fields.is_empty()
                    && homes.next_page_token.is_empty()
                    && homes.homes.len() == 1
                    && homes.homes[0].__buffa_unknown_fields.is_empty() =>
            {
                HomeName::try_from(homes.homes[0].name.as_str()).ok()
            }
            Ok(_) => None,
            Err(error) => {
                let status = failure_status(&error);
                let _ = session.observe_failure(lease.generation, &error).await;
                self.failed(context, status);
                return;
            }
        };
        let Some(home) = home else {
            self.failed(context, Status::Invalid);
            return;
        };
        let mut catalog = Vec::new();
        let mut names = std::collections::BTreeSet::new();
        let mut tokens = std::collections::BTreeSet::new();
        let mut page_token = String::new();
        loop {
            let response = api
                .query_calendar_sources(
                    wire::QueryCalendarSourcesRequest {
                        parent: home.as_str().into(),
                        page_size: 256,
                        page_token: page_token.clone(),
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
            let page = match response {
                Ok(page) => match admit::admit_sources(&page, &home) {
                    Ok(value) => value,
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
            if catalog
                .len()
                .saturating_add(page.wire().calendar_sources.len())
                > 256
                || tokens.len() >= 32
            {
                self.failed(context, Status::TooLarge);
                return;
            }
            for source in &page.wire().calendar_sources {
                if !names.insert(source.calendar.clone()) {
                    self.failed(context, Status::Invalid);
                    return;
                }
                catalog.push(source.clone());
            }
            page_token = page.wire().next_page_token.clone();
            if page_token.is_empty() {
                break;
            }
            if !tokens.insert(page_token.clone()) {
                self.failed(context, Status::Invalid);
                return;
            }
        }
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
        if state.selected_source().is_none() {
            state.selected = if let Some(target) = &self.0.widget {
                Some(target.calendar.native_id().clone())
            } else {
                state
                    .catalog
                    .first()
                    .and_then(|source| CalendarName::try_from(source.calendar.as_str()).ok())
                    .map(|name| name.native_id().clone())
            };
            state.snapshot = None;
            state.draft = None;
        }
        self.pin_widget(&mut state);
        state.reset_reads();
        if state.catalog.is_empty() {
            state.notice = "The server provided no authorized calendars.".into();
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
                CalendarReadEvent::NotMaterialized(name) => {
                    if state.selected_name().as_ref() == Some(&name) {
                        state.snapshot = None;
                        state.ready = true;
                        state.source = Some(lease.generation);
                        state.status = Status::Ready;
                        for source in &mut state.catalog {
                            if source.calendar == name.as_str() {
                                source.materialized = false;
                            }
                        }
                    }
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
                | admit::AdmissionError::StaleVersion
                | admit::AdmissionError::ReadOnly
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
                let outcome = (*prepared)
                    .submit(&api, &session, &cancel, guard, receipt_current)
                    .await;
                let mut state = self.0.state.lock().expect("calendar state");
                if state.context != context {
                    return;
                }
                state.busy = false;
                let phase = match outcome {
                    CalendarWriteOutcome::Committed(result) => {
                        use home_client_actions::calendars::ConfirmedMutation;
                        match result.as_ref() {
                            ConfirmedMutation::Calendar(resource) => {
                                for source in &mut state.catalog {
                                    if source.calendar == resource.wire().name {
                                        source.materialized = true;
                                    }
                                }
                            }
                            ConfirmedMutation::DeletedCalendar(name) => {
                                for source in &mut state.catalog {
                                    if source.calendar == name.as_str() {
                                        source.materialized = false;
                                        source.mutation_access =
                                            wire::CalendarMutationAccess::Denied.into();
                                    }
                                }
                            }
                            ConfirmedMutation::Event(_) | ConfirmedMutation::DeletedEvent(_) => {}
                        }
                        state.draft = None;
                        state.snapshot = None;
                        state.reset_reads();
                        state.notice.clear();
                        None
                    }
                    CalendarWriteOutcome::Conflict => {
                        state.notice="Conflict: your draft is unchanged. Reload or explicitly resolve the conflict before saving.".into();
                        Some(EditPhase::Conflict)
                    }
                    CalendarWriteOutcome::CommitUnknown(_) => {
                        state.uncertain = true;
                        state.notice="Save outcome unknown · no automatic resubmission. Reload explicitly before submitting another action.".into();
                        Some(EditPhase::Unknown)
                    }
                    CalendarWriteOutcome::AuthorityTerminated => {
                        state.clear_private();
                        state.status = Status::Denied;
                        state.notice = "Authorization removed; private data cleared.".into();
                        None
                    }
                    CalendarWriteOutcome::NotSent(_) => {
                        state.notice = "Action not sent: the context changed.".into();
                        Some(EditPhase::Ready)
                    }
                    CalendarWriteOutcome::Rejected(_) => {
                        state.notice =
                            "Change rejected by the server; it will not be resubmitted.".into();
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
                    .get_calendar_event(
                        wire::GetCalendarEventRequest {
                            name: key.as_str().into(),
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
                        match admit::admit_event(&response, &key) {
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
                        if error.api_failure().ok().flatten().is_some_and(|failure| {
                            failure.reason() == wire::ApiErrorReason::ResourceNotFound
                        }) {
                            let mut state = self.0.state.lock().expect("calendar state");
                            if state.finish_read_job(context, generation) {
                                if let Some(draft) = &mut state.draft {
                                    draft.phase = EditPhase::Conflict;
                                }
                                state.notice = "This event is no longer available. Reload the calendar before editing.".into();
                                state.reset_reads();
                            }
                            self.publish(&state);
                            self.0.changed.notify_one();
                            return;
                        }
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
                    .is_none_or(|calendar| calendar.etag != value.wire().calendar_etag)
                {
                    state.notice = "The calendar revision changed; reload the range.".into();
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
                        "Event revision reloaded. Nothing was saved automatically."
                    } else {
                        "This event is missing or has been deleted."
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
                    .resolve_calendar_time(request.clone(), lease.token(), TIMEOUT, &cancel)
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
                            state.notice = "Time resolution rejected or draft changed.".into();
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
                self.pin_widget(&mut state);
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
                _=clock.tick()=>{let mut state=self.0.state.lock().expect("calendar state");if state.active&&state.status==Status::Offline{state.reset_reads();}
                    if state.active&&let Some(target)=&self.0.widget {
                        let expired=state.query_override.as_ref().and_then(|query|query.window.as_option()).and_then(|window|match &window.bounds{Some(wire::calendar_window::Bounds::Times(span))=>admit::timestamp_text(&span.start_time).ok().and_then(|value|chrono::DateTime::parse_from_rfc3339(&value).ok()),Some(wire::calendar_window::Bounds::Dates(_))|None=>None}).is_none_or(|start|chrono::Utc::now().signed_duration_since(start).num_seconds()>=60);
                        if expired {state.query_override=state.selected_name().as_ref().and_then(|name|state::widget_query(target,name,chrono::Utc::now()));state.reset_reads();}
                    }
                    self.publish(&state);},
            }
        }
        attempt.cancel();
        self.clear();
        while tasks.join_next().await.is_some() {}
        *self.0.session.lock().expect("calendar session") = None;
    }
}
