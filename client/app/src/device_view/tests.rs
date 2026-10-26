use super::*;
use crate::dashboard::{self, LayoutState};
use buffa_types::google::protobuf::Timestamp;
use home_client_cache::{Filter, SnapshotCapture};
pub(super) fn request() -> ProjectionRequest {
    let layout = dashboard::fixture();
    ProjectionRequest {
        room: Some(layout.rooms[1].id.clone()),
        dashboard: LayoutState::Ready(Arc::new(layout)),
        ..Default::default()
    }
}
pub(super) fn light(request: &ProjectionRequest) -> state::ViewEntity {
    let layout = request.dashboard.layout().unwrap();
    let id = layout
        .room(request.room.as_ref().unwrap())
        .unwrap()
        .entities
        .iter()
        .next()
        .unwrap();
    state::ViewEntity {
        entity_id: id.as_str().into(),
        current_alias: "Synthetic controlled light".into(),
        capabilities: vec![registry::CapabilityDescription {
            schema_id: "core.light.v1".into(),
            major: 1,
            endpoint: "lamp".into(),
            constraints: Some(registry::capability_description::Constraints::Light(
                registry::LightConstraints {
                    set_on: true,
                    brightness_percent: registry::NumberBounds {
                        minimum: 20.,
                        maximum: 80.,
                        step: Some(5.),
                        ..Default::default()
                    }
                    .into(),
                    ..Default::default()
                }
                .into(),
            )),
            ..Default::default()
        }],
        action_access: vec![state::CapabilityActionAccess {
            schema_id: "core.light.v1".into(),
            endpoint: "lamp".into(),
            access: state::ActionAccess::Allowed.into(),
            ..Default::default()
        }],
        state: state::ObservedState {
            entity: registry::EntityRef {
                reference: Some(registry::entity_ref::Reference::EntityId(
                    id.as_str().into(),
                )),
                ..Default::default()
            }
            .into(),
            revision: 1,
            value: Some(state::observed_state::Value::Light(
                state::LightValue {
                    brightness_percent: Some(30.),
                    ..Default::default()
                }
                .into(),
            )),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    }
}
pub(super) fn view(request: &ProjectionRequest, entities: Vec<state::ViewEntity>) -> CachedView {
    let layout = request.dashboard.layout().unwrap();
    let filter = Filter::all();
    let now = Timestamp::from_unix_secs(0);
    let first = state::OpenViewResponse {
        snapshot_token: "synthetic-capture".into(),
        cursor: state::ViewCursor {
            epoch: "aabbccddeeff00112233445566778899".into(),
            global_sequence: 1,
            config_revision: layout.config_revision,
            authorization_revision: layout.authorization_revision,
            config_sha256: layout.config_hash.to_vec(),
            grant_sha256: vec![2; 32],
            filter_sha256: filter.digest().to_vec(),
            admission_proof: vec![3; 40],
            ..Default::default()
        }
        .into(),
        expires_at: Timestamp::from_unix_secs(60).into(),
        entities,
        ..Default::default()
    };
    SnapshotCapture::begin(filter, 256, first, &now)
        .unwrap()
        .finish(&now)
        .unwrap()
}
fn prepare(store: &DeviceStore, row: &CardRow) -> Prepared {
    store
        .prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On))
        .ok()
        .unwrap()
}
#[test]
fn visibility_withdraws_a_held_gesture_even_when_a_duplicate_entity_stays_visible() {
    use crate::dashboard_layout::{Kind, NativeLayout, NodeId};
    use home_client_model::dashboard::{Comparison, Predicate, Recipes};
    let mut request = request();
    let mut entity = light(&request);
    entity.state.as_option_mut().unwrap().quality = state::Quality {
        availability: state::Availability::Available.into(),
        freshness: state::Freshness::Fresh.into(),
        provenance: state::Provenance::Observed.into(),
        ..Default::default()
    }
    .into();
    let set_on = |entity: &mut state::ViewEntity, on| {
        let Some(state::observed_state::Value::Light(value)) =
            entity.state.as_option_mut().unwrap().value.as_mut()
        else {
            panic!()
        };
        value.on = Some(on);
    };
    set_on(&mut entity, true);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let mut native =
        NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
    for node in native.nodes.values_mut() {
        if node.entity.is_some() {
            node.entity = Some(entity.entity_id.parse().unwrap());
        }
    }
    native
        .nodes
        .get_mut(&NodeId::parse("grid").unwrap())
        .unwrap()
        .recipes = Some(Recipes {
        interactions: BTreeMap::new(),
        visibility: Some(Predicate::State {
            entity: entity.entity_id.parse().unwrap(),
            expected: "on".into(),
            comparison: Comparison::Equal,
        }),
    });
    let mut duplicate = native.nodes[&NodeId::parse("tile").unwrap()].clone();
    duplicate.id = NodeId::parse("always-visible").unwrap();
    duplicate.source.path = "always-visible".into();
    native
        .nodes
        .get_mut(&NodeId::parse("section").unwrap())
        .unwrap()
        .children
        .push(duplicate.id.clone());
    native.nodes.insert(duplicate.id.clone(), duplicate);
    layout.native = Some(native);
    request.room = None;
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let store = DeviceStore::default();
    let before = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    assert_eq!(
        before.rows.len(),
        1,
        "two occurrences share one exact device target"
    );
    let prepared = prepare(&store, &before.rows[0]);
    entity.state.as_option_mut().unwrap().revision += 1;
    let updated = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    assert_eq!(updated.rows[0].generation, before.rows[0].generation);
    assert!(
        (prepared.guard)(),
        "ordinary observations preserve a held gesture"
    );
    set_on(&mut entity, false);
    let hidden = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    assert_eq!(
        hidden.rows.len(),
        1,
        "the unconditional occurrence remains visible"
    );
    assert!(
        !hidden
            .native_nodes
            .iter()
            .any(|node| node.id.as_str() == "tile")
    );
    assert_ne!(hidden.rows[0].generation, before.rows[0].generation);
    let rendered = crate::dashboard_presentation::Board::project(
        request.dashboard.layout(),
        &request,
        &hidden,
    )
    .rows(&slint::VecModel::default());
    assert_eq!(
        rendered.len(),
        3,
        "the renderer consumes the exact visibility-filtered projection"
    );
    assert_eq!(
        rendered
            .iter()
            .filter(|row| row.kind == crate::ProLayoutKind::Device)
            .count(),
        1
    );
    assert!(
        !(prepared.guard)(),
        "the hidden occurrence must lose its captured authority"
    );
    set_on(&mut entity, true);
    let visible = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    assert_ne!(visible.rows[0].generation, before.rows[0].generation);
    assert!(
        !(prepared.guard)(),
        "reappearance cannot revive an old intention"
    );
    let fresh = prepare(&store, &visible.rows[0]);
    entity
        .state
        .as_option_mut()
        .unwrap()
        .quality
        .as_option_mut()
        .unwrap()
        .freshness = state::Freshness::Stale.into();
    let stale = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    let diagnostic = stale
        .native_nodes
        .iter()
        .find(|node| node.id.as_str() == "grid")
        .unwrap();
    assert_eq!(diagnostic.kind, Kind::Unsupported);
    assert!(
        diagnostic.entity.is_none()
            && diagnostic.children.is_empty()
            && diagnostic.recipes.is_none()
    );
    assert!(
        !(fresh.guard)(),
        "loss of fresh predicate evidence cancels a prepared command"
    );
    entity
        .state
        .as_option_mut()
        .unwrap()
        .quality
        .as_option_mut()
        .unwrap()
        .freshness = state::Freshness::Fresh.into();
    let cache = view(&request, vec![entity.clone()]);
    let offline = store.project(CachePhase::Offline, Some(&cache), &request);
    assert!(!offline.ready);
    assert!(
        offline
            .native_nodes
            .iter()
            .any(|node| node.id.as_str() == "grid" && node.kind == Kind::Unsupported)
    );
    assert!(matches!(
        store.prepare(
            offline.rows[0].token,
            offline.rows[0].generation,
            request.revision,
            DeviceIntent::Power(Power::On)
        ),
        Err(Refusal::Stale)
    ));
    let mut foreign_request = request.clone();
    let mut foreign_layout = request.dashboard.layout().unwrap().clone();
    foreign_layout.config_revision += 1;
    foreign_request.dashboard = LayoutState::Ready(Arc::new(foreign_layout));
    let foreign_cache = view(&foreign_request, vec![entity]);
    let mismatched = store.project(CachePhase::Live, Some(&foreign_cache), &request);
    assert!(mismatched.rows.is_empty() && mismatched.native_nodes.is_empty());
}
#[test]
fn room_cards_follow_server_order_and_reordering_withdraws_a_captured_gesture() {
    let mut request = request();
    let first = light(&request);
    let mut second = first.clone();
    second.entity_id = "order-entity-0002".into();
    second.current_alias = "Second synthetic control".into();
    second.state.as_option_mut().unwrap().entity = registry::EntityRef {
        reference: Some(registry::entity_ref::Reference::EntityId(
            second.entity_id.clone(),
        )),
        ..Default::default()
    }
    .into();
    let mut layout = request.dashboard.layout().unwrap().clone();
    let room = layout
        .rooms
        .iter_mut()
        .find(|room| Some(&room.id) == request.room.as_ref())
        .unwrap();
    room.entities = dashboard::RoomEntities::new(vec![
        second.entity_id.parse().unwrap(),
        first.entity_id.parse().unwrap(),
    ])
    .unwrap();
    request.dashboard = LayoutState::Ready(Arc::new(layout.clone()));
    let mut entities = vec![first.clone(), second.clone()];
    entities.sort_by(|a, b| a.entity_id.cmp(&b.entity_id));
    let cache = view(&request, entities);
    let store = DeviceStore::default();
    let before = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(before.rows[0].title.as_str(), second.current_alias);
    assert_eq!(before.rows[1].title.as_str(), first.current_alias);
    let captured = prepare(&store, &before.rows[0]);
    let room = layout
        .rooms
        .iter_mut()
        .find(|room| Some(&room.id) == request.room.as_ref())
        .unwrap();
    room.entities = dashboard::RoomEntities::new(vec![
        first.entity_id.parse().unwrap(),
        second.entity_id.parse().unwrap(),
    ])
    .unwrap();
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let after = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(after.rows[0].title.as_str(), first.current_alias);
    assert_eq!(after.rows[1].title.as_str(), second.current_alias);
    assert_ne!(before.rows[0].generation, after.rows[0].generation);
    assert!(!(captured.guard)());
}
#[test]
fn observations_preserve_the_gesture_but_permission_capability_or_config_changes_withdraw_it() {
    let request = request();
    let entity = light(&request);
    let store = DeviceStore::default();
    let first = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    let row = &first.rows[0];
    assert!(
        !row.power_known,
        "brightness alone does not establish on/off"
    );
    let prepared = prepare(&store, row);
    assert!((prepared.guard)());
    assert_eq!(prepared.target.entity().as_str(), entity.entity_id);
    let mut observed = entity.clone();
    observed.state.as_option_mut().unwrap().value = Some(state::observed_state::Value::Light(
        state::LightValue {
            on: Some(true),
            brightness_percent: Some(60.),
            ..Default::default()
        }
        .into(),
    ));
    let changed = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![observed])),
        &request,
    );
    assert_eq!(changed.rows[0].generation, row.generation);
    assert!((prepared.guard)());
    enum Withdrawal {
        Permission,
        Capability,
        Configuration,
    }
    for change in [
        Withdrawal::Permission,
        Withdrawal::Capability,
        Withdrawal::Configuration,
    ] {
        let store = DeviceStore::default();
        let initial = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        );
        let prepared = prepare(&store, &initial.rows[0]);
        let mut next = entity.clone();
        let mut configuration = request.clone();
        match change {
            Withdrawal::Permission => {
                next.action_access[0].access = state::ActionAccess::Denied.into()
            }
            Withdrawal::Capability => next.capabilities.clear(),
            Withdrawal::Configuration => {
                let mut layout = configuration.dashboard.layout().unwrap().clone();
                layout.config_revision += 1;
                configuration.dashboard = LayoutState::Ready(Arc::new(layout));
            }
        }
        store.project(
            CachePhase::Live,
            Some(&view(&configuration, vec![next])),
            &configuration,
        );
        assert!(!(prepared.guard)());
    }
}
#[test]
fn offline_reconnect_private_clear_and_reused_positions_never_revive_old_intentions() {
    let request = request();
    let entity = light(&request);
    let cache = view(&request, vec![entity.clone()]);
    let store = DeviceStore::default();
    let row = store
        .project(CachePhase::Live, Some(&cache), &request)
        .rows
        .remove(0);
    let prepared = prepare(&store, &row);
    let offline = store.project(CachePhase::Offline, Some(&cache), &request);
    assert!(!offline.ready);
    assert!(!(prepared.guard)());
    assert!(matches!(
        store.prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On)),
        Err(Refusal::Stale)
    ));
    let restored = store.project(CachePhase::Live, Some(&cache), &request);
    assert_ne!(restored.rows[0].generation, row.generation);
    assert!(!(prepared.guard)());
    let after = prepare(&store, &restored.rows[0]);
    store.clear_private();
    assert!(!(after.guard)());
    let again = store.project(CachePhase::Live, Some(&cache), &request);
    assert_ne!(again.rows[0].generation, restored.rows[0].generation);
    let mut room = request.clone();
    room.revision += 1;
    room.room = Some(room.dashboard.layout().unwrap().rooms[0].id.clone());
    store.project(CachePhase::Live, Some(&cache), &room);
    assert!(!(after.guard)());
}
#[test]
fn slider_uses_injected_bounds_and_major_unknown_or_read_only_slots_cannot_write() {
    let request = request();
    let entity = light(&request);
    let store = DeviceStore::default();
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    let prepared = store
        .prepare(
            row.token,
            row.generation,
            0,
            DeviceIntent::Brightness(64.999996),
        )
        .ok()
        .unwrap();
    let Some(actions::action_args::Arguments::SetLight(args)) =
        &prepared.request.arguments.arguments
    else {
        panic!("typed light arguments")
    };
    assert_eq!(args.brightness_percent, Some(65.));
    for invalid in [f64::NAN, f64::INFINITY, 19., 81.] {
        assert!(matches!(
            store.prepare(
                row.token,
                row.generation,
                0,
                DeviceIntent::Brightness(invalid)
            ),
            Err(Refusal::InvalidValue)
        ));
    }
    let mut future = entity.clone();
    future.capabilities[0].major = 2;
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![future])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(row.kind, ProDeviceKind::Unsupported);
    assert!(!row.can_power && !row.can_brightness);
    assert!(matches!(
        store.prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On)),
        Err(Refusal::Unsupported)
    ));
    let mut legacy = entity;
    legacy.action_access.clear();
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![legacy])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(row.access, ProControlAccess::Unknown);
    assert!(matches!(
        store.prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On)),
        Err(Refusal::ReadOnly)
    ));
}
#[test]
fn configuration_selects_cards_and_presentation_never_contains_opaque_identity() {
    let mut request = request();
    let entity = light(&request);
    let id = entity.entity_id.clone();
    let cache = view(&request, vec![entity]);
    let store = DeviceStore::default();
    let room = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(room.rows.len(), 1);
    request.room = None;
    assert!(
        store
            .project(CachePhase::Live, Some(&cache), &request)
            .rows
            .is_empty(),
        "entity is not selected as a server favorite"
    );
    request.detail = Some(id.parse().unwrap());
    let detail = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(detail.rows.len(), 1);
    assert!(
        !format!("{:?}", detail.rows[0]).contains(&id),
        "opaque identity remains in Rust"
    );
}

