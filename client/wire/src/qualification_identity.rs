//! Explicit synthetic-harness native identities, absent from the product graph.
//! Only the private issuer's verified lease supplies these values. Public opaque
//! AccountName/SessionName values are never decoded or looked up by the client.
use crate::home::auth::v1 as native;
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Invalid {
    Identity,
}
macro_rules! identity {
    ($name:ident,$wire:ty) => {
        #[derive(Debug, Clone, PartialEq, Eq)]
        pub struct $name(String);
        impl $name {
            pub fn parse(value: &str) -> Result<Self, Invalid> {
                if value.is_empty() || value.len() > 128 || value.contains(['\0', '\r', '\n']) {
                    return Err(Invalid::Identity);
                }
                Ok(Self(value.to_owned()))
            }
            pub fn as_str(&self) -> &str {
                &self.0
            }
        }
        impl TryFrom<&$wire> for $name {
            type Error = Invalid;
            fn try_from(value: &$wire) -> Result<Self, Invalid> {
                if !value.__buffa_unknown_fields.is_empty() {
                    return Err(Invalid::Identity);
                }
                Self::parse(&value.value)
            }
        }
    };
}
identity!(AccountId, native::AccountId);
identity!(SessionId, native::SessionId);
