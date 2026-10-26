//! Opaque list identities and identity-bound optimistic concurrency revisions.
//! Names, row positions and authorization never substitute for these values.
use std::{error::Error, fmt, num::NonZeroU64, str::FromStr};

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

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RevisionError {
    OutOfRange,
}
impl fmt::Display for RevisionError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::OutOfRange => f.write_str("list revision must be a positive signed 64-bit value"),
        }
    }
}
impl Error for RevisionError {}
fn revision(number: u64) -> Result<NonZeroU64, RevisionError> {
    NonZeroU64::new(number)
        .filter(|number| number.get() <= i64::MAX as u64)
        .ok_or(RevisionError::OutOfRange)
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ListRevision {
    list: ListId,
    number: NonZeroU64,
}
impl ListRevision {
    pub fn new(list: ListId, number: u64) -> Result<Self, RevisionError> {
        Ok(Self {
            list,
            number: revision(number)?,
        })
    }
    pub fn list(&self) -> &ListId {
        &self.list
    }
    pub fn number(&self) -> NonZeroU64 {
        self.number
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ItemRevision {
    item: ItemKey,
    number: NonZeroU64,
}
impl ItemRevision {
    pub fn new(item: ItemKey, number: u64) -> Result<Self, RevisionError> {
        Ok(Self {
            item,
            number: revision(number)?,
        })
    }
    pub fn item(&self) -> &ItemKey {
        &self.item
    }
    pub fn number(&self) -> NonZeroU64 {
        self.number
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
    fn equal_numbers_and_item_uids_cannot_alias_other_lists_or_revision_domains() {
        let one: ListId = "one".parse().unwrap();
        let two: ListId = "two".parse().unwrap();
        let uid: ItemUid = "same-item".parse().unwrap();
        let first = ItemKey::new(one.clone(), uid.clone());
        let other = ItemKey::new(two.clone(), uid);
        assert_ne!(first, other);
        assert_ne!(
            ListRevision::new(one, 7).unwrap(),
            ListRevision::new(two, 7).unwrap()
        );
        assert_ne!(
            ItemRevision::new(first.clone(), 7).unwrap(),
            ItemRevision::new(other, 7).unwrap()
        );
        assert_eq!(ItemRevision::new(first.clone(), 99).unwrap().item(), &first);
        for invalid in [0, i64::MAX as u64 + 1, u64::MAX] {
            assert_eq!(
                ItemRevision::new(first.clone(), invalid),
                Err(RevisionError::OutOfRange)
            );
        }
        assert_eq!(
            ItemRevision::new(first, i64::MAX as u64)
                .unwrap()
                .number()
                .get(),
            i64::MAX as u64
        );
    }
}
