//! Typed visibility over the exact capability in the admitted capture.
use crate::{
    dashboard::Layout,
    dashboard_layout::{NativeLayout, Node, NodeId},
};
use home_client_cache::{CachePhase, CachedView};
use home_client_model::{
    api::CapabilityName,
    dashboard::{FieldBinding, FieldKey, Literal, Visibility},
};
use home_wire_contract::home::api::v2 as wire;
use std::collections::{BTreeMap, BTreeSet};

pub(crate) fn capability<'a>(
    view: &'a CachedView,
    name: &CapabilityName,
) -> Option<&'a wire::Capability> {
    let entry = view.get(&name.entity())?;
    if !entry.__buffa_unknown_fields.is_empty() {
        return None;
    };
    let mut values = entry
        .capabilities
        .iter()
        .filter(|value| value.name == name.as_str());
    let value = values.next()?;
    if values.next().is_some() || !crate::device_view::observation::fresh(value) {
        return None;
    };
    Some(value)
}
pub(crate) fn power(value: &wire::Capability) -> Option<bool> {
    crate::device_view::observation::power(value)
}
pub(crate) fn literal(view: &CachedView, binding: &FieldBinding) -> Option<Literal> {
    use wire::{capability::Trait as T, dashboard_literal::Value as V};
    let capability = capability(view, &binding.capability)?;
    let value = match (binding.field, capability.r#trait.as_ref()?) {
        (FieldKey::CurrentTemperature, T::NumericMeasurement(v))
            if v.unit == "Cel" && v.role == wire::MeasurementRole::Temperature =>
        {
            V::NumberValue(v.observation.value?)
        }
        (FieldKey::Setpoint, T::TemperatureControl(v)) => match v.observation.setpoint.as_ref()? {
            wire::temperature_control_observation::Setpoint::SetpointCelsius(v) => {
                V::NumberValue(*v)
            }
            _ => return None,
        },
        (FieldKey::Measurement | FieldKey::Value, T::NumericMeasurement(v)) => {
            // Exact decimals remain independent of floating-point rounding, but
            // a supplied non-finite double is invalid observation evidence.
            if v.observation.value.is_some_and(|value| !value.is_finite()) {
                return None;
            }
            V::DecimalValue(v.observation.decimal_value.clone()?)
        }
        (FieldKey::State, T::Power(_)) => V::PowerValue(
            if power(capability)? {
                wire::PowerValue::On
            } else {
                wire::PowerValue::Off
            }
            .into(),
        ),
        (FieldKey::State | FieldKey::Value, T::BinaryObservation(v)) => {
            V::BooleanValue(v.observation.value?)
        }
        (FieldKey::State, T::TemperatureControl(v)) => V::TemperatureMode(v.observation.mode?),
        (FieldKey::State, T::FanControl(v)) => V::FanSpeed(v.observation.speed.clone()?),
        (FieldKey::State, T::Cover(v)) => {
            let value = v.observation.value.as_option()?;
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            };
            V::CoverState(value.state?)
        }
        (FieldKey::State, T::Lock(v)) => {
            let value = v.observation.value.as_option()?;
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            };
            V::LockState(value.state)
        }
        (FieldKey::State, T::Alarm(v)) => {
            let value = v.observation.value.as_option()?;
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            };
            V::AlarmState(value.state)
        }
        (FieldKey::State, T::Weather(v)) => {
            let value = v.observation.value.as_option()?;
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            };
            V::WeatherCondition(value.condition?)
        }
        (FieldKey::State, T::Presence(v)) => {
            let value = v.observation.value.as_option()?;
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            };
            V::Residence(value.residence)
        }
        (FieldKey::State | FieldKey::Value, T::Helper(v)) => {
            let value = v.observation.value.as_option()?;
            if !value.__buffa_unknown_fields.is_empty() {
                return None;
            };
            match value.value.as_ref()? {
                wire::helper_value::Value::BooleanValue(v) => V::BooleanValue(*v),
                wire::helper_value::Value::Counter(v) => V::IntegerValue(*v),
                wire::helper_value::Value::Number(v) => V::NumberValue(*v),
                wire::helper_value::Value::Text(v)
                | wire::helper_value::Value::SelectedOption(v) => V::TextValue(v.clone()),
                _ => return None,
            }
        }
        _ => return None,
    };
    home_wire_contract::dashboard_contract::decode_literal(&wire::DashboardLiteral {
        value: Some(value),
        ..Default::default()
    })
    .ok()
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
            .filter_map(|id| Some((id.clone(), literal(view, &id)?)))
            .collect()
    } else {
        BTreeMap::new()
    };
    project_nodes(native, root, &states)
}

