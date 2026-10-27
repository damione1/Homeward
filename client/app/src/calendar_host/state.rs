use crate::calendar::{Draft, EditPhase};
use home_client_actions::calendars::PreparedCalendarMutation;
use home_client_cache::calendars::CalendarReadSnapshot;
use home_client_model::calendars::{CalendarId, CivilDate, EventKey};
use home_client_session::Generation;
use home_wire_contract::{calendars_contract as admit, home::calendars::v1 as wire};
use std::{sync::Arc, time::SystemTime};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Status {
    Loading,
    Ready,
    Offline,
    Denied,
    Unsupported,
    Invalid,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Open {
    Replace,
    Rebase,
}
pub(super) enum Job {
    Open(EventKey, Open),
    Resolve(crate::calendar::Boundary, wire::ResolveLocalTimeRequest),
    Write(PreparedCalendarMutation),
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
    pub catalog: Vec<wire::Calendar>,
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
    pub fn calendar(&self) -> Option<&wire::Calendar> {
        self.snapshot
            .as_ref()
            .map(|snapshot| snapshot.calendar())
            .or_else(|| {
                self.catalog.iter().find(|calendar| {
                    admit::calendar_id(&calendar.id).as_ref() == self.selected.as_ref()
                })
            })
    }
    pub fn current(&self) -> Option<admit::Admitted<wire::Calendar>> {
        admit::admit_calendar(self.calendar()?, self.selected.as_ref()?).ok()
    }
    pub fn editable(&self) -> bool {
        self.active
            && self.interactive
            && self.ready
            && !self.uncertain
            && self.status == Status::Ready
            && self.snapshot.as_ref().is_some_and(|snapshot| {
                snapshot.freshness() == home_client_cache::calendars::CalendarFreshness::Fresh
                    && snapshot.expiry_bounds().all(|until| {
                        chrono::DateTime::parse_from_rfc3339(until).is_ok_and(|until| {
                            SystemTime::from(until.with_timezone(&chrono::Utc)) > SystemTime::now()
                        })
                    })
            })
            && self.current().is_some_and(|calendar| {
                admit::calendar_editable(&calendar)
                    && calendar.wire().quality == wire::Quality::Fresh
                    && calendar.wire().fresh_until.as_deref().is_none_or(|until| {
                        chrono::DateTime::parse_from_rfc3339(until).is_ok_and(|until| {
                            SystemTime::from(until.with_timezone(&chrono::Utc)) > SystemTime::now()
                        })
                    })
            })
    }
    pub fn query(&self) -> Option<wire::QueryOccurrencesRequest> {
        let query = wire::QueryOccurrencesRequest {
            id: wire::CalendarId {
                entity_id: self.selected.as_ref()?.entity().as_str().into(),
                ..Default::default()
            }
            .into(),
            window: wire::Window {
                zone: self.zone.clone(),
                bounds: Some(wire::window::Bounds::Dates(Box::new(wire::DateSpan {
                    start: self.start.clone(),
                    end_exclusive: self.end.clone(),
                    ..Default::default()
                }))),
                ..Default::default()
            }
            .into(),
            page_size: 1000,
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
    pub fn snapshot(&mut self, snapshot: Arc<CalendarReadSnapshot>, source: Generation) {
        if self.selected.as_ref() != admit::calendar_id(&snapshot.calendar().id).as_ref() {
            return;
        }
        if let Some(old) = &self.snapshot
            && old.calendar().revision != snapshot.calendar().revision
        {
            self.bump();
            if let Some(draft) = &mut self.draft
                && draft.phase == EditPhase::Ready
            {
                draft.phase = EditPhase::Conflict;
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
                    let local = match &row.span.value {
                        Some(wire::span::Value::Dates(value)) => &value.start,
                        Some(wire::span::Value::Times(value)) => &value.anchor_local,
                        None => return None,
                    };
                    if self.week
                        && self
                            .day
                            .is_some_and(|day| local.get(..10) != Some(day.to_string().as_str()))
                    {
                        None
                    } else {
                        Some(index)
                    }
                })
                .collect()
        })
    }
}
