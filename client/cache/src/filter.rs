use std::collections::BTreeSet;

use home_client_model::EntityId;
use home_wire_contract::home::state::v1 as wire;
use sha2::{Digest, Sha256};

use crate::{CacheFailure, MAX_ENTITIES};

#[derive(Clone)]
enum Selection {
    All,
    Only(BTreeSet<EntityId>),
}

/// Complete filter retained with every resume cursor. Its digest is not a filter.
#[derive(Clone)]
pub struct Filter {
    selection: Selection,
    digest: [u8; 32],
}

impl Filter {
    pub fn all() -> Self {
        Self {
            selection: Selection::All,
            digest: Sha256::digest([2]).into(),
        }
    }

    pub fn only(ids: impl IntoIterator<Item = EntityId>) -> Result<Self, CacheFailure> {
        let mut selected = BTreeSet::new();
        for id in ids {
            selected.insert(id);
            if selected.len() > MAX_ENTITIES {
                return Err(CacheFailure::InvalidFilter);
            }
        }
        if selected.is_empty() {
            return Err(CacheFailure::InvalidFilter);
        }
        // Matches internal/view.hashIDs: tag + sorted length-prefixed UTF-8.
        let mut hash = Sha256::new();
        hash.update([3]);
        for id in &selected {
            hash.update((id.as_str().len() as u64).to_be_bytes());
            hash.update(id.as_str().as_bytes());
        }
        Ok(Self {
            selection: Selection::Only(selected),
            digest: hash.finalize().into(),
        })
    }

    pub fn to_wire(&self) -> wire::ViewFilter {
        wire::ViewFilter {
            entity_ids: match &self.selection {
                Selection::All => Vec::new(),
                Selection::Only(ids) => ids.iter().map(|id| id.as_str().to_owned()).collect(),
            },
            ..Default::default()
        }
    }

    pub fn digest(&self) -> &[u8; 32] {
        &self.digest
    }

    pub(crate) fn permits(&self, id: &EntityId) -> bool {
        match &self.selection {
            Selection::All => true,
            Selection::Only(ids) => ids.contains(id),
        }
    }
}
