//! Exact media adapter. The server owns routing and capabilities; quantities
//! remain integer basis points/milliseconds until the renderer projects them.
use buffa::Message;
use home_client_actions::ActionTarget;
use home_client_model::EntityId;
use home_wire_contract::{
    home::{actions::v1 as actions, media::v1 as wire, registry::v1 as registry},
    media_contract as admit,
};
use std::{
    io::Cursor,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Field {
    Volume,
    Position,
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
    fn command(self) -> wire::media_action::Command {
        match self {
            Self::Volume(v) => wire::media_action::Command::VolumeBasisPoints(v.0),
            Self::Position(v) => wire::media_action::Command::PositionMillis(v.0),
        }
    }
    fn new(field: Field, value: u64) -> Result<Self, Refusal> {
        match field {
            Field::Volume => u32::try_from(value)
                .ok()
                .filter(|v| *v <= 10_000)
                .map(|v| Self::Volume(VolumeBasisPoints(v)))
                .ok_or(Refusal::Bounds),
            Field::Position => (value <= admit::MAX_POSITION_MILLIS)
                .then_some(Self::Position(PositionMillis(value)))
                .ok_or(Refusal::Bounds),
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Refusal {
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
    snapshot: wire::Snapshot,
    target: ActionTarget,
    coordinator: EntityId,
}
impl Data {
    pub fn new(snapshot: wire::Snapshot, expected: &EntityId) -> Result<Self, Refusal> {
        if !admit::snapshot(&snapshot, expected.as_str()) {
            return Err(Refusal::Contract);
        }
        let route = snapshot.route.as_option().ok_or(Refusal::Contract)?;
        let target = ActionTarget::new(expected.clone(), admit::ACTION_SCHEMA, &route.endpoint)
            .ok_or(Refusal::Contract)?;
        let coordinator = route
            .coordinator_entity_id
            .parse()
            .map_err(|_| Refusal::Contract)?;
        Ok(Self {
            snapshot,
            target,
            coordinator,
        })
    }
    pub fn snapshot(&self) -> &wire::Snapshot {
        &self.snapshot
    }
    pub fn target(&self) -> &ActionTarget {
        &self.target
    }
    pub fn coordinator(&self) -> &EntityId {
        &self.coordinator
    }
    pub fn same_controls(&self, other: &Self) -> bool {
        self.snapshot.route == other.snapshot.route
            && self.snapshot.capabilities == other.snapshot.capabilities
            && self.snapshot.action_access == other.snapshot.action_access
            && self.snapshot.quality == other.snapshot.quality
            && self.snapshot.descriptor_sha256 == other.snapshot.descriptor_sha256
    }
    pub fn limits(&self, field: Field) -> Option<&wire::QuantityBounds> {
        let limits = self.snapshot.capabilities.as_option()?;
        match field {
            Field::Volume => limits.volume_basis_points.as_option(),
            Field::Position => limits.position_millis.as_option(),
        }
    }
    pub fn observed(&self, field: Field) -> Option<Quantity> {
        match field {
            Field::Volume => self
                .snapshot
                .volume_basis_points
                .map(|v| Quantity::Volume(VolumeBasisPoints(v))),
            Field::Position => self
                .snapshot
                .position_millis
                .map(|v| Quantity::Position(PositionMillis(v))),
        }
    }
    pub fn parse(&self, field: Field, text: &str) -> Result<Quantity, Refusal> {
        let limits = self.limits(field).ok_or(Refusal::Unsupported)?;
        let value = parse_fixed(text, field.decimals())?;
        if value < limits.minimum || value > limits.maximum {
            return Err(Refusal::Bounds);
        }
        if !admit::quantity(value, limits) {
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
        let count = (limits.maximum - limits.minimum) / limits.step;
        let step = (f64::from(position.clamp(0., 1.)) * count as f64).round() as u64;
        let value = limits
            .minimum
            .checked_add(step.checked_mul(limits.step).ok_or(Refusal::Bounds)?)
            .ok_or(Refusal::Bounds)?;
        if !admit::quantity(value, limits) {
            return Err(Refusal::Bounds);
        }
        Quantity::new(field, value)
    }
    pub fn position(&self, quantity: Quantity) -> f32 {
        let Some(limits) = self.limits(quantity.field()) else {
            return 0.;
        };
        if limits.maximum == limits.minimum {
            return 0.;
        }
        ((quantity.canonical().saturating_sub(limits.minimum) as f64
            / (limits.maximum - limits.minimum) as f64)
            .clamp(0., 1.)) as f32
    }
    pub fn quantity_request(&self, value: Quantity) -> Result<actions::CommandRequest, Refusal> {
        self.request(value.command())
    }
    pub fn request(
        &self,
        command: wire::media_action::Command,
    ) -> Result<actions::CommandRequest, Refusal> {
        let action = wire::MediaAction {
            route: self.snapshot.route.clone(),
            command: Some(command),
            ..Default::default()
        };
        if !admit::action(&action, &self.snapshot) {
            return Err(Refusal::Permission);
        }
        let route = self.snapshot.route.as_option().ok_or(Refusal::Contract)?;
        Ok(actions::CommandRequest {
            admission_epoch: route.admission_epoch.clone(),
            config_revision: route.config_revision,
            target: registry::EntityRef {
                reference: Some(registry::entity_ref::Reference::EntityId(
                    self.target.entity().as_str().into(),
                )),
                ..Default::default()
            }
            .into(),
            capability_schema_id: admit::ACTION_SCHEMA.into(),
            endpoint: route.endpoint.clone(),
            arguments: actions::ActionArgs {
                arguments: Some(actions::action_args::Arguments::Extension(Box::new(
                    actions::ExtensionPayload {
                        schema_id: admit::ACTION_SCHEMA.into(),
                        major: 1,
                        descriptor_sha256: self.snapshot.descriptor_sha256.clone(),
                        protobuf_payload: action.encode_to_vec(),
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })
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
    width: u32,
    height: u32,
    rgba: Arc<[u8]>,
    expires: SystemTime,
}
impl Artwork {
    pub fn width(&self) -> u32 {
        self.width
    }
    pub fn height(&self) -> u32 {
        self.height
    }
    pub fn rgba(&self) -> &[u8] {
        &self.rgba
    }
    pub fn expires(&self) -> SystemTime {
        self.expires
    }
}
pub fn expiry(
    value: &buffa_types::google::protobuf::Timestamp,
) -> Result<SystemTime, ArtworkFailure> {
    let seconds = u64::try_from(value.seconds).map_err(|_| ArtworkFailure::Expired)?;
    let nanos = u32::try_from(value.nanos)
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
) -> Result<Artwork, ArtworkFailure> {
    if expires <= now {
        return Err(ArtworkFailure::Expired);
    }
    if bytes.len() > admit::MAX_ARTWORK_BYTES {
        return Err(ArtworkFailure::Capacity);
    }
    let mut decoder = png::Decoder::new(Cursor::new(bytes));
    decoder.set_limits(png::Limits {
        bytes: 4 * 1024 * 1024,
    });
    decoder.set_transformations(png::Transformations::EXPAND | png::Transformations::STRIP_16);
    let mut reader = decoder.read_info().map_err(|_| ArtworkFailure::Invalid)?;
    let info = reader.info();
    if info.width == 0
        || info.height == 0
        || info.width > 512
        || info.height > 512
        || info.animation_control.is_some()
    {
        return Err(ArtworkFailure::Capacity);
    }
    let size = reader
        .output_buffer_size()
        .filter(|n| *n <= 512 * 512 * 4)
        .ok_or(ArtworkFailure::Capacity)?;
    let mut buffer = vec![0; size];
    let output = reader
        .next_frame(&mut buffer)
        .map_err(|_| ArtworkFailure::Invalid)?;
    let mut rgba = Vec::with_capacity(output.width as usize * output.height as usize * 4);
    let data = &buffer[..output.buffer_size()];
    match output.color_type {
        png::ColorType::Rgba => rgba.extend_from_slice(data),
        png::ColorType::Rgb => {
            for p in data.as_chunks::<3>().0 {
                rgba.extend_from_slice(&[p[0], p[1], p[2], 255]);
            }
        }
        png::ColorType::Grayscale => {
            for p in data {
                rgba.extend_from_slice(&[*p, *p, *p, 255]);
            }
        }
        png::ColorType::GrayscaleAlpha => {
            for p in data.as_chunks::<2>().0 {
                rgba.extend_from_slice(&[p[0], p[0], p[0], p[1]]);
            }
        }
        png::ColorType::Indexed => return Err(ArtworkFailure::Invalid),
    }
    reader.finish().map_err(|_| ArtworkFailure::Invalid)?;
    if rgba.len() != output.width as usize * output.height as usize * 4 {
        return Err(ArtworkFailure::Invalid);
    }
    Ok(Artwork {
        width: output.width,
        height: output.height,
        rgba: rgba.into(),
        expires,
    })
}

#[cfg(test)]
mod tests;