#[test]
fn an_unknown_layout_version_never_enables_commands_through_the_overview() {
    use home_wire_contract::home::dashboard::v1 as dashboard_wire;
    let mut request = request();
    let entity = light(&request);
    let cache = view(&request, vec![entity]);
    let store = DeviceStore::default();
    let before = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(before.rows[0].access, crate::ProControlAccess::Allowed);
    let mut layout = request.dashboard.layout().unwrap().clone();
    layout.native = Some(
        crate::dashboard_layout::NativeLayout::try_from(&dashboard_wire::NativeLayout {
            version: 99,
            views: vec!["source-view".into()],
            nodes: vec![dashboard_wire::LayoutNode {
                id: "source-view".into(),
                kind: dashboard_wire::LayoutKind::View.into(),
                source: dashboard_wire::LayoutSource {
                    file: "synthetic.yaml".into(),
                    path: "views[0]".into(),
                    variant: "future-view".into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }],
            ..Default::default()
        })
        .unwrap(),
    );
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let after = store.project(CachePhase::Live, Some(&cache), &request);
    assert!(
        after.rows.is_empty(),
        "unknown layout exposes no configured interactive card"
    );
    request.detail = Some(cache.entities().next().unwrap().0.clone());
    let after = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(after.rows[0].access, crate::ProControlAccess::Denied);
    assert!(matches!(
        store.prepare(
            after.rows[0].token,
            after.rows[0].generation,
            0,
            DeviceIntent::Power(Power::On)
        ),
        Err(Refusal::ReadOnly)
    ));
    assert!(
        store
            .prepare(
                before.rows[0].token,
                before.rows[0].generation,
                0,
                DeviceIntent::Power(Power::On)
            )
            .is_err()
    );
}

fn two_endpoints(request: &ProjectionRequest) -> state::ViewEntity {
    let mut entity = light(request);
    let mut capability = entity.capabilities[0].clone();
    capability.endpoint = "secondary".into();
    entity.capabilities.push(capability);
    let mut access = entity.action_access[0].clone();
    access.endpoint = "secondary".into();
    entity.action_access.push(access);
    entity
}

#[test]
fn an_unrelated_card_capability_change_preserves_the_other_held_command() {
    let request = request();
    let mut entity = two_endpoints(&request);
    let store = DeviceStore::default();
    let before = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    assert_eq!(before.rows.len(), 2);
    let first = prepare(&store, &before.rows[0]);
    let second = prepare(&store, &before.rows[1]);
    assert!((first.guard)() && (second.guard)());
    let Some(registry::capability_description::Constraints::Light(constraints)) =
        entity.capabilities[1].constraints.as_mut()
    else {
        panic!("light constraints");
    };
    constraints
        .brightness_percent
        .as_option_mut()
        .unwrap()
        .maximum = 90.;
    let after = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity])),
        &request,
    );
    assert_eq!(before.rows[0].generation, after.rows[0].generation);
    assert_ne!(before.rows[1].generation, after.rows[1].generation);
    assert!(
        (first.guard)(),
        "one card must not interrupt another card's unchanged gesture"
    );
    assert!(
        !(second.guard)(),
        "the changed card must withdraw its old admission proof"
    );
    assert!(matches!(
        store.prepare(
            before.rows[1].token,
            before.rows[1].generation,
            0,
            DeviceIntent::Power(Power::On)
        ),
        Err(Refusal::Stale)
    ));
}

