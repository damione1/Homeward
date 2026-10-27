use buffa::EnumValue;
use home_client_model::calendars::{CalendarId, EventKey};
use home_wire_contract::{calendars_contract as admit, home::calendars::v1 as wire};

fn id(value: &str) -> wire::CalendarId {
    wire::CalendarId {
        entity_id: value.into(),
        ..Default::default()
    }
}
fn domain(value: &str) -> CalendarId {
    CalendarId::new(value.parse().unwrap())
}
fn revision(calendar: &str, uid: &str, number: u64) -> wire::Revision {
    wire::Revision {
        resource: wire::EventKey {
            calendar: id(calendar).into(),
            uid: uid.into(),
            ..Default::default()
        }
        .into(),
        presence: wire::Presence::Present.into(),
        number,
        ..Default::default()
    }
}
fn calendar(value: &str) -> wire::Calendar {
    wire::Calendar {
        id: id(value).into(),
        origin: wire::Origin::Local.into(),
        name: Some("Synthetic calendar".into()),
        revision: revision(value, "", 3).into(),
        quality: wire::Quality::Fresh.into(),
        provenance: wire::DataProvenance::Synthetic.into(),
        mutation_access: wire::MutationAccess::Allowed.into(),
        ..Default::default()
    }
}
fn dates(start: &str, end: &str) -> wire::DateSpan {
    wire::DateSpan {
        start: start.into(),
        end_exclusive: end.into(),
        ..Default::default()
    }
}
fn event_request() -> wire::MutateEventRequest {
    wire::MutateEventRequest {
        key: wire::EventKey {
            calendar: id("one").into(),
            uid: "event".into(),
            ..Default::default()
        }
        .into(),
        mutation: wire::Mutation::Update.into(),
        expected_revision: revision("one", "event", 8).into(),
        value: wire::Event {
            summary: "Draft".into(),
            span: wire::Span {
                value: Some(wire::span::Value::Dates(Box::new(dates(
                    "2026-11-01",
                    "2026-11-02",
                )))),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
#[test]
fn mutation_admission_requires_current_exact_resource_and_explicit_local_access() {
    let local = admit::admit_calendar(&calendar("one"), &domain("one")).unwrap();
    let key = EventKey::new(domain("one"), "event".parse().unwrap());
    let previous = admit::event_revision(&revision("one", "event", 8), &key).unwrap();
    let request = event_request();
    assert!(admit::admit_event_mutation(&request, &local, &previous).is_ok());
    let mut stale = request.clone();
    stale.expected_revision = revision("one", "event", 7).into();
    assert!(matches!(
        admit::admit_event_mutation(&stale, &local, &previous),
        Err(admit::AdmissionError::StaleRevision)
    ));
    let mut foreign = request.clone();
    foreign.key.as_option_mut().unwrap().calendar = id("other").into();
    assert!(matches!(
        admit::admit_event_mutation(&foreign, &local, &previous),
        Err(admit::AdmissionError::ForeignResource)
    ));
    for origin in [wire::Origin::RemoteIcs, wire::Origin::Calculated] {
        let mut read_only = calendar("one");
        read_only.origin = origin.into();
        let read_only = admit::admit_calendar(&read_only, &domain("one")).unwrap();
        assert!(admit::admit_event_mutation(&request, &read_only, &previous).is_err());
    }
    let mut invalid = request.clone();
    invalid.value.as_option_mut().unwrap().summary.clear();
    assert!(admit::admit_event_mutation(&invalid, &local, &previous).is_err());
    let mut duplicate = request.clone();
    duplicate.value.as_option_mut().unwrap().exclusions =
        vec!["2026-11-01".into(), "2026-11-01".into()];
    assert!(admit::admit_event_mutation(&duplicate, &local, &previous).is_err());
    let mut delete = request;
    delete.mutation = wire::Mutation::Delete.into();
    assert!(admit::admit_event_mutation(&delete, &local, &previous).is_err());
    delete.value = Default::default();
    assert!(admit::admit_event_mutation(&delete, &local, &previous).is_ok());
}
#[test]
fn receipts_cannot_acknowledge_a_different_event_operation_or_old_revision() {
    let local = admit::admit_calendar(&calendar("one"), &domain("one")).unwrap();
    let key = EventKey::new(domain("one"), "event".parse().unwrap());
    let previous = admit::event_revision(&revision("one", "event", 8), &key).unwrap();
    let previous_calendar =
        admit::calendar_revision(&revision("one", "", 3), &domain("one")).unwrap();
    let request = admit::admit_event_mutation(&event_request(), &local, &previous).unwrap();
    let receipt = wire::MutationReceipt {
        revision: revision("one", "event", 9).into(),
        calendar_revision: revision("one", "", 4).into(),
        ..Default::default()
    };
    assert!(admit::admit_event_receipt(&receipt, &request, &previous_calendar).is_ok());
    let mut bad = receipt.clone();
    bad.revision = revision("other", "event", 9).into();
    assert!(admit::admit_event_receipt(&bad, &request, &previous_calendar).is_err());
    let mut bad = receipt.clone();
    bad.revision = revision("one", "event", 8).into();
    assert!(admit::admit_event_receipt(&bad, &request, &previous_calendar).is_err());
    let mut bad = receipt.clone();
    bad.revision.as_option_mut().unwrap().presence = wire::Presence::Removed.into();
    assert!(admit::admit_event_receipt(&bad, &request, &previous_calendar).is_err());
    let mut bad = receipt;
    bad.calendar_revision = revision("one", "", 3).into();
    assert!(admit::admit_event_receipt(&bad, &request, &previous_calendar).is_err());
}
#[test]
fn calendar_crud_preserves_cas_presence_and_tombstones() {
    let local = admit::admit_calendar(&calendar("one"), &domain("one")).unwrap();
    let request = wire::MutateCalendarRequest {
        id: id("one").into(),
        mutation: wire::Mutation::Update.into(),
        expected_revision: revision("one", "", 3).into(),
        name: "Renamed".into(),
        ..Default::default()
    };
    let admitted = admit::admit_calendar_mutation(&request, &local).unwrap();
    let receipt = wire::MutationReceipt {
        revision: revision("one", "", 4).into(),
        calendar_revision: revision("one", "", 4).into(),
        ..Default::default()
    };
    assert!(admit::admit_calendar_receipt(&receipt, &admitted).is_ok());
    let mut bad = receipt.clone();
    bad.calendar_revision = revision("other", "", 4).into();
    assert!(admit::admit_calendar_receipt(&bad, &admitted).is_err());
    let mut create = request.clone();
    create.mutation = wire::Mutation::Create.into();
    assert!(admit::admit_calendar_mutation(&create, &local).is_err());
    let mut absent = calendar("one");
    absent.name = None;
    absent.revision = wire::Revision {
        presence: wire::Presence::Absent.into(),
        number: 0,
        ..revision("one", "", 0)
    }
    .into();
    create.expected_revision = absent.revision.clone();
    assert!(
        admit::admit_calendar_mutation(
            &create,
            &admit::admit_calendar(&absent, &domain("one")).unwrap()
        )
        .is_ok()
    );
    let mut removed = absent;
    removed.revision = wire::Revision {
        presence: wire::Presence::Removed.into(),
        ..revision("one", "", 5)
    }
    .into();
    create.expected_revision = removed.revision.clone();
    assert!(
        admit::admit_calendar_mutation(
            &create,
            &admit::admit_calendar(&removed, &domain("one")).unwrap()
        )
        .is_err()
    );
    let mut delete = request;
    delete.mutation = wire::Mutation::Delete.into();
    delete.name.clear();
    let delete = admit::admit_calendar_mutation(&delete, &local).unwrap();
    assert!(admit::admit_calendar_receipt(&receipt, &delete).is_err());
    let mut removed_receipt = receipt;
    removed_receipt.revision.as_option_mut().unwrap().presence = wire::Presence::Removed.into();
    removed_receipt.calendar_revision = removed_receipt.revision.clone();
    assert!(admit::admit_calendar_receipt(&removed_receipt, &delete).is_ok());
}
fn request() -> wire::QueryOccurrencesRequest {
    wire::QueryOccurrencesRequest {
        id: id("synthetic-a").into(),
        page_size: 100,
        window: wire::Window {
            zone: "America/Toronto".into(),
            bounds: Some(wire::window::Bounds::Dates(Box::new(dates(
                "2026-11-01",
                "2026-11-08",
            )))),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
fn page() -> wire::QueryOccurrencesResponse {
    wire::QueryOccurrencesResponse {
        revision: revision("synthetic-a", "", 3).into(),
        quality: wire::Quality::Fresh.into(),
        provenance: wire::DataProvenance::Synthetic.into(),
        occurrences: vec![wire::Occurrence {
            key: wire::EventKey {
                calendar: id("synthetic-a").into(),
                uid: "event".into(),
                ..Default::default()
            }
            .into(),
            recurrence_id: "2026-11-01".into(),
            zone: "America/Toronto".into(),
            summary: "Civil event".into(),
            span: wire::Span {
                value: Some(wire::span::Value::Dates(Box::new(dates(
                    "2026-11-01",
                    "2026-11-02",
                )))),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }],
        ..Default::default()
    }
}
#[test]
fn identities_and_unknown_metadata_cannot_grant_mutation_or_alias_resources() {
    let mut value = calendar("opaque ID beyond an alias");
    let target = domain("opaque ID beyond an alias");
    assert!(admit::calendar_editable(
        &admit::admit_calendar(&value, &target).unwrap()
    ));
    for origin in [wire::Origin::RemoteIcs, wire::Origin::Calculated] {
        value.origin = origin.into();
        assert!(!admit::calendar_editable(
            &admit::admit_calendar(&value, &target).unwrap()
        ));
    }
    value.origin = wire::Origin::Local.into();
    value.mutation_access = wire::MutationAccess::Unspecified.into();
    assert!(!admit::calendar_editable(
        &admit::admit_calendar(&value, &target).unwrap()
    ));
    value.origin = EnumValue::Unknown(93);
    assert!(admit::admit_calendar(&value, &target).is_err());
    assert!(admit::admit_calendar(&calendar("foreign"), &target).is_err());
    let long = "opaque".repeat(100);
    assert!(admit::admit_calendar(&calendar(&long), &domain(&long)).is_ok());
    let mut catalog = wire::ListCalendarsResponse {
        calendars: vec![calendar("one"), calendar("one")],
        ..Default::default()
    };
    assert!(admit::admit_catalog(&catalog).is_err());
    catalog.calendars.pop();
    assert!(admit::admit_catalog(&catalog).is_ok());
}
#[test]
fn revision_presence_and_identity_are_checked_before_use() {
    let key = EventKey::new(domain("one"), "event".parse().unwrap());
    assert!(admit::event_revision(&revision("one", "event", 8), &key).is_some());
    assert!(admit::event_revision(&revision("two", "event", 8), &key).is_none());
    assert!(admit::calendar_revision(&revision("one", "event", 8), &domain("one")).is_none());
    for number in [0, i64::MAX as u64 + 1, u64::MAX] {
        assert!(admit::event_revision(&revision("one", "event", number), &key).is_none());
    }
    let mut absent = revision("one", "event", 0);
    absent.presence = wire::Presence::Absent.into();
    assert!(admit::event_revision(&absent, &key).is_some());
    absent.number = 1;
    assert!(admit::event_revision(&absent, &key).is_none());
}
#[test]
fn bounded_pages_keep_all_day_dates_and_reject_foreign_duplicate_or_stale_results() {
    let query = admit::admit_query(&request()).unwrap();
    let original = page();
    let admitted = admit::admit_page(&original, &query, None).unwrap();
    assert_eq!(admitted.wire().occurrences[0].recurrence_id, "2026-11-01");
    let mut bad = original.clone();
    bad.occurrences[0].key.as_option_mut().unwrap().calendar = id("foreign").into();
    assert!(admit::admit_page(&bad, &query, None).is_err());
    let mut bad = original.clone();
    bad.occurrences.push(bad.occurrences[0].clone());
    assert!(admit::admit_page(&bad, &query, None).is_err());
    let previous =
        admit::calendar_revision(&revision("synthetic-a", "", 2), &domain("synthetic-a")).unwrap();
    assert!(matches!(
        admit::admit_page(&original, &query, Some(&previous)),
        Err(admit::AdmissionError::StaleRevision)
    ));
    let mut bad = request();
    bad.page_size = 1001;
    assert!(admit::admit_query(&bad).is_err());
    let mut bad = request();
    bad.window.as_option_mut().unwrap().bounds = Some(wire::window::Bounds::Dates(Box::new(
        dates("2026-01-01", "2027-01-03"),
    )));
    assert!(admit::admit_query(&bad).is_err());
    let mut leap = request();
    leap.window.as_option_mut().unwrap().bounds = Some(wire::window::Bounds::Dates(Box::new(
        dates("2028-02-28", "2028-03-01"),
    )));
    assert!(admit::admit_query(&leap).is_ok());
}
#[test]
fn server_resolution_keeps_both_fold_instants_and_rejects_stale_or_incoherent_candidates() {
    let request = wire::ResolveLocalTimeRequest {
        id: id("one").into(),
        local: "2026-11-01T01:30:00".into(),
        zone: "America/Toronto".into(),
        ..Default::default()
    };
    let mut response = wire::ResolveLocalTimeResponse {
        id: request.id.clone(),
        local: request.local.clone(),
        zone: request.zone.clone(),
        resolution: wire::LocalTimeResolution::Ambiguous.into(),
        candidates: vec![
            wire::LocalTimeCandidate {
                instant: "2026-11-01T05:30:00Z".into(),
                offset_seconds: -14400,
                ..Default::default()
            },
            wire::LocalTimeCandidate {
                instant: "2026-11-01T06:30:00Z".into(),
                offset_seconds: -18000,
                ..Default::default()
            },
        ],
        ..Default::default()
    };
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
    response.candidates[0].offset_seconds = i32::MIN;
    assert!(admit::admit_resolution(&response, &request).is_err());
    response.candidates[0].offset_seconds = -14400;
    response.local = "2026-11-01T01:31:00".into();
    assert!(admit::admit_resolution(&response, &request).is_err());
    let mut gap = request.clone();
    gap.local = "2026-03-08T02:30:00".into();
    response.local = gap.local.clone();
    response.candidates.clear();
    response.resolution = wire::LocalTimeResolution::Nonexistent.into();
    assert!(admit::admit_resolution(&response, &gap).is_ok());
    response.resolution = EnumValue::Unknown(32);
    assert!(admit::admit_resolution(&response, &gap).is_err());
}
#[test]
fn watch_has_explicit_resync_and_monotonic_sequences_without_cross_calendar_updates() {
    let id = domain("one");
    let ready = wire::WatchCalendarResponse {
        stream_sequence: 1,
        frame: Some(wire::watch_calendar_response::Frame::Ready(Box::default())),
        ..Default::default()
    };
    assert!(admit::admit_watch(&ready, &id, 0).is_ok());
    assert!(admit::admit_watch(&ready, &id, 1).is_err());
    let mut changed = wire::WatchCalendarResponse {
        stream_sequence: 2,
        frame: Some(wire::watch_calendar_response::Frame::Changed(Box::new(
            wire::CalendarChanged {
                revision: revision("one", "", 4).into(),
                ..Default::default()
            },
        ))),
        ..Default::default()
    };
    assert!(admit::admit_watch(&changed, &id, 1).is_ok());
    assert!(admit::admit_watch(&changed, &id, 0).is_err());
    if let Some(wire::watch_calendar_response::Frame::Changed(value)) = &mut changed.frame {
        value.revision = revision("foreign", "", 4).into();
    }
    assert!(admit::admit_watch(&changed, &id, 1).is_err());
    assert!(admit::admit_watch(&ready, &id, u64::MAX).is_err());
}
