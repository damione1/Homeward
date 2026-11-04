//! Read-only correlation names within one retained execution epoch.
use crate::api::{AdmissionError, HomeName, resource_name};
macro_rules! correlation {
    ($name:ident, $collection:literal) => {
        #[derive(Debug, Clone, PartialEq, Eq, Hash)]
        pub struct $name(String);
        impl TryFrom<&str> for $name {
            type Error = AdmissionError;
            fn try_from(value: &str) -> Result<Self, Self::Error> {
                resource_name(value, &["homes", "executionEpochs", $collection])?;
                Ok(Self(value.to_owned()))
            }
        }
        impl $name {
            pub fn as_str(&self) -> &str {
                &self.0
            }
            pub fn belongs_to(&self, home: &HomeName) -> bool {
                self.0
                    .strip_prefix(home.as_str())
                    .is_some_and(|suffix| suffix.starts_with("/executionEpochs/"))
            }
        }
    };
}
correlation!(HistoryRunName, "runs");
correlation!(HistoryControllerName, "controllers");

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn correlation_names_are_canonical_and_bound_to_the_home() {
        let home = HomeName::try_from("homes/synthetic").unwrap();
        let name =
            HistoryRunName::try_from("homes/synthetic/executionEpochs/e-one/runs/r-one").unwrap();
        assert!(name.belongs_to(&home));
        assert!(!name.belongs_to(&HomeName::try_from("homes/synth").unwrap()));
        for invalid in [
            "native-run-42",
            "homes/synthetic/executionEpochs/e-one/controllers/r-one",
            "homes/synthetic/executionEpochs/e-one/runs/r-one/",
            "homes/Synthetic/executionEpochs/e-one/runs/r-one",
        ] {
            assert!(HistoryRunName::try_from(invalid).is_err());
        }
    }
}
