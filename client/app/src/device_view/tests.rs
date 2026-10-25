use super::*;
use crate::dashboard::{self, LayoutState};
use buffa_types::google::protobuf::Timestamp;
use home_client_cache::{Filter, SnapshotCapture};
fn request() -> ProjectionRequest {
    let layout = dashboard::fixture();
    ProjectionRequest {
        room: Some(layout.rooms[1].id.clone()),
        dashboard: LayoutState::Ready(Arc::new(layout)),
        ..Default::default()
    }
}
fn light(request: &ProjectionRequest) -> state::ViewEntity {
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
fn view(request: &ProjectionRequest, entities: Vec<state::ViewEntity>) -> CachedView {
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
fn prepare(store: &DeviceStore, row: &ProDeviceRow) -> Prepared {
    store
        .prepare(row.token, row.generation, 0, DeviceIntent::Power(Power::On))
        .ok()
        .unwrap()
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