#[test]
fn reordering_ephemeral_rows_never_retargets_a_previously_prepared_command() {
    let request = request();
    let mut entity = two_endpoints(&request);
    let store = DeviceStore::default();
    let before = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    let first = prepare(&store, &before.rows[0]);
    let second = prepare(&store, &before.rows[1]);
    entity.capabilities.reverse();
    let after = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity])),
        &request,
    );
    assert!(!(first.guard)() && !(second.guard)());
    assert_ne!(before.rows[0].generation, after.rows[0].generation);
    assert_ne!(before.rows[1].generation, after.rows[1].generation);
    assert!(
        store
            .entity(before.rows[0].token, before.rows[0].generation, 0)
            .is_none()
    );
    assert!(
        store
            .entity(before.rows[1].token, before.rows[1].generation, 0)
            .is_none()
    );
}

#[test]
fn typed_climate_and_fan_arguments_use_the_existing_target_and_admission_guard() {
    let request = request();
    let mut entity = light(&request);
    entity.capabilities[0].schema_id = "core.climate.v1".into();
    entity.action_access[0].schema_id = "core.climate.v1".into();
    entity.capabilities[0].constraints =
        Some(registry::capability_description::Constraints::Climate(
            registry::ClimateConstraints {
                setpoint_celsius: registry::NumberBounds {
                    minimum: 5.,
                    maximum: 30.,
                    step: Some(0.5),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            }
            .into(),
        ));
    let store = DeviceStore::default();
    let projection = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    let row = &projection.rows[0];
    let prepared = store
        .prepare(
            row.token,
            row.generation,
            0,
            DeviceIntent::Climate(climate::ClimateIntent::Setpoint(
                number::NumericInput::Text("20,5".into()),
            )),
        )
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::SetClimate(value)) if value.setpoint_celsius==Some(20.5))
    );
    assert!((prepared.guard)());
    entity.capabilities[0].schema_id = "core.fan.v1".into();
    entity.action_access[0].schema_id = "core.fan.v1".into();
    entity.capabilities[0].constraints = Some(registry::capability_description::Constraints::Fan(
        registry::FanConstraints {
            speed_options: vec!["Low".into()],
            ..Default::default()
        }
        .into(),
    ));
    let projection = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity])),
        &request,
    );
    assert!(!(prepared.guard)());
    let row = &projection.rows[0];
    let prepared = store
        .prepare(
            row.token,
            row.generation,
            0,
            DeviceIntent::FanSpeed(state::FanSpeed::Low),
        )
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::SetFan(value)) if value.speed.as_deref()==Some("Low"))
    );
}

