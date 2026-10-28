//! Versioned, non-secret UI preferences. No installation or session data.
use crate::HomewardWindow;
use serde::{Deserialize, Serialize};
use std::{
    io::{Read, Write},
    path::Path,
};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
enum Appearance {
    #[default]
    Light,
    Dark,
}
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
enum Surfaces {
    #[default]
    Opaque,
    Translucent,
}
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
enum TextSize {
    #[default]
    Standard,
    Double,
}
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
enum Motion {
    #[default]
    Reduced,
    Full,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct Preferences {
    appearance: Appearance,
    surfaces: Surfaces,
    text: TextSize,
    motion: Motion,
}
#[derive(Serialize, Deserialize)]
#[serde(tag = "version", deny_unknown_fields)]
enum Saved {
    #[serde(rename = "1")]
    V1 { preferences: Preferences },
}

impl Preferences {
    pub fn from_ui(dark: bool, opaque: bool, large: bool, reduced: bool) -> Self {
        Self {
            appearance: if dark {
                Appearance::Dark
            } else {
                Appearance::Light
            },
            surfaces: if opaque {
                Surfaces::Opaque
            } else {
                Surfaces::Translucent
            },
            text: if large {
                TextSize::Double
            } else {
                TextSize::Standard
            },
            motion: if reduced {
                Motion::Reduced
            } else {
                Motion::Full
            },
        }
    }
    pub fn current(window: &HomewardWindow) -> Self {
        Self::from_ui(
            window.get_dark_mode(),
            window.get_opaque_surfaces(),
            window.get_large_text(),
            window.get_reduced_motion(),
        )
    }
    pub fn apply(self, window: &HomewardWindow) {
        home_client_platform::appearance::apply(match self.appearance {
            Appearance::Light => home_client_platform::appearance::SystemBars::Light,
            Appearance::Dark => home_client_platform::appearance::SystemBars::Dark,
        });
        window.set_dark_mode(match self.appearance {
            Appearance::Light => false,
            Appearance::Dark => true,
        });
        window.set_opaque_surfaces(match self.surfaces {
            Surfaces::Opaque => true,
            Surfaces::Translucent => false,
        });
        window.set_large_text(match self.text {
            TextSize::Standard => false,
            TextSize::Double => true,
        });
        window.set_reduced_motion(match self.motion {
            Motion::Reduced => true,
            Motion::Full => false,
        });
    }
    pub fn load(directory: &Path) -> std::io::Result<Self> {
        let mut file = match std::fs::File::open(directory.join("interface.json")) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(Self::default());
            }
            Err(error) => return Err(error),
        };
        let mut bytes = Vec::new();
        Read::by_ref(&mut file).take(4097).read_to_end(&mut bytes)?;
        if bytes.len() > 4096 {
            return Err(std::io::Error::other("oversized interface preferences"));
        }
        match serde_json::from_slice(&bytes).map_err(std::io::Error::other)? {
            Saved::V1 { preferences } => Ok(preferences),
        }
    }
    pub fn save(self, directory: &Path) -> std::io::Result<()> {
        use std::{
            fs::{DirBuilder, OpenOptions},
            os::unix::fs::{DirBuilderExt, OpenOptionsExt},
        };
        DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(directory)?;
        let bytes =
            serde_json::to_vec(&Saved::V1 { preferences: self }).map_err(std::io::Error::other)?;
        let nonce = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_err(std::io::Error::other)?
            .as_nanos();
        let temporary = directory.join(format!("interface.{}.{nonce}.writing", std::process::id()));
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .mode(0o600)
            .open(&temporary)?;
        let result = (|| {
            file.write_all(&bytes)?;
            file.sync_all()?;
            std::fs::rename(&temporary, directory.join("interface.json"))
        })();
        if result.is_err() {
            let _ = std::fs::remove_file(&temporary);
        }
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn preferences_persist_atomically_with_private_permissions_and_bounded_input() {
        use std::os::unix::fs::PermissionsExt;
        let nonce = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let directory = std::env::temp_dir().join(format!(
            "homeward-ui-preferences-{}-{nonce}",
            std::process::id()
        ));
        std::fs::create_dir(&directory).unwrap();
        struct OwnedDirectory(std::path::PathBuf);
        impl Drop for OwnedDirectory {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let owned = OwnedDirectory(directory);
        assert_eq!(Preferences::load(&owned.0).unwrap(), Preferences::default());
        let first = Preferences::from_ui(true, false, true, false);
        first.save(&owned.0).unwrap();
        assert_eq!(Preferences::load(&owned.0).unwrap(), first);
        assert_eq!(
            std::fs::metadata(owned.0.join("interface.json"))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
        Preferences::default().save(&owned.0).unwrap();
        assert_eq!(Preferences::load(&owned.0).unwrap(), Preferences::default());
        assert_eq!(
            std::fs::read_dir(&owned.0).unwrap().count(),
            1,
            "successful saves leave no temporary files"
        );
        std::fs::write(owned.0.join("interface.json"), vec![b' '; 4097]).unwrap();
        assert!(Preferences::load(&owned.0).is_err());
    }
    #[test]
    fn only_ui_preferences_round_trip_and_unknown_versions_are_rejected() {
        let values = Preferences::from_ui(true, false, true, false);
        let bytes = serde_json::to_vec(&Saved::V1 {
            preferences: values,
        })
        .unwrap();
        let Saved::V1 { preferences } = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(preferences, values);
        assert!(serde_json::from_slice::<Saved>(br#"{"version":"2","preferences":{}}"#).is_err());
        let mut object: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        object["preferences"]["session"] = serde_json::json!("must never be accepted");
        assert!(serde_json::from_value::<Saved>(object).is_err());
    }
}
