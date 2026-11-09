use super::state::{State, Status};
use crate::{
    CalendarDay, CalendarDraftView, CalendarPresentation, CivilDateView, ContentItem,
    HomewardWindow, LocalDateTimeView, LocalTimeView, ProAgendaAccess, ProAgendaColumn,
    ProAgendaDay, ProAgendaMode, ProAgendaPhase, ProAgendaRow, ProAgendaTemporalKind,
    ZonedTimePresentation,
    calendar::{Draft, EditPhase, Resolution, Temporal},
};
use chrono::Datelike;
use home_client_cache::calendars::CalendarReadSnapshot;
use home_client_model::{
    api_calendars::{CalendarEventName, CalendarName},
    calendars::CalendarId,
};
use home_client_session::{Generation, GenerationGuard, SessionCoordinator};
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};
use slint::{Model, ModelNotify, ModelRc, ModelTracker};
use std::sync::Arc;

pub(super) struct Projection {
    generation: i32,
    privacy_generation: i32,
    source: Option<Generation>,
    session: Option<GenerationGuard>,
    catalog: Vec<wire::CalendarSource>,
    selected: Option<CalendarId>,
    snapshot: Option<Arc<CalendarReadSnapshot>>,
    indices: Vec<usize>,
    editable: bool,
    ready: bool,
    busy: bool,
    active: bool,
    status: Status,
    start: String,
    end: String,
    zone: String,
    mode: ProAgendaMode,
    active_index: Option<usize>,
    draft: Option<Draft>,
    occurrence: Option<wire::Occurrence>,
    draft_token: i32,
    notice: String,
}
impl Projection {
    pub fn new(state: &State, session: Option<SessionCoordinator>) -> Self {
        Self {
            generation: state.generation,
            privacy_generation: state.privacy_generation,
            source: state.source,
            session: session.map(|session| session.generation_guard()),
            catalog: state.catalog.clone(),
            selected: state.selected.clone(),
            snapshot: state.snapshot.clone(),
            indices: state.row_indices(),
            editable: state.editable(),
            ready: state.ready,
            busy: state.busy(),
            active: state.active,
            status: state.status,
            start: state.start.clone(),
            end: state.end.clone(),
            zone: state.zone.clone(),
            mode: state.mode,
            active_index: state.active_index,
            draft: state.draft.clone(),
            occurrence: state.occurrence.clone(),
            draft_token: state.draft_token,
            notice: state.notice.clone(),
        }
    }
}
fn date(value: &str) -> CivilDateView {
    chrono::NaiveDate::parse_from_str(value, "%Y-%m-%d")
        .ok()
        .map_or_else(CivilDateView::default, |date| CivilDateView {
            year: date.year(),
            month: date.month() as i32,
            day: date.day() as i32,
        })
}
fn local(date_text: &str, time_text: &str) -> LocalDateTimeView {
    let time = chrono::NaiveTime::parse_from_str(time_text, "%H:%M:%S").ok();
    use chrono::Timelike;
    LocalDateTimeView {
        date: date(date_text),
        time: time.map_or_else(LocalTimeView::default, |time| LocalTimeView {
            hour: time.hour() as i32,
            minute: time.minute() as i32,
            second: time.second() as i32,
        }),
    }
}
fn phase(phase: EditPhase) -> ProAgendaPhase {
    match phase {
        EditPhase::Ready => ProAgendaPhase::Ready,
        EditPhase::Pending => ProAgendaPhase::Pending,
        EditPhase::Conflict => ProAgendaPhase::Conflict,
        EditPhase::Unknown => ProAgendaPhase::CommitUnknown,
    }
}
fn resolution(value: &Resolution) -> (ZonedTimePresentation, String) {
    match value {
        Resolution::Unresolved => (
            ZonedTimePresentation::Unresolved,
            "Resolve this local time with the server.".into(),
        ),
        Resolution::Accepted(instant) => (
            ZonedTimePresentation::Unique,
            format!("Server-provided instant retained: {instant}"),
        ),
        Resolution::Unique(candidate) => (
            ZonedTimePresentation::Unique,
            format!("Validated instant: {}", candidate.instant),
        ),
        Resolution::Ambiguous {
            candidates,
            selected,
        } => (
            ZonedTimePresentation::Ambiguous,
            match selected {
                Some(index) => candidates.get(*index).map_or_else(
                    || "Invalid selection.".into(),
                    |candidate| format!("Selected occurrence: {}", candidate.instant),
                ),
                None => "This local time occurs twice. Choose one occurrence explicitly.".into(),
            },
        ),
        Resolution::Nonexistent => (
            ZonedTimePresentation::Nonexistent,
            "This local time does not exist in this time zone. It has not been adjusted.".into(),
        ),
        Resolution::InvalidZone => (
            ZonedTimePresentation::InvalidZone,
            "Time zone rejected by the server.".into(),
        ),
    }
}
fn occurrence_view(
    row: &wire::Occurrence,
    token: i32,
    generation: i32,
    access: ProAgendaAccess,
) -> CalendarDraftView {
    let mut view = CalendarDraftView {
        selected: true,
        token,
        generation,
        title: "Occurrence".into(),
        existing: true,
        access: if access == ProAgendaAccess::Offline {
            access
        } else {
            ProAgendaAccess::ReadOnly
        },
        summary: row.summary.clone().into(),
        description: row.description.clone().into(),
        location: row.location.clone().into(),
        phase: ProAgendaPhase::Ready,
        ..Default::default()
    };
    match &row.span.value {
        Some(wire::calendar_span::Value::Dates(span)) => {
            view.kind = ProAgendaTemporalKind::AllDay;
            view.start_date = admit::date(&span.start_date)
                .map(|date| date.to_string())
                .unwrap_or_default()
                .into();
            view.end_date = admit::date(&span.end_date)
                .map(|date| date.to_string())
                .unwrap_or_default()
                .into();
            view.accepted_start = local(&view.start_date, "");
            view.accepted_end = local(&view.end_date, "");
        }
        Some(wire::calendar_span::Value::Times(span)) => {
            view.kind = ProAgendaTemporalKind::Timed;
            let start = admit::civil_text(&span.start_local).unwrap_or_default();
            let end = admit::civil_text(&span.end_local).unwrap_or_default();
            view.start_date = start.get(..10).unwrap_or_default().into();
            view.end_date = end.get(..10).unwrap_or_default().into();
            view.start_time = start.get(11..).unwrap_or_default().into();
            view.end_time = end.get(11..).unwrap_or_default().into();
            view.zone = row.time_zone.clone().into();
            view.accepted_start = local(&view.start_date, &view.start_time);
            view.accepted_end = local(&view.end_date, &view.end_time);
            view.start_resolution = ZonedTimePresentation::Unique;
            view.end_resolution = ZonedTimePresentation::Unique;
        }
        None => return CalendarDraftView::default(),
    }
    view
}

