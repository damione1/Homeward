use super::*;
use crate::dashboard::{self, LayoutState};
pub(super) fn request() -> fixture::Scenario {
    fixture::Scenario::new()
}
pub(super) fn light(request: &ProjectionRequest) -> wire::EntityViewEntry {
    let layout = request.dashboard.layout().unwrap();
    let name = layout
        .room(request.room.as_ref().unwrap())
        .unwrap()
        .entities
        .iter()
        .next()
        .unwrap();
    fixture::entry(
        name.as_str(),
        "Synthetic controlled light",
        vec![fixture::capability(
            &format!("{}/capabilities/power", name.as_str()),
            wire::capability::Trait::LightControl(Box::new(wire::LightControlCapability {
                supports_power: true,
                brightness_percent: wire::NumberBounds {
                    minimum: 20.,
                    maximum: 80.,
                    step: Some(5.),
                    ..Default::default()
                }
                .into(),
                observation: wire::LightControlObservation {
                    brightness_percent: Some(30.),
                    quality: fixture::quality().into(),
                    ..Default::default()
                }
                .into(),
                ..Default::default()
            })),
        )],
    )
}
pub(super) fn view(
    scenario: &fixture::Scenario,
    entities: Vec<wire::EntityViewEntry>,
) -> CachedView {
    scenario.view(entities)
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
    let mut observed_power = fixture::power(
        &format!("{}/capabilities/power-state", entity.entity.name),
        Some(wire::PowerValue::Off),
    );
    observed_power.action_access = wire::ActionAccess::Denied.into();
    entity.capabilities.push(observed_power);
    let set_on = |entity: &mut wire::EntityViewEntry, on| {
        let Some(wire::capability::Trait::Power(value)) = entity.capabilities[1].r#trait.as_mut()
        else {
            panic!()
        };
        value.observation.as_option_mut().unwrap().value = Some(
            if on {
                wire::PowerValue::On
            } else {
                wire::PowerValue::Off
            }
            .into(),
        );
        fixture::light_mut(entity)
            .observation
            .as_option_mut()
            .unwrap()
            .on = Some(on);
    };
    set_on(&mut entity, true);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let mut native =
        NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
    for node in native.nodes.values_mut() {
        if node.entity.is_some() {
            node.entity = Some(EntityName::try_from(entity.entity.name.as_str()).unwrap());
            node.capability =
                Some(CapabilityName::try_from(entity.capabilities[0].name.as_str()).unwrap());
        }
    }
    native
        .nodes
        .get_mut(&NodeId::parse("grid").unwrap())
        .unwrap()
        .recipes = Some(Recipes {
        interactions: BTreeMap::new(),
        visibility: Some(Predicate::State {
            field: home_client_model::dashboard::FieldBinding {
                capability: CapabilityName::try_from(
                    format!("{}/capabilities/power-state", entity.entity.name).as_str(),
                )
                .unwrap(),
                field: home_client_model::dashboard::FieldKey::State,
            },
            expected: home_client_model::dashboard::Literal::Power(
                home_client_model::api::Power::On,
            ),
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
        "two occurrences share one cohesive light control"
    );
    let prepared = prepare(&store, &before.rows[0]);
    fixture::light_mut(&mut entity)
        .observation
        .as_option_mut()
        .unwrap()
        .observed_time = buffa_types::google::protobuf::Timestamp::from_unix_secs(11).into();
    // This fixture exercises visibility, with equivalent cohesive/sibling
    // observations. Different evidence timestamps intentionally prevent the
    // separate presentation deduplication from hiding a sibling control.
    let Some(wire::capability::Trait::Power(power)) = entity.capabilities[1].r#trait.as_mut()
    else {
        panic!()
    };
    power.observation.as_option_mut().unwrap().observed_time =
        buffa_types::google::protobuf::Timestamp::from_unix_secs(11).into();
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
        None,
    )
    .rows(&slint::VecModel::default(), &crate::IconGlyph::default());
    assert_eq!(
        rendered.len(),
        3,
        "the renderer consumes the exact visibility-filtered projection"
    );
    assert_eq!(
        rendered
            .iter()
            .filter(|row| row.kind == crate::ProLayoutKind::Tile)
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
    fixture::light_mut(&mut entity)
        .observation
        .as_option_mut()
        .unwrap()
        .quality
        .as_option_mut()
        .unwrap()
        .freshness = wire::Freshness::Stale.into();
    let Some(wire::capability::Trait::Power(power)) = entity.capabilities[1].r#trait.as_mut()
    else {
        panic!()
    };
    power
        .observation
        .get_or_insert_default()
        .quality
        .get_or_insert_default()
        .freshness = wire::Freshness::Stale.into();
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
    fixture::light_mut(&mut entity)
        .observation
        .as_option_mut()
        .unwrap()
        .quality
        .as_option_mut()
        .unwrap()
        .freshness = wire::Freshness::Current.into();
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
    foreign_layout.etag = "different-definition".into();
    foreign_request.dashboard = LayoutState::Ready(Arc::new(foreign_layout));
    let foreign_cache = fixture::capture(vec![entity]);
    let mismatched = store.project(CachePhase::Live, Some(&foreign_cache), &request);
    assert!(mismatched.rows.is_empty() && mismatched.native_nodes.is_empty());
}
#[test]
fn room_cards_follow_server_order_and_reordering_withdraws_a_captured_gesture() {
    let mut request = request();
    let first = light(&request);
    let mut second = first.clone();
    second.entity.as_option_mut().unwrap().name =
        "homes/synthetic/entities/order-entity-0002".into();
    second.entity.as_option_mut().unwrap().display_name = "Second synthetic control".into();
    second.capabilities[0].name = format!("{}/capabilities/power", second.entity.name);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let room = layout
        .rooms
        .iter_mut()
        .find(|room| Some(&room.id) == request.room.as_ref())
        .unwrap();
    room.entities = dashboard::RoomEntities::new(vec![
        EntityName::try_from(second.entity.name.as_str()).unwrap(),
        EntityName::try_from(first.entity.name.as_str()).unwrap(),
    ])
    .unwrap();
    let native = layout.native.as_mut().unwrap();
    let root = native.root(request.room.as_ref()).unwrap().clone();
    let section = native.nodes[&root].children[0].clone();
    let first_node = native.nodes[&section].children[0].clone();
    let mut second_node = native.nodes[&first_node].clone();
    second_node.id = crate::dashboard_layout::NodeId::parse("ordered-second").unwrap();
    second_node.entity = Some(EntityName::try_from(second.entity.name.as_str()).unwrap());
    second_node.capability =
        Some(CapabilityName::try_from(second.capabilities[0].name.as_str()).unwrap());
    native.nodes.get_mut(&section).unwrap().children =
        vec![second_node.id.clone(), first_node.clone()];
    native.nodes.insert(second_node.id.clone(), second_node);
    request.dashboard = LayoutState::Ready(Arc::new(layout.clone()));
    let mut entities = vec![first.clone(), second.clone()];
    entities.sort_by(|a, b| a.entity.name.cmp(&b.entity.name));
    let cache = view(&request, entities);
    let store = DeviceStore::default();
    let before = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(before.rows[0].title.as_str(), second.entity.display_name);
    assert_eq!(before.rows[1].title.as_str(), first.entity.display_name);
    let captured = prepare(&store, &before.rows[0]);
    let room = layout
        .rooms
        .iter_mut()
        .find(|room| Some(&room.id) == request.room.as_ref())
        .unwrap();
    room.entities = dashboard::RoomEntities::new(vec![
        EntityName::try_from(first.entity.name.as_str()).unwrap(),
        EntityName::try_from(second.entity.name.as_str()).unwrap(),
    ])
    .unwrap();
    layout
        .native
        .as_mut()
        .unwrap()
        .nodes
        .get_mut(&section)
        .unwrap()
        .children
        .reverse();
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let after = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(after.rows[0].title.as_str(), first.entity.display_name);
    assert_eq!(after.rows[1].title.as_str(), second.entity.display_name);
    assert_ne!(before.rows[0].generation, after.rows[0].generation);
    assert!(!(captured.guard)());
}
#[test]
fn detail_uses_the_server_entity_label_for_its_exact_capability() {
    let mut request = request();
    let mut entity = light(&request);
    request.detail = Some(EntityName::try_from(entity.entity.name.as_str()).unwrap());
    entity.entity.as_option_mut().unwrap().display_name = "Reading light".into();
    let store = DeviceStore::default();
    let named = store.project(
        CachePhase::Live,
        Some(&view(&request, vec![entity.clone()])),
        &request,
    );
    assert_eq!(named.rows[0].title.as_str(), "Reading light");
    let row = &named.rows[0];
    let prepared = store
        .prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On))
        .unwrap();
    assert_eq!(
        prepared.request.target().as_str(),
        entity.capabilities[0].name
    );
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
    assert_eq!(prepared.target.entity().as_str(), entity.entity.name);
    let mut observed = entity.clone();
    let value = fixture::light_mut(&mut observed)
        .observation
        .as_option_mut()
        .unwrap();
    value.on = Some(true);
    value.brightness_percent = Some(60.);
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
                next.capabilities[0].action_access = wire::ActionAccess::Denied.into()
            }
            Withdrawal::Capability => next.capabilities.clear(),
            Withdrawal::Configuration => {
                let mut layout = configuration.dashboard.layout().unwrap().clone();
                layout.etag = "different-definition".into();
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
    let Some(wire::execute_capability_request::Command::Light(args)) =
        home_wire_contract::api_contract::encode_command(&prepared.request).command
    else {
        panic!("typed light arguments")
    };
    assert_eq!(
        args.level.target,
        Some(wire::set_level_command::Target::Percentage(65.))
    );
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
    future.capabilities[0].r#trait = Some(wire::capability::Trait::Opaque(Box::default()));
    let mut unsupported_detail = request.clone();
    unsupported_detail.detail = Some(EntityName::try_from(future.entity.name.as_str()).unwrap());
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![future])),
            &unsupported_detail,
        )
        .rows
        .remove(0);
    assert_eq!(row.kind, ProDeviceKind::Unsupported);
    assert!(!row.can_power && !row.can_brightness);
    assert!(matches!(
        store.prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On)),
        Err(Refusal::ReadOnly)
    ));
    let mut legacy = entity;
    legacy.capabilities[0].action_access = wire::ActionAccess::Denied.into();
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![legacy])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(row.access, ProControlAccess::Denied);
    assert!(matches!(
        store.prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On)),
        Err(Refusal::ReadOnly)
    ));
}

