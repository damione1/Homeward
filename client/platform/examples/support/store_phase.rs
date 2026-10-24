use serde::{Deserialize, Serialize, de::IntoDeserializer};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum StorePhase {
    Write,
    Read,
    Clear,
    Absent,
    Hold,
    Blocked,
}

impl std::str::FromStr for StorePhase {
    type Err = serde::de::value::Error;
    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::deserialize(value.into_deserializer())
    }
}
