//! Same calendar owner as the native screen, actual Go TLS/auth/SQL fixture.
use super::*;
use crate::calendar::{Resolution, Temporal};
use home_client_session::{SecureStore, StoreFailure};
use home_client_transport::Connection;
use serde::{Deserialize, Serialize};
use std::{path::PathBuf, time::Duration};
use zeroize::Zeroizing;

#[derive(Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Mode {
    SyntheticOnly,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Credential {
    login_name: String,
    password: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Entities {
    local: String,
    remote: String,
    calculated: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Fixture {
    mode: Mode,
    origin: String,
    ca_pem: String,
    entities: Entities,
    reader: Credential,
    production_calendars: bool,
}
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
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Control {
    Seed,
    LoseNextReply,
    RevokeRead,
    AllowRead,
    Outage,
    RestoreNetwork,
    PauseWatch,
    ResumeWatch,
    Snapshot,
}
#[derive(Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Instruction {
    sequence: u64,
    action: Control,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Stats {
    mutation_requests: u64,
    active_streams: u64,
}
struct Controls {
    root: PathBuf,
    sequence: u64,
}
impl Controls {
    async fn send(&mut self, action: Control) {
        self.sequence += 1;
        let command = Instruction {
            sequence: self.sequence,
            action,
        };
        let temporary = self.root.join("control.writing");
        std::fs::write(&temporary, serde_json::to_vec(&command).unwrap()).unwrap();
        std::fs::rename(temporary, self.root.join("control.json")).unwrap();
        let ack = self.root.join(format!("ack-{:02}.json", self.sequence));
        tokio::time::timeout(Duration::from_secs(8), async {
            loop {
                if let Ok(raw) = std::fs::read(&ack) {
                    assert_eq!(
                        serde_json::from_slice::<Instruction>(&raw).unwrap(),
                        command
                    );
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("fixture control acknowledgement");
    }
    async fn stats(&mut self) -> Stats {
        self.send(Control::Snapshot).await;
        serde_json::from_slice(
            &std::fs::read(self.root.join(format!("stats-{:02}.json", self.sequence))).unwrap(),
        )
        .unwrap()
    }
}
async fn until(owner: &Calendars, label: &str, predicate: impl Fn(&State) -> bool) {
    let outcome = tokio::time::timeout(Duration::from_secs(20), async {
        loop {
            if predicate(&owner.0.state.lock().unwrap()) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await;
    if outcome.is_err() {
        let s = owner.0.state.lock().unwrap();
        panic!(
            "calendar wait {label}: status={:?}, active={}, ready={}, busy={}, uncertain={}, rows={}, draft={}, notice={}",
            s.status,
            s.active,
            s.ready,
            s.busy,
            s.uncertain,
            s.snapshot.as_ref().map_or(0, |v| v.occurrences().len()),
            s.draft.is_some(),
            s.notice
        );
    }
}
fn ready(s: &State) -> bool {
    s.active && s.ready && !s.busy && s.status == Status::Ready
}
async fn choose(owner: &Calendars, id: &str) {
    until(owner, "catalog", |s| {
        s.active && !s.busy && !s.catalog.is_empty()
    })
    .await;
    let (token, generation) = {
        let s = owner.0.state.lock().unwrap();
        (
            s.catalog
                .iter()
                .position(|v| v.id.entity_id == id)
                .expect("authorized calendar") as i32
                + 1,
            s.generation,
        )
    };
    owner.choose(token, generation);
    until(owner, "selected fresh calendar", |s| {
        ready(s)
            && s.selected
                .as_ref()
                .is_some_and(|v| v.entity().as_str() == id)
    })
    .await;
}
async fn open(owner: &Calendars, uid: &str) {
    until(owner, "range before event selection", ready).await;
    let (token, generation) = {
        let s = owner.0.state.lock().unwrap();
        (
            s.snapshot
                .as_ref()
                .unwrap()
                .occurrences()
                .iter()
                .position(|v| v.key.uid == uid)
                .expect("occurrence") as i32
                + 1,
            s.generation,
        )
    };
    owner.select(token, generation);
    until(owner, "event editor", |s| {
        ready(s)
            && s.draft
                .as_ref()
                .is_some_and(|v| v.key.uid().as_str() == uid)
    })
    .await;
}
fn edit(owner: &Calendars, summary: &str) {
    let (t, g) = {
        let s = owner.0.state.lock().unwrap();
        (s.draft_token, s.generation)
    };
    owner.edit(t, g, |draft| draft.edit(Field::Summary, summary.into()));
}
fn action(owner: &Calendars, action: ProAgendaAction) {
    let (t, g) = {
        let s = owner.0.state.lock().unwrap();
        (s.draft_token, s.generation)
    };
    owner.action(t, g, action);
}
fn command(owner: &Calendars, command: CalendarCommand, a: &str, b: &str, c: &str) {
    let g = owner.0.state.lock().unwrap().generation;
    owner.command(g, command, a, b, c);
}
async fn committed(owner: &Calendars, summary: &str) {
    until(owner, "commit followed by actual snapshot", |s| {
        ready(s)
            && !s.uncertain
            && s.draft.is_none()
            && s.snapshot
                .as_ref()
                .is_some_and(|v| v.occurrences().iter().any(|v| v.summary == summary))
    })
    .await;
}
async fn resolve(owner: &Calendars, boundary: Boundary) {
    {
        let mut s = owner.0.state.lock().unwrap();
        assert!(s.can_edit_draft(s.draft_token, s.generation));
        let request = s
            .draft
            .as_ref()
            .unwrap()
            .resolution_request(boundary)
            .expect("complete wall time");
        assert!(s.enqueue(Job::Resolve(boundary, request)));
    }
    owner.0.changed.notify_one();
    until(owner, "server time resolution", |s| {
        ready(s)
            && s.draft.as_ref().is_some_and(|d| match &d.temporal {
                Temporal::Timed { start, end, .. } => !matches!(
                    match boundary {
                        Boundary::Start => &start.resolution,
                        Boundary::End => &end.resolution,
                    },
                    Resolution::Unresolved
                ),
                Temporal::AllDay { .. } => false,
            })
    })
    .await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "explicit isolated Go Calendar TLS fixture"]
async fn actual_calendar_owner_tls_cas_uncertainty_and_revocation() {
    let root = PathBuf::from(
        std::env::var("HOMEWARD_NHF015_TLS_FIXTURE").expect("isolated fixture required"),
    );
    assert!(root.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    assert!(matches!(fixture.mode, Mode::SyntheticOnly));
    assert!(!fixture.production_calendars);
    let uri = url::Url::parse(&fixture.origin).unwrap();
    assert_eq!(uri.scheme(), "https");
    assert_eq!(uri.host_str(), Some("127.0.0.1"));
    assert!(uri.port().is_some());
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes()).unwrap();
    let session = SessionCoordinator::new(connection.auth(), Arc::new(MemoryStore::default()));
    session
        .login(
            fixture.reader.login_name,
            Zeroizing::new(fixture.reader.password),
        )
        .await
        .unwrap();
    assert_eq!(session.subscribe().borrow().phase, Phase::Ready);
    let owner = Calendars::detached();
    {
        let mut s = owner.0.state.lock().unwrap();
        s.start = "2026-11-01".into();
        s.end = "2026-11-08".into();
        s.zone = "America/Toronto".into();
    }
    owner.interactive(true);
    let cancel = CancellationToken::new();
    let worker = tokio::spawn({
        let owner = owner.clone();
        let session = session.clone();
        let api = connection.calendars();
        let stop = cancel.clone();
        async move { owner.run(session, api, stop).await }
    });
    let mut controls = Controls { root, sequence: 0 };
    choose(&owner, &fixture.entities.local).await;
    assert!(!owner.0.state.lock().unwrap().present());
    command(
        &owner,
        CalendarCommand::Create,
        "Owner created calendar",
        "",
        "",
    );
    until(&owner, "calendar created from absent", |s| {
        ready(s) && s.present()
    })
    .await;
    assert_eq!(controls.stats().await.mutation_requests, 1);
    controls.send(Control::Seed).await;
    until(&owner, "explicit fixture events read", |s| {
        ready(s)
            && s.snapshot
                .as_ref()
                .is_some_and(|v| v.occurrences().len() == 4)
    })
    .await;
    assert_eq!(controls.stats().await.mutation_requests, 4);
    // One real client calendar create plus three explicitly controlled Go seed writes.
    const SETUP_MUTATIONS: u64 = 4;

    open(&owner, "synthetic-civil").await;
    edit(&owner, "Owner nominal all-day");
    action(&owner, ProAgendaAction::Save);
    committed(&owner, "Owner nominal all-day").await;
    assert_eq!(
        controls.stats().await.mutation_requests,
        1 + SETUP_MUTATIONS
    );

    // A delayed Watch permits a genuine stale CAS request from the same owner.
    open(&owner, "synthetic-civil").await;
    edit(&owner, "Local draft survives CAS");
    controls.send(Control::PauseWatch).await;
    let api = connection.calendars();
    let lease = session.access(&cancel).await.unwrap();
    let key = {
        let s = owner.0.state.lock().unwrap();
        event_key(&s.draft.as_ref().unwrap().key)
    };
    let remote = api
        .get_event(
            wire::GetEventRequest {
                key: key.clone().into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(8),
            &cancel,
        )
        .await
        .unwrap();
    let mut value = remote.event.value.as_option().unwrap().clone();
    value.summary = "Concurrent server edit".into();
    api.mutate_event(
        wire::MutateEventRequest {
            key: key.clone().into(),
            mutation: wire::Mutation::Update.into(),
            expected_revision: remote.event.revision.clone(),
            value: value.into(),
            ..Default::default()
        },
        lease.token(),
        Duration::from_secs(8),
        &cancel,
    )
    .await
    .unwrap();
    action(&owner, ProAgendaAction::Save);
    until(&owner, "server CAS conflict", |s| {
        !s.busy
            && s.draft
                .as_ref()
                .is_some_and(|d| d.phase == EditPhase::Conflict)
    })
    .await;
    assert_eq!(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .draft
            .as_ref()
            .unwrap()
            .original
            .summary,
        "Local draft survives CAS"
    );
    assert_eq!(
        controls.stats().await.mutation_requests,
        3 + SETUP_MUTATIONS
    );
    controls.send(Control::ResumeWatch).await;
    until(&owner, "post-conflict Watch", |s| {
        ready(s)
            && s.snapshot.as_ref().is_some_and(|v| {
                v.occurrences()
                    .iter()
                    .any(|v| v.summary == "Concurrent server edit")
            })
    })
    .await;
    action(&owner, ProAgendaAction::Rebase);
    until(&owner, "explicit rebase", |s| {
        ready(s)
            && s.draft
                .as_ref()
                .is_some_and(|d| d.phase == EditPhase::Ready)
    })
    .await;
    assert_eq!(
        controls.stats().await.mutation_requests,
        3 + SETUP_MUTATIONS,
        "rebase is a read"
    );
    action(&owner, ProAgendaAction::Save);
    committed(&owner, "Local draft survives CAS").await;

    open(&owner, "synthetic-civil").await;
    edit(&owner, "Committed but reply lost");
    controls.send(Control::LoseNextReply).await;
    action(&owner, ProAgendaAction::Save);
    until(&owner, "unknown commit", |s| !s.busy && s.uncertain).await;
    action(&owner, ProAgendaAction::Save);
    assert_eq!(
        controls.stats().await.mutation_requests,
        5 + SETUP_MUTATIONS
    );
    controls.send(Control::Outage).await;
    until(&owner, "offline while commit is unknown", |s| {
        s.status == Status::Offline && s.uncertain
    })
    .await;
    controls.send(Control::RestoreNetwork).await;
    until(&owner, "unknown warning survives read recovery", |s| {
        ready(s) && s.uncertain && s.notice.contains("Résultat du commit inconnu")
    })
    .await;
    assert_eq!(
        controls.stats().await.mutation_requests,
        5 + SETUP_MUTATIONS
    );
    command(&owner, CalendarCommand::Reload, "", "", "");
    until(&owner, "explicit reconciliation", |s| {
        ready(s)
            && !s.uncertain
            && s.snapshot.as_ref().is_some_and(|v| {
                v.occurrences()
                    .iter()
                    .any(|v| v.summary == "Committed but reply lost")
            })
    })
    .await;
    assert_eq!(
        controls.stats().await.mutation_requests,
        5 + SETUP_MUTATIONS,
        "no refresh replay"
    );

    command(&owner, CalendarCommand::NewEvent, "", "", "");
    {
        let s = owner.0.state.lock().unwrap();
        let (t, g) = (s.draft_token, s.generation);
        drop(s);
        owner.edit(t, g, |d| {
            d.edit(Field::Summary, "Owner explicit second fold".into());
            d.switch(true, "America/Toronto");
            d.timed(
                Boundary::Start,
                "2026-11-01".into(),
                "01:30:00".into(),
                "America/Toronto".into(),
            );
            d.timed(
                Boundary::End,
                "2026-11-01".into(),
                "02:30:00".into(),
                "America/Toronto".into(),
            );
        });
    }
    resolve(&owner, Boundary::Start).await;
    {
        let s = owner.0.state.lock().unwrap();
        assert!(s.draft.as_ref().unwrap().event().is_none());
        let (t, g) = (s.draft_token, s.generation);
        drop(s);
        owner.edit(t, g, |d| d.choose(Boundary::Start, true));
    }
    resolve(&owner, Boundary::End).await;
    let fold_key = {
        let s = owner.0.state.lock().unwrap();
        event_key(&s.draft.as_ref().unwrap().key)
    };
    action(&owner, ProAgendaAction::Save);
    committed(&owner, "Owner explicit second fold").await;
    let fold = api
        .get_event(
            wire::GetEventRequest {
                key: fold_key.into(),
                ..Default::default()
            },
            lease.token(),
            Duration::from_secs(8),
            &cancel,
        )
        .await
        .unwrap();
    assert!(
        matches!(&fold.event.value.span.value,Some(wire::span::Value::Times(v)) if v.start_instant=="2026-11-01T06:30:00Z"&&v.anchor_local=="2026-11-01T01:30:00")
    );
    let deleted_uid = fold.event.key.uid.clone();
    open(&owner, &deleted_uid).await;
    action(&owner, ProAgendaAction::Remove);
    until(&owner, "event deletion reread", |s| {
        ready(s)
            && s.draft.is_none()
            && s.snapshot
                .as_ref()
                .is_some_and(|v| v.occurrences().iter().all(|v| v.key.uid != deleted_uid))
    })
    .await;
    command(&owner, CalendarCommand::NewEvent, "", "", "");
    {
        let s = owner.0.state.lock().unwrap();
        let (t, g) = (s.draft_token, s.generation);
        drop(s);
        owner.edit(t, g, |d| {
            d.edit(Field::Summary, "Gap stays invalid".into());
            d.switch(true, "America/Toronto");
            d.timed(
                Boundary::Start,
                "2026-03-08".into(),
                "02:30:00".into(),
                "America/Toronto".into(),
            );
        });
    }
    resolve(&owner, Boundary::Start).await;
    assert!(
        matches!(&owner.0.state.lock().unwrap().draft.as_ref().unwrap().temporal,Temporal::Timed{start,..} if matches!(start.resolution,Resolution::Nonexistent))
    );
    action(&owner, ProAgendaAction::Save);
    assert_eq!(
        controls.stats().await.mutation_requests,
        7 + SETUP_MUTATIONS,
        "gap never dispatches"
    );

    for id in [&fixture.entities.remote, &fixture.entities.calculated] {
        choose(&owner, id).await;
        assert!(!owner.0.state.lock().unwrap().editable());
        command(&owner, CalendarCommand::NewEvent, "", "", "");
        command(&owner, CalendarCommand::Remove, "", "", "");
        assert_eq!(
            controls.stats().await.mutation_requests,
            7 + SETUP_MUTATIONS
        );
    }
    choose(&owner, &fixture.entities.local).await;
    open(&owner, "synthetic-civil").await;
    edit(&owner, "Offline draft retained");
    controls.send(Control::Outage).await;
    until(&owner, "interrupted Watch", |s| {
        s.status == Status::Offline && !s.ready
    })
    .await;
    {
        let s = owner.0.state.lock().unwrap();
        assert!(s.snapshot.is_some());
        assert_eq!(
            s.draft.as_ref().unwrap().original.summary,
            "Offline draft retained"
        );
        assert!(!s.editable());
    }
    action(&owner, ProAgendaAction::Save);
    controls.send(Control::RestoreNetwork).await;
    until(&owner, "read-only reconnect", ready).await;
    assert_eq!(
        controls.stats().await.mutation_requests,
        7 + SETUP_MUTATIONS
    );
    assert!(
        !owner.0.state.lock().unwrap().notice.contains("Hors ligne"),
        "recovery removes stale offline notice"
    );
    controls.send(Control::RevokeRead).await;
    until(&owner, "revocation purge", |s| {
        s.catalog.is_empty() && s.snapshot.is_none() && s.draft.is_none()
    })
    .await;
    controls.send(Control::AllowRead).await;
    command(&owner, CalendarCommand::Reload, "", "", "");
    until(&owner, "explicit authorized reread", ready).await;
    assert_eq!(
        controls.stats().await.mutation_requests,
        7 + SETUP_MUTATIONS
    );

    command(
        &owner,
        CalendarCommand::Rename,
        "Owner renamed calendar",
        "",
        "",
    );
    until(&owner, "rename", |s| {
        ready(s)
            && s.calendar()
                .is_some_and(|v| v.name.as_deref() == Some("Owner renamed calendar"))
    })
    .await;
    command(&owner, CalendarCommand::Remove, "", "", "");
    until(&owner, "calendar tombstone", |s| {
        ready(s)
            && s.calendar()
                .is_some_and(|v| v.revision.presence == wire::Presence::Removed)
    })
    .await;
    assert!(!owner.0.state.lock().unwrap().editable());
    command(
        &owner,
        CalendarCommand::Create,
        "Forbidden identity resurrection",
        "",
        "",
    );
    assert_eq!(
        controls.stats().await.mutation_requests,
        9 + SETUP_MUTATIONS
    );
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(8), worker)
        .await
        .unwrap()
        .unwrap();
    let stats = controls.stats().await;
    assert_eq!(stats.active_streams, 0);
    assert_eq!(stats.mutation_requests, 9 + SETUP_MUTATIONS);
    assert!(owner.0.state.lock().unwrap().snapshot.is_none());
    println!(
        "NHF015_OWNER_TLS_PASS cas_explicit_rebase_unknown_no_replay_fold_gap_readonly_offline_revocation_calendar_crud_joined"
    );
}
