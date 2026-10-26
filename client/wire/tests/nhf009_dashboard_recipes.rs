use buffa::{EnumValue, Message};
use home_client_model::dashboard::{
    CommandKind, Comparison, Gesture, Intention, Predicate, Visibility,
};
use home_wire_contract::{
    dashboard_contract::{RecipeFailure, decode_recipes},
    home::dashboard::v1 as w,
};

fn source() -> w::LayoutSource {
    w::LayoutSource {
        file: "synthetic.yaml".into(),
        path: "$.views[0].sections[0].cards[0]".into(),
        variant: "synthetic".into(),
        line: 7,
        column: 3,
        ..Default::default()
    }
}
fn recipes() -> w::NativeRecipes {
    w::NativeRecipes {
        version: 1,
        interactions: vec![w::NativeInteraction {
            gesture: w::NativeGesture::Tap.into(),
            intention: Some(w::native_interaction::Intention::Command(
                w::NativeCommand {
                    entity_id: "synthetic-lock".into(),
                    kind: w::NativeCommandKind::Unlock.into(),
                    ..Default::default()
                }
                .into(),
            )),
            confirmation: w::NativeConfirmation {
                text: String::new(),
                ..Default::default()
            }
            .into(),
            source: source().into(),
            ..Default::default()
        }],
        ..Default::default()
    }
}
fn unknown<M: Message + Default>(message: &M) -> M {
    let mut bytes = message.encode_to_vec();
    bytes.extend_from_slice(&[0x98, 0x06, 0x07]); // future field 99
    M::decode_from_slice(&bytes).unwrap()
}
fn predicate(condition: w::native_predicate::Condition) -> w::NativePredicate {
    w::NativePredicate {
        condition: Some(condition),
        source: source().into(),
        ..Default::default()
    }
}

#[test]
fn wire_roundtrip_preserves_explicit_none_each_gesture_and_empty_confirmation() {
    let mut wire = recipes();
    wire.interactions.push(w::NativeInteraction {
        gesture: w::NativeGesture::Hold.into(),
        intention: Some(w::native_interaction::Intention::None(
            w::NativeNoAction::default().into(),
        )),
        source: source().into(),
        ..Default::default()
    });
    wire.interactions.push(w::NativeInteraction {
        gesture: w::NativeGesture::DoubleTap.into(),
        intention: Some(w::native_interaction::Intention::MoreInfo(
            w::NativeEntityTarget {
                entity_id: "synthetic-lock".into(),
                ..Default::default()
            }
            .into(),
        )),
        source: source().into(),
        ..Default::default()
    });
    let bytes = wire.encode_to_vec();
    let decoded = w::NativeRecipes::decode_from_slice(&bytes).unwrap();
    assert_eq!(decoded.encode_to_vec(), bytes);
    let model = decode_recipes(&decoded).unwrap();
    assert_eq!(model.interactions.len(), 3);
    assert_eq!(
        model.interactions[&Gesture::Tap].confirmation.as_deref(),
        Some("")
    );
    assert!(matches!(
        model.interactions[&Gesture::Tap].intention,
        Intention::Command {
            kind: CommandKind::Unlock,
            ..
        }
    ));
    assert_eq!(
        model.interactions[&Gesture::Hold].intention,
        Intention::None
    );
    assert_eq!(
        model.interactions[&Gesture::DoubleTap].source.file,
        "synthetic.yaml"
    );
    let mut duplicate = wire.clone();
    duplicate.interactions[2].gesture = w::NativeGesture::Tap.into();
    assert_eq!(decode_recipes(&duplicate), Err(RecipeFailure::Invalid));
    wire.interactions[1].confirmation = w::NativeConfirmation::default().into();
    assert_eq!(decode_recipes(&wire), Err(RecipeFailure::Unsupported));
}

#[test]
fn unknown_semantics_at_every_nested_boundary_never_fall_back_to_a_command() {
    let mut cases = Vec::new();
    cases.push(unknown(&recipes()));
    let mut wire = recipes();
    wire.version = 99;
    cases.push(wire);
    let mut wire = recipes();
    wire.interactions[0] = unknown(&wire.interactions[0]);
    cases.push(wire);
    let mut wire = recipes();
    wire.interactions[0].gesture = EnumValue::Unknown(99);
    cases.push(wire);
    let mut wire = recipes();
    wire.interactions[0].source = unknown(&source()).into();
    cases.push(wire);
    let mut wire = recipes();
    wire.interactions[0].confirmation = unknown(&w::NativeConfirmation::default()).into();
    cases.push(wire);
    let mut wire = recipes();
    let Some(w::native_interaction::Intention::Command(target)) =
        &mut wire.interactions[0].intention
    else {
        panic!()
    };
    target.kind = EnumValue::Unknown(99);
    cases.push(wire);
    let mut wire = recipes();
    let Some(w::native_interaction::Intention::Command(target)) =
        &mut wire.interactions[0].intention
    else {
        panic!()
    };
    **target = unknown(target.as_ref());
    cases.push(wire);
    for wire in cases {
        assert_eq!(decode_recipes(&wire), Err(RecipeFailure::Unsupported));
    }
    let mut wire = recipes();
    wire.interactions[0].intention = None;
    assert_eq!(decode_recipes(&wire), Err(RecipeFailure::Invalid));
}

