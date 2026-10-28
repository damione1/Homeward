use buffa::{EnumValue, Message};
use buffa_types::google::protobuf::Timestamp;
use home_client_model::cameras::{CameraId, EventKey, ImageKey};
use home_wire_contract::{cameras_contract as admit, home::cameras::v1 as wire};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

fn stamp(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        nanos: 0,
        ..Default::default()
    }
}
const NOW: i64 = 1_793_110_000;
fn wall() -> SystemTime {
    UNIX_EPOCH + Duration::from_secs(NOW as u64)
}
fn session(camera: &str) -> wire::SessionRef {
    wire::SessionRef {
        entity_id: camera.into(),
        reference: vec![42; 32],
        ..Default::default()
    }
}
fn raw_lease() -> wire::Lease {
    wire::Lease {
        session: session("driveway").into(),
        issued_at: stamp(NOW - 1).into(),
        expires_at: stamp(NOW + 30).into(),
        event_revision: 7,
        ..Default::default()
    }
}
fn opened(raw: wire::Lease) -> wire::OpenSessionResponse {
    wire::OpenSessionResponse {
        result: Some(wire::open_session_response::Result::Lease(Box::new(raw))),
        ..Default::default()
    }
}
fn lease(now: Instant) -> admit::Lease {
    let expected = "driveway".parse().unwrap();
    let admit::Outcome::Value(value) =
        admit::lease(&opened(raw_lease()), &expected, wall(), now).unwrap()
    else {
        panic!("expected lease")
    };
    value
}
fn snapshot_key() -> wire::ImageKey {
    wire::ImageKey {
        session: session("driveway").into(),
        target: Some(wire::image_key::Target::Snapshot(Box::default())),
        ..Default::default()
    }
}
fn raw_image() -> wire::Image {
    wire::Image {
        key: snapshot_key().into(),
        captured_at: stamp(NOW - 60).into(),
        expires_at: stamp(NOW + 5).into(),
        png: b"\x89PNG\r\n\x1a\nnot-a-decoded-image".to_vec(),
        ..Default::default()
    }
}
fn image(value: wire::Image) -> wire::ReadImageResponse {
    wire::ReadImageResponse {
        result: Some(wire::read_image_response::Result::Image(Box::new(value))),
        ..Default::default()
    }
}
fn unknown<T: Message + Default>(value: &T) -> T {
    let mut bytes = value.encode_to_vec();
    bytes.extend_from_slice(&[0xf8, 0x07, 1]);
    T::decode_from_slice(&bytes).unwrap()
}

#[test]
fn catalog_preserves_similar_exact_ids_and_rejects_unknown_policy_values() {
    let make = |id: &str| wire::Camera {
        entity_id: id.into(),
        title: "Same camera title".into(),
        source: "Synthetic source".into(),
        availability: wire::Availability::Available.into(),
        provenance: wire::DataProvenance::Synthetic.into(),
        ..Default::default()
    };
    let catalog = wire::ListCamerasResponse {
        cameras: vec![make("driveway"), make("_driveway")],
        ..Default::default()
    };
    let rows = admit::catalog(&catalog).unwrap();
    assert_ne!(rows[0].id, rows[1].id);
    let mut invalid = catalog.clone();
    invalid.cameras[1].entity_id = "driveway".into();
    assert!(admit::catalog(&invalid).is_err());
    for policy in [
        EnumValue::Unknown(92),
        wire::Availability::Unspecified.into(),
    ] {
        let mut invalid = catalog.clone();
        invalid.cameras[0].availability = policy;
        assert!(admit::catalog(&invalid).is_err());
    }
    let mut invalid = catalog.clone();
    invalid.cameras[0].provenance = EnumValue::Unknown(92);
    assert!(admit::catalog(&invalid).is_err());
    let mut invalid = catalog.clone();
    invalid.cameras[0] = unknown(&invalid.cameras[0]);
    assert!(admit::catalog(&invalid).is_err());
    assert!(admit::catalog(&unknown(&catalog)).is_err());
    let too_many = wire::ListCamerasResponse {
        cameras: (0..=admit::MAX_CATALOG)
            .map(|i| make(&format!("camera-{i}")))
            .collect(),
        ..Default::default()
    };
    assert!(matches!(
        admit::catalog(&too_many),
        Err(admit::AdmissionError::Capacity)
    ));
}

