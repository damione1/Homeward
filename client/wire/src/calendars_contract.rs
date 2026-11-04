//! Current CalendarService resource boundary. Civil values stay civil; only the
//! server resolves wall times. Unknown fields/enums retire protected captures.
use crate::home::api::v2 as wire;
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use chrono::{DateTime, Utc};
use home_client_model::{
    api::{ClockTime, EntityName, HomeName},
    api_calendars::{
        CalendarCivil, CalendarEtag, CalendarEventName, CalendarEventVersion, CalendarName,
        CalendarPoint, CalendarPointKind, CalendarSpan, CalendarTimedSpan, CalendarVersion,
        CalendarZone,
    },
    calendars::CivilDate,
    local_time::UtcInstant,
};
use std::collections::BTreeSet;

pub const MAX_EVENT_SUMMARY_BYTES: usize = 512;
pub const MAX_EVENT_DESCRIPTION_BYTES: usize = 4096;
pub const MAX_EVENT_LOCATION_BYTES: usize = 512;
pub const MAX_ZONE_BYTES: usize = 512;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AdmissionError {
    Invalid,
    ForeignResource,
    Capacity,
    StaleVersion,
    Sequence,
    ReadOnly,
}
#[derive(Clone)]
pub struct Admitted<T> {
    wire: T,
}
impl<T> Admitted<T> {
    pub fn wire(&self) -> &T {
        &self.wire
    }
}
fn admitted<T: Clone>(value: &T) -> Admitted<T> {
    Admitted {
        wire: value.clone(),
    }
}
fn invalid<E>(_: E) -> AdmissionError {
    AdmissionError::Invalid
}
fn text(value: &str, max: usize, required: bool) -> bool {
    (!required || !value.is_empty()) && value.len() <= max && !value.contains('\0')
}
fn zone(value: &str) -> bool {
    CalendarZone::try_from(value).is_ok()
}

