use buffa::Message;
use home_wire_contract::home::{actions::v1 as actions, registry::v1 as registry};

#[test]
fn nha178_light_seconds_keep_absence_and_explicit_zero() {
    let absent = actions::SetLight::default().with_on(false);
    let decoded = actions::SetLight::decode_from_slice(&absent.encode_to_vec()).unwrap();
    assert_eq!(decoded.on, Some(false));
    assert_eq!(decoded.transition_seconds, None);

    let zero = actions::SetLight::default()
        .with_on(false)
        .with_transition_seconds(0.0);
    let decoded = actions::SetLight::decode_from_slice(&zero.encode_to_vec()).unwrap();
    assert_eq!(decoded.on, Some(false));
    assert_eq!(decoded.transition_seconds, Some(0.0));
    assert_ne!(absent.encode_to_vec(), zero.encode_to_vec());

    let old = registry::LightConstraints::default();
    assert!(old.transition_seconds.as_option().is_none());
    let newer = registry::LightConstraints {
        transition_seconds: registry::NumberBounds::default().into(),
        ..Default::default()
    };
    let decoded = registry::LightConstraints::decode_from_slice(&newer.encode_to_vec()).unwrap();
    assert!(decoded.transition_seconds.as_option().is_some());
}
