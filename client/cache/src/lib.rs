//! Private in-memory views. No UI, disk persistence, command dispatch or credentials.
#![deny(clippy::wildcard_enum_match_arm)]

mod capture;
mod filter;
mod owner;
mod projection;
mod snapshot_reader;
mod stream_reader;
mod watch;

use std::{collections::BTreeMap, sync::Arc};

use buffa_types::google::protobuf::Timestamp;
use home_client_model::EntityId;
use home_wire_contract::home::state::v1 as wire;

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

/// Complete captured view. Entity identities and the full opaque cursor stay in Rust.
/// Clones share immutable entity records; a replacement never mutates an old view.
#[derive(Clone)]
pub struct CachedView {
    pub(crate) entities: BTreeMap<EntityId, Arc<wire::ViewEntity>>,
    pub(crate) cursor: wire::ViewCursor,
    pub(crate) filter: Filter,
    pub(crate) encoded_bytes: usize,
}

impl CachedView {
    pub fn len(&self) -> usize {
        self.entities.len()
    }
    pub fn is_empty(&self) -> bool {
        self.entities.is_empty()
    }
    pub fn get(&self, id: &EntityId) -> Option<&wire::ViewEntity> {
        self.entities.get(id).map(Arc::as_ref)
    }
    pub fn entities(&self) -> impl ExactSizeIterator<Item = (&EntityId, &wire::ViewEntity)> {
        self.entities
            .iter()
            .map(|(id, entity)| (id, entity.as_ref()))
    }
    pub fn cursor(&self) -> &wire::ViewCursor {
        &self.cursor
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

fn admit_cursor(cursor: &wire::ViewCursor, filter: &Filter) -> Result<(), CacheFailure> {
    if cursor.epoch.is_empty()
        || cursor.config_revision == 0
        || cursor.authorization_revision == 0
        || cursor.filter_sha256.as_slice() != filter.digest()
        || cursor.config_sha256.len() != 32
        || cursor.config_sha256.iter().all(|byte| *byte == 0)
        || cursor.grant_sha256.len() != 32
        || cursor.grant_sha256.iter().all(|byte| *byte == 0)
        || cursor.admission_proof.len() != 40
        || cursor.admission_proof.iter().all(|byte| *byte == 0)
        || !cursor.__buffa_unknown_fields.is_empty()
    {
        return Err(CacheFailure::InvalidResponse);
    }
    Ok(())
}

fn same_generation(left: &wire::ViewCursor, right: &wire::ViewCursor) -> bool {
    left.epoch == right.epoch
        && left.config_revision == right.config_revision
        && left.authorization_revision == right.authorization_revision
        && left.filter_sha256 == right.filter_sha256
        && left.config_sha256 == right.config_sha256
        && left.grant_sha256 == right.grant_sha256
}

fn observation_id(observation: &wire::ObservedState) -> Result<EntityId, CacheFailure> {
    use home_wire_contract::home::registry::v1::entity_ref::Reference;
    match observation
        .entity
        .as_option()
        .and_then(|entity| entity.reference.as_ref())
    {
        Some(Reference::EntityId(id)) => id.parse().map_err(|_| CacheFailure::InvalidResponse),
        Some(Reference::HaAlias(_)) | None => Err(CacheFailure::InvalidResponse),
    }
}
