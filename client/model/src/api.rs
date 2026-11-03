//! Admitted API v2 values. Generated protobuf types stay outside this crate.
//! Syntax does not prove permission, supported capabilities or current expiry.
use std::{error::Error, fmt};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AdmissionError {
    ResourceName,
    Percentage,
    RequestId,
    Deadline,
}
impl fmt::Display for AdmissionError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::ResourceName => "invalid resource name",
            Self::Percentage => "percentage must be finite and in [0,100]",
            Self::RequestId => "request ID must be a UUIDv4",
            Self::Deadline => "invalid deadline",
        })
    }
}
impl Error for AdmissionError {}

fn resource_name(value: &str, collections: &[&str]) -> Result<(), AdmissionError> {
    let maximum = collections.iter().map(|c| c.len() + 65).sum::<usize>() - 1;
    if value.len() > maximum {
        return Err(AdmissionError::ResourceName);
    }
    let parts: Vec<_> = value.split('/').collect();
    if parts.len() != 2 * collections.len() {
        return Err(AdmissionError::ResourceName);
    }
    for (i, collection) in collections.iter().enumerate() {
        let id = parts[2 * i + 1];
        if parts[2 * i] != *collection
            || id.is_empty()
            || id.len() > 63
            || id.starts_with('-')
            || id.ends_with('-')
            || !id
                .bytes()
                .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
        {
            return Err(AdmissionError::ResourceName);
        }
    }
    Ok(())
}

macro_rules! name {
    ($type:ident, [$($collection:literal),+]) => {
        #[derive(Debug, Clone, PartialEq, Eq, PartialOrd, Ord, Hash)]
        pub struct $type(String);
        impl TryFrom<&str> for $type {
            type Error = AdmissionError;
            fn try_from(value: &str) -> Result<Self, Self::Error> {
                resource_name(value, &[$($collection),+])?;
                Ok(Self(value.to_owned()))
            }
        }
        impl $type {
            pub fn as_str(&self) -> &str { &self.0 }
        }
    };
}
name!(HomeName, ["homes"]);
name!(EntityName, ["homes", "entities"]);
name!(CapabilityName, ["homes", "entities", "capabilities"]);

/// Only the constructor admits external numeric values; zero is legitimate.
///
/// ```compile_fail
/// use home_client_model::api::Percentage;
/// let bypass = Percentage(f64::NAN);
/// ```
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Percentage(f64);
impl TryFrom<f64> for Percentage {
    type Error = AdmissionError;
    fn try_from(value: f64) -> Result<Self, Self::Error> {
        if value.is_finite() && (0.0..=100.0).contains(&value) {
            Ok(Self(value))
        } else {
            Err(AdmissionError::Percentage)
        }
    }
}
impl Percentage {
    pub fn value(self) -> f64 {
        self.0
    }
}

/// Closed operational intent. Unspecified and unknown wire enums never enter it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Power {
    On,
    Off,
}

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum Intent {
    Power(Power),
    Level(Percentage),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RequestId(String);
impl TryFrom<&str> for RequestId {
    type Error = AdmissionError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        let bytes = value.as_bytes();
        if bytes.len() != 36
            || bytes[14] != b'4'
            || !matches!(bytes[19], b'8' | b'9' | b'a' | b'A' | b'b' | b'B')
            || !bytes.iter().enumerate().all(|(i, b)| {
                if matches!(i, 8 | 13 | 18 | 23) {
                    *b == b'-'
                } else {
                    b.is_ascii_hexdigit()
                }
            })
        {
            return Err(AdmissionError::RequestId);
        }
        Ok(Self(value.to_ascii_lowercase()))
    }
}
impl RequestId {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

/// Protobuf timestamp representation. Runtime deadline checks belong at effect.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Deadline {
    seconds: i64,
    nanos: i32,
}
impl Deadline {
    pub fn new(seconds: i64, nanos: i32) -> Result<Self, AdmissionError> {
        if !(-62_135_596_800..=253_402_300_799).contains(&seconds)
            || !(0..1_000_000_000).contains(&nanos)
            || (seconds == -62_135_596_800 && nanos == 0)
        {
            return Err(AdmissionError::Deadline);
        }
        Ok(Self { seconds, nanos })
    }
    pub fn seconds(self) -> i64 {
        self.seconds
    }
    pub fn nanos(self) -> i32 {
        self.nanos
    }
}

/// Immutable admitted syntax, with no Default or public mutable representation.
#[derive(Debug, Clone, PartialEq)]
pub struct Command {
    target: CapabilityName,
    intent: Intent,
    request_id: Option<RequestId>,
    deadline: Deadline,
}
impl Command {
    pub fn new(
        target: CapabilityName,
        intent: Intent,
        request_id: Option<RequestId>,
        deadline: Deadline,
    ) -> Self {
        Self {
            target,
            intent,
            request_id,
            deadline,
        }
    }
    pub fn target(&self) -> &CapabilityName {
        &self.target
    }
    pub fn intent(&self) -> Intent {
        self.intent
    }
    pub fn request_id(&self) -> Option<&RequestId> {
        self.request_id.as_ref()
    }
    pub fn deadline(&self) -> Deadline {
        self.deadline
    }
}
