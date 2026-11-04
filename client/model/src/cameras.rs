//! Stable UI keys for anonymous camera projections. Source identity is the
//! admitted MediaSource name; no provider alias or native entity ID resolves it.
use crate::api_content::MediaSourceName;
use std::{error::Error, fmt, str::FromStr};
pub const MAX_ID_BYTES: usize = 256;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum IdentityError {
    Empty,
    TooLong,
    ControlCharacter,
}
impl fmt::Display for IdentityError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Empty => "event identity must be nonempty",
            Self::TooLong => "event identity exceeds the protocol byte limit",
            Self::ControlCharacter => "event identity contains a control character",
        })
    }
}
impl Error for IdentityError {}
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct EventId(String);
impl FromStr for EventId {
    type Err = IdentityError;
    fn from_str(value: &str) -> Result<Self, IdentityError> {
        if value.is_empty() {
            return Err(IdentityError::Empty);
        }
        if value.len() > MAX_ID_BYTES {
            return Err(IdentityError::TooLong);
        }
        if value.chars().any(char::is_control) {
            return Err(IdentityError::ControlCharacter);
        }
        Ok(Self(value.to_owned()))
    }
}
impl EventId {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct EventKey {
    source: MediaSourceName,
    event: EventId,
}
impl EventKey {
    pub fn new(source: MediaSourceName, event: EventId) -> Self {
        Self { source, event }
    }
    pub fn source(&self) -> &MediaSourceName {
        &self.source
    }
    pub fn event(&self) -> &EventId {
        &self.event
    }
}
/// UI target identity only. Actual bytes are additionally bound to the exact
/// MediaSession and monotonic lease by the content admission owner.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum ImageKey {
    Snapshot(MediaSourceName),
    EventThumbnail(EventKey),
}
impl ImageKey {
    pub fn source(&self) -> &MediaSourceName {
        match self {
            Self::Snapshot(source) => source,
            Self::EventThumbnail(event) => event.source(),
        }
    }
}