fn with_helper(
    request: &ProjectionRequest,
    helper_revision: u64,
    observation_revision: u64,
) -> state::ViewEntity {
    use home_wire_contract::home::helpers::v1 as helpers;
    let mut entity = light(request);
    entity.capabilities.push(registry::CapabilityDescription {
        schema_id: "core.helper.v1".into(),
        major: 1,
        endpoint: "helper-value".into(),
        constraints: Some(registry::capability_description::Constraints::Helper(
            helpers::HelperConstraints {
                kind: helpers::HelperKind::Boolean.into(),
                operations: vec![helpers::HelperOperation::Set.into()],
                ..Default::default()
            }
            .into(),
        )),
        ..Default::default()
    });
    entity.action_access.push(state::CapabilityActionAccess {
        schema_id: "core.helper.v1".into(),
        endpoint: "helper-value".into(),
        access: state::ActionAccess::Allowed.into(),
        ..Default::default()
    });
    let observed = entity.state.as_option_mut().unwrap();
    observed.revision = observation_revision;
    observed.value = Some(state::observed_state::Value::Helper(
        helpers::HelperValue {
            helper_revision,
            value: Some(helpers::helper_value::Value::Boolean(false)),
            ..Default::default()
        }
        .into(),
    ));
    entity
}
#[test]
fn sql_helper_revision_is_independent_of_observation_revision_and_other_card_generations() {
    let request = request();
    let store = DeviceStore::default();
    let first = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![with_helper(&request, 7, 999)])),
        &request,
    );
    let light = prepare(&store, &first.rows[0]);
    let helper_row = &first.rows[1];
    let helper = store
        .prepare(
            helper_row.token,
            helper_row.generation,
            0,
            DeviceIntent::Helper(helper::HelperIntent::Boolean(true)),
        )
        .ok()
        .unwrap();
    assert!(
        helper.request.expected_revision.is_none(),
        "the observation CAS must not be substituted for the SQL CAS"
    );
    let Some(actions::action_args::Arguments::Helper(arguments)) =
        helper.request.arguments.arguments.as_ref()
    else {
        panic!("typed helper arguments");
    };
    assert_eq!(arguments.expected_helper_revision, Some(7));
    assert_eq!(arguments.value.helper_revision, 0);
    let refreshed = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![with_helper(&request, 7, 1000)])),
        &request,
    );
    assert_eq!(helper_row.generation, refreshed.rows[1].generation);
    assert!((helper.guard)());
    let changed = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![with_helper(&request, 8, 1001)])),
        &request,
    );
    assert_eq!(first.rows[0].generation, changed.rows[0].generation);
    assert!((light.guard)());
    assert_ne!(helper_row.generation, changed.rows[1].generation);
    assert!(!(helper.guard)());
}
#[test]
fn missing_helper_state_never_invents_a_zero_sql_precondition() {
    let request = request();
    let store = DeviceStore::default();
    let entity = with_helper(&request, 0, 100);
    let before = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    let row = &before.rows[1];
    let prepared = store
        .prepare(
            row.token,
            row.generation,
            0,
            DeviceIntent::Helper(helper::HelperIntent::Boolean(false)),
        )
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::Helper(arguments)) if arguments.expected_helper_revision==Some(0))
    );
    let mut missing = entity;
    missing.state.as_option_mut().unwrap().value = None;
    let after = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![missing])),
        &request,
    );
    let row = &after.rows[1];
    assert!(!(prepared.guard)());
    assert!(matches!(
        store.prepare(
            row.token,
            row.generation,
            0,
            DeviceIntent::Helper(helper::HelperIntent::Boolean(true))
        ),
        Err(Refusal::Unavailable)
    ));
}

