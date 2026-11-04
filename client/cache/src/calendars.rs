//! A bounded, revision-fenced calendar range. A page is never published alone.
mod reader;
use buffa::Message;
use home_client_model::api_calendars::{CalendarEventName, CalendarName, CalendarVersion};
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};
pub use reader::{CalendarReadEvent, CalendarReadExit, read_calendar_once};
use std::{
    collections::{BTreeMap, BTreeSet},
    time::SystemTime,
};
const MAX_ROWS: usize = 10_000;
const MAX_BYTES: usize = 8 << 20;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CalendarFreshness {
    Fresh,
    Stale,
    Unavailable,
}
impl CalendarFreshness {
    fn admitted(
        value: buffa::EnumValue<wire::CalendarFreshness>,
    ) -> Result<Self, admit::AdmissionError> {
        use buffa::EnumValue;
        match value {
            EnumValue::Known(wire::CalendarFreshness::Fresh) => Ok(Self::Fresh),
            EnumValue::Known(wire::CalendarFreshness::Stale) => Ok(Self::Stale),
            EnumValue::Known(wire::CalendarFreshness::Unavailable) => Ok(Self::Unavailable),
            EnumValue::Known(wire::CalendarFreshness::Unspecified) | EnumValue::Unknown(_) => {
                Err(admit::AdmissionError::Invalid)
            }
        }
    }
    fn combine(self, other: Self) -> Self {
        match (self, other) {
            (Self::Fresh, Self::Fresh) => Self::Fresh,
            (Self::Fresh, Self::Stale)
            | (Self::Stale, Self::Fresh)
            | (Self::Stale, Self::Stale) => Self::Stale,
            (Self::Unavailable, Self::Fresh)
            | (Self::Unavailable, Self::Stale)
            | (Self::Unavailable, Self::Unavailable)
            | (Self::Fresh, Self::Unavailable)
            | (Self::Stale, Self::Unavailable) => Self::Unavailable,
        }
    }
}

