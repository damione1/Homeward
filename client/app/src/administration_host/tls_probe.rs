//! Actual settings owner over the isolated default Go Run and verified TLS.
use super::*;
use home_client_session::{SecureStore, StoreFailure};
use home_wire_contract::home::auth::v1 as auth;
use serde::Deserialize;
use std::{path::PathBuf, time::Duration};
use zeroize::Zeroizing;

#[derive(Default)]
struct MemoryStore(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for MemoryStore {
    fn load(&self) -> Result<Option<Zeroizing<Vec<u8>>>, StoreFailure> {
        Ok(self.0.lock().unwrap().clone())
    }
    fn save(&self, value: &[u8]) -> Result<(), StoreFailure> {
        *self.0.lock().unwrap() = Some(Zeroizing::new(value.to_vec()));
        Ok(())
    }
    fn clear(&self) -> Result<(), StoreFailure> {
        *self.0.lock().unwrap() = None;
        Ok(())
    }
}
#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Credentials {
    login_name: String,
    password: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    admin: Credentials,
    ordinary: Credentials,
    keeper: Credentials,
    production_configuration: bool,
}
async fn login(session: &SessionCoordinator, credentials: &Credentials) {
    session
        .login(
            credentials.login_name.clone(),
            Zeroizing::new(credentials.password.clone()),
        )
        .await
        .expect("synthetic native login");
}
async fn until(owner: &Administration, label: &str, predicate: impl Fn(&State) -> bool) {
    tokio::time::timeout(Duration::from_secs(15), async {
        loop {
            if predicate(&owner.0.state.lock().unwrap()) {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap_or_else(|_| {
        let state = owner.0.state.lock().unwrap();
        panic!(
            "administration {label}: section={:?} stage={:?} active={} rows={}",
            state.section,
            state.stage,
            state.active,
            state.rows.len()
        );
    });
}
fn ready(state: &State) -> bool {
    matches!(state.stage, Stage::Ready | Stage::Empty)
        && state.private.pane(state.section).is_some()
}
fn generation(owner: &Administration) -> i32 {
    owner.0.state.lock().unwrap().generation
}
async fn section(owner: &Administration, section: Section) {
    let token = Section::ALL
        .iter()
        .position(|value| *value == section)
        .unwrap()
        + 1;
    owner.section(i32::try_from(token).unwrap(), generation(owner));
    until(owner, section.label(), |state| {
        state.section == section && ready(state)
    })
    .await;
}

#[tokio::test]
#[ignore = "explicit isolated actual Run/TLS administration fixture"]
async fn actual_administration_owner_reads_personal_access_and_withdrawal() {
    let root = PathBuf::from(std::env::var("HOMEWARD_NHF018_TLS_FIXTURE").unwrap());
    assert!(root.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    match fixture.mode {
        Mode::SyntheticOnly => {}
    }
    assert!(!fixture.production_configuration);
    let origin = url::Url::parse(&fixture.origin).unwrap();
    assert_eq!(origin.scheme(), "https");
    assert_eq!(origin.host_str(), Some("127.0.0.1"));
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes()).unwrap();
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    login(&session, &fixture.admin).await;
    let owner = Administration::detached();
    let cancel = CancellationToken::new();
    let (run, source, api, token) = (
        owner.clone(),
        session.clone(),
        connection.clone(),
        cancel.clone(),
    );
    let task = tokio::spawn(async move { run.run(source, api, token).await });
    owner.visible(true);
    until(&owner, "initial administrator", |state| {
        ready(state)
            && state
                .private
                .authority()
                .is_some_and(|value| value.administrator())
    })
    .await;
    let admin_identity = owner
        .0
        .state
        .lock()
        .unwrap()
        .private
        .authority()
        .unwrap()
        .identity
        .clone();
    for value in Section::ALL {
        section(&owner, value).await;
    }
    section(&owner, Section::Integrations).await;
    let token = {
        let state = owner.0.state.lock().unwrap();
        let Some(Payload::Integrations(value)) =
            &state.private.pane(Section::Integrations).unwrap().content
        else {
            panic!("integration catalogue")
        };
        assert_eq!(value.types.len(), 8);
        assert_eq!(value.response.instances.len(), 1);
        state.rows[0].token
    };
    owner.select(token, generation(&owner));
    until(&owner, "exact integration detail", |state| {
        ready(state)
            && matches!(
                state
                    .private
                    .pane(Section::Integrations)
                    .and_then(|pane| pane.content.as_ref()),
                Some(Payload::Integration(_))
            )
    })
    .await;
    {
        let state = owner.0.state.lock().unwrap();
        let Some(Payload::Integration(value)) = state
            .private
            .pane(Section::Integrations)
            .unwrap()
            .content
            .as_ref()
        else {
            unreachable!()
        };
        assert!(value.kind.is_ok());
        assert!(value.health.is_ok());
    }
    section(&owner, Section::Policies).await;
    let account_token = {
        let state = owner.0.state.lock().unwrap();
        state
            .rows
            .iter()
            .find(|row| row.target == render::Target::Account(admin_identity.account.clone()))
            .unwrap()
            .token
    };
    owner.select(account_token, generation(&owner));
    until(&owner, "exact policy detail", |state| {
        ready(state)
            && matches!(
                state
                    .private
                    .pane(Section::Policies)
                    .and_then(|pane| pane.content.as_ref()),
                Some(Payload::Policy { .. })
            )
    })
    .await;
    let stale = generation(&owner);
    owner.foreground(false);
    assert!(owner.0.state.lock().unwrap().private.authority().is_none());
    owner.select(account_token, stale);
    assert!(owner.0.state.lock().unwrap().selected.is_none());
    owner.foreground(true);
    until(&owner, "fresh foreground", ready).await;
    session.logout().await.unwrap();
    until(&owner, "logout purges protected data", |state| {
        !state.active && state.private.authority().is_none()
    })
    .await;
    login(&session, &fixture.ordinary).await;
    until(
        &owner,
        "ordinary personal settings without GetAccount permission",
        ready,
    )
    .await;
    {
        let state = owner.0.state.lock().unwrap();
        let authority = state.private.authority().unwrap();
        assert_ne!(authority.identity.account, admin_identity.account);
        assert!(authority.account().is_none());
        assert!(!authority.administrator());
    }
    for value in [Section::Sessions, Section::Factors, Section::McpKeys] {
        section(&owner, value).await;
    }
    let before = generation(&owner);
    owner.section(
        Section::ALL
            .iter()
            .position(|value| *value == Section::Accounts)
            .unwrap() as i32
            + 1,
        before,
    );
    assert_eq!(
        generation(&owner),
        before,
        "ordinary user cannot enqueue an admin view"
    );
    session.logout().await.unwrap();
    login(&session, &fixture.admin).await;
    until(&owner, "administrator session replacement", |state| {
        ready(state)
            && state
                .private
                .authority()
                .is_some_and(|value| value.administrator())
    })
    .await;
    section(&owner, Section::Accounts).await;
    let stale = generation(&owner);
    let keeper = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    login(&keeper, &fixture.keeper).await;
    let lease = keeper.access(&cancel).await.unwrap();
    connection
        .accounts()
        .update_account(
            auth::UpdateAccountRequest {
                account_id: auth::AccountId {
                    value: admin_identity.account.as_str().into(),
                    ..Default::default()
                }
                .into(),
                role: Some(auth::AccountRole::User.into()),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .expect("isolated administrator demotion");
    until(
        &owner,
        "periodic authority withdrawal without a dashboard watch",
        |state| {
            !state
                .private
                .authority()
                .is_some_and(|value| value.administrator())
                && state.private.pane(Section::Accounts).is_none()
        },
    )
    .await;
    owner.select(1, stale);
    assert!(owner.0.state.lock().unwrap().selected.is_none());
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(5), task)
        .await
        .unwrap()
        .unwrap();
    assert!(owner.0.state.lock().unwrap().private.authority().is_none());
    println!("NHF018_ADMINISTRATION_OWNER_TLS_PASS");
}
