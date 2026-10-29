//! Durable read selectors, never saved consents, telemetry, credentials or jobs.
//! A registration nonce is committed before its one explicit creation request.
//! Recovery can only GET that registration under the same native subject/epoch.
use home_client_session::{CredentialKey, SecureStore, StoreFailure};
use home_wire_contract::{
    auth_contract::{AccountId, SessionId},
    companion_contract::{Epoch, InstallationId, RequestId},
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::sync::{Arc, Mutex};
use zeroize::Zeroizing;

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Registration {
    account: String,
    session: String,
    epoch: String,
    parent: Option<String>,
    nonce: String,
}
#[derive(Serialize, Deserialize)]
#[serde(tag = "version", deny_unknown_fields)]
enum Record {
    #[serde(rename = "1")]
    Registrations {
        companion: Option<Registration>,
        notifications: Option<Registration>,
    },
}
#[derive(Clone, Copy)]
pub(crate) enum Kind {
    Companion,
    Notifications,
}
pub(crate) struct Subject<'a> {
    pub account: &'a AccountId,
    pub session: &'a SessionId,
    pub epoch: &'a str,
    pub parent: Option<&'a str>,
}
impl Subject<'_> {
    fn registration(&self, nonce: String) -> Registration {
        Registration {
            account: self.account.as_str().into(),
            session: self.session.as_str().into(),
            epoch: self.epoch.into(),
            parent: self.parent.map(str::to_owned),
            nonce,
        }
    }
}
pub(crate) struct Registrations {
    store: Arc<dyn SecureStore>,
    serial: Mutex<()>,
}
impl Registrations {
    pub fn key(endpoint: CredentialKey) -> CredentialKey {
        let mut digest = Sha256::new();
        digest.update(b"Homeward-registration-read-selectors-v1\0");
        digest.update(endpoint.identifier().as_bytes());
        CredentialKey::from_digest(digest.finalize().into())
    }
    pub fn new(store: Arc<dyn SecureStore>) -> Arc<Self> {
        Arc::new(Self {
            store,
            serial: Mutex::new(()),
        })
    }
    fn load(&self) -> Result<Record, StoreFailure> {
        let Some(bytes) = self.store.load()? else {
            return Ok(Record::Registrations {
                companion: None,
                notifications: None,
            });
        };
        if bytes.len() > 8192 {
            return Err(StoreFailure::Corrupt);
        }
        let record: Record = serde_json::from_slice(&bytes).map_err(|_| StoreFailure::Corrupt)?;
        let Record::Registrations {
            companion,
            notifications,
        } = &record;
        for value in [companion, notifications].into_iter().flatten() {
            if AccountId::parse(&value.account).is_err()
                || SessionId::parse(&value.session).is_err()
                || RequestId::parse(&value.nonce).is_err()
                || Epoch::parse(&value.epoch).is_err()
                || value
                    .parent
                    .as_ref()
                    .is_some_and(|id| InstallationId::parse(id).is_err())
            {
                return Err(StoreFailure::Corrupt);
            }
        }
        Ok(record)
    }
    /// All OS calls are serialized on an owned blocking worker, away from Slint.
    pub async fn lookup(
        self: &Arc<Self>,
        kind: Kind,
        subject: Subject<'_>,
    ) -> Result<Option<String>, StoreFailure> {
        let wanted = subject.registration(String::new());
        let owner = self.clone();
        tokio::task::spawn_blocking(move || {
            let _serial = owner.serial.lock().map_err(|_| StoreFailure::Unavailable)?;
            let Record::Registrations {
                companion,
                notifications,
            } = owner.load()?;
            let value = match kind {
                Kind::Companion => companion,
                Kind::Notifications => notifications,
            };
            Ok(value
                .filter(|value| {
                    value.account == wanted.account
                        && value.session == wanted.session
                        && value.epoch == wanted.epoch
                        && value.parent == wanted.parent
                })
                .map(|value| value.nonce))
        })
        .await
        .map_err(|_| StoreFailure::Unavailable)?
    }
    pub async fn remember(
        self: &Arc<Self>,
        kind: Kind,
        subject: Subject<'_>,
        nonce: &str,
    ) -> Result<(), StoreFailure> {
        RequestId::parse(nonce).map_err(|_| StoreFailure::Corrupt)?;
        let value = subject.registration(nonce.into());
        let owner = self.clone();
        tokio::task::spawn_blocking(move || {
            let _serial = owner.serial.lock().map_err(|_| StoreFailure::Unavailable)?;
            let Record::Registrations { companion, .. } = owner.load()?;
            let (companion, notifications) = match kind {
                Kind::Companion => (Some(value), None),
                Kind::Notifications => (companion, Some(value)),
            };
            let bytes = Zeroizing::new(
                serde_json::to_vec(&Record::Registrations {
                    companion,
                    notifications,
                })
                .map_err(|_| StoreFailure::Corrupt)?,
            );
            owner.store.save(&bytes)
        })
        .await
        .map_err(|_| StoreFailure::Unavailable)?
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[derive(Default)]
    struct Store {
        bytes: Mutex<Option<Vec<u8>>>,
        fail: Mutex<bool>,
    }
    impl SecureStore for Store {
        fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
            Ok(self.bytes.lock().unwrap().clone().map(Zeroizing::new))
        }
        fn save(&self, bytes: &[u8]) -> Result<(), StoreFailure> {
            if *self.fail.lock().unwrap() {
                return Err(StoreFailure::Unavailable);
            }
            *self.bytes.lock().unwrap() = Some(bytes.to_vec());
            Ok(())
        }
        fn clear(&self) -> Result<(), StoreFailure> {
            *self.bytes.lock().unwrap() = None;
            Ok(())
        }
    }
    const EPOCH: &str = "11111111111111111111111111111111";
    const NONCE: &str = "22222222222222222222222222222222";
    const PARENT: &str = "33333333333333333333333333333333";
    fn subject<'a>(
        account: &'a AccountId,
        session: &'a SessionId,
        parent: Option<&'a str>,
    ) -> Subject<'a> {
        Subject {
            account,
            session,
            epoch: EPOCH,
            parent,
        }
    }
    #[tokio::test]
    async fn restart_restores_read_selectors_bound_to_subject_epoch_and_parent() {
        let store = Arc::new(Store::default());
        let account = AccountId::parse("account").unwrap();
        let session = SessionId::parse("session").unwrap();
        let first = Registrations::new(store.clone());
        first
            .remember(Kind::Companion, subject(&account, &session, None), NONCE)
            .await
            .unwrap();
        first
            .remember(
                Kind::Notifications,
                subject(&account, &session, Some(PARENT)),
                NONCE,
            )
            .await
            .unwrap();
        drop(first);
        let restarted = Registrations::new(store);
        assert_eq!(
            restarted
                .lookup(Kind::Companion, subject(&account, &session, None))
                .await
                .unwrap()
                .as_deref(),
            Some(NONCE)
        );
        assert_eq!(
            restarted
                .lookup(
                    Kind::Notifications,
                    subject(&account, &session, Some(PARENT))
                )
                .await
                .unwrap()
                .as_deref(),
            Some(NONCE)
        );
        let foreign_account = AccountId::parse("other-account").unwrap();
        let foreign_session = SessionId::parse("other-session").unwrap();
        for rejected in [
            subject(&foreign_account, &session, None),
            subject(&account, &foreign_session, None),
            Subject {
                epoch: PARENT,
                ..subject(&account, &session, None)
            },
        ] {
            assert!(
                restarted
                    .lookup(Kind::Companion, rejected)
                    .await
                    .unwrap()
                    .is_none()
            );
        }
        assert!(
            restarted
                .lookup(
                    Kind::Notifications,
                    subject(&account, &session, Some(EPOCH))
                )
                .await
                .unwrap()
                .is_none()
        );
    }
    #[tokio::test]
    async fn unavailable_or_corrupt_storage_never_becomes_an_empty_registration() {
        let store = Arc::new(Store::default());
        let owner = Registrations::new(store.clone());
        let account = AccountId::parse("account").unwrap();
        let session = SessionId::parse("session").unwrap();
        *store.fail.lock().unwrap() = true;
        assert_eq!(
            owner
                .remember(Kind::Companion, subject(&account, &session, None), NONCE)
                .await,
            Err(StoreFailure::Unavailable)
        );
        assert!(store.bytes.lock().unwrap().is_none());
        *store.bytes.lock().unwrap() = Some(
            br#"{"version":"1","companion":null,"notifications":null,"native_delivery":true}"#
                .to_vec(),
        );
        assert_eq!(
            owner
                .lookup(Kind::Companion, subject(&account, &session, None))
                .await,
            Err(StoreFailure::Corrupt)
        );
    }
    #[test]
    fn endpoint_namespaces_are_separate_from_credentials_and_each_other() {
        let endpoint = CredentialKey::from_digest([1; 32]);
        let other = CredentialKey::from_digest([2; 32]);
        assert!(Registrations::key(endpoint) != endpoint);
        assert!(Registrations::key(endpoint) != Registrations::key(other));
    }
}
