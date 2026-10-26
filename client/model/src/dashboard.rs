//! Data-only dashboard intentions. These types confer no authority to dispatch.
use crate::EntityId;
use std::collections::{BTreeMap, BTreeSet};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Profile {
    Basic,
    Recipes,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct UnsupportedProfile;
impl Profile {
    pub const fn name(self) -> &'static str {
        match self {
            Self::Basic => "homeward-dashboard-1",
            Self::Recipes => "homeward-dashboard-2",
        }
    }
    pub const fn layout_version(self) -> u32 {
        match self {
            Self::Basic => 1,
            Self::Recipes => 2,
        }
    }
}
impl TryFrom<&str> for Profile {
    type Error = UnsupportedProfile;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        match value {
            "homeward-dashboard-1" => Ok(Self::Basic),
            "homeward-dashboard-2" => Ok(Self::Recipes),
            _ => Err(UnsupportedProfile),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Gesture {
    Tap,
    Hold,
    DoubleTap,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Presentation {
    Page,
    Popup,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CommandKind {
    Toggle,
    TurnOn,
    TurnOff,
    Activate,
    CoverOpen,
    CoverClose,
    CoverStop,
    Lock,
    Unlock,
    AlarmArmHome,
    AlarmArmAway,
    AlarmDisarm,
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct DashboardId(String);
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct NodeId(String);
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum InvalidIdentity {
    Representation,
}

fn identity(value: &str) -> Result<String, InvalidIdentity> {
    if value.is_empty() || value.len() > 4096 || value.chars().any(char::is_control) {
        Err(InvalidIdentity::Representation)
    } else {
        Ok(value.to_owned())
    }
}
impl DashboardId {
    pub fn parse(value: &str) -> Result<Self, InvalidIdentity> {
        identity(value).map(Self)
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
}
impl NodeId {
    pub fn parse(value: &str) -> Result<Self, InvalidIdentity> {
        identity(value).map(Self)
    }
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Destination {
    pub dashboard: DashboardId,
    pub node: NodeId,
    pub presentation: Presentation,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Intention {
    None,
    MoreInfo(EntityId),
    Navigate(Destination),
    Command { entity: EntityId, kind: CommandKind },
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Source {
    pub file: String,
    pub line: u32,
    pub column: u32,
    pub path: String,
    pub variant: String,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Interaction {
    pub intention: Intention,
    // Some("") requests a native default prompt, and differs from no prompt.
    pub confirmation: Option<String>,
    pub source: Source,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Comparison {
    Equal,
    NotEqual,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Predicate {
    Constant(bool),
    State {
        entity: EntityId,
        expected: String,
        comparison: Comparison,
    },
    All(Vec<Predicate>),
    Any(Vec<Predicate>),
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Visibility {
    Visible,
    Hidden,
    Unavailable,
}
impl Predicate {
    pub fn dependencies(&self) -> BTreeSet<&EntityId> {
        match self {
            Self::Constant(_) => BTreeSet::new(),
            Self::State { entity, .. } => BTreeSet::from([entity]),
            Self::All(children) | Self::Any(children) => {
                children.iter().flat_map(Self::dependencies).collect()
            }
        }
    }
    /// The caller supplies only authorized, fresh, representable observations.
    /// Missing values cannot satisfy even a negative comparison. No HA template
    /// is evaluated and an exact empty string remains a present observation.
    pub fn evaluate<'a>(&self, state: &impl Fn(&EntityId) -> Option<&'a str>) -> Visibility {
        match self {
            Self::Constant(true) => Visibility::Visible,
            Self::Constant(false) => Visibility::Hidden,
            Self::State {
                entity,
                expected,
                comparison,
            } => {
                let Some(actual) = state(entity) else {
                    return Visibility::Unavailable;
                };
                let matches = match comparison {
                    Comparison::Equal => actual == expected,
                    Comparison::NotEqual => actual != expected,
                };
                if matches {
                    Visibility::Visible
                } else {
                    Visibility::Hidden
                }
            }
            Self::All(children) => {
                if children.is_empty() {
                    return Visibility::Unavailable;
                }
                let mut missing = false;
                for child in children {
                    match child.evaluate(state) {
                        Visibility::Hidden => return Visibility::Hidden,
                        Visibility::Unavailable => missing = true,
                        Visibility::Visible => {}
                    }
                }
                if missing {
                    Visibility::Unavailable
                } else {
                    Visibility::Visible
                }
            }
            Self::Any(children) => {
                if children.is_empty() {
                    return Visibility::Unavailable;
                }
                let mut missing = false;
                for child in children {
                    match child.evaluate(state) {
                        Visibility::Visible => return Visibility::Visible,
                        Visibility::Unavailable => missing = true,
                        Visibility::Hidden => {}
                    }
                }
                if missing {
                    Visibility::Unavailable
                } else {
                    Visibility::Hidden
                }
            }
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Recipes {
    pub interactions: BTreeMap<Gesture, Interaction>,
    pub visibility: Option<Predicate>,
}