#[test]
fn dimmer_projection_and_submission_use_integers_without_rewriting_observations() {
    let request = request();
    for (limits, expected) in [
        ((0., 100., None), (0., 100., 1., 47.)),
        ((0.5, 80., Some(2.5)), (3., 78., 5., 48.)),
    ] {
        let mut entity = light(&request);
        let control = fixture::light_mut(&mut entity);
        control.brightness_percent = wire::NumberBounds {
            minimum: limits.0,
            maximum: limits.1,
            step: limits.2,
            ..Default::default()
        }
        .into();
        control
            .observation
            .as_option_mut()
            .unwrap()
            .brightness_percent = Some(31.25);
        let store = DeviceStore::default();
        let projected = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        let row = &projected.rows[0];
        assert_eq!(row.brightness_percent, 31.25, "keep the observed quantity");
        assert_eq!(row.value.as_str(), "31 %");
        assert_eq!(
            (
                row.brightness_minimum,
                row.brightness_maximum,
                row.brightness_step
            ),
            (expected.0, expected.1, expected.2)
        );
        let prepared = store
            .prepare(
                row.token,
                row.generation,
                0,
                DeviceIntent::Brightness(47.25),
            )
            .ok()
            .unwrap();
        let Some(wire::execute_capability_request::Command::Light(args)) =
            home_wire_contract::api_contract::encode_command(&prepared.request).command
        else {
            panic!("typed light command")
        };
        assert_eq!(
            args.level.target,
            Some(wire::set_level_command::Target::Percentage(expected.3))
        );
        assert!((prepared.guard)());
    }
}

#[test]
fn fractional_only_dimmer_keeps_power_and_observation_but_cannot_send_a_level() {
    let request = request();
    for limits in [(0.1, 0.9), (0.2, 1.2)] {
        let mut entity = light(&request);
        let control = fixture::light_mut(&mut entity);
        control.brightness_percent = wire::NumberBounds {
            minimum: limits.0,
            maximum: limits.1,
            ..Default::default()
        }
        .into();
        control
            .observation
            .as_option_mut()
            .unwrap()
            .brightness_percent = Some(0.75);
        let store = DeviceStore::default();
        let projected = store.project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        );
        let row = &projected.rows[0];
        assert!(row.can_power);
        assert!(!row.can_brightness);
        assert_eq!(row.brightness_percent, 0.75);
        assert_eq!(row.value.as_str(), "1 %");
        assert!(
            store
                .prepare(row.token, row.generation, 0, DeviceIntent::Brightness(1.))
                .is_err()
        );
        assert!((prepare(&store, row).guard)());
    }
}
#[test]
fn configuration_selects_cards_and_presentation_never_contains_opaque_identity() {
    let mut request = request();
    let entity = light(&request);
    let id = entity.entity.name.clone();
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
    request.detail = Some(EntityName::try_from(id.as_str()).unwrap());
    let detail = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(detail.rows.len(), 1);
    assert!(
        !format!("{:?}", detail.rows[0]).contains(&id),
        "opaque identity remains in Rust"
    );
}

