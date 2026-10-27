//! Calendar identities and civil dates. No recurrence or local-time resolver.
use crate::EntityId;
use chrono::{Datelike, NaiveDate};
use std::{error::Error, fmt, str::FromStr};

/// Calendar objects are registry entities, distinct from arbitrary state targets.
/// Construction proves representation only; the server grants access and origin.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct CalendarId(EntityId);
impl CalendarId {
    pub fn new(entity: EntityId) -> Self {
        Self(entity)
    }
    pub fn entity(&self) -> &EntityId {
        &self.0
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum UidError {
    Empty,
    TooLong,
    ForbiddenCharacter,
}
impl fmt::Display for UidError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Empty => "event UID is empty",
            Self::TooLong => "event UID exceeds 255 UTF-8 bytes",
            Self::ForbiddenCharacter => "event UID contains a forbidden control character",
        })
    }
}
impl Error for UidError {}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct EventUid(String);
impl FromStr for EventUid {
    type Err = UidError;
    fn from_str(value: &str) -> Result<Self, Self::Err> {
        if value.is_empty() {
            return Err(UidError::Empty);
        }
        if value.len() > 255 {
            return Err(UidError::TooLong);
        }
        if value.bytes().any(|b| matches!(b, b'\0' | b'\r' | b'\n')) {
            return Err(UidError::ForbiddenCharacter);
        }
        Ok(Self(value.to_owned()))
    }
}
impl EventUid {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

/// An event UID is scoped to its calendar, including when two sources reuse it.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct EventKey {
    calendar: CalendarId,
    uid: EventUid,
}
impl EventKey {
    pub fn new(calendar: CalendarId, uid: EventUid) -> Self {
        Self { calendar, uid }
    }
    pub fn calendar(&self) -> &CalendarId {
        &self.calendar
    }
    pub fn uid(&self) -> &EventUid {
        &self.uid
    }
}

/// Admitted server origin. It never establishes a user's write permission.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Origin {
    Local,
    RemoteIcs,
    Calculated,
}

/// A Gregorian civil date in the server's supported range, never midnight UTC.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct CivilDate(NaiveDate);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct InvalidDate;
impl fmt::Display for InvalidDate {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("expected a valid civil date YYYY-MM-DD, year 0001 through 9999")
    }
}
impl Error for InvalidDate {}
impl FromStr for CivilDate {
    type Err = InvalidDate;
    fn from_str(value: &str) -> Result<Self, Self::Err> {
        if value.len() != 10
            || value.bytes().enumerate().any(|(index, byte)| {
                if index == 4 || index == 7 {
                    byte != b'-'
                } else {
                    !byte.is_ascii_digit()
                }
            })
        {
            return Err(InvalidDate);
        }
        NaiveDate::parse_from_str(value, "%Y-%m-%d")
            .ok()
            .filter(|date| (1..=9999).contains(&date.year()))
            .map(Self)
            .ok_or(InvalidDate)
    }
}
impl fmt::Display for CivilDate {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.format("%Y-%m-%d").fmt(f)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn civil_dates_admit_leap_days_and_reject_normalization_or_zone_inference() {
        for value in [
            "0001-01-01",
            "2028-02-29",
            "2026-03-08",
            "2026-11-01",
            "9999-12-31",
        ] {
            assert_eq!(value.parse::<CivilDate>().unwrap().to_string(), value);
        }
        for value in [
            "0000-01-01",
            "2026-02-29",
            "2026-04-31",
            "2026-1-01",
            "2026-01-01T00:00:00Z",
            "2026-01-01 ",
        ] {
            assert!(value.parse::<CivilDate>().is_err(), "{value}");
        }
    }

    #[test]
    fn event_keys_keep_same_uid_in_different_calendars_distinct() {
        let first = CalendarId::new("calendar-a".parse().unwrap());
        let second = CalendarId::new("calendar-b".parse().unwrap());
        let uid: EventUid = "shared UID@source".parse().unwrap();
        let key = EventKey::new(first.clone(), uid.clone());
        assert_eq!(key.calendar(), &first);
        assert_eq!(key.uid().as_str(), "shared UID@source");
        assert_ne!(key, EventKey::new(second, uid));
    }

    #[test]
    fn uid_admission_preserves_case_and_checks_the_server_byte_bound() {
        assert_eq!("ÀbC".parse::<EventUid>().unwrap().as_str(), "ÀbC");
        assert_eq!("".parse::<EventUid>(), Err(UidError::Empty));
        assert_eq!("é".repeat(128).parse::<EventUid>(), Err(UidError::TooLong));
        for value in ["uid\0", "uid\r", "uid\n"] {
            assert_eq!(value.parse::<EventUid>(), Err(UidError::ForbiddenCharacter));
        }
    }
}
