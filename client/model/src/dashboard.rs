//! Data-only dashboard intentions. These types confer no authority to dispatch.
pub use crate::api::DashboardName;
use crate::api::{CapabilityName, EntityName, Finite, Power, TemperatureMode};
use std::collections::{BTreeMap, BTreeSet};

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
    AlarmArmNight,
    AlarmDisarm,
}

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
    pub dashboard: DashboardName,
    pub node: NodeId,
    pub presentation: Presentation,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Intention {
    None,
    MoreInfo(EntityName),
    Navigate(Destination),
    Command {
        capability: CapabilityName,
        kind: CommandKind,
    },
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
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum FieldKey {
    State,
    Value,
    CurrentTemperature,
    Setpoint,
    Measurement,
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct FieldBinding {
    pub capability: CapabilityName,
    pub field: FieldKey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TextLiteral(String);
impl TryFrom<&str> for TextLiteral {
    type Error = InvalidIdentity;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.len() > 256 || value.contains('\0') {
            return Err(InvalidIdentity::Representation);
        }
        Ok(Self(value.to_owned()))
    }
}
impl TextLiteral {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct DecimalLiteral(String);
impl TryFrom<&str> for DecimalLiteral {
    type Error = InvalidIdentity;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        if value.len() > 256 {
            return Err(InvalidIdentity::Representation);
        }
        let unsigned = value.strip_prefix('-').unwrap_or(value);
        let (integer, fraction) = unsigned
            .split_once('.')
            .map_or((unsigned, None), |(integer, fraction)| {
                (integer, Some(fraction))
            });
        if integer.is_empty()
            || !integer.bytes().all(|v| v.is_ascii_digit())
            || (integer.len() > 1 && integer.starts_with('0'))
            || fraction.is_some_and(|v| v.is_empty() || !v.bytes().all(|b| b.is_ascii_digit()))
        {
            return Err(InvalidIdentity::Representation);
        }
        Ok(Self(value.to_owned()))
    }
}
impl DecimalLiteral {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CoverState {
    Open,
    Closed,
    Opening,
    Closing,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LockState {
    Locked,
    Unlocked,
    Locking,
    Unlocking,
    Jammed,
    Opening,
    Open,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AlarmState {
    Disarmed,
    ArmedHome,
    ArmedAway,
    ArmedNight,
    ArmedVacation,
    ArmedCustomBypass,
    Pending,
    Arming,
    Disarming,
    Triggered,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum FanSpeed {
    Off,
    Low,
    Medium,
    MediumHigh,
    High,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WeatherCondition {
    Unknown,
    ClearDay,
    ClearNight,
    Cloudy,
    PartlyCloudy,
    Rainy,
    Pouring,
    Snowy,
    SnowyRainy,
    Hail,
    Lightning,
    LightningRainy,
    Windy,
    WindyVariant,
    Fog,
    Exceptional,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Residence {
    Home,
    Away,
    Unknown,
}

/// Typed observation values. Only the native projector interprets source
/// syntax; UI comparisons never coerce text, enum states or numeric families.
#[derive(Clone, Debug, PartialEq)]
pub enum Literal {
    Boolean(bool),
    Integer(i64),
    Number(Finite),
    Text(TextLiteral),
    Decimal(DecimalLiteral),
    Power(Power),
    Cover(CoverState),
    Lock(LockState),
    Alarm(AlarmState),
    TemperatureMode(TemperatureMode),
    FanSpeed(FanSpeed),
    WeatherCondition(WeatherCondition),
    Residence(Residence),
}
// Finite excludes NaN, so its equality is reflexive; the other alternatives
// already implement Eq. This does not equate unlike literal alternatives.
impl Eq for Literal {}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Predicate {
    Constant(bool),
    State {
        field: FieldBinding,
        expected: Literal,
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
    pub fn dependencies(&self) -> BTreeSet<&FieldBinding> {
        match self {
            Self::Constant(_) => BTreeSet::new(),
            Self::State { field, .. } => BTreeSet::from([field]),
            Self::All(children) | Self::Any(children) => {
                children.iter().flat_map(Self::dependencies).collect()
            }
        }
    }
    /// The caller supplies only authorized, fresh, representable observations.
    /// Missing values cannot satisfy even a negative comparison. No source syntax
    /// is evaluated and an exact empty text remains a present observation.
    pub fn evaluate<'a>(
        &self,
        state: &impl Fn(&FieldBinding) -> Option<&'a Literal>,
    ) -> Visibility {
        match self {
            Self::Constant(true) => Visibility::Visible,
            Self::Constant(false) => Visibility::Hidden,
            Self::State {
                field,
                expected,
                comparison,
            } => {
                let Some(actual) = state(field) else {
                    return Visibility::Unavailable;
                };
                // A different typed family is unavailable evidence, not an
                // unequal value that could make a visibility guard succeed.
                if std::mem::discriminant(actual) != std::mem::discriminant(expected) {
                    return Visibility::Unavailable;
                }
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
