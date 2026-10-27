use super::state::{State, Status};
use crate::{
    CalendarDay, CalendarDraftView, CalendarPresentation, CivilDateView, ContentItem,
    HomewardWindow, LocalDateTimeView, LocalTimeView, ProAgendaAccess, ProAgendaDay, ProAgendaMode,
    ProAgendaPhase, ProAgendaRow, ProAgendaTemporalKind, ZonedTimePresentation,
    calendar::{Draft, EditPhase, Resolution, Temporal},
};
use chrono::Datelike;
use home_client_cache::calendars::CalendarReadSnapshot;
use home_client_model::calendars::{CalendarId, EventKey};
use home_client_session::{Generation, SessionCoordinator};
use home_wire_contract::{calendars_contract as admit, home::calendars::v1 as wire};
use slint::{Model, ModelNotify, ModelRc, ModelTracker};
use std::sync::Arc;

pub(super) struct Projection {
    generation: i32,
    privacy_generation: i32,
    source: Option<Generation>,
    session: Option<SessionCoordinator>,
    catalog: Vec<wire::Calendar>,
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
    week: bool,
    day: Option<home_client_model::calendars::CivilDate>,
    active_index: Option<usize>,
    draft: Option<Draft>,
    draft_token: i32,
    notice: String,
}
impl Projection {
    pub fn new(state: &State, session: Option<SessionCoordinator>) -> Self {
        Self {
            generation: state.generation,
            privacy_generation: state.privacy_generation,
            source: state.source,
            session,
            catalog: state.catalog.clone(),
            selected: state.selected.clone(),
            snapshot: state.snapshot.clone(),
            indices: state.row_indices(),
            editable: state.editable(),
            ready: state.ready,
            busy: state.busy,
            active: state.active,
            status: state.status,
            start: state.start.clone(),
            end: state.end.clone(),
            zone: state.zone.clone(),
            week: state.week,
            day: state.day,
            active_index: state.active_index,
            draft: state.draft.clone(),
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
        EditPhase::Withheld => ProAgendaPhase::CommittedResponseWithheld,
    }
}
fn resolution(value: &Resolution) -> (ZonedTimePresentation, String) {
    match value {
        Resolution::Unresolved => (
            ZonedTimePresentation::Unresolved,
            "Résoudre cette heure auprès du serveur.".into(),
        ),
        Resolution::Accepted(instant) => (
            ZonedTimePresentation::Unique,
            format!("Instant reçu du serveur conservé : {instant}"),
        ),
        Resolution::Unique(candidate) => (
            ZonedTimePresentation::Unique,
            format!("Instant validé : {}", candidate.instant),
        ),
        Resolution::Ambiguous {
            candidates,
            selected,
        } => (
            ZonedTimePresentation::Ambiguous,
            match selected {
                Some(index) => candidates.get(*index).map_or_else(
                    || "Choix non valide.".into(),
                    |candidate| format!("Occurrence choisie : {}", candidate.instant),
                ),
                None => {
                    "Cette heure existe deux fois. Choisissez explicitement une occurrence.".into()
                }
            },
        ),
        Resolution::Nonexistent => (
            ZonedTimePresentation::Nonexistent,
            "Cette heure n’existe pas dans ce fuseau. Aucune normalisation n’a été appliquée."
                .into(),
        ),
        Resolution::InvalidZone => (
            ZonedTimePresentation::InvalidZone,
            "Fuseau refusé par le serveur.".into(),
        ),
    }
}
fn editor(value: &Projection) -> CalendarDraftView {
    let access = if !value.active || !value.ready || value.status != Status::Ready {
        ProAgendaAccess::Offline
    } else if value.editable && !value.busy {
        ProAgendaAccess::Editable
    } else {
        ProAgendaAccess::ReadOnly
    };
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
        summary: draft.original.summary.clone().into(),
        description: draft.original.description.clone().into(),
        location: draft.original.location.clone().into(),
        exceptions: format!(
            "Récurrence {} · {} exception(s), {} exclusion(s) conservée(s)",
            draft
                .original
                .recurrence_rule
                .as_deref()
                .unwrap_or("absente"),
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
    draft: Option<(EventKey, EditPhase)>,
    notify: ModelNotify,
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
            Some(wire::span::Value::Dates(span)) => (
                span.start.as_str(),
                format!("{} – {} (fin exclue)", span.start, span.end_exclusive),
                ProAgendaTemporalKind::AllDay,
            ),
            Some(wire::span::Value::Times(span)) => (
                span.anchor_local.get(..10)?,
                format!(
                    "{} – {} · {}",
                    span.anchor_local.get(11..)?,
                    span.end_local,
                    span.zone
                ),
                ProAgendaTemporalKind::Timed,
            ),
            None => return None,
        };
        let phase = self
            .draft
            .as_ref()
            .filter(|(key, _)| admit::event_key(&row.key).as_ref() == Some(key))
            .map_or(ProAgendaPhase::Ready, |(_, value)| phase(*value));
        Some(ProAgendaRow {
            token: actual as i32 + 1,
            generation: self.generation,
            date: date(day),
            day_label: day.into(),
            time_label: time.into(),
            title: row.summary.clone().into(),
            detail: format!(
                "{} · {} · occurrence {}",
                row.location, row.description, row.recurrence_id
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
fn source(calendar: &wire::Calendar) -> String {
    use buffa::EnumValue;
    let origin = match calendar.origin {
        EnumValue::Known(wire::Origin::Local) => "Calendrier local",
        EnumValue::Known(wire::Origin::RemoteIcs) => "Source distante · lecture seule",
        EnumValue::Known(wire::Origin::Calculated) => "Source calculée · lecture seule",
        EnumValue::Known(wire::Origin::Unspecified) | EnumValue::Unknown(_) => "Source non admise",
    };
    let provenance = match calendar.provenance {
        EnumValue::Known(wire::DataProvenance::Synthetic) => " · MOCK ISOLÉ",
        EnumValue::Known(wire::DataProvenance::Observed) => " · source observée",
        EnumValue::Known(wire::DataProvenance::Unspecified) | EnumValue::Unknown(_) => {
            " · provenance non établie"
        }
    };
    format!("{origin}{provenance}")
}
fn quality(snapshot: &CalendarReadSnapshot) -> &'static str {
    use home_client_cache::calendars::CalendarFreshness;
    match snapshot.freshness() {
        CalendarFreshness::Unavailable => "Données indisponibles",
        CalendarFreshness::Stale => "Données périmées",
        CalendarFreshness::Fresh => {
            if snapshot.expiry_bounds().all(|until| {
                chrono::DateTime::parse_from_rfc3339(until).is_ok_and(|until| {
                    std::time::SystemTime::from(until.with_timezone(&chrono::Utc))
                        > std::time::SystemTime::now()
                })
            }) {
                "Données fraîches"
            } else {
                "Données périmées"
            }
        }
    }
}
pub(super) fn render(window: &HomewardWindow, value: Projection) {
    if ((!value.catalog.is_empty() || value.snapshot.is_some() || value.draft.is_some())
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
    let current = value
        .snapshot
        .as_ref()
        .map(|snapshot| snapshot.calendar())
        .or_else(|| {
            value.catalog.iter().find(|calendar| {
                admit::calendar_id(&calendar.id).as_ref() == value.selected.as_ref()
            })
        });
    let calendar_token = value
        .catalog
        .iter()
        .position(|calendar| admit::calendar_id(&calendar.id).as_ref() == value.selected.as_ref())
        .map_or(0, |index| index as i32 + 1);
    let present =
        current.is_some_and(|calendar| calendar.revision.presence == wire::Presence::Present);
    let catalog = value
        .catalog
        .iter()
        .enumerate()
        .map(|(index, calendar)| ContentItem {
            token: index as i32 + 1,
            title: calendar
                .name
                .as_deref()
                .unwrap_or("Calendrier à créer")
                .into(),
            excerpt: source(calendar).into(),
            selected: admit::calendar_id(&calendar.id).as_ref() == value.selected.as_ref(),
            disabled: !value.active || value.busy,
            ..Default::default()
        })
        .collect::<Vec<_>>();
    let draft = value
        .draft
        .as_ref()
        .map(|draft| (draft.key.clone(), draft.phase));
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
                    draft,
                    notify: ModelNotify::default(),
                })
            }
        }
        None => ModelRc::default(),
    };
    let start = chrono::NaiveDate::parse_from_str(&value.start, "%Y-%m-%d").ok();
    let days = start.map_or_else(Vec::new, |start| {
        (0..7)
            .filter_map(|offset| {
                start
                    .checked_add_days(chrono::Days::new(offset))
                    .map(|day| ProAgendaDay {
                        token: 100 + offset as i32,
                        generation: value.generation,
                        date: date(&day.to_string()),
                        label: day.format("%d/%m").to_string().into(),
                        selected: value
                            .day
                            .is_some_and(|selected| selected.to_string() == day.to_string()),
                    })
            })
            .collect()
    });
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
                                    today: day == chrono::Local::now().date_naive(),
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
    let title = current
        .and_then(|calendar| calendar.name.as_deref())
        .unwrap_or("Agenda");
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
    let notice = if value.notice.is_empty() {
        match value.status {
            Status::Loading => "Lecture des calendriers…",
            Status::Ready => "",
            Status::Offline => "Hors ligne · copie datée · écritures suspendues",
            Status::Denied => "Accès retiré",
            Status::Unsupported => "Interface calendrier indisponible",
            Status::Invalid => "Données calendrier non admises",
        }
    } else {
        &value.notice
    };
    let frame = CalendarPresentation {
        generation: value.generation,
        privacy_generation: value.privacy_generation,
        catalog: ModelRc::from(catalog.as_slice()),
        rows,
        days: ModelRc::from(days.as_slice()),
        selected: value.selected.is_some(),
        calendar_token,
        present,
        ready: value.active && value.ready,
        editable: value.editable,
        busy: value.busy,
        title: title.into(),
        notice: notice.into(),
        source: current
            .map_or_else(String::new, |calendar| {
                format!(
                    "{} · {}",
                    source(calendar),
                    value
                        .snapshot
                        .as_ref()
                        .map_or("Lecture en cours", |snapshot| quality(snapshot))
                )
            })
            .into(),
        revision: current
            .map_or_else(String::new, |calendar| {
                format!("Révision {}", calendar.revision.number)
            })
            .into(),
        cached_at: value
            .snapshot
            .as_ref()
            .map_or_else(String::new, |snapshot| {
                format!(
                    "Lu {}",
                    chrono::DateTime::<chrono::Utc>::from(snapshot.fetched_at()).to_rfc3339()
                )
            })
            .into(),
        start: value.start.clone().into(),
        end: value.end.clone().into(),
        zone: value.zone.clone().into(),
        mode: if value.week {
            ProAgendaMode::Week
        } else {
            ProAgendaMode::List
        },
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
