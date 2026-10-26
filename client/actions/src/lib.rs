//! Client action identity and progress, independent of widgets and observed
//! values. No persistence, offline queue or automatic command resubmission.
#![deny(clippy::wildcard_enum_match_arm)]

mod access;
mod controller;
pub mod lists;
pub use controller::{
    ActionClient, ActionController, ActionHandle, ActionNotice, ActionSnapshot, AdmissionCheck,
    ControllerError, NoticeKind, TrackingIssue,
};
mod target;
mod tracking;
pub use access::{WriteAccess, write_access};
pub use target::{ActionTarget, Endpoint, SchemaId};
pub use tracking::{ActionRecord, LiveState, SubmissionState, TrackingError};

use home_wire_contract::actions_contract;
use std::{fmt, str::FromStr};

#[derive(Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct CommandId(String);

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum IdentityFailure {
    Invalid,
    RandomUnavailable,
}

impl CommandId {
    pub fn generate() -> Result<Self, IdentityFailure> {
        let mut bytes = [0u8; 16];
        getrandom::fill(&mut bytes).map_err(|_| IdentityFailure::RandomUnavailable)?;
        Ok(Self(
            bytes.iter().map(|byte| format!("{byte:02x}")).collect(),
        ))
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
impl FromStr for CommandId {
    type Err = IdentityFailure;
    fn from_str(value: &str) -> Result<Self, Self::Err> {
        if actions_contract::command_id(value) {
            Ok(Self(value.to_owned()))
        } else {
            Err(IdentityFailure::Invalid)
        }
    }
}
impl fmt::Debug for CommandId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("CommandId(..)")
    }
}
