//! Explicit synthetic faults; injected outcomes never forward a mutation.
use super::*;
use std::path::Path;

#[derive(Clone, Copy)]
enum Fault {
    Normal,
    LoseCommitted,
    Unknown,
    Reconciling,
    Restart,
    Held,
}
impl Fault {
    fn set(self, path: &Path) {
        let value = match self {
            Self::Normal => "normal",
            Self::LoseCommitted => "lose-committed-response",
            Self::Unknown => "commit-unknown",
            Self::Reconciling => "committed-reconciling",
            Self::Restart => "restart-required",
            Self::Held => "held",
        };
        std::fs::write(path.join("fault.pending"), value).unwrap();
        std::fs::rename(path.join("fault.pending"), path.join("fault-mode")).unwrap();
    }
}
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Counts {
    received: u64,
    forwarded: u64,
    injected: u64,
    lost: u64,
    canceled: u64,
    active: u64,
}
fn counts(path: &Path) -> Counts {
    serde_json::from_slice(&std::fs::read(path.join("counts.json")).unwrap()).unwrap()
}
async fn idle(path: &Path) -> Counts {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            let current = counts(path);
            if current.active == 0 {
                return current;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("synthetic mutation did not retire")
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Held {
    sequence: u64,
}
async fn held(path: &Path, sequence: u64) {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Ok(bytes) = std::fs::read(path.join("held.json"))
                && serde_json::from_slice::<Held>(&bytes).unwrap().sequence == sequence
            {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("synthetic mutation was not held");
}
async fn no_replay(path: &Path, received: u64) {
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert_eq!(counts(path).received, received, "mutation replayed");
}
async fn candidate(owner: &Administration, source: &str) {
    close(owner).await;
    owner.action(0, generation(owner), ProAdminAction::Preview);
    edit(owner, PlanField::Identity, "operational.main");
    edit(owner, PlanField::Entry, "configuration.yaml");
    edit(owner, PlanField::Path, "configuration.yaml");
    edit(
        owner,
        PlanField::Source,
        format!("{source}\n# synthetic fault candidate\n"),
    );
    owner.editor_action(1, generation(owner), ProAdminAction::Preview);
    until(owner, "fault candidate admitted", |state| {
        state.configuration.active.is_none()
            && state
                .configuration
                .planner
                .as_ref()
                .is_some_and(|editor| editor.plan_state() == Some(plan::PlanState::Admitted))
    })
    .await;
}
async fn lost_response(
    owner: &Administration,
    session: &SessionCoordinator,
    connection: &Connection,
    path: &Path,
    source: &str,
) {
    close(owner).await;
    let before = read(session, connection).await;
    let count = idle(path).await;
    owner.action(0, generation(owner), ProAdminAction::Save);
    owner.edit(
        1,
        generation(owner),
        Field::Source as i32,
        format!("{source}\n# response lost after actual commit\n"),
    );
    Fault::LoseCommitted.set(path);
    let old_generation = generation(owner);
    owner.editor_action(1, old_generation, ProAdminAction::Save);
    until(
        owner,
        "response lost after commit remains uncertain",
        |state| {
            state.configuration.active.is_none()
                && state
                    .configuration
                    .draft
                    .as_ref()
                    .is_some_and(|draft| draft.stage() == EditStage::Uncertain)
        },
    )
    .await;
    let after = read(session, connection).await;
    assert_eq!(after.sql().revision(), before.sql().revision() + 1);
    {
        let state = owner.0.state.lock().unwrap();
        let draft = state.configuration.draft.as_ref().unwrap();
        assert_eq!(draft.captured.active(), before.active());
        assert!(draft.receipt.is_none(), "lost response invented a receipt");
    }
    owner.editor_action(1, old_generation, ProAdminAction::Save);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    let after_count = idle(path).await;
    assert_eq!(after_count.received, count.received + 1);
    assert_eq!(after_count.forwarded, count.forwarded + 1);
    assert_eq!(after_count.lost, count.lost + 1);
    no_replay(path, after_count.received).await;
    Fault::Normal.set(path);
    owner.editor_action(1, generation(owner), ProAdminAction::Resolve);
    until(owner, "explicit resolution after lost response", |state| {
        state.configuration.synchronized
            && state.configuration.active.is_none()
            && state
                .configuration
                .draft
                .as_ref()
                .is_some_and(|draft| draft.stage() == EditStage::Draft)
    })
    .await;
    no_replay(path, after_count.received).await;
    owner.edit(1, generation(owner), Field::Source as i32, source.into());
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    until(
        owner,
        "new confirmation restores original source",
        |state| outcome(state, Outcome::Published),
    )
    .await;
    assert_eq!(
        read(session, connection).await.sql().revision(),
        after.sql().revision() + 1
    );
}
async fn business_outcomes(
    owner: &Administration,
    session: &SessionCoordinator,
    connection: &Connection,
    path: &Path,
    source: &str,
) {
    for (fault, expected) in [
        (Fault::Unknown, Outcome::CommitUnknown),
        (Fault::Reconciling, Outcome::CommittedReconciling),
        (Fault::Restart, Outcome::RestartRequired),
    ] {
        candidate(owner, source).await;
        let before = read(session, connection).await;
        let count = idle(path).await;
        fault.set(path);
        owner.editor_action(1, generation(owner), ProAdminAction::Apply);
        until(owner, "injected business outcome", |state| {
            outcome(state, expected)
        })
        .await;
        {
            let state = owner.0.state.lock().unwrap();
            assert_eq!(
                state
                    .configuration
                    .planner
                    .as_ref()
                    .unwrap()
                    .activation_displayed_active(),
                Some(before.active())
            );
        }
        assert_eq!(
            read(session, connection).await.sql(),
            before.sql(),
            "injected outcome forwarded Apply"
        );
        owner.editor_action(1, generation(owner), ProAdminAction::Apply);
        let after_count = idle(path).await;
        assert_eq!(after_count.received, count.received + 1);
        assert_eq!(after_count.forwarded, count.forwarded);
        assert_eq!(after_count.injected, count.injected + 1);
        no_replay(path, after_count.received).await;
        Fault::Normal.set(path);
    }
}
async fn interruptions(
    owner: &Administration,
    session: &SessionCoordinator,
    connection: &Connection,
    path: &Path,
    source: &str,
) {
    candidate(owner, source).await;
    let before = read(session, connection).await;
    let count = idle(path).await;
    Fault::Held.set(path);
    owner.editor_action(1, generation(owner), ProAdminAction::Apply);
    held(path, count.received + 1).await;
    owner.foreground(false);
    {
        let state = owner.0.state.lock().unwrap();
        assert!(!state.configuration.opened());
        assert!(state.configuration.current.is_none());
    }
    let after_count = idle(path).await;
    assert_eq!(after_count.canceled, count.canceled + 1);
    assert_eq!(after_count.forwarded, count.forwarded);
    Fault::Normal.set(path);
    owner.foreground(true);
    until(owner, "resume administrator", |state| {
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
    owner.section(i32::try_from(section).unwrap(), generation(owner));
    until(owner, "resume fresh configuration", root_ready).await;
    assert_eq!(read(session, connection).await.sql(), before.sql());
    no_replay(path, after_count.received).await;

    owner.action(0, generation(owner), ProAdminAction::Save);
    owner.edit(
        1,
        generation(owner),
        Field::Source as i32,
        format!("{source}\n# never forwarded after role withdrawal\n"),
    );
    let count = idle(path).await;
    Fault::Held.set(path);
    owner.editor_action(1, generation(owner), ProAdminAction::Save);
    held(path, count.received + 1).await;
    std::fs::write(
        path.join("revoke-admin"),
        b"explicit synthetic account only",
    )
    .unwrap();
    until(
        owner,
        "administrator withdrawal purges private source",
        |state| {
            !state.configuration.opened()
                && state.configuration.current.is_none()
                && state
                    .private
                    .authority()
                    .is_none_or(|authority| !authority.administrator())
        },
    )
    .await;
    assert!(path.join("admin-revoked.json").is_file());
    let after_count = idle(path).await;
    assert_eq!(after_count.canceled, count.canceled + 1);
    assert_eq!(after_count.forwarded, count.forwarded);
    no_replay(path, after_count.received).await;
    Fault::Normal.set(path);
}
pub(super) async fn qualify(
    owner: &Administration,
    session: &SessionCoordinator,
    connection: &Connection,
    path: &Path,
    source: &str,
) {
    lost_response(owner, session, connection, path, source).await;
    business_outcomes(owner, session, connection, path, source).await;
    interruptions(owner, session, connection, path, source).await;
}
