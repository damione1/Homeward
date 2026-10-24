use serde::{Deserialize, Serialize, de::IntoDeserializer};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum SessionScenario {
    Login,
    WrongPassword,
    RateLimit,
    Enrollment,
    Recovery,
    Persist,
    Restore,
    Concurrent,
    Lose,
    RestoreUncertain,
    Cleanup,
}

impl std::str::FromStr for SessionScenario {
    type Err = serde::de::value::Error;
    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::deserialize(value.into_deserializer())
    }
}

impl SessionScenario {
    pub fn requires_stored_record(self) -> bool {
        match self {
            Self::Restore | Self::RestoreUncertain | Self::Cleanup => true,
            Self::Login
            | Self::WrongPassword
            | Self::RateLimit
            | Self::Enrollment
            | Self::Recovery
            | Self::Persist
            | Self::Concurrent
            | Self::Lose => false,
        }
    }
}
