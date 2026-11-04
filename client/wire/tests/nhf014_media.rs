use buffa::EnumValue;
use home_wire_contract::{api_playback_contract::playback, home::api::v2 as wire};

fn capability() -> wire::Capability {
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
                        step: 100,
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
fn media(value: &mut wire::Capability) -> &mut wire::MediaPlaybackCapability {
    let Some(wire::capability::Trait::MediaPlayback(media)) = &mut value.r#trait else {
        panic!("fixture trait")
    };
    media
}
#[test]
fn zero_false_and_unknown_playback_remain_independent() {
    let value = playback(&capability()).unwrap().unwrap();
    let reading = value.observation().unwrap();
    assert_eq!(reading.volume().unwrap().value(), 0);
    assert_eq!(reading.powered_on(), Some(false));
    assert_eq!(
        reading.playback(),
        home_client_model::api_playback::PlaybackState::Unknown
    );
    assert!(value.can_prepare());
    let bounds = value.definition().unwrap().volume().unwrap().quantity();
    assert!(bounds.admits(0));
    assert!(!bounds.admits(1));
}
#[test]
fn both_grants_and_actual_definition_are_required_to_prepare() {
    let mut raw = capability();
    raw.action_access = wire::ActionAccess::Denied.into();
    assert!(!playback(&raw).unwrap().unwrap().can_prepare());
    raw.action_access = wire::ActionAccess::Allowed.into();
    media(&mut raw).action_access = wire::ActionAccess::Denied.into();
    assert!(!playback(&raw).unwrap().unwrap().can_prepare());
    media(&mut raw).action_access = wire::ActionAccess::Allowed.into();
    media(&mut raw).definition = Default::default();
    let parsed = playback(&raw).unwrap().unwrap();
    assert!(parsed.definition().is_none());
    assert!(!parsed.can_prepare());
}
#[test]
fn foreign_route_future_enum_and_duplicate_support_fail_closed() {
    let mut raw = capability();
    media(&mut raw)
        .observation
        .as_option_mut()
        .unwrap()
        .route
        .as_option_mut()
        .unwrap()
        .entity = "homes/test/entities/foreign".into();
    assert!(playback(&raw).is_err());
    let mut raw = capability();
    media(&mut raw)
        .observation
        .as_option_mut()
        .unwrap()
        .playback = EnumValue::Unknown(999);
    assert!(playback(&raw).is_err());
    let mut raw = capability();
    media(&mut raw)
        .definition
        .as_option_mut()
        .unwrap()
        .operations
        .push(wire::MediaOperation::Play.into());
    assert!(playback(&raw).is_err());
    let mut raw = capability();
    media(&mut raw).data_provenance = EnumValue::Unknown(999);
    assert!(playback(&raw).is_err());
}
#[test]
fn stale_or_absent_observation_never_authorizes_commands() {
    let mut raw = capability();
    media(&mut raw)
        .observation
        .as_option_mut()
        .unwrap()
        .quality
        .as_option_mut()
        .unwrap()
        .freshness = wire::Freshness::Stale.into();
    assert!(!playback(&raw).unwrap().unwrap().can_prepare());
    media(&mut raw).observation = Default::default();
    let parsed = playback(&raw).unwrap().unwrap();
    assert!(parsed.observation().is_none());
    assert!(!parsed.can_prepare());
}
