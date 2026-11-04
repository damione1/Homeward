use buffa::{EnumValue, Message};
use home_client_model::dashboard::{
    CommandKind, Comparison, Gesture, Intention, Literal, Predicate, TextLiteral, Visibility,
};
use home_wire_contract::{
    dashboard_contract::{RecipeFailure, decode_interactions, decode_predicate},
    home::api::v2 as w,
};

fn source() -> w::DashboardLayoutSource {
    w::DashboardLayoutSource {
        file: "synthetic.yaml".into(),
        path: "$.views[0].sections[0].cards[0]".into(),
        variant: "synthetic".into(),
        line: 7,
        column: 3,
        ..Default::default()
    }
}
fn recipes() -> w::DashboardInteractions {
    w::DashboardInteractions {
        interactions: vec![w::DashboardInteraction {
            gesture: w::DashboardGesture::Tap.into(),
            intention: Some(w::dashboard_interaction::Intention::Command(
                w::DashboardCommand {
                    capability: "homes/synthetic/entities/synthetic-lock/capabilities/lock".into(),
                    kind: w::DashboardCommandKind::Unlock.into(),
                    ..Default::default()
                }
                .into(),
            )),
            confirmation: w::DashboardConfirmation {
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
fn predicate(condition: w::dashboard_predicate::Condition) -> w::DashboardPredicate {
    w::DashboardPredicate {
        condition: Some(condition),
        source: source().into(),
        ..Default::default()
    }
}

#[test]
fn wire_roundtrip_preserves_explicit_none_each_gesture_and_empty_confirmation() {
    let mut wire = recipes();
    wire.interactions.push(w::DashboardInteraction {
        gesture: w::DashboardGesture::Hold.into(),
        intention: Some(w::dashboard_interaction::Intention::None(
            w::DashboardNoAction::default().into(),
        )),
        source: source().into(),
        ..Default::default()
    });
    wire.interactions.push(w::DashboardInteraction {
        gesture: w::DashboardGesture::DoubleTap.into(),
        intention: Some(w::dashboard_interaction::Intention::MoreInfo(
            w::DashboardEntityTarget {
                entity: "homes/synthetic/entities/synthetic-lock".into(),
                ..Default::default()
            }
            .into(),
        )),
        source: source().into(),
        ..Default::default()
    });
    let bytes = wire.encode_to_vec();
    let decoded = w::DashboardInteractions::decode_from_slice(&bytes).unwrap();
    assert_eq!(decoded.encode_to_vec(), bytes);
    let model = decode_interactions(&decoded).unwrap();
    assert_eq!(model.len(), 3);
    assert_eq!(model[&Gesture::Tap].confirmation.as_deref(), Some(""));
    assert!(matches!(
        model[&Gesture::Tap].intention,
        Intention::Command {
            kind: CommandKind::Unlock,
            ..
        }
    ));
    assert_eq!(model[&Gesture::Hold].intention, Intention::None);
    assert_eq!(model[&Gesture::DoubleTap].source.file, "synthetic.yaml");
    let mut duplicate = wire.clone();
    duplicate.interactions[2].gesture = w::DashboardGesture::Tap.into();
    assert_eq!(decode_interactions(&duplicate), Err(RecipeFailure::Invalid));
    wire.interactions[1].confirmation = w::DashboardConfirmation::default().into();
    assert_eq!(decode_interactions(&wire), Err(RecipeFailure::Unsupported));
}

#[test]
fn unknown_semantics_at_every_nested_boundary_never_fall_back_to_a_command() {
    let mut cases = Vec::new();
    cases.push(unknown(&recipes()));
    let mut obsolete_version = recipes().encode_to_vec();
    obsolete_version.extend_from_slice(&[0x08, 99]); // retired version negotiation
    cases.push(w::DashboardInteractions::decode_from_slice(&obsolete_version).unwrap());
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
    wire.interactions[0].confirmation = unknown(&w::DashboardConfirmation::default()).into();
    cases.push(wire);
    let mut wire = recipes();
    let Some(w::dashboard_interaction::Intention::Command(target)) =
        &mut wire.interactions[0].intention
    else {
        panic!()
    };
    target.kind = EnumValue::Unknown(99);
    cases.push(wire);
    let mut wire = recipes();
    let Some(w::dashboard_interaction::Intention::Command(target)) =
        &mut wire.interactions[0].intention
    else {
        panic!()
    };
    **target = unknown(target.as_ref());
    cases.push(wire);
    for wire in cases {
        assert_eq!(decode_interactions(&wire), Err(RecipeFailure::Unsupported));
    }
    for (line, column) in [(-1, 3), (7, -1)] {
        let mut wire = recipes();
        let mut location = source();
        location.line = line;
        location.column = column;
        wire.interactions[0].source = location.into();
        assert_eq!(decode_interactions(&wire), Err(RecipeFailure::Invalid));
    }
    let mut wire = recipes();
    wire.interactions[0].intention = None;
    assert_eq!(decode_interactions(&wire), Err(RecipeFailure::Invalid));
}

#[test]
fn navigation_has_only_a_resolved_typed_destination_and_never_a_device_intention() {
    let mut wire = recipes();
    let destination = w::DashboardDestination {
        dashboard: "homes/synthetic/dashboards/synthetic-dashboard".into(),
        node_id: "configured-source-identity".into(),
        presentation: w::DashboardPresentation::Popup.into(),
        ..Default::default()
    };
    wire.interactions[0].intention = Some(w::dashboard_interaction::Intention::Navigate(
        destination.clone().into(),
    ));
    let model = decode_interactions(&wire).unwrap();
    let Intention::Navigate(target) = &model[&Gesture::Tap].intention else {
        panic!()
    };
    assert_eq!(
        target.dashboard.as_str(),
        "homes/synthetic/dashboards/synthetic-dashboard"
    );
    assert_eq!(target.node.as_str(), "configured-source-identity");
    let mut future = destination;
    future.presentation = EnumValue::Unknown(99);
    wire.interactions[0].intention =
        Some(w::dashboard_interaction::Intention::Navigate(future.into()));
    assert_eq!(decode_interactions(&wire), Err(RecipeFailure::Unsupported));
}

#[test]
fn missing_observations_are_not_values_and_empty_strings_are_preserved() {
    let mut wire = predicate(w::dashboard_predicate::Condition::State(
        w::DashboardStatePredicate {
            field: w::DashboardFieldBinding {
                capability: "homes/synthetic/entities/synthetic-helper/capabilities/helper".into(),
                field: w::DashboardFieldKey::Value.into(),
                ..Default::default()
            }
            .into(),
            expected: w::DashboardLiteral {
                value: Some(w::dashboard_literal::Value::TextValue(String::new())),
                ..Default::default()
            }
            .into(),
            comparison: w::DashboardComparison::NotEqual.into(),
            ..Default::default()
        }
        .into(),
    ));
    let model = decode_predicate(&wire).unwrap();
    assert_eq!(model.evaluate(&|_| None), Visibility::Unavailable);
    let empty = Literal::Text(TextLiteral::try_from("").unwrap());
    let off = Literal::Text(TextLiteral::try_from("off").unwrap());
    assert_eq!(model.evaluate(&|_| Some(&empty)), Visibility::Hidden);
    assert_eq!(model.evaluate(&|_| Some(&off)), Visibility::Visible);
    let Predicate::State { comparison, .. } = model else {
        panic!()
    };
    assert_eq!(comparison, Comparison::NotEqual);
    wire = unknown(&wire);
    assert_eq!(decode_predicate(&wire), Err(RecipeFailure::Unsupported));
}

#[test]
fn nested_visibility_has_real_depth_and_total_node_budgets() {
    use w::dashboard_predicate::Condition;
    let leaf = predicate(Condition::Constant(false));
    let either = predicate(Condition::Any(
        w::DashboardPredicateList {
            conditions: vec![leaf.clone(), predicate(Condition::Constant(true))],
            ..Default::default()
        }
        .into(),
    ));
    assert_eq!(
        decode_predicate(&either).unwrap().evaluate(&|_| None),
        Visibility::Visible
    );
    let mut nested = leaf.clone();
    for _ in 0..8 {
        nested = predicate(Condition::All(
            w::DashboardPredicateList {
                conditions: vec![nested],
                ..Default::default()
            }
            .into(),
        ));
    }
    assert!(decode_predicate(&nested).is_ok());
    let too_deep = predicate(Condition::All(
        w::DashboardPredicateList {
            conditions: vec![nested],
            ..Default::default()
        }
        .into(),
    ));
    assert_eq!(decode_predicate(&too_deep), Err(RecipeFailure::Budget));
    let too_wide = predicate(Condition::All(
        w::DashboardPredicateList {
            conditions: vec![leaf; 64],
            ..Default::default()
        }
        .into(),
    ));
    assert_eq!(decode_predicate(&too_wide), Err(RecipeFailure::Budget));
    let empty = predicate(Condition::Any(w::DashboardPredicateList::default().into()));
    assert_eq!(decode_predicate(&empty), Err(RecipeFailure::Invalid));
}

#[test]
fn predicate_literals_preserve_types_false_zero_and_empty_without_coercion() {
    use home_wire_contract::dashboard_contract::decode_literal;
    use w::dashboard_literal::Value as V;
    let decode = |value| {
        decode_literal(&w::DashboardLiteral {
            value: Some(value),
            ..Default::default()
        })
    };
    assert_eq!(decode(V::BooleanValue(false)), Ok(Literal::Boolean(false)));
    assert_eq!(decode(V::IntegerValue(0)), Ok(Literal::Integer(0)));
    let text = decode(V::TextValue("0".into())).unwrap();
    let integer = decode(V::IntegerValue(0)).unwrap();
    let number = decode(V::NumberValue(0.0)).unwrap();
    let decimal = decode(V::DecimalValue("0.00".into())).unwrap();
    assert_ne!(text, integer);
    assert_ne!(integer, number);
    assert_ne!(number, decimal);
    assert_eq!(
        decode(V::TextValue(String::new())),
        Ok(Literal::Text(TextLiteral::try_from("").unwrap()))
    );
    for value in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY] {
        assert_eq!(decode(V::NumberValue(value)), Err(RecipeFailure::Invalid));
    }
    for value in ["", "1e2", "01", "1.", ".5", "NaN"] {
        assert_eq!(
            decode(V::DecimalValue(value.into())),
            Err(RecipeFailure::Invalid)
        );
    }
    assert_eq!(
        decode(V::CoverState(EnumValue::Unknown(99))),
        Err(RecipeFailure::Unsupported)
    );
    assert_eq!(
        decode(V::PowerValue(w::PowerValue::Unspecified.into())),
        Err(RecipeFailure::Unsupported)
    );
    let value = w::DashboardLiteral {
        value: Some(V::TextValue("actual helper text".into())),
        ..Default::default()
    };
    assert_eq!(
        decode_literal(&unknown(&value)),
        Err(RecipeFailure::Unsupported)
    );
    assert_eq!(
        decode_literal(&w::DashboardLiteral::default()),
        Err(RecipeFailure::Invalid)
    );
}
