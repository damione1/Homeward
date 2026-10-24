use std::{
    collections::{BTreeMap, BTreeSet},
    sync::Arc,
};

use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::EntityId;
use home_wire_contract::home::state::v1 as wire;

use crate::{
    CacheFailure, CachedView, Filter, MAX_ENTITIES, MAX_PAGE_SIZE, MAX_VIEW_BYTES, admit_cursor,
    observation_id, time_key, valid_time,
};

/// Provisional data cannot be inspected or projected. Any failed page erases it.
pub struct SnapshotCapture {
    filter: Filter,
    page_size: u32,
    token: String,
    cursor: wire::ViewCursor,
    expires_at: Timestamp,
    started_at: Timestamp,
    next: String,
    seen_tokens: BTreeSet<String>,
    entities: BTreeMap<EntityId, Arc<wire::ViewEntity>>,
    encoded_bytes: usize,
    abandoned: bool,
}

impl SnapshotCapture {
    pub fn begin(
        filter: Filter,
        page_size: u32,
        first: wire::OpenViewResponse,
        now: &Timestamp,
    ) -> Result<Self, CacheFailure> {
        if !(1..=MAX_PAGE_SIZE).contains(&page_size) {
            return Err(CacheFailure::InvalidPageSize);
        }
        let cursor = first
            .cursor
            .as_option()
            .ok_or(CacheFailure::InvalidResponse)?
            .clone();
        admit_cursor(&cursor, &filter)?;
        let expires_at = first
            .expires_at
            .as_option()
            .ok_or(CacheFailure::InvalidResponse)?
            .clone();
        if first.snapshot_token.is_empty() || !valid_time(&expires_at) || !valid_time(now) {
            return Err(CacheFailure::InvalidResponse);
        }
        let mut capture = Self {
            filter,
            page_size,
            token: first.snapshot_token.clone(),
            cursor,
            expires_at,
            started_at: now.clone(),
            next: String::new(),
            seen_tokens: BTreeSet::new(),
            entities: BTreeMap::new(),
            encoded_bytes: 0,
            abandoned: false,
        };
        capture.append(first, now)?;
        Ok(capture)
    }

    pub fn next_request(&self) -> Option<wire::ReadPageRequest> {
        if self.abandoned || self.next.is_empty() {
            return None;
        }
        Some(wire::ReadPageRequest {
            snapshot_token: self.token.clone(),
            page_token: self.next.clone(),
            ..Default::default()
        })
    }

    pub fn push(
        &mut self,
        page: wire::OpenViewResponse,
        now: &Timestamp,
    ) -> Result<(), CacheFailure> {
        let result = if self.abandoned {
            Err(CacheFailure::Abandoned)
        } else if self.next.is_empty() {
            Err(CacheFailure::InvalidResponse)
        } else {
            self.append(page, now)
        };
        if result.is_err() {
            self.abandon();
        }
        result
    }

    /// Call on a refused/failed ReadPage as well as dropping the capture.
    pub fn abandon(&mut self) {
        self.abandoned = true;
        self.entities.clear();
        self.next.clear();
        self.encoded_bytes = 0;
    }

    fn unexpired(&self, now: &Timestamp) -> Result<(), CacheFailure> {
        if !valid_time(now) || time_key(now) < time_key(&self.started_at) {
            return Err(CacheFailure::InvalidResponse);
        }
        if time_key(now) >= time_key(&self.expires_at) {
            return Err(CacheFailure::Expired);
        }
        Ok(())
    }

    fn append(
        &mut self,
        page: wire::OpenViewResponse,
        now: &Timestamp,
    ) -> Result<(), CacheFailure> {
        self.unexpired(now)?;
        if page.snapshot_token != self.token
            || page.cursor.as_option() != Some(&self.cursor)
            || page.expires_at.as_option() != Some(&self.expires_at)
        {
            return Err(CacheFailure::GenerationChanged);
        }
        if page.entities.len() > self.page_size as usize
            || (!page.next_page_token.is_empty() && page.entities.is_empty())
            || !page.__buffa_unknown_fields.is_empty()
        {
            return Err(CacheFailure::InvalidResponse);
        }
        // Includes duplicated legacy states/capabilities in the wire memory budget.
        self.encoded_bytes = self
            .encoded_bytes
            .checked_add(page.encoded_len() as usize)
            .ok_or(CacheFailure::BudgetExceeded)?;
        if self.encoded_bytes > MAX_VIEW_BYTES
            || self.entities.len() + page.entities.len() > MAX_ENTITIES
        {
            return Err(CacheFailure::BudgetExceeded);
        }
        for entity in page.entities {
            let id: EntityId = entity
                .entity_id
                .parse()
                .map_err(|_| CacheFailure::InvalidResponse)?;
            if !self.filter.permits(&id) {
                return Err(CacheFailure::ForeignEntity);
            }
            if let Some(observed) = entity.state.as_option()
                && (observation_id(observed)? != id || observed.revision == 0)
            {
                return Err(CacheFailure::InvalidResponse);
            }
            if self.entities.insert(id, Arc::new(entity)).is_some() {
                return Err(CacheFailure::DuplicateEntity);
            }
        }
        if !page.next_page_token.is_empty()
            && !self.seen_tokens.insert(page.next_page_token.clone())
        {
            return Err(CacheFailure::OutOfOrder);
        }
        self.next = page.next_page_token;
        Ok(())
    }

    pub fn finish(self, now: &Timestamp) -> Result<CachedView, CacheFailure> {
        if self.abandoned {
            return Err(CacheFailure::Abandoned);
        }
        self.unexpired(now)?;
        if !self.next.is_empty() {
            return Err(CacheFailure::Incomplete);
        }
        Ok(CachedView {
            entities: self.entities,
            cursor: self.cursor,
            filter: self.filter,
            encoded_bytes: self.encoded_bytes,
        })
    }
}
