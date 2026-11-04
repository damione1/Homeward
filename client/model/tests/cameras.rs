use home_client_model::{
    api_content::{ContentKind, EventsEtag, MediaSession, MediaSessionName, MediaSourceName},
    cameras::{EventId, EventKey, IdentityError, ImageKey, MAX_ID_BYTES},
    local_time::UtcInstant,
};
fn source(hex: char) -> MediaSourceName {
    MediaSourceName::try_from(
        format!(
            "homes/synthetic/mediaSources/source-{}",
            hex.to_string().repeat(48)
        )
        .as_str(),
    )
    .unwrap()
}
#[test]
fn camera_source_identity_is_canonical_and_never_an_alias_or_native_entity_id() {
    let first = source('a');
    let second = source('b');
    assert_ne!(first, second);
    assert_eq!(MediaSourceName::try_from(first.as_str()).unwrap(), first);
    for raw in [
        "driveway",
        "_driveway",
        "Source:É/Camera 1",
        " camera ",
        "homes/synthetic/mediaSources/source-ABC",
    ] {
        assert!(MediaSourceName::try_from(raw).is_err());
    }
}
#[test]
fn event_ids_remain_exact_utf8_data_with_byte_and_control_bounds() {
    for raw in ["event-42", " événement / 1 ", "同じ項目"] {
        assert_eq!(raw.parse::<EventId>().unwrap().as_str(), raw);
    }
    for raw in ["", "event\n", "event\0", "event\u{7f}"] {
        assert!(raw.parse::<EventId>().is_err());
    }
    assert!("x".repeat(MAX_ID_BYTES).parse::<EventId>().is_ok());
    assert_eq!(
        "é".repeat(MAX_ID_BYTES / 2 + 1).parse::<EventId>(),
        Err(IdentityError::TooLong)
    );
}
#[test]
fn reused_event_id_cannot_move_a_target_to_another_configured_slot() {
    let first = source('a');
    let second = source('b');
    let event: EventId = "same-event".parse().unwrap();
    let one = EventKey::new(first.clone(), event.clone());
    let two = EventKey::new(second.clone(), event);
    assert_ne!(one, two);
    assert_eq!(ImageKey::EventThumbnail(one).source(), &first);
    assert_eq!(ImageKey::Snapshot(second.clone()).source(), &second);
}
#[test]
fn session_names_and_kind_specific_metadata_do_not_grant_or_extend_a_lease() {
    let raw = format!("homes/synthetic/mediaSessions/s-{}", "a".repeat(52));
    let name = MediaSessionName::try_from(raw.as_str()).unwrap();
    assert!(format!("{name:?}").contains("[REDACTED]"));
    let alternate = format!("homes/synthetic/mediaSessions/s-{}b", "a".repeat(51));
    assert!(MediaSessionName::try_from(alternate.as_str()).is_err());
    assert!(MediaSessionName::try_from(raw.to_uppercase().as_str()).is_err());
    let created = UtcInstant::new(1, 0).unwrap();
    let expires = UtcInstant::new(61, 0).unwrap();
    let etag = EventsEtag::try_from("\"opaque\"").unwrap();
    assert!(
        MediaSession::new(
            name.clone(),
            source('a'),
            ContentKind::Camera,
            created,
            expires,
            Some(etag.clone())
        )
        .is_ok()
    );
    assert!(
        MediaSession::new(
            name.clone(),
            source('a'),
            ContentKind::Artwork,
            created,
            expires,
            Some(etag)
        )
        .is_err()
    );
    assert!(
        MediaSession::new(
            name.clone(),
            source('a'),
            ContentKind::Camera,
            created,
            expires,
            None
        )
        .is_err()
    );
    assert!(
        MediaSession::new(
            name,
            source('a'),
            ContentKind::Artwork,
            created,
            UtcInstant::new(62, 0).unwrap(),
            None
        )
        .is_err()
    );
}
