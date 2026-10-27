//! Opt-in TLS13/Go integration. Ordinary cargo runs do not claim this fixture.
use home_client_transport::{AccessToken, Connection};
use home_wire_contract::{
    calendars_contract as admit,
    home::{auth::v1 as auth, calendars::v1 as wire},
};
use serde::{Deserialize, Serialize};
use std::{
    net::IpAddr,
    path::{Path, PathBuf},
    time::Duration,
};
use tokio_util::sync::CancellationToken;

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
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
enum Control {
    LoseNextReply,
    RevokeRead,
    AllowRead,
    RevokeWrite,
    AllowWrite,
}
#[derive(Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Instruction {
    sequence: u64,
    action: Control,
}
async fn control(root: &Path, sequence: &mut u64, action: Control) {
    *sequence += 1;
    let command = Instruction {
        sequence: *sequence,
        action,
    };
    let temporary = root.join("control.writing");
    std::fs::write(&temporary, serde_json::to_vec(&command).unwrap()).unwrap();
    std::fs::rename(temporary, root.join("control.json")).unwrap();
    let acknowledgement = root.join(format!("ack-{:02}.json", *sequence));
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Ok(raw) = std::fs::read(&acknowledgement) {
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
    .expect("calendar fixture control timed out");
}
fn id(entity_id: &str) -> wire::CalendarId {
    wire::CalendarId {
        entity_id: entity_id.into(),
        ..Default::default()
    }
}
#[tokio::test]
async fn actual_calendar_tls_mutations_fold_resync_and_uncertainty() {
    let Ok(directory) = std::env::var("HOMEWARD_NHF015_TLS_FIXTURE") else {
        eprintln!("NHF015 TLS fixture not enabled; no integration claim");
        return;
    };
    let root = PathBuf::from(directory);
    assert!(root.is_absolute());
    let fixture: Fixture =
        serde_json::from_slice(&std::fs::read(root.join("ready.json")).unwrap()).unwrap();
    assert!(matches!(fixture.mode, Mode::SyntheticOnly));
    assert!(!fixture.production_calendars);
    let uri: http::Uri = fixture.origin.parse().unwrap();
    assert_eq!(uri.scheme_str(), Some("https"));
    assert!(uri.host().unwrap().parse::<IpAddr>().unwrap().is_loopback());
    let connection = Connection::new(&fixture.origin, fixture.ca_pem.as_bytes()).unwrap();
    let cancel = CancellationToken::new();
    let timeout = Duration::from_secs(5);
    let logged_in = connection
        .auth()
        .start_login(
            auth::StartLoginRequest {
                login_name: fixture.reader.login_name,
                password: fixture.reader.password,
                ..Default::default()
            },
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let Some(auth::login_decision::Outcome::Authenticated(tokens)) =
        logged_in.decision.outcome.as_ref()
    else {
        panic!("synthetic native session was not authenticated")
    };
    let token = AccessToken::parse(&tokens.access_token).unwrap();
    let api = connection.calendars();
    let catalog = api
        .list_calendars(
            wire::ListCalendarsRequest::default(),
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let catalog = admit::admit_catalog(&catalog).unwrap();
    assert_eq!(catalog.wire().calendars.len(), 3);
    let local = id(&fixture.entities.local);
    let local_id = admit::calendar_id(&local).unwrap();
    let current = catalog
        .wire()
        .calendars
        .iter()
        .find(|calendar| calendar.id.entity_id == local.entity_id)
        .unwrap();
    let current = admit::admit_calendar(current, &local_id).unwrap();
    assert!(admit::calendar_editable(&current));
    let mut watch = api
        .watch_calendar(
            wire::WatchCalendarRequest {
                id: local.clone().into(),
                ..Default::default()
            },
            &token,
            Duration::from_secs(40),
            &cancel,
        )
        .await
        .unwrap();
    let frame = watch.next().await.unwrap().unwrap();
    admit::admit_watch(&frame, &local_id, 0).unwrap();
    assert!(matches!(
        frame.frame,
        Some(wire::watch_calendar_response::Frame::Ready(_))
    ));
    let frame = watch.next().await.unwrap().unwrap();
    admit::admit_watch(&frame, &local_id, 1).unwrap();
    assert!(matches!(
        frame.frame,
        Some(wire::watch_calendar_response::Frame::ResyncRequired(_))
    ));
    let resolution_request = wire::ResolveLocalTimeRequest {
        id: local.clone().into(),
        local: "2026-11-01T01:30:00".into(),
        zone: "America/Toronto".into(),
        ..Default::default()
    };
    let resolution = api
        .resolve_local_time(resolution_request.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    let resolution = admit::admit_resolution(&resolution, &resolution_request).unwrap();
    assert_eq!(
        resolution.wire().resolution,
        wire::LocalTimeResolution::Ambiguous
    );
    let second = resolution.wire().candidates[1].instant.clone();
    let key = wire::EventKey {
        calendar: local.clone().into(),
        uid: "rust-transport-explicit-fold".into(),
        ..Default::default()
    };
    let absent = wire::Revision {
        resource: key.clone().into(),
        presence: wire::Presence::Absent.into(),
        ..Default::default()
    };
    let event = wire::Event {
        summary: "Synthetic explicit fold".into(),
        span: wire::Span {
            value: Some(wire::span::Value::Times(Box::new(wire::TimeSpan {
                anchor_local: resolution_request.local.clone(),
                zone: resolution_request.zone.clone(),
                start_instant: second.clone(),
                end_instant: "2026-11-01T07:30:00Z".into(),
                ..Default::default()
            }))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let request = wire::MutateEventRequest {
        key: key.clone().into(),
        mutation: wire::Mutation::Create.into(),
        expected_revision: absent.clone().into(),
        value: event.clone().into(),
        ..Default::default()
    };
    let domain_key = admit::event_key(&key).unwrap();
    let previous = admit::event_revision(&absent, &domain_key).unwrap();
    let admitted = admit::admit_event_mutation(&request, &current, &previous).unwrap();
    let receipt = api
        .mutate_event(request.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    let previous_calendar = admit::calendar_revision(&current.wire().revision, &local_id).unwrap();
    admit::admit_event_receipt(&receipt.receipt, &admitted, &previous_calendar).unwrap();
    let frame = watch.next().await.unwrap().unwrap();
    admit::admit_watch(&frame, &local_id, 2).unwrap();
    assert!(matches!(
        frame.frame,
        Some(wire::watch_calendar_response::Frame::Changed(_))
    ));
    let read = api
        .get_event(
            wire::GetEventRequest {
                key: key.clone().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let read = admit::admit_event(&read.event, &domain_key).unwrap();
    let Some(wire::span::Value::Times(span)) = &read.wire().value.span.value else {
        panic!("timed span changed kind")
    };
    assert_eq!(span.start_instant, second);
    let stale = api
        .mutate_event(request.clone(), &token, timeout, &cancel)
        .await
        .err()
        .unwrap();
    assert_eq!(
        stale.calendar_error().unwrap().unwrap().code,
        wire::ErrorCode::Conflict
    );
    for foreign in [&fixture.entities.remote, &fixture.entities.calculated] {
        let mut denied = request.clone();
        denied.key.as_option_mut().unwrap().calendar = id(foreign).into();
        denied.expected_revision.as_option_mut().unwrap().resource = denied.key.clone();
        let error = api
            .mutate_event(denied, &token, timeout, &cancel)
            .await
            .err()
            .unwrap();
        assert_eq!(
            error.calendar_error().unwrap().unwrap().code,
            wire::ErrorCode::ReadOnly
        );
    }
    let mut query = wire::QueryOccurrencesRequest {
        id: local.clone().into(),
        page_size: 1,
        window: wire::Window {
            zone: "America/Toronto".into(),
            bounds: Some(wire::window::Bounds::Dates(Box::new(wire::DateSpan {
                start: "2026-11-01".into(),
                end_exclusive: "2026-11-08".into(),
                ..Default::default()
            }))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let admitted_query = admit::admit_query(&query).unwrap();
    let page = api
        .query_occurrences(query.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    admit::admit_page(&page, &admitted_query, None).unwrap();
    assert_eq!(page.occurrences.len(), 1);
    assert!(!page.next_page_token.is_empty());
    query.page_token = page.next_page_token.clone();
    let revision = admit::calendar_revision(&page.revision, &local_id).unwrap();
    let next = api
        .query_occurrences(query.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    admit::admit_page(&next, &admit::admit_query(&query).unwrap(), Some(&revision)).unwrap();
    assert_ne!(
        (
            &next.occurrences[0].key.uid,
            &next.occurrences[0].recurrence_id
        ),
        (
            &page.occurrences[0].key.uid,
            &page.occurrences[0].recurrence_id
        )
    );
    let mut sequence = 0;
    control(&root, &mut sequence, Control::LoseNextReply).await;
    let mut update = request;
    update.mutation = wire::Mutation::Update.into();
    update.expected_revision = receipt.receipt.revision.clone();
    let error = api
        .mutate_event(update, &token, timeout, &cancel)
        .await
        .err()
        .unwrap();
    assert_eq!(
        error.calendar_error().unwrap().unwrap().code,
        wire::ErrorCode::CommitUnknown
    );
    let frame = watch.next().await.unwrap().unwrap();
    admit::admit_watch(&frame, &local_id, 3).unwrap();
    let read = api
        .get_event(
            wire::GetEventRequest {
                key: key.clone().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_event(&read.event, &domain_key).unwrap();
    assert_eq!(read.event.revision.number, 2);
    let stale_cursor = api
        .query_occurrences(query, &token, timeout, &cancel)
        .await
        .err()
        .unwrap();
    assert_eq!(
        stale_cursor.calendar_error().unwrap().unwrap().code,
        wire::ErrorCode::StaleCursor
    );
    control(&root, &mut sequence, Control::RevokeWrite).await;
    let calendar = api
        .get_calendar(
            wire::GetCalendarRequest {
                id: local.clone().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert!(!admit::calendar_editable(
        &admit::admit_calendar(&calendar.calendar, &local_id).unwrap()
    ));
    let delete = wire::MutateEventRequest {
        key: key.clone().into(),
        mutation: wire::Mutation::Delete.into(),
        expected_revision: read.event.revision.clone(),
        ..Default::default()
    };
    let error = api
        .mutate_event(delete.clone(), &token, timeout, &cancel)
        .await
        .err()
        .unwrap();
    assert!(matches!(
        error.kind(),
        home_client_transport::FailureKind::Rpc(home_client_transport::RpcCode::PermissionDenied)
    ));
    control(&root, &mut sequence, Control::AllowWrite).await;
    control(&root, &mut sequence, Control::RevokeRead).await;
    assert!(watch.next().await.is_err());
    drop(watch);
    control(&root, &mut sequence, Control::AllowRead).await;
    let mut fresh = api
        .watch_calendar(
            wire::WatchCalendarRequest {
                id: local.clone().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let first = fresh.next().await.unwrap().unwrap();
    assert_eq!(first.stream_sequence, 1);
    assert!(matches!(
        first.frame,
        Some(wire::watch_calendar_response::Frame::Ready(_))
    ));
    drop(fresh);
    let reread = api
        .get_event(
            wire::GetEventRequest {
                key: key.clone().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert_eq!(reread.event.revision.number, 2);
    let gap_request = wire::ResolveLocalTimeRequest {
        local: "2026-03-08T02:30:00".into(),
        ..resolution_request
    };
    let gap = api
        .resolve_local_time(gap_request.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    admit::admit_resolution(&gap, &gap_request).unwrap();
    assert_eq!(gap.resolution, wire::LocalTimeResolution::Nonexistent);
    assert!(gap.candidates.is_empty());
    let deleted = api
        .mutate_event(delete, &token, timeout, &cancel)
        .await
        .unwrap();
    assert_eq!(deleted.receipt.revision.presence, wire::Presence::Removed);
    assert_eq!(deleted.receipt.revision.number, 3);
    println!(
        "NHF015_CALENDAR_TLS_PASS verified_tls13_native_session_calendar_rpc_crud_fold_gap_pagination_cas_readonly_watch_revocation_commit_unknown_no_replay"
    );
}
