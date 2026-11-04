//! Exact media adapter. The server owns routing and capabilities; quantities
//! remain integer basis points/milliseconds until the renderer projects them.
use home_client_model::{
    api::{Command, Deadline, EntityName, Intent},
    api_media::{MediaAction, MediaDataProvenance, MediaIntent},
    api_playback::{Playback, QuantityBounds},
};
use std::{
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Field {
    Volume,
    Position,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Motion {
    Previous,
    Next,
    First,
    Last,
    PagePrevious,
    PageNext,
}
impl Field {
    pub fn token(self) -> i32 {
        match self {
            Self::Volume => 1,
            Self::Position => 2,
        }
    }
    pub fn from_token(token: i32) -> Option<Self> {
        match token {
            1 => Some(Self::Volume),
            2 => Some(Self::Position),
            _ => None,
        }
    }
    fn decimals(self) -> u32 {
        match self {
            Self::Volume => 2,
            Self::Position => 3,
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct VolumeBasisPoints(u32);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PositionMillis(u64);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Quantity {
    Volume(VolumeBasisPoints),
    Position(PositionMillis),
}
impl Quantity {
    pub fn field(self) -> Field {
        match self {
            Self::Volume(_) => Field::Volume,
            Self::Position(_) => Field::Position,
        }
    }
    pub fn canonical(self) -> u64 {
        match self {
            Self::Volume(v) => u64::from(v.0),
            Self::Position(v) => v.0,
        }
    }
    pub fn text(self) -> String {
        let places = self.field().decimals();
        let divisor = 10_u64.pow(places);
        let value = self.canonical();
        format!(
            "{}.{:0width$}",
            value / divisor,
            value % divisor,
            width = places as usize
        )
    }
    fn command(self) -> Result<MediaAction, Refusal> {
        match self {
            Self::Volume(v) => Ok(MediaAction::Volume(
                home_client_model::api_media::VolumeBasisPoints::try_from(
                    i32::try_from(v.0).map_err(|_| Refusal::Bounds)?,
                )
                .map_err(|_| Refusal::Bounds)?,
            )),
            Self::Position(v) => Ok(MediaAction::Position(
                home_client_model::api_media::PositionMillis::try_from(
                    i64::try_from(v.0).map_err(|_| Refusal::Bounds)?,
                )
                .map_err(|_| Refusal::Bounds)?,
            )),
        }
    }
    fn new(field: Field, value: u64) -> Result<Self, Refusal> {
        match field {
            Field::Volume => u32::try_from(value)
                .ok()
                .filter(|v| *v <= 10_000)
                .map(|v| Self::Volume(VolumeBasisPoints(v)))
                .ok_or(Refusal::Bounds),
            Field::Position => (value <= home_client_model::api_media::MAX_POSITION_MILLIS as u64)
                .then_some(Self::Position(PositionMillis(value)))
                .ok_or(Refusal::Bounds),
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Refusal {
    ContextUnavailable,
    Contract,
    Unsupported,
    Permission,
    Invalid,
    Precision,
    Bounds,
    Step,
}
#[derive(Clone)]
pub struct Data {
    playback: Playback,
}
impl Data {
    pub fn moved(&self, value: Quantity, motion: Motion) -> Result<Quantity, Refusal> {
        let limits = self.limits(value.field()).ok_or(Refusal::Unsupported)?;
        let count = (limits.maximum() - limits.minimum()) / limits.step();
        let current = (value.canonical().clamp(limits.minimum(), limits.maximum())
            - limits.minimum())
            / limits.step();
        let next = match motion {
            Motion::Previous => current.saturating_sub(1),
            Motion::Next => current.saturating_add(1).min(count),
            Motion::First => 0,
            Motion::Last => count,
            Motion::PagePrevious => current.saturating_sub(10),
            Motion::PageNext => current.saturating_add(10).min(count),
        };
        Quantity::new(value.field(), limits.minimum() + next * limits.step())
    }
    pub fn new(playback: Playback, expected: &EntityName) -> Result<Self, Refusal> {
        if playback
            .name()
            .as_str()
            .rsplit_once("/capabilities/")
            .map(|(entity, _)| entity)
            != Some(expected.as_str())
        {
            return Err(Refusal::Contract);
        }
        Ok(Self { playback })
    }
    pub fn snapshot(&self) -> &Playback {
        &self.playback
    }
    pub fn coordinator(&self) -> Option<&EntityName> {
        self.playback
            .observation()?
            .route()
            .map(|route| route.coordinator())
    }
    pub fn same_controls(&self, other: &Self) -> bool {
        self.playback.name() == other.playback.name()
            && self.playback.etag() == other.playback.etag()
            && self.playback.action_access() == other.playback.action_access()
            && self.playback.media_access() == other.playback.media_access()
            && self.playback.definition() == other.playback.definition()
            && self.playback.data_provenance() == other.playback.data_provenance()
            && self
                .playback
                .observation()
                .map(|reading| (reading.route(), reading.quality()))
                == other
                    .playback
                    .observation()
                    .map(|reading| (reading.route(), reading.quality()))
    }
    pub fn limits(&self, field: Field) -> Option<QuantityBounds> {
        let definition = self.playback.definition()?;
        match field {
            Field::Volume => definition.volume().map(|bounds| bounds.quantity()),
            Field::Position => definition.position().map(|bounds| bounds.quantity()),
        }
    }
    pub fn observed(&self, field: Field) -> Option<Quantity> {
        let reading = self.playback.observation()?;
        match field {
            Field::Volume => reading
                .volume()
                .map(|value| Quantity::Volume(VolumeBasisPoints(value.value() as u32))),
            Field::Position => reading
                .position()
                .map(|value| Quantity::Position(PositionMillis(value.value() as u64))),
        }
    }
    pub fn parse(&self, field: Field, text: &str) -> Result<Quantity, Refusal> {
        let limits = self.limits(field).ok_or(Refusal::Unsupported)?;
        let value = parse_fixed(text, field.decimals())?;
        if value < limits.minimum() || value > limits.maximum() {
            return Err(Refusal::Bounds);
        }
        if !limits.admits(value) {
            return Err(Refusal::Step);
        }
        Quantity::new(field, value)
    }
    /// Renderer positions are approximate; the resulting canonical step is
    /// checked again and is the only value eligible for submission.
    pub fn at_position(&self, field: Field, position: f32) -> Result<Quantity, Refusal> {
        if !position.is_finite() {
            return Err(Refusal::Invalid);
        }
        let limits = self.limits(field).ok_or(Refusal::Unsupported)?;
        let count = (limits.maximum() - limits.minimum()) / limits.step();
        let step = (f64::from(position.clamp(0., 1.)) * count as f64).round() as u64;
        let value = limits
            .minimum()
            .checked_add(step.checked_mul(limits.step()).ok_or(Refusal::Bounds)?)
            .ok_or(Refusal::Bounds)?;
        if !limits.admits(value) {
            return Err(Refusal::Bounds);
        }
        Quantity::new(field, value)
    }
    pub fn position(&self, quantity: Quantity) -> f32 {
        let Some(limits) = self.limits(quantity.field()) else {
            return 0.;
        };
        if limits.maximum() == limits.minimum() {
            return 0.;
        }
        ((quantity.canonical().saturating_sub(limits.minimum()) as f64
            / (limits.maximum() - limits.minimum()) as f64)
            .clamp(0., 1.)) as f32
    }
    pub fn quantity_request(&self, value: Quantity) -> Result<Command, Refusal> {
        self.request(value.command()?)
    }
    pub fn request(&self, action: MediaAction) -> Result<Command, Refusal> {
        if !self.playback.can_prepare()
            || !self
                .playback
                .definition()
                .is_some_and(|definition| definition.supports(&action))
        {
            return Err(Refusal::Permission);
        }
        let route = self
            .playback
            .observation()
            .and_then(|reading| reading.route())
            .ok_or(Refusal::Unsupported)?
            .clone();
        let etag = self.playback.etag().ok_or(Refusal::Permission)?.clone();
        let expires = SystemTime::now()
            .checked_add(Duration::from_secs(10))
            .and_then(|value| value.duration_since(UNIX_EPOCH).ok())
            .ok_or(Refusal::ContextUnavailable)?;
        let deadline = Deadline::new(
            i64::try_from(expires.as_secs()).map_err(|_| Refusal::ContextUnavailable)?,
            expires.subsec_nanos() as i32,
        )
        .map_err(|_| Refusal::ContextUnavailable)?;
        let request_id =
            home_client_actions::new_request_id().map_err(|_| Refusal::ContextUnavailable)?;
        Ok(Command::new(
            self.playback.name().clone(),
            Intent::Media(MediaIntent::new(route, action)),
            Some(request_id),
            deadline,
        )
        .with_definition_etag(etag))
    }
}

fn parse_fixed(text: &str, decimals: u32) -> Result<u64, Refusal> {
    let text = text.trim();
    if text.is_empty() || text.len() > 64 {
        return Err(Refusal::Invalid);
    }
    let mut parts = text.split('.');
    let whole = parts.next().ok_or(Refusal::Invalid)?;
    let fractional = parts.next().unwrap_or("");
    if parts.next().is_some()
        || whole.is_empty() && fractional.is_empty()
        || !whole
            .bytes()
            .chain(fractional.bytes())
            .all(|b| b.is_ascii_digit())
    {
        return Err(Refusal::Invalid);
    }
    if fractional.len() > decimals as usize {
        return Err(Refusal::Precision);
    }
    let whole = if whole.is_empty() {
        0
    } else {
        whole.parse::<u64>().map_err(|_| Refusal::Bounds)?
    };
    let fraction = if fractional.is_empty() {
        0
    } else {
        fractional.parse::<u64>().map_err(|_| Refusal::Invalid)?
    };
    whole
        .checked_mul(10_u64.pow(decimals))
        .and_then(|v| v.checked_add(fraction * 10_u64.pow(decimals - fractional.len() as u32)))
        .ok_or(Refusal::Bounds)
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ArtworkFailure {
    Invalid,
    Capacity,
    Expired,
}
#[derive(Clone)]
pub struct Artwork {
    provenance: MediaDataProvenance,
    width: u32,
    height: u32,
    rgba: Arc<[u8]>,
    expires: SystemTime,
    deadline: std::time::Instant,
}
impl Artwork {
    pub fn provenance(&self) -> MediaDataProvenance {
        self.provenance
    }
    pub fn width(&self) -> u32 {
        self.width
    }
    pub fn height(&self) -> u32 {
        self.height
    }
    pub fn rgba(&self) -> &[u8] {
        &self.rgba
    }
    pub fn deadline(&self) -> std::time::Instant {
        self.deadline
    }
    pub fn expires(&self) -> SystemTime {
        self.expires
    }
}
pub fn expiry(
    value: home_client_model::local_time::UtcInstant,
) -> Result<SystemTime, ArtworkFailure> {
    let seconds = u64::try_from(value.seconds()).map_err(|_| ArtworkFailure::Expired)?;
    let nanos = u32::try_from(value.nanos())
        .ok()
        .filter(|n| *n < 1_000_000_000)
        .ok_or(ArtworkFailure::Invalid)?;
    UNIX_EPOCH
        .checked_add(Duration::new(seconds, nanos))
        .ok_or(ArtworkFailure::Invalid)
}
/// Call only on an owned background task. Header checks precede pixel allocation;
/// the decoder also has a memory limit independent of compressed byte length.
pub fn decode_artwork(
    bytes: &[u8],
    expires: SystemTime,
    now: SystemTime,
    deadline: std::time::Instant,
    provenance: MediaDataProvenance,
) -> Result<Artwork, ArtworkFailure> {
    if expires <= now || deadline <= std::time::Instant::now() {
        return Err(ArtworkFailure::Expired);
    }
    let pixels = crate::raster::decode_png(
        bytes,
        crate::raster::Limits {
            encoded: 128 * 1024,
            side: 512,
            decoded: 512 * 512 * 4,
            decoder: 4 * 1024 * 1024,
        },
    )
    .map_err(|error| match error {
        crate::raster::Failure::Invalid => ArtworkFailure::Invalid,
        crate::raster::Failure::Capacity => ArtworkFailure::Capacity,
    })?;

    Ok(Artwork {
        provenance,
        width: pixels.width,
        height: pixels.height,
        rgba: pixels.rgba,
        expires,
        deadline,
    })
}

#[cfg(test)]
pub(crate) mod tests;