pub(super) fn climate_entity(request: &ProjectionRequest) -> state::ViewEntity {
    let mut entity = light(request);
    entity.capabilities[0].schema_id = "core.climate.v1".into();
    entity.action_access[0].schema_id = "core.climate.v1".into();
    entity.capabilities[0].constraints =
        Some(registry::capability_description::Constraints::Climate(
            registry::ClimateConstraints {
                setpoint_celsius: registry::NumberBounds {
                    minimum: -5.,
                    maximum: 30.,
                    step: Some(0.5),
                    ..Default::default()
                }
                .into(),
                observed_setpoint_celsius: registry::ClimateTemperatureObservation::default()
                    .into(),
                mode_options: vec![
                    registry::ClimateMode::Heat.into(),
                    registry::ClimateMode::Cool.into(),
                ],
                observed_mode_options: vec![registry::ClimateMode::Cool.into()],
                activity_options: vec![registry::ClimateActivity::Idle.into()],
                observes_fan_activity: true,
                ..Default::default()
            }
            .into(),
        ));
    entity.state.as_option_mut().unwrap().value = Some(state::observed_state::Value::Climate(
        state::ClimateValue {
            setpoint: Some(state::climate_value::Setpoint::SetpointCelsius(20.)),
            mode: Some(registry::ClimateMode::Cool.into()),
            activity: Some(registry::ClimateActivity::Idle.into()),
            fan_active: Some(false),
            ..Default::default()
        }
        .into(),
    ));
    entity
}
fn edit_event(event: crate::ProValueEvent, lower: &str, upper: &str) -> crate::ProValueIntent {
    crate::ProValueIntent {
        event,
        lower: lower.into(),
        upper: upper.into(),
        ..Default::default()
    }
}
#[test]
fn numeric_drafts_survive_refresh_and_invalid_text_never_becomes_an_intention() {
    let request = request();
    let entity = climate_entity(&request);
    let store = DeviceStore::default();
    let capture = view(&request, vec![entity]);
    let row = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert_eq!(row.kind, ProDeviceKind::Climate);
    assert_eq!(row.values[0].lower, "20");
    assert_eq!(row.choices.len(), 2);
    assert!(matches!(
        store.edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Draft, "-", "")
        ),
        Err(Refusal::InvalidValue)
    ));
    let refreshed = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert_eq!(refreshed.values[0].lower, "-");
    assert!(!refreshed.values[0].error.is_empty());
    assert!(refreshed.readings.contains("20 °C"));
    let intent = store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Submit, "-0,5", ""),
        )
        .unwrap()
        .unwrap();
    let prepared = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::SetClimate(value)) if value.setpoint_celsius==Some(-0.5))
    );
    let refreshed = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert_eq!(refreshed.values[0].lower, "-0,5");
    assert!(refreshed.values[0].error.is_empty());
    assert!(
        refreshed.readings.contains("20 °C"),
        "intent must not rewrite observed state"
    );
}
#[test]
fn numeric_pointer_gesture_survives_projection_but_withdrawal_cancels_its_release() {
    let request = request();
    let mut entity = climate_entity(&request);
    let store = DeviceStore::default();
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert!(
        store
            .edit(
                row.token,
                row.generation,
                0,
                0,
                edit_event(crate::ProValueEvent::Commit, "", "")
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
                edit_event(crate::ProValueEvent::Start, "", "")
            )
            .unwrap()
            .is_none()
    );
    store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    let position = crate::ProValueIntent {
        position: 0.,
        ..edit_event(crate::ProValueEvent::Position, "", "")
    };
    assert!(
        store
            .edit(row.token, row.generation, 0, 0, position)
            .unwrap()
            .is_none()
    );
    let intent = store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Commit, "", ""),
        )
        .unwrap()
        .unwrap();
    let prepared = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::SetClimate(value)) if value.setpoint_celsius==Some(-5.))
    );
    store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Start, "", ""),
        )
        .unwrap();
    entity.action_access[0].access = state::ActionAccess::Denied.into();
    let current = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        )
        .rows
        .remove(0);
    assert_ne!(row.generation, current.generation);
    assert!(matches!(
        store.edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Commit, "", "")
        ),
        Err(Refusal::Stale)
    ));
    assert!(!(prepared.guard)());
}
#[test]
fn choice_tokens_cannot_retarget_modes_and_absent_observations_are_removed() {
    let request = request();
    let mut entity = climate_entity(&request);
    let store = DeviceStore::default();
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert!(row.readings.contains("Climatisation"));
    assert!(row.readings.contains("Au repos"));
    assert!(row.readings.contains("Inactive"));
    let intent = store.choose(row.token, row.generation, 0, 0).unwrap();
    let prepared = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::SetClimate(value)) if value.mode.and_then(|mode|mode.as_known())==Some(registry::ClimateMode::Heat) && value.fan_target.is_none())
    );
    assert!(store.choose(row.token, row.generation, 0, -1).is_err());
    assert!(store.choose(row.token, row.generation, 0, 2).is_err());
    entity.state.as_option_mut().unwrap().value = Some(state::observed_state::Value::Climate(
        state::ClimateValue::default().into(),
    ));
    let next = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(row.generation, next.generation);
    assert!(!next.readings.contains("Climatisation"));
    assert!(!next.readings.contains("Au repos"));
    assert!(!next.readings.contains("Inactive"));
    assert_eq!(next.values[0].lower, "");
    if let Some(registry::capability_description::Constraints::Climate(value)) =
        entity.capabilities[0].constraints.as_mut()
    {
        value.mode_options.reverse();
    }
    let next = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        )
        .rows
        .remove(0);
    assert_ne!(next.generation, row.generation);
    assert!(store.choose(row.token, row.generation, 0, 0).is_err());
    assert!(!(prepared.guard)());
}

