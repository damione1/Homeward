//! Host integration for SDK secure-control rows, opaque choice tokens, drafts
//! and revocation. Synthetic entities only; no transport/device side effects.
use super::*;
use super::{
    controls::CoverIntent,
    number::NumericInput,
    tests::{light, request, view},
};
use home_wire_contract::home::api::v2 as wire;
use slint::Model;
use wire::execute_capability_request::Command as Arguments;
#[derive(Clone, Copy)]
enum Family {
    Cover,
    Lock,
    Alarm,
}
impl Family {
    fn entity(self, request: &ProjectionRequest) -> wire::EntityViewEntry {
        let mut entity = light(request);
        entity.capabilities[0].r#trait = Some(match self {
            Self::Cover => wire::capability::Trait::Cover(Box::new(wire::CoverCapability {
                definition: wire::CoverDefinition {
                    operations: vec![
                        wire::CoverOperation::Stop.into(),
                        wire::CoverOperation::Open.into(),
                        wire::CoverOperation::Close.into(),
                    ],
                    observed_states: vec![wire::CoverState::Open.into()],
                    set_position_percent: wire::PercentageBounds {
                        minimum: 10.,
                        maximum: 90.,
                        step: Some(5.),
                        ..Default::default()
                    }
                    .into(),
                    observes_position_percent: true,
                    observes_obstruction: true,
                    ..Default::default()
                }
                .into(),
                observation: wire::CoverObservation {
                    value: wire::CoverValue {
                        state: Some(wire::CoverState::Open.into()),
                        position_percent: Some(35.),
                        obstructed: Some(false),
                        ..Default::default()
                    }
                    .into(),
                    quality: fixture::quality().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })),
            Self::Lock => wire::capability::Trait::Lock(Box::new(wire::LockCapability {
                definition: wire::LockDefinition {
                    operations: vec![
                        wire::LockOperation::Unlock.into(),
                        wire::LockOperation::Lock.into(),
                    ],
                    observed_states: vec![wire::LockState::Jammed.into()],
                    ..Default::default()
                }
                .into(),
                observation: wire::LockObservation {
                    value: wire::LockValue {
                        state: wire::LockState::Jammed.into(),
                        ..Default::default()
                    }
                    .into(),
                    quality: fixture::quality().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })),
            Self::Alarm => wire::capability::Trait::Alarm(Box::new(wire::AlarmCapability {
                definition: wire::AlarmDefinition {
                    operations: vec![
                        wire::AlarmOperation::Disarm.into(),
                        wire::AlarmOperation::ArmAway.into(),
                    ],
                    observed_states: vec![wire::AlarmState::Triggered.into()],
                    ..Default::default()
                }
                .into(),
                observation: wire::AlarmObservation {
                    value: wire::AlarmValue {
                        state: wire::AlarmState::Triggered.into(),
                        ..Default::default()
                    }
                    .into(),
                    quality: fixture::quality().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })),
        });
        entity
    }
    fn kind(self) -> ProDeviceKind {
        match self {
            Self::Cover => ProDeviceKind::Cover,
            Self::Lock => ProDeviceKind::Lock,
            Self::Alarm => ProDeviceKind::Alarm,
        }
    }
    fn withdraw(self, entity: &mut wire::EntityViewEntry) {
        use wire::capability::Trait as C;
        match entity.capabilities[0].r#trait.as_mut().unwrap() {
            C::Cover(c) => c.definition.as_option_mut().unwrap().operations.clear(),
            C::Lock(c) => c.definition.as_option_mut().unwrap().operations.clear(),
            C::Alarm(c) => c.definition.as_option_mut().unwrap().operations.clear(),
            C::Power(_)
            | C::Level(_)
            | C::TemperatureControl(_)
            | C::NumericMeasurement(_)
            | C::BinaryObservation(_)
            | C::LightControl(_)
            | C::FanControl(_)
            | C::Helper(_)
            | C::Solar(_)
            | C::SolarEvent(_)
            | C::Weather(_)
            | C::Presence(_)
            | C::PresenceDetails(_)
            | C::Opaque(_)
            | C::MediaPlayback(_) => panic!("fixture family"),
        }
        entity.capabilities[0].etag = "withdrawn-operations".into();
    }
}
fn cover_value(entity: &mut wire::EntityViewEntry) -> &mut wire::CoverValue {
    let Some(wire::capability::Trait::Cover(value)) = entity.capabilities[0].r#trait.as_mut()
    else {
        panic!("cover fixture")
    };
    value
        .observation
        .as_option_mut()
        .unwrap()
        .value
        .as_option_mut()
        .unwrap()
}

