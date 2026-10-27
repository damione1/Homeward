use super::*;
fn id() -> wire::CalendarId {
    wire::CalendarId {
        entity_id: "synthetic-only".into(),
        ..Default::default()
    }
}
fn revision(number: u64) -> wire::Revision {
    wire::Revision {
        resource: wire::EventKey {
            calendar: id().into(),
            ..Default::default()
        }
        .into(),
        presence: wire::Presence::Present.into(),
        number,
        ..Default::default()
    }
}
fn calendar() -> wire::Calendar {
    wire::Calendar {
        id: id().into(),
        revision: revision(7).into(),
        name: Some("Synthetic capture".into()),
        origin: wire::Origin::Local.into(),
        quality: wire::Quality::Fresh.into(),
        mutation_access: wire::MutationAccess::Allowed.into(),
        ..Default::default()
    }
}
fn query() -> wire::QueryOccurrencesRequest {
    wire::QueryOccurrencesRequest {
        id: id().into(),
        page_size: 1000,
        window: wire::Window {
            zone: "UTC".into(),
            bounds: Some(wire::window::Bounds::Dates(Box::new(wire::DateSpan {
                start: "2026-11-01".into(),
                end_exclusive: "2026-11-08".into(),
                ..Default::default()
            }))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn page(uid: &str, next: &str) -> wire::QueryOccurrencesResponse {
    wire::QueryOccurrencesResponse {
        revision: revision(7).into(),
        quality: wire::Quality::Fresh.into(),
        next_page_token: next.into(),
        occurrences: vec![wire::Occurrence {
            key: wire::EventKey {
                calendar: id().into(),
                uid: uid.into(),
                ..Default::default()
            }
            .into(),
            recurrence_id: "2026-11-01".into(),
            span: wire::Span {
                value: Some(wire::span::Value::Dates(Box::new(wire::DateSpan {
                    start: "2026-11-01".into(),
                    end_exclusive: "2026-11-02".into(),
                    ..Default::default()
                }))),
                ..Default::default()
            }
            .into(),
            zone: "UTC".into(),
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
    changed.revision = revision(8).into();
    assert!(matches!(
        capture.finish(&changed),
        Err(admit::AdmissionError::StaleRevision)
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
    stale.revision = revision(8).into();
    assert!(matches!(
        capture.push(&stale),
        Err(admit::AdmissionError::StaleRevision)
    ));
    assert!(capture.finish(&calendar()).is_err());
}
#[test]
fn an_absent_calendar_stays_explicit_without_querying_or_inventing_events() {
    let mut absent = calendar();
    absent.name = None;
    absent.revision = wire::Revision {
        presence: wire::Presence::Absent.into(),
        number: 0,
        ..revision(0)
    }
    .into();
    let capture = CalendarCapture::new(&absent, &query()).unwrap();
    assert!(capture.request().is_none());
    let snapshot = capture.finish(&absent).unwrap();
    assert!(snapshot.occurrences().is_empty());
    assert_eq!(
        snapshot.calendar().revision.presence,
        wire::Presence::Absent
    );
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
