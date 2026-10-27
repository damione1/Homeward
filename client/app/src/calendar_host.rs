//! A session-owned calendar reader/editor. Only reads reconnect; prepared writes
//! are consumed once, and every asynchronous result is fenced by view context.
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
use home_client_cache::{ProjectionMailbox, Publication};
use home_client_model::calendars::{CalendarId, CivilDate, EventKey, EventRevision, RevisionState};
use home_client_session::{Phase, SessionCoordinator};
use home_client_transport::CalendarTransport;
use home_wire_contract::{calendars_contract as admit, home::calendars::v1 as wire};
use slint::ComponentHandle;
use state::{Job, Open, State, Status};
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
    session: Mutex<Option<SessionCoordinator>>,
    window: Option<slint::Weak<HomewardWindow>>,
    retry: AtomicBool,
}
fn boundary(value: ProAgendaBoundary) -> Boundary {
    match value {
        ProAgendaBoundary::Start => Boundary::Start,
        ProAgendaBoundary::End => Boundary::End,
    }
}
impl Calendars {
    fn new(window: Option<slint::Weak<HomewardWindow>>) -> Self {
        Self(Arc::new(Inner {
            state: Mutex::new(State::default()),
            changed: Notify::new(),
            mailbox: ProjectionMailbox::new(),
            session: Mutex::new(None),
            window,
            retry: AtomicBool::new(false),
        }))
    }
    #[cfg(test)]
    pub fn detached() -> Self {
        Self::new(None)
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
            if value.len()>limit||value.contains('\0'){owner.reject_edit(token,generation,"Texte trop long ou caractère non autorisé ; le brouillon précédent est conservé.");return;}
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
                owner.reject_edit(t, g, "Saisissez une date civile au format AAAA-MM-JJ.");
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
                    "Date, heure ou fuseau hors limites ; le brouillon précédent est conservé.",
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
                    "Date, heure ou fuseau incomplet ; aucune requête de résolution envoyée."
                        .into();
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
                || !state.ready
                || state.busy
                || !(100..107).contains(&token)
            {
                return;
            }
            let date = chrono::NaiveDate::parse_from_str(&state.start, "%Y-%m-%d")
                .ok()
                .and_then(|date| date.checked_add_days(chrono::Days::new((token - 100) as u64)));
            if let Some(date) = date.and_then(|date| date.to_string().parse().ok()) {
                state.day = Some(date);
                state.active_index = None;
                state.draft = None;
                state.bump();
                owner.publish(&state);
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
        let projection = render::Projection::new(state, session);
        if let Publication::Schedule(ticket) = self
            .0
            .mailbox
            .publish(self.0.mailbox.generation(), projection)
            && let Some(window) = &self.0.window
        {
            let _ = window.upgrade_in_event_loop(move |window| {
                ticket.deliver(|_, value| render::render(&window, value));
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
        if !active {
            state.reset_reads();
            state.job = None;
            state.busy = false;
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn choose(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("calendar state");
        if !state.active || !state.interactive || generation != state.generation || state.busy {
            return;
        }
        let Some(id) = token
            .checked_sub(1)
            .and_then(|index| usize::try_from(index).ok())
            .and_then(|index| state.catalog.get(index))
            .and_then(|row| admit::calendar_id(&row.id))
        else {
            return;
        };
        state.selected = Some(id);
        state.snapshot = None;
        state.draft = None;
        state.active_index = None;
        state.reset_reads();
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn select(&self, token: i32, generation: i32) {
        let mut state = self.0.state.lock().expect("calendar state");
        if generation != state.generation || !state.ready || !state.interactive || state.busy {
            return;
        }
        let Some((index, key)) = token
            .checked_sub(1)
            .and_then(|index| usize::try_from(index).ok())
            .and_then(|index| {
                state
                    .snapshot
                    .as_ref()?
                    .occurrences()
                    .get(index)
                    .and_then(|row| admit::event_key(&row.key))
                    .map(|key| (index, key))
            })
        else {
            return;
        };
        state.active_index = Some(index);
        state.draft = None;
        state.draft_token = token;
        state.enqueue(Job::Open(key, Open::Replace));
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
        if !state.active || !state.interactive || !state.ready || state.busy {
            return;
        }
        state.week = match mode {
            ProAgendaMode::List => false,
            ProAgendaMode::Week => true,
        };
        if state.week {
            let Some(end) = chrono::NaiveDate::parse_from_str(&state.start, "%Y-%m-%d")
                .ok()
                .and_then(|date| date.checked_add_days(chrono::Days::new(7)))
            else {
                return;
            };
            if state.end != end.to_string() {
                state.end = end.to_string();
                state.snapshot = None;
                state.status = Status::Loading;
                state.reset_reads();
            }
        }
        state.day = None;
        state.active_index = None;
        state.draft = None;
        state.bump();
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
        if g != state.generation || !state.interactive || state.busy {
            return;
        }
        match command {
            CalendarCommand::Reload => {
                self.0.retry.store(true, Ordering::SeqCst);
                state.catalog_needed = true;
                state.uncertain = false;
                state.status = Status::Loading;
                state.reset_reads();
                state.notice.clear();
            }
            CalendarCommand::ApplyRange => {
                let parsed = a.parse::<CivilDate>();
                let end = if b.is_empty() {
                    chrono::NaiveDate::parse_from_str(a, "%Y-%m-%d")
                        .ok()
                        .and_then(|date| date.checked_add_days(chrono::Days::new(7)))
                        .map(|date| date.to_string())
                } else {
                    Some(b.to_owned())
                };
                if parsed.is_err()
                    || end
                        .as_deref()
                        .is_none_or(|end| end.parse::<CivilDate>().is_err())
                {
                    state.notice = "Dates civiles invalides ; aucune lecture envoyée.".into();
                } else {
                    let (old_start, old_end, old_zone) =
                        (state.start.clone(), state.end.clone(), state.zone.clone());
                    state.start = a.into();
                    state.end = end.expect("checked civil end");
                    state.zone = c.into();
                    if state.query().is_some() {
                        if chrono::NaiveDate::parse_from_str(&state.start, "%Y-%m-%d")
                            .ok()
                            .and_then(|date| date.checked_add_days(chrono::Days::new(7)))
                            .is_none_or(|end| end.to_string() != state.end)
                        {
                            state.week = false;
                        }
                        state.status = Status::Loading;
                        state.draft = None;
                        state.snapshot = None;
                        state.day = None;
                        state.reset_reads();
                    } else {
                        state.start = old_start;
                        state.end = old_end;
                        state.zone = old_zone;
                        state.notice =
                            "Plage invalide (366 jours maximum) ou fuseau manquant.".into();
                    }
                }
            }
            CalendarCommand::NewEvent => {
                if !state.editable() || !state.present() {
                    return;
                }
                let Some(id) = state.selected.clone() else {
                    return;
                };
                let Ok(date) = state.start.parse::<CivilDate>() else {
                    return;
                };
                use sha2::{Digest, Sha256};
                let uid = format!(
                    "frontend-{:x}",
                    Sha256::digest(format!(
                        "{:?}-{}-{}",
                        std::time::SystemTime::now(),
                        state.context,
                        state.generation
                    ))
                );
                let key = EventKey::new(id, uid.parse().expect("bounded generated UID"));
                state.bump();
                state.draft_token = 0;
                state.draft = Some(Draft::new(key, &date));
            }
            CalendarCommand::Create | CalendarCommand::Rename | CalendarCommand::Remove => {
                if !state.editable() {
                    return;
                }
                let Some(current) = state.current() else {
                    return;
                };
                let mutation = match command {
                    CalendarCommand::Create => wire::Mutation::Create,
                    CalendarCommand::Rename => wire::Mutation::Update,
                    CalendarCommand::Remove => wire::Mutation::Delete,
                    CalendarCommand::Reload
                    | CalendarCommand::NewEvent
                    | CalendarCommand::ApplyRange => unreachable!("calendar mutation branch"),
                };
                let request = wire::MutateCalendarRequest {
                    id: current.wire().id.clone(),
                    expected_revision: current.wire().revision.clone(),
                    mutation: mutation.into(),
                    name: a.into(),
                    ..Default::default()
                };
                match PreparedCalendarMutation::calendar(&current, request) {
                    Ok(prepared) => {
                        state.enqueue(Job::Write(prepared));
                    }
                    Err(_) => {
                        state.notice =
                            "Modification de calendrier invalide ou source en lecture seule.".into()
                    }
                }
            }
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
    fn action(&self, t: i32, g: i32, action: ProAgendaAction) {
        let mut state = self.0.state.lock().expect("calendar state");
        if g != state.generation || t != state.draft_token || !state.interactive || state.busy {
            return;
        }
        match action {
            ProAgendaAction::Cancel => {
                state.draft = None;
                state.bump();
            }
            ProAgendaAction::Reload | ProAgendaAction::Rebase => {
                if let Some(key) = state.draft.as_ref().map(|draft| draft.key.clone()) {
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
                let mutation = match action {
                    ProAgendaAction::Remove => wire::Mutation::Delete,
                    ProAgendaAction::Save => {
                        if draft.existing {
                            wire::Mutation::Update
                        } else {
                            wire::Mutation::Create
                        }
                    }
                    ProAgendaAction::Reload | ProAgendaAction::Rebase | ProAgendaAction::Cancel => {
                        unreachable!("write draft branch")
                    }
                };
                let value = if mutation == wire::Mutation::Delete {
                    Default::default()
                } else {
                    let Some(value) = draft.event() else {
                        return;
                    };
                    value.into()
                };
                let request = wire::MutateEventRequest {
                    key: event_key(&draft.key).into(),
                    mutation: mutation.into(),
                    expected_revision: event_revision(&draft.revision).into(),
                    value,
                    ..Default::default()
                };
                match PreparedCalendarMutation::event(&current, &draft.revision, request) {
                    Ok(prepared) => {
                        state.draft.as_mut().expect("draft").phase = EditPhase::Pending;
                        state.enqueue(Job::Write(prepared));
                    }
                    Err(_) => {
                        state.notice =
                            "Événement invalide ou source en lecture seule ; rien n’a été envoyé."
                                .into()
                    }
                }
            }
        }
        self.publish(&state);
        self.0.changed.notify_one();
    }
}
fn event_key(key: &EventKey) -> wire::EventKey {
    wire::EventKey {
        calendar: wire::CalendarId {
            entity_id: key.calendar().entity().as_str().into(),
            ..Default::default()
        }
        .into(),
        uid: key.uid().as_str().into(),
        ..Default::default()
    }
}
fn event_revision(revision: &EventRevision) -> wire::Revision {
    let (presence, number) = match revision.state() {
        RevisionState::Absent => (wire::Presence::Absent, 0),
        RevisionState::Present(n) => (wire::Presence::Present, n.get()),
        RevisionState::Removed(n) => (wire::Presence::Removed, n.get()),
    };
    wire::Revision {
        resource: event_key(revision.key()).into(),
        presence: presence.into(),
        number,
        ..Default::default()
    }
}