#[test]
fn an_unsupported_layout_node_never_enables_commands_through_unbound_navigation() {
    let mut request = request();
    let entity = light(&request);
    let cache = view(&request, vec![entity]);
    let store = DeviceStore::default();
    let before = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(before.rows[0].access, crate::ProControlAccess::Allowed);
    let mut layout = request.dashboard.layout().unwrap().clone();
    use crate::dashboard_layout::tests::{children, node};
    use wire::dashboard_layout_node::Content as C;
    layout.native = Some(
        crate::dashboard_layout::NativeLayout::try_from(&wire::DashboardLayout {
            views: vec!["source-view".into()],
            nodes: vec![
                node(
                    "source-view",
                    C::View(
                        wire::DashboardViewNode {
                            children: children(&["unsupported"]).into(),
                            ..Default::default()
                        }
                        .into(),
                    ),
                ),
                node(
                    "unsupported",
                    C::Unsupported(wire::DashboardUnsupportedNode::default().into()),
                ),
            ],
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

#[test]
fn an_active_sibling_node_cannot_authorize_an_unbound_detail_entity() {
    let mut request = request();
    let original = light(&request);
    let mut sibling = original.clone();
    sibling.entity.as_option_mut().unwrap().name =
        "homes/synthetic/entities/unrelated-source".into();
    sibling.capabilities[0].name = format!("{}/capabilities/power", sibling.entity.name);
    let mut layout = request.dashboard.layout().unwrap().clone();
    for node in layout.native.as_mut().unwrap().nodes.values_mut() {
        if node
            .entity
            .as_ref()
            .is_some_and(|entity| entity.as_str() == original.entity.name)
        {
            node.entity = Some(EntityName::try_from(sibling.entity.name.as_str()).unwrap());
            node.capability =
                Some(CapabilityName::try_from(sibling.capabilities[0].name.as_str()).unwrap());
        }
    }
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let cache = view(&request, vec![original.clone(), sibling]);
    let store = DeviceStore::default();
    let active = store.project(CachePhase::Live, Some(&cache), &request);
    assert!(
        active
            .rows
            .iter()
            .any(|row| row.access == ProControlAccess::Allowed)
    );
    request.detail = Some(EntityName::try_from(original.entity.name.as_str()).unwrap());
    let unbound = store.project(CachePhase::Live, Some(&cache), &request);
    assert_eq!(unbound.rows[0].access, ProControlAccess::Denied);
    assert!(matches!(
        store.prepare(
            unbound.rows[0].token,
            unbound.rows[0].generation,
            0,
            DeviceIntent::Power(Power::On)
        ),
        Err(Refusal::ReadOnly)
    ));
}

fn two_endpoints(request: &ProjectionRequest) -> wire::EntityViewEntry {
    let mut entity = light(request);
    let mut capability = entity.capabilities[0].clone();
    capability.name = format!("{}/capabilities/secondary", entity.entity.name);
    entity.capabilities.push(capability);
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
    let Some(wire::capability::Trait::LightControl(constraints)) =
        entity.capabilities[1].r#trait.as_mut()
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
    entity.capabilities[0].r#trait = Some(wire::capability::Trait::TemperatureControl(Box::new(
        wire::TemperatureControlCapability {
            setpoint_celsius: wire::NumberBounds {
                minimum: 16.,
                maximum: 28.,
                step: Some(0.5),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        },
    )));
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
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::TemperatureControl(value)) if value.setpoint==Some(wire::set_temperature_control_command::Setpoint::SetpointCelsius(20.5)))
    );
    assert!((prepared.guard)());
    entity.capabilities[0].etag = "fan-definition".into();
    entity.capabilities[0].r#trait = Some(wire::capability::Trait::FanControl(Box::new(
        wire::FanControlCapability {
            level: wire::ControlLevelDefinition {
                definition: Some(wire::control_level_definition::Definition::Steps(
                    wire::ControlChoices {
                        choices: vec!["Low".into()]
                            .into_iter()
                            .map(|key: String| wire::ControlChoice {
                                display_name: key.clone(),
                                key,
                                ..Default::default()
                            })
                            .collect(),
                        ..Default::default()
                    }
                    .into(),
                )),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        },
    )));
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
            DeviceIntent::FanSpeed(home_client_model::api::SelectorKey::try_from("Low").unwrap()),
        )
        .ok()
        .unwrap();
    assert!(
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::Fan(value)) if value.speed.as_deref()==Some("Low"))
    );
}

fn with_helper(
    request: &ProjectionRequest,
    helper_revision: u64,
    observation_revision: u64,
) -> wire::EntityViewEntry {
    let mut entity = light(request);
    entity.capabilities.push(fixture::capability(
        &format!("{}/capabilities/helper-value", entity.entity.name),
        wire::capability::Trait::Helper(Box::new(wire::HelperCapability {
            definition: wire::HelperDefinition {
                kind: wire::HelperKind::Boolean.into(),
                operations: vec![wire::HelperOperation::Set.into()],
                ..Default::default()
            }
            .into(),
            observation: wire::HelperObservation {
                value: wire::HelperValue {
                    helper_revision: i64::try_from(helper_revision).unwrap(),
                    value: Some(wire::helper_value::Value::BooleanValue(false)),
                    ..Default::default()
                }
                .into(),
                observed_time: buffa_types::google::protobuf::Timestamp::from_unix_secs(
                    i64::try_from(observation_revision).unwrap(),
                )
                .into(),
                quality: fixture::quality().into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        })),
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
        helper.request.definition_etag().is_some(),
        "the observation CAS must not be substituted for the SQL CAS"
    );
    let Some(wire::execute_capability_request::Command::Helper(arguments)) =
        home_wire_contract::api_contract::encode_command(&helper.request).command
    else {
        panic!("typed helper arguments");
    };
    assert_eq!(arguments.expected_helper_revision, Some(7));
    // HelperInputValue has no storage revision field; CAS is solely the explicit precondition.
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
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::Helper(arguments)) if arguments.expected_helper_revision==Some(0))
    );
    let mut missing = entity;
    fixture::helper_mut(&mut missing, 1)
        .observation
        .as_option_mut()
        .unwrap()
        .value = Default::default();
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