fn first(
    store: &DeviceStore,
    request: &fixture::Scenario,
    entity: &wire::EntityViewEntry,
    phase: CachePhase,
) -> CardRow {
    store
        .project(phase, Some(&view(request, vec![entity.clone()])), request)
        .rows
        .remove(0)
}
fn event(event: crate::ProValueEvent, text: &str) -> crate::ProValueIntent {
    crate::ProValueIntent {
        event,
        lower: text.into(),
        ..Default::default()
    }
}
#[test]
fn typed_sdk_choices_resolve_to_the_same_host_choice_and_exact_wire_operation() {
    for family in [Family::Cover, Family::Lock, Family::Alarm] {
        let request = request();
        let entity = family.entity(&request);
        let store = DeviceStore::default();
        let row = first(&store, &request, &entity, CachePhase::Live);
        assert_eq!(row.kind, family.kind());
        assert!(!row.can_power);
        assert!(!row.power_known);
        assert!(row.choices.is_empty());
        let expected = match family {
            Family::Cover => {
                assert_eq!(
                    row.cover_operations[0].operation,
                    crate::ProCoverOperation::Stop
                );
                assert!(row.lock_operations.is_empty() && row.alarm_operations.is_empty());
                Arguments::Cover(
                    wire::SetCoverCommand {
                        target: Some(wire::set_cover_command::Target::Operation(
                            wire::CoverOperation::Stop.into(),
                        )),
                        ..Default::default()
                    }
                    .into(),
                )
            }
            Family::Lock => {
                assert_eq!(
                    row.lock_operations[0].operation,
                    crate::ProLockOperation::Unlock
                );
                assert!(row.cover_operations.is_empty() && row.alarm_operations.is_empty());
                assert!(row.value.contains("jammed"));
                Arguments::Lock(
                    wire::SetLockCommand {
                        operation: wire::LockOperation::Unlock.into(),
                        ..Default::default()
                    }
                    .into(),
                )
            }
            Family::Alarm => {
                assert_eq!(
                    row.alarm_operations[0].operation,
                    crate::ProAlarmOperation::Disarm
                );
                assert!(row.cover_operations.is_empty() && row.lock_operations.is_empty());
                assert!(row.value.contains("triggered"));
                Arguments::Alarm(
                    wire::SetAlarmCommand {
                        operation: wire::AlarmOperation::Disarm.into(),
                        ..Default::default()
                    }
                    .into(),
                )
            }
        };
        let intent = store.choose(row.token, row.generation, 0, 0).unwrap();
        let prepared = store.prepare(row.token, row.generation, 0, intent).unwrap();
        assert_eq!(
            home_wire_contract::api_contract::encode_command(&prepared.request).command,
            Some(expected)
        );
        assert!((prepared.guard)());
        assert!(store.choose(row.token, row.generation, 0, -1).is_err());
        assert!(store.choose(row.token, row.generation, 0, 99).is_err());
        assert!(
            store
                .prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On))
                .is_err()
        );
        let refreshed = first(&store, &request, &entity, CachePhase::Live);
        assert_eq!(refreshed.value, row.value);
        assert_eq!(refreshed.generation, row.generation);
    }
}
#[test]
fn operation_withdrawal_cancels_prepared_intentions_and_never_revives_old_identity() {
    for family in [Family::Cover, Family::Lock, Family::Alarm] {
        let request = request();
        let original = family.entity(&request);
        let mut entity = original.clone();
        let store = DeviceStore::default();
        let row = first(&store, &request, &entity, CachePhase::Live);
        let intent = store.choose(row.token, row.generation, 0, 0).unwrap();
        let held = store
            .prepare(row.token, row.generation, 0, intent.clone())
            .unwrap();
        family.withdraw(&mut entity);
        let changed = first(&store, &request, &entity, CachePhase::Live);
        assert!(!(held.guard)());
        assert_ne!(changed.generation, row.generation);
        assert!(
            store
                .choose(changed.token, changed.generation, 0, 0)
                .is_err()
        );
        assert!(
            store
                .prepare(changed.token, changed.generation, 0, intent.clone())
                .is_err()
        );
        let restored = first(&store, &request, &original, CachePhase::Live);
        assert!(!(held.guard)());
        assert_ne!(restored.generation, row.generation);
        assert!(store.prepare(row.token, row.generation, 0, intent).is_err());
    }
}
#[test]
fn readonly_and_offline_revoke_each_secure_family_without_mutating_observations() {
    for family in [Family::Cover, Family::Lock, Family::Alarm] {
        let request = request();
        let mut entity = family.entity(&request);
        let store = DeviceStore::default();
        let row = first(&store, &request, &entity, CachePhase::Live);
        let intent = store.choose(row.token, row.generation, 0, 0).unwrap();
        let held = store
            .prepare(row.token, row.generation, 0, intent.clone())
            .unwrap();
        entity.capabilities[0].action_access = wire::ActionAccess::Denied.into();
        let denied = first(&store, &request, &entity, CachePhase::Live);
        assert_eq!(denied.access, ProControlAccess::Denied);
        assert_eq!(denied.value, row.value);
        assert!(!(held.guard)());
        assert!(matches!(
            store.prepare(denied.token, denied.generation, 0, intent.clone()),
            Err(Refusal::ReadOnly)
        ));
        entity.capabilities[0].action_access = wire::ActionAccess::Allowed.into();
        let live = first(&store, &request, &entity, CachePhase::Live);
        let held = store
            .prepare(live.token, live.generation, 0, intent.clone())
            .unwrap();
        let offline = first(&store, &request, &entity, CachePhase::Offline);
        assert_eq!(offline.value, row.value);
        assert!(!(held.guard)());
        assert!(
            store
                .prepare(offline.token, offline.generation, 0, intent)
                .is_err()
        );
    }
}
#[test]
fn cover_drafts_remain_separate_from_observations_and_require_explicit_valid_submission() {
    let request = request();
    let mut entity = Family::Cover.entity(&request);
    let store = DeviceStore::default();
    let row = first(&store, &request, &entity, CachePhase::Live);
    assert_eq!(row.values[0].lower, "35");
    assert_eq!(row.values[0].suffix, "%");
    assert!(
        store
            .edit(
                row.token,
                row.generation,
                0,
                0,
                event(crate::ProValueEvent::Draft, "-")
            )
            .is_err()
    );
    let value = cover_value(&mut entity);
    value.position_percent = Some(50.);
    let refreshed = first(&store, &request, &entity, CachePhase::Live);
    assert_eq!(refreshed.generation, row.generation);
    assert_eq!(refreshed.values[0].lower, "-");
    assert!(refreshed.value.contains("50 %"));
    assert!(
        store
            .edit(
                row.token,
                row.generation,
                0,
                0,
                event(crate::ProValueEvent::Submit, "37")
            )
            .is_err()
    );
    assert!(
        store
            .edit(
                row.token,
                row.generation,
                0,
                0,
                event(crate::ProValueEvent::Draft, "40")
            )
            .unwrap()
            .is_none()
    );
    let intent = store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            event(crate::ProValueEvent::Submit, "40"),
        )
        .unwrap()
        .unwrap();
    let prepared = store.prepare(row.token, row.generation, 0, intent).unwrap();
    assert!(
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(Arguments::Cover(value)) if value.target==Some(wire::set_cover_command::Target::PositionPercent(40.)))
    );
    let row = first(&store, &request, &entity, CachePhase::Live);
    assert_eq!(row.values[0].lower, "40");
    assert!(row.value.contains("50 %"));
    assert!(
        store
            .prepare(
                row.token,
                row.generation,
                0,
                DeviceIntent::Cover(CoverIntent::Position(NumericInput::Text("95".into())))
            )
            .is_err()
    );
}
#[test]
fn missing_and_outside_command_position_are_not_used_as_an_initial_command() {
    for position in [None, Some(0.), Some(35.)] {
        let request = request();
        let mut entity = Family::Cover.entity(&request);
        let value = cover_value(&mut entity);
        value.position_percent = position;
        let row = first(&DeviceStore::default(), &request, &entity, CachePhase::Live);
        assert_eq!(
            row.values[0].lower,
            position
                .filter(|p| *p >= 10.)
                .map(|p| p.to_string())
                .unwrap_or_default()
        );
    }
}

