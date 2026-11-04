use super::*;
use home_wire_contract::{api_playback_contract::playback, home::api::v2 as wire};
pub(crate) fn snapshot() -> wire::Capability {
    wire::Capability {
        name: "homes/test/entities/player/capabilities/playback".into(),
        etag: "W/\"definition-one\"".into(),
        action_access: wire::ActionAccess::Allowed.into(),
        r#trait: Some(wire::capability::Trait::MediaPlayback(
            wire::MediaPlaybackCapability {
                definition: wire::MediaPlaybackDefinition {
                    operations: vec![wire::MediaOperation::Play.into()],
                    volume_basis_points: wire::MediaVolumeBounds {
                        minimum: 0,
                        maximum: 10000,
                        step: 1,
                        ..Default::default()
                    }
                    .into(),
                    position: wire::MediaPositionBounds {
                        minimum: buffa_types::google::protobuf::Duration::default().into(),
                        maximum: buffa_types::google::protobuf::Duration {
                            seconds: 240,
                            ..Default::default()
                        }
                        .into(),
                        step: buffa_types::google::protobuf::Duration {
                            nanos: 1_000_000,
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }
                .into(),
                observation: wire::MediaPlaybackObservation {
                    route: wire::MediaRoute {
                        entity: "homes/test/entities/player".into(),
                        endpoint: "playback".into(),
                        coordinator: "homes/test/entities/coordinator".into(),
                        topology_revision: 1,
                        admission_epoch: "1234567890abcdef1234567890abcdef".into(),
                        config_revision: 7,
                        ..Default::default()
                    }
                    .into(),
                    position: buffa_types::google::protobuf::Duration::default().into(),
                    duration: buffa_types::google::protobuf::Duration {
                        seconds: 240,
                        ..Default::default()
                    }
                    .into(),
                    volume_basis_points: Some(0),
                    powered_on: Some(false),
                    quality: wire::ObservationQuality {
                        availability: wire::Availability::Available.into(),
                        freshness: wire::Freshness::Current.into(),
                        provenance: wire::Provenance::Observed.into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }
                .into(),
                action_access: wire::ActionAccess::Allowed.into(),
                data_provenance: wire::MediaDataProvenance::Synthetic.into(),
                ..Default::default()
            }
            .into(),
        )),
        ..Default::default()
    }
}
pub(crate) fn media(value: &mut wire::Capability) -> &mut wire::MediaPlaybackCapability {
    let Some(wire::capability::Trait::MediaPlayback(media)) = &mut value.r#trait else {
        panic!("fixture trait")
    };
    media
}

fn data() -> Data {
    Data::new(
        playback(&snapshot()).unwrap().unwrap(),
        &"homes/test/entities/player".parse().unwrap(),
    )
    .unwrap()
}
#[test]
fn decimals_to_canonical_media_quantities_are_exact_without_rounding() {
    let data = data();
    for (field, text, canonical, display) in [
        (Field::Volume, "72.35", 7235, "72.35"),
        (Field::Volume, "0", 0, "0.00"),
        (Field::Position, "123.456", 123456, "123.456"),
        (Field::Position, ".001", 1, "0.001"),
    ] {
        let value = data.parse(field, text).unwrap();
        assert_eq!(value.canonical(), canonical);
        assert_eq!(value.text(), display);
        let request = data.quantity_request(value).unwrap();
        let Intent::Media(intent) = request.intent() else {
            panic!("media intent")
        };
        assert_eq!(
            intent.route(),
            data.snapshot().observation().unwrap().route().unwrap()
        );
        match (field, intent.action()) {
            (Field::Volume, MediaAction::Volume(v)) => assert_eq!(v.value() as u64, canonical),
            (Field::Position, MediaAction::Position(v)) => assert_eq!(v.value() as u64, canonical),
            (Field::Volume, MediaAction::Position(_))
            | (Field::Position, MediaAction::Volume(_))
            | (
                Field::Volume | Field::Position,
                MediaAction::Operation(_) | MediaAction::Source(_) | MediaAction::Group(_),
            ) => panic!("wrong quantity branch"),
        }
    }
    assert_eq!(data.parse(Field::Volume, "1.001"), Err(Refusal::Precision));
    assert_eq!(
        data.parse(Field::Position, "1.0001"),
        Err(Refusal::Precision)
    );
    for text in ["-1", "NaN", "inf", "1e2", ".", "1..2"] {
        assert!(data.parse(Field::Volume, text).is_err(), "{text}");
    }
    assert_eq!(data.parse(Field::Volume, "100.01"), Err(Refusal::Bounds));
    assert_eq!(
        data.parse(Field::Position, "18446744073709551616"),
        Err(Refusal::Bounds)
    );
}

