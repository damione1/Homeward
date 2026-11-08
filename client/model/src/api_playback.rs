//! Immutable playback definitions and independently optional observations.
//! Source support, current grants, quality and observed values stay distinct.
use crate::{
    api::{CapabilityName, DefinitionEtag, EntityName},
    api_media::{
        MediaAction, MediaDataProvenance, MediaIdentifier, MediaOperation, MediaRoute,
        PositionMillis, VolumeBasisPoints,
    },
    api_observations::{ActionAccess, ObservationQuality},
    local_time::UtcInstant,
};
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PlaybackError {
    Bounds,
    Text,
    Duplicate,
    Route,
    Observation,
    Limit,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PlaybackState {
    Unknown,
    Stopped,
    Paused,
    Playing,
    Buffering,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct QuantityBounds {
    minimum: u64,
    maximum: u64,
    step: u64,
}
impl QuantityBounds {
    fn new(minimum: u64, maximum: u64, step: u64) -> Result<Self, PlaybackError> {
        if minimum > maximum || step == 0 {
            return Err(PlaybackError::Bounds);
        }
        Ok(Self {
            minimum,
            maximum,
            step,
        })
    }
    pub fn minimum(self) -> u64 {
        self.minimum
    }
    pub fn maximum(self) -> u64 {
        self.maximum
    }
    pub fn step(self) -> u64 {
        self.step
    }
    pub fn admits(self, value: u64) -> bool {
        value >= self.minimum
            && value <= self.maximum
            && (value - self.minimum).is_multiple_of(self.step)
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct VolumeBounds(QuantityBounds);
impl VolumeBounds {
    pub fn new(
        minimum: VolumeBasisPoints,
        maximum: VolumeBasisPoints,
        step: VolumeBasisPoints,
    ) -> Result<Self, PlaybackError> {
        Ok(Self(QuantityBounds::new(
            minimum.value() as u64,
            maximum.value() as u64,
            step.value() as u64,
        )?))
    }
    pub fn quantity(self) -> QuantityBounds {
        self.0
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PositionBounds(QuantityBounds);
impl PositionBounds {
    pub fn new(
        minimum: PositionMillis,
        maximum: PositionMillis,
        step: PositionMillis,
    ) -> Result<Self, PlaybackError> {
        Ok(Self(QuantityBounds::new(
            minimum.value() as u64,
            maximum.value() as u64,
            step.value() as u64,
        )?))
    }
    pub fn quantity(self) -> QuantityBounds {
        self.0
    }
}
fn text(value: &str, empty: bool) -> bool {
    (empty || !value.is_empty())
        && value.len() <= 1024
        && !value.chars().any(|c| c.is_control() && c != '\n')
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MediaChoice {
    id: MediaIdentifier,
    display_name: String,
}
impl MediaChoice {
    pub fn new(id: MediaIdentifier, display_name: &str) -> Result<Self, PlaybackError> {
        if !text(display_name, false) {
            return Err(PlaybackError::Text);
        }
        Ok(Self {
            id,
            display_name: display_name.to_owned(),
        })
    }
    pub fn id(&self) -> &MediaIdentifier {
        &self.id
    }
    pub fn display_name(&self) -> &str {
        &self.display_name
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MediaMember {
    entity: EntityName,
    display_name: String,
}
impl MediaMember {
    pub fn new(entity: EntityName, display_name: &str) -> Result<Self, PlaybackError> {
        if !text(display_name, false) {
            return Err(PlaybackError::Text);
        }
        Ok(Self {
            entity,
            display_name: display_name.to_owned(),
        })
    }
    pub fn entity(&self) -> &EntityName {
        &self.entity
    }
    pub fn display_name(&self) -> &str {
        &self.display_name
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PlaybackDefinition {
    operations: Vec<MediaOperation>,
    volume: Option<VolumeBounds>,
    position: Option<PositionBounds>,
    sources: Vec<MediaChoice>,
    groups: Vec<MediaChoice>,
}
impl PlaybackDefinition {
    pub fn new(
        operations: Vec<MediaOperation>,
        volume: Option<VolumeBounds>,
        position: Option<PositionBounds>,
        sources: Vec<MediaChoice>,
        groups: Vec<MediaChoice>,
    ) -> Result<Self, PlaybackError> {
        if operations.len() > 5 || sources.len() > 16 || groups.len() > 16 {
            return Err(PlaybackError::Limit);
        }
        if operations
            .iter()
            .enumerate()
            .any(|(i, value)| operations[..i].contains(value))
        {
            return Err(PlaybackError::Duplicate);
        }
        for options in [&sources, &groups] {
            if options
                .iter()
                .enumerate()
                .any(|(i, value)| options[..i].iter().any(|old| old.id == value.id))
            {
                return Err(PlaybackError::Duplicate);
            }
        }
        Ok(Self {
            operations,
            volume,
            position,
            sources,
            groups,
        })
    }
    pub fn operations(&self) -> &[MediaOperation] {
        &self.operations
    }
    pub fn volume(&self) -> Option<VolumeBounds> {
        self.volume
    }
    pub fn position(&self) -> Option<PositionBounds> {
        self.position
    }
    pub fn sources(&self) -> &[MediaChoice] {
        &self.sources
    }
    pub fn groups(&self) -> &[MediaChoice] {
        &self.groups
    }
    pub fn supports(&self, action: &MediaAction) -> bool {
        match action {
            MediaAction::Operation(operation) => self.operations.contains(operation),
            MediaAction::Volume(value) => self
                .volume
                .is_some_and(|bounds| bounds.quantity().admits(value.value() as u64)),
            MediaAction::Position(value) => self
                .position
                .is_some_and(|bounds| bounds.quantity().admits(value.value() as u64)),
            MediaAction::Source(id) => self.sources.iter().any(|option| option.id() == id),
            MediaAction::Group(id) => self.groups.iter().any(|option| option.id() == id),
        }
    }
}
#[derive(Clone, PartialEq, Eq)]
pub struct ArtworkReference {
    bytes: Vec<u8>,
    expires: UtcInstant,
    media_source: Option<crate::api_content::MediaSourceName>,
}
impl std::fmt::Debug for ArtworkReference {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("ArtworkReference([REDACTED])")
    }
}
impl ArtworkReference {
    pub fn new(
        bytes: Vec<u8>,
        expires: UtcInstant,
        media_source: Option<crate::api_content::MediaSourceName>,
    ) -> Result<Self, PlaybackError> {
        if bytes.is_empty() || bytes.len() > 512 {
            return Err(PlaybackError::Limit);
        }
        Ok(Self {
            bytes,
            expires,
            media_source,
        })
    }
    pub fn media_source(&self) -> Option<&crate::api_content::MediaSourceName> {
        self.media_source.as_ref()
    }
    pub fn bytes(&self) -> &[u8] {
        &self.bytes
    }
    pub fn expires(&self) -> UtcInstant {
        self.expires
    }
}
/// Independent readings; converting this input admits relationships and freezes
/// it. Missing readings and metadata-only observations acquire no defaults.
pub struct PlaybackReadingInput {
    pub route: Option<MediaRoute>,
    pub playback: PlaybackState,
    pub title: Option<String>,
    pub artist: Option<String>,
    pub collection: Option<String>,
    pub volume: Option<VolumeBasisPoints>,
    pub position: Option<PositionMillis>,
    pub duration: Option<PositionMillis>,
    pub source: Option<MediaIdentifier>,
    pub group: Option<MediaIdentifier>,
    pub members: Vec<MediaMember>,
    pub artwork: Option<ArtworkReference>,
    pub quality: ObservationQuality,
    pub observed_time: Option<UtcInstant>,
    pub powered_on: Option<bool>,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PlaybackObservation {
    route: Option<MediaRoute>,
    playback: PlaybackState,
    title: Option<String>,
    artist: Option<String>,
    collection: Option<String>,
    volume: Option<VolumeBasisPoints>,
    position: Option<PositionMillis>,
    duration: Option<PositionMillis>,
    source: Option<MediaIdentifier>,
    group: Option<MediaIdentifier>,
    members: Vec<MediaMember>,
    artwork: Option<ArtworkReference>,
    quality: ObservationQuality,
    observed_time: Option<UtcInstant>,
    powered_on: Option<bool>,
}
impl PlaybackObservation {
    pub fn new(input: PlaybackReadingInput) -> Result<Self, PlaybackError> {
        for value in [&input.title, &input.artist, &input.collection]
            .into_iter()
            .flatten()
        {
            if !text(value, true) {
                return Err(PlaybackError::Text);
            }
        }
        if input
            .position
            .zip(input.duration)
            .is_some_and(|(position, duration)| position.value() > duration.value())
        {
            return Err(PlaybackError::Bounds);
        }
        if input.members.len() > 16 {
            return Err(PlaybackError::Limit);
        }
        if input.members.iter().enumerate().any(|(i, member)| {
            input.members[..i]
                .iter()
                .any(|old| old.entity == member.entity)
        }) {
            return Err(PlaybackError::Duplicate);
        }
        if input.route.is_none()
            && (input.playback != PlaybackState::Unknown
                || input.title.is_some()
                || input.artist.is_some()
                || input.collection.is_some()
                || input.volume.is_some()
                || input.position.is_some()
                || input.duration.is_some()
                || input.source.is_some()
                || input.group.is_some()
                || !input.members.is_empty()
                || input.artwork.is_some()
                || input.powered_on.is_some())
        {
            return Err(PlaybackError::Route);
        }
        Ok(Self {
            route: input.route,
            playback: input.playback,
            title: input.title,
            artist: input.artist,
            collection: input.collection,
            volume: input.volume,
            position: input.position,
            duration: input.duration,
            source: input.source,
            group: input.group,
            members: input.members,
            artwork: input.artwork,
            quality: input.quality,
            observed_time: input.observed_time,
            powered_on: input.powered_on,
        })
    }
    pub fn route(&self) -> Option<&MediaRoute> {
        self.route.as_ref()
    }
    pub fn playback(&self) -> PlaybackState {
        self.playback
    }
    pub fn title(&self) -> Option<&str> {
        self.title.as_deref()
    }
    pub fn artist(&self) -> Option<&str> {
        self.artist.as_deref()
    }
    pub fn collection(&self) -> Option<&str> {
        self.collection.as_deref()
    }
    pub fn volume(&self) -> Option<VolumeBasisPoints> {
        self.volume
    }
    pub fn position(&self) -> Option<PositionMillis> {
        self.position
    }
    pub fn duration(&self) -> Option<PositionMillis> {
        self.duration
    }
    pub fn source(&self) -> Option<&MediaIdentifier> {
        self.source.as_ref()
    }
    pub fn group(&self) -> Option<&MediaIdentifier> {
        self.group.as_ref()
    }
    pub fn members(&self) -> &[MediaMember] {
        &self.members
    }
    pub fn artwork(&self) -> Option<&ArtworkReference> {
        self.artwork.as_ref()
    }
    pub fn quality(&self) -> ObservationQuality {
        self.quality
    }
    pub fn observed_time(&self) -> Option<UtcInstant> {
        self.observed_time
    }
    pub fn powered_on(&self) -> Option<bool> {
        self.powered_on
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Playback {
    name: CapabilityName,
    etag: Option<DefinitionEtag>,
    access: ActionAccess,
    media_access: ActionAccess,
    definition: Option<PlaybackDefinition>,
    observation: Option<PlaybackObservation>,
    data_provenance: MediaDataProvenance,
}
impl Playback {
    pub fn new(
        name: CapabilityName,
        etag: Option<DefinitionEtag>,
        access: ActionAccess,
        media_access: ActionAccess,
        definition: Option<PlaybackDefinition>,
        observation: Option<PlaybackObservation>,
        data_provenance: MediaDataProvenance,
    ) -> Result<Self, PlaybackError> {
        if definition
            .as_ref()
            .and_then(|definition| definition.position())
            .zip(observation.as_ref().and_then(|reading| reading.duration()))
            .is_some_and(|(bounds, duration)| bounds.quantity().maximum() > duration.value() as u64)
        {
            return Err(PlaybackError::Bounds);
        }
        if let Some(route) = observation
            .as_ref()
            .and_then(|observation| observation.route())
        {
            if name
                .as_str()
                .rsplit_once("/capabilities/")
                .map(|(entity, _)| entity)
                != Some(route.entity().as_str())
            {
                return Err(PlaybackError::Route);
            }
            if observation.as_ref().is_some_and(|reading| {
                reading.members.iter().any(|member| {
                    member.entity.as_str().split('/').take(2).ne(route
                        .entity()
                        .as_str()
                        .split('/')
                        .take(2))
                })
            }) {
                return Err(PlaybackError::Route);
            }
        }
        if observation
            .as_ref()
            .and_then(|reading| reading.artwork())
            .and_then(|artwork| artwork.media_source())
            .is_some_and(|source| {
                source
                    .home()
                    .as_str()
                    .split('/')
                    .take(2)
                    .ne(name.as_str().split('/').take(2))
            })
        {
            return Err(PlaybackError::Route);
        }
        Ok(Self {
            name,
            etag,
            access,
            media_access,
            definition,
            observation,
            data_provenance,
        })
    }
    pub fn name(&self) -> &CapabilityName {
        &self.name
    }
    pub fn etag(&self) -> Option<&DefinitionEtag> {
        self.etag.as_ref()
    }
    pub fn definition(&self) -> Option<&PlaybackDefinition> {
        self.definition.as_ref()
    }
    pub fn observation(&self) -> Option<&PlaybackObservation> {
        self.observation.as_ref()
    }
    pub fn action_access(&self) -> ActionAccess {
        self.access
    }
    pub fn media_access(&self) -> ActionAccess {
        self.media_access
    }
    pub fn data_provenance(&self) -> MediaDataProvenance {
        self.data_provenance
    }
    pub fn can_prepare(&self) -> bool {
        self.definition.is_some()
            && self.access == ActionAccess::Allowed
            && self.media_access == ActionAccess::Allowed
            && self.etag.is_some()
            && self.observation.as_ref().is_some_and(|observation| {
                observation.route.is_some() && observation.quality.current_available()
            })
    }
}
