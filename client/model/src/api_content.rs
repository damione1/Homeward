//! Canonical content sources and caller-bound session metadata. Resource names
//! establish identity only; they never prove permission or open provider I/O.
use crate::{
    api::{EntityName, HomeName},
    api_media::MediaDataProvenance,
    api_observations::ObservationQuality,
    local_time::UtcInstant,
};
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ContentError {
    Name,
    Etag,
    Kind,
    Label,
    Time,
    ForeignSource,
    Limit,
}
fn resource_home(value: &str, collection: &str) -> Result<HomeName, ContentError> {
    let mut parts = value.split('/');
    let home_collection = parts.next().ok_or(ContentError::Name)?;
    let home = parts.next().ok_or(ContentError::Name)?;
    if parts.next() != Some(collection) || parts.next().is_none() || parts.next().is_some() {
        return Err(ContentError::Name);
    }
    HomeName::try_from(format!("{home_collection}/{home}").as_str()).map_err(|_| ContentError::Name)
}
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct MediaSourceName {
    value: String,
    home: HomeName,
}
impl TryFrom<&str> for MediaSourceName {
    type Error = ContentError;
    fn try_from(value: &str) -> Result<Self, ContentError> {
        if value.len() > 160 {
            return Err(ContentError::Name);
        }
        let home = resource_home(value, "mediaSources")?;
        let id = value
            .rsplit('/')
            .next()
            .ok_or(ContentError::Name)?
            .strip_prefix("source-")
            .ok_or(ContentError::Name)?;
        if id.len() != 48
            || !id
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        {
            return Err(ContentError::Name);
        }
        Ok(Self {
            value: value.to_owned(),
            home,
        })
    }
}
impl MediaSourceName {
    pub fn as_str(&self) -> &str {
        &self.value
    }
    pub fn home(&self) -> &HomeName {
        &self.home
    }
}
#[derive(Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct MediaSessionName {
    value: String,
    home: HomeName,
}
impl std::fmt::Debug for MediaSessionName {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("MediaSessionName([REDACTED])")
    }
}
impl TryFrom<&str> for MediaSessionName {
    type Error = ContentError;
    fn try_from(value: &str) -> Result<Self, ContentError> {
        if value.len() > 160 {
            return Err(ContentError::Name);
        }
        let home = resource_home(value, "mediaSessions")?;
        let id = value
            .rsplit('/')
            .next()
            .ok_or(ContentError::Name)?
            .strip_prefix("s-")
            .ok_or(ContentError::Name)?;
        if id.len() != 52
            || !id
                .bytes()
                .all(|b| b.is_ascii_lowercase() || (b'2'..=b'7').contains(&b))
            || !matches!(id.as_bytes().last(), Some(b'a' | b'q'))
        {
            return Err(ContentError::Name);
        }
        Ok(Self {
            value: value.to_owned(),
            home,
        })
    }
}
impl MediaSessionName {
    pub fn as_str(&self) -> &str {
        &self.value
    }
    pub fn home(&self) -> &HomeName {
        &self.home
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum ContentKind {
    Artwork,
    Camera,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct EventsEtag(String);
impl TryFrom<&str> for EventsEtag {
    type Error = ContentError;
    fn try_from(value: &str) -> Result<Self, ContentError> {
        if value.len() < 3
            || value.len() > 256
            || !value.starts_with('"')
            || !value.ends_with('"')
            || !value.as_bytes()[1..value.len() - 1]
                .iter()
                .all(|b| b.is_ascii_graphic() && *b != b'"')
        {
            return Err(ContentError::Etag);
        }
        Ok(Self(value.to_owned()))
    }
}
impl EventsEtag {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MediaSource {
    name: MediaSourceName,
    entity: EntityName,
    display_name: String,
    kinds: Vec<ContentKind>,
    quality: ObservationQuality,
    provenance: MediaDataProvenance,
}
impl MediaSource {
    pub fn new(
        name: MediaSourceName,
        entity: EntityName,
        display_name: &str,
        kinds: Vec<ContentKind>,
        quality: ObservationQuality,
        provenance: MediaDataProvenance,
    ) -> Result<Self, ContentError> {
        if !entity
            .as_str()
            .starts_with(&format!("{}/entities/", name.home().as_str()))
        {
            return Err(ContentError::ForeignSource);
        }
        if display_name.is_empty()
            || display_name.len() > 1024
            || display_name.chars().any(|c| c.is_control() && c != '\n')
        {
            return Err(ContentError::Label);
        }
        if kinds.is_empty()
            || kinds.len() > 2
            || kinds
                .iter()
                .enumerate()
                .any(|(i, k)| kinds[..i].contains(k))
        {
            return Err(ContentError::Kind);
        }
        Ok(Self {
            name,
            entity,
            display_name: display_name.to_owned(),
            kinds,
            quality,
            provenance,
        })
    }
    pub fn name(&self) -> &MediaSourceName {
        &self.name
    }
    pub fn entity(&self) -> &EntityName {
        &self.entity
    }
    pub fn display_name(&self) -> &str {
        &self.display_name
    }
    pub fn kinds(&self) -> &[ContentKind] {
        &self.kinds
    }
    pub fn quality(&self) -> ObservationQuality {
        self.quality
    }
    pub fn data_provenance(&self) -> MediaDataProvenance {
        self.provenance
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MediaSession {
    name: MediaSessionName,
    source: MediaSourceName,
    kind: ContentKind,
    created: UtcInstant,
    expires: UtcInstant,
    events_etag: Option<EventsEtag>,
}
impl MediaSession {
    pub fn new(
        name: MediaSessionName,
        source: MediaSourceName,
        kind: ContentKind,
        created: UtcInstant,
        expires: UtcInstant,
        events_etag: Option<EventsEtag>,
    ) -> Result<Self, ContentError> {
        if matches!(
            (kind, events_etag.as_ref()),
            (ContentKind::Camera, None) | (ContentKind::Artwork, Some(_))
        ) {
            return Err(ContentError::Etag);
        }
        if name.home() != source.home() {
            return Err(ContentError::ForeignSource);
        }
        let duration = (i128::from(expires.seconds()) - i128::from(created.seconds()))
            * 1_000_000_000
            + i128::from(expires.nanos())
            - i128::from(created.nanos());
        if created.nanos() % 1000 != 0
            || expires.nanos() % 1000 != 0
            || !(1..=60_000_000_000).contains(&duration)
        {
            return Err(ContentError::Time);
        }
        Ok(Self {
            name,
            source,
            kind,
            created,
            expires,
            events_etag,
        })
    }
    pub fn name(&self) -> &MediaSessionName {
        &self.name
    }
    pub fn source(&self) -> &MediaSourceName {
        &self.source
    }
    pub fn kind(&self) -> ContentKind {
        self.kind
    }
    pub fn created(&self) -> UtcInstant {
        self.created
    }
    pub fn expires(&self) -> UtcInstant {
        self.expires
    }
    pub fn events_etag(&self) -> Option<&EventsEtag> {
        self.events_etag.as_ref()
    }
    /// Watch metadata may change the event image, never renew the original lease.
    pub fn same_lease(&self, other: &Self) -> bool {
        self.name == other.name
            && self.source == other.source
            && self.kind == other.kind
            && self.created == other.created
            && self.expires == other.expires
    }
}

impl std::str::FromStr for MediaSourceName {
    type Err = ContentError;
    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::try_from(value)
    }
}
impl std::fmt::Display for MediaSourceName {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

impl std::fmt::Display for ContentError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "invalid content {:?}", self)
    }
}
impl std::error::Error for ContentError {}
