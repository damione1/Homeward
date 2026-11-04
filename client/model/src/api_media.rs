//! Admitted generic media intent. Wire messages, providers and live authority
//! stay outside this module; a valid value never proves current topology.
use crate::api::EntityName;

pub const MAX_POSITION_MILLIS: i64 = 7 * 24 * 60 * 60 * 1000;
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MediaError {
    Route,
    Identifier,
    Volume,
    Position,
    Provenance,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MediaIdentifier(String);
impl TryFrom<&str> for MediaIdentifier {
    type Error = MediaError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.is_empty() || value.len() > 256 || value.chars().any(char::is_control) {
            return Err(MediaError::Identifier);
        }
        Ok(Self(value.to_owned()))
    }
}
impl MediaIdentifier {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MediaRoute {
    entity: EntityName,
    coordinator: EntityName,
    endpoint: MediaIdentifier,
    topology_revision: i64,
    admission_epoch: String,
    config_revision: i64,
}
impl MediaRoute {
    pub fn new(
        entity: EntityName,
        endpoint: MediaIdentifier,
        coordinator: EntityName,
        topology_revision: i64,
        admission_epoch: &str,
        config_revision: i64,
    ) -> Result<Self, MediaError> {
        if entity
            .as_str()
            .rsplit_once("/entities/")
            .map(|(home, _)| home)
            != coordinator
                .as_str()
                .rsplit_once("/entities/")
                .map(|(home, _)| home)
            || topology_revision < 1
            || config_revision < 1
            || admission_epoch.len() != 32
            || !admission_epoch
                .bytes()
                .all(|value| value.is_ascii_hexdigit())
            || !admission_epoch.bytes().any(|value| value != b'0')
        {
            return Err(MediaError::Route);
        }
        Ok(Self {
            entity,
            coordinator,
            endpoint,
            topology_revision,
            admission_epoch: admission_epoch.to_owned(),
            config_revision,
        })
    }
    pub fn entity(&self) -> &EntityName {
        &self.entity
    }
    pub fn coordinator(&self) -> &EntityName {
        &self.coordinator
    }
    pub fn endpoint(&self) -> &MediaIdentifier {
        &self.endpoint
    }
    pub fn topology_revision(&self) -> i64 {
        self.topology_revision
    }
    pub fn admission_epoch(&self) -> &str {
        &self.admission_epoch
    }
    pub fn config_revision(&self) -> i64 {
        self.config_revision
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MediaOperation {
    Play,
    Pause,
    Stop,
    Previous,
    Next,
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct VolumeBasisPoints(i32);
impl TryFrom<i32> for VolumeBasisPoints {
    type Error = MediaError;
    fn try_from(value: i32) -> Result<Self, Self::Error> {
        if !(0..=10000).contains(&value) {
            return Err(MediaError::Volume);
        }
        Ok(Self(value))
    }
}
impl VolumeBasisPoints {
    pub fn value(self) -> i32 {
        self.0
    }
}
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PositionMillis(i64);
impl TryFrom<i64> for PositionMillis {
    type Error = MediaError;
    fn try_from(value: i64) -> Result<Self, Self::Error> {
        if !(0..=MAX_POSITION_MILLIS).contains(&value) {
            return Err(MediaError::Position);
        }
        Ok(Self(value))
    }
}
impl PositionMillis {
    pub fn value(self) -> i64 {
        self.0
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum MediaAction {
    Operation(MediaOperation),
    Volume(VolumeBasisPoints),
    Position(PositionMillis),
    Source(MediaIdentifier),
    Group(MediaIdentifier),
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MediaIntent {
    route: MediaRoute,
    action: MediaAction,
}
impl MediaIntent {
    pub fn new(route: MediaRoute, action: MediaAction) -> Self {
        Self { route, action }
    }
    pub fn route(&self) -> &MediaRoute {
        &self.route
    }
    pub fn action(&self) -> &MediaAction {
        &self.action
    }
}

/// Qualification data never claims to be an observed installation. Unknown
/// wire provenance cannot enter this actionable closed domain set.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum MediaDataProvenance {
    Observed,
    Synthetic,
}