fn project_nodes(
    native: &NativeLayout,
    root: &NodeId,
    states: &BTreeMap<FieldBinding, Literal>,
) -> Vec<Node> {
    let visibility = |node: &Node| {
        node.recipes
            .as_ref()
            .and_then(|recipes| recipes.visibility.as_ref())
            .map_or(Visibility::Visible, |predicate| {
                predicate.evaluate(&|id| states.get(id))
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
    node.inert("Visibility condition unavailable in the current authorized view.");
    node.children.clear();
    node
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dashboard_layout::Kind;
    use buffa::{EnumValue, Message};
    use home_client_model::dashboard::{Comparison, Predicate, Recipes};
    use wire::capability::Trait as T;
    const CAP: &str = "homes/synthetic/entities/synthetic-observation/capabilities/power";
    fn quality() -> wire::ObservationQuality {
        wire::ObservationQuality {
            availability: wire::Availability::Available.into(),
            freshness: wire::Freshness::Current.into(),
            provenance: wire::Provenance::Observed.into(),
            ..Default::default()
        }
    }
    fn cap(value: T) -> wire::Capability {
        wire::Capability {
            name: CAP.into(),
            etag: "definition-etag".into(),
            action_access: wire::ActionAccess::Denied.into(),
            r#trait: Some(value),
            ..Default::default()
        }
    }
    fn power_cap(value: Option<wire::PowerValue>) -> wire::Capability {
        cap(T::Power(
            wire::PowerCapability {
                observation: wire::PowerObservation {
                    value: value.map(Into::into),
                    quality: quality().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }
            .into(),
        ))
    }
    fn binding(field: FieldKey) -> FieldBinding {
        FieldBinding {
            capability: CapabilityName::try_from(CAP).unwrap(),
            field,
        }
    }
    fn read(value: wire::Capability, field: FieldKey) -> Option<Literal> {
        let entity = CapabilityName::try_from(value.name.as_str())
            .unwrap()
            .entity();
        let view = crate::dashboard::capture_fixture(vec![wire::EntityViewEntry {
            entity: wire::Entity {
                name: entity.as_str().into(),
                etag: "entity-etag".into(),
                display_name: "Synthetic observation".into(),
                ..Default::default()
            }
            .into(),
            capabilities: vec![value],
            ..Default::default()
        }]);
        literal(&view, &binding(field))
    }
    #[test]
    fn literals_preserve_observed_false_and_empty_text_without_localized_fallbacks() {
        assert_eq!(
            read(power_cap(Some(wire::PowerValue::Off)), FieldKey::State),
            Some(Literal::Power(home_client_model::api::Power::Off))
        );
        let mut empty = cap(T::Helper(
            wire::HelperCapability {
                observation: wire::HelperObservation {
                    value: wire::HelperValue {
                        value: Some(wire::helper_value::Value::Text(String::new())),
                        ..Default::default()
                    }
                    .into(),
                    quality: quality().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }
            .into(),
        ));
        assert_eq!(
            read(empty.clone(), FieldKey::State),
            Some(Literal::Text(
                home_client_model::dashboard::TextLiteral::try_from("").unwrap()
            ))
        );
        // A restored durable helper is usable data, not a device readback.
        let Some(T::Helper(helper)) = empty.r#trait.as_mut() else {
            panic!()
        };
        helper
            .observation
            .get_or_insert_default()
            .quality
            .get_or_insert_default()
            .provenance = wire::Provenance::Restored.into();
        assert_eq!(
            read(empty, FieldKey::State),
            Some(Literal::Text(
                home_client_model::dashboard::TextLiteral::try_from("").unwrap()
            ))
        );
        assert_eq!(read(power_cap(None), FieldKey::State), None);
        let lossy = cap(T::NumericMeasurement(
            wire::NumericMeasurementCapability {
                observation: wire::NumericObservation {
                    value: Some(12.5),
                    quality: quality().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }
            .into(),
        ));
        assert_eq!(
            read(lossy, FieldKey::Measurement),
            None,
            "rendered float is not exact source decimal"
        );
    }
    #[test]
    fn stale_restored_foreign_and_future_observations_never_satisfy_a_predicate() {
        let base = power_cap(Some(wire::PowerValue::On));
        assert!(read(base.clone(), FieldKey::State).is_some());
        let mut cases = vec![base; 7];
        for (i, value) in cases.iter_mut().enumerate() {
            let Some(T::Power(body)) = &mut value.r#trait else {
                panic!()
            };
            let observation = body.observation.get_or_insert_default();
            match i {
                0 => {
                    observation.quality.get_or_insert_default().freshness =
                        wire::Freshness::Stale.into()
                }
                1 => {
                    observation.quality.get_or_insert_default().provenance =
                        wire::Provenance::Restored.into()
                }
                2 => {
                    observation.quality.get_or_insert_default().availability =
                        wire::Availability::Unavailable.into()
                }
                3 => {
                    observation.quality.get_or_insert_default().availability =
                        EnumValue::Unknown(99)
                }
                4 => value.name = "homes/synthetic/entities/foreign/capabilities/power".into(),
                5 => observation.value = None,
                _ => {
                    let mut bytes = observation.encode_to_vec();
                    bytes.extend_from_slice(&[0x98, 0x06, 0x07]);
                    *observation = wire::PowerObservation::decode_from_slice(&bytes).unwrap();
                }
            }
        }
        for value in cases {
            assert_eq!(read(value, FieldKey::State), None);
        }
    }
    #[test]
    fn measurement_literals_keep_exact_precision_and_refuse_non_finite_or_non_numeric_data() {
        let measurement = |number, decimal: &str| {
            cap(T::NumericMeasurement(
                wire::NumericMeasurementCapability {
                    observation: wire::NumericObservation {
                        value: Some(number),
                        decimal_value: Some(decimal.into()),
                        quality: quality().into(),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }
                .into(),
            ))
        };
        assert_eq!(
            read(measurement(12.5, "12.5000"), FieldKey::Measurement),
            Some(Literal::Decimal(
                home_client_model::dashboard::DecimalLiteral::try_from("12.5000").unwrap()
            ))
        );
        assert_eq!(
            read(measurement(f64::NAN, "12.5000"), FieldKey::Measurement),
            None
        );
        assert_eq!(read(measurement(12.5, "NaN"), FieldKey::Measurement), None);
        assert_eq!(
            read(measurement(12.5, "not-a-number"), FieldKey::Measurement),
            None
        );
    }
    #[test]
    fn hidden_and_unavailable_ancestors_prune_commands_and_popup_entry() {
        let mut native =
            NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
        let field = binding(FieldKey::State);
        native
            .nodes
            .get_mut(&NodeId::parse("grid").unwrap())
            .unwrap()
            .recipes = Some(Recipes {
            interactions: BTreeMap::new(),
            visibility: Some(Predicate::State {
                field: field.clone(),
                expected: Literal::Power(home_client_model::api::Power::Off),
                comparison: Comparison::NotEqual,
            }),
        });
        let root = native.root(None).unwrap();
        let popup = NodeId::parse("popup").unwrap();
        let visible = BTreeMap::from([(
            field.clone(),
            Literal::Power(home_client_model::api::Power::On),
        )]);
        let shown = project_nodes(&native, root, &visible);
        assert!(
            shown
                .iter()
                .any(|node| node.id.as_str() == "tile" && node.entity.is_some())
        );
        assert_eq!(project_nodes(&native, &popup, &visible).len(), 2);
        let hidden = BTreeMap::from([(field, Literal::Power(home_client_model::api::Power::Off))]);
        assert_eq!(project_nodes(&native, root, &hidden).len(), 2);
        assert!(project_nodes(&native, &popup, &hidden).is_empty());
        let unavailable = project_nodes(&native, root, &BTreeMap::new());
        assert_eq!(unavailable.len(), 3);
        let grid = unavailable.last().unwrap();
        assert_eq!(grid.kind, Kind::Unsupported);
        assert!(grid.entity.is_none() && grid.recipes.is_none() && grid.children.is_empty());
        assert!(grid.diagnostic.contains("Visibility"));
        let popup = project_nodes(&native, &popup, &BTreeMap::new());
        assert_eq!(popup.len(), 1);
        assert!(popup[0].children.is_empty() && popup[0].recipes.is_none());
    }
}