fn native_request(
    mut request: fixture::Scenario,
    entity: &wire::EntityViewEntry,
    features: Vec<crate::dashboard_layout::FeatureKind>,
    command: home_client_model::dashboard::CommandKind,
) -> fixture::Scenario {
    use crate::dashboard_layout::{NativeLayout, NodeId};
    use home_client_model::dashboard::{Gesture, Intention, Interaction, Recipes};
    let mut layout = request.dashboard.layout().unwrap().clone();
    let mut native =
        NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
    for node in native.nodes.values_mut() {
        if node.entity.is_some() {
            node.entity = Some(EntityName::try_from(entity.entity.name.as_str()).unwrap());
            node.capability =
                Some(CapabilityName::try_from(entity.capabilities[0].name.as_str()).unwrap());
        }
        node.recipes = None;
    }
    let tile = native
        .nodes
        .get_mut(&NodeId::parse("tile").unwrap())
        .unwrap();
    tile.features = features;
    tile.recipes = Some(Recipes {
        interactions: BTreeMap::from([(
            Gesture::Tap,
            Interaction {
                intention: Intention::Command {
                    capability: CapabilityName::try_from(entity.capabilities[0].name.as_str())
                        .unwrap(),
                    kind: command,
                },
                confirmation: Some("Confirmer la commande synthétique".into()),
                source: tile.source.clone(),
            },
        )]),
        visibility: None,
    });
    layout.native = Some(native);
    request.room = None;
    request.dashboard = crate::dashboard::LayoutState::Ready(Arc::new(layout));
    request
}
#[test]
fn cover_body_features_and_header_recipes_have_distinct_admission_without_extra_endpoints() {
    use crate::dashboard_layout::{FeatureKind, NodeId};
    use home_client_model::dashboard::{CommandKind, Gesture};
    let initial = request();
    let entity = Family::Cover.entity(&initial);
    let request = native_request(
        initial,
        &entity,
        vec![FeatureKind::CoverPosition],
        CommandKind::CoverOpen,
    );
    let store = DeviceStore::default();
    let projection = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    let row = &projection.native_devices[&NodeId::parse("tile").unwrap()];
    assert!(row.cover_operations.is_empty());
    assert_eq!(row.values.len(), 1);
    assert!(
        store
            .prepare(
                row.token,
                row.generation,
                0,
                DeviceIntent::Cover(CoverIntent::Operation(wire::CoverOperation::Open))
            )
            .is_err()
    );
    assert!(
        store
            .prepare(
                row.token,
                row.generation,
                0,
                DeviceIntent::Cover(CoverIntent::Position(NumericInput::Text("40".into())))
            )
            .is_ok()
    );
    let recipe = &projection.recipes[&NodeId::parse("tile").unwrap()];
    assert_eq!(recipe.access(Gesture::Tap), GestureAccess::Command);
    let prepared = store
        .prepare_recipe(recipe.token, recipe.generation, 0, Gesture::Tap)
        .unwrap();
    assert!(prepared.is_current());
    assert!(prepared.confirmation.is_some());
    let NativeIntent::Command(command) = prepared.intent else {
        panic!()
    };
    assert!(
        matches!(home_wire_contract::api_contract::encode_command(&command.request).command.as_ref(),Some(Arguments::Cover(value)) if value.target==Some(wire::set_cover_command::Target::Operation(wire::CoverOperation::Open.into())))
    );
    let mut entity = entity;
    Family::Cover.withdraw(&mut entity);
    store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity])),
        &request,
    );
    assert!(!(command.guard)());
}
#[test]
fn native_alarm_recipe_requires_alarm_family_and_refuses_incompatible_features() {
    use crate::dashboard_layout::{FeatureKind, Kind, NodeId, RecipeKind};
    use home_client_model::dashboard::CommandKind;
    for family in [Family::Cover, Family::Lock, Family::Alarm] {
        let initial = request();
        let entity = family.entity(&initial);
        let mut request = native_request(
            initial,
            &entity,
            vec![FeatureKind::AlarmModes],
            CommandKind::AlarmDisarm,
        );
        let mut layout = request.dashboard.layout().unwrap().clone();
        let tile = layout
            .native
            .as_mut()
            .unwrap()
            .nodes
            .get_mut(&NodeId::parse("tile").unwrap())
            .unwrap();
        tile.kind = Kind::Recipe;
        tile.recipe_kind = Some(RecipeKind::Alarm);
        request.dashboard = crate::dashboard::LayoutState::Ready(Arc::new(layout));
        let store = DeviceStore::default();
        let projection = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        match family {
            Family::Alarm => {
                let row = &projection.native_devices[&NodeId::parse("tile").unwrap()];
                assert_eq!(row.alarm_operations.len(), 2);
                assert!(
                    store
                        .prepare(
                            row.token,
                            row.generation,
                            0,
                            DeviceIntent::Alarm(wire::AlarmOperation::Disarm)
                        )
                        .is_ok()
                );
                assert!(
                    store
                        .prepare(
                            row.token,
                            row.generation,
                            0,
                            DeviceIntent::Alarm(wire::AlarmOperation::ArmHome)
                        )
                        .is_err()
                );
            }
            Family::Cover | Family::Lock => {
                assert!(
                    !projection
                        .native_devices
                        .contains_key(&NodeId::parse("tile").unwrap())
                );
                let node = projection
                    .native_nodes
                    .iter()
                    .find(|node| node.id.as_str() == "tile")
                    .unwrap();
                assert_eq!(node.kind, Kind::Unsupported);
                assert!(!node.source.file.is_empty());
                assert!(!node.diagnostic.is_empty());
            }
        }
    }
}
#[test]
fn secure_header_commands_resolve_exact_families_and_reject_unadvertised_operations() {
    use crate::dashboard_layout::NodeId;
    use home_client_model::dashboard::{CommandKind as C, Gesture};
    let commands = [
        C::CoverOpen,
        C::CoverClose,
        C::CoverStop,
        C::Lock,
        C::Unlock,
        C::AlarmArmHome,
        C::AlarmArmAway,
        C::AlarmArmNight,
        C::AlarmDisarm,
    ];
    for family in [Family::Cover, Family::Lock, Family::Alarm] {
        for command in commands {
            let initial = request();
            let entity = family.entity(&initial);
            let request = native_request(initial, &entity, Vec::new(), command);
            let store = DeviceStore::default();
            let projection = store.project(
                CachePhase::Live,
                Some(&view(&request, vec![entity.clone()])),
                &request,
            );
            let recipe = &projection.recipes[&NodeId::parse("tile").unwrap()];
            let expected = match (family, command) {
                (Family::Cover, C::CoverOpen | C::CoverClose | C::CoverStop)
                | (Family::Lock, C::Lock | C::Unlock)
                | (Family::Alarm, C::AlarmArmAway | C::AlarmDisarm) => true,
                (
                    Family::Cover,
                    C::Toggle
                    | C::TurnOn
                    | C::TurnOff
                    | C::Activate
                    | C::Lock
                    | C::Unlock
                    | C::AlarmArmHome
                    | C::AlarmArmAway
                    | C::AlarmArmNight
                    | C::AlarmDisarm,
                )
                | (
                    Family::Lock,
                    C::Toggle
                    | C::TurnOn
                    | C::TurnOff
                    | C::Activate
                    | C::CoverOpen
                    | C::CoverClose
                    | C::CoverStop
                    | C::AlarmArmHome
                    | C::AlarmArmAway
                    | C::AlarmArmNight
                    | C::AlarmDisarm,
                )
                | (
                    Family::Alarm,
                    C::Toggle
                    | C::TurnOn
                    | C::TurnOff
                    | C::Activate
                    | C::CoverOpen
                    | C::CoverClose
                    | C::CoverStop
                    | C::Lock
                    | C::Unlock
                    | C::AlarmArmHome
                    | C::AlarmArmNight,
                ) => false,
            };
            assert_eq!(
                recipe.access(Gesture::Tap) == GestureAccess::Command,
                expected
            );
            let prepared = store.prepare_recipe(recipe.token, recipe.generation, 0, Gesture::Tap);
            assert_eq!(prepared.is_ok(), expected);
            if let Ok(prepared) = prepared {
                assert!(prepared.is_current());
                assert!(matches!(prepared.intent, NativeIntent::Command(_)));
            }
        }
    }
}

