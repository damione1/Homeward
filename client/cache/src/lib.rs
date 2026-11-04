//! Private in-memory views. No UI, disk persistence, command dispatch or credentials.
#![deny(clippy::wildcard_enum_match_arm)]

pub mod calendars;
mod capture;
mod filter;
pub mod history;
pub mod lists;
mod owner;
mod projection;
mod snapshot_cleanup;
mod snapshot_reader;
mod stream_reader;
mod watch;

use std::{collections::BTreeMap, sync::Arc};

use buffa_types::google::protobuf::Timestamp;
use home_client_model::api::{EntityName, EntityViewName};
use home_wire_contract::home::api::v2 as wire;

pub use capture::SnapshotCapture;
pub use filter::Filter;
pub use owner::{
    ActivationPermit, CacheEvent, CacheHandle, CacheOwner, CachePhase, ControlFailure,
    ControlOutcome, Viewport,
};
pub use projection::{
    DeliveryOutcome, DeliveryTicket, ProjectionGeneration, ProjectionMailbox, ProjectionMetrics,
    Publication, PublicationRefusal,
};
pub use snapshot_reader::{SnapshotFailure, read_snapshot};
pub use stream_reader::{WatchExit, watch_once};
pub use watch::{FrameEffect, LiveView};

pub const MAX_ENTITIES: usize = 4096;
pub const MAX_PAGE_SIZE: u32 = 256;
pub const MAX_VIEW_BYTES: usize = 16 << 20;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CacheFailure {
    InvalidFilter,
    InvalidPageSize,
    InvalidResponse,
    Expired,
    GenerationChanged,
    DuplicateEntity,
    ForeignEntity,
    OutOfOrder,
    BudgetExceeded,
    Incomplete,
    Abandoned,
}

impl std::fmt::Display for CacheFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for CacheFailure {}

/// Process-local identity of one completed capture, never a credential or a
/// server token. Clones and watch updates retain it; a new capture never does.
#[derive(Clone)]
pub struct CaptureIncarnation(Arc<()>);
impl PartialEq for CaptureIncarnation {
    fn eq(&self, other: &Self) -> bool {
        Arc::ptr_eq(&self.0, &other.0)
    }
}
impl Eq for CaptureIncarnation {}
impl std::fmt::Debug for CaptureIncarnation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("CaptureIncarnation([local])")
    }
}

/// Complete admitted capture. The opaque checkpoint and native capture identity
/// stay in Rust; no old epoch/config/grant protocol is reconstructed.
#[derive(Clone)]
pub struct CachedView {
    pub(crate) incarnation: CaptureIncarnation,
    pub(crate) _cleanup: Option<Arc<snapshot_cleanup::ReleaseGuard>>,
    pub(crate) entities: BTreeMap<EntityName, Arc<wire::EntityViewEntry>>,
    pub(crate) name: EntityViewName,
    pub(crate) resume: String,
    pub(crate) expires: Timestamp,
    pub(crate) filter: Filter,
    pub(crate) encoded_bytes: usize,
}
impl CachedView {
    pub fn incarnation(&self) -> &CaptureIncarnation {
        &self.incarnation
    }
    pub fn len(&self) -> usize {
        self.entities.len()
    }
    pub fn is_empty(&self) -> bool {
        self.entities.is_empty()
    }
    pub fn get(&self, name: &EntityName) -> Option<&wire::EntityViewEntry> {
        self.entities.get(name).map(Arc::as_ref)
    }
    pub fn entities(&self) -> impl ExactSizeIterator<Item = (&EntityName, &wire::EntityViewEntry)> {
        self.entities
            .iter()
            .map(|(name, value)| (name, value.as_ref()))
    }
    pub fn name(&self) -> &EntityViewName {
        &self.name
    }
    pub fn checkpoint(&self) -> &str {
        &self.resume
    }
    pub fn expires(&self) -> &Timestamp {
        &self.expires
    }
    pub fn filter(&self) -> &Filter {
        &self.filter
    }
}

fn valid_time(time: &Timestamp) -> bool {
    (-62_135_596_800..=253_402_300_799).contains(&time.seconds)
        && (0..1_000_000_000).contains(&time.nanos)
        && time.__buffa_unknown_fields.is_empty()
}

fn time_key(time: &Timestamp) -> (i64, i32) {
    (time.seconds, time.nanos)
}

fn valid_token(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 8192
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

// This validates resource envelopes. Each trait's controls and readings still
// pass its dedicated typed codec before projection or command preparation.
fn entry_name(entry: &wire::EntityViewEntry) -> Result<EntityName, CacheFailure> {
    use home_client_model::api::{CapabilityName, DefinitionEtag};
    use home_wire_contract::api_observations::parse_action_access;
    let entity = entry
        .entity
        .as_option()
        .ok_or(CacheFailure::InvalidResponse)?;
    if !entry.__buffa_unknown_fields.is_empty()
        || !entity.__buffa_unknown_fields.is_empty()
        || entity.display_name.chars().count() > 63
        || entity.display_name.chars().any(char::is_control)
        || entity.etag.is_empty()
        || entity.etag.len() > 256
        || entry.capabilities.len() > 64
    {
        return Err(CacheFailure::InvalidResponse);
    }
    for time in [
        entity.create_time.as_option(),
        entity.update_time.as_option(),
    ]
    .into_iter()
    .flatten()
    {
        if !valid_time(time) {
            return Err(CacheFailure::InvalidResponse);
        }
    }
    let name =
        EntityName::try_from(entity.name.as_str()).map_err(|_| CacheFailure::InvalidResponse)?;
    let mut slots = std::collections::BTreeSet::new();
    for capability in &entry.capabilities {
        let slot = CapabilityName::try_from(capability.name.as_str())
            .map_err(|_| CacheFailure::InvalidResponse)?;
        if slot.entity() != name
            || !slots.insert(slot)
            || !capability.__buffa_unknown_fields.is_empty()
            || capability.r#trait.is_none()
            || DefinitionEtag::try_from(capability.etag.as_str()).is_err()
            || parse_action_access(capability.action_access).is_err()
        {
            return Err(CacheFailure::InvalidResponse);
        }
    }
    Ok(name)
}

fn entry_bytes(entry: &wire::EntityViewEntry) -> Result<usize, CacheFailure> {
    use buffa::Message;
    (entry.encoded_len() as usize)
        .checked_mul(4)
        .and_then(|n| n.checked_add(256 + entry.capabilities.len() * 128))
        .ok_or(CacheFailure::BudgetExceeded)
}
fn wall_now() -> Result<Timestamp, CacheFailure> {
    use std::time::{SystemTime, UNIX_EPOCH};
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|_| CacheFailure::InvalidResponse)?;
    let seconds = i64::try_from(now.as_secs()).map_err(|_| CacheFailure::InvalidResponse)?;
    Ok(Timestamp {
        seconds,
        nanos: now.subsec_nanos() as i32,
        ..Default::default()
    })
}
