use super::*;

#[test]
fn page_freshness_and_expiry_cannot_be_upgraded_by_fresh_calendar_metadata() {
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    let mut stale = page("stale", "");
    stale.freshness = wire::CalendarFreshness::Stale.into();
    stale.fresh_until_time = admit::parse_timestamp("2026-01-01T00:00:00Z", true)
        .unwrap()
        .into();
    capture.push(&stale).unwrap();
    let snapshot = capture.finish(&calendar()).unwrap();
    assert_eq!(snapshot.freshness(), CalendarFreshness::Stale);
    assert_eq!(
        snapshot.expiry_bounds().collect::<Vec<_>>(),
        ["2026-01-01T00:00:00Z"]
    );
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    let mut unavailable = page("unavailable", "");
    unavailable.freshness = wire::CalendarFreshness::Unavailable.into();
    capture.push(&unavailable).unwrap();
    assert_eq!(
        capture.finish(&calendar()).unwrap().freshness(),
        CalendarFreshness::Unavailable
    );
}

#[test]
fn pages_cannot_mix_synthetic_and_observed_provenance_under_one_range() {
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    let mut synthetic = page("synthetic", "");
    synthetic.provenance = wire::CalendarProvenance::Synthetic.into();
    assert!(capture.push(&synthetic).is_err());
    assert!(capture.finish(&calendar()).is_err());
}
const CALENDAR: &str = "homes/synthetic/calendars/id-63616c";
fn event_name(uid: &str) -> String {
    format!(
        "{CALENDAR}/events/id-{}",
        uid.bytes()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>()
    )
}
fn civil_date(value: &str) -> home_wire_contract::google::r#type::Date {
    admit::date_wire(value.parse().unwrap())
}
fn calendar() -> wire::Calendar {
    wire::Calendar {
        name: CALENDAR.into(),
        display_name: "Synthetic capture".into(),
        source_entity: "homes/synthetic/entities/source".into(),
        etag: "W/\"calendar-7\"".into(),
        origin: wire::CalendarOrigin::Local.into(),
        freshness: wire::CalendarFreshness::Fresh.into(),
        provenance: wire::CalendarProvenance::Observed.into(),
        mutation_access: wire::CalendarMutationAccess::Allowed.into(),
        ..Default::default()
    }
}
fn query() -> wire::QueryOccurrencesRequest {
    wire::QueryOccurrencesRequest {
        parent: format!("{CALENDAR}/events/-"),
        page_size: 256,
        window: wire::CalendarWindow {
            time_zone: "UTC".into(),
            bounds: Some(wire::calendar_window::Bounds::Dates(Box::new(
                wire::CalendarDateSpan {
                    start_date: civil_date("2026-11-01").into(),
                    end_date: civil_date("2026-11-08").into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn page(uid: &str, next: &str) -> wire::QueryOccurrencesResponse {
    wire::QueryOccurrencesResponse {
        calendar_etag: calendar().etag,
        freshness: wire::CalendarFreshness::Fresh.into(),
        provenance: wire::CalendarProvenance::Observed.into(),
        next_page_token: next.into(),
        occurrences: vec![wire::Occurrence {
            calendar_event: event_name(uid),
            recurrence_point: wire::CalendarRecurrencePoint {
                value: Some(wire::calendar_recurrence_point::Value::Date(Box::new(
                    civil_date("2026-11-01"),
                ))),
                ..Default::default()
            }
            .into(),
            span: wire::CalendarSpan {
                value: Some(wire::calendar_span::Value::Dates(Box::new(
                    wire::CalendarDateSpan {
                        start_date: civil_date("2026-11-01").into(),
                        end_date: civil_date("2026-11-02").into(),
                        ..Default::default()
                    },
                ))),
                ..Default::default()
            }
            .into(),
            time_zone: "UTC".into(),
            ..Default::default()
        }],
        ..Default::default()
    }
}
#[test]
fn a_range_is_published_only_after_complete_pages_and_matching_final_metadata() {
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    capture.push(&page("a", "next")).unwrap();
    assert!(capture.finish(&calendar()).is_err());
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    capture.push(&page("a", "next")).unwrap();
    assert_eq!(capture.request().unwrap().page_token, "next");
    capture.push(&page("b", "")).unwrap();
    assert!(capture.request().is_none());
    let snapshot = capture.finish(&calendar()).unwrap();
    assert_eq!(snapshot.occurrences().len(), 2);
    assert!(snapshot.query().page_token.is_empty());
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    capture.push(&page("a", "")).unwrap();
    let mut changed = calendar();
    changed.etag = "W/\"calendar-8\"".into();
    assert!(matches!(
        capture.finish(&changed),
        Err(admit::AdmissionError::StaleVersion)
    ));
}
#[test]
fn overlapping_pages_repeated_tokens_and_revision_changes_poison_the_capture() {
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    capture.push(&page("a", "next")).unwrap();
    assert!(capture.push(&page("a", "")).is_err());
    assert!(capture.push(&page("b", "")).is_err());
    assert!(capture.finish(&calendar()).is_err());
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    capture.push(&page("a", "next")).unwrap();
    assert!(matches!(
        capture.push(&page("b", "next")),
        Err(admit::AdmissionError::Sequence)
    ));
    assert!(capture.finish(&calendar()).is_err());
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    let mut stale = page("a", "");
    stale.calendar_etag = "W/\"calendar-8\"".into();
    assert!(matches!(
        capture.push(&stale),
        Err(admit::AdmissionError::StaleVersion)
    ));
    assert!(capture.finish(&calendar()).is_err());
}
#[test]
fn missing_resource_metadata_cannot_be_fabricated_as_a_calendar_snapshot() {
    let mut missing = calendar();
    missing.etag.clear();
    assert!(CalendarCapture::new(&missing, &query()).is_err());
}
#[test]
fn a_nonprogressing_server_is_bounded_even_when_its_cursor_keeps_changing() {
    let mut capture = CalendarCapture::new(&calendar(), &query()).unwrap();
    for i in 0..64 {
        capture
            .push(&page(&format!("event-{i}"), &format!("cursor-{i}")))
            .unwrap();
    }
    assert!(matches!(
        capture.push(&page("last", "")),
        Err(admit::AdmissionError::Capacity)
    ));
    assert!(capture.finish(&calendar()).is_err());
}