#[test]
fn night_selection_uses_the_advertised_token_and_observed_mode() {
    let request = request();
    let mut entity = Family::Alarm.entity(&request);
    let Some(wire::capability::Trait::Alarm(capability)) = entity.capabilities[0].r#trait.as_mut()
    else {
        panic!("alarm fixture")
    };
    capability
        .definition
        .as_option_mut()
        .unwrap()
        .operations
        .push(wire::AlarmOperation::ArmNight.into());
    capability
        .definition
        .as_option_mut()
        .unwrap()
        .observed_states
        .push(wire::AlarmState::ArmedNight.into());
    capability
        .observation
        .as_option_mut()
        .unwrap()
        .value
        .as_option_mut()
        .unwrap()
        .state = wire::AlarmState::ArmedNight.into();
    let store = DeviceStore::default();
    let row = first(&store, &request, &entity, CachePhase::Live);
    assert_eq!(
        row.alarm_operations[2].operation,
        crate::ProAlarmOperation::ArmNight
    );
    let panel = super::detail::project(&row, row.ui(None, &Default::default()), None);
    assert_eq!(panel.alarm.condition, crate::ProAlarmCondition::Armed);
    assert!(panel.alarm.modes.row_data(2).unwrap().selected);
    let intent = store.choose(row.token, row.generation, 0, 2).unwrap();
    let prepared = store.prepare(row.token, row.generation, 0, intent).unwrap();
    let Some(Arguments::Alarm(command)) =
        home_wire_contract::api_contract::encode_command(&prepared.request).command
    else {
        panic!("typed alarm request")
    };
    assert_eq!(
        command.operation.as_known(),
        Some(wire::AlarmOperation::ArmNight)
    );
}

