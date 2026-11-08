use super::*;
use crate::dashboard::LayoutState;
use crate::dashboard_layout::{Modules, NativeLayout, NodeId};
use crate::device_view::{
    fixture,
    tests::{light, request, view},
};
use crate::{ProControlAccess, ProIdentity, ProModuleEvent, ProPowerIntent};
use home_client_cache::CachePhase;
use std::sync::Arc;

fn fixture() -> (fixture::Scenario, wire::EntityViewEntry) {
    let mut scenario = request();
    let mut entity = light(&scenario);
    let capability = &mut entity.capabilities[0];
    let wire::capability::Trait::LightControl(light) = capability.r#trait.as_mut().unwrap() else {
        panic!()
    };
    light.rgb = true;
    light.presets = wire::ControlChoices {
        choices: vec![
            wire::ControlChoice {
                key: "mode:quiet-7".into(),
                display_name: "Quiet".into(),
                ..Default::default()
            },
            wire::ControlChoice {
                key: "mode:bright-29".into(),
                display_name: "Bright".into(),
                ..Default::default()
            },
        ],
        ..Default::default()
    }
    .into();
    light.observation.as_option_mut().unwrap().on = Some(false);
    capability.modules = vec![
        wire::ControlModule {
            role: wire::ControlModuleRole::Power.into(),
            definition: Some(wire::control_module::Definition::Power(true)),
            ..Default::default()
        },
        wire::ControlModule {
            role: wire::ControlModuleRole::Range.into(),
            definition: Some(wire::control_module::Definition::Range(Box::new(
                wire::NumberBounds {
                    minimum: 20.,
                    maximum: 80.,
                    step: Some(5.),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        },
        wire::ControlModule {
            role: wire::ControlModuleRole::Color.into(),
            definition: Some(wire::control_module::Definition::Color(
                wire::ColorDefinition {
                    rgb: true,
                    ..Default::default()
                }
                .into(),
            )),
            ..Default::default()
        },
        wire::ControlModule {
            role: wire::ControlModuleRole::Preset.into(),
            definition: Some(wire::control_module::Definition::Preset(
                light.presets.as_option().unwrap().clone().into(),
            )),
            ..Default::default()
        },
    ];
    for module in &mut capability.modules {
        module.capability = capability.name.clone();
        module.etag = capability.etag.clone();
        module.action_access = capability.action_access;
    }
    let mut layout = scenario.dashboard.layout().unwrap().clone();
    let mut native =
        NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
    for node in native.nodes.values_mut() {
        node.recipes = None;
        if node.entity.is_some() {
            node.entity = Some(model::EntityName::try_from(entity.entity.name.as_str()).unwrap());
            node.capability =
                Some(model::CapabilityName::try_from(capability.name.as_str()).unwrap());
        }
    }
    let node = native
        .nodes
        .get_mut(&NodeId::parse("tile").unwrap())
        .unwrap();
    node.control_family = Some(VisualFamily::ContinuousLevel);
    node.control_modules =
        Modules(control_modules_contract::capability_modules(capability).unwrap());
    layout.native = Some(native);
    scenario.room = None;
    scenario.dashboard = LayoutState::Ready(Arc::new(layout));
    (scenario, entity)
}
fn row(projection: &crate::device_view::DeviceProjection, index: usize) -> crate::ProControlModule {
    projection.native_devices[&NodeId::parse("tile").unwrap()].modules[index].ui(None)
}
fn intent(row: &crate::ProControlModule, event: ProModuleEvent) -> ProModuleIntent {
    ProModuleIntent {
        identity: row.state.identity.clone(),
        role: row.role,
        event,
        ..Default::default()
    }
}
#[test]
fn module_roles_exact_rgb_preset_and_rebinding_share_one_admission_path() {
    let (scenario, entity) = fixture();
    let store = DeviceStore::default();
    let capture = view(&scenario, vec![entity.clone()]);
    let projection = store.project(CachePhase::Live, Some(&capture), &scenario);
    let rgb = row(&projection, 2);
    assert!(rgb.rgb_supported);
    let mut event = intent(&rgb, ProModuleEvent::RgbCommit);
    event.rgb = crate::SrgbColorView {
        red: 0,
        green: 237,
        blue: 17,
        alpha: 255,
    };
    let command = store.module_event(0, event.clone()).unwrap().unwrap();
    let prepared = store
        .prepare(
            rgb.state.identity.token,
            rgb.state.identity.generation,
            0,
            command,
        )
        .unwrap();
    let encoded = home_wire_contract::api_contract::encode_command(&prepared.request);
    let Some(wire::execute_capability_request::Command::Light(value)) = encoded.command else {
        panic!()
    };
    assert_eq!(value.rgb.red, Some(0));
    assert_eq!(value.rgb.green, Some(237));
    assert!(value.color_temperature_mireds.is_none());
    assert!(value.preset.is_none());
    assert!((prepared.guard)());
    let preset = row(&projection, 3);
    let mut choose = intent(&preset, ProModuleEvent::Choice);
    choose.choice_token = 1;
    let command = store.module_event(0, choose.clone()).unwrap().unwrap();
    let prepared_preset = store
        .prepare(
            preset.state.identity.token,
            preset.state.identity.generation,
            0,
            command,
        )
        .unwrap();
    let encoded = home_wire_contract::api_contract::encode_command(&prepared_preset.request);
    assert!(
        matches!(encoded.command,Some(wire::execute_capability_request::Command::Light(v)) if v.preset.as_deref()==Some("mode:bright-29") && !v.rgb.is_set())
    );
    event.role = ProControlRole::Power;
    assert!(store.module_event(0, event).is_err());
    let mut changed = entity;
    changed.capabilities[0].etag = "changed-definition".into();
    let replacement = view(&scenario, vec![changed]);
    store.project(CachePhase::Live, Some(&replacement), &scenario);
    assert!(!(prepared.guard)());
    assert!(!(prepared_preset.guard)());
    assert!(store.module_event(0, choose).is_err());
}
#[test]
fn numeric_gesture_is_draft_until_commit_and_cannot_replay_after_commit_or_privacy_clear() {
    let (scenario, entity) = fixture();
    let store = DeviceStore::default();
    let capture = view(&scenario, vec![entity]);
    let projection = store.project(CachePhase::Live, Some(&capture), &scenario);
    let range = row(&projection, 1);
    let mut event = intent(&range, ProModuleEvent::Numeric);
    event.value.event = ProValueEvent::Start;
    assert!(store.module_event(0, event.clone()).unwrap().is_none());
    event.value.event = ProValueEvent::Position;
    event.value.position = 0.42;
    assert!(store.module_event(0, event.clone()).unwrap().is_none());
    event.value.event = ProValueEvent::Commit;
    let command = store.module_event(0, event.clone()).unwrap().unwrap();
    let prepared = store
        .prepare(
            range.state.identity.token,
            range.state.identity.generation,
            0,
            command,
        )
        .unwrap();
    let wire = home_wire_contract::api_contract::encode_command(&prepared.request);
    assert!(
        matches!(wire.command,Some(wire::execute_capability_request::Command::Light(v)) if v.level.target==Some(wire::set_level_command::Target::Percentage(45.)))
    );
    assert!(store.module_event(0, event.clone()).is_err());
    store.clear_private();
    assert!(!(prepared.guard)());
    assert!(store.module_event(0, event).is_err());
}
#[test]
fn module_cannot_borrow_power_access_or_capability_definition() {
    let (scenario, mut entity) = fixture();
    let source = &mut entity.capabilities[0];
    let mut binding = control_modules_contract::capability_modules(source).unwrap()[0].clone();
    binding.allowed = false;
    assert!(!current(&binding, source));
    let wire::capability::Trait::LightControl(value) = source.r#trait.as_mut().unwrap() else {
        panic!()
    };
    value.supports_power = false;
    assert!(!current(
        &control_modules_contract::capability_modules(source).unwrap()[0],
        source
    ));
    source.action_access = wire::ActionAccess::Denied.into();
    for module in &mut source.modules {
        module.action_access = source.action_access;
    }
    let store = DeviceStore::default();
    let capture = view(&scenario, vec![entity]);
    store.project(CachePhase::Live, Some(&capture), &scenario);
    let event = ProModuleIntent {
        identity: ProIdentity {
            token: 0,
            generation: 1,
        },
        role: ProControlRole::Power,
        event: ProModuleEvent::Power,
        power: ProPowerIntent::On,
        ..Default::default()
    };
    assert!(store.module_event(0, event).is_err());
    let _ = ProControlAccess::Denied;
}

#[test]
fn a_modular_layout_without_a_private_capture_never_panics_or_retains_controls() {
    let (scenario, entity) = fixture();
    let store = DeviceStore::default();
    let capture = view(&scenario, vec![entity]);
    let projected = store.project(CachePhase::Live, Some(&capture), &scenario);
    assert!(!projected.native_devices.is_empty());
    store.clear_private();
    let absent = store.project(CachePhase::Live, None, &scenario);
    assert!(absent.rows.is_empty());
    assert!(absent.native_devices.is_empty());
    assert!(!absent.ready);
}
#[test]
fn numeric_position_and_commit_without_start_are_refused() {
    let (scenario, entity) = fixture();
    let store = DeviceStore::default();
    let capture = view(&scenario, vec![entity]);
    let projection = store.project(CachePhase::Live, Some(&capture), &scenario);
    let range = row(&projection, 1);
    let mut event = intent(&range, ProModuleEvent::Numeric);
    event.value.event = ProValueEvent::Position;
    event.value.position = 0.5;
    assert!(store.module_event(0, event.clone()).is_err());
    event.value.event = ProValueEvent::Commit;
    assert!(store.module_event(0, event).is_err());
}

#[test]
fn read_only_color_inspection_and_cancel_are_local_but_rgb_commit_remains_denied() {
    let (mut scenario, mut entity) = fixture();
    let capability = &mut entity.capabilities[0];
    capability.action_access = wire::ActionAccess::Denied.into();
    for module in &mut capability.modules {
        module.action_access = capability.action_access;
    }
    let mut layout = scenario.dashboard.layout().unwrap().clone();
    layout
        .native
        .as_mut()
        .unwrap()
        .nodes
        .get_mut(&NodeId::parse("tile").unwrap())
        .unwrap()
        .control_modules =
        Modules(control_modules_contract::capability_modules(capability).unwrap());
    scenario.dashboard = LayoutState::Ready(Arc::new(layout));
    let store = DeviceStore::default();
    let capture = view(&scenario, vec![entity]);
    let projected = store.project(CachePhase::Live, Some(&capture), &scenario);
    let color = row(&projected, 2);
    assert_eq!(color.state.access, ProControlAccess::Denied);
    let mut event = intent(&color, ProModuleEvent::ColorMode);
    event.color_mode = ProColorMode::Rgb;
    assert!(store.module_event(0, event.clone()).unwrap().is_none());
    event.event = ProModuleEvent::Cancel;
    assert!(store.module_event(0, event.clone()).unwrap().is_none());
    event.event = ProModuleEvent::RgbCommit;
    event.rgb = crate::SrgbColorView {
        red: 1,
        green: 2,
        blue: 3,
        alpha: 255,
    };
    assert!(matches!(
        store.module_event(0, event),
        Err(Refusal::ReadOnly)
    ));
}

#[test]
fn stale_then_fresh_same_capture_cannot_revive_a_prepared_command_or_gesture() {
    let (scenario, mut entity) = fixture();
    let store = DeviceStore::default();
    let capture = view(&scenario, vec![entity.clone()]);
    let projection = store.project(CachePhase::Live, Some(&capture), &scenario);
    let power = row(&projection, 0);
    let range = row(&projection, 1);
    let command = store
        .module_event(0, intent(&power, ProModuleEvent::Power))
        .unwrap()
        .unwrap();
    let prepared = store
        .prepare(
            power.state.identity.token,
            power.state.identity.generation,
            0,
            command,
        )
        .unwrap();
    assert!((prepared.guard)());
    let mut gesture = intent(&range, ProModuleEvent::Numeric);
    gesture.value.event = ProValueEvent::Start;
    store.module_event(0, gesture.clone()).unwrap();
    gesture.value.event = ProValueEvent::Position;
    gesture.value.position = 0.5;
    store.module_event(0, gesture.clone()).unwrap();
    fixture::light_mut(&mut entity)
        .observation
        .as_option_mut()
        .unwrap()
        .quality
        .as_option_mut()
        .unwrap()
        .freshness = wire::Freshness::Stale.into();
    let stale_capture = view(&scenario, vec![entity.clone()]);
    assert_eq!(capture.incarnation(), stale_capture.incarnation());
    let stale = store.project(CachePhase::Live, Some(&stale_capture), &scenario);
    let stale_range = row(&stale, 1);
    assert_ne!(
        range.state.identity.generation,
        stale_range.state.identity.generation
    );
    assert!(!(prepared.guard)());
    let mut cancel = intent(&stale_range, ProModuleEvent::Numeric);
    cancel.value.event = ProValueEvent::Cancel;
    assert!(
        store.module_event(0, cancel).unwrap().is_none(),
        "cancel stays local without fresh evidence"
    );
    fixture::light_mut(&mut entity)
        .observation
        .as_option_mut()
        .unwrap()
        .quality
        .as_option_mut()
        .unwrap()
        .freshness = wire::Freshness::Current.into();
    let fresh_capture = view(&scenario, vec![entity]);
    let fresh = store.project(CachePhase::Live, Some(&fresh_capture), &scenario);
    assert_ne!(
        range.state.identity.generation,
        row(&fresh, 1).state.identity.generation
    );
    assert!(
        !(prepared.guard)(),
        "old transport authority never resurrects"
    );
    gesture.value.event = ProValueEvent::Commit;
    assert!(store.module_event(0, gesture).is_err());
}
#[test]
fn a_detail_step_or_cancel_cannot_reuse_an_earlier_pointer_commit() {
    let (scenario, entity) = fixture();
    let store = DeviceStore::default();
    let capture = view(&scenario, vec![entity]);
    let projection = store.project(CachePhase::Live, Some(&capture), &scenario);
    let range = row(&projection, 1);
    for event_after_position in [ProValueEvent::Step, ProValueEvent::Cancel] {
        let mut event = intent(&range, ProModuleEvent::Numeric);
        event.value.event = ProValueEvent::Start;
        store.module_event(0, event.clone()).unwrap();
        event.value.event = ProValueEvent::Position;
        event.value.position = 0.5;
        store.module_event(0, event.clone()).unwrap();
        event.value.event = event_after_position;
        event.value.direction = crate::StepDirection::Increment;
        assert!(store.module_event(0, event.clone()).unwrap().is_none());
        event.value.event = ProValueEvent::Commit;
        assert!(store.module_event(0, event).is_err());
    }
}

#[test]
fn observation_publication_preserves_module_models_but_rebinding_replaces_nested_models() {
    use slint::Model;
    let mut card = super::super::presentation::CardRow {
        token: 7,
        generation: 11,
        modular: true,
        modules: vec![super::Row {
            state: crate::ProModuleState {
                identity: crate::ProIdentity {
                    token: 31,
                    generation: 41,
                },
                observation: "Observed".into(),
                ..Default::default()
            },
            role: crate::ProControlRole::Color,
            choices: vec![crate::ProDeviceChoice {
                token: 3,
                label: "Reading".into(),
                ..Default::default()
            }],
            motion_operations: vec![crate::ProCoverChoice::default()],
            ..Default::default()
        }],
        ..Default::default()
    };
    let first = card.ui(None, &crate::IconGlyph::default());
    let original = first.modules.row_data(0).unwrap();
    card.modules[0].state.observation = "New observation".into();
    let refreshed = card.ui(Some(&first), &crate::IconGlyph::default());
    let current = refreshed.modules.row_data(0).unwrap();
    assert_eq!(first.modules, refreshed.modules);
    assert_eq!(original.choices, current.choices);
    assert_eq!(original.motion_operations, current.motion_operations);
    assert_eq!(current.state.observation, "New observation");

    card.modules[0].state.identity.generation += 1;
    let rebound = card.ui(Some(&refreshed), &crate::IconGlyph::default());
    let replacement = rebound.modules.row_data(0).unwrap();
    assert_ne!(current.choices, replacement.choices);
    assert_ne!(current.motion_operations, replacement.motion_operations);
    assert_ne!(current.state.identity, replacement.state.identity);

    card.modules[0].role = crate::ProControlRole::Preset;
    let rerolled = card.ui(Some(&rebound), &crate::IconGlyph::default());
    assert_ne!(
        replacement.choices,
        rerolled.modules.row_data(0).unwrap().choices
    );
    card.generation += 1;
    let new_card = card.ui(Some(&rerolled), &crate::IconGlyph::default());
    assert_ne!(rerolled.modules, new_card.modules);
}
