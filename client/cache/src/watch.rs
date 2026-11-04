use crate::{
    CacheFailure, CachedView, MAX_ENTITIES, MAX_VIEW_BYTES, entry_bytes, entry_name, time_key,
    valid_token, wall_now,
};
use buffa::Message;
use home_client_model::api::EntityName;
use home_wire_contract::home::api::v2 as wire;
use std::sync::Arc;

#[derive(Debug, PartialEq, Eq)]
pub enum FrameEffect {
    Applied,
}

/// One stream attempt replaces complete entries atomically. Its checkpoint is
/// advanced only after a validated membership change; failures erase the view.
pub struct LiveView {
    view: Option<CachedView>,
}
impl LiveView {
    pub fn new(view: CachedView) -> Self {
        Self { view: Some(view) }
    }
    pub fn current(&self) -> Option<&CachedView> {
        self.view.as_ref()
    }
    pub fn into_current(self) -> Option<CachedView> {
        self.view
    }
    pub fn apply(
        &mut self,
        frame: wire::WatchEntityViewResponse,
    ) -> Result<FrameEffect, CacheFailure> {
        let result = self.apply_checked(frame);
        if result.is_err() {
            self.view = None;
        }
        result
    }
    fn apply_checked(
        &mut self,
        frame: wire::WatchEntityViewResponse,
    ) -> Result<FrameEffect, CacheFailure> {
        use wire::watch_entity_view_response::Change;
        let view = self.view.as_mut().ok_or(CacheFailure::Abandoned)?;
        if time_key(&wall_now()?) >= time_key(&view.expires) {
            return Err(CacheFailure::Expired);
        }
        if !frame.__buffa_unknown_fields.is_empty()
            || frame.encoded_len() as usize > 1024 * 1024
            || !valid_token(&frame.resume_token)
        {
            return Err(CacheFailure::InvalidResponse);
        }
        if frame.resume_token == view.resume {
            return Err(CacheFailure::OutOfOrder);
        }
        let home = view.name.home();
        match frame.change {
            Some(Change::Upsert(entry)) => {
                let name = entry_name(&entry)?;
                if name.home() != home || !view.filter.permits(&name) {
                    return Err(CacheFailure::ForeignEntity);
                }
                let previous = view
                    .entities
                    .get(&name)
                    .map(|value| entry_bytes(value))
                    .transpose()?
                    .unwrap_or(0);
                let next = view
                    .encoded_bytes
                    .checked_sub(previous)
                    .and_then(|n| n.checked_add(entry_bytes(&entry).ok()?))
                    .ok_or(CacheFailure::BudgetExceeded)?;
                if next > MAX_VIEW_BYTES
                    || (!view.entities.contains_key(&name) && view.entities.len() >= MAX_ENTITIES)
                {
                    return Err(CacheFailure::BudgetExceeded);
                }
                view.entities.insert(name, Arc::new(*entry));
                view.encoded_bytes = next;
            }
            Some(Change::Remove(raw)) => {
                let name = EntityName::try_from(raw.as_str())
                    .map_err(|_| CacheFailure::InvalidResponse)?;
                if name.home() != home {
                    return Err(CacheFailure::ForeignEntity);
                }
                if let Some(previous) = view.entities.remove(&name) {
                    view.encoded_bytes = view
                        .encoded_bytes
                        .checked_sub(entry_bytes(&previous)?)
                        .ok_or(CacheFailure::BudgetExceeded)?;
                }
            }
            None => return Err(CacheFailure::InvalidResponse),
        }
        view.resume = frame.resume_token;
        Ok(FrameEffect::Applied)
    }
}