#[test]
fn close_only_cover_preserves_observation_without_position_or_other_commands() {
    let request = request();
    let mut entity = Family::Cover.entity(&request);
    let Some(wire::capability::Trait::Cover(capability)) = entity.capabilities[0].r#trait.as_mut()
    else {
        panic!("cover fixture")
    };
    let definition = capability.definition.as_option_mut().unwrap();
    definition.operations = vec![wire::CoverOperation::Close.into()];
    definition.set_position_percent = Default::default();
    use crate::dashboard_layout::{FeatureKind, NodeId};
    use home_client_model::dashboard::CommandKind;
    let request = native_request(
        request,
        &entity,
        vec![FeatureKind::CoverOpenClose, FeatureKind::CoverPosition],
        CommandKind::CoverClose,
    );
    let store = DeviceStore::default();
    let projection = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity])),
        &request,
    );
    let row = projection
        .native_devices
        .get(&NodeId::parse("tile").unwrap())
        .expect("configured cover features stay supported");
    assert_eq!(row.kind, ProDeviceKind::Cover);
    assert_eq!(row.cover_operations.len(), 1);
    assert_eq!(
        row.cover_operations[0].operation,
        crate::ProCoverOperation::Close
    );
    assert!(row.values.is_empty());
    let panel = super::detail::project(row, row.ui(None, &Default::default()), None);
    assert!(panel.cover.position_known);
    assert_eq!(panel.cover.position, 35.);
    assert!(!panel.positioned);
    for intent in [
        CoverIntent::Operation(wire::CoverOperation::Open),
        CoverIntent::Operation(wire::CoverOperation::Stop),
        CoverIntent::Position(NumericInput::Text("0".into())),
    ] {
        assert!(
            store
                .prepare(row.token, row.generation, 0, DeviceIntent::Cover(intent))
                .is_err()
        );
    }
    let intent = store.choose(row.token, row.generation, 0, 0).unwrap();
    let prepared = store.prepare(row.token, row.generation, 0, intent).unwrap();
    let Some(Arguments::Cover(command)) =
        home_wire_contract::api_contract::encode_command(&prepared.request).command
    else {
        panic!("typed cover request")
    };
    assert_eq!(
        command.target,
        Some(wire::set_cover_command::Target::Operation(
            wire::CoverOperation::Close.into()
        ))
    );
}

