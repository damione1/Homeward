use home_client_model::{
    api::{CapabilityName, DashboardName, Power},
    dashboard::{Comparison, FieldBinding, FieldKey, Literal, Predicate, Visibility},
};

#[test]
fn dashboard_names_are_canonical_resources_not_source_ids_or_routes() {
    let name = DashboardName::try_from("homes/synthetic/dashboards/overview").unwrap();
    assert_eq!(name.home().as_str(), "homes/synthetic");
    for value in [
        "overview",
        "/overview",
        "homes/synthetic/entities/overview",
        "homes/synthetic/dashboards/../overview",
        "homes/synthetic/dashboards/Overview",
        "homes/synthetic/dashboards/overview/",
    ] {
        assert!(DashboardName::try_from(value).is_err(), "{value}");
    }
}

#[test]
fn visibility_uses_the_exact_capability_and_missing_never_satisfies_inequality() {
    let selected =
        CapabilityName::try_from("homes/synthetic/entities/relay/capabilities/power").unwrap();
    let sibling =
        CapabilityName::try_from("homes/synthetic/entities/relay/capabilities/contact").unwrap();
    let selected = FieldBinding {
        capability: selected,
        field: FieldKey::State,
    };
    let sibling = FieldBinding {
        capability: sibling,
        field: FieldKey::State,
    };
    let on = Literal::Power(Power::On);
    let off = Literal::Power(Power::Off);
    let predicate = Predicate::State {
        field: selected.clone(),
        expected: off.clone(),
        comparison: Comparison::NotEqual,
    };
    assert_eq!(
        predicate.dependencies().into_iter().collect::<Vec<_>>(),
        vec![&selected]
    );
    assert_eq!(
        predicate.evaluate(&|name| (name == &sibling).then_some(&on)),
        Visibility::Unavailable
    );
    assert_eq!(
        predicate.evaluate(&|name| (name == &selected).then_some(&on)),
        Visibility::Visible
    );
    assert_eq!(
        predicate.evaluate(&|name| (name == &selected).then_some(&off)),
        Visibility::Hidden
    );
    let other_family = Literal::Boolean(true);
    assert_eq!(
        predicate.evaluate(&|name| (name == &selected).then_some(&other_family)),
        Visibility::Unavailable,
        "a type mismatch must not satisfy inequality"
    );
}
