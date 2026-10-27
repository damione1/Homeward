use super::*;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::home::state::v1 as state;

pub(crate) fn snapshot() -> wire::Snapshot {
    wire::Snapshot {
        route: wire::Route {
            entity_id: "reader".into(),
            endpoint: "media".into(),
            coordinator_entity_id: "coordinator".into(),
            topology_revision: 1,
            admission_epoch: "0123456789abcdef0123456789abcdef".into(),
            config_revision: 7,
            ..Default::default()
        }
        .into(),
        capabilities: wire::Capabilities {
            operations: vec![wire::Operation::Play.into()],
            volume_basis_points: wire::QuantityBounds {
                minimum: 0,
                maximum: 10_000,
                step: 1,
                ..Default::default()
            }
            .into(),
            position_millis: wire::QuantityBounds {
                minimum: 0,
                maximum: 240_000,
                step: 1,
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }
        .into(),
        action_access: state::ActionAccess::Allowed.into(),
        playback: wire::PlaybackState::Paused.into(),
        volume_basis_points: Some(0),
        position_millis: Some(0),
        duration_millis: Some(240_000),
        quality: state::Quality {
            availability: state::Availability::Available.into(),
            freshness: state::Freshness::Fresh.into(),
            provenance: state::Provenance::Estimated.into(),
            ..Default::default()
        }
        .into(),
        received_at: Timestamp {
            seconds: 1_790_000_000,
            ..Default::default()
        }
        .into(),
        descriptor_sha256: vec![42; 32],
        ..Default::default()
    }
}
fn data() -> Data {
    Data::new(snapshot(), &"reader".parse().unwrap()).unwrap()
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
        let Some(actions::action_args::Arguments::Extension(extension)) =
            &request.arguments.arguments
        else {
            panic!("typed media extension")
        };
        let payload =
            wire::MediaAction::decode(&mut extension.protobuf_payload.as_slice()).unwrap();
        assert_eq!(payload.route.as_option(), data.snapshot().route.as_option());
        match (field, payload.command) {
            (Field::Volume, Some(wire::media_action::Command::VolumeBasisPoints(v))) => {
                assert_eq!(u64::from(v), canonical)
            }
            (Field::Position, Some(wire::media_action::Command::PositionMillis(v))) => {
                assert_eq!(v, canonical)
            }
            (
                Field::Volume | Field::Position,
                Some(
                    wire::media_action::Command::Operation(_)
                    | wire::media_action::Command::SourceId(_)
                    | wire::media_action::Command::GroupId(_),
                )
                | None,
            ) => panic!("wrong media quantity branch"),
            (Field::Volume, Some(wire::media_action::Command::PositionMillis(_)))
            | (Field::Position, Some(wire::media_action::Command::VolumeBasisPoints(_))) => {
                panic!("swapped media quantities")
            }
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
    snapshot.volume_basis_points = None;
    snapshot
        .capabilities
        .as_option_mut()
        .unwrap()
        .volume_basis_points
        .modify(|v| v.step = 100);
    let data = Data::new(snapshot, &"reader".parse().unwrap()).unwrap();
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
    snapshot.title = Some("New title".into());
    snapshot.position_millis = Some(20_000);
    let updated = Data::new(snapshot.clone(), &"reader".parse().unwrap()).unwrap();
    assert!(original.same_controls(&updated));
    snapshot.route.modify(|r| {
        r.coordinator_entity_id = "next-coordinator".into();
        r.topology_revision += 1;
    });
    let moved = Data::new(snapshot.clone(), &"reader".parse().unwrap()).unwrap();
    assert!(!original.same_controls(&moved));
    let request = moved
        .quantity_request(moved.parse(Field::Volume, "0").unwrap())
        .unwrap();
    assert_eq!(
        request.target.reference,
        Some(registry::entity_ref::Reference::EntityId("reader".into()))
    );
    snapshot.action_access = state::ActionAccess::Denied.into();
    let denied = Data::new(snapshot, &"reader".parse().unwrap()).unwrap();
    assert!(!moved.same_controls(&denied));
    assert!(
        denied
            .request(wire::media_action::Command::Operation(
                wire::Operation::Play.into()
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
    let image = decode_artwork(&png_bytes(2, 1), deadline, now).unwrap();
    assert_eq!((image.width(), image.height()), (2, 1));
    assert_eq!(image.rgba(), &[17; 8]);
    assert_eq!(image.expires(), deadline);
    assert!(matches!(
        decode_artwork(&png_bytes(513, 1), deadline, now),
        Err(ArtworkFailure::Capacity)
    ));
    assert!(matches!(
        decode_artwork(&vec![0; admit::MAX_ARTWORK_BYTES + 1], deadline, now),
        Err(ArtworkFailure::Capacity)
    ));
    assert!(matches!(
        decode_artwork(b"https://example.invalid/private-token", deadline, now),
        Err(ArtworkFailure::Invalid)
    ));
    assert!(matches!(
        decode_artwork(&png_bytes(2, 1), now, now),
        Err(ArtworkFailure::Expired)
    ));
}