#[test]
fn configured_detail_title_keeps_its_exact_opening_node_without_an_entity_override() {
    use crate::dashboard_layout::{FeatureKind, NodeId};
    use home_client_model::dashboard::{CommandKind, Gesture, Intention};
    use slint::ComponentHandle;
    i_slint_backend_testing::init_no_event_loop();
    let initial = request();
    let mut entity = Family::Alarm.entity(&initial);
    entity.entity.get_or_insert_default().display_name.clear();
    let mut scenario = native_request(
        initial,
        &entity,
        vec![FeatureKind::AlarmModes],
        CommandKind::AlarmArmHome,
    );
    let id = EntityName::try_from(entity.entity.name.as_str()).unwrap();
    let source = NodeId::parse("tile").unwrap();
    let mut layout = scenario.dashboard.layout().unwrap().clone();
    let node = layout
        .native
        .as_mut()
        .unwrap()
        .nodes
        .get_mut(&source)
        .unwrap();
    node.title = "Configured entrance control".into();
    node.recipes
        .as_mut()
        .unwrap()
        .interactions
        .get_mut(&Gesture::Tap)
        .unwrap()
        .intention = Intention::MoreInfo(id.clone());
    scenario.dashboard = crate::dashboard::LayoutState::Ready(Arc::new(layout));
    let store = DeviceStore::default();
    let cache = view(&scenario, vec![entity.clone()]);
    let initial = store.project(CachePhase::Live, Some(&cache), &scenario);
    let recipe = &initial.recipes[&source];
    let prepared = store
        .prepare_recipe(
            recipe.token,
            recipe.generation,
            scenario.revision,
            Gesture::Tap,
        )
        .unwrap();
    let window = crate::create_window().unwrap();
    crate::cache_view::bind(&window);
    let shared = Arc::new(Mutex::new(scenario.request.clone()));
    let navigation = crate::navigation::bind_devices(&window, shared.clone(), || {}, || None);
    navigation
        .more_info(
            id,
            Some(crate::dashboard_focus::Origin::prepared(&prepared)),
        )
        .unwrap();
    assert_eq!(shared.lock().unwrap().detail_source.as_ref(), Some(&source));
    let publish = |entity: &wire::EntityViewEntry| {
        let cache = view(&scenario, vec![entity.clone()]);
        let request = shared.lock().unwrap().clone();
        let projection = crate::cache_view::project_devices(
            CachePhase::Live,
            Some(&cache),
            home_client_cache::Viewport::default(),
            &request,
            &store,
        );
        crate::cache_view::render(
            &window,
            home_client_cache::ProjectionMailbox::<()>::new().generation(),
            projection,
        );
    };
    publish(&entity);
    assert_eq!(window.get_detail_title(), "Configured entrance control");
    assert_eq!(
        window.get_detail_panels().row_data(0).unwrap().device.title,
        "Configured entrance control"
    );
    entity.entity.get_or_insert_default().display_name = "Registry default".into();
    publish(&entity);
    assert_eq!(
        window.get_detail_title(),
        "Configured entrance control",
        "the configured source remains authoritative"
    );
    {
        let mut request = shared.lock().unwrap();
        let mut layout = request.dashboard.layout().unwrap().clone();
        let node = layout
            .native
            .as_mut()
            .unwrap()
            .nodes
            .get_mut(&source)
            .unwrap();
        node.entity = Some(EntityName::try_from("homes/synthetic/entities/unrelated").unwrap());
        request.dashboard = crate::dashboard::LayoutState::Ready(Arc::new(layout));
    }
    publish(&entity);
    assert_eq!(
        window.get_detail_title(),
        "Registry default",
        "a source rebound to another entity cannot rename this detail"
    );
    assert_eq!(
        window.get_detail_panels().row_data(0).unwrap().device.title,
        "Registry default"
    );
    window.invoke_go_back();
    assert!(shared.lock().unwrap().detail_source.is_none());
    assert!(store.0.lock().unwrap().actions.is_empty());
    window.hide().unwrap();
}