#[test]
fn navigation_has_only_a_resolved_typed_destination_and_never_a_device_intention() {
    let mut wire = recipes();
    let destination = w::NativeDestination {
        dashboard_id: w::DashboardId {
            value: "synthetic-dashboard".into(),
            ..Default::default()
        }
        .into(),
        node_id: "configured-source-identity".into(),
        presentation: w::NativePresentation::Popup.into(),
        ..Default::default()
    };
    wire.interactions[0].intention = Some(w::native_interaction::Intention::Navigate(
        destination.clone().into(),
    ));
    let model = decode_recipes(&wire).unwrap();
    let Intention::Navigate(target) = &model.interactions[&Gesture::Tap].intention else {
        panic!()
    };
    assert_eq!(target.dashboard.as_str(), "synthetic-dashboard");
    assert_eq!(target.node.as_str(), "configured-source-identity");
    let mut future = destination;
    future.presentation = EnumValue::Unknown(99);
    wire.interactions[0].intention =
        Some(w::native_interaction::Intention::Navigate(future.into()));
    assert_eq!(decode_recipes(&wire), Err(RecipeFailure::Unsupported));
}

#[test]
fn missing_observations_are_not_values_and_empty_strings_are_preserved() {
    let mut wire = recipes();
    wire.visibility = predicate(w::native_predicate::Condition::State(
        w::NativeStatePredicate {
            entity_id: "synthetic-helper".into(),
            state: String::new(),
            comparison: w::NativeComparison::NotEqual.into(),
            ..Default::default()
        }
        .into(),
    ))
    .into();
    let model = decode_recipes(&wire).unwrap();
    let predicate = model.visibility.unwrap();
    assert_eq!(predicate.evaluate(&|_| None), Visibility::Unavailable);
    assert_eq!(predicate.evaluate(&|_| Some("")), Visibility::Hidden);
    assert_eq!(predicate.evaluate(&|_| Some("off")), Visibility::Visible);
    let Predicate::State { comparison, .. } = predicate else {
        panic!()
    };
    assert_eq!(comparison, Comparison::NotEqual);
    wire.visibility = unknown(wire.visibility.as_option().unwrap()).into();
    assert_eq!(decode_recipes(&wire), Err(RecipeFailure::Unsupported));
}

#[test]
fn nested_visibility_has_real_depth_and_total_node_budgets() {
    use w::native_predicate::Condition;
    let leaf = predicate(Condition::Constant(false));
    let mut wire = recipes();
    wire.visibility = predicate(Condition::Any(
        w::NativePredicateList {
            conditions: vec![leaf.clone(), predicate(Condition::Constant(true))],
            ..Default::default()
        }
        .into(),
    ))
    .into();
    assert_eq!(
        decode_recipes(&wire)
            .unwrap()
            .visibility
            .unwrap()
            .evaluate(&|_| None),
        Visibility::Visible
    );
    let mut nested = leaf.clone();
    for _ in 0..8 {
        nested = predicate(Condition::All(
            w::NativePredicateList {
                conditions: vec![nested],
                ..Default::default()
            }
            .into(),
        ));
    }
    wire.visibility = nested.clone().into();
    assert!(decode_recipes(&wire).is_ok());
    wire.visibility = predicate(Condition::All(
        w::NativePredicateList {
            conditions: vec![nested],
            ..Default::default()
        }
        .into(),
    ))
    .into();
    assert_eq!(decode_recipes(&wire), Err(RecipeFailure::Budget));
    wire.visibility = predicate(Condition::All(
        w::NativePredicateList {
            conditions: vec![leaf; 64],
            ..Default::default()
        }
        .into(),
    ))
    .into();
    assert_eq!(decode_recipes(&wire), Err(RecipeFailure::Budget));
    wire.visibility = predicate(Condition::Any(w::NativePredicateList::default().into())).into();
    assert_eq!(decode_recipes(&wire), Err(RecipeFailure::Invalid));
}
