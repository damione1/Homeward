//! Exact native identities used as UI keys inside a current Home cohort.
//! Names, row positions and authorization never substitute for these values.
use std::{error::Error, fmt, str::FromStr};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum IdentityError {
    Empty,
    TooLong,
    ControlCharacter,
}
impl fmt::Display for IdentityError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Empty => "list identity is empty",
            Self::TooLong => "list identity exceeds 255 UTF-8 bytes",
            Self::ControlCharacter => "list identity contains a forbidden control character",
        })
    }
}
impl Error for IdentityError {}

macro_rules! identity {
    ($name:ident) => {
        #[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
        pub struct $name(String);
        impl FromStr for $name {
            type Err = IdentityError;
            fn from_str(value: &str) -> Result<Self, Self::Err> {
                if value.is_empty() {
                    return Err(IdentityError::Empty);
                }
                if value.len() > 255 {
                    return Err(IdentityError::TooLong);
                }
                if value.bytes().any(|b| matches!(b, b'\0' | b'\r' | b'\n')) {
                    return Err(IdentityError::ControlCharacter);
                }
                Ok(Self(value.to_owned()))
            }
        }
        impl $name {
            pub fn as_str(&self) -> &str {
                &self.0
            }
        }
    };
}
identity!(ListId);
identity!(ItemUid);

/// Stable identity across moves. An item UID is scoped to its list.
#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct ItemKey {
    list: ListId,
    uid: ItemUid,
}
impl ItemKey {
    pub fn new(list: ListId, uid: ItemUid) -> Self {
        Self { list, uid }
    }
    pub fn list(&self) -> &ListId {
        &self.list
    }
    pub fn uid(&self) -> &ItemUid {
        &self.uid
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn opaque_identities_follow_server_byte_limits_without_trimming_or_uuid_assumptions() {
        for raw in [
            "list:arbitrary",
            " Liste été ",
            "同じ項目",
            &"a".repeat(255),
        ] {
            assert_eq!(raw.parse::<ListId>().unwrap().as_str(), raw);
            assert_eq!(raw.parse::<ItemUid>().unwrap().as_str(), raw);
        }
        for raw in [
            "",
            "a\0b",
            "a\rb",
            "a\nb",
            &"a".repeat(256),
            &"é".repeat(128),
        ] {
            assert!(raw.parse::<ListId>().is_err());
            assert!(raw.parse::<ItemUid>().is_err());
        }
    }

    #[test]
    fn item_identity_is_scoped_to_its_list() {
        let one = ItemKey::new("one".parse().unwrap(), "same".parse().unwrap());
        let two = ItemKey::new("two".parse().unwrap(), "same".parse().unwrap());
        assert_ne!(one, two);
    }
}
