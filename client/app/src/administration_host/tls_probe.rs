//! Actual settings owner over the isolated default Go Run and verified TLS.
use super::*;
use home_client_session::{SecureStore, StoreFailure};
use home_wire_contract::home::auth::v1 as auth;
use serde::Deserialize;
use std::{path::PathBuf, time::Duration};
use zeroize::Zeroizing;
mod advanced;
mod observation;
mod security;

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
    fault_origin: String,
    native_invitation: InvitationFixture,
    ca_pem: String,
    admin: Credentials,
    ordinary: Credentials,
    keeper: Credentials,
    factor: MfaCredentials,
    witness: MfaCredentials,
    production_configuration: bool,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MfaCredentials {
    login_name: String,
    password: String,
    recovery_codes: Vec<String>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct InvitationFixture {
    login_name: String,
    token: String,
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
            "administration {label}: section={:?} stage={:?} active={} rows={} mutation={:?} secret_present={}",
            state.section,
            state.stage,
            state.active,
            state.rows.len(),
            state.private.draft().map(|draft|draft.state),
            state.private.secret().is_some(),
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
fn managed(state: &State) -> Option<&auth::Account> {
    let Payload::Accounts(accounts) = state.private.pane(Section::Accounts)?.content.as_ref()?
    else {
        return None;
    };
    accounts
        .iter()
        .find(|account| account.login_name == "NHF018 managed")
}
fn select_managed(owner: &Administration) {
    let token = {
        let state = owner.0.state.lock().unwrap();
        let account = managed(&state).unwrap();
        state.rows.iter().find(|row|matches!(&row.target,render::Target::Account(id) if id.as_str()==account.id.value)).unwrap().token
    };
    owner.select(token, generation(owner));
}
async fn account_forms(owner: &Administration) {
    section(owner, Section::Accounts).await;
    owner.action(0, generation(owner), ProAdminAction::Create);
    // Empty local input never queues a mutation, and remains editable.
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    {
        let state = owner.0.state.lock().unwrap();
        assert!(state.pending.is_none());
        assert!(state.private.draft().unwrap().editable());
    }
    for (field, value) in [
        (forms::Field::Login, "NHF018 managed"),
        (forms::Field::Name, "Before edit"),
        (forms::Field::Password, "NHF018 isolated managed password"),
    ] {
        owner.edit(1, generation(owner), field as i32, value.into());
    }
    let stale = generation(owner);
    owner.editor_action(1, stale, ProAdminAction::Save);
    let submitted = generation(owner);
    owner.editor_action(1, stale, ProAdminAction::Save);
    assert_eq!(
        generation(owner),
        submitted,
        "a captured second confirmation cannot submit another write"
    );
    until(owner, "account created through editor", |state| {
        ready(state) && state.private.draft().is_none() && managed(state).is_some()
    })
    .await;
    select_managed(owner);
    owner.action(0, generation(owner), ProAdminAction::Save);
    owner.edit(
        1,
        generation(owner),
        forms::Field::Name as i32,
        "After edit".into(),
    );
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(owner, "account updated through editor", |state| {
        ready(state)
            && state.private.draft().is_none()
            && managed(state).is_some_and(|account| account.display_name == "After edit")
    })
    .await;
    select_managed(owner);
    owner.action(0, generation(owner), ProAdminAction::Remove);
    assert!(
        owner.0.state.lock().unwrap().pending.is_none(),
        "opening deletion is not consent to delete"
    );
    owner.editor_action(1, generation(owner), ProAdminAction::Cancel);
    until(owner, "cancelled deletion keeps the account", |state| {
        ready(state) && state.private.draft().is_none() && managed(state).is_some()
    })
    .await;
    select_managed(owner);
    owner.action(0, generation(owner), ProAdminAction::Remove);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(
        owner,
        "account deleted after explicit confirmation",
        |state| ready(state) && state.private.draft().is_none() && managed(state).is_none(),
    )
    .await;
}
async fn create_invitation(owner: &Administration, login: &str) -> Zeroizing<String> {
    section(owner, Section::Invitations).await;
    owner.action(0, generation(owner), ProAdminAction::Create);
    owner.edit(
        1,
        generation(owner),
        forms::Field::Login as i32,
        login.into(),
    );
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(owner, "one-time invitation secret", |state| {
        state.private.secret_kind() == Some(crate::administration::SecretKind::Invitation)
            && state.private.secret().is_some()
    })
    .await;
    let secret = Zeroizing::new(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .private
            .secret()
            .unwrap()
            .to_owned(),
    );
    owner.editor_action(1, generation(owner), ProAdminAction::CloseSecret);
    assert!(owner.0.state.lock().unwrap().private.secret().is_none());
    until(owner, "invitation metadata after secret closure", ready).await;
    secret
}
async fn invitation_forms(owner: &Administration, connection: &Connection) {
    let cancel = CancellationToken::new();
    let token = create_invitation(owner, "NHF018 invited").await;
    let outcome = crate::invitation::accept(
        &connection.accounts(),
        crate::invitation::Request {
            token,
            password: Zeroizing::new("NHF018 invited synthetic password".into()),
            display_name: "Invited person".into(),
        },
        &cancel,
    )
    .await;
    match outcome {
        crate::invitation::Outcome::Accepted { login } => assert_eq!(login, "NHF018 invited"),
        crate::invitation::Outcome::Rejected
        | crate::invitation::Outcome::Uncertain
        | crate::invitation::Outcome::Cancelled => panic!("anonymous invitation acceptance failed"),
    }
    let invited = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    login(
        &invited,
        &Credentials {
            login_name: "NHF018 invited".into(),
            password: "NHF018 invited synthetic password".into(),
        },
    )
    .await;
    invited.logout().await.unwrap();
    let token = create_invitation(owner, "NHF018 revoked invitation").await;
    let row_token = {
        let state = owner.0.state.lock().unwrap();
        let Some(Payload::Invitations(rows)) = state
            .private
            .pane(Section::Invitations)
            .unwrap()
            .content
            .as_ref()
        else {
            unreachable!()
        };
        let id = &rows
            .iter()
            .find(|row| row.login_name == "NHF018 revoked invitation")
            .unwrap()
            .id
            .value;
        state
            .rows
            .iter()
            .find(
                |row| matches!(&row.target,render::Target::Invitation(value) if value.as_str()==id),
            )
            .unwrap()
            .token
    };
    owner.select(row_token, generation(owner));
    owner.action(0, generation(owner), ProAdminAction::Revoke);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(owner,"invitation revoked after confirmation",|state|ready(state)&&state.private.draft().is_none()&&state.private.pane(Section::Invitations).and_then(|pane|pane.content.as_ref()).is_some_and(|payload|matches!(payload,Payload::Invitations(rows) if rows.iter().any(|row|row.login_name=="NHF018 revoked invitation"&&row.revoked_at.is_set())))).await;
    assert!(
        connection
            .accounts()
            .accept_invitation(
                auth::AcceptInvitationRequest {
                    invitation_token: token.to_string(),
                    password: "NHF018 revoked synthetic password".into(),
                    display_name: "Revoked person".into(),
                    ..Default::default()
                },
                Duration::from_secs(4),
                &cancel
            )
            .await
            .is_err()
    );
}
async fn mcp_forms(owner: &Administration, connection: &Connection, session: &SessionCoordinator) {
    section(owner, Section::McpKeys).await;
    let cancel = CancellationToken::new();
    let native = session.access(&cancel).await.unwrap();
    owner.action(0, generation(owner), ProAdminAction::Create);
    owner.edit(
        1,
        generation(owner),
        forms::Field::ClientName as i32,
        "NHF018 synthetic MCP client".into(),
    );
    owner.choose(1, generation(owner), forms::ChoiceField::Scope as i32, 1);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(
        owner,
        "equipment scope refused without an object grant",
        |state| {
            state
                .private
                .draft()
                .is_some_and(|draft| draft.state == crate::administration::MutationState::Rejected)
                && state.private.secret().is_none()
        },
    )
    .await;
    owner.choose(1, generation(owner), forms::ChoiceField::Scope as i32, 1);
    let permitted = forms::scopes::ScopeChoice::all()
        .find(|(_, scope)| {
            scope.family == auth::ScopeFamily::Integrations
                && scope.operation == auth::ScopeOperation::Read
        })
        .unwrap()
        .0;
    owner.choose(
        1,
        generation(owner),
        forms::ChoiceField::Scope as i32,
        permitted,
    );
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(owner, "one-time MCP secret", |state| {
        state.private.secret_kind() == Some(crate::administration::SecretKind::McpKey)
            && state.private.secret().is_some()
    })
    .await;
    let secret = Zeroizing::new(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .private
            .secret()
            .unwrap()
            .to_owned(),
    );
    // A bearer for MCP is never a native interactive session.
    let bearer = home_client_transport::AccessToken::parse(&secret).unwrap();
    assert!(
        connection
            .auth()
            .list_sessions(
                auth::ListSessionsRequest {
                    account_id: auth::AccountId {
                        value: native.account_id().as_str().into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                },
                &bearer,
                Duration::from_secs(4),
                &cancel
            )
            .await
            .is_err()
    );
    owner.editor_action(1, generation(owner), ProAdminAction::CloseSecret);
    assert!(owner.0.state.lock().unwrap().private.secret().is_none());
    until(owner, "MCP metadata never rereads its secret", ready).await;
    let row_token = {
        let state = owner.0.state.lock().unwrap();
        let Some(Payload::Keys(rows)) = state
            .private
            .pane(Section::McpKeys)
            .unwrap()
            .content
            .as_ref()
        else {
            unreachable!()
        };
        let key = rows
            .iter()
            .find(|key| key.client_name == "NHF018 synthetic MCP client")
            .unwrap();
        assert_eq!(key.audience, auth::CredentialAudience::Mcp);
        assert_eq!(key.scopes.len(), 1);
        state
            .rows
            .iter()
            .find(|row| matches!(&row.target,render::Target::Key(id) if id.as_str()==key.id.value))
            .unwrap()
            .token
    };
    owner.select(row_token, generation(owner));
    owner.action(0, generation(owner), ProAdminAction::Revoke);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(owner,"MCP key revoked without revoking native session",|state|ready(state)&&state.private.draft().is_none()&&state.private.pane(Section::McpKeys).and_then(|pane|pane.content.as_ref()).is_some_and(|payload|matches!(payload,Payload::Keys(rows) if rows.iter().any(|key|key.client_name=="NHF018 synthetic MCP client"&&key.revoked_at.is_set())))).await;
    section(owner, Section::Sessions).await;
    let current = session.access(&cancel).await.unwrap();
    assert_eq!(current.session_id(), native.session_id());
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
    assert_eq!(
        fixture.native_invitation.login_name,
        "NHF018 native invited"
    );
    assert!(!fixture.native_invitation.token.is_empty());
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
    account_forms(&owner).await;
    invitation_forms(&owner, &connection).await;
    mcp_forms(&owner, &connection, &session).await;
    security::qualify(&owner, &connection, &session, &fixture).await;
    advanced::qualify(&owner, &connection, &session).await;
    observation::qualify(&owner, &connection, &session, &fixture, &root).await;
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
    assert!(
        forms::available(&owner.0.state.lock().unwrap())
            .iter()
            .all(|command| !command.section().administrative()),
        "ordinary account has no administrative mutation available"
    );
    owner.action(0, generation(&owner), ProAdminAction::Create);
    assert!(
        owner.0.state.lock().unwrap().private.draft().is_some(),
        "personal MCP creation remains available to an ordinary native session"
    );
    owner.editor_action(1, generation(&owner), ProAdminAction::Cancel);
    until(&owner, "ordinary personal form cancellation", ready).await;
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
    // Retiring authority must also discard an open protected draft.
    owner.action(0, generation(&owner), ProAdminAction::Create);
    owner.edit(
        1,
        generation(&owner),
        forms::Field::Login as i32,
        "NHF018 draft to discard".into(),
    );
    owner.foreground(false);
    assert!(owner.0.state.lock().unwrap().private.draft().is_none());
    owner.foreground(true);
    until(&owner, "foreground after retiring account draft", ready).await;
    section(&owner, Section::Invitations).await;
    owner.action(0, generation(&owner), ProAdminAction::Create);
    owner.edit(
        1,
        generation(&owner),
        forms::Field::Login as i32,
        "NHF018 secret to discard".into(),
    );
    owner.editor_action(1, generation(&owner), ProAdminAction::Save);
    until(
        &owner,
        "protected one-time secret before role withdrawal",
        |state| state.private.secret().is_some(),
    )
    .await;
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
                && state.private.pane(Section::Invitations).is_none()
                && state.private.draft().is_none()
                && state.private.secret().is_none()
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
