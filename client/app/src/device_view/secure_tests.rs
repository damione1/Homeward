//! Host integration for SDK secure-control rows, opaque choice tokens, drafts
//! and revocation. Synthetic entities only; no transport/device side effects.
use super::*;
use super::{
    controls::CoverIntent,
    number::NumericInput,
    tests::{light, request, view},
};
use actions::action_args::Arguments;
use home_wire_contract::home::controls::v1 as wire;
#[derive(Clone, Copy)]
enum Family {
    Cover,
    Lock,
    Alarm,
}
impl Family {
    fn entity(self, request: &ProjectionRequest) -> state::ViewEntity {
        let mut entity = light(request);
        let (schema, constraints, observed) = match self {
            Self::Cover => (
                "core.cover.v1",
                registry::capability_description::Constraints::Cover(
                    wire::CoverConstraints {
                        operations: vec![
                            wire::CoverOperation::Stop.into(),
                            wire::CoverOperation::Open.into(),
                            wire::CoverOperation::Close.into(),
                        ],
                        set_position_percent: wire::PercentBounds {
                            minimum: 10.,
                            maximum: 90.,
                            step: Some(5.),
                            ..Default::default()
                        }
                        .into(),
                        observed_states: vec![wire::CoverState::Open.into()],
                        observes_position_percent: true,
                        observes_obstruction: true,
                        ..Default::default()
                    }
                    .into(),
                ),
                state::observed_state::Value::Cover(
                    wire::CoverValue {
                        state: Some(wire::CoverState::Open.into()),
                        position_percent: Some(35.),
                        obstructed: Some(false),
                        ..Default::default()
                    }
                    .into(),
                ),
            ),
            Self::Lock => (
                "core.lock.v1",
                registry::capability_description::Constraints::Lock(
                    wire::LockConstraints {
                        operations: vec![
                            wire::LockOperation::Unlock.into(),
                            wire::LockOperation::Lock.into(),
                        ],
                        observed_states: vec![wire::LockState::Jammed.into()],
                        ..Default::default()
                    }
                    .into(),
                ),
                state::observed_state::Value::Lock(
                    wire::LockValue {
                        state: wire::LockState::Jammed.into(),
                        ..Default::default()
                    }
                    .into(),
                ),
            ),
            Self::Alarm => (
                "core.alarm.v1",
                registry::capability_description::Constraints::Alarm(
                    wire::AlarmConstraints {
                        operations: vec![
                            wire::AlarmOperation::Disarm.into(),
                            wire::AlarmOperation::ArmAway.into(),
                        ],
                        observed_states: vec![wire::AlarmState::Triggered.into()],
                        ..Default::default()
                    }
                    .into(),
                ),
                state::observed_state::Value::Alarm(
                    wire::AlarmValue {
                        state: wire::AlarmState::Triggered.into(),
                        ..Default::default()
                    }
                    .into(),
                ),
            ),
        };
        entity.state.as_option_mut().unwrap().quality = state::Quality {
            availability: state::Availability::Available.into(),
            freshness: state::Freshness::Fresh.into(),
            provenance: state::Provenance::Observed.into(),
            ..Default::default()
        }
        .into();
        entity.capabilities[0].schema_id = schema.into();
        entity.capabilities[0].constraints = Some(constraints);
        entity.action_access[0].schema_id = schema.into();
        entity.state.as_option_mut().unwrap().value = Some(observed);
        entity
    }
    fn kind(self) -> ProDeviceKind {
        match self {
            Self::Cover => ProDeviceKind::Cover,
            Self::Lock => ProDeviceKind::Lock,
            Self::Alarm => ProDeviceKind::Alarm,
        }
    }
    fn withdraw(self, entity: &mut state::ViewEntity) {
        use registry::capability_description::Constraints as C;
        match entity.capabilities[0].constraints.as_mut().unwrap() {
            C::Cover(c) => c.operations.clear(),
            C::Lock(c) => c.operations.clear(),
            C::Alarm(c) => c.operations.clear(),
            C::Light(_)
            | C::Switch(_)
            | C::Climate(_)
            | C::Fan(_)
            | C::Helper(_)
            | C::Measurement(_)
            | C::Binary(_)
            | C::Solar(_)
            | C::SolarEvent(_)
            | C::Extension(_) => panic!("fixture family"),
        }
    }
}
fn first(
    store: &DeviceStore,
    request: &ProjectionRequest,
    entity: &state::ViewEntity,
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
                    wire::CoverAction {
                        command: Some(wire::cover_action::Command::Operation(
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
                assert!(row.value.contains("bloquée"));
                Arguments::Lock(
                    wire::LockAction {
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
                assert!(row.value.contains("déclenchée"));
                Arguments::Alarm(
                    wire::AlarmAction {
                        operation: wire::AlarmOperation::Disarm.into(),
                        ..Default::default()
                    }
                    .into(),
                )
            }
        };
        let intent = store.choose(row.token, row.generation, 0, 0).unwrap();
        let prepared = store.prepare(row.token, row.generation, 0, intent).unwrap();
        assert_eq!(prepared.request.arguments.arguments, Some(expected));
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
        entity.action_access[0].access = state::ActionAccess::Denied.into();
        let denied = first(&store, &request, &entity, CachePhase::Live);
        assert_eq!(denied.access, ProControlAccess::Denied);
        assert_eq!(denied.value, row.value);
        assert!(!(held.guard)());
        assert!(matches!(
            store.prepare(denied.token, denied.generation, 0, intent.clone()),
            Err(Refusal::ReadOnly)
        ));
        entity.action_access[0].access = state::ActionAccess::Allowed.into();
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
    let Some(state::observed_state::Value::Cover(value)) =
        entity.state.as_option_mut().unwrap().value.as_mut()
    else {
        panic!()
    };
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
        matches!(prepared.request.arguments.arguments.as_ref(),Some(Arguments::Cover(value)) if value.command==Some(wire::cover_action::Command::PositionPercent(40.)))
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
        let Some(state::observed_state::Value::Cover(value)) =
            entity.state.as_option_mut().unwrap().value.as_mut()
        else {
            panic!()
        };
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
    mut request: ProjectionRequest,
    entity: &state::ViewEntity,
    features: Vec<crate::dashboard_layout::FeatureKind>,
    command: home_client_model::dashboard::CommandKind,
) -> ProjectionRequest {
    use crate::dashboard_layout::{NativeLayout, NodeId};
    use home_client_model::dashboard::{Gesture, Intention, Interaction, Recipes};
    let mut layout = request.dashboard.layout().unwrap().clone();
    let mut native =
        NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
    for node in native.nodes.values_mut() {
        if node.entity.is_some() {
            node.entity = Some(entity.entity_id.parse().unwrap());
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
                    entity: entity.entity_id.parse().unwrap(),
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
        matches!(command.request.arguments.arguments.as_ref(),Some(Arguments::Cover(value)) if value.command==Some(wire::cover_action::Command::Operation(wire::CoverOperation::Open.into())))
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
                    | C::AlarmArmHome,
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
