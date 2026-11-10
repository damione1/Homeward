//! Calendar editing preserves civil dates and asks the server to resolve wall
//! times. The client never guesses a zone offset or an ambiguous occurrence.
#[cfg(test)]
mod tests;
use home_client_model::{
    api_calendars::{CalendarEventName, CalendarEventVersion, CalendarName},
    calendars::CivilDate,
};
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Boundary {
    Start,
    End,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Field {
    Summary,
    Description,
    Location,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum EditPhase {
    Ready,
    Pending,
    Conflict,
    Unknown,
}
#[derive(Clone, PartialEq)]
pub(crate) struct Candidate {
    pub instant: String,
}
impl Candidate {
    fn from_wire(value: &wire::CalendarTimeCandidate) -> Result<Self, admit::AdmissionError> {
        Ok(Self {
            instant: admit::timestamp_text(&value.selected_time)?,
        })
    }
}
#[derive(Clone, PartialEq)]
pub(crate) enum Resolution {
    Unresolved,
    Accepted(String),
    Unique(Candidate),
    Ambiguous {
        candidates: [Candidate; 2],
        selected: Option<usize>,
    },
    Nonexistent,
    InvalidZone,
}
impl Resolution {
    pub fn instant(&self) -> Option<&str> {
        match self {
            Self::Accepted(instant) => Some(instant),
            Self::Unique(candidate) => Some(&candidate.instant),
            Self::Ambiguous {
                candidates,
                selected,
            } => selected
                .and_then(|index| candidates.get(index))
                .map(|v| v.instant.as_str()),
            Self::Unresolved | Self::Nonexistent | Self::InvalidZone => None,
        }
    }
}
#[derive(Clone, PartialEq)]
pub(crate) struct Wall {
    pub date: String,
    pub time: String,
    pub resolution: Resolution,
}
impl Wall {
    pub fn local(&self) -> String {
        format!("{}T{}", self.date, self.time)
    }
    pub fn from_server(local: &str, instant: &str) -> Self {
        Self {
            date: local.get(..10).unwrap_or_default().into(),
            time: local.get(11..).unwrap_or_default().into(),
            resolution: Resolution::Accepted(instant.into()),
        }
    }
}
#[derive(Clone, PartialEq)]
pub(crate) enum Temporal {
    AllDay {
        start: String,
        end: String,
    },
    Timed {
        start: Box<Wall>,
        end: Box<Wall>,
        zone: String,
    },
}
#[derive(Clone, PartialEq)]
pub(crate) struct Draft {
    pub key: Option<CalendarEventName>,
    pub parent: CalendarName,
    pub revision: Option<CalendarEventVersion>,
    pub original: wire::CalendarEvent,
    pub temporal: Temporal,
    pub phase: EditPhase,
    pub existing: bool,
}
impl Draft {
    pub fn from_server(snapshot: &admit::Admitted<wire::CalendarEvent>) -> Option<Self> {
        let value = snapshot.wire();
        let revision = admit::event_version(value).ok()?;
        let key = revision.name().clone();
        let temporal = match &value.span.value {
            Some(wire::calendar_span::Value::Dates(dates)) => Temporal::AllDay {
                start: admit::date(&dates.start_date).ok()?.to_string(),
                end: admit::date(&dates.end_date).ok()?.to_string(),
            },
            Some(wire::calendar_span::Value::Times(times)) => Temporal::Timed {
                start: Box::new(Wall::from_server(
                    &admit::civil_text(&times.start_local).ok()?,
                    &admit::timestamp_text(&times.start_time).ok()?,
                )),
                end: Box::new(Wall::from_server(
                    &admit::civil_text(&times.end_local).ok()?,
                    &admit::timestamp_text(&times.end_time).ok()?,
                )),
                zone: times.time_zone.clone(),
            },
            None => unreachable!("admission requires an event span"),
        };
        Some(Self {
            parent: key.parent().clone(),
            revision: Some(revision),
            key: Some(key),
            original: value.clone(),
            temporal,
            phase: EditPhase::Ready,
            existing: true,
        })
    }
    pub fn new(parent: CalendarName, date: &CivilDate) -> Self {
        let end = chrono::NaiveDate::parse_from_str(&date.to_string(), "%Y-%m-%d")
            .ok()
            .and_then(|date| date.succ_opt())
            .map_or_else(|| date.to_string(), |date| date.to_string());
        Self {
            revision: None,
            key: None,
            parent,
            original: wire::CalendarEvent::default(),
            temporal: Temporal::AllDay {
                start: date.to_string(),
                end,
            },
            phase: EditPhase::Ready,
            existing: false,
        }
    }
    pub fn edit(&mut self, field: Field, value: String) {
        match field {
            Field::Summary => self.original.summary = value,
            Field::Description => self.original.description = value,
            Field::Location => self.original.location = value,
        }
    }
    pub fn date(&mut self, boundary: Boundary, value: String) {
        match &mut self.temporal {
            Temporal::AllDay { start, end } => match boundary {
                Boundary::Start => *start = value,
                Boundary::End => *end = value,
            },
            Temporal::Timed { start, end, .. } => {
                let wall = match boundary {
                    Boundary::Start => start,
                    Boundary::End => end,
                };
                wall.date = value;
                wall.resolution = Resolution::Unresolved;
            }
        }
    }
    pub fn timed(&mut self, boundary: Boundary, date: String, time: String, zone: String) {
        if let Temporal::Timed {
            start,
            end,
            zone: previous,
        } = &mut self.temporal
        {
            if *previous != zone {
                start.resolution = Resolution::Unresolved;
                end.resolution = Resolution::Unresolved;
                *previous = zone;
            }
            let wall = match boundary {
                Boundary::Start => start,
                Boundary::End => end,
            };
            if wall.date != date || wall.time != time {
                wall.date = date;
                wall.time = time;
                wall.resolution = Resolution::Unresolved;
            }
        }
    }
    pub fn switch(&mut self, timed: bool, zone: &str) {
        self.temporal = match &self.temporal {
            Temporal::AllDay { start, end } if timed => Temporal::Timed {
                start: Box::new(Wall {
                    date: start.clone(),
                    time: "09:00:00".into(),
                    resolution: Resolution::Unresolved,
                }),
                end: Box::new(Wall {
                    date: end.clone(),
                    time: "10:00:00".into(),
                    resolution: Resolution::Unresolved,
                }),
                zone: zone.into(),
            },
            Temporal::Timed { start, end, .. } if !timed => Temporal::AllDay {
                start: start.date.clone(),
                end: end.date.clone(),
            },
            Temporal::AllDay { .. } | Temporal::Timed { .. } => return,
        };
    }
    pub fn resolution_request(
        &self,
        boundary: Boundary,
    ) -> Option<wire::ResolveCalendarTimeRequest> {
        let request = match &self.temporal {
            Temporal::Timed { start, end, zone } => Some(wire::ResolveCalendarTimeRequest {
                name: self.parent.as_str().into(),
                local: admit::parse_civil(&match boundary {
                    Boundary::Start => start.local(),
                    Boundary::End => end.local(),
                })
                .ok()?
                .into(),
                time_zone: zone.clone(),
                ..Default::default()
            }),
            Temporal::AllDay { .. } => None,
        }?;
        admit::admit_resolution_request(&request)
            .ok()
            .map(|request| request.wire().clone())
    }
    pub fn resolve(
        &mut self,
        boundary: Boundary,
        request: &wire::ResolveCalendarTimeRequest,
        response: &wire::ResolveCalendarTimeResponse,
    ) -> Result<(), admit::AdmissionError> {
        let expected = self
            .resolution_request(boundary)
            .ok_or(admit::AdmissionError::Invalid)?;
        if expected != *request {
            return Err(admit::AdmissionError::StaleVersion);
        }
        let response = admit::admit_resolution(response, request)?;
        let value = response.wire();
        use buffa::EnumValue;
        let resolution = match value.resolution {
            EnumValue::Known(wire::CalendarTimeResolution::Unique) => {
                Resolution::Unique(Candidate::from_wire(&value.candidates[0])?)
            }
            EnumValue::Known(wire::CalendarTimeResolution::Ambiguous) => Resolution::Ambiguous {
                candidates: [
                    Candidate::from_wire(&value.candidates[0])?,
                    Candidate::from_wire(&value.candidates[1])?,
                ],
                selected: None,
            },
            EnumValue::Known(wire::CalendarTimeResolution::Nonexistent) => Resolution::Nonexistent,
            EnumValue::Known(wire::CalendarTimeResolution::InvalidZone) => Resolution::InvalidZone,
            EnumValue::Known(wire::CalendarTimeResolution::Unspecified) | EnumValue::Unknown(_) => {
                return Err(admit::AdmissionError::Invalid);
            }
        };
        if let Temporal::Timed { start, end, .. } = &mut self.temporal {
            match boundary {
                Boundary::Start => start,
                Boundary::End => end,
            }
            .resolution = resolution;
        }
        Ok(())
    }
    pub fn choose(&mut self, boundary: Boundary, later: bool) {
        if let Temporal::Timed { start, end, .. } = &mut self.temporal
            && let Resolution::Ambiguous { selected, .. } = &mut match boundary {
                Boundary::Start => start,
                Boundary::End => end,
            }
            .resolution
        {
            *selected = Some(usize::from(later));
        }
    }
    pub fn event(&self) -> Option<wire::CalendarEvent> {
        if self.original.summary.is_empty()
            || self.original.summary.len() > 512
            || self.original.description.len() > 4096
            || self.original.location.len() > 512
            || [
                &self.original.summary,
                &self.original.description,
                &self.original.location,
            ]
            .iter()
            .any(|value| value.contains('\0'))
        {
            return None;
        }
        let mut event = self.original.clone();
        let value = match &self.temporal {
            Temporal::AllDay { start, end } => {
                let a = start.parse::<CivilDate>().ok()?;
                let b = end.parse::<CivilDate>().ok()?;
                if a >= b {
                    return None;
                }
                wire::calendar_span::Value::Dates(Box::new(wire::CalendarDateSpan {
                    start_date: admit::date_wire(a).into(),
                    end_date: admit::date_wire(b).into(),
                    ..Default::default()
                }))
            }
            Temporal::Timed { start, end, zone } => {
                let a = start.resolution.instant()?;
                let b = end.resolution.instant()?;
                if chrono::DateTime::parse_from_rfc3339(a).ok()?
                    >= chrono::DateTime::parse_from_rfc3339(b).ok()?
                {
                    return None;
                }
                wire::calendar_span::Value::Times(Box::new(wire::CalendarTimeSpan {
                    start_local: admit::parse_civil(&start.local()).ok()?.into(),
                    end_local: admit::parse_civil(&end.local()).ok()?.into(),
                    start_time: admit::parse_timestamp(a, true).ok()?.into(),
                    end_time: admit::parse_timestamp(b, true).ok()?.into(),
                    time_zone: zone.clone(),
                    ..Default::default()
                }))
            }
        };
        event.span = wire::CalendarSpan {
            value: Some(value),
            ..Default::default()
        }
        .into();
        Some(event)
    }
}