pub(super) fn climate_entity(request: &ProjectionRequest) -> wire::EntityViewEntry {
    let mut entity = light(request);
    entity.capabilities[0].r#trait = Some(wire::capability::Trait::TemperatureControl(Box::new(
        wire::TemperatureControlCapability {
            setpoint_celsius: wire::NumberBounds {
                minimum: -5.,
                maximum: 30.,
                step: Some(0.5),
                ..Default::default()
            }
            .into(),
            observed_setpoint_celsius: wire::TemperatureObservationDefinition::default().into(),
            mode_options: vec![
                wire::TemperatureMode::Heat.into(),
                wire::TemperatureMode::Cool.into(),
            ],
            observed_mode_options: vec![wire::TemperatureMode::Cool.into()],
            observed_activity_options: vec![wire::TemperatureActivity::Idle.into()],
            observes_fan_activity: true,
            observation: wire::TemperatureControlObservation {
                setpoint: Some(
                    wire::temperature_control_observation::Setpoint::SetpointCelsius(20.),
                ),
                mode: Some(wire::TemperatureMode::Cool.into()),
                activity: Some(wire::TemperatureActivity::Idle.into()),
                fan_active: Some(false),
                quality: fixture::quality().into(),
                ..Default::default()
            }
            .into(),
            ..Default::default()
        },
    )));
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
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::TemperatureControl(value)) if value.setpoint==Some(wire::set_temperature_control_command::Setpoint::SetpointCelsius(-0.5)))
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
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::TemperatureControl(value)) if value.setpoint==Some(wire::set_temperature_control_command::Setpoint::SetpointCelsius(-5.)))
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
    entity.capabilities[0].action_access = wire::ActionAccess::Denied.into();
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
    assert!(row.readings.contains("Cool"));
    assert!(row.readings.contains("Idle"));
    assert!(row.readings.contains("Inactive"));
    let intent = store.choose(row.token, row.generation, 0, 0).unwrap();
    let prepared = store
        .prepare(row.token, row.generation, 0, intent)
        .ok()
        .unwrap();
    assert!(
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::TemperatureControl(value)) if value.mode.and_then(|mode|mode.as_known())==Some(wire::TemperatureMode::Heat) && value.fan_target.is_none())
    );
    assert!(store.choose(row.token, row.generation, 0, -1).is_err());
    assert!(store.choose(row.token, row.generation, 0, 2).is_err());
    fixture::temperature_mut(&mut entity).observation = wire::TemperatureControlObservation {
        quality: fixture::quality().into(),
        ..Default::default()
    }
    .into();
    let next = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity.clone()])),
            &request,
        )
        .rows
        .remove(0);
    assert_eq!(row.generation, next.generation);
    assert!(!next.readings.contains("Cool"));
    assert!(!next.readings.contains("Idle"));
    assert!(!next.readings.contains("Inactive"));
    assert_eq!(next.values[0].lower, "");
    {
        let value = fixture::temperature_mut(&mut entity);
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

fn helper_request() -> fixture::Scenario {
    let mut request = request();
    let mut layout = request.dashboard.layout().unwrap().clone();
    for node in layout.native.as_mut().unwrap().nodes.values_mut() {
        if let Some(entity) = &node.entity {
            node.capability = Some(
                CapabilityName::try_from(
                    format!("{}/capabilities/helper-value", entity.as_str()).as_str(),
                )
                .unwrap(),
            );
        }
    }
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    request
}
pub(super) fn helper_entity(
    request: &ProjectionRequest,
    kind: home_wire_contract::home::api::v2::HelperKind,
) -> wire::EntityViewEntry {
    use home_wire_contract::home::api::v2::{
        self as h, HelperOperation as O, helper_value::Value as V,
    };
    let mut entity = with_helper(request, 7, 500);
    entity.capabilities.remove(0);
    let mut constraints = h::HelperDefinition {
        kind: kind.into(),
        operations: vec![O::Set.into()],
        ..Default::default()
    };
    let value = match kind {
        h::HelperKind::Boolean => {
            constraints.operations.push(O::Toggle.into());
            Some(V::BooleanValue(false))
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
            home_wire_contract::google::r#type::Date {
                year: 2028,
                month: 2,
                day: 29,
                ..Default::default()
            }
            .into(),
        )),
        h::HelperKind::Time => Some(V::Time(
            home_wire_contract::google::r#type::TimeOfDay::default().into(),
        )),
        h::HelperKind::Datetime => Some(V::LocalValue(
            h::LocalDateTime {
                date: home_wire_contract::google::r#type::Date {
                    year: 2028,
                    month: 2,
                    day: 29,
                    ..Default::default()
                }
                .into(),
                time: home_wire_contract::google::r#type::TimeOfDay::default().into(),
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
    let Some(wire::capability::Trait::Helper(helper)) = entity.capabilities[0].r#trait.as_mut()
    else {
        panic!()
    };
    helper.definition = constraints.into();
    helper.observation.as_option_mut().unwrap().value = h::HelperValue {
        helper_revision: 7,
        value,
        ..Default::default()
    }
    .into();
    entity
}
#[test]
fn helper_editors_preserve_exact_values_and_seal_sql_revision_at_command_admission() {
    use home_wire_contract::home::api::v2::{HelperKind as K, helper_input_value::Value as V};
    let request = helper_request();
    for (kind, input, expected) in [
        (K::Number, "-0,5", V::Number(-0.5)),
        (K::Counter, "9223372036854775806", V::Counter(i64::MAX - 1)),
        (K::Text, "", V::Text(String::new())),
        (
            K::Date,
            "2028-02-29",
            V::Date(
                home_wire_contract::google::r#type::Date {
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
            V::Time(home_wire_contract::google::r#type::TimeOfDay::default().into()),
        ),
        (
            K::Datetime,
            "2028-02-29T00:00",
            V::LocalValue(
                wire::LocalDateTime {
                    date: home_wire_contract::google::r#type::Date {
                        year: 2028,
                        month: 2,
                        day: 29,
                        ..Default::default()
                    }
                    .into(),
                    time: home_wire_contract::google::r#type::TimeOfDay::default().into(),
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
        let Some(wire::execute_capability_request::Command::Helper(action)) =
            home_wire_contract::api_contract::encode_command(&prepared.request).command
        else {
            panic!("helper command");
        };
        assert_eq!(action.expected_helper_revision, Some(7));
        assert_eq!(action.value.value, Some(expected));
        assert!(prepared.request.definition_etag().is_some());
    }
}
#[test]
fn invalid_helper_text_remains_editable_but_a_sql_revision_change_withdraws_it() {
    use home_wire_contract::home::api::v2::HelperKind as K;
    let request = helper_request();
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
        {
            let value = fixture::helper_mut(&mut entity, 0)
                .observation
                .as_option_mut()
                .unwrap()
                .value
                .as_option_mut()
                .unwrap();
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
    use home_wire_contract::home::api::v2::{
        HelperKind as K, HelperOperation as O, helper_input_value::Value as V,
    };
    let request = helper_request();
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
        matches!(home_wire_contract::api_contract::encode_command(&command.request).command.as_ref(),Some(wire::execute_capability_request::Command::Helper(value)) if value.value.value==Some(V::BooleanValue(false)))
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
        matches!(home_wire_contract::api_contract::encode_command(&command.request).command.as_ref(),Some(wire::execute_capability_request::Command::Helper(value)) if value.operation.as_known()==Some(O::Press) && value.value.as_option().is_none())
    );
    {
        let value = fixture::helper_mut(&mut entity, 0)
            .definition
            .as_option_mut()
            .unwrap();
        value.operations.clear();
        value.unavailable_reason =
            home_wire_contract::home::api::v2::HelperReason::NoAuthorizedTarget.into();
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
    use home_wire_contract::home::api::v2::{HelperKind as K, helper_input_value::Value as V};
    let request = helper_request();
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
        matches!(home_wire_contract::api_contract::encode_command(&command.request).command.as_ref(),Some(wire::execute_capability_request::Command::Helper(value)) if value.value.value==Some(V::SelectedOption("custom B".into())) && value.expected_helper_revision==Some(7))
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
    use home_wire_contract::home::api::v2::{HelperKind as K, helper_value::Value as V};
    let request = helper_request();
    let store = DeviceStore::default();
    let mut entity = helper_entity(&request, K::Counter);
    {
        let value = fixture::helper_mut(&mut entity, 0)
            .observation
            .as_option_mut()
            .unwrap()
            .value
            .as_option_mut()
            .unwrap();
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
    fixture::helper_mut(&mut entity, 0).observation = Default::default();
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

fn color_light(request: &ProjectionRequest) -> wire::EntityViewEntry {
    let mut entity = light(request);
    {
        let value = fixture::light_mut(&mut entity);
        value.color_temperature_mireds = wire::NumberBounds {
            minimum: 250.,
            maximum: 454.,
            step: Some(7.),
            ..Default::default()
        }
        .into();
    }
    {
        let value = fixture::light_mut(&mut entity)
            .observation
            .as_option_mut()
            .unwrap();
        value.color_temperature_mireds = Some(334);
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
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::Light(value)) if value.color_temperature_mireds==Some(369) && value.power.as_option().is_none() && value.level.as_option().is_none() && value.transition_duration.as_option().is_none())
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
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::Light(value)) if value.color_temperature_mireds==Some(327))
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
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(),Some(wire::execute_capability_request::Command::Light(value)) if value.color_temperature_mireds==Some(250))
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
    {
        let value = fixture::light_mut(&mut entity);
        value.color_temperature_mireds = Default::default();
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
    {
        let value = fixture::light_mut(&mut entity)
            .observation
            .as_option_mut()
            .unwrap();
        value.color_temperature_mireds = None;
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
    entity.capabilities[0].action_access = wire::ActionAccess::Denied.into();
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

#[test]
fn panel_setpoint_step_is_one_complete_typed_intent_and_rejects_an_old_capture() {
    let request = request();
    let entity = climate_entity(&request);
    let capture = view(&request, vec![entity]);
    let store = DeviceStore::default();
    let row = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    let intent = store
        .step(
            row.token,
            row.generation,
            request.revision,
            crate::StepDirection::Increment,
        )
        .unwrap()
        .expect("atomic step commits");
    let prepared = store
        .prepare(row.token, row.generation, request.revision, intent)
        .unwrap();
    assert!(
        matches!(home_wire_contract::api_contract::encode_command(&prepared.request).command.as_ref(), Some(wire::execute_capability_request::Command::TemperatureControl(value)) if value.setpoint == Some(wire::set_temperature_control_command::Setpoint::SetpointCelsius(20.5)))
    );
    store.project(CachePhase::Live, None, &request);
    assert!(
        store
            .step(
                row.token,
                row.generation,
                request.revision,
                crate::StepDirection::Increment
            )
            .is_err()
    );
}

// The helper-only capture has no power capability. Bind the synthetic server
// document to the actual helper before joining its capture proof.
fn joined_helper_fixture(view: &CachedView) -> dashboard::Layout {
    let mut document = dashboard::fixture_wire();
    for node in &mut document.layout.get_or_insert_default().nodes {
        if let Some(wire::dashboard_layout_node::Content::Tile(tile)) = &mut node.content {
            let target = CapabilityName::try_from(tile.capability.as_str()).unwrap();
            tile.capability = format!("{}/capabilities/helper-value", target.entity().as_str());
        }
    }
    let response = dashboard::query_fixture(document, view);
    dashboard::Layout::join(
        &response,
        &home_client_model::api::DashboardName::try_from(response.dashboard.name.as_str()).unwrap(),
        view,
    )
    .unwrap()
}

fn helper_draft_request(view: &CachedView, entity: &wire::EntityViewEntry) -> ProjectionRequest {
    let layout = joined_helper_fixture(view);
    let detail = EntityName::try_from(entity.entity.name.as_str()).unwrap();
    let room = layout
        .rooms
        .iter()
        .find(|room| room.entities.contains(&detail))
        .expect("the synthetic document explicitly binds the helper's entity")
        .id
        .clone();
    ProjectionRequest {
        detail: Some(detail),
        room: Some(room),
        dashboard: LayoutState::Ready(Arc::new(layout)),
        ..Default::default()
    }
}

fn assert_helper_draft_admitted(store: &DeviceStore, row: &CardRow, request: &ProjectionRequest) {
    assert_eq!(
        row.access,
        crate::ProControlAccess::Allowed,
        "the draft fixture must start in the helper's visible authorized view"
    );
    let state = store.0.lock().unwrap();
    let slot = active_slot(&state, row.token, row.generation, request.revision).expect(
        "current joined capture, exact access and helper SQL CAS before the tested gesture",
    );
    assert_eq!(Some(&slot.target.entity()), request.detail.as_ref());
    let wire::capability::Trait::Helper(helper) = slot.capability.r#trait.as_ref().unwrap() else {
        panic!("the admitted draft target must be the helper");
    };
    assert_eq!(helper.observation.value.helper_revision, 7);
    assert_eq!(
        helper.observation.observed_time.seconds, 500,
        "synthetic observation timestamp stays distinct from helper SQL CAS"
    );
}

fn draft_rotation_fixture(
    kind: wire::HelperKind,
) -> (wire::EntityViewEntry, CachedView, ProjectionRequest) {
    let entity = helper_entity(&request(), kind);
    let first = fixture::capture(vec![entity.clone()]);
    let request = helper_draft_request(&first, &entity);
    (entity, first, request)
}

#[tokio::test]
async fn typed_drafts_survive_pending_and_coalesced_capture_rotations_without_admission() {
    for kind in [wire::HelperKind::Text, wire::HelperKind::Number] {
        for pending in [false, true] {
            let (entity, first, mut request) = draft_rotation_fixture(kind);
            let second = fixture::capture(vec![entity.clone()]);
            let third = fixture::capture(vec![entity]);
            let store = DeviceStore::default();
            let old = store
                .project(CachePhase::Live, Some(&first), &request)
                .rows
                .remove(0);
            assert_helper_draft_admitted(&store, &old, &request);
            let text = if kind == wire::HelperKind::Text {
                "abcdef"
            } else {
                "-"
            };
            assert!(
                store
                    .edit(
                        old.token,
                        old.generation,
                        request.revision,
                        0,
                        edit_event(crate::ProValueEvent::Draft, text, "")
                    )
                    .is_err()
            );
            if pending {
                assert!(
                    store
                        .project(CachePhase::Live, Some(&second), &request)
                        .rows
                        .is_empty()
                );
                let (serial, deadline) = {
                    let state = store.0.lock().unwrap();
                    assert!(state.context.is_none());
                    let saved = state
                        .retained_drafts
                        .as_ref()
                        .expect("bounded text archive");
                    (saved.serial, saved.deadline())
                };
                assert!(
                    store
                        .edit(
                            old.token,
                            old.generation,
                            request.revision,
                            0,
                            edit_event(crate::ProValueEvent::Submit, "ok", "")
                        )
                        .is_err()
                );
                store.project(CachePhase::Live, Some(&third), &request);
                let state = store.0.lock().unwrap();
                let saved = state.retained_drafts.as_ref().unwrap();
                assert_eq!(
                    (saved.serial, saved.deadline()),
                    (serial, deadline),
                    "another unjoined capture grants no grace"
                );
            }
            let joined = if pending { &third } else { &second };
            request.dashboard = LayoutState::Ready(Arc::new(joined_helper_fixture(joined)));
            let fresh = store
                .project(CachePhase::Live, Some(joined), &request)
                .rows
                .remove(0);
            assert_ne!(fresh.generation, old.generation);
            assert_eq!(fresh.values[0].lower, text);
            assert!(!fresh.values[0].error.is_empty());
            assert!(store.0.lock().unwrap().retained_drafts.is_none());
            assert!(
                store
                    .edit(
                        old.token,
                        old.generation,
                        request.revision,
                        0,
                        edit_event(crate::ProValueEvent::Submit, text, "")
                    )
                    .is_err()
            );
            assert!(
                store
                    .edit(
                        fresh.token,
                        fresh.generation,
                        request.revision,
                        0,
                        edit_event(crate::ProValueEvent::Commit, "", "")
                    )
                    .is_err(),
                "retained text is not an armed gesture"
            );
            let valid = if kind == wire::HelperKind::Text {
                "ok"
            } else {
                "0.5"
            };
            assert!(
                store
                    .edit(
                        fresh.token,
                        fresh.generation,
                        request.revision,
                        0,
                        edit_event(crate::ProValueEvent::Draft, valid, "")
                    )
                    .unwrap()
                    .is_none()
            );
            assert!(
                store
                    .edit(
                        fresh.token,
                        fresh.generation,
                        request.revision,
                        0,
                        edit_event(crate::ProValueEvent::Submit, valid, "")
                    )
                    .unwrap()
                    .is_some()
            );
        }
    }
}

#[tokio::test]
async fn draft_archives_cannot_cross_private_clear_scope_contract_or_authority_changes() {
    for change in [
        "none",
        "private-clear",
        "view-clear",
        "offline",
        "loading",
        "revision",
        "query",
        "detail",
        "document",
        "denied",
        "etag",
        "entity-etag",
        "removed",
        "capability-removed",
        "helper-cas",
        "constraints",
        "expired",
    ] {
        let (mut entity, first, mut request) = draft_rotation_fixture(wire::HelperKind::Text);
        let store = DeviceStore::default();
        let old = store
            .project(CachePhase::Live, Some(&first), &request)
            .rows
            .remove(0);
        assert_helper_draft_admitted(&store, &old, &request);
        assert!(
            store
                .edit(
                    old.token,
                    old.generation,
                    0,
                    0,
                    edit_event(crate::ProValueEvent::Draft, "abcdef", "")
                )
                .is_err()
        );
        let pending = fixture::capture(vec![entity.clone()]);
        store.project(CachePhase::Live, Some(&pending), &request);
        assert!(store.0.lock().unwrap().retained_drafts.is_some());
        match change {
            "none" => {
                store.project(CachePhase::Live, None, &request);
            }
            "offline" => {
                store.project(CachePhase::Offline, Some(&pending), &request);
            }
            "loading" => {
                request.dashboard = LayoutState::Loading;
                store.project(CachePhase::Live, Some(&pending), &request);
            }
            "private-clear" => store.clear_private(),
            "view-clear" => store.discard_retained_drafts(),
            "revision" => request.revision += 1,
            "query" => request.query = "Synthetic".into(),
            "detail" => request.detail = None,
            "denied" => entity.capabilities[0].action_access = wire::ActionAccess::Denied.into(),
            "etag" => entity.capabilities[0].etag = "new-capability-definition".into(),
            "entity-etag" => {
                entity.entity.as_option_mut().unwrap().etag = "new-entity-definition".into();
            }
            "capability-removed" => entity.capabilities.clear(),
            "helper-cas" => {
                fixture::helper_mut(&mut entity, 0)
                    .observation
                    .as_option_mut()
                    .unwrap()
                    .value
                    .as_option_mut()
                    .unwrap()
                    .helper_revision += 1
            }
            "constraints" => {
                fixture::helper_mut(&mut entity, 0)
                    .definition
                    .as_option_mut()
                    .unwrap()
                    .text_maximum_length = Some(10)
            }
            "expired" => {
                let mut state = store.0.lock().unwrap();
                let saved = state.retained_drafts.as_ref().unwrap();
                let (serial, deadline) = (saved.serial, saved.deadline());
                assert!(!saved.permits(CachePhase::Live, Some(&pending), &request, deadline));
                assert!(draft_retention::expire(&mut state, serial, deadline, deadline).is_none());
                assert!(state.retained_drafts.is_none());
            }
            "document" | "removed" => {}
            _ => unreachable!(),
        }
        let fresh = fixture::capture(if change == "removed" {
            vec![]
        } else {
            vec![entity]
        });
        let mut layout = joined_helper_fixture(&fresh);
        if change == "document" {
            layout.etag = "new-document".into();
        }
        request.dashboard = LayoutState::Ready(Arc::new(layout));
        let projection = store.project(CachePhase::Live, Some(&fresh), &request);
        assert!(
            store.0.lock().unwrap().retained_drafts.is_none(),
            "{change}"
        );
        assert!(
            projection
                .rows
                .iter()
                .flat_map(|row| &row.values)
                .all(|value| value.lower != "abcdef"),
            "{change}"
        );
    }
}

#[tokio::test]
async fn archive_expiry_is_independent_of_ui_delivery_and_does_not_own_the_store() {
    let (entity, first, mut request) = draft_rotation_fixture(wire::HelperKind::Text);
    let deadline = chrono::Utc::now() + chrono::Duration::milliseconds(500);
    let mut layout = joined_helper_fixture(&first);
    layout.binding_expires = Some((
        deadline.timestamp(),
        deadline.timestamp_subsec_nanos() as i32,
    ));
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let store = DeviceStore::default();
    let old = store
        .project(CachePhase::Live, Some(&first), &request)
        .rows
        .remove(0);
    assert_helper_draft_admitted(&store, &old, &request);
    store
        .edit(
            old.token,
            old.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Draft, "draft", ""),
        )
        .unwrap();
    let second = fixture::capture(vec![entity]);
    store.project(CachePhase::Live, Some(&second), &request);
    assert!(store.0.lock().unwrap().retained_drafts.is_some());
    // No Slint window, render delivery, timer or subsequent project is involved.
    tokio::time::timeout(std::time::Duration::from_secs(2), async {
        loop {
            if store.0.lock().unwrap().retained_drafts.is_none() {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
    drop(store);
    // This second task is still sleeping when its owner disappears.
    let (entity, first, request) = draft_rotation_fixture(wire::HelperKind::Text);
    let store = DeviceStore::default();
    let row = store
        .project(CachePhase::Live, Some(&first), &request)
        .rows
        .remove(0);
    assert_helper_draft_admitted(&store, &row, &request);
    store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Draft, "draft", ""),
        )
        .unwrap();
    store.project(
        CachePhase::Live,
        Some(&fixture::capture(vec![entity])),
        &request,
    );
    assert!(store.0.lock().unwrap().retained_drafts.is_some());
    tokio::task::yield_now().await;
    let weak = Arc::downgrade(&store.0);
    drop(store);
    assert!(
        weak.upgrade().is_none(),
        "the sleeping expiry task holds only Weak<State>"
    );
}

#[test]
fn a_detached_draft_without_an_expiry_scheduler_fails_closed() {
    let (entity, first, request) = draft_rotation_fixture(wire::HelperKind::Text);
    let store = DeviceStore::default();
    let old = store
        .project(CachePhase::Live, Some(&first), &request)
        .rows
        .remove(0);
    assert_helper_draft_admitted(&store, &old, &request);
    store
        .edit(
            old.token,
            old.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Draft, "draft", ""),
        )
        .unwrap();
    store.project(
        CachePhase::Live,
        Some(&fixture::capture(vec![entity])),
        &request,
    );
    let state = store.0.lock().unwrap();
    assert!(state.context.is_none());
    assert!(state.retained_drafts.is_none());
    assert!(state.drafts.is_empty());
}

#[tokio::test]
async fn an_old_draft_expiry_cannot_clear_restored_text_or_a_replacement_archive() {
    let (entity, first, mut request) = draft_rotation_fixture(wire::HelperKind::Text);
    let second = fixture::capture(vec![entity.clone()]);
    let third = fixture::capture(vec![entity]);
    let store = DeviceStore::default();
    let row = store
        .project(CachePhase::Live, Some(&first), &request)
        .rows
        .remove(0);
    assert_helper_draft_admitted(&store, &row, &request);
    store
        .edit(
            row.token,
            row.generation,
            0,
            0,
            edit_event(crate::ProValueEvent::Draft, "draft", ""),
        )
        .unwrap();
    store.project(CachePhase::Live, Some(&second), &request);
    let (serial, deadline) = {
        let state = store.0.lock().unwrap();
        let saved = state.retained_drafts.as_ref().unwrap();
        (saved.serial, saved.deadline())
    };
    request.dashboard = LayoutState::Ready(Arc::new(joined_helper_fixture(&second)));
    let fresh = store
        .project(CachePhase::Live, Some(&second), &request)
        .rows
        .remove(0);
    {
        let mut state = store.0.lock().unwrap();
        assert!(draft_retention::expire(&mut state, serial, deadline, deadline).is_none());
        assert_eq!(
            state.drafts[&(CardGeneration(fresh.generation), 0)].lower,
            "draft"
        );
    }
    store.project(CachePhase::Live, Some(&third), &request);
    let mut state = store.0.lock().unwrap();
    let replacement = state.retained_drafts.as_ref().unwrap().serial;
    assert_ne!(replacement, serial);
    assert!(draft_retention::expire(&mut state, serial, deadline, deadline).is_none());
    assert_eq!(state.retained_drafts.as_ref().unwrap().serial, replacement);
}

#[tokio::test]
async fn clean_fields_and_incomplete_pointer_gestures_are_not_archived_as_text() {
    for gesture in [false, true] {
        let (entity, first, mut request) = draft_rotation_fixture(wire::HelperKind::Number);
        let store = DeviceStore::default();
        let old = store
            .project(CachePhase::Live, Some(&first), &request)
            .rows
            .remove(0);
        assert_helper_draft_admitted(&store, &old, &request);
        let observed = old.values[0].lower.clone();
        if gesture {
            store
                .edit(
                    old.token,
                    old.generation,
                    0,
                    0,
                    edit_event(crate::ProValueEvent::Start, "", ""),
                )
                .unwrap();
            let mut position = edit_event(crate::ProValueEvent::Position, "", "");
            position.position = 0.8;
            store
                .edit(old.token, old.generation, 0, 0, position)
                .unwrap();
        }
        let second = fixture::capture(vec![entity]);
        store.project(CachePhase::Live, Some(&second), &request);
        assert!(store.0.lock().unwrap().retained_drafts.is_none());
        request.dashboard = LayoutState::Ready(Arc::new(joined_helper_fixture(&second)));
        let fresh = store
            .project(CachePhase::Live, Some(&second), &request)
            .rows
            .remove(0);
        assert_eq!(fresh.values[0].lower, observed);
        assert!(
            store
                .edit(
                    fresh.token,
                    fresh.generation,
                    0,
                    0,
                    edit_event(crate::ProValueEvent::Commit, "", "")
                )
                .is_err()
        );
    }
}

#[tokio::test]
async fn renewal_preserves_only_unarmed_draft_text_with_fresh_capture_admission() {
    for pending in [false, true] {
        let credentials = crate::renewal_tests::Credentials::new().await;
        let entity = helper_entity(&request(), wire::HelperKind::Text);
        let first = credentials
            .captured(fixture::capture(vec![entity.clone()]))
            .await;
        let mut request = helper_draft_request(&first, &entity);
        let store = DeviceStore::default();
        let old = store
            .project(CachePhase::Live, Some(&first), &request)
            .rows
            .remove(0);
        assert_helper_draft_admitted(&store, &old, &request);
        assert!(
            store
                .edit(
                    old.token,
                    old.generation,
                    request.revision,
                    0,
                    edit_event(crate::ProValueEvent::Draft, "abcdef", "")
                )
                .is_err()
        );
        let source = home_client_cache::PresentationSource::from_view(&first).unwrap();
        credentials.renew().await;
        assert!(
            store
                .edit(
                    old.token,
                    old.generation,
                    request.revision,
                    0,
                    edit_event(crate::ProValueEvent::Submit, "ok", "")
                )
                .is_err()
        );
        if pending {
            assert!(
                store
                    .project_owner(CachePhase::Renewing, None, &request, Some(&source))
                    .rows
                    .is_empty()
            );
            let state = store.0.lock().unwrap();
            assert!(state.context.is_none() && state.retained_drafts.is_some());
        }
        let next = credentials.captured(fixture::capture(vec![entity])).await;
        request.dashboard = LayoutState::Ready(Arc::new(joined_helper_fixture(&next)));
        let fresh = store
            .project(CachePhase::Live, Some(&next), &request)
            .rows
            .remove(0);
        assert_ne!(old.generation, fresh.generation);
        assert_eq!(fresh.values[0].lower, "abcdef");
        assert!(!fresh.values[0].error.is_empty());
        assert!(
            store
                .edit(
                    fresh.token,
                    fresh.generation,
                    request.revision,
                    0,
                    edit_event(crate::ProValueEvent::Commit, "", "")
                )
                .is_err()
        );
        store.clear_private();
        credentials.session.close().await;
    }
}

#[test]
fn color_temperature_panel_uses_the_canonical_editor_and_exact_server_grid() {
    use i_slint_backend_testing::ElementQuery;
    use slint::{ComponentHandle, ModelRc};
    use std::{cell::RefCell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let request = request();
    let store = DeviceStore::default();
    let capture = view(&request, vec![color_light(&request)]);
    let row = store
        .project(CachePhase::Live, Some(&capture), &request)
        .rows
        .remove(0);
    let panel = super::detail::project(&row, row.ui(None, &Default::default()), None);
    assert!(panel.has_temperature);
    assert_eq!(panel.temperature_field, row.values[0]);
    let window = crate::create_window().unwrap();
    window.set_show_endpoint(false);
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_view_loaded(true);
    window.set_connection_status(crate::ConnectionStatus::Live);
    window.set_commands_ready(true);
    window.set_page(crate::ViewPage::Detail);
    window
        .window()
        .set_size(slint::LogicalSize::new(390., 844.));
    window.set_device_cards(ModelRc::from([panel.device.clone()]));
    window.set_detail_panels(ModelRc::from([panel]));
    let emitted = Rc::new(RefCell::new(Vec::new()));
    let output = emitted.clone();
    let owner = store.clone();
    window.on_device_edit(move |token, generation, field, event| {
        if let Some(intent) = owner.edit(token, generation, 0, field, event).unwrap() {
            let prepared = owner.prepare(token, generation, 0, intent).unwrap();
            output
                .borrow_mut()
                .push(home_wire_contract::api_contract::encode_command(
                    &prepared.request,
                ));
        }
    });
    window.show().unwrap();
    ElementQuery::from_root(&window)
        .match_predicate(|item| item.accessible_label().as_deref() == Some("White"))
        .find_first()
        .expect("white capability section")
        .invoke_accessible_default_action();
    let slider = ElementQuery::from_root(&window)
        .match_inherits("ProDimmerControl")
        .find_first()
        .expect("canonical editor inside the light panel")
        .query_descendants()
        .match_inherits("KitSlider")
        .find_first()
        .unwrap();
    slider.invoke_accessible_increment_action();
    let values = emitted.borrow();
    assert_eq!(values.len(), 1);
    assert!(
        matches!(values[0].command.as_ref(),Some(wire::execute_capability_request::Command::Light(value)) if value.color_temperature_mireds==Some(327))
    );
    drop(values);
    window.set_commands_ready(false);
    slider.invoke_accessible_increment_action();
    assert_eq!(
        emitted.borrow().len(),
        1,
        "revocation keeps the editor inert"
    );
    window.hide().unwrap();
}

#[test]
fn climate_panel_keeps_observed_setpoint_distinct_from_ambient_and_quality_notices() {
    let request = request();
    let entity = climate_entity(&request);
    let store = DeviceStore::default();
    let row = store
        .project(
            CachePhase::Live,
            Some(&view(&request, vec![entity])),
            &request,
        )
        .rows
        .remove(0);
    let panel = super::detail::project(&row, row.ui(None, &Default::default()), None);
    assert!(
        panel.climate.current.is_empty(),
        "the capability supplies no ambient sensor"
    );
    assert_eq!(panel.climate_field.lower, "20");
    assert_eq!(panel.climate.activity, "Idle");
    assert!(row.readings.contains("Observed setpoint"));
    let mut entity = light(&request);
    let capability = &mut entity.capabilities[0];
    assert!(super::observation::quality_notice(capability).is_empty());
    if let Some(wire::capability::Trait::LightControl(light)) = &mut capability.r#trait {
        light
            .observation
            .as_option_mut()
            .unwrap()
            .quality
            .as_option_mut()
            .unwrap()
            .freshness = wire::Freshness::Stale.into();
    } else {
        panic!("synthetic light");
    }
    assert!(super::observation::quality_notice(capability).contains("Stale"));
}

fn temporal_fixture(
    value: wire::capability::Trait,
    now: home_client_model::local_time::UtcInstant,
) -> (DeviceStore, CachedView, fixture::Scenario) {
    let mut scenario = request();
    let mut layout = scenario.dashboard.layout().unwrap().clone();
    layout.profile_zone =
        crate::environment_view::ProfileZone::Known("America/Toronto".parse().unwrap());
    layout.native = None; // Exercise the legacy card that actually displays observed_age.
    scenario.dashboard = LayoutState::Ready(Arc::new(layout));
    let name = light(&scenario).entity.name.clone();
    let entry = fixture::entry(
        &name,
        "Synthetic timed reading",
        vec![fixture::capability(
            &format!("{name}/capabilities/reading"),
            value,
        )],
    );
    let view = view(&scenario, vec![entry]);
    let store = DeviceStore::default();
    let projected = store.project_owner_at(CachePhase::Offline, Some(&view), &scenario, None, now);
    assert_eq!(projected.rows.len(), 1);
    assert_eq!(store.0.lock().unwrap().temporal.len(), 1);
    (store, view, scenario)
}
fn temporal_time(seconds: i64) -> home_client_model::local_time::UtcInstant {
    home_client_model::local_time::UtcInstant::new(seconds, 0).unwrap()
}
#[test]
fn temporal_observation_checks_are_noop_until_the_displayed_age_changes_and_do_not_consume_it() {
    let now = temporal_time(1_793_512_800);
    let cap = wire::WeatherCapability {
        observation: wire::WeatherObservation {
            observed_time: buffa_types::google::protobuf::Timestamp::from_unix_secs(
                now.seconds() - 61,
            )
            .into(),
            quality: fixture::quality().into(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let (store, view, scenario) =
        temporal_fixture(wire::capability::Trait::Weather(Box::new(cap)), now);
    let (key, context) = {
        let state = store.0.lock().unwrap();
        (state.temporal.clone(), state.context.clone())
    };
    for _ in 0..1000 {
        assert!(!store.temporal_changed(scenario.revision, temporal_time(now.seconds() + 10)));
    }
    let later = temporal_time(now.seconds() + 60);
    assert!(store.temporal_changed(scenario.revision, later));
    assert!(
        store.temporal_changed(scenario.revision, later),
        "a tick cannot consume the change"
    );
    {
        let state = store.0.lock().unwrap();
        assert!(state.temporal == key && state.context == context);
    }
    store.project_owner_at(CachePhase::Offline, Some(&view), &scenario, None, later);
    assert!(!store.temporal_changed(scenario.revision, later));
    assert!(!store.temporal_changed(scenario.revision + 1, temporal_time(later.seconds() + 60)));
    store.clear_private();
    assert!(!store.temporal_changed(scenario.revision, later));
    assert!(store.0.lock().unwrap().temporal.is_empty());
}
#[test]
fn temporal_forecast_expiry_is_detected_offline_without_a_new_observation() {
    use buffa_types::google::protobuf::Timestamp;
    let now = temporal_time(1_793_512_800);
    let forecast = wire::Forecast {
        issue_time: Timestamp::from_unix_secs(now.seconds() - 7200).into(),
        expire_time: Timestamp::from_unix_secs(now.seconds() + 10).into(),
        periods: vec![wire::ForecastPeriod {
            start_time: Timestamp::from_unix_secs(now.seconds()).into(),
            end_time: Timestamp::from_unix_secs(now.seconds() + 3600).into(),
            kind: wire::ForecastKind::Hourly.into(),
            ..Default::default()
        }],
        ..Default::default()
    };
    let cap = wire::WeatherCapability {
        observation: wire::WeatherObservation {
            value: wire::WeatherValue {
                forecast: forecast.into(),
                ..Default::default()
            }
            .into(),
            quality: fixture::quality().into(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let (store, view, scenario) =
        temporal_fixture(wire::capability::Trait::Weather(Box::new(cap)), now);
    assert!(!store.temporal_changed(scenario.revision, temporal_time(now.seconds() + 9)));
    let expired = temporal_time(now.seconds() + 10);
    assert!(store.temporal_changed(scenario.revision, expired));
    store.project_owner_at(CachePhase::Offline, Some(&view), &scenario, None, expired);
    assert!(!store.temporal_changed(scenario.revision, expired));
}
#[test]
fn temporal_solar_clock_preserves_date_and_dst_offset_without_replacing_identity() {
    let before = chrono::DateTime::parse_from_rfc3339("2026-11-01T05:59:50Z")
        .unwrap()
        .timestamp();
    let (store, view, scenario) = temporal_fixture(
        wire::capability::Trait::Solar(Box::default()),
        temporal_time(before),
    );
    let generations = store
        .0
        .lock()
        .unwrap()
        .context
        .as_ref()
        .unwrap()
        .slots
        .iter()
        .map(|s| s.generation)
        .collect::<Vec<_>>();
    assert!(!store.temporal_changed(scenario.revision, temporal_time(before + 9)));
    let after = temporal_time(before + 10);
    assert!(store.temporal_changed(scenario.revision, after));
    store.project_owner_at(CachePhase::Offline, Some(&view), &scenario, None, after);
    assert!(!store.temporal_changed(scenario.revision, after));
    assert!(store.temporal_changed(scenario.revision, temporal_time(before + 86400)));
    let first_fold = temporal_time(
        chrono::DateTime::parse_from_rfc3339("2026-11-01T05:30:00Z")
            .unwrap()
            .timestamp(),
    );
    let second_fold = temporal_time(first_fold.seconds() + 3600);
    store.project_owner_at(
        CachePhase::Offline,
        Some(&view),
        &scenario,
        None,
        first_fold,
    );
    assert!(
        store.temporal_changed(scenario.revision, second_fold),
        "identical wall clock/date still has a different DST offset"
    );
    assert!(
        store
            .0
            .lock()
            .unwrap()
            .context
            .as_ref()
            .unwrap()
            .slots
            .iter()
            .map(|s| s.generation)
            .collect::<Vec<_>>()
            == generations
    );
}
#[test]
fn temporal_checks_ignore_devices_without_clock_dependent_fields() {
    let scenario = request();
    let view = view(&scenario, vec![light(&scenario)]);
    let store = DeviceStore::default();
    let now = temporal_time(1_793_512_800);
    store.project_owner_at(CachePhase::Live, Some(&view), &scenario, None, now);
    assert!(store.0.lock().unwrap().temporal.is_empty());
    assert!(!store.temporal_changed(scenario.revision, temporal_time(now.seconds() + 86400)));
}

#[test]
fn temporal_hidden_freshness_is_not_a_reason_to_reproject_native_or_detail_presence() {
    let now = temporal_time(1_793_512_800);
    let cap = wire::PresenceCapability {
        observation: wire::PresenceObservation {
            observed_time: buffa_types::google::protobuf::Timestamp::from_unix_secs(
                now.seconds() - 61,
            )
            .into(),
            quality: fixture::quality().into(),
            ..Default::default()
        }
        .into(),
        ..Default::default()
    };
    let (store, view, mut scenario) =
        temporal_fixture(wire::capability::Trait::Presence(Box::new(cap)), now);
    let later = temporal_time(now.seconds() + 60);
    assert!(
        store.temporal_changed(scenario.revision, later),
        "legacy card displays the changing age"
    );
    let entity = view.entities().next().unwrap().0.clone();
    scenario.detail = Some(entity.clone());
    scenario.revision += 1;
    store.project_owner_at(CachePhase::Offline, Some(&view), &scenario, None, now);
    assert!(
        !store.temporal_changed(scenario.revision, later),
        "detail entity tile does not display observed_age"
    );
    assert!(
        store
            .0
            .lock()
            .unwrap()
            .temporal
            .iter()
            .all(|(_, _, age)| age.is_none())
    );
    let mut native = fixture::Scenario::new()
        .dashboard
        .layout()
        .unwrap()
        .native
        .clone()
        .unwrap();
    let capability =
        CapabilityName::try_from(format!("{}/capabilities/reading", entity.as_str()).as_str())
            .unwrap();
    for node in native
        .nodes
        .values_mut()
        .filter(|node| node.entity.is_some())
    {
        node.entity = Some(entity.clone());
        node.capability = Some(capability.clone());
        node.features.clear();
    }
    let mut layout = scenario.dashboard.layout().unwrap().clone();
    layout.native = Some(native);
    scenario.dashboard = LayoutState::Ready(Arc::new(layout));
    scenario.detail = None;
    scenario.revision += 1;
    let projected = store.project_owner_at(CachePhase::Offline, Some(&view), &scenario, None, now);
    assert!(!projected.native_devices.is_empty());
    assert!(!store.0.lock().unwrap().temporal.is_empty());
    assert!(
        store
            .0
            .lock()
            .unwrap()
            .temporal
            .iter()
            .all(|(_, _, age)| age.is_none())
    );
    assert!(
        !store.temporal_changed(scenario.revision, later),
        "native entity tiles do not display observed_age"
    );
}
