//! Canonical human-account resources. Names admit syntax, never authority.
use crate::api::{AdmissionError, HomeName, resource_name};

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
name!(AccountName, ["homes", "accounts"]);
name!(InvitationName, ["homes", "invitations"]);
name!(SessionName, ["homes", "accounts", "sessions"]);
name!(
    FactorCredentialName,
    ["homes", "accounts", "factorCredentials"]
);
name!(McpKeyName, ["homes", "accounts", "mcpKeys"]);
impl AccountName {
    pub fn home(&self) -> HomeName {
        HomeName::try_from(
            self.0
                .rsplit_once("/accounts/")
                .expect("admitted account")
                .0,
        )
        .expect("admitted home")
    }
}
macro_rules! parent {
    ($type:ident, $collection:literal) => {
        impl $type {
            pub fn account(&self) -> AccountName {
                AccountName(
                    self.0
                        .rsplit_once(concat!("/", $collection, "/"))
                        .expect("admitted child")
                        .0
                        .to_owned(),
                )
            }
        }
    };
}
parent!(SessionName, "sessions");
parent!(FactorCredentialName, "factorCredentials");
parent!(McpKeyName, "mcpKeys");
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AuthorizationPolicyName(AccountName);
impl TryFrom<&str> for AuthorizationPolicyName {
    type Error = AdmissionError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        Ok(Self(AccountName::try_from(
            value
                .strip_suffix("/authorizationPolicy")
                .ok_or(AdmissionError::ResourceName)?,
        )?))
    }
}
impl AuthorizationPolicyName {
    pub fn account(&self) -> &AccountName {
        &self.0
    }
    pub fn as_string(&self) -> String {
        format!("{}/authorizationPolicy", self.0.as_str())
    }
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SecondFactorPolicyName(HomeName);
impl TryFrom<&str> for SecondFactorPolicyName {
    type Error = AdmissionError;
    fn try_from(value: &str) -> Result<Self, Self::Error> {
        Ok(Self(HomeName::try_from(
            value
                .strip_suffix("/secondFactorPolicy")
                .ok_or(AdmissionError::ResourceName)?,
        )?))
    }
}
impl SecondFactorPolicyName {
    pub fn home(&self) -> &HomeName {
        &self.0
    }
    pub fn as_string(&self) -> String {
        format!("{}/secondFactorPolicy", self.0.as_str())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn session_identity_retains_exact_account_and_home_boundaries() {
        let session =
            SessionName::try_from("homes/house-a/accounts/person-a/sessions/session-a").unwrap();
        assert_eq!(
            session.account().as_str(),
            "homes/house-a/accounts/person-a"
        );
        assert_eq!(session.account().home().as_str(), "homes/house-a");
        assert!(AccountName::try_from(session.as_str()).is_err());
        assert!(McpKeyName::try_from(session.as_str()).is_err());
        for name in [
            "homes/h/accounts/a/sessions/../b",
            "homes/h/accounts/a/sessions/x%2fy",
            "homes/H/accounts/a/sessions/b",
            "homes/h/accounts/a/sessions/-b",
            "homes/h/accounts/a/sessions/b/",
        ] {
            assert!(SessionName::try_from(name).is_err(), "{name}");
        }
    }
    #[test]
    fn policy_singletons_cannot_cross_resource_families() {
        let policy =
            AuthorizationPolicyName::try_from("homes/h/accounts/a/authorizationPolicy").unwrap();
        assert_eq!(policy.account().as_str(), "homes/h/accounts/a");
        assert!(AuthorizationPolicyName::try_from("homes/h/authorizationPolicy").is_err());
        assert!(SecondFactorPolicyName::try_from(policy.as_string().as_str()).is_err());
        assert_eq!(
            SecondFactorPolicyName::try_from("homes/h/secondFactorPolicy")
                .unwrap()
                .home()
                .as_str(),
            "homes/h"
        );
    }
}
