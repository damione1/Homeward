//! A session-owned calendar reader/editor. Only reads reconnect; prepared writes
//! are consumed once, and every asynchronous result is fenced by view context.
mod deadlines;
mod read;
mod render;
mod state;
#[cfg(test)]
mod tests;
#[cfg(test)]
mod tls_probe;
use crate::{
    AmbiguousTimeChoice, CalendarCommand, HomewardWindow, NavigationStep, ProAgendaAction,
    ProAgendaBoundary, ProAgendaField, ProAgendaMode, ProAgendaTemporalKind,
    calendar::{Boundary, Draft, EditPhase, Field},
};
use home_client_actions::calendars::{CalendarWriteOutcome, PreparedCalendarMutation};
use home_client_cache::{ProjectionGeneration, ProjectionMailbox, Publication};
use home_client_model::{
    api_calendars::{CalendarEventName, CalendarName},
    calendars::CivilDate,
};
use home_client_session::{Generation, Phase, SessionCoordinator};
use home_client_transport::CalendarTransport;
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};
use slint::ComponentHandle;
use state::{Activity, Job, Open, State, Status};
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, Ordering},
};
use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;

#[derive(Clone)]
pub(crate) struct Calendars(Arc<Inner>);
struct Inner {
    state: Mutex<State>,
    changed: Notify,
    mailbox: ProjectionMailbox<render::Projection>,
    published: Mutex<Option<(ProjectionGeneration, render::Projection)>>,
    session: Mutex<Option<SessionCoordinator>>,
    window: Option<slint::Weak<HomewardWindow>>,
    retry: AtomicBool,
    widget: Option<crate::dashboard_widget_host::AgendaTarget>,
}
fn boundary(value: ProAgendaBoundary) -> Boundary {
    match value {
        ProAgendaBoundary::Start => Boundary::Start,
        ProAgendaBoundary::End => Boundary::End,
    }
}
impl Calendars {
    pub(crate) fn display_profile_publisher(
        &self,
    ) -> Arc<dyn Fn(Generation, crate::environment_view::ProfileZone) + Send + Sync> {
        let weak = Arc::downgrade(&self.0);
        Arc::new(move |source, zone| {
            if let Some(inner) = weak.upgrade() {
                Calendars(inner).display_profile(source, zone);
            }
        })
    }
    fn display_profile(&self, source: Generation, profile: crate::environment_view::ProfileZone) {
        if self.0.widget.is_some() {
            return;
        }
        let mut state = self.0.state.lock().expect("calendar state");
        if self
            .0
            .session
            .lock()
            .expect("calendar session")
            .as_ref()
            .is_some_and(|session| !session.is_current(source))
        {
            return;
        }
        if !state.apply_profile(source, profile) {
            return;
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        Self::with_widget(window, None)
    }
    fn with_widget(
        window: Option<slint::Weak<HomewardWindow>>,
        widget: Option<crate::dashboard_widget_host::AgendaTarget>,
    ) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            changed: Notify::new(),
            mailbox: ProjectionMailbox::new(),
            published: Mutex::new(None),
            session: Mutex::new(None),
            window,
            retry: AtomicBool::new(false),
            widget,
        }))
    }
    #[cfg(test)]
    pub fn detached() -> Self {
        Self::new(None)
    }
    pub(crate) fn for_widget(
        window: Option<slint::Weak<HomewardWindow>>,
        target: crate::dashboard_widget_host::AgendaTarget,
    ) -> Self {
        let owner = Self::with_widget(window, Some(target));
        {
            let mut state = owner.0.state.lock().expect("calendar state");
            owner.pin_widget(&mut state);
        }
        owner
    }
    fn pin_widget(&self, state: &mut State) {
        if let Some(target) = &self.0.widget {
            state.interactive = true;

            state.zone = target.zone.name().into();
            state.selected = Some(target.calendar.native_id().clone());
            if state.query_override.is_none() {
                state.query_override = state
                    .selected_name()
                    .as_ref()
                    .and_then(|name| state::widget_query(target, name, chrono::Utc::now()));
            }
        }
    }
    pub(crate) fn widget_can_select(&self, token: i32, generation: i32) -> bool {
        let state = self.0.state.lock().expect("calendar state");
        let session = self.0.session.lock().expect("calendar session");
        let current = state.source.is_some_and(|generation| {
            session
                .as_ref()
                .is_some_and(|session| session.is_current(generation))
        });
        current
            && state.active
            && state.ready
            && state.interactive
            && state.status == Status::Ready
            && self.0.widget.as_ref().is_some_and(|target| {
                state.fresh_at(std::time::SystemTime::now())
                    && generation == state.generation
                    && state
                        .row_indices()
                        .into_iter()
                        .take(target.limit)
                        .any(|index| {
                            i32::try_from(index)
                                .ok()
                                .and_then(|index| index.checked_add(1))
                                == Some(token)
                        })
            })
    }
    pub fn bind(window: &HomewardWindow) -> Self {
        let this = Self::new(Some(window.as_weak()));
        let owner = this.clone();
        window.on_calendars_interactive(move |active| owner.interactive(active));
        let owner = this.clone();
        window.on_calendars_choose(move |token, generation| owner.choose(token, generation));
        let owner = this.clone();
        window.on_calendars_select(move |token, generation| owner.select(token, generation));
        let owner = this.clone();
        window.on_calendars_command(move |generation, command, a, b, c| {
            owner.command(generation, command, &a, &b, &c)
        });
        let owner = this.clone();
        window.on_calendars_edit(move |token, generation, field, value| {
            let limit=match field{ProAgendaField::Summary=>admit::MAX_EVENT_SUMMARY_BYTES,ProAgendaField::Description=>admit::MAX_EVENT_DESCRIPTION_BYTES,ProAgendaField::Location=>admit::MAX_EVENT_LOCATION_BYTES};
            if value.len()>limit||value.contains('\0'){owner.reject_edit(token,generation,"Text too long or contains a forbidden character; the previous draft is retained.");return;}
            owner.edit(token, generation, |draft| {
                draft.edit(
                    match field {
                        ProAgendaField::Summary => Field::Summary,
                        ProAgendaField::Description => Field::Description,
                        ProAgendaField::Location => Field::Location,
                    },
                    value.into(),
                )
            })
        });
        let owner = this.clone();
        window.on_calendars_date(move |t, g, b, v| {
            if v.len() > 10 {
                owner.reject_edit(t, g, "Enter a calendar date in YYYY-MM-DD format.");
                return;
            }
            owner.edit(t, g, |draft| draft.date(boundary(b), v.into()))
        });
        let owner = this.clone();
        window.on_calendars_time(move |t, g, b, v| {
            if v.date.len() > 10 || v.time.len() > 8 || v.zone.len() > admit::MAX_ZONE_BYTES {
                owner.reject_edit(
                    t,
                    g,
                    "Date, time or time zone outside the allowed range; the previous draft is retained.",
                );
                return;
            }
            owner.edit(t, g, |draft| {
                draft.timed(boundary(b), v.date.into(), v.time.into(), v.zone.into())
            })
        });
        let owner = this.clone();
        window.on_calendars_resolve(move |t, g, b, v| {
            let mut state = owner.0.state.lock().expect("calendar state");
            if !state.can_edit_draft(t, g) {
                return;
            }
            let draft = state.draft.as_mut().expect("admitted draft");
            let b = boundary(b);
            draft.timed(b, v.date.into(), v.time.into(), v.zone.into());
            if let Some(request) = draft.resolution_request(b) {
                state.enqueue(Job::Resolve(b, request));
            } else {
                state.notice =
                    "Date, time or time zone incomplete; no resolution request was sent.".into();
            }
            owner.publish(&state);
            owner.0.changed.notify_one();
        });
        let owner = this.clone();
        window.on_calendars_ambiguity(move |t, g, b, c| {
            owner.edit(t, g, |draft| {
                draft.choose(
                    boundary(b),
                    match c {
                        AmbiguousTimeChoice::Earlier => false,
                        AmbiguousTimeChoice::Later => true,
                    },
                )
            })
        });
        let owner = this.clone();
        window.on_calendars_kind(move |t, g, k| {
            let zone = owner.0.state.lock().expect("calendar state").zone.clone();
            owner.edit(t, g, |draft| {
                draft.switch(
                    match k {
                        ProAgendaTemporalKind::AllDay => false,
                        ProAgendaTemporalKind::Timed => true,
                    },
                    &zone,
                )
            });
        });
        let owner = this.clone();
        window.on_calendars_action(move |t, g, a| owner.action(t, g, a));
        let owner = this.clone();
        window.on_calendars_mode(move |mode| owner.mode(mode));
        let owner = this.clone();
        window.on_calendars_navigate(move |step| owner.navigate(step));
        let owner = this.clone();
        window.on_calendars_day(move |token, generation| {
            let mut state = owner.0.state.lock().expect("calendar state");
            if generation != state.generation
                || !state.active
                || !state.interactive
                || state.busy()
                || !(100..107).contains(&token)
            {
                return;
            }
            let date = chrono::NaiveDate::parse_from_str(&state.start, "%Y-%m-%d")
                .ok()
                .and_then(|date| date.checked_add_days(chrono::Days::new((token - 100) as u64)));
            if let Some(date) = date {
                state.focus_period(date, ProAgendaMode::Day);
                owner.publish(&state);
                owner.0.changed.notify_one();
            }
        });
        let owner = this.clone();
        window.on_calendars_date_chosen(move |day| {
            if !day.enabled {
                return;
            }
            if let Some(date) = u32::try_from(day.month).ok().and_then(|month| {
                u32::try_from(day.day).ok().and_then(|day_of_month| {
                    chrono::NaiveDate::from_ymd_opt(day.year, month, day_of_month)
                })
            }) {
                let (generation, zone) = {
                    let state = owner.0.state.lock().expect("calendar state");
                    (state.generation, state.zone.clone())
                };
                owner.command(
                    generation,
                    CalendarCommand::ApplyRange,
                    &date.to_string(),
                    "",
                    &zone,
                );
            }
        });
        let owner = this.clone();
        window.on_calendars_month(move |direction| {
            use crate::CalendarNavigation;
            use chrono::{Datelike, Days, Months};
            let (date, generation, zone) = {
                let state = owner.0.state.lock().expect("calendar state");
                (
                    chrono::NaiveDate::parse_from_str(&state.start, "%Y-%m-%d").ok(),
                    state.generation,
                    state.zone.clone(),
                )
            };
            if let Some(date) = date.and_then(|date| match direction {
                CalendarNavigation::PreviousDay => date.checked_sub_days(Days::new(1)),
                CalendarNavigation::NextDay => date.checked_add_days(Days::new(1)),
                CalendarNavigation::PreviousWeek => date.checked_sub_days(Days::new(7)),
                CalendarNavigation::NextWeek => date.checked_add_days(Days::new(7)),
                CalendarNavigation::WeekStart => date
                    .checked_sub_days(Days::new(u64::from(date.weekday().num_days_from_monday()))),
                CalendarNavigation::WeekEnd => date.checked_add_days(Days::new(u64::from(
                    6 - date.weekday().num_days_from_monday(),
                ))),
                CalendarNavigation::PreviousMonth => date.checked_sub_months(Months::new(1)),
                CalendarNavigation::NextMonth => date.checked_add_months(Months::new(1)),
                CalendarNavigation::PreviousYear => date.checked_sub_months(Months::new(12)),
                CalendarNavigation::NextYear => date.checked_add_months(Months::new(12)),
            }) {
                owner.command(
                    generation,
                    CalendarCommand::ApplyRange,
                    &date.to_string(),
                    "",
                    &zone,
                );
            }
        });
        this
    }
    fn publish(&self, state: &State) {
        let session = self.0.session.lock().expect("calendar session").clone();
        let generation = self.0.mailbox.generation();
        let mut published = self.0.published.lock().expect("calendar publication");
        if published
            .as_ref()
            .is_some_and(|(old_generation, previous)| {
                *old_generation == generation && previous.matches(state, session.as_ref())
            })
        {
            return;
        }
        let projection = render::Projection::new(state, session);
        *published = Some((generation, projection.clone()));
        if let Publication::Schedule(ticket) = self.0.mailbox.publish(generation, projection)
            && let Some(window) = &self.0.window
        {
            let target = self.0.widget.clone();
            let _ = window.upgrade_in_event_loop(move |window| {
                ticket.deliver(|_, value| match target {
                    Some(target) => render::render_widget(&window, value, &target),
                    None => render::render(&window, value),
                });
            });
        }
    }
    pub fn clear(&self) {
        self.0.retry.store(false, Ordering::SeqCst);
        let mut state = self.0.state.lock().expect("calendar state");
        state.clear_private();
        state.active = false;
        let _ = self.0.mailbox.invalidate();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    pub fn foreground(&self, active: bool) {
        let mut state = self.0.state.lock().expect("calendar state");
        state.foreground = active;
        if !active {
            state.clear_private();
            let _ = self.0.mailbox.invalidate();
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn interactive(&self, active: bool) {
        let mut state = self.0.state.lock().expect("calendar state");
        if state.interactive == active {
            return;
        }
        state.interactive = active;
        if active {
            state.catalog_needed = true;
            state.reset_reads();
        }
        if !active {
            state.uncertain |= state.activity == Activity::Write && state.job.is_none();
            state.reset_reads();
            state.job = None;
            state.activity = Activity::Idle;
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn choose(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("calendar state");
        if !state.active || !state.interactive || generation != state.generation || state.busy() {
            return;
        }
        let Some(id) = token
            .checked_sub(1)
            .and_then(|index| usize::try_from(index).ok())
            .and_then(|index| state.catalog.get(index))
            .and_then(|row| {
                CalendarName::try_from(row.calendar.as_str())
                    .ok()
                    .map(|name| name.native_id().clone())
            })
        else {
            return;
        };
        state.selected = Some(id);
        state.snapshot = None;
        state.draft = None;
        state.occurrence = None;
        state.active_index = None;
        state.reset_reads();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn select(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("calendar state");
        if generation != state.generation || !state.ready || !state.interactive || state.busy() {
            return;
        }
        let Some((index, key, occurrence)) = token
            .checked_sub(1)
            .and_then(|index| usize::try_from(index).ok())
            .and_then(|index| {
                state
                    .snapshot
                    .as_ref()?
                    .occurrences()
                    .get(index)
                    .and_then(|row| {
                        CalendarEventName::try_from(row.calendar_event.as_str())
                            .ok()
                            .map(|key| (index, key, row.clone()))
                    })
            })
        else {
            return;
        };
        state.active_index = Some(index);
        state.draft = None;
        state.occurrence = None;
        state.draft_token = token;
        if state.editable() {
            state.enqueue(Job::Open(key, Open::Replace));
        } else {
            state.occurrence = Some(occurrence);
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn navigate(&self, step: NavigationStep) {
        let selection = {
            let state = self.0.state.lock().expect("calendar state");
            let rows = state.row_indices();
            if rows.is_empty() {
                return;
            }
            let old = state
                .active_index
                .and_then(|v| rows.iter().position(|index| *index == v))
                .unwrap_or(0);
            let next = match step {
                NavigationStep::Next => (old + 1).min(rows.len() - 1),
                NavigationStep::Previous => old.saturating_sub(1),
                NavigationStep::First => 0,
                NavigationStep::Last => rows.len() - 1,
                NavigationStep::PageNext => (old + 5).min(rows.len() - 1),
                NavigationStep::PagePrevious => old.saturating_sub(5),
            };
            (rows[next] as i32 + 1, state.generation)
        };
        self.select(selection.0, selection.1);
    }
    fn mode(&self, mode: ProAgendaMode) {
        let mut state = self.0.state.lock().expect("calendar state");
        if !state.active || !state.interactive || state.busy() {
            return;
        }
        if mode == ProAgendaMode::List {
            state.mode = mode;
            state.bump();
        } else {
            let today = state
                .zone
                .parse::<home_client_model::local_time::DisplayTimeZone>()
                .ok()
                .map(|zone| zone.local(crate::environment_view::now()).date());
            let anchor = if mode == ProAgendaMode::Day {
                today
                    .filter(|day| *day >= state.start && *day < state.end)
                    .unwrap_or_else(|| state.start.clone())
            } else {
                state.start.clone()
            };
            let Some(date) = chrono::NaiveDate::parse_from_str(&anchor, "%Y-%m-%d").ok() else {
                return;
            };
            state.focus_period(date, mode);
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn edit(&self, t: i32, g: i32, change: impl FnOnce(&mut Draft)) {
        let mut state = self.0.state.lock().expect("calendar state");
        if state.can_edit_draft(t, g) {
            change(state.draft.as_mut().expect("admitted draft"));
            self.publish(&state);
        }
    }
    fn reject_edit(&self, t: i32, g: i32, message: &str) {
        let mut state = self.0.state.lock().expect("calendar state");
        if state.can_edit_draft(t, g) {
            state.notice = message.into();
            self.publish(&state);
        }
    }
    fn command(&self, g: i32, command: CalendarCommand, a: &str, b: &str, c: &str) {
        let mut state = self.0.state.lock().expect("calendar state");
        if g != state.generation || !state.interactive || state.busy() {
            return;
        }
        match command {
            CalendarCommand::PreviousWeek | CalendarCommand::Today | CalendarCommand::NextWeek => {
                let Some(start) = chrono::NaiveDate::parse_from_str(&state.start, "%Y-%m-%d").ok()
                else {
                    return;
                };
                let step = state.period_days();
                let next = match command {
                    CalendarCommand::PreviousWeek => {
                        start.checked_sub_days(chrono::Days::new(step))
                    }
                    CalendarCommand::NextWeek => start.checked_add_days(chrono::Days::new(step)),
                    CalendarCommand::Today => state
                        .zone
                        .parse::<home_client_model::local_time::DisplayTimeZone>()
                        .ok()
                        .and_then(|zone| {
                            chrono::NaiveDate::parse_from_str(
                                &zone.local(crate::environment_view::now()).date(),
                                "%Y-%m-%d",
                            )
                            .ok()
                        }),
                    CalendarCommand::Create
                    | CalendarCommand::Rename
                    | CalendarCommand::Remove
                    | CalendarCommand::Reload
                    | CalendarCommand::NewEvent
                    | CalendarCommand::ApplyRange => unreachable!("navigation branch"),
                };
                if let Some(next) = next {
                    let mode = state.mode;
                    state.focus_period(next, mode);
                }
            }

            CalendarCommand::Reload => {
                self.0.retry.store(true, Ordering::SeqCst);
                state.catalog_needed = true;
                state.uncertain = false;
                state.status = Status::Loading;
                state.reset_reads();
                state.notice.clear();
            }
            CalendarCommand::ApplyRange => {
                let start = chrono::NaiveDate::parse_from_str(a, "%Y-%m-%d").ok();
                let zone = home_client_model::api_calendars::CalendarZone::try_from(c);
                let end = if b.is_empty() {
                    start.and_then(|date| {
                        date.checked_add_days(chrono::Days::new(state.period_days()))
                    })
                } else {
                    chrono::NaiveDate::parse_from_str(b, "%Y-%m-%d").ok()
                };
                if let Some((start, end)) = start
                    .zip(end)
                    .filter(|(start, end)| *end > *start && (*end - *start).num_days() <= 366)
                    .filter(|_| zone.is_ok())
                {
                    state.zone = c.into();
                    if b.is_empty() {
                        let mode = state.mode;
                        state.focus_period(start, mode);
                    } else {
                        state.mode = ProAgendaMode::List;
                        state.start = start.to_string();
                        state.end = end.to_string();
                        state.snapshot = None;
                        state.draft = None;
                        state.occurrence = None;
                        state.active_index = None;
                        state.reset_reads();
                    }
                    state.notice.clear();
                } else {
                    state.notice = "Invalid range (maximum 366 days) or missing time zone.".into();
                }
            }

            CalendarCommand::NewEvent => {
                if !state.editable() || !state.present() {
                    return;
                }
                let Some(parent) = state.selected_name() else {
                    return;
                };
                let Ok(date) = state.start.parse::<CivilDate>() else {
                    return;
                };
                state.bump();
                state.draft_token = 0;
                state.occurrence = None;
                state.draft = Some(Draft::new(parent, &date));
            }
            CalendarCommand::Create | CalendarCommand::Rename | CalendarCommand::Remove => {
                if !state.editable() {
                    return;
                }
                let prepared = match command {
                    CalendarCommand::Create => state.selected_source().and_then(|source| {
                        let name = CalendarName::try_from(source.calendar.as_str()).ok()?;
                        let source = admit::admit_source(source, name.home()).ok()?;
                        PreparedCalendarMutation::create(&source, a.into()).ok()
                    }),
                    CalendarCommand::Rename => state.current().and_then(|current| {
                        PreparedCalendarMutation::rename(&current, a.into()).ok()
                    }),
                    CalendarCommand::Remove => state
                        .current()
                        .and_then(|current| PreparedCalendarMutation::delete(&current, false).ok()),
                    CalendarCommand::Reload
                    | CalendarCommand::NewEvent
                    | CalendarCommand::ApplyRange
                    | CalendarCommand::PreviousWeek
                    | CalendarCommand::Today
                    | CalendarCommand::NextWeek => unreachable!("calendar mutation branch"),
                };
                if let Some(prepared) = prepared {
                    state.enqueue(Job::Write(Box::new(prepared)));
                } else {
                    state.notice = "Invalid calendar change or read-only source.".into();
                }
            }
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn action(&self, t: i32, g: i32, action: ProAgendaAction) {
        let mut state = self.0.state.lock().expect("calendar state");
        if g != state.generation || t != state.draft_token || !state.interactive || state.busy() {
            return;
        }
        match action {
            ProAgendaAction::Cancel => {
                state.draft = None;
                state.occurrence = None;
                state.bump();
            }
            ProAgendaAction::Reload | ProAgendaAction::Rebase => {
                if state.occurrence.is_some() && action == ProAgendaAction::Reload {
                    state.status = Status::Loading;
                    state.reset_reads();
                }
                if let Some(key) = state.draft.as_ref().and_then(|draft| draft.key.clone()) {
                    state.enqueue(Job::Open(
                        key,
                        match action {
                            ProAgendaAction::Reload => Open::Replace,
                            ProAgendaAction::Rebase => Open::Rebase,
                            ProAgendaAction::Save
                            | ProAgendaAction::Remove
                            | ProAgendaAction::Cancel => unreachable!("read draft branch"),
                        },
                    ));
                }
            }
            ProAgendaAction::Save | ProAgendaAction::Remove => {
                if !state.can_edit_draft(t, g) {
                    return;
                }
                let Some(draft) = &state.draft else {
                    return;
                };
                if draft.phase != EditPhase::Ready {
                    return;
                }
                let Some(current) = state.current() else {
                    return;
                };
                let prepared = match action {
                    ProAgendaAction::Remove => draft.revision.as_ref().and_then(|version| {
                        PreparedCalendarMutation::delete_event(&current, version).ok()
                    }),
                    ProAgendaAction::Save => {
                        draft.event().and_then(|value| match &draft.revision {
                            Some(version) => {
                                PreparedCalendarMutation::update_event(&current, version, value)
                                    .ok()
                            }
                            None => PreparedCalendarMutation::create_event(&current, value).ok(),
                        })
                    }
                    ProAgendaAction::Reload | ProAgendaAction::Rebase | ProAgendaAction::Cancel => {
                        unreachable!("write draft branch")
                    }
                };
                match prepared {
                    Some(prepared) => {
                        state.draft.as_mut().expect("draft").phase = EditPhase::Pending;
                        state.enqueue(Job::Write(Box::new(prepared)));
                    }
                    None => {
                        state.notice = "Invalid event or read-only source; nothing was sent.".into()
                    }
                }
            }
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
}
