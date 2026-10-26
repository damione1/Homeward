//! One visibility decision for rendering and device admission. Only exact
//! literals from a fresh authorized cache observation enter a predicate.
use crate::{
    dashboard::Layout,
    dashboard_layout::{NativeLayout, Node, NodeId},
};
use buffa::EnumValue;
use home_client_cache::{CachePhase, CachedView};
use home_client_model::{EntityId, dashboard::Visibility};
use home_wire_contract::home::{
    helpers::v1 as helpers, registry::v1 as registry, state::v1 as state,
};
use std::collections::{BTreeMap, BTreeSet};

fn on_off(value: bool) -> &'static str {
    if value { "on" } else { "off" }
}

fn fresh(entity: &state::ViewEntity) -> Option<&state::ObservedState> {
    if !entity.__buffa_unknown_fields.is_empty() {
        return None;
    }
    let observation = entity.state.as_option()?;
    let quality = observation.quality.as_option()?;
    let target = observation.entity.as_option()?;
    if !observation.__buffa_unknown_fields.is_empty()
        || !quality.__buffa_unknown_fields.is_empty()
        || !target.__buffa_unknown_fields.is_empty()
        || quality.availability != EnumValue::Known(state::Availability::Available)
        || quality.freshness != EnumValue::Known(state::Freshness::Fresh)
        || quality.provenance != EnumValue::Known(state::Provenance::Observed)
        || !matches!(&target.reference, Some(registry::entity_ref::Reference::EntityId(id)) if id == &entity.entity_id)
    {
        return None;
    }
    Some(observation)
}
pub(crate) fn power(entity: &state::ViewEntity) -> Option<bool> {
    use state::observed_state::Value;
    match fresh(entity)?.value.as_ref()? {
        Value::SwitchOn(value) => Some(*value),
        Value::Light(value) if value.__buffa_unknown_fields.is_empty() => value.on,
        Value::Fan(value) if value.__buffa_unknown_fields.is_empty() => match value.speed {
            EnumValue::Known(state::FanSpeed::Off) => Some(false),
            EnumValue::Known(
                state::FanSpeed::Low
                | state::FanSpeed::Medium
                | state::FanSpeed::MediumHigh
                | state::FanSpeed::High,
            ) => Some(true),
            EnumValue::Known(state::FanSpeed::Unspecified) | EnumValue::Unknown(_) => None,
        },
        Value::Light(_)
        | Value::Fan(_)
        | Value::Measurement(_)
        | Value::Climate(_)
        | Value::Helper(_)
        | Value::Solar(_)
        | Value::SolarEvent(_)
        | Value::Cover(_)
        | Value::Lock(_)
        | Value::Alarm(_) => None,
    }
}
fn literal(entity: &state::ViewEntity) -> Option<&str> {
    let observation = fresh(entity)?;
    use state::observed_state::Value;
    let value = match observation.value.as_ref()? {
        Value::SwitchOn(value) => Some(on_off(*value)),
        Value::Light(value) => {
            if value.__buffa_unknown_fields.is_empty() {
                value.on.map(on_off)
            } else {
                None
            }
        }
        Value::Fan(value) => {
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            }
            match value.speed {
                EnumValue::Known(state::FanSpeed::Off) => Some("off"),
                EnumValue::Known(
                    state::FanSpeed::Low
                    | state::FanSpeed::Medium
                    | state::FanSpeed::MediumHigh
                    | state::FanSpeed::High,
                ) => Some("on"),
                EnumValue::Known(state::FanSpeed::Unspecified) | EnumValue::Unknown(_) => None,
            }
        }
        Value::Climate(value) => {
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            }
            match value.mode {
                Some(EnumValue::Known(registry::ClimateMode::Off)) => Some("off"),
                Some(EnumValue::Known(registry::ClimateMode::Heat)) => Some("heat"),
                Some(EnumValue::Known(registry::ClimateMode::Cool)) => Some("cool"),
                Some(EnumValue::Known(registry::ClimateMode::Auto)) => Some("auto"),
                Some(EnumValue::Known(registry::ClimateMode::Dry)) => Some("dry"),
                Some(EnumValue::Known(registry::ClimateMode::FanOnly)) => Some("fan_only"),
                Some(EnumValue::Known(registry::ClimateMode::Unspecified))
                | Some(EnumValue::Unknown(_))
                | None => None,
            }
        }
        Value::Helper(value) => {
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            }
            use helpers::helper_value::Value;
            match value.value.as_ref()? {
                Value::Boolean(value) => Some(on_off(*value)),
                Value::Text(value) | Value::SelectedOption(value) => Some(value.as_str()),
                // These typed values do not retain the original HA state
                // spelling. Never substitute a rounded/localized UI value.
                Value::Number(_)
                | Value::Counter(_)
                | Value::ButtonPressedAt(_)
                | Value::Date(_)
                | Value::Time(_)
                | Value::Datetime(_) => None,
            }
        }
        Value::Measurement(value) => {
            if value.__buffa_unknown_fields.is_empty() && value.value.is_finite() {
                value
                    .decimal_value
                    .as_deref()
                    .filter(|literal| literal.parse::<f64>().is_ok_and(f64::is_finite))
            } else {
                None
            }
        }
        Value::Cover(value) => {
            home_wire_contract::controls_contract::cover_value(value).then_some(())?;
            value
                .state
                .and_then(home_wire_contract::controls_contract::cover_state)
                .and_then(crate::control_text::cover_state)
                .map(|text| text.literal)
        }
        Value::Lock(value) => {
            home_wire_contract::controls_contract::lock_value(value).then_some(())?;
            home_wire_contract::controls_contract::lock_state(value.state)
                .and_then(crate::control_text::lock_state)
                .map(|text| text.literal)
        }
        Value::Alarm(value) => {
            home_wire_contract::controls_contract::alarm_value(value).then_some(())?;
            home_wire_contract::controls_contract::alarm_state(value.state)
                .and_then(crate::control_text::alarm_state)
                .map(|text| text.literal)
        }
        Value::Solar(_) | Value::SolarEvent(_) => None,
    }?;
    (value.len() <= 256 && !value.contains('\0')).then_some(value)
}

