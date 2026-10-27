//! Camera-scoped identities. Names and provider labels never resolve a camera.
//! These values establish representation and ownership, never authorization.
use crate::EntityId;
use std::{error::Error, fmt, str::FromStr};

pub const MAX_ID_BYTES: usize = 256;
pub const SESSION_REFERENCE_BYTES: usize = 32;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Availability {
    Available,
    Unavailable,
    Unknown,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Provenance {
    Observed,
    Synthetic,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum IdentityError {
    Empty,
    TooLong,
    ControlCharacter,
}
impl fmt::Display for IdentityError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Empty => "camera identity must be nonempty",
            Self::TooLong => "camera identity exceeds the protocol byte limit",
            Self::ControlCharacter => "camera identity contains a control character",
        })
    }
}
impl Error for IdentityError {}

fn identity(value: &str) -> Result<(), IdentityError> {
    if value.is_empty() {
        return Err(IdentityError::Empty);
    }
    if value.len() > MAX_ID_BYTES {
        return Err(IdentityError::TooLong);
    }
    if value.chars().any(char::is_control) {
        return Err(IdentityError::ControlCharacter);
    }
    Ok(())
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct CameraId(EntityId);
impl TryFrom<EntityId> for CameraId {
    type Error = IdentityError;
    fn try_from(value: EntityId) -> Result<Self, Self::Error> {
        identity(value.as_str())?;
        Ok(Self(value))
    }
}
impl FromStr for CameraId {
    type Err = IdentityError;
    fn from_str(value: &str) -> Result<Self, Self::Err> {
        identity(value)?;
        Ok(Self(value.parse().map_err(|_| IdentityError::Empty)?))
    }
}
impl CameraId {
    pub fn entity(&self) -> &EntityId {
        &self.0
    }
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct EventId(String);
impl FromStr for EventId {
    type Err = IdentityError;
    fn from_str(value: &str) -> Result<Self, Self::Err> {
        identity(value)?;
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
    camera: CameraId,
    event: EventId,
}
impl EventKey {
    pub fn new(camera: CameraId, event: EventId) -> Self {
        Self { camera, event }
    }
    pub fn camera(&self) -> &CameraId {
        &self.camera
    }
    pub fn event(&self) -> &EventId {
        &self.event
    }
}

/// The complete image resource identity. A provider event ID is camera-scoped.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum ImageKey {
    Snapshot(CameraId),
    EventThumbnail(EventKey),
}
impl ImageKey {
    pub fn camera(&self) -> &CameraId {
        match self {
            Self::Snapshot(camera) => camera,
            Self::EventThumbnail(event) => event.camera(),
        }
    }
}

/// Short-lived opaque reference, never a URL or a permanently stored credential.
/// Expiry and current rights must still be checked on every server operation.
/// No formatting implementation exposes the reference bytes.
#[derive(Clone, PartialEq, Eq)]
pub struct SessionReference {
    camera: CameraId,
    reference: [u8; SESSION_REFERENCE_BYTES],
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct InvalidSessionReference;
impl fmt::Display for InvalidSessionReference {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("camera session reference has an invalid length")
    }
}
impl Error for InvalidSessionReference {}
impl fmt::Debug for SessionReference {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("SessionReference")
            .field("camera", &self.camera)
            .field("reference", &"[REDACTED]")
            .finish()
    }
}
impl SessionReference {
    pub fn new(camera: CameraId, value: &[u8]) -> Result<Self, InvalidSessionReference> {
        let reference = value.try_into().map_err(|_| InvalidSessionReference)?;
        Ok(Self { camera, reference })
    }
    pub fn camera(&self) -> &CameraId {
        &self.camera
    }
    pub fn bytes(&self) -> &[u8; SESSION_REFERENCE_BYTES] {
        &self.reference
    }
    pub fn owns(&self, image: &ImageKey) -> bool {
        &self.camera == image.camera()
    }
}