pub struct CalendarReadSnapshot {
    calendar: wire::Calendar,
    query: wire::QueryOccurrencesRequest,
    occurrences: Vec<wire::Occurrence>,
    exceptions: Vec<wire::CalendarSeriesExceptions>,
    fetched_at: SystemTime,
    freshness: CalendarFreshness,
    fresh_until: BTreeSet<String>,
}
impl CalendarReadSnapshot {
    pub fn calendar(&self) -> &wire::Calendar {
        &self.calendar
    }
    pub fn query(&self) -> &wire::QueryOccurrencesRequest {
        &self.query
    }
    pub fn occurrences(&self) -> &[wire::Occurrence] {
        &self.occurrences
    }
    pub fn exceptions(&self) -> &[wire::CalendarSeriesExceptions] {
        &self.exceptions
    }
    pub fn fetched_at(&self) -> SystemTime {
        self.fetched_at
    }
    pub fn freshness(&self) -> CalendarFreshness {
        self.freshness
    }
    pub fn expiry_bounds(&self) -> impl Iterator<Item = &str> {
        self.fresh_until.iter().map(String::as_str)
    }
}
pub struct CalendarCapture {
    calendar: admit::Admitted<wire::Calendar>,
    query: admit::Admitted<wire::QueryOccurrencesRequest>,
    id: CalendarName,
    revision: CalendarVersion,
    occurrences: Vec<wire::Occurrence>,
    identities: BTreeSet<(CalendarEventName, String)>,
    exceptions: BTreeMap<CalendarEventName, wire::CalendarSeriesExceptions>,
    tokens: BTreeSet<String>,
    complete: bool,
    poisoned: bool,
    pages: usize,
    bytes: usize,
    freshness: CalendarFreshness,
    fresh_until: BTreeSet<String>,
}
impl CalendarCapture {
    pub fn new(
        calendar: &wire::Calendar,
        query: &wire::QueryOccurrencesRequest,
    ) -> Result<Self, admit::AdmissionError> {
        let (id, _) = admit::query_parent(&query.parent)?;
        let calendar = admit::admit_calendar(calendar, &id)?;
        let revision = admit::calendar_version(calendar.wire())?;
        if !query.page_token.is_empty() {
            return Err(admit::AdmissionError::Sequence);
        }
        Ok(Self {
            freshness: CalendarFreshness::admitted(calendar.wire().freshness)?,
            fresh_until: calendar
                .wire()
                .fresh_until_time
                .as_option()
                .map(admit::timestamp_text)
                .transpose()?
                .into_iter()
                .collect(),
            poisoned: false,
            pages: 0,
            complete: false,
            calendar,
            query: admit::admit_query(query)?,
            id,
            revision,
            occurrences: Vec::new(),
            identities: BTreeSet::new(),
            exceptions: BTreeMap::new(),
            tokens: BTreeSet::new(),
            bytes: 0,
        })
    }
    pub fn request(&self) -> Option<&wire::QueryOccurrencesRequest> {
        (!self.complete).then_some(self.query.wire())
    }
    pub fn push(
        &mut self,
        page: &wire::QueryOccurrencesResponse,
    ) -> Result<(), admit::AdmissionError> {
        if self.complete || self.poisoned {
            return Err(admit::AdmissionError::Sequence);
        }
        self.poisoned = true;
        if self.pages >= 64 {
            return Err(admit::AdmissionError::Capacity);
        }
        admit::admit_page(page, &self.query, &self.revision)?;
        if page.provenance != self.calendar.wire().provenance {
            return Err(admit::AdmissionError::Invalid);
        }
        self.freshness = self
            .freshness
            .combine(CalendarFreshness::admitted(page.freshness)?);
        self.fresh_until.extend(
            page.fresh_until_time
                .as_option()
                .map(admit::timestamp_text)
                .transpose()?,
        );
        let size = page.encoded_len() as usize;
        if self.bytes.saturating_add(size) > MAX_BYTES
            || self
                .occurrences
                .len()
                .saturating_add(page.occurrences.len())
                > MAX_ROWS
        {
            return Err(admit::AdmissionError::Capacity);
        }
        for row in &page.occurrences {
            let key = CalendarEventName::try_from(row.calendar_event.as_str())
                .map_err(|_| admit::AdmissionError::Invalid)?;
            if !self
                .identities
                .insert((key, admit::point_key(&row.recurrence_point)?))
            {
                return Err(admit::AdmissionError::Invalid);
            }
        }
        for group in &page.series_exceptions {
            let key = CalendarEventName::try_from(group.calendar_event.as_str())
                .map_err(|_| admit::AdmissionError::Invalid)?;
            if let Some(old) = self.exceptions.get(&key) {
                if old != group {
                    return Err(admit::AdmissionError::StaleVersion);
                }
            } else {
                self.exceptions.insert(key, group.clone());
            }
        }
        if self.exceptions.len() > 1000 {
            return Err(admit::AdmissionError::Capacity);
        }
        if !page.next_page_token.is_empty() && !self.tokens.insert(page.next_page_token.clone()) {
            return Err(admit::AdmissionError::Sequence);
        }
        self.pages += 1;
        self.bytes += size;
        self.occurrences.extend(page.occurrences.iter().cloned());
        self.complete = page.next_page_token.is_empty();
        let mut query = self.query.wire().clone();
        query.page_token = page.next_page_token.clone();
        self.query = admit::admit_query(&query)?;
        self.poisoned = false;
        Ok(())
    }
    pub fn finish(
        mut self,
        current: &wire::Calendar,
    ) -> Result<CalendarReadSnapshot, admit::AdmissionError> {
        admit::admit_calendar(current, &self.id)?;
        if !self.complete || self.poisoned {
            return Err(admit::AdmissionError::Sequence);
        }
        if admit::calendar_version(current)? != self.revision
            || current.origin != self.calendar.wire().origin
            || current.provenance != self.calendar.wire().provenance
        {
            return Err(admit::AdmissionError::StaleVersion);
        }
        let mut query = self.query.wire().clone();
        query.page_token.clear();
        self.fresh_until.extend(
            current
                .fresh_until_time
                .as_option()
                .map(admit::timestamp_text)
                .transpose()?,
        );
        Ok(CalendarReadSnapshot {
            calendar: current.clone(),
            query,
            occurrences: self.occurrences,
            exceptions: self.exceptions.into_values().collect(),
            fetched_at: SystemTime::now(),
            freshness: self
                .freshness
                .combine(CalendarFreshness::admitted(current.freshness)?),
            fresh_until: self.fresh_until,
        })
    }
}
#[cfg(test)]
mod tests;