#[test]
fn absent_reading_is_not_zero_and_renderer_positions_still_obey_canonical_steps() {
    let mut snapshot = snapshot();
    media(&mut snapshot)
        .observation
        .as_option_mut()
        .unwrap()
        .volume_basis_points = None;
    media(&mut snapshot)
        .definition
        .as_option_mut()
        .unwrap()
        .volume_basis_points
        .modify(|v| v.step = 100);
    let data = Data::new(
        playback(&snapshot).unwrap().unwrap(),
        &"homes/test/entities/player".parse().unwrap(),
    )
    .unwrap();
    assert_eq!(data.observed(Field::Volume), None);
    assert_eq!(data.observed(Field::Position).unwrap().canonical(), 0);
    assert_eq!(data.parse(Field::Volume, "0.01"), Err(Refusal::Step));
    assert_eq!(data.at_position(Field::Volume, 0.).unwrap().canonical(), 0);
    assert_eq!(
        data.at_position(Field::Volume, 1.).unwrap().canonical(),
        10_000
    );
    assert_eq!(
        data.at_position(Field::Volume, f32::NAN),
        Err(Refusal::Invalid)
    );
    for position in [0.01, 0.237, 0.777, 0.999] {
        assert!(
            data.at_position(Field::Volume, position)
                .unwrap()
                .canonical()
                .is_multiple_of(100)
        );
    }
}

#[test]
fn route_or_permission_changes_replace_control_identity_but_plain_observations_do_not() {
    let original = data();
    let mut snapshot = snapshot();
    media(&mut snapshot)
        .observation
        .as_option_mut()
        .unwrap()
        .title = Some("New title".into());
    media(&mut snapshot)
        .observation
        .as_option_mut()
        .unwrap()
        .position = buffa_types::google::protobuf::Duration {
        seconds: 20,
        ..Default::default()
    }
    .into();
    let updated = Data::new(
        playback(&snapshot).unwrap().unwrap(),
        &"homes/test/entities/player".parse().unwrap(),
    )
    .unwrap();
    assert!(original.same_controls(&updated));
    media(&mut snapshot)
        .observation
        .as_option_mut()
        .unwrap()
        .route
        .modify(|r| {
            r.coordinator = "homes/test/entities/next-coordinator".into();
            r.topology_revision += 1;
        });
    let moved = Data::new(
        playback(&snapshot).unwrap().unwrap(),
        &"homes/test/entities/player".parse().unwrap(),
    )
    .unwrap();
    assert!(!original.same_controls(&moved));
    let request = moved
        .quantity_request(moved.parse(Field::Volume, "0").unwrap())
        .unwrap();
    assert_eq!(
        request.target().as_str(),
        "homes/test/entities/player/capabilities/playback"
    );
    snapshot.action_access = wire::ActionAccess::Denied.into();
    let denied = Data::new(
        playback(&snapshot).unwrap().unwrap(),
        &"homes/test/entities/player".parse().unwrap(),
    )
    .unwrap();
    assert!(!moved.same_controls(&denied));
    assert!(
        denied
            .request(MediaAction::Operation(
                home_client_model::api_media::MediaOperation::Play
            ))
            .is_err()
    );
}

fn png_bytes(width: u32, height: u32) -> Vec<u8> {
    let mut bytes = Vec::new();
    {
        let mut encoder = png::Encoder::new(&mut bytes, width, height);
        encoder.set_color(png::ColorType::Rgba);
        encoder.set_depth(png::BitDepth::Eight);
        let mut writer = encoder.write_header().unwrap();
        writer
            .write_image_data(&vec![17; width as usize * height as usize * 4])
            .unwrap();
    }
    bytes
}
#[test]
fn artwork_decoding_is_bounded_and_preserves_pixels_without_any_url_fetch() {
    let now = UNIX_EPOCH + Duration::from_secs(1_790_000_000);
    let deadline = now + Duration::from_secs(30);
    let monotonic = std::time::Instant::now() + Duration::from_secs(30);
    let image = decode_artwork(
        &png_bytes(2, 1),
        deadline,
        now,
        monotonic,
        MediaDataProvenance::Synthetic,
    )
    .unwrap();
    assert_eq!((image.width(), image.height()), (2, 1));
    assert_eq!(image.rgba(), &[17; 8]);
    assert_eq!(image.expires(), deadline);
    assert_eq!(image.provenance(), MediaDataProvenance::Synthetic);
    assert!(matches!(
        decode_artwork(
            &png_bytes(513, 1),
            deadline,
            now,
            monotonic,
            MediaDataProvenance::Synthetic
        ),
        Err(ArtworkFailure::Capacity)
    ));
    assert!(matches!(
        decode_artwork(
            &vec![0; 128 * 1024 + 1],
            deadline,
            now,
            monotonic,
            MediaDataProvenance::Synthetic
        ),
        Err(ArtworkFailure::Capacity)
    ));
    assert!(matches!(
        decode_artwork(
            b"https://example.invalid/private-token",
            deadline,
            now,
            monotonic,
            MediaDataProvenance::Synthetic
        ),
        Err(ArtworkFailure::Invalid)
    ));
    assert!(matches!(
        decode_artwork(
            &png_bytes(2, 1),
            now,
            now,
            monotonic,
            MediaDataProvenance::Synthetic
        ),
        Err(ArtworkFailure::Expired)
    ));
}