pub fn timestamp(value: &Timestamp) -> Result<UtcInstant, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || (value.seconds == -62_135_596_800 && value.nanos == 0)
    {
        return Err(AdmissionError::Invalid);
    }
    UtcInstant::new(value.seconds, value.nanos).map_err(invalid)
}
pub fn timestamp_text(value: &Timestamp) -> Result<String, AdmissionError> {
    let time = timestamp(value)?;
    DateTime::<Utc>::from_timestamp(time.seconds(), time.nanos() as u32)
        .map(|time| time.to_rfc3339_opts(chrono::SecondsFormat::AutoSi, true))
        .ok_or(AdmissionError::Invalid)
}
pub fn parse_timestamp(value: &str, whole: bool) -> Result<Timestamp, AdmissionError> {
    if value.len() > 30 || !value.ends_with('Z') {
        return Err(AdmissionError::Invalid);
    }
    let time = DateTime::parse_from_rfc3339(value)
        .map_err(invalid)?
        .with_timezone(&Utc);
    let out = Timestamp {
        seconds: time.timestamp(),
        nanos: time.timestamp_subsec_nanos() as i32,
        ..Default::default()
    };
    timestamp(&out)?;
    if whole && out.nanos != 0 {
        return Err(AdmissionError::Invalid);
    }
    Ok(out)
}
pub fn date(value: &crate::google::r#type::Date) -> Result<CivilDate, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || !(1..=9999).contains(&value.year)
        || !(1..=12).contains(&value.month)
        || !(1..=31).contains(&value.day)
    {
        return Err(AdmissionError::Invalid);
    }
    format!("{:04}-{:02}-{:02}", value.year, value.month, value.day)
        .parse()
        .map_err(invalid)
}
pub fn date_wire(value: CivilDate) -> crate::google::r#type::Date {
    let (y, m, d) = value.components();
    crate::google::r#type::Date {
        year: y,
        month: m as i32,
        day: d as i32,
        ..Default::default()
    }
}
pub fn civil(value: &wire::CalendarCivilTime) -> Result<CalendarCivil, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    let date = date(value.date.as_option().ok_or(AdmissionError::Invalid)?)?;
    let clock = value.time.as_option().ok_or(AdmissionError::Invalid)?;
    if !clock.__buffa_unknown_fields.is_empty() || clock.nanos != 0 {
        return Err(AdmissionError::Invalid);
    }
    Ok(CalendarCivil::new(
        date,
        ClockTime::new(clock.hours, clock.minutes, clock.seconds).map_err(invalid)?,
    ))
}
pub fn civil_wire(value: CalendarCivil) -> wire::CalendarCivilTime {
    let (h, m, s) = value.clock().components();
    wire::CalendarCivilTime {
        date: date_wire(value.date()).into(),
        time: crate::google::r#type::TimeOfDay {
            hours: h,
            minutes: m,
            seconds: s,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
pub fn civil_text(value: &wire::CalendarCivilTime) -> Result<String, AdmissionError> {
    Ok(civil(value)?.to_string())
}
pub fn parse_civil(value: &str) -> Result<wire::CalendarCivilTime, AdmissionError> {
    if value.len() != 19 || value.as_bytes()[10] != b'T' {
        return Err(AdmissionError::Invalid);
    }
    let date: CivilDate = value[..10].parse().map_err(invalid)?;
    let parsed = chrono::NaiveTime::parse_from_str(&value[11..], "%H:%M:%S").map_err(invalid)?;
    use chrono::Timelike;
    let clock = ClockTime::new(
        parsed.hour() as i32,
        parsed.minute() as i32,
        parsed.second() as i32,
    )
    .map_err(invalid)?;
    let out = civil_wire(CalendarCivil::new(date, clock));
    if civil_text(&out)? != value {
        return Err(AdmissionError::Invalid);
    }
    Ok(out)
}
pub fn span(value: &wire::CalendarSpan) -> Result<CalendarSpan, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    match &value.value {
        Some(wire::calendar_span::Value::Dates(dates)) => {
            if !dates.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            CalendarSpan::dates(
                date(
                    dates
                        .start_date
                        .as_option()
                        .ok_or(AdmissionError::Invalid)?,
                )?,
                date(dates.end_date.as_option().ok_or(AdmissionError::Invalid)?)?,
            )
            .map_err(invalid)
        }
        Some(wire::calendar_span::Value::Times(times)) => {
            if !times.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            let start = timestamp(
                times
                    .start_time
                    .as_option()
                    .ok_or(AdmissionError::Invalid)?,
            )?;
            let end = timestamp(times.end_time.as_option().ok_or(AdmissionError::Invalid)?)?;
            let start_local = civil(
                times
                    .start_local
                    .as_option()
                    .ok_or(AdmissionError::Invalid)?,
            )?;
            let end_local = civil(times.end_local.as_option().ok_or(AdmissionError::Invalid)?)?;
            let zone = CalendarZone::try_from(times.time_zone.as_str()).map_err(invalid)?;
            Ok(CalendarSpan::times(
                CalendarTimedSpan::new(start_local, end_local, start, end, zone)
                    .map_err(invalid)?,
            ))
        }
        None => Err(AdmissionError::Invalid),
    }
}
pub fn point(value: &wire::CalendarRecurrencePoint) -> Result<CalendarPoint, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    match &value.value {
        Some(wire::calendar_recurrence_point::Value::Date(value)) => {
            Ok(CalendarPoint::date(date(value)?))
        }
        Some(wire::calendar_recurrence_point::Value::Local(value)) => {
            Ok(CalendarPoint::local(civil(value)?))
        }
        Some(wire::calendar_recurrence_point::Value::Resolved(value)) => {
            if !value.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            CalendarPoint::resolved(
                civil(value.local.as_option().ok_or(AdmissionError::Invalid)?)?,
                timestamp(
                    value
                        .selected_time
                        .as_option()
                        .ok_or(AdmissionError::Invalid)?,
                )?,
            )
            .map_err(invalid)
        }
        None => Err(AdmissionError::Invalid),
    }
}
pub fn point_key(value: &wire::CalendarRecurrencePoint) -> Result<String, AdmissionError> {
    Ok(point(value)?.as_str().to_owned())
}
fn point_matches(point: &CalendarPoint, span: &CalendarSpan) -> bool {
    matches!(
        (point.kind(), span.date_bounds().is_some()),
        (CalendarPointKind::Date, true)
            | (
                CalendarPointKind::Local | CalendarPointKind::Resolved,
                false
            )
    )
}
fn exception_shape(
    value: &wire::CalendarException,
    local: bool,
) -> Result<CalendarPoint, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    let original = point(value.original.as_option().ok_or(AdmissionError::Invalid)?)?;
    if local && original.kind() == CalendarPointKind::Resolved {
        return Err(AdmissionError::Invalid);
    }
    match &value.change {
        Some(wire::calendar_exception::Change::Cancel(empty)) => {
            if !empty.__buffa_unknown_fields.is_empty()
                || value.replacement_summary.is_some()
                || value.replacement_description.is_some()
                || value.replacement_location.is_some()
            {
                return Err(AdmissionError::Invalid);
            }
        }
        Some(wire::calendar_exception::Change::Replacement(replacement)) => {
            let replacement = span(replacement)?;
            if !point_matches(&original, &replacement) || local && !replacement.valid_local_input()
            {
                return Err(AdmissionError::Invalid);
            }
        }
        None => return Err(AdmissionError::Invalid),
    }
    if value
        .replacement_summary
        .as_deref()
        .is_some_and(|v| !text(v, 512, false))
        || value
            .replacement_description
            .as_deref()
            .is_some_and(|v| !text(v, 4096, false))
        || value
            .replacement_location
            .as_deref()
            .is_some_and(|v| !text(v, 512, false))
    {
        return Err(AdmissionError::Invalid);
    }
    if local
        && (value.replacement_summary.is_some()
            || value.replacement_description.is_some()
            || value.replacement_location.is_some())
    {
        return Err(AdmissionError::ReadOnly);
    }
    Ok(original)
}
fn exception(
    value: &wire::CalendarException,
    kind: &CalendarSpan,
    local: bool,
) -> Result<(), AdmissionError> {
    let original = exception_shape(value, local)?;
    if !point_matches(&original, kind) {
        return Err(AdmissionError::Invalid);
    }
    Ok(())
}