#[test]
fn leases_are_exact_bounded_expiring_and_unknown_nested_data_is_not_ignored() {
    let now = Instant::now();
    let expected: CameraId = "driveway".parse().unwrap();
    let valid = raw_lease();
    assert!(admit::lease(&opened(valid.clone()), &expected, wall(), now).is_ok());
    let mut invalid = valid.clone();
    invalid.session = session("_driveway").into();
    assert!(matches!(
        admit::lease(&opened(invalid), &expected, wall(), now),
        Err(admit::AdmissionError::ForeignResource)
    ));
    let mut invalid = valid.clone();
    invalid.session.as_option_mut().unwrap().reference.pop();
    assert!(admit::lease(&opened(invalid), &expected, wall(), now).is_err());
    for end in [NOW - 2, NOW, NOW + 60] {
        let mut invalid = valid.clone();
        invalid.expires_at = stamp(end).into();
        assert!(admit::lease(&opened(invalid), &expected, wall(), now).is_err());
    }
    let mut invalid = valid.clone();
    invalid.issued_at = stamp(NOW + 1).into();
    assert!(admit::lease(&opened(invalid), &expected, wall(), now).is_err());
    let mut invalid = valid.clone();
    invalid.event_revision = 0;
    assert!(admit::lease(&opened(invalid), &expected, wall(), now).is_err());
    let mut invalid = valid.clone();
    invalid.session = unknown(&session("driveway")).into();
    assert!(admit::lease(&opened(invalid), &expected, wall(), now).is_err());
    let mut invalid = valid.clone();
    invalid.expires_at = unknown(&stamp(NOW + 30)).into();
    assert!(admit::lease(&opened(invalid), &expected, wall(), now).is_err());
    assert!(admit::lease(&opened(unknown(&valid)), &expected, wall(), now).is_err());
    let lease = lease(now);
    assert!(lease.live(now + Duration::from_secs(29)).is_ok());
    assert_eq!(
        lease.live(now + Duration::from_secs(30)),
        Err(admit::AdmissionError::Expired)
    );
    let debug = format!("{lease:?}");
    assert!(debug.contains("[REDACTED]"));
    assert!(!debug.contains(&format!("{:?}", valid.session.reference)));
}

#[test]
fn image_ownership_includes_native_camera_reference_and_exact_event_target() {
    let now = Instant::now();
    let lease = lease(now);
    let expected = ImageKey::Snapshot("driveway".parse().unwrap());
    assert!(matches!(
        admit::image(&image(raw_image()), &lease, &expected, wall(), now),
        Ok(admit::Outcome::Value(_))
    ));
    let mut wrong = raw_image();
    wrong.key.as_option_mut().unwrap().session = session("_driveway").into();
    assert!(matches!(
        admit::image(&image(wrong), &lease, &expected, wall(), now),
        Err(admit::AdmissionError::ForeignResource)
    ));
    let mut wrong = raw_image();
    wrong
        .key
        .as_option_mut()
        .unwrap()
        .session
        .as_option_mut()
        .unwrap()
        .reference[0] ^= 1;
    assert!(admit::image(&image(wrong), &lease, &expected, wall(), now).is_err());
    let mut wrong = raw_image();
    wrong.key.as_option_mut().unwrap().target =
        Some(wire::image_key::Target::EventId("event-1".into()));
    assert!(admit::image(&image(wrong), &lease, &expected, wall(), now).is_err());
    let target = ImageKey::EventThumbnail(EventKey::new(
        "_driveway".parse().unwrap(),
        "event-1".parse().unwrap(),
    ));
    assert_eq!(
        admit::image_request(&lease, &target).err(),
        Some(admit::AdmissionError::ForeignResource)
    );
}

#[test]
fn wall_clock_rollback_cannot_extend_an_image_or_its_monotonic_lease() {
    let now = Instant::now();
    let lease = lease(now);
    let expected = ImageKey::Snapshot("driveway".parse().unwrap());
    let rolled_back = wall() - Duration::from_secs(20);
    let admit::Outcome::Value(image) =
        admit::image(&image(raw_image()), &lease, &expected, rolled_back, now).unwrap()
    else {
        panic!("expected bounded image")
    };
    assert_eq!(image.deadline, now + Duration::from_secs(5));
    assert_eq!(
        admit::image(
            &super_image(),
            &lease,
            &expected,
            rolled_back,
            now + Duration::from_secs(5)
        )
        .err(),
        Some(admit::AdmissionError::Expired)
    );
}
fn super_image() -> wire::ReadImageResponse {
    image(raw_image())
}

#[test]
fn image_bytes_expiry_and_unknown_fields_fail_before_private_projection() {
    let now = Instant::now();
    let lease = lease(now);
    let expected = ImageKey::Snapshot("driveway".parse().unwrap());
    let check = |raw| admit::image(&image(raw), &lease, &expected, wall(), now).is_err();
    let mut invalid = raw_image();
    invalid.png.resize(admit::MAX_IMAGE_BYTES + 1, 0);
    assert!(check(invalid));
    let mut invalid = raw_image();
    invalid.png.clear();
    assert!(check(invalid));
    let mut invalid = raw_image();
    invalid.expires_at = stamp(NOW + 31).into();
    assert!(check(invalid));
    let mut invalid = raw_image();
    invalid.captured_at = stamp(NOW + 1).into();
    assert!(check(invalid));
    let mut invalid = raw_image();
    invalid.key = unknown(&snapshot_key()).into();
    assert!(check(invalid));
    let mut invalid = raw_image();
    invalid.key.as_option_mut().unwrap().target = Some(wire::image_key::Target::Snapshot(
        Box::new(unknown(&Default::default())),
    ));
    assert!(check(invalid));
    assert!(check(unknown(&raw_image())));
    assert!(
        admit::image(
            &unknown(&image(raw_image())),
            &lease,
            &expected,
            wall(),
            now
        )
        .is_err()
    );
}

