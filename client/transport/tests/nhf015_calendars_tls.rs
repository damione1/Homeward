//! Opt-in TLS13/Go integration. Ordinary cargo runs do not claim this fixture.
use home_client_transport::{AccessToken, Connection};
use home_wire_contract::{
    calendars_contract as admit,
    home::{api::v2 as wire, api::v2 as auth},
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
fn request_id(n: u32) -> String {
    format!("12345678-1234-4000-8000-{n:012x}")
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
    let homes = connection
        .home()
        .list_homes(
            wire::ListHomesRequest {
                page_size: 2,
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert_eq!(homes.homes.len(), 1);
    assert!(homes.next_page_token.is_empty());
    let home = home_client_model::api::HomeName::try_from(homes.homes[0].name.as_str()).unwrap();
    let catalog = api
        .query_calendar_sources(
            wire::QueryCalendarSourcesRequest {
                parent: home.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_sources(&catalog, &home).unwrap();
    assert_eq!(catalog.calendar_sources.len(), 3);
    let source = |native: &str| {
        catalog
            .calendar_sources
            .iter()
            .find(|source| {
                home_client_model::api_calendars::CalendarName::try_from(source.calendar.as_str())
                    .is_ok_and(|name| name.native_id().entity().as_str() == native)
            })
            .unwrap()
            .calendar
            .clone()
    };
    let local = home_client_model::api_calendars::CalendarName::try_from(
        source(&fixture.entities.local).as_str(),
    )
    .unwrap();
    let current = api
        .get_calendar(
            wire::GetCalendarRequest {
                name: local.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert!(admit::calendar_editable(
        &admit::admit_calendar(&current, &local).unwrap()
    ));
    let mut watch = api
        .watch_calendar(
            wire::WatchCalendarRequest {
                name: local.as_str().into(),
                ..Default::default()
            },
            &token,
            Duration::from_secs(40),
            &cancel,
        )
        .await
        .unwrap();
    let ready = watch.next().await.unwrap().unwrap();
    assert_eq!(
        admit::admit_watch(&ready).unwrap(),
        (1, admit::WatchFrame::Ready)
    );
    let resolution_request = wire::ResolveCalendarTimeRequest {
        name: local.as_str().into(),
        local: admit::parse_civil("2026-11-01T01:30:00").unwrap().into(),
        time_zone: "America/Toronto".into(),
        ..Default::default()
    };
    let resolution = api
        .resolve_calendar_time(resolution_request.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    admit::admit_resolution(&resolution, &resolution_request).unwrap();
    assert_eq!(
        resolution.resolution,
        wire::CalendarTimeResolution::Ambiguous
    );
    let second = resolution.candidates[1].selected_time.clone();
    let event = wire::CalendarEvent {
        summary: "Synthetic explicit fold".into(),
        span: wire::CalendarSpan {
            value: Some(wire::calendar_span::Value::Times(Box::new(
                wire::CalendarTimeSpan {
                    start_local: resolution_request.local.clone(),
                    end_local: admit::parse_civil("2026-11-01T02:30:00").unwrap().into(),
                    time_zone: resolution_request.time_zone.clone(),
                    start_time: second.clone(),
                    end_time: admit::parse_timestamp("2026-11-01T07:30:00Z", true)
                        .unwrap()
                        .into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let created = api
        .create_calendar_event(
            wire::CreateCalendarEventRequest {
                parent: local.as_str().into(),
                calendar_event: event.clone().into(),
                request_id: request_id(1),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let key = home_client_model::api_calendars::CalendarEventName::try_from(created.name.as_str())
        .unwrap();
    admit::admit_event(&created, &key).unwrap();
    let changed = watch.next().await.unwrap().unwrap();
    assert!(matches!(
        admit::admit_watch(&changed).unwrap(),
        (2, admit::WatchFrame::Changed(_))
    ));
    let read = api
        .get_calendar_event(
            wire::GetCalendarEventRequest {
                name: key.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let Some(wire::calendar_span::Value::Times(span)) = &read.span.value else {
        panic!("timed span changed kind")
    };
    assert_eq!(span.start_time, second);
    assert_eq!(
        admit::civil_text(&span.end_local).unwrap(),
        "2026-11-01T02:30:00"
    );
    let mask = || buffa_types::google::protobuf::FieldMask {
        paths: vec!["summary".into()],
        ..Default::default()
    };
    let mut stale = read.clone();
    stale.etag = "W/\"different-opaque-token\"".into();
    let failure = api
        .update_calendar_event(
            wire::UpdateCalendarEventRequest {
                calendar_event: stale.into(),
                update_mask: mask().into(),
                request_id: request_id(2),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap_err();
    assert_eq!(
        failure.api_failure().unwrap().unwrap().reason(),
        wire::ApiErrorReason::EtagMismatch
    );
    for (index, foreign) in [&fixture.entities.remote, &fixture.entities.calculated]
        .iter()
        .enumerate()
    {
        let failure = api
            .create_calendar_event(
                wire::CreateCalendarEventRequest {
                    parent: source(foreign),
                    calendar_event: event.clone().into(),
                    request_id: request_id(3 + index as u32),
                    ..Default::default()
                },
                &token,
                timeout,
                &cancel,
            )
            .await
            .unwrap_err();
        assert_eq!(
            failure.api_failure().unwrap().unwrap().reason(),
            wire::ApiErrorReason::SourceReadOnly
        );
    }
    let mut query = wire::QueryOccurrencesRequest {
        parent: format!("{}/events/-", local.as_str()),
        page_size: 1,
        window: wire::CalendarWindow {
            time_zone: "America/Toronto".into(),
            bounds: Some(wire::calendar_window::Bounds::Dates(Box::new(
                wire::CalendarDateSpan {
                    start_date: admit::date_wire("2026-11-01".parse().unwrap()).into(),
                    end_date: admit::date_wire("2026-11-08".parse().unwrap()).into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let calendar = api
        .get_calendar(
            wire::GetCalendarRequest {
                name: local.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    let version = admit::calendar_version(&calendar).unwrap();
    let page = api
        .query_occurrences(query.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    admit::admit_page(&page, &admit::admit_query(&query).unwrap(), &version).unwrap();
    assert_eq!(page.occurrences.len(), 1);
    assert!(!page.next_page_token.is_empty());
    query.page_token = page.next_page_token.clone();
    let next = api
        .query_occurrences(query.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    admit::admit_page(&next, &admit::admit_query(&query).unwrap(), &version).unwrap();
    assert_ne!(
        (
            &next.occurrences[0].calendar_event,
            admit::point_key(&next.occurrences[0].recurrence_point).unwrap()
        ),
        (
            &page.occurrences[0].calendar_event,
            admit::point_key(&page.occurrences[0].recurrence_point).unwrap()
        )
    );
    let mut sequence = 0;
    control(&root, &mut sequence, Control::LoseNextReply).await;
    let mut updated = read.clone();
    updated.summary = "Commit without reply".into();
    let failure = api
        .update_calendar_event(
            wire::UpdateCalendarEventRequest {
                calendar_event: updated.into(),
                update_mask: mask().into(),
                request_id: request_id(5),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap_err();
    assert_eq!(
        failure.api_failure().unwrap().unwrap().reason(),
        wire::ApiErrorReason::CommitOutcomeUnknown
    );
    let changed = watch.next().await.unwrap().unwrap();
    assert_eq!(admit::admit_watch(&changed).unwrap().0, 3);
    let committed = api
        .get_calendar_event(
            wire::GetCalendarEventRequest {
                name: key.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    admit::admit_event(&committed, &key).unwrap();
    assert_ne!(committed.etag, read.etag);
    assert_eq!(committed.summary, "Commit without reply");
    let failure = api
        .query_occurrences(query, &token, timeout, &cancel)
        .await
        .unwrap_err();
    assert_eq!(
        failure.api_failure().unwrap().unwrap().reason(),
        wire::ApiErrorReason::ResyncRequired
    );
    control(&root, &mut sequence, Control::RevokeWrite).await;
    let calendar = api
        .get_calendar(
            wire::GetCalendarRequest {
                name: local.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert!(!admit::calendar_editable(
        &admit::admit_calendar(&calendar, &local).unwrap()
    ));
    let delete = wire::DeleteCalendarEventRequest {
        name: key.as_str().into(),
        etag: committed.etag.clone(),
        request_id: request_id(6),
        ..Default::default()
    };
    let failure = api
        .delete_calendar_event(delete.clone(), &token, timeout, &cancel)
        .await
        .unwrap_err();
    assert_eq!(
        failure.api_failure().unwrap().unwrap().reason(),
        wire::ApiErrorReason::AccessDenied
    );
    control(&root, &mut sequence, Control::AllowWrite).await;
    control(&root, &mut sequence, Control::RevokeRead).await;
    assert!(watch.next().await.is_err());
    drop(watch);
    control(&root, &mut sequence, Control::AllowRead).await;
    let mut fresh = api
        .watch_calendar(
            wire::WatchCalendarRequest {
                name: local.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert_eq!(
        admit::admit_watch(&fresh.next().await.unwrap().unwrap()).unwrap(),
        (1, admit::WatchFrame::Ready)
    );
    drop(fresh);
    let reread = api
        .get_calendar_event(
            wire::GetCalendarEventRequest {
                name: key.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap();
    assert_eq!(reread.etag, committed.etag);
    let gap_request = wire::ResolveCalendarTimeRequest {
        local: admit::parse_civil("2026-03-08T02:30:00").unwrap().into(),
        ..resolution_request
    };
    let gap = api
        .resolve_calendar_time(gap_request.clone(), &token, timeout, &cancel)
        .await
        .unwrap();
    admit::admit_resolution(&gap, &gap_request).unwrap();
    assert_eq!(gap.resolution, wire::CalendarTimeResolution::Nonexistent);
    assert!(gap.candidates.is_empty());
    api.delete_calendar_event(delete, &token, timeout, &cancel)
        .await
        .unwrap();
    let missing = api
        .get_calendar_event(
            wire::GetCalendarEventRequest {
                name: key.as_str().into(),
                ..Default::default()
            },
            &token,
            timeout,
            &cancel,
        )
        .await
        .unwrap_err();
    assert_eq!(
        missing.api_failure().unwrap().unwrap().reason(),
        wire::ApiErrorReason::ResourceNotFound
    );
    println!(
        "NHF015_CALENDAR_TLS_PASS v2_crud_fold_gap_pages_cas_readonly_revocation_commit_unknown_no_replay"
    );
}