fn editor(value: &Projection) -> CalendarDraftView {
    let access = if !value.active || !value.ready || value.status != Status::Ready {
        ProAgendaAccess::Offline
    } else if value.editable && !value.busy {
        ProAgendaAccess::Editable
    } else {
        ProAgendaAccess::ReadOnly
    };
    if let Some(occurrence) = &value.occurrence {
        return occurrence_view(occurrence, value.draft_token, value.generation, access);
    }
    let Some(draft) = &value.draft else {
        return CalendarDraftView {
            access,
            ..Default::default()
        };
    };
    let mut view = CalendarDraftView {
        selected: true,
        token: value.draft_token,
        generation: value.generation,
        access,
        phase: phase(draft.phase),
        existing: draft.existing,
        title: if draft
            .original
            .recurrence_rule
            .as_deref()
            .is_some_and(|rule| !rule.is_empty())
        {
            "Event series"
        } else {
            "Event"
        }
        .into(),
        summary: draft.original.summary.clone().into(),
        description: draft.original.description.clone().into(),
        location: draft.original.location.clone().into(),
        exceptions: format!(
            "Recurrence {} · {} exception(s), {} exclusion(s) retained",
            draft
                .original
                .recurrence_rule
                .as_deref()
                .unwrap_or("missing"),
            draft.original.exceptions.len(),
            draft.original.exclusions.len()
        )
        .into(),
        ..Default::default()
    };
    match &draft.temporal {
        Temporal::AllDay { start, end } => {
            view.kind = ProAgendaTemporalKind::AllDay;
            view.start_date = start.clone().into();
            view.end_date = end.clone().into();
            view.accepted_start = local(start, "");
            view.accepted_end = local(end, "");
        }
        Temporal::Timed { start, end, zone } => {
            view.kind = ProAgendaTemporalKind::Timed;
            view.start_date = start.date.clone().into();
            view.end_date = end.date.clone().into();
            view.start_time = start.time.clone().into();
            view.end_time = end.time.clone().into();
            view.zone = zone.clone().into();
            view.accepted_start = local(&start.date, &start.time);
            view.accepted_end = local(&end.date, &end.time);
            let (a, message) = resolution(&start.resolution);
            view.start_resolution = a;
            view.start_message = message.into();
            let (a, message) = resolution(&end.resolution);
            view.end_resolution = a;
            view.end_message = message.into();
        }
    }
    view.valid = !draft.original.summary.is_empty()
        && draft.event().is_some()
        && draft.phase == EditPhase::Ready;
    view
}
struct Rows {
    snapshot: Arc<CalendarReadSnapshot>,
    indices: Vec<usize>,
    generation: i32,
    draft: Option<(CalendarEventName, EditPhase)>,
    notify: ModelNotify,
}
fn timed_label(start: &str, end: &str) -> Option<String> {
    Some(if start.get(..10)? == end.get(..10)? {
        format!("{}–{}", start.get(11..16)?, end.get(11..16)?)
    } else {
        format!("{} – {}", start.get(..16)?, end.get(..16)?)
    })
}

