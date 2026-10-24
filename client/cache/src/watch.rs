use std::{collections::BTreeMap, sync::Arc};

use buffa::{EnumValue, Message};
use home_client_model::EntityId;
use home_wire_contract::home::state::v1 as wire;

use crate::{
    CacheFailure, CachedView, MAX_VIEW_BYTES, admit_cursor, observation_id, same_generation,
};

#[derive(Debug, PartialEq, Eq)]
pub enum FrameEffect {
    Applied,
    Resync(EnumValue<wire::ResyncReason>),
}

/// One stream attempt. A new attempt starts sequence 1 and retains the last
/// applied global cursor; a global watermark may skip filtered observations.
pub struct LiveView {
    view: Option<CachedView>,
    stream_sequence: u64,
}

impl LiveView {
    pub fn new(view: CachedView) -> Self {
        Self {
            view: Some(view),
            stream_sequence: 0,
        }
    }

    pub fn current(&self) -> Option<&CachedView> {
        self.view.as_ref()
    }

    pub fn into_current(self) -> Option<CachedView> {
        self.view
    }

    pub fn apply(&mut self, frame: wire::WatchViewResponse) -> Result<FrameEffect, CacheFailure> {
        let result = self.apply_checked(frame);
        if result.is_err() {
            self.view = None;
        }
        result
    }

    fn apply_checked(
        &mut self,
        frame: wire::WatchViewResponse,
    ) -> Result<FrameEffect, CacheFailure> {
        use wire::watch_view_response::Frame;
        let view = self.view.as_mut().ok_or(CacheFailure::Abandoned)?;
        if self.stream_sequence.checked_add(1) != Some(frame.stream_sequence) {
            return Err(CacheFailure::OutOfOrder);
        }
        if !frame.__buffa_unknown_fields.is_empty() {
            return Err(CacheFailure::InvalidResponse);
        }
        match frame.frame {
            Some(Frame::ResyncRequired(resync)) => {
                if frame.cursor.is_set() {
                    return Err(CacheFailure::InvalidResponse);
                }
                // Known and future reasons both terminate and erase the old view.
                let reason = resync.reason;
                self.view = None;
                Ok(FrameEffect::Resync(reason))
            }
            Some(Frame::Delta(delta)) => {
                let cursor = frame
                    .cursor
                    .as_option()
                    .ok_or(CacheFailure::InvalidResponse)?;
                admit_cursor(cursor, &view.filter)?;
                if !same_generation(&view.cursor, cursor) {
                    return Err(CacheFailure::GenerationChanged);
                }
                if cursor.global_sequence <= view.cursor.global_sequence {
                    return Err(CacheFailure::OutOfOrder);
                }
                if delta.states.is_empty() || !delta.__buffa_unknown_fields.is_empty() {
                    return Err(CacheFailure::InvalidResponse);
                }
                let mut replacements: BTreeMap<EntityId, Arc<wire::ViewEntity>> = BTreeMap::new();
                let mut bytes = view.encoded_bytes;
                for observed in delta.states {
                    let id = observation_id(&observed)?;
                    let previous = replacements
                        .get(&id)
                        .or_else(|| view.entities.get(&id))
                        .ok_or(CacheFailure::ForeignEntity)?;
                    if observed.revision == 0
                        || previous
                            .state
                            .as_option()
                            .is_some_and(|old| observed.revision <= old.revision)
                    {
                        return Err(CacheFailure::OutOfOrder);
                    }
                    let mut entity = previous.as_ref().clone();
                    bytes = bytes.saturating_sub(entity.encoded_len() as usize);
                    // Full replacement preserves absence and future fields. No field merge.
                    entity.state = observed.into();
                    bytes = bytes
                        .checked_add(entity.encoded_len() as usize)
                        .ok_or(CacheFailure::BudgetExceeded)?;
                    if bytes > MAX_VIEW_BYTES {
                        return Err(CacheFailure::BudgetExceeded);
                    }
                    replacements.insert(id, Arc::new(entity));
                }
                view.entities.extend(replacements);
                // Advance only after every observation in this frame was applied.
                view.cursor = cursor.clone();
                view.encoded_bytes = bytes;
                self.stream_sequence = frame.stream_sequence;
                Ok(FrameEffect::Applied)
            }
            None => Err(CacheFailure::InvalidResponse),
        }
    }
}
