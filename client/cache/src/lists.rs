//! TaskService read fences. Watch is subscribed before pages are read; a
//! change invalidates the whole provisional capture, never patches a page.
//! No cursor decoding, replay cursor, credentials or mutation retry lives here.
use buffa::Message;
use home_client_model::{
    api::HomeName,
    api_tasks::{TaskListResource, TaskResource},
    lists::ListId,
};
use home_wire_contract::{home::api::v2 as wire, tasks_contract};
use std::{collections::BTreeSet, sync::Arc};

mod reader;
pub use reader::{
    ListReadEvent, ListReadExit, ListReadMetadata, ListReadSnapshot, read_lists_once,
};

const MAX_ROWS: usize = 65_536;
const MAX_BYTES: usize = 32 << 20;
const MAX_TOKEN_BYTES: usize = 1024;
const PAGE_SIZE: usize = 256;

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Scope {
    Catalog,
    Items(ListId),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReadFailure {
    InvalidResponse,
    OutOfOrder,
    WrongScope,
    Duplicate,
    BudgetExceeded,
    Incomplete,
    Abandoned,
    ChangedDuringRead,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WatchEffect {
    Reload,
    Resubscribe,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum WatchPhase {
    AwaitingReady,
    Reading,
    Closed,
}

/// A single stream lifetime. New streams get distinct unforgeable fences,
/// including when the stream sequence and invalidation counter start over.
pub struct ListReadWatch {
    scope: Scope,
    home: HomeName,
    identity: Arc<()>,
    phase: WatchPhase,
    sequence: u64,
    invalidation: u64,
}

impl ListReadWatch {
    pub fn new(scope: Scope, home: HomeName) -> Self {
        Self {
            scope,
            home,
            identity: Arc::new(()),
            phase: WatchPhase::AwaitingReady,
            sequence: 0,
            invalidation: 0,
        }
    }

    pub fn apply(
        &mut self,
        frame: wire::WatchTaskListsResponse,
    ) -> Result<WatchEffect, ReadFailure> {
        let result = self.apply_checked(frame);
        if result.is_err() {
            self.phase = WatchPhase::Closed;
        }
        result
    }

    fn apply_checked(
        &mut self,
        frame: wire::WatchTaskListsResponse,
    ) -> Result<WatchEffect, ReadFailure> {
        use tasks_contract::TaskWatchFrame as Frame;
        if self.phase == WatchPhase::Closed {
            return Err(ReadFailure::Abandoned);
        }
        let (sequence, frame) =
            tasks_contract::parse_task_watch(&frame).map_err(|_| ReadFailure::InvalidResponse)?;
        if self.sequence.checked_add(1) != Some(sequence) {
            return Err(ReadFailure::OutOfOrder);
        }
        self.sequence = sequence;
        match frame {
            Frame::Ready => {
                if self.phase != WatchPhase::AwaitingReady {
                    return Err(ReadFailure::OutOfOrder);
                }
                self.phase = WatchPhase::Reading;
                Ok(WatchEffect::Reload)
            }
            Frame::Changed { list, .. } => {
                if self.phase != WatchPhase::Reading {
                    return Err(ReadFailure::OutOfOrder);
                }
                if list.home() != &self.home {
                    return Err(ReadFailure::WrongScope);
                }
                if let Scope::Items(expected) = &self.scope
                    && list.native_id() != expected
                {
                    return Err(ReadFailure::WrongScope);
                }
                self.invalidation = self
                    .invalidation
                    .checked_add(1)
                    .ok_or(ReadFailure::Abandoned)?;
                Ok(WatchEffect::Reload)
            }
            Frame::Resync => {
                self.phase = WatchPhase::Closed;
                Ok(WatchEffect::Resubscribe)
            }
        }
    }

    pub fn begin_capture(&self) -> Result<ListCapture, ReadFailure> {
        if self.phase != WatchPhase::Reading {
            return Err(ReadFailure::Abandoned);
        }
        Ok(ListCapture {
            scope: self.scope.clone(),
            home: self.home.clone(),
            identity: self.identity.clone(),
            invalidation: self.invalidation,
            rows: match &self.scope {
                Scope::Catalog => CapturedRows::Catalog(Vec::new()),
                Scope::Items(id) => CapturedRows::Items {
                    list: id.clone(),
                    rows: Vec::new(),
                },
            },
            ids: BTreeSet::new(),
            tokens: BTreeSet::new(),
            next_token: String::new(),
            bytes: 0,
            pages: 0,
            total: None,
            failed: false,
        })
    }

    /// Closing also makes all in-flight page replies unpublishable. The owner
    /// separately decides whether an already published cache may remain offline.
    pub fn close(&mut self) {
        self.phase = WatchPhase::Closed;
    }

    pub fn finish(&self, capture: ListCapture) -> Result<CapturedRows, ReadFailure> {
        if self.phase != WatchPhase::Reading || !Arc::ptr_eq(&self.identity, &capture.identity) {
            return Err(ReadFailure::Abandoned);
        }
        if self.invalidation != capture.invalidation {
            return Err(ReadFailure::ChangedDuringRead);
        }
        if capture.failed {
            return Err(ReadFailure::Abandoned);
        }
        if capture.pages == 0 || !capture.next_token.is_empty() {
            return Err(ReadFailure::Incomplete);
        }
        capture.total()?;
        Ok(capture.rows)
    }
}

/// An immutable complete capture with exact server total_size retained separately.
#[derive(Clone)]
pub enum CapturedRows {
    Catalog(Vec<Arc<TaskListResource>>),
    Items {
        list: ListId,
        rows: Vec<Arc<TaskResource>>,
    },
}

pub struct ListCapture {
    scope: Scope,
    home: HomeName,
    identity: Arc<()>,
    invalidation: u64,
    rows: CapturedRows,
    ids: BTreeSet<String>,
    tokens: BTreeSet<String>,
    next_token: String,
    bytes: usize,
    pages: usize,
    total: Option<usize>,
    failed: bool,
}

impl ListCapture {
    pub fn next_token(&self) -> &str {
        &self.next_token
    }

    pub fn push_lists(&mut self, page: wire::ListTaskListsResponse) -> Result<(), ReadFailure> {
        let result = self.push_lists_checked(page);
        self.poison_on_failure(result)
    }

    fn push_lists_checked(&mut self, page: wire::ListTaskListsResponse) -> Result<(), ReadFailure> {
        if self.scope != Scope::Catalog {
            return Err(ReadFailure::WrongScope);
        }
        if !page.__buffa_unknown_fields.is_empty() {
            return Err(ReadFailure::InvalidResponse);
        }
        self.admit_total(page.total_size)?;
        self.admit_page(
            page.task_lists.len(),
            page.encoded_len() as usize,
            &page.next_page_token,
        )?;
        for row in page.task_lists {
            let row =
                tasks_contract::parse_task_list(&row).map_err(|_| ReadFailure::InvalidResponse)?;
            if row.name().home() != &self.home {
                return Err(ReadFailure::WrongScope);
            }
            self.admit_identity(row.name().as_str().to_owned())?;
            match &mut self.rows {
                CapturedRows::Catalog(rows) => rows.push(Arc::new(row)),
                CapturedRows::Items { .. } => return Err(ReadFailure::WrongScope),
            }
        }
        Ok(())
    }

    pub fn push_items(&mut self, page: wire::ListTasksResponse) -> Result<(), ReadFailure> {
        let result = self.push_items_checked(page);
        self.poison_on_failure(result)
    }

    fn push_items_checked(&mut self, page: wire::ListTasksResponse) -> Result<(), ReadFailure> {
        let expected = match &self.scope {
            Scope::Items(id) => id.clone(),
            Scope::Catalog => return Err(ReadFailure::WrongScope),
        };
        if !page.__buffa_unknown_fields.is_empty() {
            return Err(ReadFailure::InvalidResponse);
        }
        self.admit_total(page.total_size)?;
        self.admit_page(
            page.tasks.len(),
            page.encoded_len() as usize,
            &page.next_page_token,
        )?;
        for row in page.tasks {
            let row = tasks_contract::parse_task(&row).map_err(|_| ReadFailure::InvalidResponse)?;
            if row.name().parent().home() != &self.home || row.key().list() != &expected {
                return Err(ReadFailure::WrongScope);
            }
            self.admit_identity(row.name().as_str().to_owned())?;
            match &mut self.rows {
                CapturedRows::Items { rows, .. } => {
                    if rows
                        .last()
                        .is_some_and(|last| last.position() > row.position())
                    {
                        return Err(ReadFailure::OutOfOrder);
                    }
                    rows.push(Arc::new(row));
                }
                CapturedRows::Catalog(_) => return Err(ReadFailure::WrongScope),
            }
        }
        Ok(())
    }

    fn admit_total(&mut self, total: i32) -> Result<(), ReadFailure> {
        let total = usize::try_from(total).map_err(|_| ReadFailure::InvalidResponse)?;
        if total > MAX_ROWS {
            return Err(ReadFailure::BudgetExceeded);
        }
        if self.total.is_some_and(|prior| prior != total) {
            return Err(ReadFailure::ChangedDuringRead);
        }
        self.total = Some(total);
        Ok(())
    }
    pub fn total(&self) -> Result<u64, ReadFailure> {
        let total = self.total.ok_or(ReadFailure::Incomplete)?;
        if self.next_token.is_empty() && self.ids.len() != total {
            return Err(ReadFailure::Incomplete);
        }
        Ok(total as u64)
    }
    fn admit_identity(&mut self, id: String) -> Result<(), ReadFailure> {
        if self.ids.len() >= MAX_ROWS {
            return Err(ReadFailure::BudgetExceeded);
        }
        if !self.ids.insert(id) {
            return Err(ReadFailure::Duplicate);
        }
        Ok(())
    }

    fn admit_page(&mut self, count: usize, bytes: usize, token: &str) -> Result<(), ReadFailure> {
        // Charge generated bytes, typed names/keys and capture identity/token indexes.
        let bytes = bytes
            .saturating_mul(8)
            .saturating_add(count.saturating_mul(512));
        if self.failed || (self.pages > 0 && self.next_token.is_empty()) {
            return Err(ReadFailure::Abandoned);
        }
        if count > PAGE_SIZE
            || self.pages >= MAX_ROWS
            || token.len() > MAX_TOKEN_BYTES
            || self.bytes.saturating_add(bytes) > MAX_BYTES
        {
            return Err(ReadFailure::BudgetExceeded);
        }
        if !token.is_empty() && (!self.tokens.insert(token.to_owned())) {
            return Err(ReadFailure::InvalidResponse);
        }
        self.bytes += bytes;
        self.pages += 1;
        self.next_token = token.to_owned();
        Ok(())
    }

    fn poison_on_failure(&mut self, result: Result<(), ReadFailure>) -> Result<(), ReadFailure> {
        if result.is_err() {
            self.failed = true;
            match &mut self.rows {
                CapturedRows::Catalog(rows) => rows.clear(),
                CapturedRows::Items { rows, .. } => rows.clear(),
            }
            self.ids.clear();
            self.tokens.clear();
            self.next_token.clear();
        }
        result
    }
}

#[cfg(test)]
mod tests;
