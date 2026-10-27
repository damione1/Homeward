//! Bounded Calendar admission. The server owns rights, recurrences and timezone
//! resolution. Numeric offset checks validate its statement without a zone engine.
use crate::home::calendars::v1 as wire;
use buffa::EnumValue;
use chrono::{DateTime, Datelike, NaiveDateTime, Utc};
use home_client_model::calendars::{
    CalendarId, CalendarRevision, CivilDate, EventKey, EventRevision, EventUid, RevisionState,
};
use std::collections::BTreeSet;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AdmissionError {
    Invalid,
    ForeignResource,
    Capacity,
    StaleRevision,
    Sequence,
}
pub struct Admitted<T> {
    wire: T,
}
impl<T> Admitted<T> {
    pub fn wire(&self) -> &T {
        &self.wire
    }
}
fn admit<T: Clone>(value: &T, valid: bool) -> Result<Admitted<T>, AdmissionError> {
    if valid {
        Ok(Admitted {
            wire: value.clone(),
        })
    } else {
        Err(AdmissionError::Invalid)
    }
}
fn text(value: &str, limit: usize, required: bool) -> bool {
    (!required || !value.is_empty()) && value.len() <= limit && !value.contains('\0')
}
fn zone(value: &str) -> bool {
    text(value, 512, true) && !value.chars().any(char::is_control)
}
pub fn calendar_id(value: &wire::CalendarId) -> Option<CalendarId> {
    if !value.__buffa_unknown_fields.is_empty()
        || (value.entity_id.is_empty() || value.entity_id.len() > 156 * 1024)
    {
        return None;
    }
    value.entity_id.parse().ok().map(CalendarId::new)
}
pub fn event_key(value: &wire::EventKey) -> Option<EventKey> {
    if !value.__buffa_unknown_fields.is_empty() {
        return None;
    }
    Some(EventKey::new(
        calendar_id(value.calendar.as_option()?)?,
        value.uid.parse::<EventUid>().ok()?,
    ))
}
fn local(value: &str) -> Option<NaiveDateTime> {
    if value.len() != 19
        || value.get(..10)?.parse::<CivilDate>().is_err()
        || value.as_bytes()[10] != b'T'
    {
        return None;
    }
    let parsed = NaiveDateTime::parse_from_str(value, "%Y-%m-%dT%H:%M:%S").ok()?;
    (parsed.format("%Y-%m-%dT%H:%M:%S").to_string() == value).then_some(parsed)
}
fn stamp(value: &str, whole: bool) -> Option<DateTime<Utc>> {
    if value.len() > 30 || !value.ends_with('Z') || whole && value.len() != 20 {
        return None;
    }
    if value.get(..10)?.parse::<CivilDate>().is_err() || value.as_bytes().get(10) != Some(&b'T') {
        return None;
    }
    let result = DateTime::parse_from_rfc3339(value)
        .ok()?
        .with_timezone(&Utc);
    if !(1..=9999).contains(&result.year())
        || (result.timestamp() == -62_135_596_800 && result.timestamp_subsec_nanos() == 0)
        || result.timestamp_subsec_nanos() >= 1_000_000_000
        || whole && result.timestamp_subsec_nanos() != 0
    {
        return None;
    }
    Some(result)
}
fn revision_state(v: &wire::Revision, id: &CalendarId, uid: &str) -> Option<RevisionState> {
    if !v.__buffa_unknown_fields.is_empty() {
        return None;
    }
    let resource = v.resource.as_option()?;
    if !resource.__buffa_unknown_fields.is_empty()
        || calendar_id(resource.calendar.as_option()?)?.ne(id)
        || resource.uid != uid
    {
        return None;
    }
    match v.presence {
        EnumValue::Known(wire::Presence::Absent) => {
            (v.number == 0).then_some(RevisionState::Absent)
        }
        EnumValue::Known(wire::Presence::Present) => {
            Some(RevisionState::Present(v.number.try_into().ok()?))
        }
        EnumValue::Known(wire::Presence::Removed) => {
            Some(RevisionState::Removed(v.number.try_into().ok()?))
        }
        EnumValue::Known(wire::Presence::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub fn calendar_revision(v: &wire::Revision, id: &CalendarId) -> Option<CalendarRevision> {
    Some(CalendarRevision::new(
        id.clone(),
        revision_state(v, id, "")?,
    ))
}
pub fn event_revision(v: &wire::Revision, key: &EventKey) -> Option<EventRevision> {
    Some(EventRevision::new(
        key.clone(),
        revision_state(v, key.calendar(), key.uid().as_str())?,
    ))
}
fn quality(v: EnumValue<wire::Quality>) -> bool {
    match v {
        EnumValue::Known(
            wire::Quality::Fresh | wire::Quality::Stale | wire::Quality::Unavailable,
        ) => true,
        EnumValue::Known(wire::Quality::Unspecified) | EnumValue::Unknown(_) => false,
    }
}
fn provenance(v: EnumValue<wire::DataProvenance>) -> bool {
    match v {
        EnumValue::Known(
            wire::DataProvenance::Unspecified
            | wire::DataProvenance::Observed
            | wire::DataProvenance::Synthetic,
        ) => true,
        EnumValue::Unknown(_) => false,
    }
}
pub fn admit_calendar(
    v: &wire::Calendar,
    expected: &CalendarId,
) -> Result<Admitted<wire::Calendar>, AdmissionError> {
    let id =
        v.id.as_option()
            .and_then(calendar_id)
            .ok_or(AdmissionError::Invalid)?;
    if &id != expected {
        return Err(AdmissionError::ForeignResource);
    }
    let revision = v
        .revision
        .as_option()
        .and_then(|r| calendar_revision(r, &id))
        .ok_or(AdmissionError::Invalid)?;
    let origin = match v.origin {
        EnumValue::Known(
            wire::Origin::Local | wire::Origin::RemoteIcs | wire::Origin::Calculated,
        ) => true,
        EnumValue::Known(wire::Origin::Unspecified) | EnumValue::Unknown(_) => false,
    };
    let access = match v.mutation_access {
        EnumValue::Known(
            wire::MutationAccess::Unspecified
            | wire::MutationAccess::Denied
            | wire::MutationAccess::Allowed,
        ) => true,
        EnumValue::Unknown(_) => false,
    };
    let name = match revision.state() {
        RevisionState::Present(_) => v.name.as_deref().is_some_and(|n| text(n, 512, true)),
        RevisionState::Absent | RevisionState::Removed(_) => v.name.is_none(),
    };
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && origin
            && access
            && name
            && quality(v.quality)
            && provenance(v.provenance)
            && v.fresh_until
                .as_deref()
                .is_none_or(|s| stamp(s, false).is_some()),
    )
}
/// This is a UI eligibility check, never a replacement for the server permit.
pub fn calendar_editable(v: &Admitted<wire::Calendar>) -> bool {
    v.wire.origin == wire::Origin::Local
        && v.wire.quality == wire::Quality::Fresh
        && v.wire.mutation_access == wire::MutationAccess::Allowed
}
pub fn admit_catalog(
    v: &wire::ListCalendarsResponse,
) -> Result<Admitted<wire::ListCalendarsResponse>, AdmissionError> {
    if v.calendars.len() > 256 {
        return Err(AdmissionError::Capacity);
    }
    let mut ids = BTreeSet::new();
    for calendar in &v.calendars {
        let id = calendar
            .id
            .as_option()
            .and_then(calendar_id)
            .ok_or(AdmissionError::Invalid)?;
        admit_calendar(calendar, &id)?;
        if !ids.insert(id) {
            return Err(AdmissionError::Invalid);
        }
    }
    admit(v, v.__buffa_unknown_fields.is_empty())
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum TemporalKind {
    Civil,
    Timed,
}
fn point(value: &str) -> Option<TemporalKind> {
    if value.parse::<CivilDate>().is_ok() {
        return Some(TemporalKind::Civil);
    }
    if local(value).is_some() {
        return Some(TemporalKind::Timed);
    }
    let (wall, at) = value.split_once('@')?;
    let delta = local(wall)?.and_utc().timestamp() - stamp(at, true)?.timestamp();
    (delta.abs() <= 26 * 3600).then_some(TemporalKind::Timed)
}
fn dates(v: &wire::DateSpan) -> bool {
    if !v.__buffa_unknown_fields.is_empty() {
        return false;
    }
    let (Ok(start), Ok(end)) = (
        v.start.parse::<CivilDate>(),
        v.end_exclusive.parse::<CivilDate>(),
    ) else {
        return false;
    };
    (1..=366).contains(&start.days_until(end))
}
fn times(v: &wire::TimeSpan, allow_point: bool, anchor_required: bool) -> bool {
    if !v.__buffa_unknown_fields.is_empty() || !zone(&v.zone) {
        return false;
    }
    let (Some(start), Some(end)) = (stamp(&v.start_instant, true), stamp(&v.end_instant, true))
    else {
        return false;
    };
    let duration = (end - start).num_seconds();
    if duration < 0 || duration == 0 && !allow_point || duration > 366 * 86400 {
        return false;
    }
    if anchor_required || !v.anchor_local.is_empty() {
        let Some(anchor) = local(&v.anchor_local) else {
            return false;
        };
        if (anchor.and_utc().timestamp() - start.timestamp()).abs() > 26 * 3600 {
            return false;
        }
    }
    true
}
fn span(v: &wire::Span, allow_point: bool) -> Option<TemporalKind> {
    if !v.__buffa_unknown_fields.is_empty() {
        return None;
    }
    match &v.value {
        Some(wire::span::Value::Dates(value)) => dates(value).then_some(TemporalKind::Civil),
        Some(wire::span::Value::Times(value)) => {
            times(value, allow_point, true).then_some(TemporalKind::Timed)
        }
        None => None,
    }
}
fn exception(v: &wire::Exception, kind: TemporalKind) -> bool {
    if !v.__buffa_unknown_fields.is_empty() || point(&v.original) != Some(kind) {
        return false;
    }
    match &v.change {
        Some(wire::exception::Change::Cancel(empty)) => {
            empty.__buffa_unknown_fields.is_empty()
                && v.replacement_summary.is_none()
                && v.replacement_description.is_none()
                && v.replacement_location.is_none()
        }
        Some(wire::exception::Change::Replacement(replacement)) => {
            span(replacement, true) == Some(kind)
                && v.replacement_summary
                    .as_deref()
                    .is_none_or(|s| text(s, 512, false))
                && v.replacement_description
                    .as_deref()
                    .is_none_or(|s| text(s, 4096, false))
                && v.replacement_location
                    .as_deref()
                    .is_none_or(|s| text(s, 512, false))
        }
        None => false,
    }
}
fn event(v: &wire::Event, allow_point: bool) -> bool {
    if !v.__buffa_unknown_fields.is_empty()
        || !text(&v.summary, 512, false)
        || !text(&v.description, 4096, false)
        || !text(&v.location, 512, false)
        || v.additions.len() > 256
        || v.exclusions.len() > 256
        || v.exceptions.len() > 256
        || !v
            .recurrence_rule
            .as_deref()
            .is_none_or(|r| text(r, 4096, true))
    {
        return false;
    }
    let Some(kind) = v.span.as_option().and_then(|s| span(s, allow_point)) else {
        return false;
    };
    v.additions
        .iter()
        .chain(&v.exclusions)
        .all(|p| point(p) == Some(kind))
        && v.exceptions.iter().all(|e| exception(e, kind))
}
pub fn admit_event(
    v: &wire::EventSnapshot,
    expected: &EventKey,
) -> Result<Admitted<wire::EventSnapshot>, AdmissionError> {
    if v.key.as_option().and_then(event_key).as_ref() != Some(expected) {
        return Err(AdmissionError::ForeignResource);
    }
    let revision = v
        .revision
        .as_option()
        .and_then(|r| event_revision(r, expected))
        .ok_or(AdmissionError::Invalid)?;
    let calendar = v
        .calendar_revision
        .as_option()
        .and_then(|r| calendar_revision(r, expected.calendar()))
        .ok_or(AdmissionError::Invalid)?;
    let value = match revision.state() {
        RevisionState::Present(_) => v.value.as_option().is_some_and(|value| event(value, true)),
        RevisionState::Absent | RevisionState::Removed(_) => v.value.as_option().is_none(),
    };
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && value
            && matches!(calendar.state(), RevisionState::Present(_)),
    )
}
pub fn admit_query(
    v: &wire::QueryOccurrencesRequest,
) -> Result<Admitted<wire::QueryOccurrencesRequest>, AdmissionError> {
    let valid_window = v.window.as_option().is_some_and(|w| {
        w.__buffa_unknown_fields.is_empty()
            && zone(&w.zone)
            && match &w.bounds {
                Some(wire::window::Bounds::Dates(d)) => dates(d),
                Some(wire::window::Bounds::Times(t)) => times(t, false, false) && t.zone == w.zone,
                None => false,
            }
    });
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && v.id.as_option().and_then(calendar_id).is_some()
            && valid_window
            && (1..=1000).contains(&v.page_size)
            && v.page_token.len() <= 5600,
    )
}
pub fn admit_page(
    v: &wire::QueryOccurrencesResponse,
    request: &Admitted<wire::QueryOccurrencesRequest>,
    previous: Option<&CalendarRevision>,
) -> Result<Admitted<wire::QueryOccurrencesResponse>, AdmissionError> {
    let id = calendar_id(&request.wire.id).ok_or(AdmissionError::Invalid)?;
    let revision = v
        .revision
        .as_option()
        .and_then(|r| calendar_revision(r, &id))
        .ok_or(AdmissionError::ForeignResource)?;
    if previous.is_some_and(|old| old != &revision) {
        return Err(AdmissionError::StaleRevision);
    }
    if v.occurrences.len() > request.wire.page_size as usize
        || v.series_exceptions.len() > 1000
        || v.next_page_token.len() > 5600
    {
        return Err(AdmissionError::Capacity);
    }
    let mut keys = BTreeSet::new();
    for occurrence in &v.occurrences {
        let key = occurrence
            .key
            .as_option()
            .and_then(event_key)
            .ok_or(AdmissionError::Invalid)?;
        if key.calendar() != &id {
            return Err(AdmissionError::ForeignResource);
        }
        let kind = occurrence.span.as_option().and_then(|s| span(s, true));
        if !occurrence.__buffa_unknown_fields.is_empty()
            || kind.is_none()
            || point(&occurrence.recurrence_id) != kind
            || !zone(&occurrence.zone)
            || !text(&occurrence.summary, 512, false)
            || !text(&occurrence.description, 4096, false)
            || !text(&occurrence.location, 512, false)
            || !keys.insert((key, occurrence.recurrence_id.clone()))
        {
            return Err(AdmissionError::Invalid);
        }
    }
    let mut series = BTreeSet::new();
    for group in &v.series_exceptions {
        let key = group
            .key
            .as_option()
            .and_then(event_key)
            .ok_or(AdmissionError::Invalid)?;
        if key.calendar() != &id {
            return Err(AdmissionError::ForeignResource);
        }
        if !group.__buffa_unknown_fields.is_empty()
            || group.exceptions.len() > 256
            || group.exclusions.len() > 256
            || !series.insert(key)
            || !group.exclusions.iter().all(|p| point(p).is_some())
            || !group
                .exceptions
                .iter()
                .all(|e| point(&e.original).is_some_and(|kind| exception(e, kind)))
        {
            return Err(AdmissionError::Invalid);
        }
    }
    admit(
        v,
        v.__buffa_unknown_fields.is_empty()
            && matches!(revision.state(), RevisionState::Present(_))
            && quality(v.quality)
            && provenance(v.provenance)
            && v.fresh_until
                .as_deref()
                .is_none_or(|s| stamp(s, false).is_some()),
    )
}
pub fn admit_resolution(
    v: &wire::ResolveLocalTimeResponse,
    request: &wire::ResolveLocalTimeRequest,
) -> Result<Admitted<wire::ResolveLocalTimeResponse>, AdmissionError> {
    let id = request
        .id
        .as_option()
        .and_then(calendar_id)
        .ok_or(AdmissionError::Invalid)?;
    let wall = local(&request.local).ok_or(AdmissionError::Invalid)?;
    if !request.__buffa_unknown_fields.is_empty()
        || !zone(&request.zone)
        || v.id.as_option().and_then(calendar_id).as_ref() != Some(&id)
        || v.local != request.local
        || v.zone != request.zone
    {
        return Err(AdmissionError::ForeignResource);
    }
    let expected = match v.resolution {
        EnumValue::Known(wire::LocalTimeResolution::Unique) => 1,
        EnumValue::Known(wire::LocalTimeResolution::Ambiguous) => 2,
        EnumValue::Known(
            wire::LocalTimeResolution::Nonexistent | wire::LocalTimeResolution::InvalidZone,
        ) => 0,
        EnumValue::Known(wire::LocalTimeResolution::Unspecified) | EnumValue::Unknown(_) => {
            return Err(AdmissionError::Invalid);
        }
    };
    if v.candidates.len() != expected {
        return Err(AdmissionError::Invalid);
    }
    let mut previous = None;
    for candidate in &v.candidates {
        let at = stamp(&candidate.instant, true).ok_or(AdmissionError::Invalid)?;
        if !candidate.__buffa_unknown_fields.is_empty()
            || i64::from(candidate.offset_seconds).abs() > 26 * 3600
            || wall.and_utc().timestamp() - at.timestamp() != i64::from(candidate.offset_seconds)
            || previous.is_some_and(|old| old >= at)
        {
            return Err(AdmissionError::Invalid);
        }
        previous = Some(at);
    }
    admit(v, v.__buffa_unknown_fields.is_empty())
}
pub fn admit_watch(
    v: &wire::WatchCalendarResponse,
    id: &CalendarId,
    previous: u64,
) -> Result<Admitted<wire::WatchCalendarResponse>, AdmissionError> {
    if previous.checked_add(1) != Some(v.stream_sequence) {
        return Err(AdmissionError::Sequence);
    }
    let frame = match &v.frame {
        Some(wire::watch_calendar_response::Frame::Ready(empty)) => {
            previous == 0 && empty.__buffa_unknown_fields.is_empty()
        }
        Some(wire::watch_calendar_response::Frame::ResyncRequired(empty)) => {
            empty.__buffa_unknown_fields.is_empty()
        }
        Some(wire::watch_calendar_response::Frame::Changed(change)) => {
            previous > 0
                && change.__buffa_unknown_fields.is_empty()
                && change
                    .revision
                    .as_option()
                    .and_then(|r| calendar_revision(r, id))
                    .is_some()
        }
        None => false,
    };
    admit(v, v.__buffa_unknown_fields.is_empty() && frame)
}