pub(super) fn helper_entity(
    request: &ProjectionRequest,
    kind: home_wire_contract::home::helpers::v1::HelperKind,
) -> state::ViewEntity {
    use home_wire_contract::home::helpers::v1::{
        self as h, HelperOperation as O, helper_value::Value as V,
    };
    let mut entity = with_helper(request, 7, 500);
    entity.capabilities.remove(0);
    entity.action_access.remove(0);
    let mut constraints = h::HelperConstraints {
        kind: kind.into(),
        operations: vec![O::Set.into()],
        ..Default::default()
    };
    let value = match kind {
        h::HelperKind::Boolean => {
            constraints.operations.push(O::Toggle.into());
            Some(V::Boolean(false))
        }
        h::HelperKind::Number => {
            constraints.number_minimum = Some(-5.);
            constraints.number_maximum = Some(5.);
            constraints.number_step = Some(0.5);
            Some(V::Number(0.))
        }
        h::HelperKind::Counter => {
            constraints.counter_minimum = Some(0);
            constraints.counter_maximum = Some(i64::MAX);
            constraints.counter_step = Some(2);
            constraints
                .operations
                .extend([O::Increment, O::Decrement, O::Reset].map(buffa::EnumValue::Known));
            Some(V::Counter(0))
        }
        h::HelperKind::Text => {
            constraints.text_minimum_length = Some(0);
            constraints.text_maximum_length = Some(5);
            Some(V::Text(String::new()))
        }
        h::HelperKind::Date => Some(V::Date(
            h::CivilDate {
                year: 2028,
                month: 2,
                day: 29,
                ..Default::default()
            }
            .into(),
        )),
        h::HelperKind::Time => Some(V::Time(h::CivilTime::default().into())),
        h::HelperKind::Datetime => Some(V::Datetime(
            h::CivilDateTime {
                date: h::CivilDate {
                    year: 2028,
                    month: 2,
                    day: 29,
                    ..Default::default()
                }
                .into(),
                time: h::CivilTime::default().into(),
                ..Default::default()
            }
            .into(),
        )),
        h::HelperKind::Select => {
            constraints.select_options = vec!["custom A".into(), "custom B".into()];
            constraints
                .operations
                .extend([O::Next, O::Previous].map(buffa::EnumValue::Known));
            Some(V::SelectedOption("custom A".into()))
        }
        h::HelperKind::ButtonPress => {
            constraints.operations = vec![O::Press.into()];
            None
        }
        h::HelperKind::Unspecified => None,
    };
    entity.capabilities[0].constraints = Some(
        registry::capability_description::Constraints::Helper(constraints.into()),
    );
    let observed = entity.state.as_option_mut().unwrap();
    observed.value = Some(state::observed_state::Value::Helper(
        h::HelperValue {
            helper_revision: 7,
            value,
            ..Default::default()
        }
        .into(),
    ));
    entity
}
#[test]
fn helper_editors_preserve_exact_values_and_seal_sql_revision_at_command_admission() {
    use home_wire_contract::home::helpers::v1::{HelperKind as K, helper_value::Value as V};
    let request = request();
    for (kind, input, expected) in [
        (K::Number, "-0,5", V::Number(-0.5)),
        (K::Counter, "9223372036854775806", V::Counter(i64::MAX - 1)),
        (K::Text, "", V::Text(String::new())),
        (
            K::Date,
            "2028-02-29",
            V::Date(
                home_wire_contract::home::helpers::v1::CivilDate {
                    year: 2028,
                    month: 2,
                    day: 29,
                    ..Default::default()
                }
                .into(),
            ),
        ),
        (
            K::Time,
            "00:00",
            V::Time(home_wire_contract::home::helpers::v1::CivilTime::default().into()),
        ),
        (
            K::Datetime,
            "2028-02-29T00:00",
            V::Datetime(
                home_wire_contract::home::helpers::v1::CivilDateTime {
                    date: home_wire_contract::home::helpers::v1::CivilDate {
                        year: 2028,
                        month: 2,
                        day: 29,
                        ..Default::default()
                    }
                    .into(),
                    time: home_wire_contract::home::helpers::v1::CivilTime::default().into(),
                    ..Default::default()
                }
                .into(),
            ),
        ),
    ] {
        let store = DeviceStore::default();
        let row = store
            .project(
                CachePhase::Live,
                Some(&view(&request, vec![helper_entity(&request, kind)])),
                &request,
            )
            .rows
            .remove(0);
        assert_eq!(row.kind, ProDeviceKind::Helper);
        assert!(!row.command_unavailable);
        assert_eq!(row.values.len(), 1);
        let intent = store
            .edit(
                row.token,
                row.generation,
                0,
                0,
                edit_event(crate::ProValueEvent::Submit, input, ""),
            )
            .unwrap()
            .unwrap();
        let prepared = store
            .prepare(row.token, row.generation, 0, intent)
            .ok()
            .unwrap();
        let Some(actions::action_args::Arguments::Helper(action)) =
            prepared.request.arguments.arguments.as_ref()
        else {
            panic!("helper command");
        };
        assert_eq!(action.expected_helper_revision, Some(7));
        assert_eq!(action.value.value, Some(expected));
        assert!(prepared.request.expected_revision.is_none());
    }
}
#[test]
fn invalid_helper_text_remains_editable_but_a_sql_revision_change_withdraws_it() {
    use home_wire_contract::home::helpers::v1::HelperKind as K;
    let request = request();
    for (kind, text) in [
        (K::Number, "0.1"),
        (K::Counter, "9223372036854775808"),
        (K::Text, "abcdef"),
        (K::Date, "2027-02-29"),
        (K::Time, "23:59:60"),
        (K::Datetime, "2028-02-29T00:00Z"),
    ] {
        let store = DeviceStore::default();
        let mut entity = helper_entity(&request, kind);
        let row = store
            .project(
                CachePhase::Live,
                Some(&view(&request, vec![entity.clone()])),
                &request,
            )
            .rows
            .remove(0);
        assert!(matches!(
            store.edit(
                row.token,
                row.generation,
                0,
                0,
                edit_event(crate::ProValueEvent::Draft, text, "")
            ),
            Err(Refusal::InvalidValue)
        ));
        let next = store
            .project(
                CachePhase::Live,
                Some(&view(&request, vec![entity.clone()])),
                &request,
            )
            .rows
            .remove(0);
        assert_eq!(next.values[0].lower, text);
        assert!(!next.values[0].error.is_empty());
        if let Some(state::observed_state::Value::Helper(value)) =
            entity.state.as_option_mut().unwrap().value.as_mut()
        {
            value.helper_revision += 1;
        }
        let next = store
            .project(
                CachePhase::Live,
                Some(&view(&request, vec![entity])),
                &request,
            )
            .rows
            .remove(0);
        assert_ne!(row.generation, next.generation);
        assert!(matches!(
            store.edit(
                row.token,
                row.generation,
                0,
                0,
                edit_event(crate::ProValueEvent::Submit, text, "")
            ),
            Err(Refusal::Stale)
        ));
    }
}
#[test]
fn helper_boolean_and_button_choices_only_offer_the_advertised_operations() {
    use home_wire_contract::home::helpers::v1::{
        HelperKind as K, HelperOperation as O, helper_value::Value as V,
    };
    let request = request();
    let store = DeviceStore::default();
    let entity = helper_entity(&request, K::Boolean);
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        )
        .rows
        .remove(0);
    assert!(row.values.is_empty());
    assert_eq!(row.choices.len(), 3);
    let intent = store.choose(row.token, row.generation, 0, 1).unwrap();
    let command = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(command.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::Helper(value)) if value.value.value==Some(V::Boolean(false)))
    );
    let mut entity = helper_entity(&request, K::ButtonPress);
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert!(row.values.is_empty());
    assert_eq!(row.choices.len(), 1);
    let intent = store.choose(row.token, row.generation, 0, 0).unwrap();
    let command = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(command.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::Helper(value)) if value.operation.as_known()==Some(O::Press) && value.value.as_option().is_none())
    );
    if let Some(registry::capability_description::Constraints::Helper(value)) =
        entity.capabilities[0].constraints.as_mut()
    {
        value.operations.clear();
        value.unavailable_reason =
            home_wire_contract::home::helpers::v1::HelperReason::NoAuthorizedTarget.into();
    }
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        )
        .rows
        .remove(0);
    assert!(row.command_unavailable);
    assert!(row.choices.is_empty());
    assert!(store.choose(row.token, row.generation, 0, 0).is_err());
}
#[test]
fn helper_selector_filters_with_generation_guards_and_never_optimistically_changes_observation() {
    use home_wire_contract::home::helpers::v1::{HelperKind as K, helper_value::Value as V};
    let request = request();
    let entity = helper_entity(&request, K::Select);
    let store = DeviceStore::default();
    let capture = view(&request, vec![entity]);
    let row = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert_eq!(row.select_options.len(), 2);
    assert_eq!(row.values[0].kind, crate::ProValueKind::Select);
    store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::SelectOpen, "", ""),
        )
        .unwrap();
    let original_generation = row.select_state.generation;
    store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::SelectQuery, "B", ""),
        )
        .unwrap();
    let current = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert_eq!(current.select_options.len(), 1);
    assert_ne!(original_generation, current.select_state.generation);
    let stale = crate::ProValueIntent {
        option_token: 0,
        option_generation: original_generation,
        ..edit_event(crate::ProValueEvent::SelectActivate, "", "")
    };
    assert!(matches!(
        store.edit(row.token, row.generation, 0, 0, stale),
        Err(Refusal::Stale)
    ));
    let activate = crate::ProValueIntent {
        option_token: 0,
        option_generation: current.select_state.generation,
        ..edit_event(crate::ProValueEvent::SelectActivate, "", "")
    };
    let intent = store
        .edit(row.token, row.generation, 0, 0, activate)
        .unwrap()
        .unwrap();
    let command = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(command.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::Helper(value)) if value.value.value==Some(V::SelectedOption("custom B".into())) && value.expected_helper_revision==Some(7))
    );
    let current = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert!(current.value.contains("custom A"));
    assert!(!current.select_state.open);
    assert_eq!(
        current.select_state.selection_availability,
        crate::SelectionAvailability::Available
    );
}
#[test]
fn helper_controls_require_present_cas_and_never_coerce_a_different_observed_value_kind() {
    use home_wire_contract::home::helpers::v1::{HelperKind as K, helper_value::Value as V};
    let request = request();
    let store = DeviceStore::default();
    let mut entity = helper_entity(&request, K::Counter);
    if let Some(state::observed_state::Value::Helper(value)) =
        entity.state.as_option_mut().unwrap().value.as_mut()
    {
        value.value = Some(V::Number(3.));
    }
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(row.values[0].lower, "");
    entity.state = Default::default();
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        )
        .rows
        .remove(0);
    assert!(row.command_unavailable);
    assert!(matches!(
        store.edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Submit, "0", "")
        ),
        Err(Refusal::Unavailable)
    ));
}

