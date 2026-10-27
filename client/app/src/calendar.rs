//! Calendar editing preserves civil dates and asks the server to resolve wall
//! times. The client never guesses a zone offset or an ambiguous occurrence.
#[cfg(test)]
mod tests;
use home_client_model::calendars::{CivilDate, EventKey, EventRevision, RevisionState};
use home_wire_contract::{calendars_contract as admit, home::calendars::v1 as wire};

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
    Withheld,
}
#[derive(Clone)]
pub(crate) enum Resolution {
    Unresolved,
    Accepted(String),
    Unique(wire::LocalTimeCandidate),
    Ambiguous {
        candidates: [wire::LocalTimeCandidate; 2],
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
#[derive(Clone)]
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
#[derive(Clone)]
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
#[derive(Clone)]
pub(crate) struct Draft {
    pub key: EventKey,
    pub revision: EventRevision,
    pub original: wire::Event,
    pub temporal: Temporal,
    pub phase: EditPhase,
    pub existing: bool,
}
impl Draft {
    pub fn from_server(snapshot: &admit::Admitted<wire::EventSnapshot>) -> Option<Self> {
        let snapshot = snapshot.wire();
        let value = snapshot.value.as_option()?;
        let key = admit::event_key(&snapshot.key).expect("admitted event identity");
        let temporal = match &value.span.value {
            Some(wire::span::Value::Dates(dates)) => Temporal::AllDay {
                start: dates.start.clone(),
                end: dates.end_exclusive.clone(),
            },
            Some(wire::span::Value::Times(times)) => Temporal::Timed {
                start: Box::new(Wall::from_server(&times.anchor_local, &times.start_instant)),
                end: Box::new(Wall::from_server(&times.end_local, &times.end_instant)),
                zone: times.zone.clone(),
            },
            None => unreachable!("admission requires an event span"),
        };
        Some(Self {
            revision: admit::event_revision(&snapshot.revision, &key)
                .expect("admitted event revision"),
            key,
            original: value.clone(),
            temporal,
            phase: EditPhase::Ready,
            existing: true,
        })
    }
    pub fn new(key: EventKey, date: &CivilDate) -> Self {
        let end = chrono::NaiveDate::parse_from_str(&date.to_string(), "%Y-%m-%d")
            .ok()
            .and_then(|date| date.succ_opt())
            .map_or_else(|| date.to_string(), |date| date.to_string());
        Self {
            revision: EventRevision::new(key.clone(), RevisionState::Absent),
            key,
            original: wire::Event::default(),
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
    pub fn resolution_request(&self, boundary: Boundary) -> Option<wire::ResolveLocalTimeRequest> {
        let request = match &self.temporal {
            Temporal::Timed { start, end, zone } => Some(wire::ResolveLocalTimeRequest {
                id: wire::CalendarId {
                    entity_id: self.key.calendar().entity().as_str().into(),
                    ..Default::default()
                }
                .into(),
                local: match boundary {
                    Boundary::Start => start.local(),
                    Boundary::End => end.local(),
                },
                zone: zone.clone(),
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
        request: &wire::ResolveLocalTimeRequest,
        response: &wire::ResolveLocalTimeResponse,
    ) -> Result<(), admit::AdmissionError> {
        let expected = self
            .resolution_request(boundary)
            .ok_or(admit::AdmissionError::Invalid)?;
        if expected != *request {
            return Err(admit::AdmissionError::StaleRevision);
        }
        let response = admit::admit_resolution(response, request)?;
        let value = response.wire();
        use buffa::EnumValue;
        let resolution = match value.resolution {
            EnumValue::Known(wire::LocalTimeResolution::Unique) => {
                Resolution::Unique(value.candidates[0].clone())
            }
            EnumValue::Known(wire::LocalTimeResolution::Ambiguous) => Resolution::Ambiguous {
                candidates: [value.candidates[0].clone(), value.candidates[1].clone()],
                selected: None,
            },
            EnumValue::Known(wire::LocalTimeResolution::Nonexistent) => Resolution::Nonexistent,
            EnumValue::Known(wire::LocalTimeResolution::InvalidZone) => Resolution::InvalidZone,
            EnumValue::Known(wire::LocalTimeResolution::Unspecified) | EnumValue::Unknown(_) => {
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
    pub fn event(&self) -> Option<wire::Event> {
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
                wire::span::Value::Dates(Box::new(wire::DateSpan {
                    start: start.clone(),
                    end_exclusive: end.clone(),
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
                wire::span::Value::Times(Box::new(wire::TimeSpan {
                    anchor_local: start.local(),
                    end_local: end.local(),
                    start_instant: a.into(),
                    end_instant: b.into(),
                    zone: zone.clone(),
                    ..Default::default()
                }))
            }
        };
        event.span = wire::Span {
            value: Some(value),
            ..Default::default()
        }
        .into();
        Some(event)
    }
}
