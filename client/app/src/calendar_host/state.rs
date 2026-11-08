use crate::calendar::{Draft, EditPhase};
use home_client_actions::calendars::PreparedCalendarMutation;
use home_client_cache::calendars::CalendarReadSnapshot;
use home_client_model::{
    api_calendars::{CalendarEventName, CalendarName},
    calendars::{CalendarId, CivilDate},
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
    pub week: bool,
    pub day: Option<CivilDate>,
    pub active_index: Option<usize>,
    pub draft: Option<Draft>,
    pub draft_token: i32,
    pub notice: String,
    pub job: Option<Job>,
    pub busy: bool,
    pub uncertain: bool,
    pub query_override: Option<wire::QueryOccurrencesRequest>,
}
impl Default for State {
    fn default() -> Self {
        let today = chrono::Local::now().date_naive();
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
            start: today.to_string(),
            end: today
                .checked_add_days(chrono::Days::new(7))
                .unwrap_or(today)
                .to_string(),
            zone: "UTC".into(),
            week: false,
            day: None,
            active_index: None,
            draft: None,
            draft_token: 0,
            notice: String::new(),
            job: None,
            busy: false,
            uncertain: false,
            query_override: None,
        }
    }
}
impl State {
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
        self.snapshot = None;
        self.draft = None;
        self.active_index = None;
        self.selected = None;
        self.source = None;
        self.job = None;
        self.busy = false;
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
            && !self.busy
            && self
                .draft
                .as_ref()
                .is_some_and(|draft| matches!(draft.phase, EditPhase::Ready | EditPhase::Conflict))
    }
    pub fn enqueue(&mut self, job: Job) -> bool {
        if self.busy || !self.active || !self.interactive {
            return false;
        }
        self.job = Some(job);
        self.busy = true;
        true
    }
    /// A Watch publication may supersede the visual generation while this read
    /// is in flight. Drop its value, but release its own busy slot. Another
    /// resource context owns a different slot and must remain untouched.
    pub fn finish_read_job(&mut self, context: u64, generation: i32) -> bool {
        if self.context != context {
            return false;
        }
        self.busy = false;
        self.generation == generation
    }
    pub fn owns_write(&self, context: u64) -> bool {
        self.context == context && self.active && self.interactive && self.busy
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
        self.snapshot = Some(snapshot);
        self.source = Some(source);
        self.ready = true;
        self.status = Status::Ready;
    }
    pub fn row_indices(&self) -> Vec<usize> {
        self.snapshot.as_ref().map_or_else(Vec::new, |snapshot| {
            snapshot
                .occurrences()
                .iter()
                .enumerate()
                .filter_map(|(index, row)| {
                    let span = admit::span(&row.span).ok()?;
                    let date = span
                        .date_bounds()
                        .map(|(start, _)| start)
                        .or_else(|| span.timed().map(|time| time.start_local().date()))?;
                    if self.week && self.day.is_some_and(|day| day != date) {
                        None
                    } else {
                        Some(index)
                    }
                })
                .collect()
        })
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
