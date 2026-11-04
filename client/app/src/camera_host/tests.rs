use super::*;
use home_client_model::cameras::EventKey;
use home_wire_contract::camera_content as admit;
use std::time::{SystemTime, UNIX_EPOCH};
fn event(camera: &str, id: &str) -> admit::Event {
    admit::Event {
        key: EventKey::new(camera.parse().unwrap(), id.parse().unwrap()),
        title: "Identical event title".into(),
        source: "Synthetic source".into(),
        captured_at: UNIX_EPOCH,
        has_image: true,
    }
}
fn png(width: u32, height: u32) -> Vec<u8> {
    let mut bytes = Vec::new();
    {
        let mut encoder = png::Encoder::new(&mut bytes, width, height);
        encoder.set_color(png::ColorType::Rgba);
        encoder.set_depth(png::BitDepth::Eight);
        let mut writer = encoder.write_header().unwrap();
        writer
            .write_image_data(&vec![127; width as usize * height as usize * 4])
            .unwrap();
    }
    bytes
}
fn image() -> admit::Image {
    admit::Image {
        session: home_client_model::api_content::MediaSessionName::try_from(
            "homes/synthetic/mediaSessions/s-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        )
        .unwrap(),
        kind: home_client_model::api_content::ContentKind::Camera,
        key: ImageKey::Snapshot(
            "homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111"
                .parse()
                .unwrap(),
        ),
        captured_at: SystemTime::now(),
        expires_at: SystemTime::now() + Duration::from_secs(30),
        deadline: Instant::now() + Duration::from_secs(30),
        png: png(2, 1),
    }
}
#[test]
fn stable_event_tokens_survive_reorder_and_remain_camera_scoped() {
    let first = Events::new(
        vec![event("homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111", "same"), event("homes/synthetic/mediaSources/source-222222222222222222222222222222222222222222222222", "same")],
        &Events::default(),
    )
    .unwrap();
    let key = first.key(first.token(0).unwrap()).unwrap().clone();
    let token = first.token(0).unwrap();
    let second = Events::new(
        vec![event("homes/synthetic/mediaSources/source-222222222222222222222222222222222222222222222222", "same"), event("homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111", "same")],
        &first,
    )
    .unwrap();
    assert_eq!(second.token(1), Some(token));
    assert_eq!(second.key(token), Some(&key));
    assert_ne!(second.token(0), second.token(1));
    assert!(
        Events::new(
            vec![event("homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111", "same"), event("homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111", "same")],
            &first
        )
        .is_err()
    );
}
#[test]
fn viewport_and_identity_changes_cancel_old_reads_and_remove_private_pixels() {
    let mut state = State {
        active: true,
        interactive: true,
        ..State::default()
    };
    state.select(
        "homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111"
            .parse()
            .unwrap(),
    );
    let context = state.context;
    let old = state.image_cancel.clone();
    let frame = Arc::new(crate::camera::decode(image(), Instant::now()).unwrap());
    let weak = Arc::downgrade(&frame);
    state.picture = Some(frame);
    state.hide_preview();
    assert!(old.is_cancelled());
    assert!(state.picture.is_none());
    assert!(weak.upgrade().is_none());
    let old = state.cancel.clone();
    state.select(
        "homes/synthetic/mediaSources/source-222222222222222222222222222222222222222222222222"
            .parse()
            .unwrap(),
    );
    assert!(old.is_cancelled());
    assert!(!state.accepts(context, Instant::now()));
    assert_eq!(
        state.target,
        Some(ImageKey::Snapshot(
            "homes/synthetic/mediaSources/source-222222222222222222222222222222222222222222222222"
                .parse()
                .unwrap()
        ))
    );
    state.clear_private();
    assert!(state.catalog.is_empty());
    assert!(state.selected.is_none());
    assert!(state.events.rows.is_empty());
    assert!(state.picture.is_none());
}
#[test]
fn returning_to_the_viewport_uses_a_live_token_without_reviving_the_cancelled_read() {
    let owner = Cameras::detached();
    {
        let mut state = owner.0.state.lock().unwrap();
        state.active = true;
        state.interactive = true;
        state.select(
            "homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111"
                .parse()
                .unwrap(),
        );
    }
    let old = owner.0.state.lock().unwrap().image_cancel.clone();
    owner.viewport(false);
    assert!(old.is_cancelled());
    owner.viewport(true);
    let state = owner.0.state.lock().unwrap();
    assert!(!state.image_cancel.is_cancelled());
    assert!(state.image_requested);
    assert!(old.is_cancelled());
    assert_eq!(
        state.target,
        Some(ImageKey::Snapshot(
            "homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111"
                .parse()
                .unwrap()
        ))
    );
}
#[test]
fn returning_to_the_viewport_preserves_expiry_and_refusals_until_explicit_reload() {
    let owner = Cameras::detached();
    for status in [
        Status::Missing,
        Status::Expired,
        Status::Revoked,
        Status::WrongCamera,
        Status::Offline,
        Status::Unavailable,
        Status::Invalid,
        Status::Unsupported,
    ] {
        {
            let mut state = owner.0.state.lock().unwrap();
            state.status = status;
            state.target = Some(ImageKey::Snapshot("homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111".parse().unwrap()));
        }
        owner.viewport(false);
        owner.viewport(true);
        let state = owner.0.state.lock().unwrap();
        assert_eq!(state.status, status);
        assert!(!state.image_requested);
        assert!(state.picture.is_none());
    }
}
#[test]
fn private_decode_rejects_deadlines_bad_pixels_and_dimensions_before_projection() {
    let value = image();
    let now = Instant::now();
    let decoded = crate::camera::decode(value, now).unwrap();
    assert_eq!(
        (
            decoded.pixels.width,
            decoded.pixels.height,
            decoded.pixels.rgba.len()
        ),
        (2, 1, 8)
    );
    let mut value = image();
    value.deadline = now;
    assert!(matches!(
        crate::camera::decode(value, now),
        Err(Status::Expired)
    ));
    let mut value = image();
    value.png = vec![0; 16];
    assert!(matches!(
        crate::camera::decode(value, now),
        Err(Status::Invalid)
    ));
    let mut value = image();
    value.png = png(admit::MAX_IMAGE_SIDE + 1, 1);
    assert!(matches!(
        crate::camera::decode(value, now),
        Err(Status::Invalid)
    ));
    let mut value = image();
    value.png.resize(admit::MAX_IMAGE_BYTES + 1, 0);
    assert!(matches!(
        crate::camera::decode(value, now),
        Err(Status::Invalid)
    ));
}
#[test]
fn privacy_retirement_and_exhausted_generations_cannot_be_reactivated() {
    let owner = Cameras::detached();
    {
        let mut state = owner.0.state.lock().unwrap();
        state.active = true;
        state.interactive = true;
        state.select(
            "homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111"
                .parse()
                .unwrap(),
        );
        state.picture = Some(Arc::new(
            crate::camera::decode(image(), Instant::now()).unwrap(),
        ));
    }
    owner.foreground(false);
    let state = owner.0.state.lock().unwrap();
    assert!(state.picture.is_none());
    assert!(state.selected.is_none());
    drop(state);
    let mut state = State {
        generation: i32::MAX,
        ..State::default()
    };
    state.bump();
    assert_eq!(state.generation, 0);
    state.clear_private();
    assert_eq!(state.generation, 0);
    assert!(!state.accepts(state.context, Instant::now()));
}

#[test]
fn thumbnail_withdrawal_cancels_inflight_decode_and_discards_pixels() {
    let source =
        "homes/synthetic/mediaSources/source-111111111111111111111111111111111111111111111111";
    let mut state = State::default();
    let mut current = event(source, "event");
    state.target = Some(ImageKey::EventThumbnail(current.key.clone()));
    state.replace_events(vec![current.clone()]).unwrap();
    state.picture = Some(Arc::new(
        crate::camera::decode(image(), Instant::now()).unwrap(),
    ));
    let private = Arc::downgrade(state.picture.as_ref().unwrap());
    let in_flight = state.image_cancel.clone();
    state.image_requested = true;
    current.has_image = false;
    state.replace_events(vec![current]).unwrap();
    assert!(in_flight.is_cancelled());
    assert!(private.upgrade().is_none());
    assert!(!state.image_requested);
    assert_eq!(state.status, Status::Unavailable);
    state.replace_events(vec![]).unwrap();
    assert_eq!(state.status, Status::Missing);
}
