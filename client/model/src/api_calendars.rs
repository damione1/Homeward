//! Current calendar resource identities and temporal values. No protobuf, client
//! timezone resolver, numeric revision decoder or implicit timezone lives here.
use crate::{
    api::{ClockTime, HomeName},
    calendars::{CalendarId, CivilDate, EventKey, EventUid},
    local_time::UtcInstant,
};
use chrono::{NaiveDate, TimeZone, Utc};
use std::fmt;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CalendarError {
    Name,
    Etag,
    Date,
    Clock,
    Instant,
    Zone,
    Span,
    Content,
    Scope,
}
fn native_segment(value: &str) -> Result<String, CalendarError> {
    if value.is_empty()
        || value.len() > 255
        || value.bytes().any(|b| matches!(b, b'\0' | b'\r' | b'\n'))
    {
        return Err(CalendarError::Name);
    }
    const HEX: &[u8] = b"0123456789abcdef";
    let mut result = String::with_capacity(3 + value.len() * 2);
    result.push_str("id-");
    for byte in value.bytes() {
        result.push(char::from(HEX[usize::from(byte >> 4)]));
        result.push(char::from(HEX[usize::from(byte & 15)]));
    }
    Ok(result)
}
fn decode_segment(value: &str) -> Result<String, CalendarError> {
    let tail = value.strip_prefix("id-").ok_or(CalendarError::Name)?;
    if tail.is_empty() || tail.len() > 510 || tail.len() % 2 != 0 {
        return Err(CalendarError::Name);
    }
    fn digit(b: u8) -> Result<u8, CalendarError> {
        match b {
            b'0'..=b'9' => Ok(b - b'0'),
            b'a'..=b'f' => Ok(b - b'a' + 10),
            _ => Err(CalendarError::Name),
        }
    }
    let bytes: Result<Vec<_>, _> = tail
        .as_bytes()
        .chunks_exact(2)
        .map(|p| Ok(digit(p[0])? * 16 + digit(p[1])?))
        .collect();
    let decoded = String::from_utf8(bytes?).map_err(|_| CalendarError::Name)?;
    if native_segment(&decoded)? != value {
        return Err(CalendarError::Name);
    }
    Ok(decoded)
}
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct CalendarName {
    value: String,
    home: HomeName,
    native: CalendarId,
}
impl CalendarName {
    pub fn new(home: HomeName, native: CalendarId) -> Result<Self, CalendarError> {
        let value = format!(
            "{}/calendars/{}",
            home.as_str(),
            native_segment(native.entity().as_str())?
        );
        Ok(Self {
            value,
            home,
            native,
        })
    }
    pub fn as_str(&self) -> &str {
        &self.value
    }
    pub fn home(&self) -> &HomeName {
        &self.home
    }
    pub fn native_id(&self) -> &CalendarId {
        &self.native
    }
}
impl TryFrom<&str> for CalendarName {
    type Error = CalendarError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.len() > 700 {
            return Err(CalendarError::Name);
        }
        let parts: Vec<_> = value.split('/').collect();
        if parts.len() != 4 || parts[2] != "calendars" {
            return Err(CalendarError::Name);
        }
        let home = HomeName::try_from(format!("{}/{}", parts[0], parts[1]).as_str())
            .map_err(|_| CalendarError::Name)?;
        let native = CalendarId::new(
            decode_segment(parts[3])?
                .parse()
                .map_err(|_| CalendarError::Name)?,
        );
        let result = Self::new(home, native)?;
        if result.as_str() != value {
            return Err(CalendarError::Name);
        }
        Ok(result)
    }
}
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct CalendarEventName {
    value: String,
    parent: CalendarName,
    uid: EventUid,
}
impl CalendarEventName {
    pub fn new(parent: CalendarName, uid: EventUid) -> Self {
        // EventUid already proves the same native 255-byte/control contract.
        let mut segment = String::from("id-");
        for byte in uid.as_str().bytes() {
            use std::fmt::Write;
            let _ = write!(segment, "{byte:02x}");
        }
        Self {
            value: format!("{}/events/{segment}", parent.as_str()),
            parent,
            uid,
        }
    }
    pub fn as_str(&self) -> &str {
        &self.value
    }
    pub fn parent(&self) -> &CalendarName {
        &self.parent
    }
    pub fn native_uid(&self) -> &EventUid {
        &self.uid
    }
    pub fn key(&self) -> EventKey {
        EventKey::new(self.parent.native_id().clone(), self.uid.clone())
    }
}
impl TryFrom<&str> for CalendarEventName {
    type Error = CalendarError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.len() > 1300 {
            return Err(CalendarError::Name);
        }
        let (parent, segment) = value.rsplit_once("/events/").ok_or(CalendarError::Name)?;
        let parent = CalendarName::try_from(parent)?;
        let uid = decode_segment(segment)?
            .parse()
            .map_err(|_| CalendarError::Name)?;
        let result = Self::new(parent, uid);
        if result.as_str() != value {
            return Err(CalendarError::Name);
        }
        Ok(result)
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CalendarEtag(String);
impl TryFrom<&str> for CalendarEtag {
    type Error = CalendarError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if !(5..=256).contains(&value.len())
            || !value.starts_with("W/\"")
            || !value.ends_with('"')
            || !value.as_bytes()[3..value.len() - 1]
                .iter()
                .all(|b| b.is_ascii_graphic() && *b != b'"')
        {
            return Err(CalendarError::Etag);
        }
        Ok(Self(value.to_owned()))
    }
}
impl CalendarEtag {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CalendarVersion {
    name: CalendarName,
    etag: CalendarEtag,
}
impl CalendarVersion {
    pub fn new(name: CalendarName, etag: CalendarEtag) -> Self {
        Self { name, etag }
    }
    pub fn name(&self) -> &CalendarName {
        &self.name
    }
    pub fn etag(&self) -> &CalendarEtag {
        &self.etag
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CalendarEventVersion {
    name: CalendarEventName,
    etag: CalendarEtag,
    calendar_etag: CalendarEtag,
}
impl CalendarEventVersion {
    pub fn new(name: CalendarEventName, etag: CalendarEtag, calendar_etag: CalendarEtag) -> Self {
        Self {
            name,
            etag,
            calendar_etag,
        }
    }
    pub fn name(&self) -> &CalendarEventName {
        &self.name
    }
    pub fn etag(&self) -> &CalendarEtag {
        &self.etag
    }
    pub fn calendar_etag(&self) -> &CalendarEtag {
        &self.calendar_etag
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CalendarCivil {
    date: CivilDate,
    clock: ClockTime,
}
impl CalendarCivil {
    pub fn new(date: CivilDate, clock: ClockTime) -> Self {
        Self { date, clock }
    }
    pub fn date(self) -> CivilDate {
        self.date
    }
    pub fn clock(self) -> ClockTime {
        self.clock
    }
    /// This numeric interpretation only checks a server-provided offset. It
    /// never selects an instant, looks up a zone or resolves a DST fold/gap.
    pub fn naive_seconds(self) -> Result<i64, CalendarError> {
        let (y, m, d) = self.date.components();
        let (h, minute, second) = self.clock.components();
        let date = NaiveDate::from_ymd_opt(y, m, d).ok_or(CalendarError::Date)?;
        let clock = date
            .and_hms_opt(h as u32, minute as u32, second as u32)
            .ok_or(CalendarError::Clock)?;
        Ok(clock.and_utc().timestamp())
    }
}
impl fmt::Display for CalendarCivil {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let (h, m, s) = self.clock.components();
        write!(f, "{}T{h:02}:{m:02}:{s:02}", self.date)
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CalendarZone(String);
impl TryFrom<&str> for CalendarZone {
    type Error = CalendarError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.is_empty() || value.len() > 512 || value.chars().any(char::is_control) {
            return Err(CalendarError::Zone);
        }
        Ok(Self(value.to_owned()))
    }
}
impl CalendarZone {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
fn selected(local: CalendarCivil, time: UtcInstant) -> Result<(), CalendarError> {
    if time.nanos() != 0
        || (time.seconds() == -62_135_596_800 && time.nanos() == 0)
        || (local.naive_seconds()? - time.seconds()).abs() > 26 * 3600
    {
        return Err(CalendarError::Instant);
    }
    Ok(())
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CalendarTimedSpan {
    start_local: CalendarCivil,
    end_local: CalendarCivil,
    start: UtcInstant,
    end: UtcInstant,
    zone: CalendarZone,
}
impl CalendarTimedSpan {
    pub fn new(
        start_local: CalendarCivil,
        end_local: CalendarCivil,
        start: UtcInstant,
        end: UtcInstant,
        zone: CalendarZone,
    ) -> Result<Self, CalendarError> {
        selected(start_local, start)?;
        selected(end_local, end)?;
        if end < start
            || end.seconds() - start.seconds() > 366 * 86400 + 52 * 3600
            || (end == start && start_local != end_local)
        {
            return Err(CalendarError::Span);
        }
        Ok(Self {
            start_local,
            end_local,
            start,
            end,
            zone,
        })
    }
    pub fn start_local(&self) -> CalendarCivil {
        self.start_local
    }
    pub fn end_local(&self) -> CalendarCivil {
        self.end_local
    }
    pub fn start(&self) -> UtcInstant {
        self.start
    }
    pub fn end(&self) -> UtcInstant {
        self.end
    }
    pub fn zone(&self) -> &CalendarZone {
        &self.zone
    }
    pub fn is_point(&self) -> bool {
        self.start == self.end
    }
    pub fn valid_local_input(&self) -> bool {
        self.end > self.start && self.end.seconds() - self.start.seconds() <= 366 * 86400
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
enum SpanValue {
    Dates(CivilDate, CivilDate),
    Times(CalendarTimedSpan),
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CalendarSpan(SpanValue);
impl CalendarSpan {
    pub fn dates(start: CivilDate, end: CivilDate) -> Result<Self, CalendarError> {
        if !(1..=366).contains(&start.days_until(end)) {
            return Err(CalendarError::Span);
        }
        Ok(Self(SpanValue::Dates(start, end)))
    }
    pub fn times(span: CalendarTimedSpan) -> Self {
        Self(SpanValue::Times(span))
    }
    pub fn date_bounds(&self) -> Option<(CivilDate, CivilDate)> {
        match self.0 {
            SpanValue::Dates(a, b) => Some((a, b)),
            SpanValue::Times(_) => None,
        }
    }
    pub fn timed(&self) -> Option<&CalendarTimedSpan> {
        match &self.0 {
            SpanValue::Times(span) => Some(span),
            SpanValue::Dates(_, _) => None,
        }
    }
    pub fn valid_local_input(&self) -> bool {
        match &self.0 {
            SpanValue::Dates(_, _) => true,
            SpanValue::Times(span) => span.valid_local_input(),
        }
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CalendarPointKind {
    Date,
    Local,
    Resolved,
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CalendarPoint {
    key: String,
    date: Option<CivilDate>,
    local: Option<CalendarCivil>,
    instant: Option<UtcInstant>,
}
impl CalendarPoint {
    pub fn date(value: CivilDate) -> Self {
        Self {
            key: value.to_string(),
            date: Some(value),
            local: None,
            instant: None,
        }
    }
    pub fn local(value: CalendarCivil) -> Self {
        Self {
            key: value.to_string(),
            date: None,
            local: Some(value),
            instant: None,
        }
    }
    pub fn resolved(local: CalendarCivil, instant: UtcInstant) -> Result<Self, CalendarError> {
        selected(local, instant)?;
        let timestamp = Utc
            .timestamp_opt(instant.seconds(), 0)
            .single()
            .ok_or(CalendarError::Instant)?;
        Ok(Self {
            key: format!("{local}@{}", timestamp.format("%Y-%m-%dT%H:%M:%SZ")),
            date: None,
            local: Some(local),
            instant: Some(instant),
        })
    }
    pub fn as_str(&self) -> &str {
        &self.key
    }
    pub fn kind(&self) -> CalendarPointKind {
        if self.date.is_some() {
            CalendarPointKind::Date
        } else if self.instant.is_some() {
            CalendarPointKind::Resolved
        } else {
            CalendarPointKind::Local
        }
    }
    pub fn civil_date(&self) -> Option<CivilDate> {
        self.date
    }
    pub fn civil(&self) -> Option<CalendarCivil> {
        self.local
    }
    pub fn instant(&self) -> Option<UtcInstant> {
        self.instant
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn canonical_names_preserve_imported_uid_and_separate_calendars() {
        let home = HomeName::try_from("homes/synthetic").unwrap();
        let a = CalendarName::new(home.clone(), CalendarId::new("calendar-a".parse().unwrap()))
            .unwrap();
        let b = CalendarName::new(home, CalendarId::new("calendar-b".parse().unwrap())).unwrap();
        let uid: EventUid = "Imported / uid 同じ".parse().unwrap();
        let event = CalendarEventName::new(a, uid.clone());
        assert_eq!(CalendarEventName::try_from(event.as_str()).unwrap(), event);
        assert_ne!(event, CalendarEventName::new(b, uid));
        assert!(CalendarName::try_from("homes/synthetic/calendars/id-0A").is_err());
        for invalid in ["unquoted", "\"bad\"quote\"", "\"line\n\"", ""] {
            assert!(CalendarEtag::try_from(invalid).is_err());
        }
    }
    #[test]
    fn civil_fold_identity_retains_explicit_selected_instant_without_resolving_zone() {
        let civil = CalendarCivil::new(
            "2026-11-01".parse().unwrap(),
            ClockTime::new(1, 30, 0).unwrap(),
        );
        let first = CalendarPoint::resolved(civil, UtcInstant::new(1_793_511_000, 0).unwrap());
        // The wire boundary validates a server's numeric offset, not which of
        // two fold candidates a user chose. Distinct instants keep distinct IDs.
        let later = CalendarPoint::resolved(civil, UtcInstant::new(1_793_514_600, 0).unwrap());
        assert!(first.is_ok() && later.is_ok());
        assert_ne!(first.unwrap(), later.unwrap());
        assert!(CalendarPoint::resolved(civil, UtcInstant::new(0, 0).unwrap()).is_err());
    }
}
