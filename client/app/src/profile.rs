//! Public endpoint configuration. Credentials never enter this representation.
use std::{io::Cursor, path::Path};

use base64::{Engine as _, engine::general_purpose::STANDARD};
use home_client_session::CredentialKey;
use home_client_transport::Connection;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub(crate) struct TrustedEndpoint {
    origin: String,
    certificates: String,
    fingerprint: String,
    key: CredentialKey,
    connection: Connection,
}

#[derive(Clone, Copy, Debug)]
pub(crate) enum ProfileFailure {
    InvalidEndpoint,
    InvalidTrust,
    Unavailable,
    UnsupportedConfiguration,
}

// A versioned external boundary; every load is revalidated before use.
#[derive(Serialize, Deserialize)]
#[serde(tag = "version", deny_unknown_fields)]
enum SavedProfile {
    #[serde(rename = "1")]
    V1 {
        origin: String,
        certificates: String,
    },
}

impl TrustedEndpoint {
    pub fn parse(origin: &str, certificates: &str) -> Result<Self, ProfileFailure> {
        if origin.len() > 4096 || certificates.len() > 256 * 1024 {
            return Err(ProfileFailure::InvalidEndpoint);
        }
        let parsed = url::Url::parse(origin).map_err(|_| ProfileFailure::InvalidEndpoint)?;
        if parsed.scheme() != "https"
            || parsed.host_str().is_none()
            || !parsed.username().is_empty()
            || parsed.password().is_some()
            || parsed.path() != "/"
            || parsed.query().is_some()
            || parsed.fragment().is_some()
        {
            return Err(ProfileFailure::InvalidEndpoint);
        }
        let origin = parsed.origin().ascii_serialization();
        let mut roots: Vec<Vec<u8>> = rustls_pemfile::certs(&mut Cursor::new(certificates))
            .map(|result| result.map(|certificate| certificate.as_ref().to_vec()))
            .collect::<Result<_, _>>()
            .map_err(|_| ProfileFailure::InvalidTrust)?;
        roots.sort();
        roots.dedup();
        if roots.is_empty() || roots.len() > 16 {
            return Err(ProfileFailure::InvalidTrust);
        }
        // Persist only parsed public certificates, never arbitrary pasted text
        // or private-key material adjacent to a certificate in the input.
        let mut certificates = String::new();
        let mut trust = Sha256::new();
        for root in &roots {
            trust.update((root.len() as u64).to_le_bytes());
            trust.update(root);
            certificates.push_str("-----BEGIN CERTIFICATE-----\n");
            let encoded = STANDARD.encode(root);
            for line in encoded.as_bytes().chunks(64) {
                certificates.push_str(std::str::from_utf8(line).expect("base64 ASCII"));
                certificates.push('\n');
            }
            certificates.push_str("-----END CERTIFICATE-----\n");
        }
        let trust = trust.finalize();
        let fingerprint = trust.iter().map(|byte| format!("{byte:02x}")).collect();
        let mut key = Sha256::new();
        key.update(b"Homeward-secure-session-v1\0");
        key.update((origin.len() as u64).to_le_bytes());
        key.update(origin.as_bytes());
        key.update(trust);
        let connection = Connection::new(&origin, certificates.as_bytes())
            .map_err(|_| ProfileFailure::InvalidTrust)?;
        Ok(Self {
            origin,
            certificates,
            fingerprint,
            key: CredentialKey::from_digest(key.finalize().into()),
            connection,
        })
    }
    pub fn origin(&self) -> &str {
        &self.origin
    }
    pub fn certificates(&self) -> &str {
        &self.certificates
    }
    pub fn fingerprint(&self) -> &str {
        &self.fingerprint
    }
    pub fn key(&self) -> CredentialKey {
        self.key
    }
    pub fn connection(&self) -> Connection {
        self.connection.clone()
    }

