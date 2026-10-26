//! Native card controls are admitted by source node and exact capability.
//! Presentation filtering and command construction use the same restricted Control.
use super::{capability::Control, helper::Kind};
use crate::dashboard_layout::{FeatureKind, Kind as NodeKind, Node, NodeId, RecipeKind};
use home_client_actions::ActionTarget;
use home_client_cache::CachedView;
use home_client_model::EntityId;
use home_wire_contract::home::helpers::v1::HelperOperation;
use std::collections::{BTreeMap, BTreeSet};

pub(super) struct Admitted {
    pub entity: EntityId,
    pub capability: usize,
    pub control: Control,
    pub title: String,
}
fn features(control: &Control) -> BTreeSet<FeatureKind> {
    let mut result = BTreeSet::new();
    match control {
        Control::Switch { power } => {
            if *power {
                result.insert(FeatureKind::Toggle);
            }
        }
        Control::Light {
            power,
            brightness,
            color,
        } => {
            if *power {
                result.insert(FeatureKind::Toggle);
            }
            if brightness.is_some() {
                result.insert(FeatureKind::Brightness);
            }
            if color.is_some() {
                result.insert(FeatureKind::ColorTemperature);
            }
        }
        Control::Climate(climate) => {
            if climate.setpoint().is_some() || climate.range().is_some() {
                result.insert(FeatureKind::TargetTemperature);
            }
            if !climate.modes().is_empty() {
                result.insert(FeatureKind::ClimateModes);
            }
        }
        Control::Fan(fan) => {
            if fan.can_power() {
                result.insert(FeatureKind::Toggle);
            }
            if !fan.speeds().is_empty() {
                result.insert(FeatureKind::FanSpeed);
            }
        }
        Control::Helper(helper) => match helper.kind() {
            Kind::Number(_) | Kind::Counter { .. } => {
                if helper.operations().contains(&HelperOperation::Set) {
                    result.insert(FeatureKind::NumericInput);
                }
            }
            Kind::Boolean => {
                if helper.operations().iter().any(|operation| {
                    [HelperOperation::Set, HelperOperation::Toggle].contains(operation)
                }) {
                    result.insert(FeatureKind::Toggle);
                }
            }
            Kind::Select(_)
            | Kind::Text { .. }
            | Kind::Date
            | Kind::Time
            | Kind::DateTime
            | Kind::Button => {}
        },
        Control::Cover(cover) => {
            if !cover.operations().is_empty() {
                result.insert(FeatureKind::CoverOpenClose);
            }
            if cover.position().is_some() {
                result.insert(FeatureKind::CoverPosition);
            }
        }
        Control::Alarm(alarm) => {
            if !alarm.operations().is_empty() {
                result.insert(FeatureKind::AlarmModes);
            }
        }
        Control::Lock(_)
        | Control::Binary
        | Control::Measurement
        | Control::ReadOnly
        | Control::Unsupported => {}
    }
    result
}
fn restrict(control: &Control, node: &Node) -> Option<Control> {
    match node.recipe_kind {
        Some(RecipeKind::Select) => {
            let valid = match control {
                Control::Helper(helper) => match helper.kind() {
                    Kind::Select(_) => true,
                    Kind::Boolean
                    | Kind::Number(_)
                    | Kind::Counter { .. }
                    | Kind::Text { .. }
                    | Kind::Date
                    | Kind::Time
                    | Kind::DateTime
                    | Kind::Button => false,
                },
                Control::Switch { .. }
                | Control::Light { .. }
                | Control::Climate(_)
                | Control::Fan(_)
                | Control::Binary
                | Control::Measurement
                | Control::Cover(_)
                | Control::Lock(_)
                | Control::Alarm(_)
                | Control::ReadOnly
                | Control::Unsupported => false,
            };
            if !valid {
                return None;
            }
        }
        Some(RecipeKind::Alarm) => {
            if !matches!(control, Control::Alarm(_)) {
                return None;
            }
        }
        Some(
            RecipeKind::Template
            | RecipeKind::BubbleButton
            | RecipeKind::BubblePopup
            | RecipeKind::Chips
            | RecipeKind::TemplateChip
            | RecipeKind::Button
            | RecipeKind::Entity
            | RecipeKind::Text,
        )
        | None => {}
    }
    let supported = features(control);
    if node
        .features
        .iter()
        .any(|feature| !supported.contains(feature))
    {
        return None;
    }
    // No explicit feature selection keeps the existing native equivalent.
    // A Select recipe still narrows operations to its selector's Set intent.
    if node.features.is_empty() {
        return Some(match control {
            Control::Helper(helper) if node.recipe_kind == Some(RecipeKind::Select) => {
                Control::Helper(helper.restricted_to(&[HelperOperation::Set]))
            }
            Control::Switch { .. }
            | Control::Light { .. }
            | Control::Climate(_)
            | Control::Fan(_)
            | Control::Helper(_)
            | Control::Binary
            | Control::Measurement
            | Control::Cover(_)
            | Control::Lock(_)
            | Control::Alarm(_)
            | Control::ReadOnly
            | Control::Unsupported => control.clone(),
        });
    }
    let has = |feature| node.features.contains(&feature);
    Some(match control {
        Control::Switch { power } => Control::Switch {
            power: *power && has(FeatureKind::Toggle),
        },
        Control::Light {
            power,
            brightness,
            color,
        } => Control::Light {
            power: *power && has(FeatureKind::Toggle),
            brightness: brightness.filter(|_| has(FeatureKind::Brightness)),
            color: color.clone().filter(|_| has(FeatureKind::ColorTemperature)),
        },
        Control::Climate(climate) => Control::Climate(climate.restricted(
            has(FeatureKind::TargetTemperature),
            has(FeatureKind::ClimateModes),
        )),
        Control::Fan(fan) => {
            Control::Fan(fan.restricted(has(FeatureKind::Toggle), has(FeatureKind::FanSpeed)))
        }
        Control::Helper(helper) => {
            Control::Helper(helper.restricted_to(if has(FeatureKind::Toggle) {
                &[HelperOperation::Set, HelperOperation::Toggle]
            } else {
                &[HelperOperation::Set]
            }))
        }
        Control::Cover(cover) => Control::Cover(cover.restricted(
            has(FeatureKind::CoverOpenClose),
            has(FeatureKind::CoverPosition),
        )),
        Control::Alarm(alarm) => Control::Alarm(alarm.clone()),
        Control::Lock(_)
        | Control::Binary
        | Control::Measurement
        | Control::ReadOnly
        | Control::Unsupported => {
            return None;
        }
    })
}
fn inert(node: &mut Node, reason: &str) {
    node.kind = NodeKind::Unsupported;
    node.diagnostic = reason.into();
    node.entity = None;
    node.recipes = None;
    node.recipe_kind = None;
    node.features.clear();
}
pub(super) fn admit(nodes: &mut [Node], view: Option<&CachedView>) -> BTreeMap<NodeId, Admitted> {
    let mut result = BTreeMap::new();
    let Some(view) = view else {
        return result;
    };
    for node in nodes {
        if !node.device_card() {
            continue;
        }
        let Some(id) = node.entity.clone() else {
            continue;
        };
        let Some(entity) = view.get(&id) else {
            inert(node, "Aucune observation autorisée pour cet élément.");
            continue;
        };
        let mut candidates =
            entity
                .capabilities
                .iter()
                .enumerate()
                .filter_map(|(index, capability)| {
                    ActionTarget::new(id.clone(), &capability.schema_id, &capability.endpoint)?;
                    let control = Control::parse(capability);
                    if matches!(control, Control::Unsupported) {
                        return None;
                    }
                    restrict(&control, node).map(|control| (index, control))
                });
        let first = candidates.next();
        let ambiguous = candidates.next().is_some();
        match first {
            Some((capability, control)) if !ambiguous => {
                result.insert(
                    node.id.clone(),
                    Admitted {
                        entity: id,
                        capability,
                        control,
                        title: if node.title.is_empty() {
                            crate::cache_view::bounded(&entity.current_alias)
                        } else {
                            node.title.clone()
                        },
                    },
                );
            }
            Some(_) => inert(
                node,
                "Plusieurs capabilities correspondent à cette carte ; aucune cible implicite n’est choisie.",
            ),
            None => inert(
                node,
                "La famille ou les features configurées ne correspondent à aucune capability native admise.",
            ),
        }
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dashboard::LayoutState;
    use crate::dashboard_layout::NativeLayout;
    use crate::device_view::{
        DeviceIntent, DeviceStore, Power, Refusal,
        tests::{light, request, view},
    };
    use home_client_cache::CachePhase;
    use std::sync::Arc;
    fn fixture() -> (
        crate::dashboard::ProjectionRequest,
        home_wire_contract::home::state::v1::ViewEntity,
    ) {
        let mut request = request();
        let entity = light(&request);
        let mut layout = request.dashboard.layout().unwrap().clone();
        let mut native =
            NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
        for node in native.nodes.values_mut() {
            node.recipes = None;
            if node.entity.is_some() {
                node.entity = Some(entity.entity_id.parse().unwrap());
            }
        }
        native
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap()
            .features = vec![FeatureKind::Brightness];
        let mut toggle = native.nodes[&NodeId::parse("tile").unwrap()].clone();
        toggle.id = NodeId::parse("toggle-only").unwrap();
        toggle.source.path = "view.toggle-only".into();
        toggle.features = vec![FeatureKind::Toggle];
        native
            .nodes
            .get_mut(&NodeId::parse("view").unwrap())
            .unwrap()
            .children
            .push(toggle.id.clone());
        native.nodes.insert(toggle.id.clone(), toggle);
        layout.native = Some(native);
        request.room = None;
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        (request, entity)
    }
    #[test]
    fn duplicate_entity_cards_admit_only_their_own_features_and_keep_distinct_bindings() {
        let (mut request, mut entity) = fixture();
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        let brightness = projection.native_devices[&NodeId::parse("tile").unwrap()].clone();
        let toggle = projection.native_devices[&NodeId::parse("toggle-only").unwrap()].clone();
        assert_ne!(brightness.token, toggle.token);
        assert_ne!(brightness.generation, toggle.generation);
        assert!(brightness.can_brightness && !brightness.can_power);
        assert!(toggle.can_power && !toggle.can_brightness);
        assert!(
            store
                .prepare(
                    brightness.token,
                    brightness.generation,
                    0,
                    DeviceIntent::Power(Power::On)
                )
                .is_err()
        );
        assert!(
            store
                .prepare(
                    toggle.token,
                    toggle.generation,
                    0,
                    DeviceIntent::Brightness(50.)
                )
                .is_err()
        );
        let prepared = store
            .prepare(
                brightness.token,
                brightness.generation,
                0,
                DeviceIntent::Brightness(50.),
            )
            .unwrap();
        assert!(
            store
                .prepare(
                    toggle.token,
                    toggle.generation,
                    0,
                    DeviceIntent::Power(Power::On)
                )
                .is_ok()
        );
        entity.state.as_option_mut().unwrap().revision += 1;
        store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        assert!(
            (prepared.guard)(),
            "an observation does not change the feature admission"
        );
        let mut layout = request.dashboard.layout().unwrap().clone();
        layout.document_revision += 1;
        layout.document_hash = [8; 32];
        layout
            .native
            .as_mut()
            .unwrap()
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap()
            .features = vec![FeatureKind::Toggle];
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        let changed = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        assert!(
            !(prepared.guard)(),
            "a feature change withdraws the held command"
        );
        let changed = &changed.native_devices[&NodeId::parse("tile").unwrap()];
        assert!(changed.can_power && !changed.can_brightness);
        assert!(matches!(
            store.prepare(
                brightness.token,
                brightness.generation,
                0,
                DeviceIntent::Power(Power::On)
            ),
            Err(Refusal::Stale)
        ));
        assert!(
            store
                .prepare(
                    changed.token,
                    changed.generation,
                    0,
                    DeviceIntent::Brightness(50.)
                )
                .is_err()
        );
        assert!(store.0.lock().unwrap().actions.is_empty());
    }
    #[test]
    fn a_capability_removed_during_a_gesture_inerts_only_its_incompatible_card() {
        use home_wire_contract::home::registry::v1::capability_description::Constraints;
        let (request, mut entity) = fixture();
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        let brightness = &projection.native_devices[&NodeId::parse("tile").unwrap()];
        let prepared = store
            .prepare(
                brightness.token,
                brightness.generation,
                0,
                DeviceIntent::Brightness(50.),
            )
            .unwrap();
        let Some(Constraints::Light(constraints)) = entity.capabilities[0].constraints.as_mut()
        else {
            panic!()
        };
        constraints.brightness_percent = Default::default();
        let changed = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        assert!(!(prepared.guard)());
        assert!(
            !changed
                .native_devices
                .contains_key(&NodeId::parse("tile").unwrap())
        );
        let diagnostic = changed
            .native_nodes
            .iter()
            .find(|node| node.id.as_str() == "tile")
            .unwrap();
        assert_eq!(diagnostic.kind, NodeKind::Unsupported);
        assert!(diagnostic.recipes.is_none());
        assert!(diagnostic.entity.is_none());
        assert_eq!(diagnostic.source.file, "synthetic.yaml");
        assert!(!diagnostic.diagnostic.is_empty());
        let toggle = &changed.native_devices[&NodeId::parse("toggle-only").unwrap()];
        assert!(
            store
                .prepare(
                    toggle.token,
                    toggle.generation,
                    0,
                    DeviceIntent::Power(Power::Off)
                )
                .is_ok()
        );
    }
    #[test]
    fn select_recipes_refuse_foreign_families_and_ambiguous_capabilities_never_choose_an_endpoint()
    {
        let (mut request, mut entity) = fixture();
        let mut layout = request.dashboard.layout().unwrap().clone();
        let tile = layout
            .native
            .as_mut()
            .unwrap()
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap();
        tile.kind = NodeKind::Recipe;
        tile.recipe_kind = Some(RecipeKind::Select);
        tile.features.clear();
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        assert!(
            !projection
                .native_devices
                .contains_key(&NodeId::parse("tile").unwrap())
        );
        let mut duplicate = entity.capabilities[0].clone();
        duplicate.endpoint = "other-endpoint".into();
        entity.capabilities.push(duplicate);
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        assert!(projection.native_devices.is_empty());
        assert_eq!(
            projection
                .native_nodes
                .iter()
                .filter(|node| node.device_card())
                .count(),
            0
        );
        assert!(
            projection
                .native_nodes
                .iter()
                .find(|node| node.id.as_str() == "toggle-only")
                .unwrap()
                .diagnostic
                .contains("Plusieurs capabilities")
        );
    }

    #[test]
    fn climate_and_fan_feature_selections_narrow_the_actual_argument_builder() {
        use crate::device_view::{
            climate::{ClimateIntent, Fan},
            number::NumericInput,
            tests::climate_entity,
        };
        use home_wire_contract::home::{registry::v1 as registry, state::v1 as state};
        let (request, _) = fixture();
        let mut node = request
            .dashboard
            .layout()
            .unwrap()
            .native
            .as_ref()
            .unwrap()
            .nodes[&NodeId::parse("tile").unwrap()]
            .clone();
        let entity = climate_entity(&crate::device_view::tests::request());
        let full = Control::parse(&entity.capabilities[0]);
        let Control::Climate(climate) = &full else {
            panic!()
        };
        let mode = climate.modes()[0];
        node.features = vec![FeatureKind::ClimateModes];
        let modes = restrict(&full, &node).unwrap();
        assert!(
            modes
                .arguments(DeviceIntent::Climate(ClimateIntent::Mode(mode)), None)
                .is_ok()
        );
        assert!(
            modes
                .arguments(
                    DeviceIntent::Climate(ClimateIntent::Setpoint(NumericInput::Text("20".into()))),
                    None
                )
                .is_err()
        );
        node.features = vec![FeatureKind::TargetTemperature];
        let temperature = restrict(&full, &node).unwrap();
        assert!(
            temperature
                .arguments(DeviceIntent::Climate(ClimateIntent::Mode(mode)), None)
                .is_err()
        );
        assert!(
            temperature
                .arguments(
                    DeviceIntent::Climate(ClimateIntent::Setpoint(NumericInput::Text("20".into()))),
                    None
                )
                .is_ok()
        );
        let full = Control::Fan(
            Fan::parse(&registry::FanConstraints {
                set_on: true,
                speed_options: vec!["Low".into(), "High".into()],
                ..Default::default()
            })
            .unwrap(),
        );
        node.features = vec![FeatureKind::FanSpeed];
        let speed = restrict(&full, &node).unwrap();
        assert!(
            speed
                .arguments(DeviceIntent::FanSpeed(state::FanSpeed::High), None)
                .is_ok()
        );
        assert!(
            speed
                .arguments(DeviceIntent::Power(Power::On), None)
                .is_err()
        );
        node.features = vec![FeatureKind::Toggle];
        let power = restrict(&full, &node).unwrap();
        assert!(
            power
                .arguments(DeviceIntent::FanSpeed(state::FanSpeed::High), None)
                .is_err()
        );
        assert!(
            power
                .arguments(DeviceIntent::Power(Power::On), None)
                .is_ok()
        );
        node.features = vec![FeatureKind::FanSpeed, FeatureKind::Brightness];
        assert!(
            restrict(&full, &node).is_none(),
            "an incompatible feature makes the whole selection inert"
        );
    }
    #[test]
    fn select_and_numeric_helper_cards_keep_their_typed_family_and_cas() {
        use crate::device_view::tests::helper_entity;
        use home_wire_contract::home::{actions::v1::action_args::Arguments, helpers::v1 as h};
        for kind in [h::HelperKind::Select, h::HelperKind::Number] {
            let (mut request, _) = fixture();
            let entity = helper_entity(&crate::device_view::tests::request(), kind);
            let mut layout = request.dashboard.layout().unwrap().clone();
            let tile = layout
                .native
                .as_mut()
                .unwrap()
                .nodes
                .get_mut(&NodeId::parse("tile").unwrap())
                .unwrap();
            match kind {
                h::HelperKind::Select => {
                    tile.kind = NodeKind::Recipe;
                    tile.recipe_kind = Some(RecipeKind::Select);
                    tile.features.clear();
                }
                h::HelperKind::Number => {
                    tile.features = vec![FeatureKind::NumericInput];
                }
                h::HelperKind::Unspecified
                | h::HelperKind::Boolean
                | h::HelperKind::Counter
                | h::HelperKind::Text
                | h::HelperKind::Date
                | h::HelperKind::Time
                | h::HelperKind::Datetime
                | h::HelperKind::ButtonPress => unreachable!(),
            }
            request.dashboard = LayoutState::Ready(Arc::new(layout));
            let store = DeviceStore::default();
            let projection = store.project(
                CachePhase::Live,
                Some(&view(&request, vec![entity.clone()])),
                &request,
            );
            let row = &projection.native_devices[&NodeId::parse("tile").unwrap()];
            assert_eq!(row.values.len(), 1);
            assert!(row.choices.is_empty());
            assert!(
                store
                    .prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On))
                    .is_err()
            );
            let intent = match kind {
                h::HelperKind::Select => {
                    crate::device_view::helper::HelperIntent::Select("custom B".into())
                }
                h::HelperKind::Number => crate::device_view::helper::HelperIntent::Number(
                    crate::device_view::number::NumericInput::Text("2".into()),
                ),
                h::HelperKind::Unspecified
                | h::HelperKind::Boolean
                | h::HelperKind::Counter
                | h::HelperKind::Text
                | h::HelperKind::Date
                | h::HelperKind::Time
                | h::HelperKind::Datetime
                | h::HelperKind::ButtonPress => unreachable!(),
            };
            let prepared = store
                .prepare(row.token, row.generation, 0, DeviceIntent::Helper(intent))
                .unwrap();
            let Some(Arguments::Helper(arguments)) =
                &prepared.request.arguments.as_option().unwrap().arguments
            else {
                panic!()
            };
            assert_eq!(arguments.expected_helper_revision, Some(7));
        }
    }

    #[test]
    fn color_only_controls_keep_canonical_units_and_the_configured_header_command_is_independent() {
        use crate::device_view::{GestureAccess, NativeIntent};
        use crate::{ProValueEvent, ProValueIntent};
        use home_client_model::dashboard::{CommandKind, Gesture, Intention, Interaction, Recipes};
        use home_wire_contract::home::{
            actions::v1::action_args::Arguments, registry::v1 as registry,
        };
        let (mut request, mut entity) = fixture();
        let Some(registry::capability_description::Constraints::Light(capability)) =
            entity.capabilities[0].constraints.as_mut()
        else {
            panic!()
        };
        capability.color_temp_mireds = registry::NumberBounds {
            minimum: 153.,
            maximum: 500.,
            step: Some(1.),
            ..Default::default()
        }
        .into();
        let mut layout = request.dashboard.layout().unwrap().clone();
        let node = layout
            .native
            .as_mut()
            .unwrap()
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap();
        node.features = vec![FeatureKind::ColorTemperature];
        node.recipes = Some(Recipes {
            interactions: BTreeMap::from([(
                Gesture::Tap,
                Interaction {
                    intention: Intention::Command {
                        entity: entity.entity_id.parse().unwrap(),
                        kind: CommandKind::TurnOff,
                    },
                    confirmation: None,
                    source: node.source.clone(),
                },
            )]),
            visibility: None,
        });
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        let row = &projection.native_devices[&NodeId::parse("tile").unwrap()];
        assert!(!row.can_power && !row.can_brightness);
        assert_eq!(row.values.len(), 1);
        assert!(
            store
                .prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On))
                .is_err()
        );
        let intent = store
            .edit(
                row.token,
                row.generation,
                0,
                0,
                ProValueIntent {
                    event: ProValueEvent::Submit,
                    lower: "4000".into(),
                    ..Default::default()
                },
            )
            .unwrap()
            .unwrap();
        let command = store.prepare(row.token, row.generation, 0, intent).unwrap();
        let Some(Arguments::SetLight(arguments)) =
            &command.request.arguments.as_option().unwrap().arguments
        else {
            panic!()
        };
        assert_eq!(arguments.color_temp_mireds, Some(250));
        assert!(arguments.on.is_none() && arguments.brightness_percent.is_none());
        let header = &projection.recipes[&NodeId::parse("tile").unwrap()];
        assert_eq!(header.access(Gesture::Tap), GestureAccess::Command);
        let command = store
            .prepare_recipe(header.token, header.generation, 0, Gesture::Tap)
            .unwrap();
        let NativeIntent::Command(command) = command.intent else {
            panic!()
        };
        let Some(Arguments::SetLight(arguments)) =
            &command.request.arguments.as_option().unwrap().arguments
        else {
            panic!()
        };
        assert_eq!(arguments.on, Some(false));
        assert!(arguments.color_temp_mireds.is_none());
        assert!(store.0.lock().unwrap().actions.is_empty());
    }
}
