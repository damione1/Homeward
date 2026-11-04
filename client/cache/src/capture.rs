use crate::{
    CacheFailure, CachedView, Filter, MAX_ENTITIES, MAX_PAGE_SIZE, MAX_VIEW_BYTES, entry_bytes,
    entry_name, time_key, valid_time, valid_token,
};
use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::api::{EntityName, EntityViewName, HomeName};
use home_wire_contract::home::api::v2 as wire;
use std::{
    collections::{BTreeMap, BTreeSet},
    sync::Arc,
};

/// Provisional pages never enter a UI projection. Any failed page erases them;
/// only the final page's checkpoint can activate a complete capture.
pub struct SnapshotCapture {
    filter: Filter,
    home: HomeName,
    name: EntityViewName,
    page_size: u32,
    expires: Timestamp,
    started: Timestamp,
    next: String,
    resume: String,
    complete: bool,
    seen_tokens: BTreeSet<String>,
    entities: BTreeMap<EntityName, Arc<wire::EntityViewEntry>>,
    encoded_bytes: usize,
    abandoned: bool,
}
impl SnapshotCapture {
    pub fn begin(
        home: HomeName,
        filter: Filter,
        page_size: u32,
        resource: wire::EntityView,
        now: &Timestamp,
    ) -> Result<Self, CacheFailure> {
        if !(1..=MAX_PAGE_SIZE).contains(&page_size) {
            return Err(CacheFailure::InvalidPageSize);
        }
        let name = EntityViewName::try_from(resource.name.as_str())
            .map_err(|_| CacheFailure::InvalidResponse)?;
        let created = resource
            .create_time
            .as_option()
            .ok_or(CacheFailure::InvalidResponse)?;
        let expires = resource
            .expire_time
            .as_option()
            .ok_or(CacheFailure::InvalidResponse)?;
        if !resource.__buffa_unknown_fields.is_empty()
            || resource.filter != filter.expression()
            || name.home() != home
            || !valid_time(created)
            || !valid_time(expires)
            || !valid_time(now)
            || time_key(expires) <= time_key(created)
            || expires.seconds - created.seconds > 61
            || time_key(now) >= time_key(expires)
        {
            return Err(CacheFailure::InvalidResponse);
        }
        Ok(Self {
            home,
            filter,
            name,
            page_size,
            expires: expires.clone(),
            started: now.clone(),
            next: String::new(),
            resume: String::new(),
            complete: false,
            seen_tokens: BTreeSet::new(),
            entities: BTreeMap::new(),
            encoded_bytes: resource.encoded_len() as usize + 512,
            abandoned: false,
        })
    }
    pub fn name(&self) -> &EntityViewName {
        &self.name
    }
    pub fn next_request(&self) -> Option<wire::QueryEntityViewRequest> {
        (!self.abandoned && !self.complete).then(|| wire::QueryEntityViewRequest {
            name: self.name.as_str().to_owned(),
            page_size: self.page_size as i32,
            page_token: self.next.clone(),
            ..Default::default()
        })
    }
    pub fn push(
        &mut self,
        page: wire::QueryEntityViewResponse,
        now: &Timestamp,
    ) -> Result<(), CacheFailure> {
        let result = if self.abandoned {
            Err(CacheFailure::Abandoned)
        } else if self.complete {
            Err(CacheFailure::InvalidResponse)
        } else {
            self.append(page, now)
        };
        if result.is_err() {
            self.abandon()
        }
        result
    }
    pub fn abandon(&mut self) {
        self.abandoned = true;
        self.entities.clear();
        self.next.clear();
        self.resume.clear();
        self.seen_tokens.clear();
        self.encoded_bytes = 0;
    }
    fn unexpired(&self, now: &Timestamp) -> Result<(), CacheFailure> {
        if !valid_time(now) || time_key(now) < time_key(&self.started) {
            return Err(CacheFailure::InvalidResponse);
        }
        if time_key(now) >= time_key(&self.expires) {
            return Err(CacheFailure::Expired);
        }
        Ok(())
    }
    fn append(
        &mut self,
        page: wire::QueryEntityViewResponse,
        now: &Timestamp,
    ) -> Result<(), CacheFailure> {
        self.unexpired(now)?;
        if !page.__buffa_unknown_fields.is_empty() || page.entries.len() > self.page_size as usize {
            return Err(CacheFailure::InvalidResponse);
        }
        let complete = page.next_page_token.is_empty();
        if complete {
            if !valid_token(&page.resume_token) {
                return Err(CacheFailure::Incomplete);
            }
        } else if !page.resume_token.is_empty()
            || !valid_token(&page.next_page_token)
            || page.entries.is_empty()
            || !self.seen_tokens.insert(page.next_page_token.clone())
        {
            return Err(CacheFailure::InvalidResponse);
        }
        self.encoded_bytes = self
            .encoded_bytes
            .checked_add(page.encoded_len() as usize * 4 + 512)
            .ok_or(CacheFailure::BudgetExceeded)?;
        if self.encoded_bytes > MAX_VIEW_BYTES
            || self.entities.len() + page.entries.len() > MAX_ENTITIES
        {
            return Err(CacheFailure::BudgetExceeded);
        }
        for entry in page.entries {
            let name = entry_name(&entry)?;
            if name.home() != self.home || !self.filter.permits(&name) {
                return Err(CacheFailure::ForeignEntity);
            }
            if self
                .entities
                .last_key_value()
                .is_some_and(|(previous, _)| previous >= &name)
            {
                return Err(CacheFailure::OutOfOrder);
            }
            if self.entities.insert(name, Arc::new(entry)).is_some() {
                return Err(CacheFailure::DuplicateEntity);
            }
        }
        self.next = page.next_page_token;
        self.resume = page.resume_token;
        self.complete = complete;
        Ok(())
    }
    pub fn finish(self, now: &Timestamp) -> Result<CachedView, CacheFailure> {
        if self.abandoned {
            return Err(CacheFailure::Abandoned);
        }
        self.unexpired(now)?;
        if !self.complete || !valid_token(&self.resume) {
            return Err(CacheFailure::Incomplete);
        }
        let mut retained = 10 * 1024 + self.filter.expression().len() + self.name.as_str().len();
        for entry in self.entities.values() {
            retained = retained
                .checked_add(entry_bytes(entry)?)
                .ok_or(CacheFailure::BudgetExceeded)?;
        }
        if retained > MAX_VIEW_BYTES {
            return Err(CacheFailure::BudgetExceeded);
        }
        Ok(CachedView {
            incarnation: crate::CaptureIncarnation(Arc::new(())),
            _cleanup: None,
            entities: self.entities,
            name: self.name,
            resume: self.resume,
            expires: self.expires,
            filter: self.filter,
            encoded_bytes: retained,
        })
    }
}
