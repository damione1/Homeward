use crate::history::{Data, Statistic};
use home_client_cache::history::Query;
use home_client_model::EntityId;
use slint_kit_pro::charts::{PreparationId, PreparedFrame};
use std::sync::Arc;
use tokio_util::sync::CancellationToken;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Status {
    Draft,
    Loading,
    Preparing,
    Ready,
    Empty,
    NotRetained,
    Failed(Failure),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Failure {
    Query,
    Stale,
    Capacity,
    Denied,
    Transport,
    Contract,
    Projection,
}
pub(super) struct State {
    pub generation: i32,
    pub entity: Option<EntityId>,
    pub foreground: bool,
    pub active: bool,
    pub pending: Option<(Query, Statistic)>,
    pub cancel: CancellationToken,
    pub data: Option<Arc<Data>>,
    pub source_generation: Option<home_client_session::Generation>,
    pub group: usize,
    pub frame: Option<Arc<PreparedFrame>>,
    pub render: bool,
    pub expected_render: Option<(PreparationId, i32)>,
    pub width: u16,
    pub height: u16,
    pub density: f32,
    pub background: [u8; 3],
    pub accent: [u8; 3],
    pub cursor: Option<usize>,
    pub range: Option<(f32, f32)>,
    pub status: Status,
}
impl Default for State {
    fn default() -> Self {
        Self {
            generation: 1,
            entity: None,
            foreground: true,
            active: false,
            pending: None,
            cancel: CancellationToken::new(),
            data: None,
            source_generation: None,
            group: 0,
            frame: None,
            render: false,
            expected_render: None,
            width: 800,
            height: 300,
            density: 1.,
            background: [255; 3],
            accent: [20, 20, 20],
            cursor: None,
            range: None,
            status: Status::Draft,
        }
    }
}
impl State {
    pub fn invalidate(&mut self) {
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.generation = if self.generation == 0 {
            0
        } else {
            self.generation.checked_add(1).unwrap_or(0)
        };
        self.pending = None;
        self.data = None;
        self.source_generation = None;
        self.frame = None;
        self.expected_render = None;
        self.render = false;
        self.cursor = None;
        self.range = None;
        self.group = 0;
        self.status = Status::Draft;
    }
    pub fn admits(&self, generation: i32) -> bool {
        generation > 0
            && generation == self.generation
            && self.active
            && self.foreground
            && self.entity.is_some()
            && !self.cancel.is_cancelled()
    }
}