fn interactive_alarm(request: &ProjectionRequest) -> wire::EntityViewEntry {
    let mut entity = Family::Alarm.entity(request);
    let Some(wire::capability::Trait::Alarm(alarm)) = entity.capabilities[0].r#trait.as_mut()
    else {
        unreachable!()
    };
    alarm
        .definition
        .get_or_insert_default()
        .code_required_operations = vec![wire::AlarmOperation::Disarm.into()];
    entity
}
#[test]
fn keypad_is_ephemeral_per_operation_and_survives_live_observation_updates() {
    let request = request();
    let mut entity = interactive_alarm(&request);
    let store = DeviceStore::default();
    let row = first(&store, &request, &entity, CachePhase::Live);
    assert!(
        store
            .alarm_choice(row.token, row.generation, 0, 0)
            .unwrap()
            .is_none()
    );
    assert!(
        store
            .prepare(
                row.token,
                row.generation,
                0,
                DeviceIntent::Alarm(wire::AlarmOperation::Disarm)
            )
            .is_err()
    );
    // Re-select after the rejected empty submission, then enter across normal observations.
    store.alarm_choice(row.token, row.generation, 0, 0).unwrap();
    store
        .alarm_digit(row.token, row.generation, 0, Some(2))
        .unwrap();
    let Some(wire::capability::Trait::Alarm(alarm)) = entity.capabilities[0].r#trait.as_mut()
    else {
        unreachable!()
    };
    alarm
        .observation
        .get_or_insert_default()
        .quality
        .get_or_insert_default()
        .provenance = wire::Provenance::Observed.into();
    let refreshed = first(&store, &request, &entity, CachePhase::Live);
    store
        .alarm_digit(refreshed.token, refreshed.generation, 0, Some(7))
        .unwrap();
    let refreshed = first(&store, &request, &entity, CachePhase::Live);
    let panel = super::detail::project(&refreshed, refreshed.ui(None, &Default::default()), None);
    assert!(panel.alarm_code_required);
    assert_eq!(panel.alarm.entered_count, 2);
    assert!(panel.alarm.can_confirm);
    let intent = store
        .alarm_confirm(refreshed.token, refreshed.generation, 0)
        .unwrap();
    let prepared = store
        .prepare(refreshed.token, refreshed.generation, 0, intent)
        .unwrap();
    assert!(prepared.alarm_code.is_some());
    assert!(
        home_wire_contract::api_contract::encode_command(&prepared.request)
            .ephemeral_alarm_code
            .is_empty()
    );
    assert!(
        store
            .alarm_confirm(refreshed.token, refreshed.generation, 0)
            .is_err()
    );
    let row = first(&store, &request, &entity, CachePhase::Live);
    assert!(matches!(
        store.alarm_choice(row.token, row.generation, 0, 1).unwrap(),
        Some(DeviceIntent::Alarm(wire::AlarmOperation::ArmAway))
    ));
    let prepared = store
        .prepare(
            row.token,
            row.generation,
            0,
            DeviceIntent::Alarm(wire::AlarmOperation::ArmAway),
        )
        .unwrap();
    assert!(prepared.alarm_code.is_none());
}
#[test]
fn keypad_revocation_close_and_bounds_never_restore_digits() {
    for revoke in [true, false] {
        let request = request();
        let mut entity = interactive_alarm(&request);
        let store = DeviceStore::default();
        let row = first(&store, &request, &entity, CachePhase::Live);
        store.alarm_choice(row.token, row.generation, 0, 0).unwrap();
        for _ in 0..32 {
            store
                .alarm_digit(row.token, row.generation, 0, Some(6))
                .unwrap();
        }
        assert!(
            store
                .alarm_digit(row.token, row.generation, 0, Some(0))
                .is_err()
        );
        assert!(
            store
                .alarm_digit(row.token, row.generation, 0, Some(-1))
                .is_err()
        );
        if revoke {
            entity.capabilities[0].action_access = wire::ActionAccess::Denied.into();
            first(&store, &request, &entity, CachePhase::Live);
            entity.capabilities[0].action_access = wire::ActionAccess::Allowed.into();
        } else {
            store.clear_private();
        }
        let restored = first(&store, &request, &entity, CachePhase::Live);
        let panel = super::detail::project(&restored, restored.ui(None, &Default::default()), None);
        assert_eq!(panel.alarm.entered_count, 0);
        assert!(!panel.alarm.can_confirm);
        assert!(
            store
                .alarm_confirm(restored.token, restored.generation, 0)
                .is_err()
        );
    }
}
#[test]
fn explicit_associated_lock_keeps_independent_authority_and_identity() {
    use crate::dashboard_layout::{CoverLock, FeatureKind, NodeId};
    use home_client_model::dashboard::CommandKind;
    let initial = request();
    let mut entity = Family::Cover.entity(&initial);
    let mut lock = Family::Lock.entity(&initial).capabilities.remove(0);
    lock.name = format!("{}/capabilities/remote-controls", entity.entity.name);
    lock.action_access = wire::ActionAccess::Denied.into();
    let lock_name = CapabilityName::try_from(lock.name.as_str()).unwrap();
    entity.capabilities.push(lock);
    let mut request = native_request(
        initial,
        &entity,
        vec![FeatureKind::CoverOpenClose, FeatureKind::CoverPosition],
        CommandKind::CoverClose,
    );
    let source = NodeId::parse("tile").unwrap();
    let mut layout = request.dashboard.layout().unwrap().clone();
    layout
        .native
        .as_mut()
        .unwrap()
        .nodes
        .get_mut(&source)
        .unwrap()
        .cover_lock = Some(CoverLock {
        capability: lock_name,
        label: "Remote controls".into(),
    });
    request.dashboard = crate::dashboard::LayoutState::Ready(Arc::new(layout));
    request.detail = Some(EntityName::try_from(entity.entity.name.as_str()).unwrap());
    request.detail_source = Some(source);
    let store = DeviceStore::default();
    let projection = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    let cover = projection
        .rows
        .iter()
        .find(|row| row.kind == ProDeviceKind::Cover)
        .unwrap();
    let lock = projection
        .rows
        .iter()
        .find(|row| row.kind == ProDeviceKind::Lock)
        .unwrap();
    let panel = super::detail::project(cover, cover.ui(None, &Default::default()), None);
    assert_eq!(panel.associated_lock.title, "Remote controls");
    assert_eq!(
        (
            panel.associated_lock.token,
            panel.associated_lock.generation
        ),
        (lock.token, lock.generation)
    );
    assert_ne!(panel.associated_lock.token, cover.token);
    assert!(!panel.lock_commands_enabled);
    assert!(lock.detail.associated_only);
    assert!(matches!(
        store.prepare(
            lock.token,
            lock.generation,
            request.revision,
            DeviceIntent::Lock(wire::LockOperation::Lock)
        ),
        Err(Refusal::ReadOnly)
    ));
    assert!(
        store
            .prepare(
                cover.token,
                cover.generation,
                request.revision,
                DeviceIntent::Cover(CoverIntent::Operation(wire::CoverOperation::Close))
            )
            .is_ok()
    );
}

