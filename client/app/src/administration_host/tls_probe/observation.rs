use super::*;
use home_wire_contract::home::observability::v1 as obs;
use std::{collections::BTreeSet, path::Path};

fn logs(owner: &Administration) -> Option<obs::QueryRuntimeLogsResponse> {
    let state = owner.0.state.lock().unwrap();
    match state.private.pane(Section::Logs)?.content.as_ref()? {
        Payload::Logs(value) => Some(*value.clone()),
        Payload::Personal { .. }
        | Payload::Sessions(_)
        | Payload::Factors(_)
        | Payload::MfaPolicy(_)
        | Payload::Keys(_)
        | Payload::Accounts(_)
        | Payload::Invitations(_)
        | Payload::Policy { .. }
        | Payload::Integrations(_)
        | Payload::Integration(_)
        | Payload::Health(_)
        | Payload::Metrics(_)
        | Payload::Audits(_) => None,
    }
}
async fn marker(root: &Path, name: &str) {
    tokio::time::timeout(Duration::from_secs(5), async {
        while !root.join(name).is_file() {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap_or_else(|_| panic!("synthetic fixture marker missing: {name}"));
}
async fn bounded_logs(owner: &Administration, root: &Path) {
    section(owner, Section::Logs).await;
    let mut seen = BTreeSet::new();
    let mut through = None;
    loop {
        let Some(obs::query_runtime_logs_response::Result::Page(page)) =
            logs(owner).unwrap().result
        else {
            panic!("runtime logs page")
        };
        assert!(!page.rows.is_empty() && page.rows.len() <= 100);
        if let Some(upper) = through {
            assert_eq!(page.upper_sequence, upper);
        }
        through = Some(page.upper_sequence);
        for row in &page.rows {
            assert!(
                seen.insert(row.sequence),
                "runtime row duplicated across pages"
            );
        }
        let has_more = owner.0.state.lock().unwrap().next.is_some();
        if !has_more {
            break;
        }
        owner.action(0, generation(owner), ProAdminAction::More);
        until(owner, "next bounded runtime log page", ready).await;
    }
    assert!(seen.len() >= 230);
    owner.action(0, generation(owner), ProAdminAction::Refresh);
    until(owner, "runtime logs restart at first page", ready).await;
    assert!(owner.0.state.lock().unwrap().page.is_none());
    assert!(owner.0.state.lock().unwrap().next.is_some());
    std::fs::write(root.join("evict-logs"), []).unwrap();
    marker(root, "logs-evicted").await;
    owner.action(0, generation(owner), ProAdminAction::More);
    until(owner, "runtime journal reports eviction", |state| {
        ready(state)
            && state
                .rows
                .iter()
                .any(|row| row.title == "Historique du journal tronqué")
    })
    .await;
    assert!(matches!(
        logs(owner).unwrap().result,
        Some(obs::query_runtime_logs_response::Result::CursorEvicted(_))
    ));
    assert!(owner.0.state.lock().unwrap().next.is_none());
    owner.action(0, generation(owner), ProAdminAction::Refresh);
    until(owner, "fresh runtime capture after eviction", ready).await;
    assert!(matches!(
        logs(owner).unwrap().result,
        Some(obs::query_runtime_logs_response::Result::Page(_))
    ));
}
async fn bounded_audits(owner: &Administration, section_value: Section) {
    section(owner, section_value).await;
    let mut seen = BTreeSet::new();
    let mut upper = None;
    loop {
        let (response, more) = {
            let state = owner.0.state.lock().unwrap();
            let Some(Payload::Audits(value)) =
                state.private.pane(section_value).unwrap().content.as_ref()
            else {
                panic!("audit payload")
            };
            (*value.clone(), state.next.is_some())
        };
        let Some(obs::query_audits_response::Result::Page(page)) = response.result else {
            panic!("audit page")
        };
        assert_eq!(page.source, section_value.audit_source().unwrap());
        assert!(!page.rows.is_empty() && page.rows.len() <= 100);
        if let Some(expected) = upper {
            assert_eq!(page.upper_id, expected);
        }
        upper = Some(page.upper_id);
        for row in &page.rows {
            assert!(seen.insert(row.id), "audit row duplicated across pages");
        }
        if !more {
            break;
        }
        owner.action(0, generation(owner), ProAdminAction::More);
        until(owner, "next bounded audit page", ready).await;
    }
    assert!(seen.len() >= 110);
    // A stale server cursor is a semantic response, not an empty successful page.
    {
        let mut state = owner.0.state.lock().unwrap();
        state.page = Some(PageToken::parse("nhf018-invalid-cursor").unwrap());
        state.request();
        owner.retired(&state);
    }
    until(owner, "stale audit cursor is visible", |state| {
        ready(state) && state.rows.iter().any(|row| row.title == "Capture expirée")
    })
    .await;
    owner.action(0, generation(owner), ProAdminAction::Refresh);
    until(owner, "refresh discards stale audit cursor", |state| {
        ready(state) && state.page.is_none() && state.private.pane(section_value)
            .and_then(|pane|pane.content.as_ref()).is_some_and(|payload|matches!(payload,Payload::Audits(value) if matches!(value.result,Some(obs::query_audits_response::Result::Page(_)))))
    }).await;
}
#[derive(Clone, Copy)]
enum ReadFault {
    Normal,
    Unavailable,
    Delayed,
}
impl ReadFault {
    fn apply(self, root: &Path) {
        let value = match self {
            Self::Normal => "normal",
            Self::Unavailable => "unavailable",
            Self::Delayed => "delayed",
        };
        std::fs::write(root.join("read-fault.pending"), value).unwrap();
        std::fs::rename(root.join("read-fault.pending"), root.join("read-fault")).unwrap();
    }
}
async fn independent_failure(session: &SessionCoordinator, fixture: &Fixture, root: &Path) {
    let origin = url::Url::parse(&fixture.fault_origin).unwrap();
    assert_eq!(origin.scheme(), "https");
    assert_eq!(origin.host_str(), Some("127.0.0.1"));
    let connection = Connection::new(&fixture.fault_origin, fixture.ca_pem.as_bytes()).unwrap();
    let owner = Administration::detached();
    let cancel = CancellationToken::new();
    let (run, source, token) = (owner.clone(), session.clone(), cancel.clone());
    let task = tokio::spawn(async move { run.run(source, connection, token).await });
    owner.visible(true);
    until(&owner, "fault proxy passes native account read", ready).await;
    ReadFault::Unavailable.apply(root);
    let section_token = Section::ALL
        .iter()
        .position(|value| *value == Section::Logs)
        .unwrap() as i32
        + 1;
    owner.section(section_token, generation(&owner));
    until(&owner, "one observation service unavailable", |state| {
        state.stage == Stage::Unavailable
    })
    .await;
    assert!(
        owner
            .0
            .state
            .lock()
            .unwrap()
            .private
            .authority()
            .unwrap()
            .administrator()
    );
    section(&owner, Section::Health).await;
    section(&owner, Section::Accounts).await;
    section(&owner, Section::Integrations).await;
    ReadFault::Delayed.apply(root);
    owner.section(section_token, generation(&owner));
    marker(root, "read-entered").await;
    let retired = generation(&owner);
    owner.foreground(false);
    assert!(owner.0.state.lock().unwrap().private.authority().is_none());
    owner.action(0, retired, ProAdminAction::Refresh);
    assert!(!owner.0.state.lock().unwrap().requested);
    ReadFault::Normal.apply(root);
    let resumed = std::time::Instant::now();
    owner.foreground(true);
    until(&owner, "fresh foreground after cancelled read", ready).await;
    assert!(
        resumed.elapsed() < Duration::from_millis(2500),
        "local cancellation must release the reader before the delayed server responds"
    );
    std::fs::write(root.join("read-release"), []).unwrap();
    marker(root, "read-finished").await;
    section(&owner, Section::Logs).await;
    assert!(matches!(
        logs(&owner).unwrap().result,
        Some(obs::query_runtime_logs_response::Result::Page(_))
    ));
    cancel.cancel();
    tokio::time::timeout(Duration::from_secs(5), task)
        .await
        .unwrap()
        .unwrap();
}
pub(super) async fn qualify(
    owner: &Administration,
    _connection: &Connection,
    session: &SessionCoordinator,
    fixture: &Fixture,
    root: &Path,
) {
    bounded_logs(owner, root).await;
    bounded_audits(owner, Section::Audits).await;
    bounded_audits(owner, Section::SecretAudits).await;
    independent_failure(session, fixture, root).await;
}
