//! Native client domain types. Transport and Slint stay outside this crate.
#![deny(clippy::wildcard_enum_match_arm)]

use std::{error::Error, fmt, str::FromStr};

pub mod dashboard;
pub mod lists;
pub mod local_time;

/// Opaque internal identity. Parsing proves representation, not host provenance.
/// No default, public field or mutable string access can create an empty identity.
///
/// ```compile_fail
/// use home_client_model::EntityId;
/// let id = EntityId(String::new());
/// ```
#[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct EntityId(String);

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ParseEntityIdError {
    Empty,
}

impl fmt::Display for ParseEntityIdError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Empty => f.write_str("entity ID must be nonempty UTF-8"),
        }
    }
}

impl Error for ParseEntityIdError {}

impl FromStr for EntityId {
    type Err = ParseEntityIdError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        if value.is_empty() {
            return Err(ParseEntityIdError::Empty);
        }
        Ok(Self(value.to_owned()))
    }
}

impl EntityId {
    /// Preserves the exact value, without imposing an unsourced ID format.
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
