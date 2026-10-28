//! The actual administration owner on a fresh Go Run/PostgreSQL/TLS fixture.
use super::*;
use crate::administration::{Section, Stage as ScreenStage};
use crate::administration_host::{
    Administration, State, read::Payload as AdminPayload, render::Target,
};
use home_client_session::{SecureStore, StoreFailure};
use serde::Deserialize;
use std::{
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};
use zeroize::Zeroizing;

#[derive(Default)]
struct Store(Mutex<Option<Zeroizing<Vec<u8>>>>);
impl SecureStore for Store {
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
    production_configuration: bool,
    no_physical_providers: bool,
}
fn generation(owner: &Administration) -> i32 {
    owner.0.state.lock().unwrap().generation
}
fn payload(state: &State) -> Option<&Payload> {
    if let Some(AdminPayload::Logic(value)) = state.private.pane(Section::Logic)?.content.as_ref() {
        Some(value)
    } else {
        None
    }
}
async fn until(owner: &Administration, label: &str, predicate: impl Fn(&State) -> bool) {
    tokio::time::timeout(Duration::from_secs(30), async {
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
            "logic {label}: section={:?},stage={:?},notice={}",
            state.section,
            state.stage,
            state.logic.notice()
        );
    });
}
async fn catalogue(owner: &Administration) -> Catalogue {
    let token = Section::ALL
        .iter()
        .position(|value| *value == Section::Logic)
        .unwrap()
        + 1;
    owner.section(token as i32, generation(owner));
    until(owner, "catalogue", |state| {
        state.stage == ScreenStage::Ready && matches!(payload(state), Some(Payload::Catalogue(_)))
    })
    .await;
    let state = owner.0.state.lock().unwrap();
    let Some(Payload::Catalogue(value)) = payload(&state) else {
        panic!("catalogue absent")
    };
    value.clone()
}
async fn select(owner: &Administration, expected: Selection) {
    let token = {
        let state = owner.0.state.lock().unwrap();
        state
            .rows
            .iter()
            .find(|row| row.target == Target::Logic(expected.clone()))
            .unwrap()
            .token
    };
    owner.select(token, generation(owner));
    until(owner, "selected data", |state| {
        state.stage == ScreenStage::Ready
            && match (payload(state), &expected) {
                (
                    Some(Payload::Definition { definition, .. }),
                    Selection::Definition { id, .. },
                ) => definition.reference.id == id.as_str(),
                (Some(Payload::Run { run, .. }), Selection::Run { id, .. }) => {
                    run.id == id.as_str()
                }
                (Some(Payload::Trace { run, .. }), Selection::Trace { id, .. }) => {
                    run.id == id.as_str()
                }
                (
                    Some(
                        Payload::Catalogue(_)
                        | Payload::Definition { .. }
                        | Payload::Run { .. }
                        | Payload::Trace { .. },
                    )
                    | None,
                    Selection::Catalogue
                    | Selection::Definition { .. }
                    | Selection::Run { .. }
                    | Selection::Trace { .. },
                ) => false,
            }
    })
    .await;
}
async fn definition(owner: &Administration, mode: wire::RunMode) -> (Context, DefinitionId) {
    let catalogue = catalogue(owner).await;
    let value = catalogue
        .definitions
        .iter()
        .find(|value| value.mode == mode && value.can_run)
        .unwrap();
    let id = DefinitionId::parse(&value.reference.id).unwrap();
    select(
        owner,
        Selection::Definition {
            context: catalogue.context.clone(),
            id: id.clone(),
        },
    )
    .await;
    (catalogue.context, id)
}
fn draft_attempt(owner: &Administration) -> RequestId {
    let state = owner.0.state.lock().unwrap();
    match &state.logic.draft.as_ref().unwrap().operation {
        Operation::Start { request, .. } | Operation::Cancel { request, .. } => request.clone(),
    }
}
async fn close(owner: &Administration) {
    owner.editor_action(1, generation(owner), ProAdminAction::Cancel);
    until(owner, "editor closed and data reread", |state| {
        !state.logic.opened() && state.stage == ScreenStage::Ready && payload(state).is_some()
    })
    .await;
}
async fn start(owner: &Administration, uncertain: bool) -> RequestId {
    owner.action(0, generation(owner), ProAdminAction::Create);
    assert!(owner.0.state.lock().unwrap().logic.opened());
    let request = draft_attempt(owner);
    let captured = generation(owner);
    owner.editor_action(1, captured, ProAdminAction::Save);
    // Both a queued duplicate and a later callback with the new UI generation
    // must fail to create another attempt while this one is sealed.
    owner.editor_action(1, captured, ProAdminAction::Save);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    let expected = if uncertain {
        Stage::Uncertain
    } else {
        Stage::Resolved
    };
    until(owner, "start completion", |state| {
        state
            .logic
            .draft
            .as_ref()
            .is_some_and(|value| value.stage == expected)
    })
    .await;
    if !uncertain {
        close(owner).await;
    }
    request
}
fn listed(owner: &Administration, attempt: &RequestId) -> wire::Run {
    let state = owner.0.state.lock().unwrap();
    let Some(Payload::Definition { runs, .. }) = payload(&state) else {
        panic!("runs absent")
    };
    let matches = runs
        .iter()
        .filter(|value| value.start_request_id == attempt.as_str())
        .collect::<Vec<_>>();
    assert_eq!(matches.len(), 1);
    matches[0].clone()
}
async fn inspect(owner: &Administration, context: &Context, run: &wire::Run) {
    select(
        owner,
        Selection::Run {
            context: context.clone(),
            id: RunId::parse(&run.id).unwrap(),
        },
    )
    .await;
    select(
        owner,
        Selection::Trace {
            context: context.clone(),
            id: RunId::parse(&run.id).unwrap(),
        },
    )
    .await;
    let state = owner.0.state.lock().unwrap();
    let Some(Payload::Trace {
        rows,
        run: captured,
        ..
    }) = payload(&state)
    else {
        panic!("trace absent")
    };
    assert_eq!(rows.len(), 480);
    assert_eq!(captured.generation, run.generation);
    assert_eq!(captured.actor, run.actor);
    assert!(rows.iter().any(
        |row| matches!(&row.event,Some(wire::trace_row::Event::Gap(value)) if value.count_unknown)
    ));
    assert!(rows.iter().any(|row|matches!(&row.event,Some(wire::trace_row::Event::Gap(value)) if value.known_missing_count == 7)));
    assert!(rows.iter().any(|row|matches!(&row.event,Some(wire::trace_row::Event::Command(value)) if value.outcome == home_wire_contract::home::history::v1::CommandOutcome::Dispatched)));
    assert!(
        state
            .rows
            .iter()
            .any(|row| row.detail.contains("Source expurgée"))
    );
    assert!(
        state
            .rows
            .iter()
            .any(|row| row.title.contains("Événements expurgés"))
    );
    assert!(
        state
            .rows
            .iter()
            .any(|row| row.detail.contains("Acteur non enregistré"))
    );
}
async fn marker(root: &Path, name: &str) {
    tokio::time::timeout(Duration::from_secs(10), async {
        while !root.join(name).is_file() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
}

#[tokio::test]
#[ignore = "explicit isolated actual Go Run/TLS logic fixture"]
async fn actual_logic_owner_modes_gaps_lost_reply_restart_and_withdrawal() {
    let root = std::path::PathBuf::from(std::env::var("HOMEWARD_NHF020_TLS_FIXTURE").unwrap());
    assert!(root.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    match fixture.mode {
        Mode::SyntheticOnly => {}
    }
    assert!(!fixture.production_configuration && fixture.no_physical_providers);
    let url = url::Url::parse(&fixture.origin).unwrap();
    assert_eq!(url.scheme(), "https");
    assert_eq!(url.host_str(), Some("127.0.0.1"));
    assert!(url.username().is_empty() && url.password().is_none());
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes()).unwrap();
    let session = SessionCoordinator::new(connection.auth(), Arc::new(Store::default()));
    session
        .login(
            fixture.admin.login_name.clone(),
            Zeroizing::new(fixture.admin.password.clone()),
        )
        .await
        .unwrap();
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
        state.stage == ScreenStage::Ready
            && state
                .private
                .authority()
                .is_some_and(|value| value.administrator())
    })
    .await;
    let initial = catalogue(&owner).await;
    assert_eq!(
        (
            initial.retained,
            initial.reported,
            initial.definitions.len()
        ),
        (17, 72, 17)
    );
    assert!(
        initial
            .definitions
            .iter()
            .any(|value| value.compatibility == wire::Compatibility::Unsupported && !value.can_run)
    );

    for mode in [
        wire::RunMode::Single,
        wire::RunMode::Restart,
        wire::RunMode::Queued,
        wire::RunMode::Parallel,
    ] {
        let (context, id) = definition(&owner, mode).await;
        let first = start(&owner, false).await;
        let second = start(&owner, false).await;
        let run1 = listed(&owner, &first);
        let run2 = listed(&owner, &second);
        assert_ne!(run1.id, run2.id);
        match mode {
            wire::RunMode::Single => {
                assert_eq!(run1.status, wire::RunStatus::Running);
                assert_eq!(run2.status, wire::RunStatus::Failed);
                assert_eq!(run2.reason, wire::RunReason::SingleBusy);
            }
            wire::RunMode::Restart => {
                assert_eq!(run1.status, wire::RunStatus::Canceled);
                assert_eq!(run1.reason, wire::RunReason::RestartReplaced);
                assert_eq!(run2.status, wire::RunStatus::Running);
            }
            wire::RunMode::Queued => {
                assert_eq!(run1.status, wire::RunStatus::Running);
                assert_eq!(run2.status, wire::RunStatus::Queued);
            }
            wire::RunMode::Parallel => {
                assert_eq!(run1.status, wire::RunStatus::Running);
                assert_eq!(run2.status, wire::RunStatus::Running);
            }
            wire::RunMode::Unspecified => panic!("invalid fixture mode"),
        }
        if matches!(mode, wire::RunMode::Single | wire::RunMode::Queued) {
            select(
                &owner,
                Selection::Run {
                    context: context.clone(),
                    id: RunId::parse(&run1.id).unwrap(),
                },
            )
            .await;
            owner.action(0, generation(&owner), ProAdminAction::Remove);
            assert!(owner.0.state.lock().unwrap().logic.opened());
            owner.editor_action(1, generation(&owner), ProAdminAction::Save);
            until(&owner, "cancel completion", |state| {
                state
                    .logic
                    .draft
                    .as_ref()
                    .is_some_and(|value| value.stage == Stage::Resolved)
            })
            .await;
            assert!(
                owner
                    .0
                    .state
                    .lock()
                    .unwrap()
                    .logic
                    .notice()
                    .contains("ne sont pas inversés")
            );
            close(&owner).await;
            // Return through the catalogue to reread server-owned run state.
            catalogue(&owner).await;
            select(
                &owner,
                Selection::Definition {
                    context: context.clone(),
                    id,
                },
            )
            .await;
            let refreshed = listed(&owner, &first);
            assert_eq!(refreshed.status, wire::RunStatus::Canceled);
            if mode == wire::RunMode::Queued {
                assert_eq!(listed(&owner, &second).status, wire::RunStatus::Running);
            }
            inspect(&owner, &context, &refreshed).await;
        }
    }
    let (context, _) = definition(&owner, wire::RunMode::Parallel).await;
    std::fs::write(root.join("lose-start-reply"), b"1").unwrap();
    let lost = start(&owner, true).await;
    std::fs::remove_file(root.join("lose-start-reply")).unwrap();
    owner.editor_action(1, generation(&owner), ProAdminAction::Save);
    assert!(owner.0.state.lock().unwrap().logic.pending.is_none());
    owner.editor_action(1, generation(&owner), ProAdminAction::Resolve);
    until(&owner, "lost reply resolved without resend", |state| {
        state
            .logic
            .draft
            .as_ref()
            .is_some_and(|value| value.stage == Stage::Resolved)
    })
    .await;
    close(&owner).await;
    let recovered = listed(&owner, &lost);
    assert_eq!(recovered.status, wire::RunStatus::Running);
    inspect(&owner, &context, &recovered).await;
    owner.foreground(false);
    {
        let state = owner.0.state.lock().unwrap();
        assert!(
            state.rows.is_empty() && state.logic.current.is_none() && state.logic.draft.is_none()
        );
    }
    owner.foreground(true);
    until(&owner, "foreground authority", |state| {
        state.stage == ScreenStage::Ready && state.private.authority().is_some()
    })
    .await;
    catalogue(&owner).await;

    std::fs::write(root.join("restart"), b"1").unwrap();
    marker(&root, "restarted.json").await;
    let after = catalogue(&owner).await;
    assert_ne!(initial.context.epoch, after.context.epoch);
    let lease = session.access(&cancel).await.unwrap();
    let missing = connection
        .logic()
        .get_run(
            wire::GetRunRequest {
                context: after.context.to_wire().into(),
                selector: Some(wire::get_run_request::Selector::StartRequestId(
                    lost.as_str().into(),
                )),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap_err();
    assert_eq!(missing.kind(), FailureKind::Rpc(RpcCode::NotFound));
    let expired = connection
        .logic()
        .get_run(
            wire::GetRunRequest {
                context: context.to_wire().into(),
                selector: Some(wire::get_run_request::Selector::RunId(recovered.id)),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap_err();
    assert_eq!(
        expired.kind(),
        FailureKind::Rpc(RpcCode::FailedPrecondition)
    );
    std::fs::write(root.join("revoke-admin"), b"1").unwrap();
    marker(&root, "admin-revoked.json").await;
    until(&owner, "withdrawal purges private logic", |state| {
        state.private.pane(Section::Logic).is_none()
            && state.logic.current.is_none()
            && state.logic.draft.is_none()
    })
    .await;
    let refused = connection
        .logic()
        .list_definitions(
            wire::ListDefinitionsRequest {
                page_size: 10,
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap_err();
    assert!(matches!(
        refused.kind(),
        FailureKind::Rpc(RpcCode::PermissionDenied | RpcCode::Unauthenticated)
    ));
    session
        .login(
            fixture.admin.login_name,
            Zeroizing::new(fixture.admin.password),
        )
        .await
        .unwrap();
    let ordinary = session.access(&cancel).await.unwrap();
    let denied = connection
        .logic()
        .list_definitions(
            wire::ListDefinitionsRequest {
                page_size: 10,
                ..Default::default()
            },
            ordinary.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap_err();
    assert_eq!(denied.kind(), FailureKind::Rpc(RpcCode::PermissionDenied));
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(5), task)
        .await
        .unwrap()
        .unwrap();
    println!("NHF020_LOGIC_OWNER_TLS_PASS");
}
