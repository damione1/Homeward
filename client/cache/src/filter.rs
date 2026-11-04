use crate::CacheFailure;
use home_client_model::api::EntityName;
use std::collections::BTreeSet;

/// Typed selection over public identities. The server admits the full bounded
/// AIP-160 expression; no native ID, grant digest or cursor is constructed here.
#[derive(Clone)]
pub struct Filter {
    names: Option<BTreeSet<EntityName>>,
    expression: String,
}
impl Filter {
    pub fn all() -> Self {
        Self {
            names: None,
            expression: String::new(),
        }
    }
    pub fn only(ids: impl IntoIterator<Item = EntityName>) -> Result<Self, CacheFailure> {
        let mut names = BTreeSet::new();
        for id in ids {
            names.insert(id);
            if names.len() > 32 {
                return Err(CacheFailure::InvalidFilter);
            }
        }
        if names.is_empty() {
            return Err(CacheFailure::InvalidFilter);
        }
        let expression = names
            .iter()
            .map(|name| format!("name = \"{}\"", name.as_str()))
            .collect::<Vec<_>>()
            .join(" OR ");
        if expression.len() > 4096 {
            return Err(CacheFailure::InvalidFilter);
        }
        Ok(Self {
            names: Some(names),
            expression,
        })
    }
    pub fn expression(&self) -> &str {
        &self.expression
    }
    pub(crate) fn permits(&self, name: &EntityName) -> bool {
        self.names.as_ref().is_none_or(|names| names.contains(name))
    }
}
