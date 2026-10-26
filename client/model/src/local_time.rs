//! Profile-based display of absolute timestamps. No location or system timezone.
use chrono::{DateTime, FixedOffset, Offset, Utc};
use chrono_tz::Tz;
use std::{fmt, num::NonZeroU64, str::FromStr};

/// An admitted protobuf timestamp; epoch zero is a real instant.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub struct UtcInstant(DateTime<Utc>);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InvalidInstant;

impl UtcInstant {
    pub fn new(seconds: i64, nanos: i32) -> Result<Self, InvalidInstant> {
        if !(-62_135_596_800..=253_402_300_799).contains(&seconds)
            || !(0..1_000_000_000).contains(&nanos)
        {
            return Err(InvalidInstant);
        }
        DateTime::from_timestamp(seconds, nanos as u32)
            .map(Self)
            .ok_or(InvalidInstant)
    }

    pub fn seconds(self) -> i64 {
        self.0.timestamp()
    }

    pub fn nanos(self) -> i32 {
        self.0.timestamp_subsec_nanos() as i32
    }

    /// Wall-clock skew stays distinct from a fresh observation.
    pub fn age_at(self, now: Self) -> Age {
        if self > now {
            Age::ClockAhead
        } else {
            match NonZeroU64::new((now.0 - self.0).num_seconds() as u64) {
                Some(seconds) => Age::Elapsed(seconds),
                None => Age::Current,
            }
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Age {
    Current,
    Elapsed(NonZeroU64),
    ClockAhead,
}

/// An explicit IANA zone from the user's profile. Invalid zones have no fallback.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DisplayTimeZone(Tz);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InvalidTimeZone;

impl FromStr for DisplayTimeZone {
    type Err = InvalidTimeZone;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        value.parse::<Tz>().map(Self).map_err(|_| InvalidTimeZone)
    }
}

impl DisplayTimeZone {
    pub fn name(self) -> &'static str {
        self.0.name()
    }

    /// Conversion changes presentation only; it never reinterprets a local time.
    pub fn local(self, instant: UtcInstant) -> LocalTime {
        let value = instant.0.with_timezone(&self.0);
        LocalTime {
            instant,
            zone: self,
            value: value.with_timezone(&value.offset().fix()),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LocalTime {
    instant: UtcInstant,
    zone: DisplayTimeZone,
    value: DateTime<FixedOffset>,
}

impl LocalTime {
    pub fn instant(self) -> UtcInstant {
        self.instant
    }

    pub fn zone(self) -> DisplayTimeZone {
        self.zone
    }

    pub fn date(self) -> String {
        self.value.format("%Y-%m-%d").to_string()
    }

    pub fn clock(self) -> String {
        self.value.format("%H:%M").to_string()
    }

    /// The offset disambiguates repeated local times during the autumn fold.
    pub fn offset(self) -> String {
        self.value.format("%:z").to_string()
    }
}

impl fmt::Display for LocalTime {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            formatter,
            "{} {} {} · {}",
            self.date(),
            self.clock(),
            self.offset(),
            self.zone.name()
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(value: &str) -> UtcInstant {
        let value = DateTime::parse_from_rfc3339(value).unwrap();
        UtcInstant::new(value.timestamp(), value.timestamp_subsec_nanos() as i32).unwrap()
    }

    #[test]
    fn spring_transition_displays_the_actual_profile_offset_without_inventing_a_gap_time() {
        let zone: DisplayTimeZone = "America/Toronto".parse().unwrap();
        let before = zone.local(at("2026-03-08T06:59:00Z"));
        let after = zone.local(at("2026-03-08T07:00:00Z"));
        assert_eq!(
            (before.clock(), before.offset()),
            ("01:59".into(), "-05:00".into())
        );
        assert_eq!(
            (after.clock(), after.offset()),
            ("03:00".into(), "-04:00".into())
        );
        assert_eq!(after.date(), "2026-03-08");
    }

    #[test]
    fn repeated_autumn_clock_times_remain_distinct_absolute_events() {
        let zone: DisplayTimeZone = "America/Toronto".parse().unwrap();
        let first = zone.local(at("2026-11-01T05:30:00Z"));
        let second = zone.local(at("2026-11-01T06:30:00Z"));
        assert_eq!(first.clock(), second.clock());
        assert_eq!(first.offset(), "-04:00");
        assert_eq!(second.offset(), "-05:00");
        assert_ne!(first.to_string(), second.to_string());
        assert!(first.instant() < second.instant());
    }

    #[test]
    fn changing_profile_preserves_the_event_and_can_change_its_local_date() {
        let instant = at("2026-01-01T00:30:00Z");
        let toronto: DisplayTimeZone = "America/Toronto".parse().unwrap();
        let paris: DisplayTimeZone = "Europe/Paris".parse().unwrap();
        let first = toronto.local(instant);
        let second = paris.local(first.instant());
        assert_eq!(first.date(), "2025-12-31");
        assert_eq!(second.date(), "2026-01-01");
        assert_eq!(first.instant(), second.instant());
        assert_eq!(second.zone(), paris);
    }

    #[test]
    fn resuming_uses_the_absolute_observation_age_and_reports_future_clocks() {
        let observed = at("2026-03-08T06:30:00Z");
        let resumed = at("2026-03-08T08:30:00Z");
        assert_eq!(
            observed.age_at(resumed),
            Age::Elapsed(NonZeroU64::new(7200).unwrap())
        );
        assert_eq!(observed.age_at(observed), Age::Current);
        assert_eq!(resumed.age_at(observed), Age::ClockAhead);
    }

    #[test]
    fn invalid_profile_or_timestamp_cannot_become_a_default_zone_or_zero() {
        for value in ["", "Local", "Unknown/Zone", " America/Toronto"] {
            assert_eq!(value.parse::<DisplayTimeZone>(), Err(InvalidTimeZone));
        }
        for (seconds, nanos) in [
            (-62_135_596_801, 0),
            (253_402_300_800, 0),
            (0, -1),
            (0, 1_000_000_000),
        ] {
            assert_eq!(UtcInstant::new(seconds, nanos), Err(InvalidInstant));
        }
        let epoch = UtcInstant::new(0, 0).unwrap();
        assert_eq!(epoch.seconds(), 0);
        assert_eq!(epoch.nanos(), 0);
        assert_eq!(
            "UTC"
                .parse::<DisplayTimeZone>()
                .unwrap()
                .local(epoch)
                .date(),
            "1970-01-01"
        );
    }
}