    pub fn load(directory: &Path) -> Result<Option<Self>, ProfileFailure> {
        let path = directory.join("endpoint.json");
        use std::io::Read;
        let mut file = match std::fs::File::open(path) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(_) => return Err(ProfileFailure::Unavailable),
        };
        let mut bytes = Vec::new();
        file.by_ref()
            .take(256 * 1024 + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| ProfileFailure::Unavailable)?;
        if bytes.len() > 256 * 1024 {
            return Err(ProfileFailure::UnsupportedConfiguration);
        }
        match serde_json::from_slice(&bytes)
            .map_err(|_| ProfileFailure::UnsupportedConfiguration)?
        {
            SavedProfile::V1 {
                origin,
                certificates,
            } => Self::parse(&origin, &certificates).map(Some),
        }
    }
    pub fn save(&self, directory: &Path) -> Result<(), ProfileFailure> {
        use std::{
            fs::{DirBuilder, OpenOptions},
            io::Write,
            os::unix::fs::{DirBuilderExt, OpenOptionsExt},
        };
        DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(directory)
            .map_err(|_| ProfileFailure::Unavailable)?;
        let bytes = serde_json::to_vec(&SavedProfile::V1 {
            origin: self.origin.clone(),
            certificates: self.certificates.clone(),
        })
        .map_err(|_| ProfileFailure::UnsupportedConfiguration)?;
        let temporary = directory.join(format!("endpoint.{}.writing", std::process::id()));
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .mode(0o600)
            .open(&temporary)
            .map_err(|_| ProfileFailure::Unavailable)?;
        let result = (|| -> std::io::Result<()> {
            file.write_all(&bytes)?;
            file.sync_all()?;
            std::fs::rename(&temporary, directory.join("endpoint.json"))
        })();
        if result.is_err() {
            let _ = std::fs::remove_file(&temporary);
        }
        result.map_err(|_| ProfileFailure::Unavailable)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    const TRUST_A: &str = include_str!("../tests/fixtures/trust-a.pem");
    const TRUST_B: &str = include_str!("../tests/fixtures/trust-b.pem");

    #[test]
    fn credential_namespace_is_bound_to_canonical_origin_and_public_trust_set() {
        let first = TrustedEndpoint::parse(
            "https://HOME.example.test:443/",
            &format!("{TRUST_A}{TRUST_B}"),
        )
        .unwrap();
        let reordered = TrustedEndpoint::parse(
            "https://home.example.test",
            &format!("{TRUST_B}{TRUST_A}{TRUST_A}"),
        )
        .unwrap();
        assert_eq!(first.key().identifier(), reordered.key().identifier());
        assert_ne!(
            first.key().identifier(),
            TrustedEndpoint::parse("https://other.example.test", &format!("{TRUST_A}{TRUST_B}"))
                .unwrap()
                .key()
                .identifier()
        );
        assert_ne!(
            first.key().identifier(),
            TrustedEndpoint::parse(
                "https://home.example.test:8443",
                &format!("{TRUST_A}{TRUST_B}")
            )
            .unwrap()
            .key()
            .identifier()
        );
        assert_ne!(
            first.key().identifier(),
            TrustedEndpoint::parse("https://home.example.test", TRUST_A)
                .unwrap()
                .key()
                .identifier()
        );
    }

    #[test]
    fn server_draft_cannot_hide_credentials_or_redirect_the_rpc_path() {
        for origin in [
            "http://home.example.test",
            "https://user:secret@home.example.test",
            "https://home.example.test/api",
            "https://home.example.test/?secret=1",
            "https://home.example.test/#fragment",
        ] {
            assert!(TrustedEndpoint::parse(origin, TRUST_A).is_err());
        }
        assert!(TrustedEndpoint::parse("https://home.example.test", "").is_err());
    }

    #[test]
    fn only_parsed_public_material_is_saved_and_unknown_versions_fail_closed() {
        use std::{
            os::unix::fs::{DirBuilderExt, PermissionsExt},
            time::{SystemTime, UNIX_EPOCH},
        };
        struct Directory(std::path::PathBuf);
        impl Drop for Directory {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let path = std::env::temp_dir().join(format!(
            "homeward-profile-{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::DirBuilder::new()
            .mode(0o700)
            .create(&path)
            .unwrap();
        let directory = Directory(path);
        let draft = format!("{TRUST_A}\naccidental-private-draft-material\n");
        let profile = TrustedEndpoint::parse("https://home.example.test", &draft).unwrap();
        profile.save(&directory.0).unwrap();
        let config = directory.0.join("endpoint.json");
        assert!(
            !std::fs::read_to_string(&config)
                .unwrap()
                .contains("accidental-private-draft-material")
        );
        assert_eq!(
            std::fs::metadata(&config).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(
            TrustedEndpoint::load(&directory.0)
                .unwrap()
                .unwrap()
                .key()
                .identifier(),
            profile.key().identifier()
        );
        std::fs::write(
            &config,
            br#"{"version":"future","origin":"https://home.example.test"}"#,
        )
        .unwrap();
        assert!(matches!(
            TrustedEndpoint::load(&directory.0),
            Err(ProfileFailure::UnsupportedConfiguration)
        ));
    }
}
