use home_client_model::cameras::{
    CameraId, EventId, EventKey, IdentityError, ImageKey, InvalidSessionReference, MAX_ID_BYTES,
    SESSION_REFERENCE_BYTES, SessionReference,
};

#[test]
fn camera_ids_remain_exact_without_alias_normalization() {
    let first: CameraId = "driveway".parse().unwrap();
    let second: CameraId = "_driveway".parse().unwrap();
    assert_ne!(first, second);
    for value in ["driveway", "_driveway", "Source:É/Camera 1", " camera "] {
        let id: CameraId = value.parse().unwrap();
        assert_eq!(id.entity().as_str(), value);
    }
}

#[test]
fn boundary_ids_are_bounded_in_bytes_without_trimming_user_data() {
    for value in ["", "camera\n", "camera\0", "camera\u{7f}"] {
        assert!(value.parse::<CameraId>().is_err());
        assert!(value.parse::<EventId>().is_err());
    }
    assert!("x".repeat(MAX_ID_BYTES).parse::<CameraId>().is_ok());
    assert_eq!(
        "x".repeat(MAX_ID_BYTES + 1).parse::<EventId>(),
        Err(IdentityError::TooLong)
    );
    assert_eq!(
        "é".repeat(MAX_ID_BYTES / 2 + 1).parse::<CameraId>(),
        Err(IdentityError::TooLong)
    );
}

#[test]
fn reused_event_id_cannot_move_a_thumbnail_to_another_camera() {
    let first: CameraId = "driveway".parse().unwrap();
    let second: CameraId = "_driveway".parse().unwrap();
    let event: EventId = "provider-event-42".parse().unwrap();
    let first_key = EventKey::new(first.clone(), event.clone());
    let second_key = EventKey::new(second.clone(), event);
    assert_ne!(first_key, second_key);
    assert_eq!(first_key.event().as_str(), "provider-event-42");
    let session = SessionReference::new(first.clone(), &[42; SESSION_REFERENCE_BYTES]).unwrap();
    assert!(session.owns(&ImageKey::Snapshot(first)));
    assert!(session.owns(&ImageKey::EventThumbnail(first_key)));
    assert!(!session.owns(&ImageKey::Snapshot(second)));
    assert!(!session.owns(&ImageKey::EventThumbnail(second_key)));
}

#[test]
fn opaque_reference_has_exact_length_and_redacted_diagnostics() {
    let id: CameraId = "driveway".parse().unwrap();
    for size in [0, SESSION_REFERENCE_BYTES - 1, SESSION_REFERENCE_BYTES + 1] {
        assert_eq!(
            SessionReference::new(id.clone(), &vec![42; size]),
            Err(InvalidSessionReference)
        );
    }
    let reference = [42; SESSION_REFERENCE_BYTES];
    let session = SessionReference::new(id.clone(), &reference).unwrap();
    assert_eq!(session.camera(), &id);
    assert_eq!(session.bytes(), &reference);
    let diagnostic = format!("{session:?}");
    assert!(diagnostic.contains("[REDACTED]"));
    assert!(!diagnostic.contains("42"));
}