pub(crate) fn project(
    layout: &Layout,
    root: &NodeId,
    phase: CachePhase,
    view: Option<&CachedView>,
) -> Vec<Node> {
    let Some(native) = layout.native.as_ref() else {
        return Vec::new();
    };
    let Some(view) = view.filter(|view| layout.compatible(view)) else {
        return Vec::new();
    };
    let dependencies = native
        .nodes
        .values()
        .filter_map(|node| node.recipes.as_ref())
        .filter_map(|recipes| recipes.visibility.as_ref())
        .flat_map(|predicate| predicate.dependencies())
        .collect::<BTreeSet<_>>();
    let states = if phase == CachePhase::Live {
        dependencies
            .into_iter()
            .filter_map(|id| Some((id.clone(), literal(view.get(id)?)?)))
            .collect()
    } else {
        BTreeMap::new()
    };
    project_nodes(native, root, &states)
}

fn project_nodes(
    native: &NativeLayout,
    root: &NodeId,
    states: &BTreeMap<EntityId, &str>,
) -> Vec<Node> {
    let visibility = |node: &Node| {
        node.recipes
            .as_ref()
            .and_then(|recipes| recipes.visibility.as_ref())
            .map_or(Visibility::Visible, |predicate| {
                predicate.evaluate(&|id| states.get(id).copied())
            })
    };
    // Opening a popup must also respect the visibility of its configured
    // ancestors, even though those ancestors are outside the displayed root.
    let parents = native
        .nodes
        .values()
        .flat_map(|node| node.children.iter().map(|child| (child, &node.id)))
        .collect::<BTreeMap<_, _>>();
    let mut ancestor = root;
    while let Some(parent) = parents.get(ancestor) {
        let Some(node) = native.nodes.get(*parent) else {
            return Vec::new();
        };
        match visibility(node) {
            Visibility::Hidden => return Vec::new(),
            Visibility::Unavailable => {
                return native
                    .nodes
                    .get(root)
                    .map(unavailable)
                    .into_iter()
                    .collect();
            }
            Visibility::Visible => {}
        }
        ancestor = parent;
    }
    let mut nodes = Vec::new();
    let mut pruned = BTreeSet::new();
    for node in native.ordered_nodes(root) {
        if pruned.contains(&node.id) {
            pruned.extend(node.children.iter().cloned());
            continue;
        }
        match visibility(node) {
            Visibility::Hidden => {
                pruned.extend(node.children.iter().cloned());
            }
            Visibility::Unavailable => {
                pruned.extend(node.children.iter().cloned());
                nodes.push(unavailable(node));
            }
            Visibility::Visible => {
                nodes.push(node.clone());
            }
        }
    }
    nodes
}
fn unavailable(node: &Node) -> Node {
    let mut node = node.clone();
    node.diagnostic.clear();
    node.inert("Condition de visibilité non disponible dans la vue autorisée à jour.");
    node.children.clear();
    node
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dashboard_layout::Kind;
    use home_client_model::dashboard::{Comparison, Predicate, Recipes};

    fn entity(value: state::observed_state::Value) -> state::ViewEntity {
        state::ViewEntity {
            entity_id: "synthetic-observation".into(),
            state: state::ObservedState {
                entity: registry::EntityRef {
                    reference: Some(registry::entity_ref::Reference::EntityId(
                        "synthetic-observation".into(),
                    )),
                    ..Default::default()
                }
                .into(),
                quality: state::Quality {
                    availability: state::Availability::Available.into(),
                    freshness: state::Freshness::Fresh.into(),
                    provenance: state::Provenance::Observed.into(),
                    ..Default::default()
                }
                .into(),
                value: Some(value),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        }
    }
    #[test]
    fn literals_preserve_observed_false_and_empty_text_without_localized_fallbacks() {
        let false_value = entity(state::observed_state::Value::SwitchOn(false));
        assert_eq!(literal(&false_value), Some("off"));
        let empty = entity(state::observed_state::Value::Helper(
            helpers::HelperValue {
                value: Some(helpers::helper_value::Value::Text(String::new())),
                ..Default::default()
            }
            .into(),
        ));
        assert_eq!(literal(&empty), Some(""));
        let absent = entity(state::observed_state::Value::Light(
            state::LightValue::default().into(),
        ));
        assert_eq!(literal(&absent), None);
        let lossy = entity(state::observed_state::Value::Measurement(
            state::Measurement {
                value: 12.5,
                unit: "unit".into(),
                ..Default::default()
            }
            .into(),
        ));
        assert_eq!(
            literal(&lossy),
            None,
            "the rendered number is not the original state spelling"
        );
    }
    #[test]
    fn stale_restored_foreign_and_future_observations_never_satisfy_a_predicate() {
        use buffa::Message;
        let base = entity(state::observed_state::Value::SwitchOn(true));
        assert_eq!(literal(&base), Some("on"));
        let mut cases = vec![base; 7];
        cases[0]
            .state
            .as_option_mut()
            .unwrap()
            .quality
            .as_option_mut()
            .unwrap()
            .freshness = state::Freshness::Stale.into();
        cases[1]
            .state
            .as_option_mut()
            .unwrap()
            .quality
            .as_option_mut()
            .unwrap()
            .provenance = state::Provenance::Restored.into();
        cases[2]
            .state
            .as_option_mut()
            .unwrap()
            .quality
            .as_option_mut()
            .unwrap()
            .availability = state::Availability::Unavailable.into();
        cases[3]
            .state
            .as_option_mut()
            .unwrap()
            .quality
            .as_option_mut()
            .unwrap()
            .availability = EnumValue::Unknown(99);
        cases[4]
            .state
            .as_option_mut()
            .unwrap()
            .entity
            .as_option_mut()
            .unwrap()
            .reference = Some(registry::entity_ref::Reference::EntityId("foreign".into()));
        cases[5].state.as_option_mut().unwrap().value = None;
        let mut bytes = cases[6].state.as_option().unwrap().encode_to_vec();
        bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
        cases[6].state = state::ObservedState::decode_from_slice(&bytes)
            .unwrap()
            .into();
        for entity in cases {
            assert_eq!(literal(&entity), None);
        }
    }
    #[test]
    fn measurement_literals_keep_exact_precision_and_refuse_non_finite_or_non_numeric_data() {
        let measurement = |number, literal: &str| {
            entity(state::observed_state::Value::Measurement(
                state::Measurement {
                    value: number,
                    decimal_value: Some(literal.into()),
                    ..Default::default()
                }
                .into(),
            ))
        };
        assert_eq!(literal(&measurement(12.5, "12.5000")), Some("12.5000"));
        assert_eq!(literal(&measurement(f64::NAN, "12.5000")), None);
        assert_eq!(literal(&measurement(12.5, "NaN")), None);
        assert_eq!(literal(&measurement(12.5, "not-a-number")), None);
    }
    #[test]
    fn hidden_and_unavailable_ancestors_prune_commands_and_popup_entry() {
        let mut native =
            NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
        let id: EntityId = "synthetic-observation".parse().unwrap();
        native
            .nodes
            .get_mut(&NodeId::parse("grid").unwrap())
            .unwrap()
            .recipes = Some(Recipes {
            interactions: BTreeMap::new(),
            visibility: Some(Predicate::State {
                entity: id.clone(),
                expected: "off".into(),
                comparison: Comparison::NotEqual,
            }),
        });
        let root = native.root(None).unwrap();
        let popup = NodeId::parse("popup").unwrap();
        let visible = BTreeMap::from([(id.clone(), "on")]);
        let shown = project_nodes(&native, root, &visible);
        assert!(
            shown
                .iter()
                .any(|node| node.id.as_str() == "tile" && node.entity.is_some())
        );
        assert_eq!(project_nodes(&native, &popup, &visible).len(), 2);
        let hidden = BTreeMap::from([(id, "off")]);
        assert_eq!(project_nodes(&native, root, &hidden).len(), 2);
        assert!(project_nodes(&native, &popup, &hidden).is_empty());
        let unavailable = project_nodes(&native, root, &BTreeMap::new());
        assert_eq!(unavailable.len(), 3);
        let grid = unavailable.last().unwrap();
        assert_eq!(grid.kind, Kind::Unsupported);
        assert!(grid.entity.is_none() && grid.recipes.is_none() && grid.children.is_empty());
        assert!(grid.diagnostic.contains("visibilité"));
        let popup = project_nodes(&native, &popup, &BTreeMap::new());
        assert_eq!(popup.len(), 1);
        assert!(popup[0].children.is_empty() && popup[0].recipes.is_none());
    }
}
