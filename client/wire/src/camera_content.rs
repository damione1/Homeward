//! Private camera presentation of MediaSource/MediaSession resources. Image and
//! event values are anonymous projections, bound to the exact live session.
use crate::{content_contract, home::api::v2 as wire};
use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::{
    api_content::{ContentKind, EventsEtag, MediaSession, MediaSessionName, MediaSourceName},
    api_media::MediaDataProvenance,
    api_observations::Availability,
    cameras::{EventId, EventKey, ImageKey},
};
use std::{
    collections::BTreeSet,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
pub const MAX_CATALOG: usize = 128;
pub const MAX_METADATA_BYTES: usize = 64 * 1024;
pub const MAX_PAGE: usize = 256;
pub const MAX_EVENTS: usize = 10_000;
pub const MAX_CURSOR_BYTES: usize = 4096;
pub const MAX_IMAGE_BYTES: usize = 512 * 1024;
pub const MAX_IMAGE_SIDE: u32 = 1024;
pub const MAX_LEASE: Duration = Duration::from_secs(60);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AdmissionError {
    Invalid,
    Capacity,
    ForeignResource,
    Expired,
    Sequence,
    StaleRevision,
}
/// Presentation metadata only; this is not an additional public camera resource.
#[derive(Clone)]
pub struct Camera {
    pub id: MediaSourceName,
    pub title: String,
    pub source: String,
    pub availability: Availability,
    pub provenance: MediaDataProvenance,
}
#[derive(Clone, Debug)]
pub struct Lease {
    session: MediaSession,
    issued_at: SystemTime,
    expires_at: SystemTime,
    deadline: Instant,
}
impl Lease {
    pub fn session(&self) -> &MediaSession {
        &self.session
    }
    pub fn name(&self) -> &MediaSessionName {
        self.session.name()
    }
    pub fn source(&self) -> &MediaSourceName {
        self.session.source()
    }
    pub fn issued_at(&self) -> SystemTime {
        self.issued_at
    }
    pub fn expires_at(&self) -> SystemTime {
        self.expires_at
    }
    pub fn deadline(&self) -> Instant {
        self.deadline
    }
    pub fn events_etag(&self) -> Option<&EventsEtag> {
        self.session.events_etag()
    }
    pub fn live(&self, now: Instant) -> Result<(), AdmissionError> {
        if now < self.deadline {
            Ok(())
        } else {
            Err(AdmissionError::Expired)
        }
    }
    pub fn owns(&self, key: &ImageKey) -> bool {
        self.source() == key.source()
    }
}
#[derive(Clone)]
pub struct Event {
    pub key: EventKey,
    pub title: String,
    pub source: String,
    pub captured_at: SystemTime,
    pub has_image: bool,
}
#[derive(Clone)]
pub struct EventsPage {
    pub etag: EventsEtag,
    pub events: Vec<Event>,
    pub next: Option<Cursor>,
    pub total: usize,
}
#[derive(Clone, PartialEq, Eq)]
pub struct Cursor(String);
impl std::fmt::Debug for Cursor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Cursor([REDACTED])")
    }
}
impl Cursor {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
pub struct Image {
    pub key: ImageKey,
    pub session: MediaSessionName,
    pub captured_at: SystemTime,
    pub expires_at: SystemTime,
    pub deadline: Instant,
    pub png: Vec<u8>,
    pub kind: ContentKind,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum WatchFrame {
    Ready(EventsEtag),
    Changed(EventsEtag),
}
pub fn timestamp(value: &Timestamp) -> Result<SystemTime, AdmissionError> {
    let value = content_contract::instant(value).map_err(|_| AdmissionError::Invalid)?;
    let offset = Duration::from_secs(value.seconds().unsigned_abs());
    let start = if value.seconds() < 0 {
        UNIX_EPOCH.checked_sub(offset)
    } else {
        UNIX_EPOCH.checked_add(offset)
    }
    .ok_or(AdmissionError::Invalid)?;
    start
        .checked_add(Duration::from_nanos(value.nanos() as u64))
        .ok_or(AdmissionError::Invalid)
}
fn label(value: &str) -> bool {
    !value.is_empty() && value.len() <= 1024 && !value.chars().any(|c| c.is_control() && c != '\n')
}
pub fn catalog(
    value: &wire::ListMediaSourcesResponse,
    home: &home_client_model::api::HomeName,
) -> Result<Vec<Camera>, AdmissionError> {
    let sources =
        content_contract::source_page(value, home).map_err(|_| AdmissionError::Invalid)?;
    let mut cameras = Vec::new();
    for source in sources {
        if !source.kinds().contains(&ContentKind::Camera) {
            continue;
        }
        cameras.push(Camera {
            id: source.name().clone(),
            title: source.display_name().to_owned(),
            source: source.entity().as_str().to_owned(),
            availability: source.quality().availability(),
            provenance: source.data_provenance(),
        });
        if cameras.len() > MAX_CATALOG {
            return Err(AdmissionError::Capacity);
        }
    }
    Ok(cameras)
}
pub fn lease(
    value: &wire::MediaSession,
    expected: &MediaSourceName,
    kind: ContentKind,
    wall: SystemTime,
    now: Instant,
) -> Result<Lease, AdmissionError> {
    let session = content_contract::session(value).map_err(|_| AdmissionError::Invalid)?;
    if session.source() != expected || session.kind() != kind {
        return Err(AdmissionError::ForeignResource);
    }
    let issued_at = timestamp(
        value
            .create_time
            .as_option()
            .ok_or(AdmissionError::Invalid)?,
    )?;
    let expires_at = timestamp(
        value
            .expire_time
            .as_option()
            .ok_or(AdmissionError::Invalid)?,
    )?;
    if issued_at > wall {
        return Err(AdmissionError::Invalid);
    }
    let remaining = expires_at
        .duration_since(wall)
        .map_err(|_| AdmissionError::Expired)?;
    if remaining.is_zero() {
        return Err(AdmissionError::Expired);
    }
    let deadline = now.checked_add(remaining).ok_or(AdmissionError::Invalid)?;
    Ok(Lease {
        session,
        issued_at,
        expires_at,
        deadline,
    })
}
fn target(value: &wire::MediaImageTarget, lease: &Lease) -> Result<ImageKey, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    match &value.value {
        Some(wire::media_image_target::Value::Snapshot(empty))
            if empty.__buffa_unknown_fields.is_empty() =>
        {
            Ok(ImageKey::Snapshot(lease.source().clone()))
        }
        Some(wire::media_image_target::Value::EventId(value))
            if lease.session.kind() == ContentKind::Camera =>
        {
            Ok(ImageKey::EventThumbnail(EventKey::new(
                lease.source().clone(),
                value
                    .parse::<EventId>()
                    .map_err(|_| AdmissionError::Invalid)?,
            )))
        }
        Some(
            wire::media_image_target::Value::Snapshot(_)
            | wire::media_image_target::Value::EventId(_),
        )
        | None => Err(AdmissionError::Invalid),
    }
}
pub fn image_request(
    lease: &Lease,
    key: &ImageKey,
) -> Result<wire::ReadMediaSessionRequest, AdmissionError> {
    if !lease.owns(key) {
        return Err(AdmissionError::ForeignResource);
    }
    let value = match key {
        ImageKey::Snapshot(_) => wire::media_image_target::Value::Snapshot(Default::default()),
        ImageKey::EventThumbnail(event) => {
            if lease.session.kind() != ContentKind::Camera {
                return Err(AdmissionError::ForeignResource);
            }
            wire::media_image_target::Value::EventId(event.event().as_str().to_owned())
        }
    };
    Ok(wire::ReadMediaSessionRequest {
        name: lease.name().as_str().to_owned(),
        target: wire::MediaImageTarget {
            value: Some(value),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    })
}
pub fn image(
    value: &wire::ReadMediaSessionResponse,
    lease: &Lease,
    expected: &ImageKey,
    wall: SystemTime,
    now: Instant,
) -> Result<Image, AdmissionError> {
    lease.live(now)?;
    if !lease.owns(expected) {
        return Err(AdmissionError::ForeignResource);
    }
    if !value.__buffa_unknown_fields.is_empty()
        || value.encoded_len() as usize > MAX_IMAGE_BYTES + MAX_METADATA_BYTES
    {
        return Err(AdmissionError::Capacity);
    }
    let value = value.image.as_option().ok_or(AdmissionError::Invalid)?;
    if !value.__buffa_unknown_fields.is_empty() || value.media_session != lease.name().as_str() {
        return Err(AdmissionError::ForeignResource);
    }
    let key = target(
        value.target.as_option().ok_or(AdmissionError::Invalid)?,
        lease,
    )?;
    if &key != expected {
        return Err(AdmissionError::ForeignResource);
    }
    let captured_at = timestamp(
        value
            .capture_time
            .as_option()
            .ok_or(AdmissionError::Invalid)?,
    )?;
    let expires_at = timestamp(
        value
            .expire_time
            .as_option()
            .ok_or(AdmissionError::Invalid)?,
    )?;
    if captured_at > wall || expires_at != lease.expires_at() || captured_at >= expires_at {
        return Err(AdmissionError::Invalid);
    }
    let (kind, max_bytes, max_side) = match lease.session.kind() {
        ContentKind::Artwork => (ContentKind::Artwork, 128 * 1024, 512),
        ContentKind::Camera => (ContentKind::Camera, MAX_IMAGE_BYTES, MAX_IMAGE_SIDE),
    };
    if value.png.len() > max_bytes {
        return Err(AdmissionError::Capacity);
    }
    // PNG dimensions are admitted before the bounded off-thread decoder allocates.
    if value.png.len() < 24
        || !value.png.starts_with(b"\x89PNG\r\n\x1a\n")
        || value.png[12..16] != *b"IHDR"
    {
        return Err(AdmissionError::Invalid);
    }
    let width = u32::from_be_bytes(
        value.png[16..20]
            .try_into()
            .map_err(|_| AdmissionError::Invalid)?,
    );
    let height = u32::from_be_bytes(
        value.png[20..24]
            .try_into()
            .map_err(|_| AdmissionError::Invalid)?,
    );
    if width == 0 || height == 0 || width > max_side || height > max_side {
        return Err(AdmissionError::Capacity);
    }
    Ok(Image {
        key,
        session: lease.name().clone(),
        captured_at,
        expires_at,
        deadline: lease.deadline(),
        png: value.png.clone(),
        kind,
    })
}
pub fn events(
    value: &wire::QueryMediaSessionEventsResponse,
    lease: &Lease,
    wall: SystemTime,
    now: Instant,
) -> Result<EventsPage, AdmissionError> {
    lease.live(now)?;
    if lease.session.kind() != ContentKind::Camera || value.media_session != lease.name().as_str() {
        return Err(AdmissionError::ForeignResource);
    }
    if !value.__buffa_unknown_fields.is_empty()
        || value.encoded_len() as usize > MAX_METADATA_BYTES
        || value.media_events.len() > MAX_PAGE
        || value.next_page_token.len() > MAX_CURSOR_BYTES
    {
        return Err(AdmissionError::Capacity);
    }
    let total = usize::try_from(value.total_size).map_err(|_| AdmissionError::Invalid)?;
    if total > MAX_EVENTS || value.media_events.len() > total {
        return Err(AdmissionError::Capacity);
    }
    let etag =
        EventsEtag::try_from(value.events_etag.as_str()).map_err(|_| AdmissionError::Invalid)?;
    let mut seen = BTreeSet::new();
    let mut events = Vec::with_capacity(value.media_events.len());
    for event in &value.media_events {
        if !event.__buffa_unknown_fields.is_empty()
            || !label(&event.display_name)
            || !label(&event.source)
        {
            return Err(AdmissionError::Invalid);
        }
        let id = event
            .event_id
            .parse::<EventId>()
            .map_err(|_| AdmissionError::Invalid)?;
        if !seen.insert(id.clone())
            || events
                .last()
                .is_some_and(|previous: &Event| previous.key.event() >= &id)
        {
            return Err(AdmissionError::Invalid);
        }
        let captured_at = timestamp(
            event
                .capture_time
                .as_option()
                .ok_or(AdmissionError::Invalid)?,
        )?;
        if captured_at > wall {
            return Err(AdmissionError::Invalid);
        }
        events.push(Event {
            key: EventKey::new(lease.source().clone(), id),
            title: event.display_name.clone(),
            source: event.source.clone(),
            captured_at,
            has_image: event.has_image,
        });
    }
    Ok(EventsPage {
        etag,
        events,
        total,
        next: (!value.next_page_token.is_empty()).then(|| Cursor(value.next_page_token.clone())),
    })
}
pub fn watch(
    value: &wire::WatchMediaSessionResponse,
    lease: &Lease,
    previous_sequence: u64,
    previous_etag: Option<&EventsEtag>,
) -> Result<WatchFrame, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || value.encoded_len() as usize > MAX_METADATA_BYTES
        || u64::try_from(value.sequence).ok() != previous_sequence.checked_add(1)
    {
        return Err(AdmissionError::Sequence);
    }
    match &value.frame {
        Some(wire::watch_media_session_response::Frame::Ready(value)) => {
            if previous_sequence != 0 {
                return Err(AdmissionError::Sequence);
            }
            let session = content_contract::session(value).map_err(|_| AdmissionError::Invalid)?;
            if !lease.session.same_lease(&session) {
                return Err(AdmissionError::ForeignResource);
            }
            Ok(WatchFrame::Ready(
                session
                    .events_etag()
                    .ok_or(AdmissionError::Invalid)?
                    .clone(),
            ))
        }
        Some(wire::watch_media_session_response::Frame::Changed(value)) => {
            if previous_sequence == 0 || !value.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Sequence);
            }
            let etag = EventsEtag::try_from(value.events_etag.as_str())
                .map_err(|_| AdmissionError::Invalid)?;
            if previous_etag == Some(&etag) {
                return Err(AdmissionError::StaleRevision);
            }
            Ok(WatchFrame::Changed(etag))
        }
        None => Err(AdmissionError::Invalid),
    }
}