fn color_light(request: &ProjectionRequest) -> state::ViewEntity {
    let mut entity = light(request);
    if let Some(registry::capability_description::Constraints::Light(value)) =
        entity.capabilities[0].constraints.as_mut()
    {
        value.color_temp_mireds = registry::NumberBounds {
            minimum: 250.,
            maximum: 454.,
            step: Some(7.),
            ..Default::default()
        }
        .into();
    }
    if let Some(state::observed_state::Value::Light(value)) =
        entity.state.as_option_mut().unwrap().value.as_mut()
    {
        value.color_temp_mireds = Some(334);
    }
    entity
}
#[test]
fn color_temperature_previews_the_converted_value_and_keeps_observation_separate() {
    let request = request();
    let store = DeviceStore::default();
    let capture = view(&request, vec![color_light(&request)]);
    let row = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert_eq!(row.values.len(), 1);
    assert_eq!(row.values[0].suffix, "K");
    assert_eq!(row.values[0].lower, "2994.012");
    assert!(row.value.contains("2994.012 K · 334 mireds"));
    assert!(
        store
            .edit(
                row.token,
                row.generation,
                0,
                0,
                edit_event(crate::ProValueEvent::Draft, "2700", "")
            )
            .unwrap()
            .is_none()
    );
    let updated = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert_eq!(updated.values[0].lower, "2700");
    assert!(
        updated.values[0]
            .description
            .contains("2710.027 K · 369 mireds")
    );
    let intent = store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Submit, "2700", ""),
        )
        .unwrap()
        .unwrap();
    let prepared = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::SetLight(value)) if value.color_temp_mireds==Some(369) && value.on.is_none() && value.brightness_percent.is_none() && value.transition_seconds.is_none())
    );
    let updated = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    assert!(
        updated.value.contains("334 mireds"),
        "draft/command must not rewrite observed color"
    );
}
#[test]
fn color_temperature_stepper_and_slider_stay_on_the_integer_server_grid_and_revoke_old_gestures() {
    let request = request();
    let store = DeviceStore::default();
    let mut entity = color_light(&request);
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    let step = crate::ProValueIntent {
        direction: crate::StepDirection::Increment,
        ..edit_event(crate::ProValueEvent::Step, "", "")
    };
    assert!(
        store
            .edit(row.token, row.generation, 0, 0, step)
            .unwrap()
            .is_none()
    );
    let updated = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(updated.values[0].lower, "3058.104");
    let intent = store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Submit, &updated.values[0].lower, ""),
        )
        .unwrap()
        .unwrap();
    let prepared = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::SetLight(value)) if value.color_temp_mireds==Some(327))
    );
    store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Start, "", ""),
        )
        .unwrap();
    let position = crate::ProValueIntent {
        position: 1.,
        ..edit_event(crate::ProValueEvent::Position, "", "")
    };
    store
        .edit(row.token, row.generation, 0, 0, position)
        .unwrap();
    let intent = store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Commit, "", ""),
        )
        .unwrap()
        .unwrap();
    let prepared = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(prepared.request.arguments.arguments.as_ref(),Some(actions::action_args::Arguments::SetLight(value)) if value.color_temp_mireds==Some(250))
    );
    store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Start, "", ""),
        )
        .unwrap();
    if let Some(registry::capability_description::Constraints::Light(value)) =
        entity.capabilities[0].constraints.as_mut()
    {
        value.color_temp_mireds = Default::default();
    }
    let updated = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        )
        .rows
        .remove(0);
    assert!(updated.values.is_empty());
    assert!(updated.can_brightness && updated.can_power);
    assert!(matches!(
        store.edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Commit, "", "")
        ),
        Err(Refusal::Stale)
    ));
    assert!(!(prepared.guard)());
}
#[test]
fn missing_color_is_not_zero_and_invalid_positions_cannot_commit_an_earlier_value() {
    let request = request();
    let store = DeviceStore::default();
    let mut entity = color_light(&request);
    if let Some(state::observed_state::Value::Light(value)) =
        entity.state.as_option_mut().unwrap().value.as_mut()
    {
        value.color_temp_mireds = None;
    }
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(row.values[0].lower, "");
    assert!(!row.value.contains("mireds"));
    store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Start, "", ""),
        )
        .unwrap();
    let home = crate::ProValueIntent {
        navigation: crate::NavigationStep::First,
        ..edit_event(crate::ProValueEvent::Navigate, "", "")
    };
    store.edit(row.token, row.generation, 0, 0, home).unwrap();
    let updated = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(updated.values[0].lower, "2207.506");
    let invalid = crate::ProValueIntent {
        position: f32::NAN,
        ..edit_event(crate::ProValueEvent::Position, "", "")
    };
    assert!(matches!(
        store.edit(row.token, row.generation, 0, 0, invalid),
        Err(Refusal::InvalidValue)
    ));
    assert!(matches!(
        store.edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Commit, "", "")
        ),
        Err(Refusal::Stale)
    ));
    for text in ["0", "-1", "NaN", "4001"] {
        assert!(matches!(
            store.edit(
                row.token,
                row.generation,
                0,
                0,
                edit_event(crate::ProValueEvent::Submit, text, "")
            ),
            Err(Refusal::InvalidValue)
        ));
    }
    entity.action_access[0].access = state::ActionAccess::Denied.into();
    let updated = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        )
        .rows
        .remove(0);
    assert!(matches!(
        store.edit(
            updated.token,
            updated.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Submit, "2700", "")
        ),
        Err(Refusal::ReadOnly)
    ));
    assert!(color_temperature_text(0).is_none());
    assert!(color_temperature_text(1001).is_none());
}
