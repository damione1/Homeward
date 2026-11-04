use buffa::Message;
use buffa_types::google::protobuf::Timestamp;
use home_client_model::{
    api::HomeName,
    api_content::{ContentKind, MediaSessionName, MediaSourceName},
    cameras::{EventKey, ImageKey},
};
use home_wire_contract::{camera_content as admit, home::api::v2 as wire};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
fn home() -> HomeName {
    HomeName::try_from("homes/synthetic").unwrap()
}
fn source(hex: char) -> MediaSourceName {
    format!(
        "homes/synthetic/mediaSources/source-{}",
        hex.to_string().repeat(48)
    )
    .parse()
    .unwrap()
}
fn session_id(last: char) -> String {
    format!("homes/synthetic/mediaSessions/s-{}{last}", "a".repeat(51))
}
fn stamp(seconds: i64) -> Timestamp {
    Timestamp {
        seconds,
        ..Default::default()
    }
}
fn raw_lease(source: char) -> wire::MediaSession {
    wire::MediaSession {
        name: session_id('a'),
        media_source: self::source(source).as_str().to_owned(),
        content_kind: wire::MediaContentKind::Camera.into(),
        create_time: stamp(100).into(),
        expire_time: stamp(160).into(),
        events_etag: "\"opaque-events\"".to_owned(),
        ..Default::default()
    }
}
fn lease() -> admit::Lease {
    admit::lease(
        &raw_lease('a'),
        &source('a'),
        ContentKind::Camera,
        UNIX_EPOCH + Duration::from_secs(101),
        Instant::now(),
    )
    .unwrap()
}
fn png(width: u32, height: u32) -> Vec<u8> {
    let mut bytes = b"\x89PNG\r\n\x1a\n\0\0\0\rIHDR".to_vec();
    bytes.extend(width.to_be_bytes());
    bytes.extend(height.to_be_bytes());
    bytes
}
fn image(lease: &admit::Lease) -> wire::ReadMediaSessionResponse {
    wire::ReadMediaSessionResponse {
        image: wire::MediaImage {
            media_session: lease.name().as_str().to_owned(),
            target: wire::MediaImageTarget {
                value: Some(wire::media_image_target::Value::Snapshot(Default::default())),
                ..Default::default()
            }
            .into(),
            capture_time: stamp(100).into(),
            expire_time: stamp(160).into(),
            png: png(2, 1),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
#[test]
fn current_sources_keep_slots_distinct_and_data_provenance_independent() {
    let raw = |hex| wire::MediaSource {
        name: source(hex).as_str().to_owned(),
        display_name: "Same label".to_owned(),
        source_entity: "homes/synthetic/entities/camera".to_owned(),
        content_kinds: vec![wire::MediaContentKind::Camera.into()],
        data_provenance: wire::MediaDataProvenance::Synthetic.into(),
        quality: wire::ObservationQuality {
            availability: wire::Availability::Unavailable.into(),
            freshness: wire::Freshness::Stale.into(),
            provenance: wire::Provenance::Observed.into(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let mut page = wire::ListMediaSourcesResponse {
        media_sources: vec![raw('a'), raw('b')],
        ..Default::default()
    };
    let rows = admit::catalog(&page, &home()).unwrap();
    assert_ne!(rows[0].id, rows[1].id);
    assert_eq!(rows[0].title, rows[1].title);
    assert_eq!(
        rows[0].provenance,
        home_client_model::api_media::MediaDataProvenance::Synthetic
    );
    page.media_sources[0].data_provenance = buffa::EnumValue::Unknown(999);
    assert!(admit::catalog(&page, &home()).is_err());
    page.media_sources[0] = raw('a');
    page.media_sources[1] = raw('a');
    assert!(admit::catalog(&page, &home()).is_err());
}
#[test]
fn session_names_source_kind_and_original_lifetime_are_admitted_together() {
    assert_eq!(lease().source(), &source('a'));
    let wall = UNIX_EPOCH + Duration::from_secs(101);
    let mut raw = raw_lease('a');
    raw.expire_time = stamp(161).into();
    assert!(
        admit::lease(
            &raw,
            &source('a'),
            ContentKind::Camera,
            wall,
            Instant::now()
        )
        .is_err()
    );
    raw = raw_lease('b');
    assert!(
        admit::lease(
            &raw,
            &source('a'),
            ContentKind::Camera,
            wall,
            Instant::now()
        )
        .is_err()
    );
    raw = raw_lease('a');
    raw.artwork_reference = vec![1];
    assert!(
        admit::lease(
            &raw,
            &source('a'),
            ContentKind::Camera,
            wall,
            Instant::now()
        )
        .is_err()
    );
    assert!(
        admit::lease(
            &raw_lease('a'),
            &source('a'),
            ContentKind::Artwork,
            wall,
            Instant::now()
        )
        .is_err()
    );
    assert!(
        admit::lease(
            &raw_lease('a'),
            &source('a'),
            ContentKind::Camera,
            UNIX_EPOCH + Duration::from_secs(161),
            Instant::now()
        )
        .is_err()
    );
}
#[test]
fn image_bytes_are_exact_session_target_and_original_expiry_bound() {
    let lease = lease();
    let key = ImageKey::Snapshot(source('a'));
    let wall = UNIX_EPOCH + Duration::from_secs(101);
    let raw = image(&lease);
    let admitted = admit::image(&raw, &lease, &key, wall, Instant::now()).unwrap();
    assert_eq!(
        admitted.session,
        MediaSessionName::try_from(raw.image.media_session.as_str()).unwrap()
    );
    assert_eq!(admitted.deadline, lease.deadline());
    let mut foreign = raw.clone();
    foreign.image.get_or_insert_default().media_session = session_id('q');
    assert!(admit::image(&foreign, &lease, &key, wall, Instant::now()).is_err());
    let event = ImageKey::EventThumbnail(EventKey::new(source('a'), "same".parse().unwrap()));
    assert!(admit::image(&raw, &lease, &event, wall, Instant::now()).is_err());
    for expires in [159, 161] {
        let mut raw = raw.clone();
        raw.image.get_or_insert_default().expire_time = stamp(expires).into();
        assert!(admit::image(&raw, &lease, &key, wall, Instant::now()).is_err());
    }
    assert!(admit::image_request(&lease, &ImageKey::Snapshot(source('b'))).is_err());
}
#[test]
fn png_bounds_and_capture_times_are_checked_before_any_decoder_allocation() {
    let lease = lease();
    let key = ImageKey::Snapshot(source('a'));
    let wall = UNIX_EPOCH + Duration::from_secs(101);
    for bytes in [
        png(0, 1),
        png(1025, 1),
        b"https://example.invalid/private".to_vec(),
        vec![0; admit::MAX_IMAGE_BYTES + 1],
    ] {
        let mut raw = image(&lease);
        raw.image.get_or_insert_default().png = bytes;
        assert!(admit::image(&raw, &lease, &key, wall, Instant::now()).is_err());
    }
    let mut raw = image(&lease);
    raw.image.get_or_insert_default().capture_time = stamp(102).into();
    assert!(admit::image(&raw, &lease, &key, wall, Instant::now()).is_err());
    let mut bytes = image(&lease).encode_to_vec();
    bytes.extend([0xfa, 0x7f, 0]);
    let raw = wire::ReadMediaSessionResponse::decode_from_slice(&bytes).unwrap();
    assert!(admit::image(&raw, &lease, &key, wall, Instant::now()).is_err());
}
#[test]
fn watch_etags_have_no_order_and_ready_cannot_renew_the_session() {
    let lease = lease();
    let ready = wire::WatchMediaSessionResponse {
        sequence: 1,
        frame: Some(wire::watch_media_session_response::Frame::Ready(Box::new(
            raw_lease('a'),
        ))),
        ..Default::default()
    };
    let admit::WatchFrame::Ready(etag) = admit::watch(&ready, &lease, 0, None).unwrap() else {
        panic!("ready")
    };
    let changed = |etag: &str| wire::WatchMediaSessionResponse {
        sequence: 2,
        frame: Some(wire::watch_media_session_response::Frame::Changed(
            Box::new(wire::MediaEventsChanged {
                events_etag: etag.to_owned(),
                ..Default::default()
            }),
        )),
        ..Default::default()
    };
    assert!(admit::watch(&changed("\"aaa\""), &lease, 1, Some(&etag)).is_ok());
    assert!(admit::watch(&changed(etag.as_str()), &lease, 1, Some(&etag)).is_err());
    let mut renewed = raw_lease('a');
    renewed.expire_time = stamp(159).into();
    let mut invalid = ready;
    invalid.frame = Some(wire::watch_media_session_response::Frame::Ready(Box::new(
        renewed,
    )));
    assert!(admit::watch(&invalid, &lease, 0, None).is_err());
}
#[test]
fn event_pages_keep_native_ids_false_and_current_session_without_fabricated_resources() {
    let lease = lease();
    let mut page = wire::QueryMediaSessionEventsResponse {
        media_session: lease.name().as_str().to_owned(),
        events_etag: "\"opaque\"".to_owned(),
        total_size: 1,
        media_events: vec![wire::MediaEvent {
            event_id: " opaque / 同じ ".to_owned(),
            display_name: "Synthetic".to_owned(),
            source: "Fixture".to_owned(),
            capture_time: stamp(100).into(),
            has_image: false,
            ..Default::default()
        }],
        ..Default::default()
    };
    let admitted = admit::events(
        &page,
        &lease,
        UNIX_EPOCH + Duration::from_secs(101),
        Instant::now(),
    )
    .unwrap();
    assert!(!admitted.events[0].has_image);
    assert_eq!(admitted.events[0].key.event().as_str(), " opaque / 同じ ");
    page.media_session = session_id('q');
    assert!(admit::events(&page, &lease, SystemTime::now(), Instant::now()).is_err());
    page.media_session = lease.name().as_str().to_owned();
    page.media_events.push(page.media_events[0].clone());
    page.total_size = 2;
    assert!(admit::events(&page, &lease, SystemTime::now(), Instant::now()).is_err());
}