impl Model for Rows {
    type Data = ProAgendaRow;
    fn row_count(&self) -> usize {
        self.indices.len()
    }
    fn row_data(&self, index: usize) -> Option<ProAgendaRow> {
        let actual = *self.indices.get(index)?;
        let row = self.snapshot.occurrences().get(actual)?;
        let (day, time, kind) = match &row.span.value {
            Some(wire::calendar_span::Value::Dates(span)) => {
                let start = admit::date(&span.start_date).ok()?.to_string();
                let end = admit::date(&span.end_date).ok()?.to_string();
                (
                    start.clone(),
                    format!("{start} – {end} (end exclusive)"),
                    ProAgendaTemporalKind::AllDay,
                )
            }
            Some(wire::calendar_span::Value::Times(span)) => {
                let start = admit::civil_text(&span.start_local).ok()?;
                let end = admit::civil_text(&span.end_local).ok()?;
                (
                    start.get(..10)?.to_owned(),
                    timed_label(&start, &end)?,
                    ProAgendaTemporalKind::Timed,
                )
            }
            None => return None,
        };
        let phase = self
            .draft
            .as_ref()
            .filter(|(key, _)| {
                CalendarEventName::try_from(row.calendar_event.as_str())
                    .ok()
                    .as_ref()
                    == Some(key)
            })
            .map_or(ProAgendaPhase::Ready, |(_, value)| phase(*value));
        Some(ProAgendaRow {
            token: actual as i32 + 1,
            generation: self.generation,
            date: date(&day),
            day_label: day.into(),
            time_label: time.into(),
            title: row.summary.clone().into(),
            detail: format!(
                "{} · {} · occurrence {}",
                row.location,
                row.description,
                admit::point_key(&row.recurrence_point).ok()?
            )
            .into(),
            kind,
            phase,
            ..Default::default()
        })
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
    fn as_any(&self) -> &dyn std::any::Any {
        self
    }
}
// Membership is a civil presentation of the server-resolved span. Exclusive
// ends are preserved, including midnight and all-day multi-day occurrences.
pub(super) fn occurrence_on_day(row: &wire::Occurrence, day: chrono::NaiveDate) -> bool {
    match &row.span.value {
        Some(wire::calendar_span::Value::Dates(span)) => {
            let bounds = admit::date(&span.start_date)
                .ok()
                .zip(admit::date(&span.end_date).ok());
            bounds.is_some_and(|(start, end)| {
                start.to_string() <= day.to_string() && day.to_string() < end.to_string()
            })
        }
        Some(wire::calendar_span::Value::Times(span)) => {
            let bounds = admit::civil_text(&span.start_local)
                .ok()
                .zip(admit::civil_text(&span.end_local).ok());
            bounds.is_some_and(|(start, end)| {
                let lower = format!("{day}T00:00:00");
                let upper = day
                    .checked_add_days(chrono::Days::new(1))
                    .map(|day| format!("{day}T00:00:00"));
                upper.is_some_and(|upper| {
                    start < upper && (end > lower || (start == end && start >= lower))
                })
            })
        }
        None => false,
    }
}

fn source(
    origin: buffa::EnumValue<wire::CalendarOrigin>,
    provenance: buffa::EnumValue<wire::CalendarProvenance>,
) -> String {
    use buffa::EnumValue;
    let origin = match origin {
        EnumValue::Known(wire::CalendarOrigin::Local) => "Local Calendar",
        EnumValue::Known(wire::CalendarOrigin::RemoteIcs) => "Remote source · read-only",
        EnumValue::Known(wire::CalendarOrigin::Calculated) => "Calculated source · read-only",
        EnumValue::Known(wire::CalendarOrigin::Unspecified) | EnumValue::Unknown(_) => {
            "Source rejected"
        }
    };
    let provenance = match provenance {
        EnumValue::Known(wire::CalendarProvenance::Synthetic) => " · ISOLATED MOCK",
        EnumValue::Known(wire::CalendarProvenance::Observed) => " · observed source",
        EnumValue::Known(wire::CalendarProvenance::Unspecified) | EnumValue::Unknown(_) => {
            " · source not verified"
        }
    };
    format!("{origin}{provenance}")
}
fn snapshot_fresh(snapshot: &CalendarReadSnapshot) -> bool {
    snapshot.freshness() == home_client_cache::calendars::CalendarFreshness::Fresh
        && snapshot.expiry_bounds().all(|until| {
            chrono::DateTime::parse_from_rfc3339(until).is_ok_and(|until| {
                std::time::SystemTime::from(until.with_timezone(&chrono::Utc))
                    > std::time::SystemTime::now()
            })
        })
}
fn quality(snapshot: &CalendarReadSnapshot) -> &'static str {
    use home_client_cache::calendars::CalendarFreshness;
    match snapshot.freshness() {
        CalendarFreshness::Unavailable => "Data unavailable",
        CalendarFreshness::Stale => "Outdated data",
        CalendarFreshness::Fresh => {
            if snapshot_fresh(snapshot) {
                "Up to date"
            } else {
                "Outdated data"
            }
        }
    }
}
pub(super) fn render(window: &HomewardWindow, value: Projection) {
    if ((!value.catalog.is_empty()
        || value.snapshot.is_some()
        || value.draft.is_some()
        || value.occurrence.is_some())
        && (value.source.is_none() || value.session.is_none()))
        || value.source.is_some_and(|generation| {
            value
                .session
                .as_ref()
                .is_none_or(|session| !session.is_current(generation))
        })
    {
        window.set_calendars_publishing(true);
        window.set_calendars(Default::default());
        draft_values(window, &CalendarDraftView::default());
        window.set_calendars_name("".into());
        window.set_calendars_publishing(false);
        return;
    }
    let previous = window.get_calendars();
    let current = value.snapshot.as_ref().map(|snapshot| snapshot.calendar());
    let selected_source = value.catalog.iter().find(|source| {
        CalendarName::try_from(source.calendar.as_str())
            .ok()
            .as_ref()
            .map(CalendarName::native_id)
            == value.selected.as_ref()
    });
    let calendar_token = value
        .catalog
        .iter()
        .position(|source| {
            CalendarName::try_from(source.calendar.as_str())
                .ok()
                .as_ref()
                .map(CalendarName::native_id)
                == value.selected.as_ref()
        })
        .map_or(0, |index| index as i32 + 1);
    let present = current.is_some();
    let catalog = value
        .catalog
        .iter()
        .enumerate()
        .map(|(index, calendar)| ContentItem {
            token: index as i32 + 1,
            title: if !calendar.display_name.is_empty() {
                calendar.display_name.clone()
            } else if calendar.materialized {
                "Calendar".into()
            } else {
                "Calendar to Create".into()
            }
            .into(),
            excerpt: source(calendar.origin, calendar.provenance).into(),
            selected: CalendarName::try_from(calendar.calendar.as_str())
                .ok()
                .as_ref()
                .map(CalendarName::native_id)
                == value.selected.as_ref(),
            disabled: !value.active || value.busy,
            ..Default::default()
        })
        .collect::<Vec<_>>();
    let draft = value
        .draft
        .as_ref()
        .and_then(|draft| draft.key.clone().map(|key| (key, draft.phase)));
    let rows = match &value.snapshot {
        Some(snapshot) => {
            let same = previous
                .rows
                .as_any()
                .downcast_ref::<Rows>()
                .is_some_and(|old| {
                    Arc::ptr_eq(&old.snapshot, snapshot)
                        && old.generation == value.generation
                        && old.indices == value.indices
                        && old.draft == draft
                });
            if same {
                previous.rows.clone()
            } else {
                ModelRc::new(Rows {
                    snapshot: snapshot.clone(),
                    indices: value.indices.clone(),
                    generation: value.generation,
                    draft: draft.clone(),
                    notify: ModelNotify::default(),
                })
            }
        }
        None => ModelRc::default(),
    };
    let start = chrono::NaiveDate::parse_from_str(&value.start, "%Y-%m-%d").ok();
    let days: Vec<ProAgendaDay> = start.map_or_else(
        || {
            (0..7)
                .map(|offset| ProAgendaDay {
                    token: 100 + offset,
                    generation: value.generation,
                    label: "—".into(),
                    ..Default::default()
                })
                .collect()
        },
        |start| {
            (0..if value.mode == ProAgendaMode::Day {
                1
            } else {
                7
            })
                .filter_map(|offset| {
                    start
                        .checked_add_days(chrono::Days::new(offset))
                        .map(|day| ProAgendaDay {
                            token: 100 + offset as i32,
                            generation: value.generation,
                            date: date(&day.to_string()),
                            label: day.format("%d/%m").to_string().into(),
                            selected: value.mode == ProAgendaMode::Day,
                        })
                })
                .collect()
        },
    );
    let mut active_column_claimed = false;
    let columns = days
        .iter()
        .map(|day| {
            let Some(day_date) = chrono::NaiveDate::from_ymd_opt(
                day.date.year,
                day.date.month as u32,
                day.date.day as u32,
            ) else {
                return ProAgendaColumn {
                    day: day.clone(),
                    active_index: -1,
                    ..Default::default()
                };
            };
            let indices = value.snapshot.as_ref().map_or_else(Vec::new, |snapshot| {
                snapshot
                    .occurrences()
                    .iter()
                    .enumerate()
                    .filter_map(|(index, row)| occurrence_on_day(row, day_date).then_some(index))
                    .collect()
            });
            let active_index = if active_column_claimed {
                -1
            } else {
                value
                    .active_index
                    .and_then(|active| indices.iter().position(|index| *index == active))
                    .map_or(-1, |index| index as i32)
            };
            active_column_claimed |= active_index >= 0;
            let column_rows = value
                .snapshot
                .as_ref()
                .map_or_else(ModelRc::default, |snapshot| {
                    ModelRc::new(Rows {
                        snapshot: snapshot.clone(),
                        indices,
                        generation: value.generation,
                        draft: draft.clone(),
                        notify: ModelNotify::default(),
                    })
                });
            ProAgendaColumn {
                active: active_index >= 0,
                active_index,
                day: ProAgendaDay {
                    label: day_date.day().to_string().into(),
                    ..day.clone()
                },
                weekday: day_date.format("%a").to_string().into(),
                today: value
                    .zone
                    .parse::<home_client_model::local_time::DisplayTimeZone>()
                    .ok()
                    .is_some_and(|zone| {
                        zone.local(crate::environment_view::now()).date() == day_date.to_string()
                    }),
                rows: column_rows,
            }
        })
        .collect::<Vec<_>>();
    let calendar_days = start
        .and_then(|day| day.with_day(1))
        .map_or_else(Vec::new, |first| {
            first
                .checked_sub_days(chrono::Days::new(u64::from(
                    first.weekday().num_days_from_monday(),
                )))
                .map_or_else(Vec::new, |first_grid| {
                    (0..42)
                        .filter_map(|offset| {
                            first_grid
                                .checked_add_days(chrono::Days::new(offset))
                                .map(|day| CalendarDay {
                                    year: day.year(),
                                    month: day.month() as i32,
                                    day: day.day() as i32,
                                    label: day.day().to_string().into(),
                                    accessible_label: day.to_string().into(),
                                    enabled: true,
                                    selected: Some(day) == start,
                                    in_range: day.month() == first.month(),
                                    today: value
                                        .zone
                                        .parse::<home_client_model::local_time::DisplayTimeZone>()
                                        .ok()
                                        .is_some_and(|zone| {
                                            zone.local(crate::environment_view::now()).date()
                                                == day.to_string()
                                        }),
                                })
                        })
                        .collect()
                })
        });
    if previous.start.as_str() != value.start {
        window.set_calendars_start(value.start.clone().into());
    }
    if previous.end.as_str() != value.end {
        window.set_calendars_end(value.end.clone().into());
    }
    if previous.zone.as_str() != value.zone {
        window.set_calendars_zone(value.zone.clone().into());
    }
    let title = current.map_or("Calendar", |calendar| calendar.display_name.as_str());
    if previous.privacy_generation != value.privacy_generation
        || previous.calendar_token != calendar_token
        || !value.active
    {
        window.set_calendars_name(if value.active { title } else { "" }.into());
    }
    let active_index = value
        .active_index
        .and_then(|index| {
            value
                .indices
                .iter()
                .position(|candidate| *candidate == index)
        })
        .map_or(-1, |index| index as i32);
    let notice = if value.status == Status::Offline || value.notice.is_empty() {
        match value.status {
            Status::Loading => "Reading calendars…",
            Status::Ready => "",
            Status::Offline => "Offline · dated copy · writes suspended",
            Status::Denied => "Access removed",
            Status::Unsupported => "Calendar interface unavailable",
            Status::Invalid => "Calendar data rejected",
            Status::TooLarge => "Shorten the calendar range.",
        }
    } else {
        &value.notice
    };
    let notice = if value.status == Status::Ready
        && let Some(snapshot) = value
            .snapshot
            .as_deref()
            .filter(|snapshot| !snapshot_fresh(snapshot))
    {
        if notice.is_empty() {
            quality(snapshot).to_owned()
        } else {
            format!("{notice}\n{}", quality(snapshot))
        }
    } else {
        notice.to_owned()
    };
    let frame = CalendarPresentation {
        generation: value.generation,
        privacy_generation: value.privacy_generation,
        catalog: ModelRc::from(catalog.as_slice()),
        rows,
        days: ModelRc::from(days.as_slice()),
        columns: ModelRc::from(columns.as_slice()),
        period: start
            .map_or_else(
                || "Calendar".into(),
                |start| {
                    format!(
                        "{} – {}",
                        start.format("%B %-d"),
                        chrono::NaiveDate::parse_from_str(&value.end, "%Y-%m-%d")
                            .ok()
                            .and_then(|end| end.checked_sub_days(chrono::Days::new(1)))
                            .unwrap_or(start)
                            .format("%B %-d, %Y")
                    )
                },
            )
            .into(),
        selected: value.selected.is_some(),
        calendar_token,
        present,
        ready: value.active && value.ready,
        editable: value.editable,
        busy: value.busy,
        title: title.into(),
        notice: notice.into(),
        source: selected_source
            .map_or_else(String::new, |calendar| {
                format!(
                    "{} · {}",
                    source(calendar.origin, calendar.provenance),
                    value
                        .snapshot
                        .as_ref()
                        .map_or("Loading", |snapshot| quality(snapshot))
                )
            })
            .into(),
        revision: current
            .map_or_else(String::new, |calendar| {
                calendar
                    .update_time
                    .as_option()
                    .and_then(|value| admit::timestamp_text(value).ok())
                    .map_or_else(String::new, |value| format!("Updated {value}"))
            })
            .into(),
        cached_at: value
            .snapshot
            .as_ref()
            .map_or_else(String::new, |snapshot| {
                format!(
                    "Read {}",
                    chrono::DateTime::<chrono::Utc>::from(snapshot.fetched_at()).to_rfc3339()
                )
            })
            .into(),
        start: value.start.clone().into(),
        end: value.end.clone().into(),
        zone: value.zone.clone().into(),
        mode: value.mode,
        active_index,
        editor: editor(&value),
        calendar_days: ModelRc::from(calendar_days.as_slice()),
        calendar_title: start
            .map_or_else(String::new, |date| date.format("%Y-%m").to_string())
            .into(),
        calendar_index: calendar_days
            .iter()
            .position(|day| day.selected)
            .map_or(-1, |i| i as i32),
        accepted_date: date(&value.start),
    };
    window.set_calendars_publishing(true);
    let editor = frame.editor.clone();
    window.set_calendars(frame);
    draft_values(window, &editor);
    window.set_calendars_publishing(false);
}