pub fn event_content(value: &wire::CalendarEvent, local: bool) -> Result<(), AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || !text(&value.summary, 512, local)
        || !text(&value.description, 4096, false)
        || !text(&value.location, 512, false)
        || value
            .recurrence_rule
            .as_deref()
            .is_some_and(|v| !text(v, 1024, true))
    {
        return Err(AdmissionError::Invalid);
    }
    let kind = span(value.span.as_option().ok_or(AdmissionError::Invalid)?)?;
    if local && !kind.valid_local_input() {
        return Err(AdmissionError::ReadOnly);
    }
    if value.additions.len() > 256 || value.exclusions.len() > 256 || value.exceptions.len() > 256 {
        return Err(AdmissionError::Capacity);
    }
    for values in [&value.additions, &value.exclusions] {
        let mut seen = BTreeSet::new();
        for value in values {
            let parsed = point(value)?;
            if !point_matches(&parsed, &kind)
                || local && parsed.kind() == CalendarPointKind::Resolved
                || !seen.insert(parsed.as_str().to_owned())
            {
                return Err(AdmissionError::Invalid);
            }
        }
    }
    let excluded: Result<BTreeSet<_>, _> = value.exclusions.iter().map(point_key).collect();
    let excluded = excluded?;
    let mut seen = BTreeSet::new();
    for value in &value.exceptions {
        exception(value, &kind, local)?;
        let key = point_key(value.original.as_option().ok_or(AdmissionError::Invalid)?)?;
        if excluded.contains(&key) || !seen.insert(key) {
            return Err(AdmissionError::Invalid);
        }
    }
    Ok(())
}
fn origin(value: EnumValue<wire::CalendarOrigin>) -> Result<(), AdmissionError> {
    match value {
        EnumValue::Known(
            wire::CalendarOrigin::Local
            | wire::CalendarOrigin::RemoteIcs
            | wire::CalendarOrigin::Calculated,
        ) => Ok(()),
        EnumValue::Known(wire::CalendarOrigin::Unspecified) | EnumValue::Unknown(_) => {
            Err(AdmissionError::Invalid)
        }
    }
}
fn provenance(value: EnumValue<wire::CalendarProvenance>) -> Result<(), AdmissionError> {
    match value {
        EnumValue::Known(
            wire::CalendarProvenance::Observed | wire::CalendarProvenance::Synthetic,
        ) => Ok(()),
        EnumValue::Known(wire::CalendarProvenance::Unspecified) | EnumValue::Unknown(_) => {
            Err(AdmissionError::Invalid)
        }
    }
}
fn freshness(value: EnumValue<wire::CalendarFreshness>) -> Result<(), AdmissionError> {
    match value {
        EnumValue::Known(
            wire::CalendarFreshness::Fresh
            | wire::CalendarFreshness::Stale
            | wire::CalendarFreshness::Unavailable,
        ) => Ok(()),
        EnumValue::Known(wire::CalendarFreshness::Unspecified) | EnumValue::Unknown(_) => {
            Err(AdmissionError::Invalid)
        }
    }
}
fn access(value: EnumValue<wire::CalendarMutationAccess>) -> Result<(), AdmissionError> {
    match value {
        EnumValue::Known(
            wire::CalendarMutationAccess::Denied | wire::CalendarMutationAccess::Allowed,
        ) => Ok(()),
        EnumValue::Known(wire::CalendarMutationAccess::Unspecified) | EnumValue::Unknown(_) => {
            Err(AdmissionError::Invalid)
        }
    }
}
pub fn calendar_version(value: &wire::Calendar) -> Result<CalendarVersion, AdmissionError> {
    Ok(CalendarVersion::new(
        CalendarName::try_from(value.name.as_str()).map_err(invalid)?,
        CalendarEtag::try_from(value.etag.as_str()).map_err(invalid)?,
    ))
}
pub fn event_version(value: &wire::CalendarEvent) -> Result<CalendarEventVersion, AdmissionError> {
    Ok(CalendarEventVersion::new(
        CalendarEventName::try_from(value.name.as_str()).map_err(invalid)?,
        CalendarEtag::try_from(value.etag.as_str()).map_err(invalid)?,
        CalendarEtag::try_from(value.calendar_etag.as_str()).map_err(invalid)?,
    ))
}
pub fn admit_source(
    value: &wire::CalendarSource,
    home: &HomeName,
) -> Result<Admitted<wire::CalendarSource>, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() || !text(&value.display_name, 512, false) {
        return Err(AdmissionError::Invalid);
    }
    let calendar = CalendarName::try_from(value.calendar.as_str()).map_err(invalid)?;
    let source = EntityName::try_from(value.source_entity.as_str()).map_err(invalid)?;
    if calendar.home() != home
        || !source
            .as_str()
            .starts_with(&format!("{}/entities/", home.as_str()))
    {
        return Err(AdmissionError::ForeignResource);
    }
    origin(value.origin)?;
    provenance(value.provenance)?;
    access(value.mutation_access)?;
    if value.origin != wire::CalendarOrigin::Local
        && value.mutation_access == wire::CalendarMutationAccess::Allowed
    {
        return Err(AdmissionError::Invalid);
    }
    Ok(admitted(value))
}
pub fn admit_sources(
    value: &wire::QueryCalendarSourcesResponse,
    home: &HomeName,
) -> Result<Admitted<wire::QueryCalendarSourcesResponse>, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || value.calendar_sources.len() > 256
        || value.next_page_token.len() > 5600
        || value.encoded_len() > 1 << 20
    {
        return Err(AdmissionError::Capacity);
    }
    let mut names = BTreeSet::new();
    for source in &value.calendar_sources {
        admit_source(source, home)?;
        if !names.insert(source.calendar.as_str()) {
            return Err(AdmissionError::Invalid);
        }
    }
    Ok(admitted(value))
}
pub fn admit_calendar(
    value: &wire::Calendar,
    expected: &CalendarName,
) -> Result<Admitted<wire::Calendar>, AdmissionError> {
    let version = calendar_version(value)?;
    if version.name() != expected {
        return Err(AdmissionError::ForeignResource);
    }
    if !value.__buffa_unknown_fields.is_empty()
        || !text(&value.display_name, 512, false)
        || value.encoded_len() > 8192
    {
        return Err(AdmissionError::Invalid);
    }
    let source = EntityName::try_from(value.source_entity.as_str()).map_err(invalid)?;
    if !source
        .as_str()
        .starts_with(&format!("{}/entities/", expected.home().as_str()))
    {
        return Err(AdmissionError::ForeignResource);
    }
    origin(value.origin)?;
    freshness(value.freshness)?;
    provenance(value.provenance)?;
    access(value.mutation_access)?;
    if value.origin != wire::CalendarOrigin::Local
        && value.mutation_access == wire::CalendarMutationAccess::Allowed
    {
        return Err(AdmissionError::Invalid);
    }
    if let Some(time) = value.update_time.as_option() {
        timestamp(time)?;
    }
    if let Some(time) = value.fresh_until_time.as_option() {
        timestamp(time)?;
    }
    Ok(admitted(value))
}
pub fn calendar_editable(value: &Admitted<wire::Calendar>) -> bool {
    value.wire.origin == wire::CalendarOrigin::Local
        && value.wire.mutation_access == wire::CalendarMutationAccess::Allowed
}
pub fn admit_event(
    value: &wire::CalendarEvent,
    expected: &CalendarEventName,
) -> Result<Admitted<wire::CalendarEvent>, AdmissionError> {
    if event_version(value)?.name() != expected {
        return Err(AdmissionError::ForeignResource);
    }
    event_content(value, false)?;
    if let Some(time) = value.update_time.as_option() {
        timestamp(time)?;
    }
    Ok(admitted(value))
}
pub fn query_parent(
    value: &str,
) -> Result<(CalendarName, Option<CalendarEventName>), AdmissionError> {
    if let Some(parent) = value.strip_suffix("/events/-") {
        return Ok((CalendarName::try_from(parent).map_err(invalid)?, None));
    }
    let event = CalendarEventName::try_from(value).map_err(invalid)?;
    Ok((event.parent().clone(), Some(event)))
}
pub fn admit_query(
    value: &wire::QueryOccurrencesRequest,
) -> Result<Admitted<wire::QueryOccurrencesRequest>, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || value.page_size < 0
        || value.page_token.len() > 5600
    {
        return Err(AdmissionError::Invalid);
    }
    query_parent(&value.parent)?;
    let window = value.window.as_option().ok_or(AdmissionError::Invalid)?;
    if !window.__buffa_unknown_fields.is_empty() || !zone(&window.time_zone) {
        return Err(AdmissionError::Invalid);
    }
    match &window.bounds {
        Some(wire::calendar_window::Bounds::Dates(value)) => {
            if !value.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            CalendarSpan::dates(
                date(
                    value
                        .start_date
                        .as_option()
                        .ok_or(AdmissionError::Invalid)?,
                )?,
                date(value.end_date.as_option().ok_or(AdmissionError::Invalid)?)?,
            )
            .map_err(invalid)?;
        }
        Some(wire::calendar_window::Bounds::Times(value)) => {
            if !value.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            let a = timestamp(
                value
                    .start_time
                    .as_option()
                    .ok_or(AdmissionError::Invalid)?,
            )?;
            let b = timestamp(value.end_time.as_option().ok_or(AdmissionError::Invalid)?)?;
            if a.nanos() != 0 || b.nanos() != 0 || b <= a || b.seconds() - a.seconds() > 366 * 86400
            {
                return Err(AdmissionError::Invalid);
            }
        }
        None => return Err(AdmissionError::Invalid),
    };
    Ok(admitted(value))
}
pub fn admit_page(
    value: &wire::QueryOccurrencesResponse,
    request: &Admitted<wire::QueryOccurrencesRequest>,
    previous: &CalendarVersion,
) -> Result<Admitted<wire::QueryOccurrencesResponse>, AdmissionError> {
    let (parent, selected) = query_parent(&request.wire.parent)?;
    let etag = CalendarEtag::try_from(value.calendar_etag.as_str()).map_err(invalid)?;
    if previous.name() != &parent || previous.etag() != &etag {
        return Err(AdmissionError::StaleVersion);
    }
    let limit = if request.wire.page_size == 0 {
        100
    } else {
        request.wire.page_size.min(256) as usize
    };
    if !value.__buffa_unknown_fields.is_empty()
        || value.occurrences.len() > limit
        || value.series_exceptions.len() > 1024
        || value.next_page_token.len() > 5600
        || value.encoded_len() > 1 << 20
    {
        return Err(AdmissionError::Capacity);
    }
    freshness(value.freshness)?;
    provenance(value.provenance)?;
    if let Some(time) = value.fresh_until_time.as_option() {
        timestamp(time)?;
    }
    let mut seen = BTreeSet::new();
    for occurrence in &value.occurrences {
        let event =
            CalendarEventName::try_from(occurrence.calendar_event.as_str()).map_err(invalid)?;
        if event.parent() != &parent || selected.as_ref().is_some_and(|wanted| wanted != &event) {
            return Err(AdmissionError::ForeignResource);
        }
        let span = span(occurrence.span.as_option().ok_or(AdmissionError::Invalid)?)?;
        let point = point(
            occurrence
                .recurrence_point
                .as_option()
                .ok_or(AdmissionError::Invalid)?,
        )?;
        if !occurrence.__buffa_unknown_fields.is_empty()
            || !point_matches(&point, &span)
            || !zone(&occurrence.time_zone)
            || occurrence.time_zone != request.wire.window.time_zone
            || !text(&occurrence.summary, 512, false)
            || !text(&occurrence.description, 4096, false)
            || !text(&occurrence.location, 512, false)
            || !seen.insert((event, point.as_str().to_owned()))
        {
            return Err(AdmissionError::Invalid);
        }
    }
    let mut groups = BTreeSet::new();
    for group in &value.series_exceptions {
        let event = CalendarEventName::try_from(group.calendar_event.as_str()).map_err(invalid)?;
        if event.parent() != &parent || selected.as_ref().is_some_and(|wanted| wanted != &event) {
            return Err(AdmissionError::ForeignResource);
        }
        if !group.__buffa_unknown_fields.is_empty()
            || group.exceptions.len() > 256
            || group.exclusions.len() > 256
            || !groups.insert(event)
        {
            return Err(AdmissionError::Invalid);
        }
        let mut exclusions = BTreeSet::new();
        for value in &group.exclusions {
            if !exclusions.insert(point_key(value)?) {
                return Err(AdmissionError::Invalid);
            }
        }
        let mut exceptions = BTreeSet::new();
        for value in &group.exceptions {
            let original = exception_shape(value, false)?;
            if !exceptions.insert(original.as_str().to_owned()) {
                return Err(AdmissionError::Invalid);
            }
        }
    }
    Ok(admitted(value))
}
pub fn admit_resolution_request(
    value: &wire::ResolveCalendarTimeRequest,
) -> Result<Admitted<wire::ResolveCalendarTimeRequest>, AdmissionError> {
    CalendarName::try_from(value.name.as_str()).map_err(invalid)?;
    civil(value.local.as_option().ok_or(AdmissionError::Invalid)?)?;
    if !value.__buffa_unknown_fields.is_empty() || !zone(&value.time_zone) {
        return Err(AdmissionError::Invalid);
    }
    Ok(admitted(value))
}
pub fn admit_resolution(
    value: &wire::ResolveCalendarTimeResponse,
    request: &wire::ResolveCalendarTimeRequest,
) -> Result<Admitted<wire::ResolveCalendarTimeResponse>, AdmissionError> {
    admit_resolution_request(request)?;
    if !value.__buffa_unknown_fields.is_empty()
        || value.local != request.local
        || value.time_zone != request.time_zone
    {
        return Err(AdmissionError::ForeignResource);
    }
    let local = civil(value.local.as_option().ok_or(AdmissionError::Invalid)?)?;
    let count = match value.resolution {
        EnumValue::Known(wire::CalendarTimeResolution::Unique) => 1,
        EnumValue::Known(wire::CalendarTimeResolution::Ambiguous) => 2,
        EnumValue::Known(
            wire::CalendarTimeResolution::Nonexistent | wire::CalendarTimeResolution::InvalidZone,
        ) => 0,
        EnumValue::Known(wire::CalendarTimeResolution::Unspecified) | EnumValue::Unknown(_) => {
            return Err(AdmissionError::Invalid);
        }
    };
    if value.candidates.len() != count {
        return Err(AdmissionError::Invalid);
    }
    let mut previous = None;
    for candidate in &value.candidates {
        if !candidate.__buffa_unknown_fields.is_empty() {
            return Err(AdmissionError::Invalid);
        }
        let selected = timestamp(
            candidate
                .selected_time
                .as_option()
                .ok_or(AdmissionError::Invalid)?,
        )?;
        if selected.nanos() != 0
            || (local.naive_seconds().map_err(invalid)? - selected.seconds())
                != i64::from(candidate.utc_offset)
            || i64::from(candidate.utc_offset).abs() > 26 * 3600
            || previous.is_some_and(|previous| selected <= previous)
        {
            return Err(AdmissionError::Invalid);
        }
        previous = Some(selected);
    }
    Ok(admitted(value))
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WatchFrame {
    Ready,
    Changed(CalendarEtag),
    Resync,
}
pub fn admit_watch(
    value: &wire::WatchCalendarResponse,
) -> Result<(u64, WatchFrame), AdmissionError> {
    let sequence = u64::try_from(value.sequence).map_err(invalid)?;
    if sequence == 0 || !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Sequence);
    }
    use wire::watch_calendar_response::Frame;
    let frame = match &value.frame {
        Some(Frame::Ready(empty)) if empty.__buffa_unknown_fields.is_empty() => WatchFrame::Ready,
        Some(Frame::Changed(value)) if value.__buffa_unknown_fields.is_empty() => {
            WatchFrame::Changed(CalendarEtag::try_from(value.etag.as_str()).map_err(invalid)?)
        }
        Some(Frame::ResyncRequired(empty)) if empty.__buffa_unknown_fields.is_empty() => {
            WatchFrame::Resync
        }
        Some(Frame::Ready(_) | Frame::Changed(_) | Frame::ResyncRequired(_)) | None => {
            return Err(AdmissionError::Invalid);
        }
    };
    Ok((sequence, frame))
}
