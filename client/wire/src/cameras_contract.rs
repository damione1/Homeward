//! Camera admission. Protected resources retain exact camera/session ownership.
//! No network, PNG decoder, timezone engine or authorization decision runs here.
use crate::home::cameras::v1 as wire;
use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::{Empty, Timestamp};
use home_client_model::cameras::{
    Availability, CameraId, EventId, EventKey, ImageKey, Provenance, SessionReference,
};
use std::{
    collections::BTreeSet,
    num::NonZeroU64,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

pub const MAX_CATALOG: usize = 128;
pub const MAX_METADATA_BYTES: usize = 64 * 1024;
pub const MAX_PAGE: usize = 64;
pub const MAX_EVENTS: usize = 10_000;
pub const MAX_CURSOR_BYTES: usize = 64;
pub const MAX_IMAGE_BYTES: usize = 512 * 1024;
pub const MAX_IMAGE_SIDE: u32 = 1024;
pub const MAX_LEASE: Duration = Duration::from_secs(60);
const MAX_LABEL_BYTES: usize = 1024;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AdmissionError {
    Invalid,
    Capacity,
    ForeignResource,
    Expired,
    Sequence,
    StaleRevision,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReadIssue {
    Missing,
    Expired,
    Revoked,
    WrongCamera,
    Unavailable,
    ResyncRequired,
}
pub enum Outcome<T> {
    Value(T),
    Refused(ReadIssue),
}

#[derive(Clone)]
pub struct Camera {
    pub id: CameraId,
    pub title: String,
    pub source: String,
    pub availability: Availability,
    pub provenance: Provenance,
}
#[derive(Clone, Debug)]
pub struct Lease {
    reference: SessionReference,
    issued_at: SystemTime,
    expires_at: SystemTime,
    deadline: Instant,
    revision: NonZeroU64,
}
impl Lease {
    pub fn reference(&self) -> &SessionReference {
        &self.reference
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
    pub fn revision(&self) -> u64 {
        self.revision.get()
    }
    pub fn live(&self, now: Instant) -> Result<(), AdmissionError> {
        if now < self.deadline {
            Ok(())
        } else {
            Err(AdmissionError::Expired)
        }
    }
    pub fn wire_reference(&self) -> wire::SessionRef {
        wire::SessionRef {
            entity_id: self.reference.camera().entity().as_str().into(),
            reference: self.reference.bytes().to_vec(),
            ..Default::default()
        }
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
    pub revision: NonZeroU64,
    pub events: Vec<Event>,
    pub next: Option<Cursor>,
    pub total: usize,
}
#[derive(Clone, PartialEq, Eq)]
pub struct Cursor(Vec<u8>);
impl std::fmt::Debug for Cursor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Cursor([REDACTED])")
    }
}
impl Cursor {
    pub fn bytes(&self) -> &[u8] {
        &self.0
    }
}
pub struct Image {
    pub key: ImageKey,
    pub captured_at: SystemTime,
    pub expires_at: SystemTime,
    pub deadline: Instant,
    pub png: Vec<u8>,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WatchFrame {
    Ready(u64),
    Changed(u64),
    Expired,
    Revoked,
    ResyncRequired,
}

fn clean_empty(value: &Empty) -> Result<(), AdmissionError> {
    if value.__buffa_unknown_fields.is_empty() {
        Ok(())
    } else {
        Err(AdmissionError::Invalid)
    }
}
fn label(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_LABEL_BYTES
        && !value.chars().any(|c| c.is_control() && c != '\n')
}
pub fn timestamp(value: &Timestamp) -> Result<SystemTime, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || !(-62_135_596_800..=253_402_300_799).contains(&value.seconds)
        || !(0..1_000_000_000).contains(&value.nanos)
    {
        return Err(AdmissionError::Invalid);
    }
    let seconds = Duration::from_secs(value.seconds.unsigned_abs());
    let start = if value.seconds < 0 {
        UNIX_EPOCH.checked_sub(seconds)
    } else {
        UNIX_EPOCH.checked_add(seconds)
    }
    .ok_or(AdmissionError::Invalid)?;
    start
        .checked_add(Duration::from_nanos(value.nanos as u64))
        .ok_or(AdmissionError::Invalid)
}
pub fn catalog(value: &wire::ListCamerasResponse) -> Result<Vec<Camera>, AdmissionError> {
    if value.cameras.len() > MAX_CATALOG || (value.encoded_len() as usize) > MAX_METADATA_BYTES {
        return Err(AdmissionError::Capacity);
    }
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    let mut seen = BTreeSet::new();
    value
        .cameras
        .iter()
        .map(|camera| {
            let id: CameraId = camera
                .entity_id
                .parse()
                .map_err(|_| AdmissionError::Invalid)?;
            if !camera.__buffa_unknown_fields.is_empty()
                || !label(&camera.title)
                || !label(&camera.source)
                || !seen.insert(id.clone())
            {
                return Err(AdmissionError::Invalid);
            }
            let availability = match camera.availability {
                EnumValue::Known(wire::Availability::Available) => Availability::Available,
                EnumValue::Known(wire::Availability::Unavailable) => Availability::Unavailable,
                EnumValue::Known(wire::Availability::Unknown) => Availability::Unknown,
                EnumValue::Known(wire::Availability::Unspecified) | EnumValue::Unknown(_) => {
                    return Err(AdmissionError::Invalid);
                }
            };
            let provenance = match camera.provenance {
                EnumValue::Known(wire::DataProvenance::Observed) => Provenance::Observed,
                EnumValue::Known(wire::DataProvenance::Synthetic) => Provenance::Synthetic,
                EnumValue::Known(wire::DataProvenance::Unspecified) | EnumValue::Unknown(_) => {
                    return Err(AdmissionError::Invalid);
                }
            };
            Ok(Camera {
                id,
                title: camera.title.clone(),
                source: camera.source.clone(),
                availability,
                provenance,
            })
        })
        .collect()
}
pub fn session_reference(value: &wire::SessionRef) -> Result<SessionReference, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    let camera = value
        .entity_id
        .parse()
        .map_err(|_| AdmissionError::Invalid)?;
    SessionReference::new(camera, &value.reference).map_err(|_| AdmissionError::Invalid)
}
fn same_session(value: Option<&wire::SessionRef>, lease: &Lease) -> Result<(), AdmissionError> {
    let reference = session_reference(value.ok_or(AdmissionError::Invalid)?)?;
    if &reference == lease.reference() {
        Ok(())
    } else {
        Err(AdmissionError::ForeignResource)
    }
}
pub fn lease(
    value: &wire::OpenSessionResponse,
    expected: &CameraId,
    wall: SystemTime,
    now: Instant,
) -> Result<Outcome<Lease>, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || (value.encoded_len() as usize) > MAX_METADATA_BYTES
    {
        return Err(AdmissionError::Invalid);
    }
    match value.result.as_ref().ok_or(AdmissionError::Invalid)? {
        wire::open_session_response::Result::Unavailable(v) => {
            clean_empty(v)?;
            Ok(Outcome::Refused(ReadIssue::Unavailable))
        }
        wire::open_session_response::Result::Lease(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            let reference =
                session_reference(v.session.as_option().ok_or(AdmissionError::Invalid)?)?;
            if reference.camera() != expected {
                return Err(AdmissionError::ForeignResource);
            }
            let issued_at = timestamp(v.issued_at.as_option().ok_or(AdmissionError::Invalid)?)?;
            let expires_at = timestamp(v.expires_at.as_option().ok_or(AdmissionError::Invalid)?)?;
            let span = expires_at
                .duration_since(issued_at)
                .map_err(|_| AdmissionError::Invalid)?;
            if span.is_zero() || span > MAX_LEASE || issued_at > wall {
                return Err(AdmissionError::Invalid);
            }
            let remaining = expires_at
                .duration_since(wall)
                .map_err(|_| AdmissionError::Expired)?;
            if remaining.is_zero() {
                return Err(AdmissionError::Expired);
            }
            let deadline = now.checked_add(remaining).ok_or(AdmissionError::Invalid)?;
            let revision = NonZeroU64::new(v.event_revision).ok_or(AdmissionError::Invalid)?;
            Ok(Outcome::Value(Lease {
                reference,
                issued_at,
                expires_at,
                deadline,
                revision,
            }))
        }
    }
}
pub fn image_key(value: &wire::ImageKey, lease: &Lease) -> Result<ImageKey, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    same_session(value.session.as_option(), lease)?;
    let camera = lease.reference().camera().clone();
    match value.target.as_ref().ok_or(AdmissionError::Invalid)? {
        wire::image_key::Target::Snapshot(v) => {
            clean_empty(v)?;
            Ok(ImageKey::Snapshot(camera))
        }
        wire::image_key::Target::EventId(v) => {
            let event = v.parse::<EventId>().map_err(|_| AdmissionError::Invalid)?;
            Ok(ImageKey::EventThumbnail(EventKey::new(camera, event)))
        }
    }
}
pub fn image_request(
    lease: &Lease,
    key: &ImageKey,
) -> Result<wire::ReadImageRequest, AdmissionError> {
    if !lease.reference().owns(key) {
        return Err(AdmissionError::ForeignResource);
    }
    let target = match key {
        ImageKey::Snapshot(_) => wire::image_key::Target::Snapshot(Box::default()),
        ImageKey::EventThumbnail(event) => {
            wire::image_key::Target::EventId(event.event().as_str().into())
        }
    };
    Ok(wire::ReadImageRequest {
        key: wire::ImageKey {
            session: lease.wire_reference().into(),
            target: Some(target),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    })
}
pub fn image(
    value: &wire::ReadImageResponse,
    lease: &Lease,
    expected: &ImageKey,
    wall: SystemTime,
    now: Instant,
) -> Result<Outcome<Image>, AdmissionError> {
    lease.live(now)?;
    if !lease.reference().owns(expected) {
        return Err(AdmissionError::ForeignResource);
    }
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    if (value.encoded_len() as usize) > MAX_IMAGE_BYTES + MAX_METADATA_BYTES {
        return Err(AdmissionError::Capacity);
    }
    use wire::read_image_response::Result as R;
    let issue = match value.result.as_ref().ok_or(AdmissionError::Invalid)? {
        R::Image(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            let key = image_key(v.key.as_option().ok_or(AdmissionError::Invalid)?, lease)?;
            if &key != expected {
                return Err(AdmissionError::ForeignResource);
            }
            if v.png.len() > MAX_IMAGE_BYTES {
                return Err(AdmissionError::Capacity);
            }
            if !v.png.starts_with(b"\x89PNG\r\n\x1a\n") {
                return Err(AdmissionError::Invalid);
            }
            let captured_at = timestamp(v.captured_at.as_option().ok_or(AdmissionError::Invalid)?)?;
            let expires_at = timestamp(v.expires_at.as_option().ok_or(AdmissionError::Invalid)?)?;
            if captured_at > wall || expires_at > lease.expires_at() || expires_at <= captured_at {
                return Err(AdmissionError::Invalid);
            }
            // Anchor image expiry to the original monotonic lease. A wall-clock
            // rollback between opening and downloading cannot extend private pixels.
            let shorter = lease
                .expires_at()
                .duration_since(expires_at)
                .map_err(|_| AdmissionError::Invalid)?;
            let deadline = lease
                .deadline()
                .checked_sub(shorter)
                .ok_or(AdmissionError::Expired)?;
            if deadline <= now {
                return Err(AdmissionError::Expired);
            }
            return Ok(Outcome::Value(Image {
                key,
                captured_at,
                expires_at,
                deadline,
                png: v.png.clone(),
            }));
        }
        R::Missing(v) => {
            clean_empty(v)?;
            ReadIssue::Missing
        }
        R::Expired(v) => {
            clean_empty(v)?;
            ReadIssue::Expired
        }
        R::Revoked(v) => {
            clean_empty(v)?;
            ReadIssue::Revoked
        }
        R::WrongCamera(v) => {
            clean_empty(v)?;
            ReadIssue::WrongCamera
        }
        R::Unavailable(v) => {
            clean_empty(v)?;
            ReadIssue::Unavailable
        }
    };
    Ok(Outcome::Refused(issue))
}
pub fn events(
    value: &wire::ListEventsResponse,
    lease: &Lease,
    wall: SystemTime,
    now: Instant,
) -> Result<Outcome<EventsPage>, AdmissionError> {
    lease.live(now)?;
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(AdmissionError::Invalid);
    }
    if (value.encoded_len() as usize) > MAX_METADATA_BYTES {
        return Err(AdmissionError::Capacity);
    }
    use wire::list_events_response::Result as R;
    let issue = match value.result.as_ref().ok_or(AdmissionError::Invalid)? {
        R::Page(v) => {
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            same_session(v.session.as_option(), lease)?;
            if v.events.len() > MAX_PAGE
                || v.total as usize > MAX_EVENTS
                || v.next_cursor.len() > MAX_CURSOR_BYTES
            {
                return Err(AdmissionError::Capacity);
            }
            if v.events.len() > v.total as usize
                || v.events.is_empty() && (v.total != 0 || !v.next_cursor.is_empty())
            {
                return Err(AdmissionError::Invalid);
            }
            let revision = NonZeroU64::new(v.revision).ok_or(AdmissionError::Invalid)?;
            if revision.get() < lease.revision() {
                return Err(AdmissionError::StaleRevision);
            }
            let mut seen = BTreeSet::new();
            let mut events = Vec::with_capacity(v.events.len());
            for event in &v.events {
                if !event.__buffa_unknown_fields.is_empty()
                    || !label(&event.title)
                    || !label(&event.source)
                {
                    return Err(AdmissionError::Invalid);
                }
                let camera = event
                    .entity_id
                    .parse::<CameraId>()
                    .map_err(|_| AdmissionError::Invalid)?;
                if &camera != lease.reference().camera() {
                    return Err(AdmissionError::ForeignResource);
                }
                let id = event
                    .event_id
                    .parse::<EventId>()
                    .map_err(|_| AdmissionError::Invalid)?;
                if !seen.insert(id.clone()) {
                    return Err(AdmissionError::Invalid);
                }
                let captured_at = timestamp(
                    event
                        .captured_at
                        .as_option()
                        .ok_or(AdmissionError::Invalid)?,
                )?;
                if captured_at > wall {
                    return Err(AdmissionError::Invalid);
                }
                let current = Event {
                    key: EventKey::new(camera, id),
                    title: event.title.clone(),
                    source: event.source.clone(),
                    captured_at,
                    has_image: event.has_image,
                };
                if events.last().is_some_and(|previous: &Event| {
                    current.captured_at > previous.captured_at
                        || current.captured_at == previous.captured_at
                            && current.key.event() <= previous.key.event()
                }) {
                    return Err(AdmissionError::Invalid);
                }
                events.push(current);
            }
            let next = (!v.next_cursor.is_empty()).then(|| Cursor(v.next_cursor.clone()));
            return Ok(Outcome::Value(EventsPage {
                revision,
                events,
                next,
                total: v.total as usize,
            }));
        }
        R::Expired(v) => {
            clean_empty(v)?;
            ReadIssue::Expired
        }
        R::Revoked(v) => {
            clean_empty(v)?;
            ReadIssue::Revoked
        }
        R::WrongCamera(v) => {
            clean_empty(v)?;
            ReadIssue::WrongCamera
        }
        R::Unavailable(v) => {
            clean_empty(v)?;
            ReadIssue::Unavailable
        }
        R::ResyncRequired(v) => {
            clean_empty(v)?;
            ReadIssue::ResyncRequired
        }
    };
    Ok(Outcome::Refused(issue))
}
pub fn watch(
    value: &wire::WatchSessionResponse,
    lease: &Lease,
    previous_sequence: u64,
    previous_revision: u64,
) -> Result<WatchFrame, AdmissionError> {
    if !value.__buffa_unknown_fields.is_empty()
        || (value.encoded_len() as usize) > MAX_METADATA_BYTES
    {
        return Err(AdmissionError::Invalid);
    }
    if previous_sequence.checked_add(1) != Some(value.sequence) {
        return Err(AdmissionError::Sequence);
    }
    use wire::watch_session_response::Frame as F;
    match value.frame.as_ref().ok_or(AdmissionError::Invalid)? {
        F::Ready(v) => {
            if previous_sequence != 0 {
                return Err(AdmissionError::Sequence);
            }
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            same_session(v.session.as_option(), lease)?;
            if timestamp(v.issued_at.as_option().ok_or(AdmissionError::Invalid)?)?
                != lease.issued_at()
                || timestamp(v.expires_at.as_option().ok_or(AdmissionError::Invalid)?)?
                    != lease.expires_at()
            {
                return Err(AdmissionError::Invalid);
            }
            if v.event_revision < lease.revision() {
                return Err(AdmissionError::StaleRevision);
            }
            Ok(WatchFrame::Ready(v.event_revision))
        }
        F::Changed(v) => {
            if previous_sequence == 0 {
                return Err(AdmissionError::Sequence);
            }
            if !v.__buffa_unknown_fields.is_empty() {
                return Err(AdmissionError::Invalid);
            }
            same_session(v.session.as_option(), lease)?;
            if v.revision <= previous_revision || v.revision < lease.revision() {
                return Err(AdmissionError::StaleRevision);
            }
            Ok(WatchFrame::Changed(v.revision))
        }
        F::Expired(v) => {
            clean_empty(v)?;
            Ok(WatchFrame::Expired)
        }
        F::Revoked(v) => {
            clean_empty(v)?;
            Ok(WatchFrame::Revoked)
        }
        F::ResyncRequired(v) => {
            clean_empty(v)?;
            Ok(WatchFrame::ResyncRequired)
        }
    }
}
