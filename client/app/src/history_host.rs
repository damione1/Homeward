//! One cancellable read and one joined Pro renderer per profile. UI handlers
//! only alter bounded state; TLS, sorting, raster work and joins stay off UI.
mod render;
mod state;
#[cfg(test)]
mod tls_probe;
use crate::{
    HistoryStatistic, HomewardWindow, NavigationStep, ProChartRangeGesture,
    history::{Data, Statistic},
};
use home_client_cache::{
    ProjectionMailbox, Publication,
    history::{self, Content, Query, Resolution},
};
use home_client_model::api::EntityName;
use home_client_session::{Phase, SessionCoordinator};
use home_client_transport::{FailureKind, HistoryTransport, RpcCode};
use slint::ComponentHandle;
use slint_kit_pro::charts::{ChartWorker, ReadyNotice};
use state::{Failure, State, Status};
use std::sync::{Arc, Mutex};
use tokio::{sync::Notify, task::JoinSet};
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(crate) struct History(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    changed: Notify,
    mailbox: ProjectionMailbox<render::Projection>,
    window: Option<slint::Weak<HomewardWindow>>,
    session: Mutex<Option<SessionCoordinator>>,
}
impl History {
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            changed: Notify::new(),
            mailbox: ProjectionMailbox::new(),
            window,
            session: Mutex::new(None),
        }))
    }
    #[cfg(test)]
    pub fn detached() -> Self {
        Self::new(None)
    }
    pub fn bind(window: &HomewardWindow) -> Self {
        let this = Self::new(Some(window.as_weak()));
        let end = chrono::Utc::now().timestamp().div_euclid(3600) * 3600;
        window.set_history_start(render::utc_seconds(end - 86400).into());
        window.set_history_end(render::utc_seconds(end).into());
        let owner = this.clone();
        window.on_history_private_cleared(move || owner.clear());
        let owner = this.clone();
        window.on_history_draft_changed(move || owner.draft());
        let owner = this.clone();
        let weak = window.as_weak();
        window.on_history_choose_statistic(move |stat| {
            if let Some(window) = weak.upgrade() {
                window.set_history_statistic(stat);
                owner.draft();
            }
        });
        let owner = this.clone();
        window.on_history_apply(move |start, end, stat| owner.apply(&start, &end, stat));
        let owner = this.clone();
        window.on_history_group(move |step| owner.group(step));
        let owner = this.clone();
        window.on_history_inspect(move |generation, x| owner.inspect(generation, x));
        let owner = this.clone();
        window.on_history_navigate(move |generation, step, extend| {
            owner.navigate(generation, step, extend)
        });
        let owner = this.clone();
        window.on_history_interval(move |generation, gesture, x| {
            owner.interval(generation, gesture, x)
        });
        let owner = this.clone();
        let weak = window.as_weak();
        window.on_history_viewport(move |width, height, background, accent| {
            let density = weak
                .upgrade()
                .map_or(1., |window| window.window().scale_factor());
            let mut state = owner.0.state.lock().expect("history state");
            if !width.is_finite() || !height.is_finite() || !density.is_finite() {
                return;
            }
            let width = (width * density).round().clamp(1., 8192.) as u16;
            let height = (height * density).round().clamp(1., 8192.) as u16;
            let rgb = |c: slint::Color| [c.red(), c.green(), c.blue()];
            let (background, accent) = (rgb(background), rgb(accent));
            if (width, height, background, accent, density)
                != (
                    state.width,
                    state.height,
                    state.background,
                    state.accent,
                    state.density,
                )
            {
                state.width = width;
                state.height = height;
                state.background = background;
                state.accent = accent;
                state.density = density;
                if !state.admits(state.generation) {
                    state.resume_render = state.data.is_some();
                } else {
                    state.render = state.data.is_some();
                }
                owner.0.changed.notify_one();
            }
        });
        this.publish(&this.0.state.lock().expect("history state"));
        this
    }
    fn publish(&self, state: &State) {
        let guard = state.source_generation.and_then(|generation| {
            self.0
                .session
                .lock()
                .expect("history session")
                .clone()
                .map(|session| (session, generation))
        });
        let projection = render::Projection::new(state, guard);
        if let Publication::Schedule(ticket) = self
            .0
            .mailbox
            .publish(self.0.mailbox.generation(), projection)
            && let Some(window) = &self.0.window
        {
            let owner = Arc::downgrade(&self.0);
            let _ = window.upgrade_in_event_loop(move |window| {
                Self::deliver(&owner, &window, ticket);
            });
        }
    }
    fn deliver(
        owner: &std::sync::Weak<Inner>,
        window: &HomewardWindow,
        ticket: home_client_cache::DeliveryTicket<render::Projection>,
    ) {
        let mut retained = None;
        ticket.deliver(|_, value| {
            retained = value.retained_source();
            render::render(window, value);
        });
        // Never re-enter State/mailbox while delivering a ticket. A delayed
        // raster can expire before the owner loop or a UI timer gets a turn.
        if let Some((generation, source)) = retained
            && crate::cache_view::presentation_remaining(&source).is_none()
            && let Some(inner) = owner.upgrade()
        {
            let this = Self(inner);
            let mut state = this.0.state.lock().expect("history state");
            if state.generation == generation
                && state.renewal.as_ref().or(state.presentation.as_ref()) == Some(&source)
            {
                state.expire_display();
                let _ = this.0.mailbox.invalidate();
                this.publish(&state);
                this.0.changed.notify_one();
            }
        }
    }
    pub fn suspend(&self, entity: Option<EntityName>) {
        let mut state = self.0.state.lock().expect("history state");
        if state.entity != entity {
            drop(state);
            self.clear();
            return;
        }
        if !state.paused {
            state.suspend();
            let _ = self.0.mailbox.invalidate();
            self.publish(&state);
            self.0.changed.notify_one();
        }
    }
    #[cfg(test)]
    pub fn choose(&self, entity: Option<EntityName>) {
        self.choose_presented(entity, None);
    }
    pub fn choose_presented(
        &self,
        entity: Option<EntityName>,
        presentation: Option<home_client_cache::PresentationSource>,
    ) {
        let mut state = self.0.state.lock().expect("history state");
        if state.entity == entity {
            let changed_credential = state
                .presentation
                .as_ref()
                .zip(presentation.as_ref())
                .is_some_and(|(old, new)| old.source().generation() != new.source().generation());
            if changed_credential && !state.hold_renewal() {
                state.invalidate();
            }
            state.presentation = presentation;
            if state.renewal.is_some() && state.paused {
                // Revalidate precisely the prior explicit query under fresh
                // admission. Old result permissions are never relabelled.
                state.pending = state
                    .data
                    .as_ref()
                    .map(|data| (data.snapshot.query.clone(), data.statistic));
                state.paused = false;
                state.render = false;
                state.resume_render = false;
                self.publish(&state);
                self.0.changed.notify_one();
                return;
            }
            if state.paused {
                state.paused = false;
                state.render = state.resume_render && state.data.is_some();
                state.resume_render = false;
                self.publish(&state);
                self.0.changed.notify_one();
            }
            return;
        }
        state.invalidate();
        state.entity = entity;
        state.presentation = presentation;
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    pub fn clear(&self) {
        let mut state = self.0.state.lock().expect("history state");
        state.invalidate();
        state.entity = None;
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    pub fn foreground(&self, value: bool) {
        let mut state = self.0.state.lock().expect("history state");
        state.foreground = value;
        if !value {
            state.invalidate();
            state.entity = None;
            let _ = self.0.mailbox.invalidate();
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn draft(&self) {
        let mut state = self.0.state.lock().expect("history state");
        if !state.admits(state.generation) {
            return;
        }
        let presentation = state.presentation.clone();
        state.invalidate();
        state.presentation = presentation;
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn apply(&self, start: &str, end: &str, stat: HistoryStatistic) {
        let mut state = self.0.state.lock().expect("history state");
        if !state.admits(state.generation) {
            return;
        }
        let presentation = state.presentation.clone();
        state.invalidate();
        state.presentation = presentation;
        let _ = self.0.mailbox.invalidate();
        let parsed = parse_query(state.entity.clone(), start, end, stat);
        match parsed {
            Ok(query) if state.admits(state.generation) => {
                state.pending = Some(query);
                state.status = Status::Loading;
            }
            Ok(_) | Err(_) => state.status = Status::Failed(Failure::Query),
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn group(&self, step: NavigationStep) {
        let mut state = self.0.state.lock().expect("history state");
        if !state.admits(state.generation) {
            return;
        }
        let Some(data) = &state.data else {
            return;
        };
        let count = data.groups.len();
        if count == 0 {
            return;
        }
        state.group = index(state.group, count, step);
        state.cursor = None;
        state.range = None;
        state.frame = None;
        state.expected_render = None;
        state.status = Status::Preparing;
        state.render = true;
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn inspect(&self, generation: i32, fraction: f32) {
        let mut state = self.0.state.lock().expect("history state");
        if !state.admits(generation) || !fraction.is_finite() {
            return;
        }
        let Some(data) = &state.data else {
            return;
        };
        let Some(group) = data.groups.get(state.group) else {
            return;
        };
        let Some(at) = render::time_at(data, fraction) else {
            return;
        };
        let samples = group.series.samples();
        if samples.is_empty() {
            return;
        }
        let right = samples.partition_point(|sample| sample.time() < at);
        let chosen = [
            right.checked_sub(1),
            (right < samples.len()).then_some(right),
        ]
        .into_iter()
        .flatten()
        .min_by_key(|i| (samples[*i].time().unix_nanos() - at.unix_nanos()).abs());
        state.cursor = chosen;
        self.publish(&state);
    }
    fn navigate(&self, generation: i32, step: NavigationStep, extend: bool) {
        let mut state = self.0.state.lock().expect("history state");
        if !state.admits(generation) {
            return;
        }
        let Some(data) = &state.data else {
            return;
        };
        let Some(group) = data.groups.get(state.group) else {
            return;
        };
        let count = group.series.samples().len();
        if count == 0 {
            return;
        }
        let current = state.cursor.unwrap_or(0);
        let next = index(current, count, step);
        let position = |i: usize| render::fraction(data, group.series.samples()[i].time());
        if extend {
            state.range = Some((
                state.range.map_or(position(current), |r| r.0),
                position(next),
            ));
        }
        state.cursor = Some(next);
        self.publish(&state);
    }
    fn interval(&self, generation: i32, gesture: ProChartRangeGesture, x: f32) {
        let mut state = self.0.state.lock().expect("history state");
        if !state.admits(generation) || !x.is_finite() {
            return;
        }
        let x = x.clamp(0., 1.);
        state.range = match gesture {
            ProChartRangeGesture::Begin => Some((x, x)),
            ProChartRangeGesture::Update | ProChartRangeGesture::Commit => {
                state.range.map(|range| (range.0, x))
            }
            ProChartRangeGesture::Cancel => None,
        };
        self.publish(&state);
    }
    pub async fn run(
        &self,
        session: SessionCoordinator,
        transport: HistoryTransport,
        cancel: CancellationToken,
    ) {
        *self.0.session.lock().expect("history session") = Some(session.clone());
        let ready = Arc::new(Mutex::new(None::<ReadyNotice>));
        let wake = Arc::new(Notify::new());
        let (slot, signal) = (ready.clone(), wake.clone());
        let Ok(mut worker) = ChartWorker::new(move |notice| {
            *slot.lock().expect("history render notice") = Some(notice);
            signal.notify_one();
        }) else {
            return;
        };
        let mut phases = session.subscribe();
        let mut session_generation = phases.borrow().generation;
        let mut reads = JoinSet::new();
        loop {
            let pending = {
                let mut state = self.0.state.lock().expect("history state");
                let current = *phases.borrow();
                if current.generation != session_generation {
                    session_generation = current.generation;
                    state.reconcile_session(current.generation);
                    let _ = self.0.mailbox.invalidate();
                    self.publish(&state);
                }
                if state
                    .renewal
                    .as_ref()
                    .is_some_and(|p| crate::cache_view::presentation_remaining(p).is_none())
                {
                    state.expire_display();
                    let _ = self.0.mailbox.invalidate();
                    self.publish(&state);
                }
                let active = matches!(phases.borrow().phase, Phase::Ready | Phase::Refreshing)
                    && state.foreground
                    && state.generation > 0;
                if !active && state.active {
                    state.invalidate();
                    state.entity = None;
                    let _ = self.0.mailbox.invalidate();
                    self.publish(&state);
                }
                state.active = active;
                state.pending.take().map(|query| {
                    (
                        state::ReadContext {
                            generation: state.generation,
                            cancel: state.cancel.clone(),
                            source: state.presentation.clone(),
                        },
                        query,
                    )
                })
            };
            if let Some((context, (query, statistic))) = pending {
                while reads.join_next().await.is_some() {}
                let (owner, credential, api) = (self.clone(), session.clone(), transport.clone());
                reads.spawn(async move {
                    owner.read(credential, api, query, statistic, context).await;
                });
            }
            {
                let mut state = self.0.state.lock().expect("history state");
                if state.render && !state.paused {
                    state.render = false;
                    if let Some(request) = render::request(&state) {
                        match worker.submit(request) {
                            Ok(id) => {
                                state.expected_render = Some((id, state.generation));
                                state.status = Status::Preparing;
                            }
                            Err(_) => {
                                state.frame = None;
                                state.status = Status::Failed(Failure::Capacity);
                            }
                        }
                    } else {
                        state.frame = None;
                        state.status = Status::Failed(Failure::Projection);
                    }
                    self.publish(&state);
                }
            }
            let expiry = self
                .0
                .state
                .lock()
                .expect("history state")
                .renewal
                .as_ref()
                .and_then(crate::cache_view::presentation_remaining);
            let expires = async {
                match expiry {
                    Some(delay) => tokio::time::sleep(delay).await,
                    None => std::future::pending::<()>().await,
                }
            };
            tokio::select! { biased;
                () = cancel.cancelled() => break,
                () = expires => {},
                change = phases.changed() => { if change.is_err() { break; } }
                () = self.0.changed.notified() => {}
                () = wake.notified() => {
                    let notice = ready.lock().expect("history render notice").take();
                    if let Some(result) = notice.and_then(ReadyNotice::take_ready) {
                        let mut state = self.0.state.lock().expect("history state");
                        if state.complete_render(result) {
                            self.publish(&state);
                        }
                    }
                }
                _ = reads.join_next(), if !reads.is_empty() => {}
            }
        }
        self.clear();
        while reads.join_next().await.is_some() {}
        worker.stop();
        ready.lock().expect("history render notice").take();
        *self.0.session.lock().expect("history session") = None;
    }
    async fn read(
        &self,
        session: SessionCoordinator,
        transport: HistoryTransport,
        query: Query,
        statistic: Statistic,
        context: state::ReadContext,
    ) {
        let state::ReadContext {
            generation,
            cancel,
            source,
        } = context;
        let lease = tokio::select! { biased; () = cancel.cancelled() => return, lease = session.access(&cancel) => match lease {
            Ok(lease) => lease,
            Err(_) => {
                let mut state = self.0.state.lock().expect("history state");
                if state.accepts_read(generation) { state.status = Status::Failed(Failure::Denied); self.publish(&state); }
                return;
            }
        } };
        if !self.0.state.lock().expect("history state").admits_source(
            generation,
            &source,
            lease.generation,
        ) || cancel.is_cancelled()
        {
            return;
        }
        let response = history::read_history(&transport, &lease, query, &cancel).await;
        if let Err(history::ReadError::Transport(failure)) = &response {
            let _ = session.observe_failure(lease.generation, failure).await;
        }
        let result = response.map_err(failure).and_then(|snapshot| {
            Data::from_snapshot(Arc::new(snapshot), statistic)
                .map(Arc::new)
                .map_err(|_| Failure::Projection)
        });
        let mut state = self.0.state.lock().expect("history state");
        if !state.admits_source(generation, &source, lease.generation) || cancel.is_cancelled() {
            return;
        }
        if lease.cancellation.is_cancelled() || !session.is_current(lease.generation) {
            state.invalidate();
            state.status = Status::Failed(Failure::Denied);
            let _ = self.0.mailbox.invalidate();
            self.publish(&state);
            return;
        }
        match result {
            Ok(data) => {
                state.source_generation = Some(lease.generation);
                state.status = if matches!(data.snapshot.content, Content::NotRetained(_)) {
                    Status::NotRetained
                } else if data.groups.is_empty() {
                    Status::Empty
                } else {
                    Status::Preparing
                };
                state.render = !data.groups.is_empty();
                state.data = Some(data);
                if !state.render {
                    state.renewal = None;
                }
            }
            Err(reason) => {
                state.renewal = None;
                state.data = None;
                state.frame = None;
                state.status = Status::Failed(reason);
            }
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
}
fn failure(error: history::ReadError) -> Failure {
    match error {
        history::ReadError::StaleCursor => Failure::Stale,
        history::ReadError::ServerBudget
        | history::ReadError::Deadline
        | history::ReadError::Contract(history::ReadFailure::BudgetExceeded) => Failure::Capacity,
        history::ReadError::Contract(_) | history::ReadError::Cancelled => Failure::Contract,
        history::ReadError::Transport(error) => {
            if matches!(
                error.kind(),
                FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated)
            ) {
                Failure::Denied
            } else {
                Failure::Transport
            }
        }
    }
}
fn index(current: usize, count: usize, step: NavigationStep) -> usize {
    match step {
        NavigationStep::Next => (current + 1).min(count - 1),
        NavigationStep::Previous => current.saturating_sub(1),
        NavigationStep::First => 0,
        NavigationStep::Last => count - 1,
        NavigationStep::PageNext => (current + 10).min(count - 1),
        NavigationStep::PagePrevious => current.saturating_sub(10),
    }
}
fn parse_query(
    entity: Option<EntityName>,
    start: &str,
    end: &str,
    statistic: HistoryStatistic,
) -> Result<(Query, Statistic), ()> {
    let parse = |source: &str| {
        if source.len() > 64 {
            return Err(());
        }
        let time = chrono::DateTime::parse_from_rfc3339(source).map_err(|_| ())?;
        if time.offset().local_minus_utc() != 0 {
            return Err(());
        }
        Ok(buffa_types::google::protobuf::Timestamp {
            seconds: time.timestamp(),
            nanos: time.timestamp_subsec_nanos() as i32,
            ..Default::default()
        })
    };
    let stat = match statistic {
        HistoryStatistic::Observation => Statistic::Observation,
        HistoryStatistic::Minimum => Statistic::Minimum,
        HistoryStatistic::Maximum => Statistic::Maximum,
        HistoryStatistic::Mean => Statistic::Mean,
        HistoryStatistic::Contribution => Statistic::Contribution,
    };
    let resolution = if stat == Statistic::Observation {
        Resolution::Observations
    } else {
        Resolution::Hourly
    };
    let period = home_wire_contract::home::api::v2::HistoryPeriod {
        start: parse(start)?.into(),
        end: parse(end)?.into(),
        ..Default::default()
    };
    Ok((
        Query::new(entity.ok_or(())?, period, resolution).map_err(|_| ())?,
        stat,
    ))
}

#[cfg(test)]
mod tests;
