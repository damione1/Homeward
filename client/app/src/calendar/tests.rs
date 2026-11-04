use super::*;

fn parent() -> CalendarName {
    CalendarName::try_from("homes/synthetic/calendars/id-63616c").unwrap()
}
fn key() -> CalendarEventName {
    CalendarEventName::new(parent(), "fold-event".parse().unwrap())
}
fn timed() -> Draft {
    Draft {
        key: Some(key()),
        parent: parent(),
        revision: Some(CalendarEventVersion::new(
            key(),
            home_client_model::api_calendars::CalendarEtag::try_from("W/\"event-current\"")
                .unwrap(),
            home_client_model::api_calendars::CalendarEtag::try_from("W/\"parent-current\"")
                .unwrap(),
        )),
        original: wire::CalendarEvent {
            summary: "Second fold".into(),
            recurrence_rule: Some("FREQ=DAILY;COUNT=2".into()),
            exclusions: vec![wire::CalendarRecurrencePoint {
                value: Some(wire::calendar_recurrence_point::Value::Local(Box::new(
                    admit::parse_civil("2026-11-02T01:15:00").unwrap(),
                ))),
                ..Default::default()
            }],
            ..Default::default()
        },
        temporal: Temporal::Timed {
            start: Box::new(Wall::from_server(
                "2026-11-01T01:15:00",
                "2026-11-01T06:15:00Z",
            )),
            end: Box::new(Wall::from_server(
                "2026-11-01T02:15:00",
                "2026-11-01T07:15:00Z",
            )),
            zone: "America/Toronto".into(),
        },
        phase: EditPhase::Ready,
        existing: true,
    }
}
fn response(request: &wire::ResolveCalendarTimeRequest) -> wire::ResolveCalendarTimeResponse {
    wire::ResolveCalendarTimeResponse {
        local: request.local.clone(),
        time_zone: request.time_zone.clone(),
        resolution: wire::CalendarTimeResolution::Ambiguous.into(),
        candidates: vec![
            wire::CalendarTimeCandidate {
                selected_time: admit::parse_timestamp("2026-11-01T05:15:00Z", true)
                    .unwrap()
                    .into(),
                utc_offset: -14400,
                ..Default::default()
            },
            wire::CalendarTimeCandidate {
                selected_time: admit::parse_timestamp("2026-11-01T06:15:00Z", true)
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
fn editing_text_keeps_the_exact_server_selected_fold_and_recurrence_metadata() {
    let mut draft = timed();
    draft.edit(Field::Summary, "Edited title".into());
    let event = draft.event().unwrap();
    let Some(wire::calendar_span::Value::Times(times)) = event.span.value.as_ref() else {
        panic!("timed span")
    };
    assert_eq!(
        admit::timestamp_text(&times.start_time).unwrap(),
        "2026-11-01T06:15:00Z"
    );
    assert_eq!(
        admit::civil_text(&times.start_local).unwrap(),
        "2026-11-01T01:15:00"
    );
    assert_eq!(event.recurrence_rule, draft.original.recurrence_rule);
    assert_eq!(event.exclusions, draft.original.exclusions);
}
#[test]
fn ambiguous_wall_time_requires_a_choice_and_never_defaults_to_the_first_fold() {
    let mut draft = timed();
    let request = draft.resolution_request(Boundary::Start).unwrap();
    draft
        .resolve(Boundary::Start, &request, &response(&request))
        .unwrap();
    assert!(draft.event().is_none());
    draft.choose(Boundary::Start, true);
    let event = draft.event().unwrap();
    let Some(wire::calendar_span::Value::Times(times)) = event.span.value.as_ref() else {
        panic!("timed span")
    };
    assert_eq!(
        admit::timestamp_text(&times.start_time).unwrap(),
        "2026-11-01T06:15:00Z"
    );
    draft.choose(Boundary::Start, false);
    let event = draft.event().unwrap();
    let Some(wire::calendar_span::Value::Times(times)) = event.span.value.as_ref() else {
        panic!("timed span")
    };
    assert_eq!(
        admit::timestamp_text(&times.start_time).unwrap(),
        "2026-11-01T05:15:00Z"
    );
}
#[test]
fn a_late_resolution_cannot_apply_after_the_zone_or_wall_draft_changed() {
    let mut draft = timed();
    let request = draft.resolution_request(Boundary::Start).unwrap();
    draft.timed(
        Boundary::Start,
        "2026-11-01".into(),
        "01:15:00".into(),
        "Europe/Paris".into(),
    );
    assert!(
        draft
            .resolve(Boundary::Start, &request, &response(&request))
            .is_err()
    );
    assert!(draft.event().is_none());
}
#[test]
fn gaps_and_reversed_intervals_remain_uncommittable_without_normalization() {
    let mut draft = timed();
    draft.timed(
        Boundary::Start,
        "2026-03-08".into(),
        "02:30:00".into(),
        "America/Toronto".into(),
    );
    let request = draft.resolution_request(Boundary::Start).unwrap();
    let response = wire::ResolveCalendarTimeResponse {
        local: request.local.clone(),
        time_zone: request.time_zone.clone(),
        resolution: wire::CalendarTimeResolution::Nonexistent.into(),
        ..Default::default()
    };
    draft.resolve(Boundary::Start, &request, &response).unwrap();
    assert!(draft.event().is_none());
    let Temporal::Timed { start, .. } = &draft.temporal else {
        panic!("timed draft")
    };
    assert_eq!(start.local(), "2026-03-08T02:30:00");
}
#[test]
fn all_day_dates_stay_civil_across_dst_and_reject_empty_or_reversed_ranges() {
    let mut draft = Draft::new(parent(), &"2026-11-01".parse().unwrap());
    draft.edit(Field::Summary, "Civil day".into());
    let event = draft.event().unwrap();
    let Some(wire::calendar_span::Value::Dates(dates)) = event.span.value.as_ref() else {
        panic!("civil span")
    };
    assert_eq!(
        (
            admit::date(&dates.start_date).unwrap().to_string(),
            admit::date(&dates.end_date).unwrap().to_string()
        ),
        ("2026-11-01".to_owned(), "2026-11-02".to_owned())
    );
    draft.date(Boundary::End, "2026-11-01".into());
    assert!(draft.event().is_none());
    draft.date(Boundary::End, "2026-10-31".into());
    assert!(draft.event().is_none());
}

#[test]
fn new_draft_has_no_client_assigned_event_identity() {
    let mut draft = Draft::new(parent(), &"2026-11-01".parse().unwrap());
    draft.edit(Field::Summary, "New event".into());
    assert!(draft.key.is_none() && draft.revision.is_none());
    let event = draft.event().unwrap();
    assert!(event.name.is_empty() && event.etag.is_empty());
}
