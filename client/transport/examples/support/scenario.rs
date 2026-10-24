use serde::{Deserialize, de::IntoDeserializer};

#[derive(Clone, Copy, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum Scenario {
    Full,
    WrongName,
}

impl std::str::FromStr for Scenario {
    type Err = serde::de::value::Error;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::deserialize(value.into_deserializer())
    }
}
