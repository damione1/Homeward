use buffa::EnumValue;
use buffa_types::google::protobuf::Timestamp;
use home_wire_contract::{
    home::{media::v1 as wire, state::v1 as state},
    media_contract as admit,
};

fn snapshot() -> wire::Snapshot {
    wire::Snapshot {
        route: wire::Route {
            entity_id: "opaque-reader".into(),
            endpoint: "playback".into(),
            coordinator_entity_id: "opaque-coordinator-a".into(),
            topology_revision: 1,
            admission_epoch: "1234567890abcdef1234567890abcdef".into(),
            config_revision: 7,
            ..Default::default()
        }
        .into(),
        capabilities: wire::Capabilities {
            operations: vec![wire::Operation::Play.into(), wire::Operation::Pause.into()],
            volume_basis_points: wire::QuantityBounds {
                minimum: 0,
                maximum: 10_000,
                step: 100,
                ..Default::default()
            }
            .into(),
            position_millis: wire::QuantityBounds {
                minimum: 0,
                maximum: 120_000,
                step: 1000,
                ..Default::default()
            }
            .into(),
            sources: vec![wire::Option {
                id: "opaque-source".into(),
                label: "Injected source".into(),
                ..Default::default()
            }],
            ..Default::default()
        }
        .into(),
        action_access: state::ActionAccess::Allowed.into(),
        playback: wire::PlaybackState::Paused.into(),
        volume_basis_points: Some(0),
        position_millis: Some(0),
        duration_millis: Some(120_000),
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
        provenance: "Synthetic mock".into(),
        ..Default::default()
    }
}
fn action(s: &wire::Snapshot, command: wire::media_action::Command) -> wire::MediaAction {
    wire::MediaAction {
        route: s.route.clone(),
        command: Some(command),
        ..Default::default()
    }
}

#[test]
fn zero_is_present_and_canonical_steps_are_enforced_without_float_rounding() {
    use wire::media_action::Command as C;
    let s = snapshot();
    assert!(admit::snapshot(&s, "opaque-reader"));
    assert!(admit::action(&action(&s, C::VolumeBasisPoints(0)), &s));
    assert!(admit::action(&action(&s, C::PositionMillis(0)), &s));
    assert!(!admit::action(&action(&s, C::VolumeBasisPoints(1)), &s));
    assert!(!admit::action(&action(&s, C::PositionMillis(120_001)), &s));
    assert!(!admit::action(
        &action(&s, C::SourceId("Injected source".into())),
        &s
    ));
    assert!(admit::action(
        &action(&s, C::SourceId("opaque-source".into())),
        &s
    ));
}

#[test]
fn coordinator_change_never_retargets_a_prepared_intention() {
    let mut s = snapshot();
    let prepared = action(
        &s,
        wire::media_action::Command::Operation(wire::Operation::Play.into()),
    );
    s.route.as_option_mut().unwrap().coordinator_entity_id = "opaque-coordinator-b".into();
    s.route.as_option_mut().unwrap().topology_revision += 1;
    assert!(!admit::action(&prepared, &s));
    assert_ne!(prepared.route.as_option(), s.route.as_option());
}

#[test]
fn reading_or_an_old_capability_is_never_write_permission() {
    let mut s = snapshot();
    let prepared = action(
        &s,
        wire::media_action::Command::Operation(wire::Operation::Play.into()),
    );
    s.action_access = state::ActionAccess::Denied.into();
    assert!(admit::snapshot(&s, "opaque-reader"));
    assert!(!admit::action(&prepared, &s));
    s.action_access = state::ActionAccess::Allowed.into();
    s.capabilities.as_option_mut().unwrap().operations.clear();
    assert!(!admit::action(&prepared, &s));
}

#[test]
fn foreign_entities_duplicate_options_unknown_variants_and_oversized_metadata_fail_closed() {
    let mut s = snapshot();
    assert!(!admit::snapshot(&s, "foreign-reader"));
    s.capabilities
        .as_option_mut()
        .unwrap()
        .operations
        .push(EnumValue::Unknown(999));
    assert!(!admit::snapshot(&s, "opaque-reader"));
    s = snapshot();
    let duplicate = s.capabilities.as_option().unwrap().sources[0].clone();
    s.capabilities
        .as_option_mut()
        .unwrap()
        .sources
        .push(duplicate);
    assert!(!admit::snapshot(&s, "opaque-reader"));
    s = snapshot();
    s.title = Some("x".repeat(1025));
    assert!(!admit::snapshot(&s, "opaque-reader"));
    s = snapshot();
    s.quality.as_option_mut().unwrap().availability = EnumValue::Unknown(999);
    assert!(!admit::snapshot(&s, "opaque-reader"));
}

#[test]
fn artwork_is_an_opaque_bounded_reference_with_an_explicit_expiry() {
    let mut s = snapshot();
    let mut reference = wire::ArtworkReference {
        reference: vec![17; 32],
        expires_at: Timestamp {
            seconds: 1_790_000_030,
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    s.artwork = reference.clone().into();
    assert!(admit::snapshot(&s, "opaque-reader"));
    reference.reference.resize(513, 17);
    s.artwork = reference.clone().into();
    assert!(!admit::snapshot(&s, "opaque-reader"));
    reference.reference.clear();
    s.artwork = reference.into();
    assert!(!admit::snapshot(&s, "opaque-reader"));
}
