use buffa::{EnumValue, Message};
use home_client_model::{
    api::HomeName,
    api_calendars::{CalendarEventName, CalendarName},
};
use home_wire_contract::{calendars_contract as admit, home::api::v2 as wire};
const CALENDAR: &str = "homes/synthetic/calendars/id-63616c";
const EVENT: &str = "homes/synthetic/calendars/id-63616c/events/id-6576656e74";
fn date(value: &str) -> home_wire_contract::google::r#type::Date {
    admit::date_wire(value.parse().unwrap())
}
fn span() -> wire::CalendarSpan {
    wire::CalendarSpan {
        value: Some(wire::calendar_span::Value::Dates(Box::new(
            wire::CalendarDateSpan {
                start_date: date("2026-11-01").into(),
                end_date: date("2026-11-02").into(),
                ..Default::default()
            },
        ))),
        ..Default::default()
    }
}
fn calendar() -> wire::Calendar {
    wire::Calendar {
        name: CALENDAR.into(),
        display_name: "Synthetic calendar".into(),
        source_entity: "homes/synthetic/entities/source".into(),
        etag: "W/\"opaque-calendar\"".into(),
        origin: wire::CalendarOrigin::Local.into(),
        freshness: wire::CalendarFreshness::Fresh.into(),
        provenance: wire::CalendarProvenance::Synthetic.into(),
        mutation_access: wire::CalendarMutationAccess::Allowed.into(),
        ..Default::default()
    }
}
fn event() -> wire::CalendarEvent {
    wire::CalendarEvent {
        name: EVENT.into(),
        summary: "Synthetic event".into(),
        span: span().into(),
        etag: "W/\"opaque-event\"".into(),
        calendar_etag: calendar().etag,
        ..Default::default()
    }
}
fn query() -> wire::QueryOccurrencesRequest {
    wire::QueryOccurrencesRequest {
        parent: format!("{CALENDAR}/events/-"),
        page_size: 1000,
        window: wire::CalendarWindow {
            time_zone: "America/Toronto".into(),
            bounds: Some(wire::calendar_window::Bounds::Dates(Box::new(
                wire::CalendarDateSpan {
                    start_date: date("2026-11-01").into(),
                    end_date: date("2026-11-08").into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn page() -> wire::QueryOccurrencesResponse {
    wire::QueryOccurrencesResponse {
        calendar_etag: calendar().etag,
        freshness: wire::CalendarFreshness::Fresh.into(),
        provenance: wire::CalendarProvenance::Synthetic.into(),
        occurrences: vec![wire::Occurrence {
            calendar_event: EVENT.into(),
            recurrence_point: wire::CalendarRecurrencePoint {
                value: Some(wire::calendar_recurrence_point::Value::Date(Box::new(
                    date("2026-11-01"),
                ))),
                ..Default::default()
            }
            .into(),
            span: span().into(),
            time_zone: "America/Toronto".into(),
            ..Default::default()
        }],
        ..Default::default()
    }
}
#[test]
fn canonical_resource_names_and_opaque_etags_replace_revision_decoding() {
    let name = CalendarName::try_from(CALENDAR).unwrap();
    let current = admit::admit_calendar(&calendar(), &name).unwrap();
    assert!(admit::calendar_editable(&current));
    let event_name = CalendarEventName::try_from(EVENT).unwrap();
    assert!(admit::admit_event(&event(), &event_name).is_ok());
    for bad in ["", "opaque-unquoted", "W/\"bad\nvalue\"", "W/\"\""] {
        let mut value = event();
        value.etag = bad.into();
        assert!(admit::admit_event(&value, &event_name).is_err());
    }
    let mut foreign = calendar();
    foreign.name = "homes/foreign/calendars/id-63616c".into();
    assert!(admit::admit_calendar(&foreign, &name).is_err());
    let mut foreign = event();
    foreign.name = "homes/synthetic/calendars/id-6f74686572/events/id-6576656e74".into();
    assert!(admit::admit_event(&foreign, &event_name).is_err());
}
#[test]
fn origin_access_quality_and_provenance_are_independent_and_unknowns_fail_closed() {
    let name = CalendarName::try_from(CALENDAR).unwrap();
    for origin in [
        wire::CalendarOrigin::RemoteIcs,
        wire::CalendarOrigin::Calculated,
    ] {
        let mut value = calendar();
        value.origin = origin.into();
        value.mutation_access = wire::CalendarMutationAccess::Denied.into();
        let value = admit::admit_calendar(&value, &name).unwrap();
        assert!(!admit::calendar_editable(&value));
    }
    let mut value = calendar();
    value.origin = wire::CalendarOrigin::RemoteIcs.into();
    assert!(admit::admit_calendar(&value, &name).is_err());
    let mut value = calendar();
    value.origin = EnumValue::Unknown(93);
    assert!(admit::admit_calendar(&value, &name).is_err());
    let mut value = calendar();
    value.mutation_access = EnumValue::Unknown(93);
    assert!(admit::admit_calendar(&value, &name).is_err());
    let mut value = calendar();
    value.provenance = EnumValue::Unknown(93);
    assert!(admit::admit_calendar(&value, &name).is_err());
    let mut bytes = calendar().encode_to_vec();
    bytes.extend([0xf8, 0x07, 1]);
    let value = wire::Calendar::decode_from_slice(&bytes).unwrap();
    assert!(admit::admit_calendar(&value, &name).is_err());
}
#[test]
fn discovery_keeps_absent_sources_without_inventing_resource_lifecycle() {
    let home = HomeName::try_from("homes/synthetic").unwrap();
    let source = wire::CalendarSource {
        calendar: CALENDAR.into(),
        source_entity: calendar().source_entity,
        origin: wire::CalendarOrigin::Local.into(),
        mutation_access: wire::CalendarMutationAccess::Allowed.into(),
        provenance: wire::CalendarProvenance::Synthetic.into(),
        ..Default::default()
    };
    assert!(admit::admit_source(&source, &home).is_ok());
    let mut response = wire::QueryCalendarSourcesResponse {
        calendar_sources: vec![source.clone(), source.clone()],
        ..Default::default()
    };
    assert!(admit::admit_sources(&response, &home).is_err());
    response.calendar_sources.pop();
    assert!(admit::admit_sources(&response, &home).is_ok());
    let mut wrong = source;
    wrong.source_entity = "homes/foreign/entities/source".into();
    assert!(admit::admit_source(&wrong, &home).is_err());
}
#[test]
fn bounded_occurrence_pages_preserve_original_identity_and_reject_mixed_captures() {
    let request = admit::admit_query(&query()).unwrap();
    let version = admit::calendar_version(&calendar()).unwrap();
    assert!(admit::admit_page(&page(), &request, &version).is_ok());
    let mut duplicate = page();
    duplicate.occurrences.push(duplicate.occurrences[0].clone());
    assert!(admit::admit_page(&duplicate, &request, &version).is_err());
    let mut stale = page();
    stale.calendar_etag = "W/\"different\"".into();
    assert!(matches!(
        admit::admit_page(&stale, &request, &version),
        Err(admit::AdmissionError::StaleVersion)
    ));
    let mut foreign = page();
    foreign.occurrences[0].calendar_event =
        "homes/synthetic/calendars/id-6f74686572/events/id-6576656e74".into();
    assert!(admit::admit_page(&foreign, &request, &version).is_err());
    let mut oversized = page();
    oversized.occurrences = vec![oversized.occurrences[0].clone(); 257];
    assert!(admit::admit_page(&oversized, &request, &version).is_err());
    let mut invalid = query();
    invalid.page_size = -1;
    assert!(admit::admit_query(&invalid).is_err());
}
fn resolution_request() -> wire::ResolveCalendarTimeRequest {
    wire::ResolveCalendarTimeRequest {
        name: CALENDAR.into(),
        local: admit::parse_civil("2026-11-01T01:30:00").unwrap().into(),
        time_zone: "America/Toronto".into(),
        ..Default::default()
    }
}
fn resolution(request: &wire::ResolveCalendarTimeRequest) -> wire::ResolveCalendarTimeResponse {
    wire::ResolveCalendarTimeResponse {
        local: request.local.clone(),
        time_zone: request.time_zone.clone(),
        resolution: wire::CalendarTimeResolution::Ambiguous.into(),
        candidates: vec![
            wire::CalendarTimeCandidate {
                selected_time: admit::parse_timestamp("2026-11-01T05:30:00Z", true)
                    .unwrap()
                    .into(),
                utc_offset: -14400,
                ..Default::default()
            },
            wire::CalendarTimeCandidate {
                selected_time: admit::parse_timestamp("2026-11-01T06:30:00Z", true)
                    .unwrap()
                    .into(),
                utc_offset: -18000,
                ..Default::default()
            },
        ],
        ..Default::default()
    }
}
#[test]
fn server_resolution_preserves_fold_choices_and_rejects_stale_or_incoherent_candidates() {
    let request = resolution_request();
    let mut response = resolution(&request);
    assert_eq!(
        admit::admit_resolution(&response, &request)
            .unwrap()
            .wire()
            .candidates
            .len(),
        2
    );
    response.candidates.swap(0, 1);
    assert!(admit::admit_resolution(&response, &request).is_err());
    response.candidates.swap(0, 1);
    response.candidates[0].utc_offset = i32::MIN;
    assert!(admit::admit_resolution(&response, &request).is_err());
    response = resolution(&request);
    response.local = admit::parse_civil("2026-11-01T01:31:00").unwrap().into();
    assert!(admit::admit_resolution(&response, &request).is_err());
    let mut gap = request.clone();
    gap.local = admit::parse_civil("2026-03-08T02:30:00").unwrap().into();
    response.local = gap.local.clone();
    response.candidates.clear();
    response.resolution = wire::CalendarTimeResolution::Nonexistent.into();
    assert!(admit::admit_resolution(&response, &gap).is_ok());
    response.resolution = EnumValue::Unknown(32);
    assert!(admit::admit_resolution(&response, &gap).is_err());
}
#[test]
fn imported_resolved_recurrence_points_preserve_original_fold_and_remain_read_only() {
    let value = wire::CalendarRecurrencePoint {
        value: Some(wire::calendar_recurrence_point::Value::Resolved(Box::new(
            wire::CalendarResolvedRecurrencePoint {
                local: admit::parse_civil("2026-11-01T01:30:00").unwrap().into(),
                selected_time: admit::parse_timestamp("2026-11-01T06:30:00Z", true)
                    .unwrap()
                    .into(),
                ..Default::default()
            },
        ))),
        ..Default::default()
    };
    let admitted = admit::point(&value).unwrap();
    assert!(admitted.instant().is_some());
    let mut event = event();
    event.additions = vec![value];
    assert!(admit::event_content(&event, true).is_err());
}
#[test]
fn watch_frames_require_positive_signed_sequence_and_explicit_closed_kinds() {
    let mut frame = wire::WatchCalendarResponse {
        sequence: 1,
        frame: Some(wire::watch_calendar_response::Frame::Ready(Box::default())),
        ..Default::default()
    };
    assert!(matches!(
        admit::admit_watch(&frame),
        Ok((1, admit::WatchFrame::Ready))
    ));
    frame.sequence = -1;
    assert!(admit::admit_watch(&frame).is_err());
    frame.sequence = 2;
    frame.frame = Some(wire::watch_calendar_response::Frame::Changed(Box::new(
        wire::CalendarChanged {
            etag: calendar().etag,
            ..Default::default()
        },
    )));
    assert!(matches!(
        admit::admit_watch(&frame),
        Ok((2, admit::WatchFrame::Changed(_)))
    ));
    frame.frame = Some(wire::watch_calendar_response::Frame::ResyncRequired(
        Box::default(),
    ));
    assert!(matches!(
        admit::admit_watch(&frame),
        Ok((2, admit::WatchFrame::Resync))
    ));
}