#[test]
fn absent_expired_revoked_wrong_camera_and_unavailable_stay_distinct() {
    use wire::read_image_response::Result as R;
    let now = Instant::now();
    let lease = lease(now);
    let key = ImageKey::Snapshot("driveway".parse().unwrap());
    for (result, expected) in [
        (R::Missing(Box::default()), admit::ReadIssue::Missing),
        (R::Expired(Box::default()), admit::ReadIssue::Expired),
        (R::Revoked(Box::default()), admit::ReadIssue::Revoked),
        (
            R::WrongCamera(Box::default()),
            admit::ReadIssue::WrongCamera,
        ),
        (
            R::Unavailable(Box::default()),
            admit::ReadIssue::Unavailable,
        ),
    ] {
        let response = wire::ReadImageResponse {
            result: Some(result),
            ..Default::default()
        };
        assert!(
            matches!(admit::image(&response,&lease,&key,wall(),now),Ok(admit::Outcome::Refused(issue)) if issue==expected)
        );
    }
}

fn raw_event(id: &str) -> wire::Event {
    wire::Event {
        entity_id: "driveway".into(),
        event_id: id.into(),
        title: "Similar titles are not identity".into(),
        source: "Synthetic source".into(),
        captured_at: stamp(NOW - 60).into(),
        has_image: true,
        ..Default::default()
    }
}
fn raw_page() -> wire::EventsPage {
    wire::EventsPage {
        session: session("driveway").into(),
        revision: 7,
        events: vec![raw_event("a"), raw_event("b")],
        next_cursor: vec![],
        total: 2,
        ..Default::default()
    }
}
fn page(value: wire::EventsPage) -> wire::ListEventsResponse {
    wire::ListEventsResponse {
        result: Some(wire::list_events_response::Result::Page(Box::new(value))),
        ..Default::default()
    }
}
#[test]
fn event_pages_reject_mixed_cameras_duplicates_bad_order_and_stale_revisions() {
    let now = Instant::now();
    let lease = lease(now);
    let check = |raw| admit::events(&page(raw), &lease, wall(), now).is_err();
    assert!(admit::events(&page(raw_page()), &lease, wall(), now).is_ok());
    let mut invalid = raw_page();
    invalid.events[1].entity_id = "_driveway".into();
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.events[1].event_id = "a".into();
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.events.reverse();
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.events[1].captured_at = stamp(NOW - 59).into();
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.revision = 6;
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.total = 1;
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.events.clear();
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.next_cursor = vec![1; admit::MAX_CURSOR_BYTES + 1];
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.total = admit::MAX_EVENTS as u32 + 1;
    assert!(check(invalid));
    let mut invalid = raw_page();
    invalid.events[0] = unknown(&invalid.events[0]);
    assert!(check(invalid));
    assert!(check(unknown(&raw_page())));
}

#[test]
fn lifecycle_frames_cannot_reset_a_lease_replay_a_sequence_or_switch_resources() {
    use wire::watch_session_response::Frame as F;
    let lease = lease(Instant::now());
    let ready = wire::WatchSessionResponse {
        sequence: 1,
        frame: Some(F::Ready(Box::new(raw_lease()))),
        ..Default::default()
    };
    assert_eq!(
        admit::watch(&ready, &lease, 0, 0),
        Ok(admit::WatchFrame::Ready(7))
    );
    assert_eq!(
        admit::watch(&ready, &lease, 1, 7),
        Err(admit::AdmissionError::Sequence)
    );
    let changed = wire::WatchSessionResponse {
        sequence: 2,
        frame: Some(F::Changed(Box::new(wire::EventsChanged {
            session: session("driveway").into(),
            revision: 8,
            ..Default::default()
        }))),
        ..Default::default()
    };
    assert_eq!(
        admit::watch(&changed, &lease, 1, 7),
        Ok(admit::WatchFrame::Changed(8))
    );
    assert!(admit::watch(&changed, &lease, 1, 8).is_err());
    assert!(admit::watch(&changed, &lease, 0, 7).is_err());
    let mut invalid = raw_lease();
    invalid.expires_at = stamp(NOW + 31).into();
    let response = wire::WatchSessionResponse {
        sequence: 1,
        frame: Some(F::Ready(Box::new(invalid))),
        ..Default::default()
    };
    assert!(admit::watch(&response, &lease, 0, 0).is_err());
    let mut invalid = changed.clone();
    invalid.frame = Some(F::Changed(Box::new(wire::EventsChanged {
        session: session("_driveway").into(),
        revision: 8,
        ..Default::default()
    })));
    assert!(admit::watch(&invalid, &lease, 1, 7).is_err());
    assert!(admit::watch(&unknown(&ready), &lease, 0, 0).is_err());
    let mut invalid = ready.clone();
    invalid.frame = None;
    assert!(admit::watch(&invalid, &lease, 0, 0).is_err());
    let mut invalid = ready;
    invalid.sequence = 0;
    assert!(admit::watch(&invalid, &lease, u64::MAX, 7).is_err());
}