fn draft_values(window: &HomewardWindow, editor: &CalendarDraftView) {
    window.set_calendars_edit_summary(editor.summary.clone());
    window.set_calendars_edit_description(editor.description.clone());
    window.set_calendars_edit_location(editor.location.clone());
    window.set_calendars_edit_start_date(editor.start_date.clone());
    window.set_calendars_edit_end_date(editor.end_date.clone());
    window.set_calendars_edit_start_time(editor.start_time.clone());
    window.set_calendars_edit_end_time(editor.end_time.clone());
    window.set_calendars_edit_zone(editor.zone.clone());
}

/// Publish one exact configured source into its own dashboard delegate. A
/// missing, stale or unavailable source is never represented as an empty agenda.
pub(super) fn render_widget(
    window: &HomewardWindow,
    value: Projection,
    target: &crate::dashboard_widget_host::AgendaTarget,
) {
    let current_session = value.source.is_some_and(|generation| {
        value
            .session
            .as_ref()
            .is_some_and(|session| session.is_current(generation))
    });
    let snapshot = value.snapshot.as_ref().filter(|snapshot| {
        current_session && value.active && snapshot.calendar().name == target.calendar.as_str()
    });
    let present = snapshot.is_some();
    let fresh = snapshot.is_some_and(|snapshot| snapshot_fresh(snapshot));
    let ready = current_session
        && value.active
        && value.ready
        && value.status == Status::Ready
        && present
        && fresh;
    let status = if !value.notice.is_empty() {
        value.notice.clone()
    } else if !current_session {
        "Calendar source unavailable".into()
    } else if value.ready && !present {
        "Configured calendar not found".into()
    } else if value.ready && !fresh {
        "Calendar data outdated or unavailable".into()
    } else {
        match value.status {
            Status::Loading => "Reading calendar…",
            Status::Ready => "",
            Status::Offline => "Offline · dated copy",
            Status::Denied => "Calendar access removed",
            Status::Unsupported => "Calendar interface unavailable",
            Status::Invalid => "Calendar data rejected",
            Status::TooLarge => "Calendar range too large",
        }
        .into()
    };
    let rows = snapshot.map_or_else(ModelRc::default, |snapshot| {
        ModelRc::new(WidgetRows {
            snapshot: snapshot.clone(),
            indices: value.indices.into_iter().take(target.limit).collect(),
            generation: value.generation,
            zone: target.zone,
            notify: ModelNotify::default(),
        })
    });
    crate::cache_view::publish_widget_agenda(
        window,
        target.token,
        target.generation,
        rows,
        ready,
        status,
    );
}
struct WidgetRows {
    snapshot: Arc<CalendarReadSnapshot>,
    indices: Vec<usize>,
    generation: i32,
    zone: home_client_model::local_time::DisplayTimeZone,
    notify: ModelNotify,
}
fn widget_local(
    value: &str,
    zone: home_client_model::local_time::DisplayTimeZone,
) -> Option<home_client_model::local_time::LocalTime> {
    let instant = chrono::DateTime::parse_from_rfc3339(value).ok()?;
    let instant = home_client_model::local_time::UtcInstant::new(
        instant.timestamp(),
        instant.timestamp_subsec_nanos() as i32,
    )
    .ok()?;
    Some(zone.local(instant))
}
impl Model for WidgetRows {
    type Data = ProAgendaRow;
    fn row_count(&self) -> usize {
        self.indices.len()
    }
    fn row_data(&self, index: usize) -> Option<Self::Data> {
        let actual = *self.indices.get(index)?;
        let row = self.snapshot.occurrences().get(actual)?;
        let (day, time, kind) = match &row.span.value {
            Some(wire::calendar_span::Value::Dates(span)) => (
                admit::date(&span.start_date).ok()?.to_string(),
                "All Day".to_owned(),
                ProAgendaTemporalKind::AllDay,
            ),
            Some(wire::calendar_span::Value::Times(span)) => {
                let start =
                    widget_local(&admit::timestamp_text(&span.start_time).ok()?, self.zone)?;
                let end = widget_local(&admit::timestamp_text(&span.end_time).ok()?, self.zone)?;
                (
                    start.date(),
                    format!(
                        "{} – {} · {} ({})",
                        start.clock(),
                        end.clock(),
                        self.zone.name(),
                        start.offset()
                    ),
                    ProAgendaTemporalKind::Timed,
                )
            }
            None => return None,
        };
        Some(ProAgendaRow {
            token: i32::try_from(actual).ok()?.checked_add(1)?,
            generation: self.generation,
            date: date(&day),
            day_label: day.into(),
            time_label: time.into(),
            title: row.summary.clone().into(),
            detail: row.location.clone().into(),
            kind,
            phase: ProAgendaPhase::Ready,
            ..Default::default()
        })
    }
    fn model_tracker(&self) -> &dyn ModelTracker {
        &self.notify
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn labels_preserve_the_end_date_when_an_event_crosses_midnight() {
        assert_eq!(
            super::timed_label("2026-11-02T09:00:00", "2026-11-02T10:00:00").as_deref(),
            Some("09:00–10:00")
        );
        assert_eq!(
            super::timed_label("2026-11-02T09:00:00", "2026-11-03T09:00:00").as_deref(),
            Some("2026-11-02T09:00 – 2026-11-03T09:00")
        );
    }

    use super::*;
    #[test]
    fn civil_columns_keep_exclusive_ends_and_cross_midnight_membership() {
        let day = |text: &str| chrono::NaiveDate::parse_from_str(text, "%Y-%m-%d").unwrap();
        let all_day = wire::Occurrence {
            span: wire::CalendarSpan {
                value: Some(wire::calendar_span::Value::Dates(Box::new(
                    wire::CalendarDateSpan {
                        start_date: admit::date_wire("2026-10-31".parse().unwrap()).into(),
                        end_date: admit::date_wire("2026-11-03".parse().unwrap()).into(),
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        assert!(occurrence_on_day(&all_day, day("2026-11-02")));
        assert!(!occurrence_on_day(&all_day, day("2026-11-03")));
        let mut timed = wire::Occurrence {
            span: wire::CalendarSpan {
                value: Some(wire::calendar_span::Value::Times(Box::new(
                    wire::CalendarTimeSpan {
                        start_local: admit::parse_civil("2026-11-01T23:00:00").unwrap().into(),
                        end_local: admit::parse_civil("2026-11-02T00:00:00").unwrap().into(),
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        };
        assert!(occurrence_on_day(&timed, day("2026-11-01")));
        assert!(!occurrence_on_day(&timed, day("2026-11-02")));
        if let Some(wire::calendar_span::Value::Times(span)) =
            &mut timed.span.as_option_mut().unwrap().value
        {
            span.end_local = admit::parse_civil("2026-11-02T00:30:00").unwrap().into();
        }
        assert!(occurrence_on_day(&timed, day("2026-11-02")));
    }
}
