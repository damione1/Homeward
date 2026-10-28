//! Actual owner/RPC/Watch qualification; requires a new isolated Go fixture.
use super::super::planner::{ActivationField, Field as PlanField, SimulationField};
use super::*;
use crate::administration::{Section, Stage};
use crate::administration_host::{Administration, State};
use crate::configuration::{SourceFamily, WorkspaceHash};
use home_client_session::{SecureStore, StoreFailure};
use home_wire_contract::home::{
    configurationplan::v1 as plan, configurationsimulation::v1 as simulation,
};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::sync::{Arc, Mutex};

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
    production_configuration: bool,
    no_physical_providers: bool,
}
fn generation(owner: &Administration) -> i32 {
    owner.0.state.lock().unwrap().generation
}
fn captured(owner: &Administration) -> Capture {
    owner
        .0
        .state
        .lock()
        .unwrap()
        .configuration
        .current
        .clone()
        .unwrap()
}
async fn until(owner: &Administration, label: &str, predicate: impl Fn(&State) -> bool) {
    tokio::time::timeout(Duration::from_secs(25), async {
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
            "configuration {label}: section={:?}, stage={:?}, requested={}, notice={}",
            state.section,
            state.stage,
            state.requested,
            state.configuration.notice()
        )
    });
}
fn root_ready(state: &State) -> bool {
    state.section == Section::Configuration
        && matches!(state.stage, Stage::Ready | Stage::Empty)
        && state
            .configuration
            .actions()
            .iter()
            .any(|action| action.action == ProAdminAction::Preview && action.enabled)
}
fn outcome(state: &State, expected: Outcome) -> bool {
    let workflow = &state.configuration;
    workflow
        .draft
        .as_ref()
        .and_then(|draft| draft.receipt.as_ref())
        .is_some_and(|receipt| receipt.outcome == expected)
        || workflow
            .planner
            .as_ref()
            .is_some_and(|planner| planner.activation_outcome() == Some(expected))
}
fn edit(owner: &Administration, field: PlanField, value: impl Into<String>) {
    owner.edit(1, generation(owner), field as i32, value.into());
}
async fn close(owner: &Administration) {
    owner.editor_action(1, generation(owner), ProAdminAction::Cancel);
    until(owner, "fresh root after editor close", root_ready).await;
}
async fn read(session: &SessionCoordinator, connection: &Connection) -> Capture {
    let cancel = CancellationToken::new();
    let lease = session.access(&cancel).await.unwrap();
    let response = connection
        .configuration()
        .read_document(
            wire::ReadDocumentRequest {
                id: wire::DocumentId {
                    value: "operational.main".into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap();
    Capture::admit(response).unwrap()
}
async fn rejected_write(
    session: &SessionCoordinator,
    connection: &Connection,
    base: &Capture,
    workspace: &WorkspaceHash,
    bytes: Vec<u8>,
    expected: wire::ErrorCode,
) {
    let cancel = CancellationToken::new();
    let lease = session.access(&cancel).await.unwrap();
    let failure = connection
        .configuration()
        .write_document(
            wire::WriteDocumentRequest {
                id: base.id().to_wire().into(),
                expected: base.sql().to_wire().into(),
                expected_workspace_sha256: workspace.bytes().to_vec(),
                exact_yaml_source: bytes,
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .err()
        .unwrap();
    assert_eq!(
        failure.configuration_error().unwrap().unwrap().code,
        expected
    );
}
#[tokio::test]
#[ignore = "explicit isolated configuration Go Run and verified TLS fixture"]
async fn actual_configuration_owner_writes_plans_simulates_applies_and_rolls_back() {
    let path = std::path::PathBuf::from(std::env::var("HOMEWARD_NHF019_TLS_FIXTURE").unwrap());
    assert!(path.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(path.join("ready.json")).unwrap()).unwrap();
    match fixture.mode {
        Mode::SyntheticOnly => (),
    }
    assert!(!fixture.production_configuration && fixture.no_physical_providers);
    let origin = url::Url::parse(&fixture.origin).unwrap();
    assert_eq!(origin.scheme(), "https");
    assert_eq!(origin.host_str(), Some("127.0.0.1"));
    assert!(
        origin.username().is_empty()
            && origin.password().is_none()
            && origin.query().is_none()
            && origin.fragment().is_none()
    );
    let source =
        Zeroizing::new(std::fs::read_to_string(path.join("supplied-source.yaml")).unwrap());
    assert!(source.len() < 1_048_576);
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes()).unwrap();
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    session
        .login(
            fixture.admin.login_name,
            Zeroizing::new(fixture.admin.password),
        )
        .await
        .unwrap();
    let owner = Administration::detached();
    let cancel = CancellationToken::new();
    let task = tokio::spawn({
        let (owner, session, connection, cancel) = (
            owner.clone(),
            session.clone(),
            connection.clone(),
            cancel.clone(),
        );
        async move { owner.run(session, connection, cancel).await }
    });
    owner.visible(true);
    until(&owner, "administrator", |state| {
        matches!(state.stage, Stage::Ready | Stage::Empty)
            && state
                .private
                .authority()
                .is_some_and(|authority| authority.administrator())
    })
    .await;
    let section = Section::ALL
        .iter()
        .position(|value| *value == Section::Configuration)
        .unwrap()
        + 1;
    owner.section(i32::try_from(section).unwrap(), generation(&owner));
    until(&owner, "configuration Ready/cursor", root_ready).await;
    let initial = captured(&owner);
    owner.action(0, generation(&owner), ProAdminAction::Save);
    assert!(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .configuration
            .draft
            .as_ref()
            .unwrap()
            .text
            .is_empty(),
        "protected metadata prefilled source"
    );
    let first = format!(
        "{}\n# NHF019 explicitly supplied first candidate\n",
        source.as_str()
    );
    owner.edit(1, generation(&owner), Field::Source as i32, first.clone());
    let old_generation = generation(&owner);
    owner.editor_action(1, old_generation, ProAdminAction::Save);
    owner.editor_action(1, old_generation, ProAdminAction::Save);
    until(&owner, "explicit source published", |state| {
        outcome(state, Outcome::Published)
    })
    .await;
    let first_capture = read(&session, &connection).await;
    assert_eq!(first_capture.sql().revision(), initial.sql().revision() + 1);
    assert_eq!(
        first_capture.sql().source().bytes().as_slice(),
        Sha256::digest(first.as_bytes()).as_slice()
    );
    let lease = session.access(&cancel).await.unwrap();
    let stale = connection
        .configuration()
        .write_document(
            wire::WriteDocumentRequest {
                id: initial.id().to_wire().into(),
                expected: initial.sql().to_wire().into(),
                expected_workspace_sha256: initial
                    .workspace()
                    .writable_hash()
                    .unwrap()
                    .bytes()
                    .to_vec(),
                exact_yaml_source: source.as_bytes().to_vec(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .err()
        .unwrap();
    assert_eq!(
        stale.configuration_error().unwrap().unwrap().code,
        wire::ErrorCode::Conflict
    );
    rejected_write(
        &session,
        &connection,
        &first_capture,
        initial.workspace().writable_hash().unwrap(),
        source.as_bytes().to_vec(),
        wire::ErrorCode::Conflict,
    )
    .await;
    rejected_write(
        &session,
        &connection,
        &first_capture,
        first_capture.workspace().writable_hash().unwrap(),
        vec![b'x'; 1_048_577],
        wire::ErrorCode::InvalidArgument,
    )
    .await;
    assert_eq!(
        read(&session, &connection).await.sql(),
        first_capture.sql(),
        "workspace conflict or oversized source changed SQL"
    );
    close(&owner).await;
    owner.action(0, generation(&owner), ProAdminAction::Preview);
    assert!(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .configuration
            .planner
            .as_ref()
            .unwrap()
            .fields(true)
            .iter()
            .find(|field| field.token == PlanField::Source as i32)
            .unwrap()
            .value
            .is_empty()
    );
    edit(&owner, PlanField::Identity, "operational.main");
    edit(&owner, PlanField::Entry, "configuration.yaml");
    edit(&owner, PlanField::Path, "configuration.yaml");
    let second = format!(
        "{}\n# NHF019 explicitly supplied apply candidate\n",
        source.as_str()
    );
    edit(&owner, PlanField::Source, second.clone());
    owner.editor_action(1, generation(&owner), ProAdminAction::Preview);
    until(&owner, "plan admitted", |state| {
        state.configuration.active.is_none()
            && state
                .configuration
                .planner
                .as_ref()
                .is_some_and(|editor| editor.plan_state() == Some(plan::PlanState::Admitted))
    })
    .await;
    assert_eq!(
        read(&session, &connection).await.sql(),
        first_capture.sql(),
        "preview mutated SQL"
    );
    owner.editor_action(1, generation(&owner), ProAdminAction::Revalidate);
    until(&owner, "plan revalidated", |state| {
        state.configuration.active.is_none()
            && state
                .configuration
                .planner
                .as_ref()
                .is_some_and(|editor| editor.plan_state() == Some(plan::PlanState::Admitted))
    })
    .await;
    owner.editor_action(1, generation(&owner), ProAdminAction::Apply);
    until(&owner, "apply published", |state| {
        outcome(state, Outcome::Published)
    })
    .await;
    let applied = read(&session, &connection).await;
    assert_eq!(applied.sql().revision(), first_capture.sql().revision() + 1);
    assert_eq!(
        applied.sql().source().bytes().as_slice(),
        Sha256::digest(second.as_bytes()).as_slice()
    );
    owner.editor_action(1, generation(&owner), ProAdminAction::Apply);
    assert_eq!(
        read(&session, &connection).await.sql(),
        applied.sql(),
        "completed apply replayed"
    );
    owner.editor_action(1, generation(&owner), ProAdminAction::Resolve);
    until(&owner, "resolved apply references", |state| {
        state.configuration.active.is_none()
            && state.configuration.synchronized
            && state
                .configuration
                .planner
                .as_ref()
                .is_some_and(|editor| editor.mutable())
    })
    .await;
    owner.editor_action(1, generation(&owner), ProAdminAction::Restore);
    until(&owner, "history loaded without selection", |state| {
        state.configuration.active.is_none()
            && state
                .configuration
                .choices()
                .iter()
                .any(|choice| choice.field_token == ActivationField::Generation as i32)
    })
    .await;
    let lease = session.access(&cancel).await.unwrap();
    let history = connection
        .configuration()
        .list_generations(
            wire::ListGenerationsRequest {
                page: wire::PageRequest {
                    page_size: 50,
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(4),
            &cancel,
        )
        .await
        .unwrap();
    let index = history
        .generations
        .iter()
        .position(|value| value.generation.revision == initial.active().revision())
        .unwrap();
    owner.choose(
        1,
        generation(&owner),
        ActivationField::Generation as i32,
        i32::try_from(index + 1).unwrap(),
    );
    owner.editor_action(1, generation(&owner), ProAdminAction::Rollback);
    until(&owner, "rollback published as new revision", |state| {
        outcome(state, Outcome::Published)
    })
    .await;
    let restored = read(&session, &connection).await;
    assert!(restored.sql().revision() > applied.sql().revision());
    assert!(restored.active().revision() > applied.active().revision());
    assert_eq!(
        restored.sql().source().bytes().as_slice(),
        Sha256::digest(source.as_bytes()).as_slice()
    );
    assert_eq!(restored.sql().source(), initial.sql().source());
    rejected_write(
        &session,
        &connection,
        &initial,
        initial.workspace().writable_hash().unwrap(),
        source.as_bytes().to_vec(),
        wire::ErrorCode::Conflict,
    )
    .await;
    assert_eq!(
        read(&session, &connection).await.sql(),
        restored.sql(),
        "same source hash bypassed the newer SQL revision"
    );
    close(&owner).await;
    owner.action(0, generation(&owner), ProAdminAction::Preview);
    owner.choose(
        1,
        generation(&owner),
        PlanField::Family as i32,
        SourceFamily::Script.to_wire() as i32,
    );
    edit(&owner, PlanField::Identity, "script.simulation");
    edit(&owner, PlanField::Entry, "logic.yaml");
    edit(&owner, PlanField::Path, "logic.yaml");
    edit(
        &owner,
        PlanField::Source,
        "sequence:\n- action: light.turn_on\n  target: {entity_id: light.fixture}\n  data: {brightness_pct: 25}\n",
    );
    owner.edit(1,generation(&owner),SimulationField::Fixtures as i32,r#"[{"alias":"light.fixture","state":"off","supports":["light_turn_on","light_brightness_absolute"]}]"#.into());
    owner.editor_action(1, generation(&owner), ProAdminAction::Simulate);
    until(&owner, "synthetic simulation report", |state| {
        state.configuration.active.is_none()
            && state
                .configuration
                .planner
                .as_ref()
                .is_some_and(|editor| editor.simulation_report().is_some())
    })
    .await;
    {
        let state = owner.0.state.lock().unwrap();
        let report = state
            .configuration
            .planner
            .as_ref()
            .unwrap()
            .simulation_report()
            .unwrap();
        assert!(report.simulated && !report.hardware_validated);
        assert_eq!(report.status, simulation::Status::Completed);
        assert_eq!(report.intents.len(), 1);
        assert_eq!(report.intents[0].alias, "light.fixture");
        let Some(simulation::intent::Parameters::Light(value)) = &report.intents[0].parameters
        else {
            panic!("synthetic light intent")
        };
        assert_eq!(value.brightness_percent, Some(25.));
    }
    assert_eq!(
        read(&session, &connection).await.sql(),
        restored.sql(),
        "simulation mutated configuration"
    );
    owner.foreground(false);
    assert!(!owner.0.state.lock().unwrap().configuration.opened());
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(15), task)
        .await
        .unwrap()
        .unwrap();
    session.logout().await.unwrap();
    println!("NHF019_CONFIGURATION_OWNER_TLS_PASS");
}
