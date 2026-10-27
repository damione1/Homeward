use super::*;

fn key() -> EventKey {
    EventKey::new(
        home_client_model::calendars::CalendarId::new("synthetic-calendar".parse().unwrap()),
        "fold-event".parse().unwrap(),
    )
}
fn timed() -> Draft {
    Draft {
        key: key(),
        revision: EventRevision::new(key(), RevisionState::Absent),
        original: wire::Event {
            summary: "Second fold".into(),
            recurrence_rule: Some("FREQ=DAILY;COUNT=2".into()),
            exclusions: vec!["2026-11-02T01:15:00".into()],
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
fn response(request: &wire::ResolveLocalTimeRequest) -> wire::ResolveLocalTimeResponse {
    wire::ResolveLocalTimeResponse {
        id: request.id.clone(),
        local: request.local.clone(),
        zone: request.zone.clone(),
        resolution: wire::LocalTimeResolution::Ambiguous.into(),
        candidates: vec![
            wire::LocalTimeCandidate {
                instant: "2026-11-01T05:15:00Z".into(),
                offset_seconds: -14400,
                ..Default::default()
            },
            wire::LocalTimeCandidate {
                instant: "2026-11-01T06:15:00Z".into(),
                offset_seconds: -18000,
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
    let Some(wire::span::Value::Times(times)) = event.span.value.as_ref() else {
        panic!("timed span")
    };
    assert_eq!(times.start_instant, "2026-11-01T06:15:00Z");
    assert_eq!(times.anchor_local, "2026-11-01T01:15:00");
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
    let Some(wire::span::Value::Times(times)) = event.span.value.as_ref() else {
        panic!("timed span")
    };
    assert_eq!(times.start_instant, "2026-11-01T06:15:00Z");
    draft.choose(Boundary::Start, false);
    let event = draft.event().unwrap();
    let Some(wire::span::Value::Times(times)) = event.span.value.as_ref() else {
        panic!("timed span")
    };
    assert_eq!(times.start_instant, "2026-11-01T05:15:00Z");
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
    let response = wire::ResolveLocalTimeResponse {
        id: request.id.clone(),
        local: request.local.clone(),
        zone: request.zone.clone(),
        resolution: wire::LocalTimeResolution::Nonexistent.into(),
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
    let mut draft = Draft::new(key(), &"2026-11-01".parse().unwrap());
    draft.edit(Field::Summary, "Civil day".into());
    let event = draft.event().unwrap();
    let Some(wire::span::Value::Dates(dates)) = event.span.value.as_ref() else {
        panic!("civil span")
    };
    assert_eq!(
        (&dates.start, &dates.end_exclusive),
        (&"2026-11-01".to_owned(), &"2026-11-02".to_owned())
    );
    draft.date(Boundary::End, "2026-11-01".into());
    assert!(draft.event().is_none());
    draft.date(Boundary::End, "2026-10-31".into());
    assert!(draft.event().is_none());
}