#[test]
fn keypad_counts_are_adaptive_and_a_rebound_capability_cannot_reuse_input() {
    let request = request();
    let mut entity = interactive_alarm(&request);
    let store = DeviceStore::default();
    let mut row = first(&store, &request, &entity, CachePhase::Live);
    // The unique operation requiring a code is selected on the first digit.
    for count in 1..=32 {
        store
            .alarm_digit(row.token, row.generation, 0, Some(5))
            .unwrap();
        row = first(&store, &request, &entity, CachePhase::Live);
        let panel = super::detail::project(&row, row.ui(None, &Default::default()), None);
        assert_eq!(panel.alarm.entered_count, count);
        assert_eq!(panel.alarm.code_length, count.max(4));
        assert_eq!(panel.alarm.code_label, format!("{count} digits entered"));
        assert!(panel.alarm.can_confirm);
    }
    assert!(matches!(
        store.alarm_confirm(row.token, row.generation, 0),
        Ok(DeviceIntent::Alarm(wire::AlarmOperation::Disarm))
    ));
    let original = entity.capabilities[0].name.clone();
    entity.capabilities[0].name = format!("{}/capabilities/rebound-alarm", entity.entity.name);
    let rebound = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    assert!(
        rebound.rows.is_empty(),
        "the configured capability is no longer available"
    );
    entity.capabilities[0].name = original;
    let restored = first(&store, &request, &entity, CachePhase::Live);
    let panel = super::detail::project(&restored, restored.ui(None, &Default::default()), None);
    assert_eq!(panel.alarm.entered_count, 0);
    assert!(!panel.alarm.can_confirm);
    assert!(store.alarm_confirm(row.token, row.generation, 0).is_err());
}

#[tokio::test]
async fn keypad_archive_is_bounded_and_cannot_restore_across_revocation_or_private_clear() {
    #[derive(Clone, Copy)]
    enum Change {
        None,
        Denied,
        Definition,
        PrivateClear,
        Offline,
        Expired,
    }
    for change in [
        Change::None,
        Change::Denied,
        Change::Definition,
        Change::PrivateClear,
        Change::Offline,
        Change::Expired,
    ] {
        let scenario = request();
        let mut request = scenario.request.clone();
        let mut entity = interactive_alarm(&request);
        let first_view = scenario.view(vec![entity.clone()]);
        let store = DeviceStore::default();
        let old = store
            .project(CachePhase::Live, Some(&first_view), &request)
            .rows
            .remove(0);
        store
            .alarm_digit(old.token, old.generation, request.revision, Some(0))
            .unwrap();
        let pending = fixture::capture(vec![entity.clone()]);
        assert!(
            store
                .project(CachePhase::Live, Some(&pending), &request)
                .rows
                .is_empty()
        );
        assert!(
            store
                .alarm_confirm(old.token, old.generation, request.revision)
                .is_err()
        );
        let (serial, deadline) = {
            let state = store.0.lock().unwrap();
            let saved = state.retained_drafts.as_ref().expect("PIN-only archive");
            assert_ne!(
                saved.serial, 0,
                "PIN-only archive arms expiry independently of UI"
            );
            (saved.serial, saved.deadline())
        };
        store.project(
            CachePhase::Live,
            Some(&fixture::capture(vec![entity.clone()])),
            &request,
        );
        {
            let state = store.0.lock().unwrap();
            let saved = state.retained_drafts.as_ref().unwrap();
            assert_eq!(
                (saved.serial, saved.deadline()),
                (serial, deadline),
                "unjoined captures grant no grace"
            );
        }
        match change {
            Change::None => {}
            Change::Denied => {
                entity.capabilities[0].action_access = wire::ActionAccess::Denied.into()
            }
            Change::Definition => entity.capabilities[0].etag = "replacement-definition".into(),
            Change::PrivateClear => store.discard_retained_drafts(),
            Change::Offline => {
                store.project(CachePhase::Offline, None, &request);
            }
            Change::Expired => {
                let mut state = store.0.lock().unwrap();
                assert!(
                    super::draft_retention::expire(&mut state, serial, deadline, deadline)
                        .is_none()
                );
                assert!(state.retained_drafts.is_none());
            }
        }
        let joined = fixture::capture(vec![entity.clone()]);
        request.dashboard = crate::dashboard::LayoutState::Ready(Arc::new(
            crate::dashboard::joined_fixture(&joined),
        ));
        let row = store
            .project(CachePhase::Live, Some(&joined), &request)
            .rows
            .remove(0);
        let panel = super::detail::project(&row, row.ui(None, &Default::default()), None);
        assert_eq!(
            panel.alarm.entered_count,
            if matches!(change, Change::None) { 1 } else { 0 }
        );
        assert_eq!(panel.alarm.can_confirm, matches!(change, Change::None));
        assert!(store.0.lock().unwrap().retained_drafts.is_none());
        if !matches!(change, Change::None) {
            entity.capabilities[0].action_access = wire::ActionAccess::Allowed.into();
            entity.capabilities[0].etag = "capability-definition".into();
            let restored = fixture::capture(vec![entity]);
            request.dashboard = crate::dashboard::LayoutState::Ready(Arc::new(
                crate::dashboard::joined_fixture(&restored),
            ));
            let row = store
                .project(CachePhase::Live, Some(&restored), &request)
                .rows
                .remove(0);
            assert!(
                store
                    .alarm_confirm(row.token, row.generation, request.revision)
                    .is_err(),
                "reappearance never resurrects a code"
            );
        }
    }
}
