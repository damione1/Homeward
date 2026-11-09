use crate::calendar::{Draft, EditPhase};
use home_client_actions::calendars::PreparedCalendarMutation;
use home_client_cache::calendars::CalendarReadSnapshot;
use home_client_model::{
    api_calendars::{CalendarEventName, CalendarName},
    calendars::CalendarId,
};
use home_client_session::Generation;
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};
use std::{sync::Arc, time::SystemTime};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Status {
    Loading,
    Ready,
    Offline,
    Denied,
    Unsupported,
    Invalid,
    TooLarge,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Open {
    Replace,
    Rebase,
}
pub(super) enum Job {
    Open(CalendarEventName, Open),
    Resolve(crate::calendar::Boundary, wire::ResolveCalendarTimeRequest),
    Write(Box<PreparedCalendarMutation>),
}
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(super) enum Activity {
    #[default]
    Idle,
    Read,
    Write,
}
pub(super) struct State {
    pub generation: i32,
    pub privacy_generation: i32,
    pub context: u64,
    pub foreground: bool,
    pub interactive: bool,
    pub active: bool,
    pub status: Status,
    pub source: Option<Generation>,
    pub catalog: Vec<wire::CalendarSource>,
    pub catalog_needed: bool,
    pub selected: Option<CalendarId>,
    pub snapshot: Option<Arc<CalendarReadSnapshot>>,
    pub ready: bool,
    pub start: String,
    pub end: String,
    pub zone: String,
    pub profile_received: bool,
    pub server_profile: Option<(Generation, crate::environment_view::ProfileZone)>,
    pub mode: crate::ProAgendaMode,
    pub active_index: Option<usize>,
    pub draft: Option<Draft>,
    pub occurrence: Option<wire::Occurrence>,
    pub draft_token: i32,
    pub notice: String,
    pub job: Option<Job>,
    pub activity: Activity,
    pub uncertain: bool,
    pub query_override: Option<wire::QueryOccurrencesRequest>,
}
impl Default for State {
    fn default() -> Self {
        Self {
            generation: 1,
            privacy_generation: 1,
            context: 1,
            foreground: true,
            interactive: false,
            active: false,
            status: Status::Loading,
            source: None,
            catalog: Vec::new(),
            catalog_needed: true,
            selected: None,
            snapshot: None,
            ready: false,
            start: String::new(),
            end: String::new(),
            zone: String::new(),
            profile_received: false,
            server_profile: None,
            mode: crate::ProAgendaMode::Week,
            active_index: None,
            draft: None,
            occurrence: None,
            draft_token: 0,
            notice: String::new(),
            job: None,
            activity: Activity::Idle,
            uncertain: false,
            query_override: None,
        }
    }
}
impl State {
    pub fn apply_profile(
        &mut self,
        source: Generation,
        profile: crate::environment_view::ProfileZone,
    ) -> bool {
        self.server_profile = Some((source, profile));
        let zone = match profile {
            crate::environment_view::ProfileZone::Known(zone) => Some(zone),
            crate::environment_view::ProfileZone::Missing
            | crate::environment_view::ProfileZone::Unsupported => None,
        };
        let name = zone.map_or("", |zone| zone.name());
        if self.profile_received && self.zone == name {
            return false;
        }
        let interrupted_write = self.activity == Activity::Write && self.job.is_none();
        self.uncertain |= interrupted_write;
        self.job = None;
        self.activity = Activity::Idle;
        self.query_override = None;
        self.profile_received = true;
        self.zone = name.into();
        self.snapshot = None;
        self.draft = None;
        self.occurrence = None;
        self.active_index = None;
        if let Some(today) = zone.and_then(|zone| {
            chrono::NaiveDate::parse_from_str(
                &zone.local(crate::environment_view::now()).date(),
                "%Y-%m-%d",
            )
            .ok()
        }) {
            let mode = self.mode;
            self.focus_period(today, mode);
            if !self.uncertain {
                self.notice.clear();
            }
            if interrupted_write {
                self.notice =
                    "Calendar operation interrupted · refresh explicitly before another change."
                        .into();
            }
        } else {
            self.start.clear();
            self.end.clear();
            self.notice = "Calendar time zone is not configured by the server.".into();
        }
        self.reset_reads();
        true
    }
    pub fn change_generation(
        &mut self,
        source: Generation,
        session: &home_client_session::SessionCoordinator,
    ) {
        let current = self
            .server_profile
            .filter(|(generation, _)| *generation == source && session.is_current(source));
        self.clear_private();
        if let Some((generation, profile)) = current {
            self.apply_profile(generation, profile);
        }
    }
    pub fn busy(&self) -> bool {
        self.activity != Activity::Idle
    }
    pub fn bump(&mut self) {
        self.generation = if self.generation > 0 {
            self.generation.checked_add(1).unwrap_or(0)
        } else {
            0
        };
        if self.generation == 0 {
            self.active = false;
            self.ready = false;
            self.job = None;
        }
    }
    pub fn reset_reads(&mut self) {
        self.context = self.context.checked_add(1).unwrap_or(0);
        self.ready = false;
        self.bump();
    }
    pub fn clear_private(&mut self) {
        self.privacy_generation = self.privacy_generation.checked_add(1).unwrap_or(0);
        self.reset_reads();
        self.catalog.clear();
        self.zone.clear();
        self.profile_received = false;
        self.server_profile = None;
        self.notice.clear();
        self.start.clear();
        self.end.clear();
        self.snapshot = None;
        self.draft = None;
        self.occurrence = None;
        self.active_index = None;
        self.selected = None;
        self.source = None;
        self.query_override = None;
        self.job = None;
        self.activity = Activity::Idle;
        self.catalog_needed = true;
        self.uncertain = false;
    }
    pub fn selected_source(&self) -> Option<&wire::CalendarSource> {
        self.catalog.iter().find(|source| {
            CalendarName::try_from(source.calendar.as_str())
                .ok()
                .as_ref()
                .map(CalendarName::native_id)
                == self.selected.as_ref()
        })
    }
    pub fn selected_name(&self) -> Option<CalendarName> {
        CalendarName::try_from(self.selected_source()?.calendar.as_str()).ok()
    }
    pub fn calendar(&self) -> Option<&wire::Calendar> {
        self.snapshot.as_ref().map(|snapshot| snapshot.calendar())
    }
    pub fn current(&self) -> Option<admit::Admitted<wire::Calendar>> {
        admit::admit_calendar(self.calendar()?, &self.selected_name()?).ok()
    }
    pub fn editable(&self) -> bool {
        if !self.active
            || !self.interactive
            || !self.ready
            || self.uncertain
            || self.status != Status::Ready
        {
            return false;
        }
        let Some(source) = self.selected_source() else {
            return false;
        };
        if source.origin != wire::CalendarOrigin::Local
            || source.mutation_access != wire::CalendarMutationAccess::Allowed
        {
            return false;
        }
        if !self.present() {
            return !source.materialized;
        }
        self.snapshot.as_ref().is_some_and(|snapshot| {
            snapshot.freshness() == home_client_cache::calendars::CalendarFreshness::Fresh
                && snapshot.expiry_bounds().all(|until| {
                    chrono::DateTime::parse_from_rfc3339(until).is_ok_and(|until| {
                        SystemTime::from(until.with_timezone(&chrono::Utc)) > SystemTime::now()
                    })
                })
        }) && self.current().is_some_and(|calendar| {
            admit::calendar_editable(&calendar)
                && calendar.wire().freshness == wire::CalendarFreshness::Fresh
        })
    }
    pub fn present(&self) -> bool {
        self.snapshot.is_some()
    }
    pub fn query(&self) -> Option<wire::QueryOccurrencesRequest> {
        let name = self.selected_name()?;
        if let Some(query) = &self.query_override {
            return (admit::query_parent(&query.parent).ok()?.0 == name).then(|| query.clone());
        }
        let query = wire::QueryOccurrencesRequest {
            parent: format!("{}/events/-", name.as_str()),
            window: wire::CalendarWindow {
                time_zone: self.zone.clone(),
                bounds: Some(wire::calendar_window::Bounds::Dates(Box::new(
                    wire::CalendarDateSpan {
                        start_date: admit::date_wire(self.start.parse().ok()?).into(),
                        end_date: admit::date_wire(self.end.parse().ok()?).into(),
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            }
            .into(),
            page_size: 256,
            ..Default::default()
        };
        admit::admit_query(&query)
            .ok()
            .map(|value| value.wire().clone())
    }
    pub fn can_edit_draft(&self, token: i32, generation: i32) -> bool {
        generation == self.generation
            && token == self.draft_token
            && self.editable()
            && !self.busy()
            && self
                .draft
                .as_ref()
                .is_some_and(|draft| matches!(draft.phase, EditPhase::Ready | EditPhase::Conflict))
    }
    pub fn enqueue(&mut self, job: Job) -> bool {
        if self.busy() || !self.active || !self.interactive {
            return false;
        }
        self.activity = match &job {
            Job::Write(_) => Activity::Write,
            Job::Open(_, _) | Job::Resolve(_, _) => Activity::Read,
        };
        self.job = Some(job);
        true
    }
    /// A Watch publication may supersede the visual generation while this read
    /// is in flight. Drop its value, but release its own busy slot. Another
    /// resource context owns a different slot and must remain untouched.
    pub fn finish_read_job(&mut self, context: u64, generation: i32) -> bool {
        if self.context != context {
            return false;
        }
        self.activity = Activity::Idle;
        self.generation == generation
    }
    pub fn owns_write(&self, context: u64) -> bool {
        self.context == context
            && self.active
            && self.interactive
            && self.activity == Activity::Write
    }
    pub fn snapshot(&mut self, snapshot: Arc<CalendarReadSnapshot>, source: Generation) {
        let Ok(version) = admit::calendar_version(snapshot.calendar()) else {
            return;
        };
        if self.selected_name().as_ref() != Some(version.name()) {
            return;
        }
        if self
            .snapshot
            .as_ref()
            .is_some_and(|old| old.calendar().etag != snapshot.calendar().etag)
        {
            self.bump();
            if let Some(draft) = &mut self.draft
                && draft.phase == EditPhase::Ready
            {
                draft.phase = EditPhase::Conflict;
            }
        }
        for metadata in &mut self.catalog {
            if metadata.calendar == snapshot.calendar().name {
                metadata.materialized = true;
                metadata.display_name = snapshot.calendar().display_name.clone();
                metadata.mutation_access = snapshot.calendar().mutation_access;
                metadata.provenance = snapshot.calendar().provenance;
            }
        }
        if let Some(selected) = &self.occurrence {
            let matched = snapshot.occurrences().iter().enumerate().find(|(_, row)| {
                row.calendar_event == selected.calendar_event
                    && row.recurrence_point == selected.recurrence_point
            });
            let index = matched.map(|(index, _)| index);
            if self.active_index != index {
                self.bump();
            }
            self.active_index = index;
            self.draft_token = index
                .and_then(|index| i32::try_from(index).ok())
                .and_then(|index| index.checked_add(1))
                .unwrap_or(0);
            self.occurrence = matched.map(|(_, row)| row.clone());
        }
        self.snapshot = Some(snapshot);
        self.source = Some(source);
        self.ready = true;
        self.status = Status::Ready;
    }
    pub fn row_indices(&self) -> Vec<usize> {
        self.snapshot.as_ref().map_or_else(Vec::new, |snapshot| {
            (0..snapshot.occurrences().len()).collect()
        })
    }
    pub fn focus_period(&mut self, date: chrono::NaiveDate, mode: crate::ProAgendaMode) -> bool {
        use chrono::Datelike;
        let days = match mode {
            crate::ProAgendaMode::Day => 1,
            crate::ProAgendaMode::Week => 7,
            crate::ProAgendaMode::List => self.period_days(),
        };
        let start = match mode {
            crate::ProAgendaMode::Week => date.checked_sub_days(chrono::Days::new(u64::from(
                date.weekday().num_days_from_monday(),
            ))),
            crate::ProAgendaMode::Day | crate::ProAgendaMode::List => Some(date),
        };
        let Some((start, end)) = start.and_then(|start| {
            start
                .checked_add_days(chrono::Days::new(days))
                .map(|end| (start, end))
        }) else {
            return false;
        };
        self.mode = mode;
        self.start = start.to_string();
        self.end = end.to_string();
        self.snapshot = None;
        self.draft = None;
        self.occurrence = None;
        self.active_index = None;
        self.status = Status::Loading;
        self.reset_reads();
        true
    }
    pub fn period_days(&self) -> u64 {
        chrono::NaiveDate::parse_from_str(&self.start, "%Y-%m-%d")
            .ok()
            .zip(chrono::NaiveDate::parse_from_str(&self.end, "%Y-%m-%d").ok())
            .and_then(|(start, end)| u64::try_from((end - start).num_days()).ok())
            .filter(|days| *days > 0)
            .unwrap_or(7)
    }
}

/// Exact absolute window from the server's configured source, duration and zone.
/// No civil-time resolution or fallback calendar is performed in the client.
pub(super) fn widget_query(
    target: &crate::dashboard_widget_host::AgendaTarget,
    name: &CalendarName,
    now: chrono::DateTime<chrono::Utc>,
) -> Option<wire::QueryOccurrencesRequest> {
    if &target.calendar != name
        || target.limit == 0
        || target.limit > 1000
        || target.window_seconds == 0
        || target.window_seconds > 31_622_400
    {
        return None;
    }
    let start = chrono::DateTime::<chrono::Utc>::from_timestamp(now.timestamp(), 0)?;
    let end =
        start.checked_add_signed(chrono::Duration::seconds(i64::from(target.window_seconds)))?;
    let query = wire::QueryOccurrencesRequest {
        parent: format!("{}/events/-", name.as_str()),
        window: wire::CalendarWindow {
            time_zone: target.zone.name().into(),
            bounds: Some(wire::calendar_window::Bounds::Times(Box::new(
                wire::CalendarInstantRange {
                    start_time: admit::parse_timestamp(
                        &start.format("%Y-%m-%dT%H:%M:%SZ").to_string(),
                        true,
                    )
                    .ok()?
                    .into(),
                    end_time: admit::parse_timestamp(
                        &end.format("%Y-%m-%dT%H:%M:%SZ").to_string(),
                        true,
                    )
                    .ok()?
                    .into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        page_size: i32::try_from(target.limit.min(256)).ok()?,
        ..Default::default()
    };
    admit::admit_query(&query)
        .ok()
        .map(|value| value.wire().clone())
}
