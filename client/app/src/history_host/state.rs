use crate::history::{Data, Statistic};
use home_client_cache::history::Query;
use home_client_model::api::EntityName;
use slint_kit_pro::charts::{PreparationId, PreparationResult, PreparedFrame};
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
    Interrupted,
}
pub(super) struct State {
    pub paused: bool,
    pub resume_render: bool,
    pub generation: i32,
    pub entity: Option<EntityName>,
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
            paused: false,
            resume_render: false,
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
        self.paused = false;
        self.resume_render = false;
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
    pub fn complete_render(&mut self, result: PreparationResult) -> bool {
        if self.expected_render != Some((result.id(), self.generation))
            || !self.admits(self.generation)
        {
            return false;
        }
        self.expected_render = None;
        match result {
            PreparationResult::Ready(frame) => {
                self.frame = Some(frame);
                self.status = Status::Ready;
            }
            PreparationResult::Failed { .. } => {
                self.frame = None;
                self.status = Status::Failed(Failure::Projection);
            }
        }
        true
    }
    pub fn suspend(&mut self) {
        if self.paused {
            return;
        }
        self.paused = true;
        self.resume_render = self.render || self.expected_render.is_some();
        if self.status == Status::Loading && self.data.is_none() && self.frame.is_none() {
            self.status = Status::Failed(Failure::Interrupted);
        }
        self.cancel.cancel();
        self.cancel = CancellationToken::new();
        self.generation = if self.generation > 0 {
            self.generation.checked_add(1).unwrap_or(0)
        } else {
            0
        };
        self.pending = None;
        self.expected_render = None;
        self.render = false;
    }
    pub fn admits(&self, generation: i32) -> bool {
        !self.paused
            && generation > 0
            && generation == self.generation
            && self.active
            && self.foreground
            && self.entity.is_some()
            && !self.cancel.is_cancelled()
    }
}
