use super::tests::{light, request, view};
use super::*;
use crate::dashboard::{LayoutState, ProjectionRequest};
use crate::dashboard_layout::{NativeLayout, NodeId};
use home_client_model::dashboard::{CommandKind, Gesture, Intention, Interaction, Recipes, Source};

fn fixture(
    intention: Intention,
    confirmation: Option<&str>,
) -> (ProjectionRequest, state::ViewEntity, DeviceStore) {
    let mut request = request();
    let mut entity = light(&request);
    set_power(&mut entity, false);
    entity.state.as_option_mut().unwrap().quality = state::Quality {
        availability: state::Availability::Available.into(),
        freshness: state::Freshness::Fresh.into(),
        provenance: state::Provenance::Observed.into(),
        ..Default::default()
    }
    .into();
    let mut layout = request.dashboard.layout().unwrap().clone();
    let mut native =
        NativeLayout::try_from(&crate::dashboard_layout::tests::extended_forest()).unwrap();
    for node in native.nodes.values_mut() {
        if node.entity.is_some() {
            node.entity = Some(entity.entity_id.parse().unwrap());
        }
        node.recipes = None;
    }
    native
        .nodes
        .get_mut(&NodeId::parse("recipe").unwrap())
        .unwrap()
        .recipes = Some(Recipes {
        interactions: BTreeMap::from([(
            Gesture::Tap,
            Interaction {
                intention,
                confirmation: confirmation.map(str::to_owned),
                source: Source {
                    file: "synthetic.yaml".into(),
                    line: 3,
                    column: 5,
                    path: "recipe.tap_action".into(),
                    variant: "synthetic".into(),
                },
            },
        )]),
        visibility: None,
    });
    layout.native = Some(native);
    request.room = None;
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    (request, entity, DeviceStore::default())
}
fn entity_id() -> EntityId {
    light(&request()).entity_id.parse().unwrap()
}
fn set_power(entity: &mut state::ViewEntity, on: bool) {
    let Some(state::observed_state::Value::Light(value)) =
        &mut entity.state.as_option_mut().unwrap().value
    else {
        panic!()
    };
    value.on = Some(on);
}
fn project(
    store: &DeviceStore,
    request: &ProjectionRequest,
    entity: &state::ViewEntity,
) -> DeviceProjection {
    store.project(
        CachePhase::Live,
        Some(&view(request, vec![entity.clone()])),
        request,
    )
}
fn recipe(projection: &DeviceProjection) -> &RecipePresentation {
    &projection.recipes[&NodeId::parse("recipe").unwrap()]
}
fn prepare(store: &DeviceStore, projection: &DeviceProjection) -> NativePrepared {
    let row = recipe(projection);
    store
        .prepare_recipe(row.token, row.generation, 0, Gesture::Tap)
        .unwrap()
}
fn physical_on(prepared: &NativePrepared) -> bool {
    let NativeIntent::Command(command) = &prepared.intent else {
        panic!()
    };
    let Some(actions::action_args::Arguments::SetLight(value)) = command
        .request
        .arguments
        .as_option()
        .unwrap()
        .arguments
        .as_ref()
    else {
        panic!()
    };
    value.on.unwrap()
}

fn publish_focus_view(
    window: &crate::HomewardWindow,
    shared: &crate::dashboard::SharedRequest,
    store: &DeviceStore,
    entity: &state::ViewEntity,
) {
    let request = shared.lock().unwrap().clone();
    let cache = view(&request, vec![entity.clone()]);
    let projection = crate::cache_view::project_devices(
        CachePhase::Live,
        Some(&cache),
        home_client_cache::Viewport::default(),
        &request,
        store,
    );
    crate::cache_view::render(
        window,
        home_client_cache::ProjectionMailbox::<()>::new().generation(),
        projection,
    );
}
fn settle_focus(window: &crate::HomewardWindow) {
    use i_slint_backend_testing::ElementQuery;
    // Query geometry for headless layout passes, then drain the queued UI
    // restoration and the focus/geometry property-change batches.
    for _ in 0..3 {
        if let Some(card) = ElementQuery::from_root(window)
            .match_inherits("ProRecipeCard")
            .find_first()
        {
            let _ = card.absolute_position();
        }
        slint::platform::update_timers_and_animations();
        i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(1));
    }
}

#[test]
fn native_back_resolves_the_source_node_after_a_visible_sibling_changes_its_ui_token() {
    use home_client_model::dashboard::{Comparison, Predicate};
    use slint::{
        ComponentHandle,
        platform::{Key, WindowEvent},
    };
    use std::{cell::RefCell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    crate::cache_view::bind(&window);
    let (mut request, mut entity, store) = fixture(Intention::MoreInfo(entity_id()), None);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let native = layout.native.as_mut().unwrap();
    let mut sibling = native.nodes[&NodeId::parse("recipe").unwrap()]
        .recipes
        .clone()
        .unwrap();
    sibling.visibility = Some(Predicate::State {
        entity: entity_id(),
        expected: "on".into(),
        comparison: Comparison::Equal,
    });
    native
        .nodes
        .get_mut(&NodeId::parse("tile").unwrap())
        .unwrap()
        .recipes = Some(sibling);
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let shared = Arc::new(Mutex::new(request));
    let devices = store.clone();
    let navigation = crate::navigation::bind_devices(
        &window,
        shared.clone(),
        || {},
        move || Some(devices.clone()),
    );
    set_power(&mut entity, true);
    publish_focus_view(&window, &shared, &store, &entity);
    window.show().unwrap();
    settle_focus(&window);
    let first = store
        .recipe_focus(
            &NodeId::parse("recipe").unwrap(),
            shared.lock().unwrap().dashboard.layout().unwrap(),
            0,
        )
        .unwrap();
    assert_eq!(first.token, 1);
    let prepared = store
        .prepare_recipe(first.token, first.generation, 0, Gesture::Tap)
        .unwrap();
    navigation
        .more_info(
            entity_id(),
            Some(crate::dashboard_focus::Origin::prepared(&prepared)),
        )
        .unwrap();
    publish_focus_view(&window, &shared, &store, &entity);
    set_power(&mut entity, false);
    window.invoke_go_back();
    settle_focus(&window);
    assert!(
        !window.get_recipe_focus().present,
        "no source restoration before the matching cache publication"
    );
    publish_focus_view(&window, &shared, &store, &entity);
    let revision = shared.lock().unwrap().revision;
    let restored = store
        .recipe_focus(
            &NodeId::parse("recipe").unwrap(),
            shared.lock().unwrap().dashboard.layout().unwrap(),
            revision,
        )
        .unwrap();
    assert_eq!(restored.token, 0);
    assert_ne!(first.generation, restored.generation);
    settle_focus(&window);
    assert!(
        !window.get_recipe_focus().present,
        "the focus acknowledgment clears the request"
    );
    let calls = Rc::new(RefCell::new(Vec::new()));
    let output = calls.clone();
    window.on_recipe_gesture(move |token, generation, gesture| {
        output.borrow_mut().push((token, generation, gesture))
    });
    window.window().dispatch_event(WindowEvent::KeyPressed {
        text: Key::Return.into(),
    });
    window.window().dispatch_event(WindowEvent::KeyReleased {
        text: Key::Return.into(),
    });
    assert_eq!(
        &*calls.borrow(),
        &[(
            restored.token,
            restored.generation,
            crate::ProRecipeGesture::Tap
        )]
    );
    assert!(
        store
            .recipe_node(first.token, first.generation, revision)
            .is_none()
    );
    assert!(store.0.lock().unwrap().actions.is_empty());
    window.hide().unwrap();
}

#[derive(Clone, Copy, Debug)]
enum FocusInterruption {
    NewRoute,
    UserFocus,
    UserPointer,
    PrivateClear,
    NewDefinition,
    HiddenOrigin,
}

#[test]
fn pending_native_focus_cannot_outlive_navigation_user_focus_clear_or_its_source_definition() {
    use slint::{
        ComponentHandle,
        platform::{Key, WindowEvent},
    };
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    for interruption in [
        FocusInterruption::NewRoute,
        FocusInterruption::UserFocus,
        FocusInterruption::UserPointer,
        FocusInterruption::PrivateClear,
        FocusInterruption::NewDefinition,
        FocusInterruption::HiddenOrigin,
    ] {
        let window = crate::create_window().unwrap();
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        crate::cache_view::bind(&window);
        let (mut request, mut entity, store) = fixture(Intention::MoreInfo(entity_id()), None);
        if matches!(interruption, FocusInterruption::HiddenOrigin) {
            let mut layout = request.dashboard.layout().unwrap().clone();
            layout
                .native
                .as_mut()
                .unwrap()
                .nodes
                .get_mut(&NodeId::parse("recipe").unwrap())
                .unwrap()
                .recipes
                .as_mut()
                .unwrap()
                .visibility = Some(home_client_model::dashboard::Predicate::State {
                entity: entity_id(),
                expected: "on".into(),
                comparison: home_client_model::dashboard::Comparison::Equal,
            });
            request.dashboard = LayoutState::Ready(Arc::new(layout));
            set_power(&mut entity, true);
        }
        let shared = Arc::new(Mutex::new(request));
        let devices = store.clone();
        let navigation = crate::navigation::bind_devices(
            &window,
            shared.clone(),
            || {},
            move || Some(devices.clone()),
        );
        publish_focus_view(&window, &shared, &store, &entity);
        window.show().unwrap();
        settle_focus(&window);
        let row = store
            .recipe_focus(
                &NodeId::parse("recipe").unwrap(),
                shared.lock().unwrap().dashboard.layout().unwrap(),
                0,
            )
            .unwrap();
        let prepared = store
            .prepare_recipe(row.token, row.generation, 0, Gesture::Tap)
            .unwrap();
        navigation
            .more_info(
                entity_id(),
                Some(crate::dashboard_focus::Origin::prepared(&prepared)),
            )
            .unwrap();
        publish_focus_view(&window, &shared, &store, &entity);
        window.invoke_go_back();
        let gestures = Rc::new(Cell::new(0));
        let observed = gestures.clone();
        window.on_recipe_gesture(move |_, _, _| observed.set(observed.get() + 1));
        match interruption {
            FocusInterruption::NewRoute => window.invoke_show_inventory(),
            FocusInterruption::UserFocus => {
                window.invoke_take_shell_focus(crate::ProShellFocus::Search)
            }
            FocusInterruption::UserPointer => {
                i_slint_backend_testing::ElementQuery::from_root(&window)
                    .match_inherits("KitSearchField")
                    .find_first()
                    .unwrap()
                    .mock_single_click(slint::platform::PointerEventButton::Left);
            }
            FocusInterruption::PrivateClear => {
                store.clear_private();
                crate::cache_view::clear(&window);
            }
            FocusInterruption::NewDefinition => {
                let mut request = shared.lock().unwrap();
                let mut layout = request.dashboard.layout().unwrap().clone();
                layout.title = "Replacement configuration".into();
                request.dashboard = LayoutState::Ready(Arc::new(layout));
            }
            FocusInterruption::HiddenOrigin => set_power(&mut entity, false),
        }
        match interruption {
            FocusInterruption::PrivateClear => window.invoke_native_layout_published(2),
            FocusInterruption::NewRoute
            | FocusInterruption::UserFocus
            | FocusInterruption::UserPointer
            | FocusInterruption::NewDefinition
            | FocusInterruption::HiddenOrigin => {
                publish_focus_view(&window, &shared, &store, &entity)
            }
        }
        settle_focus(&window);
        assert!(!window.get_recipe_focus().present);
        window.window().dispatch_event(WindowEvent::KeyPressed {
            text: Key::Return.into(),
        });
        window.window().dispatch_event(WindowEvent::KeyReleased {
            text: Key::Return.into(),
        });
        assert_eq!(
            gestures.get(),
            0,
            "a cancelled restoration cannot focus a recipe and accept Enter: {interruption:?}"
        );
        assert!(store.0.lock().unwrap().actions.is_empty());
        window.hide().unwrap();
    }
}

#[test]
fn native_history_restores_scroll_and_keeps_the_source_header_visible() {
    use slint::ComponentHandle;
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window
        .window()
        .set_size(slint::LogicalSize::new(1100., 700.));
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    crate::cache_view::bind(&window);
    let (mut request, entity, store) = fixture(Intention::MoreInfo(entity_id()), None);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let native = layout.native.as_mut().unwrap();
    native
        .nodes
        .get_mut(&NodeId::parse("grid").unwrap())
        .unwrap()
        .kind = crate::dashboard_layout::Kind::VerticalStack;
    let prototype = native.nodes[&NodeId::parse("recipe").unwrap()].clone();
    for index in 0..4 {
        let mut filler = prototype.clone();
        filler.id = NodeId::parse(&format!("synthetic-filler-{index}")).unwrap();
        filler.source.path = filler.id.as_str().into();
        filler.title = "Read-only synthetic context\n".repeat(12);
        filler.recipes = None;
        native
            .nodes
            .get_mut(&NodeId::parse("grid").unwrap())
            .unwrap()
            .children
            .insert(index, filler.id.clone());
        native.nodes.insert(filler.id.clone(), filler);
    }
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let shared = Arc::new(Mutex::new(request));
    let devices = store.clone();
    let navigation = crate::navigation::bind_devices(
        &window,
        shared.clone(),
        || {},
        move || Some(devices.clone()),
    );
    publish_focus_view(&window, &shared, &store, &entity);
    window.show().unwrap();
    settle_focus(&window);
    let row = store
        .recipe_focus(
            &NodeId::parse("recipe").unwrap(),
            shared.lock().unwrap().dashboard.layout().unwrap(),
            0,
        )
        .unwrap();
    let prepared = store
        .prepare_recipe(row.token, row.generation, 0, Gesture::Tap)
        .unwrap();
    navigation.restore_origin(&prepared);
    settle_focus(&window);
    let saved = window.get_home_scroll();
    assert!(
        saved < -500.,
        "the source header must be revealed below the initial viewport: {saved}"
    );
    navigation
        .more_info(
            entity_id(),
            Some(crate::dashboard_focus::Origin::prepared(&prepared)),
        )
        .unwrap();
    publish_focus_view(&window, &shared, &store, &entity);
    window.invoke_restore_board_scroll(0., 0., 0.);
    assert_eq!(window.get_home_scroll(), 0.);
    window.invoke_go_back();
    publish_focus_view(&window, &shared, &store, &entity);
    settle_focus(&window);
    assert!(
        (window.get_home_scroll() - saved).abs() < 1.,
        "history restores the saved position, then reveals only when required: {} vs {saved}",
        window.get_home_scroll()
    );
    assert!(!window.get_recipe_focus().present);
    assert!(store.0.lock().unwrap().actions.is_empty());
    window.hide().unwrap();
}

#[test]
fn dismissing_a_confirmation_restores_its_exact_source_without_replaying_the_command() {
    use slint::{
        ComponentHandle,
        platform::{Key, WindowEvent},
    };
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    crate::cache_view::bind(&window);
    let (request, entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::TurnOn,
        },
        Some("Confirm synthetic action"),
    );
    let shared = Arc::new(Mutex::new(request));
    let devices = store.clone();
    let navigation = crate::navigation::bind_devices(
        &window,
        shared.clone(),
        || {},
        move || Some(devices.clone()),
    );
    let prepare = store.clone();
    let revision = shared.clone();
    let sent = Rc::new(Cell::new(0));
    let count = sent.clone();
    crate::dashboard_gestures::bind(
        &window,
        move |token, generation, gesture| {
            prepare.prepare_recipe(
                token,
                generation,
                revision.lock().unwrap().revision,
                gesture,
            )
        },
        move |_| count.set(count.get() + 1),
        move |prepared| navigation.restore_origin(prepared),
    );
    publish_focus_view(&window, &shared, &store, &entity);
    window.show().unwrap();
    settle_focus(&window);
    let row = store
        .recipe_focus(
            &NodeId::parse("recipe").unwrap(),
            shared.lock().unwrap().dashboard.layout().unwrap(),
            0,
        )
        .unwrap();
    window.invoke_recipe_gesture(row.token, row.generation, crate::ProRecipeGesture::Tap);
    settle_focus(&window);
    let first = window.get_recipe_confirmation_id();
    assert!(window.get_recipe_confirmation().open);
    window.invoke_recipe_dismiss(first, crate::CloseReason::Escape);
    settle_focus(&window);
    assert!(!window.get_recipe_confirmation().open);
    assert_eq!(sent.get(), 0);
    window.window().dispatch_event(WindowEvent::KeyPressed {
        text: Key::Return.into(),
    });
    window.window().dispatch_event(WindowEvent::KeyReleased {
        text: Key::Return.into(),
    });
    assert!(
        window.get_recipe_confirmation().open,
        "Enter reaches the exact recipe again, requiring a new confirmation"
    );
    assert!(window.get_recipe_confirmation_id() > first);
    assert_eq!(sent.get(), 0);
    window.hide().unwrap();
}

#[test]
fn popup_detail_back_and_close_restore_each_exact_native_opener() {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use slint::{
        ComponentHandle,
        platform::{Key, WindowEvent},
    };
    use std::{cell::RefCell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let destination = Destination {
        dashboard: DashboardId::parse(request().dashboard.layout().unwrap().id.as_str()).unwrap(),
        node: NodeId::parse("popup").unwrap(),
        presentation: Presentation::Popup,
    };
    let (mut request, entity, store) = fixture(Intention::Navigate(destination.clone()), None);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let native = layout.native.as_mut().unwrap();
    let mut popup_recipe = native.nodes[&NodeId::parse("recipe").unwrap()]
        .recipes
        .clone()
        .unwrap();
    popup_recipe
        .interactions
        .get_mut(&Gesture::Tap)
        .unwrap()
        .intention = Intention::MoreInfo(entity_id());
    native
        .nodes
        .get_mut(&NodeId::parse("popup-tile").unwrap())
        .unwrap()
        .recipes = Some(popup_recipe);
    request.dashboard = LayoutState::Ready(Arc::new(layout.clone()));
    let window = crate::create_window().unwrap();
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    crate::cache_view::bind(&window);
    let shared = Arc::new(Mutex::new(request));
    let devices = store.clone();
    let navigation = crate::navigation::bind_devices(
        &window,
        shared.clone(),
        || {},
        move || Some(devices.clone()),
    );
    publish_focus_view(&window, &shared, &store, &entity);
    let assert_home_selected = || {
        use slint::Model;
        let selected = window.get_navigation_state().selected;
        assert!(selected.present);
        let rows = window.get_navigation_rows();
        let row = rows.iter().find(|row| row.token == selected.token).unwrap();
        assert_eq!(row.label.as_str(), "Accueil");
        assert_eq!(window.get_page(), crate::ViewPage::Home);
    };
    assert_home_selected();
    window.show().unwrap();
    settle_focus(&window);
    let home = store
        .recipe_focus(&NodeId::parse("recipe").unwrap(), &layout, 0)
        .unwrap();
    let prepared = store
        .prepare_recipe(home.token, home.generation, 0, Gesture::Tap)
        .unwrap();
    navigation
        .navigate(
            destination,
            &layout,
            Some(crate::dashboard_focus::Origin::prepared(&prepared)),
        )
        .unwrap();
    publish_focus_view(&window, &shared, &store, &entity);
    settle_focus(&window);
    assert_home_selected();
    let revision = shared.lock().unwrap().revision;
    let popup = store
        .recipe_focus(&NodeId::parse("popup-tile").unwrap(), &layout, revision)
        .unwrap();
    let prepared = store
        .prepare_recipe(popup.token, popup.generation, revision, Gesture::Tap)
        .unwrap();
    navigation
        .more_info(
            entity_id(),
            Some(crate::dashboard_focus::Origin::prepared(&prepared)),
        )
        .unwrap();
    publish_focus_view(&window, &shared, &store, &entity);
    window.invoke_go_back();
    publish_focus_view(&window, &shared, &store, &entity);
    settle_focus(&window);
    assert_home_selected();
    let revision = shared.lock().unwrap().revision;
    let popup_restored = store
        .recipe_focus(&NodeId::parse("popup-tile").unwrap(), &layout, revision)
        .unwrap();
    assert_ne!(popup.generation, popup_restored.generation);
    let calls = Rc::new(RefCell::new(Vec::new()));
    let output = calls.clone();
    window.on_recipe_gesture(move |token, generation, gesture| {
        output.borrow_mut().push((token, generation, gesture))
    });
    window.window().dispatch_event(WindowEvent::KeyPressed {
        text: Key::Return.into(),
    });
    window.window().dispatch_event(WindowEvent::KeyReleased {
        text: Key::Return.into(),
    });
    assert_eq!(
        &*calls.borrow(),
        &[(
            popup_restored.token,
            popup_restored.generation,
            crate::ProRecipeGesture::Tap
        )]
    );
    window.invoke_native_popup_dismiss(
        window.get_native_popup_generation(),
        crate::CloseReason::Escape,
    );
    publish_focus_view(&window, &shared, &store, &entity);
    settle_focus(&window);
    assert_home_selected();
    let revision = shared.lock().unwrap().revision;
    let home_restored = store
        .recipe_focus(&NodeId::parse("recipe").unwrap(), &layout, revision)
        .unwrap();
    window.window().dispatch_event(WindowEvent::KeyPressed {
        text: Key::Return.into(),
    });
    window.window().dispatch_event(WindowEvent::KeyReleased {
        text: Key::Return.into(),
    });
    assert_eq!(
        calls.borrow().last(),
        Some(&(
            home_restored.token,
            home_restored.generation,
            crate::ProRecipeGesture::Tap
        ))
    );
    assert_eq!(calls.borrow().len(), 2);
    assert!(!window.get_native_popup().open && !window.get_recipe_focus().present);
    assert!(store.0.lock().unwrap().actions.is_empty());
    window.hide().unwrap();
}

#[test]
fn popup_native_tabs_cycle_its_actual_controls_and_skip_background_and_revoked_commands() {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use slint::{
        ComponentHandle,
        platform::{Key, WindowEvent},
    };
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let (mut request, mut entity, store) = fixture(Intention::MoreInfo(entity_id()), None);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let native = layout.native.as_mut().unwrap();
    native
        .nodes
        .get_mut(&NodeId::parse("popup-tile").unwrap())
        .unwrap()
        .recipes = native.nodes[&NodeId::parse("recipe").unwrap()]
        .recipes
        .clone();
    request.dashboard = LayoutState::Ready(Arc::new(layout.clone()));
    let window = crate::create_window().unwrap();
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    crate::cache_view::bind(&window);
    let shared = Arc::new(Mutex::new(request));
    let devices = store.clone();
    let navigation = crate::navigation::bind_devices(
        &window,
        shared.clone(),
        || {},
        move || Some(devices.clone()),
    );
    publish_focus_view(&window, &shared, &store, &entity);
    window.show().unwrap();
    settle_focus(&window);
    navigation
        .navigate(
            Destination {
                dashboard: DashboardId::parse(layout.id.as_str()).unwrap(),
                node: NodeId::parse("popup").unwrap(),
                presentation: Presentation::Popup,
            },
            &layout,
            None,
        )
        .unwrap();
    publish_focus_view(&window, &shared, &store, &entity);
    settle_focus(&window);
    let recipes = Rc::new(Cell::new(0));
    let count = recipes.clone();
    window.on_recipe_gesture(move |_, _, _| count.set(count.get() + 1));
    let power = Rc::new(Cell::new(0));
    let count = power.clone();
    window.on_device_power(move |_, _, intent| {
        assert_eq!(intent, crate::ProPowerIntent::On);
        count.set(count.get() + 1);
    });
    let brightness = Rc::new(Cell::new(0));
    let count = brightness.clone();
    window.on_device_brightness(move |_, _, _| count.set(count.get() + 1));
    let details = Rc::new(Cell::new(0));
    let count = details.clone();
    window.on_device_details(move |_, _| count.set(count.get() + 1));
    let key = |key: Key| {
        window
            .window()
            .dispatch_event(WindowEvent::KeyPressed { text: key.into() });
        window
            .window()
            .dispatch_event(WindowEvent::KeyReleased { text: key.into() });
    };
    key(Key::Tab);
    key(Key::Return);
    assert_eq!(recipes.get(), 1);
    key(Key::Tab);
    key(Key::Return);
    assert_eq!(power.get(), 1);
    key(Key::Tab);
    key(Key::RightArrow);
    assert_eq!(
        brightness.get(),
        0,
        "arrow navigation only changes the local slider draft"
    );
    key(Key::Return);
    assert_eq!(brightness.get(), 1);
    key(Key::Tab);
    key(Key::Return);
    assert_eq!(details.get(), 1);
    key(Key::Tab); // Details -> Close -> header, without leaving the modal.
    key(Key::Tab);
    key(Key::Return);
    assert_eq!(recipes.get(), 2);
    key(Key::Backtab);
    key(Key::Backtab);
    key(Key::Return);
    assert_eq!(details.get(), 2);
    assert!(!window.get_search_focused() && window.get_native_popup().open);
    entity.action_access[0].access = state::ActionAccess::Denied.into();
    publish_focus_view(&window, &shared, &store, &entity);
    window.invoke_take_body_focus(crate::BodyFocus::Popup);
    key(Key::Tab);
    key(Key::Return);
    assert_eq!(recipes.get(), 3);
    key(Key::Tab);
    key(Key::Return);
    assert_eq!(details.get(), 3);
    key(Key::Tab);
    key(Key::Tab);
    key(Key::Return);
    assert_eq!(recipes.get(), 4);
    assert_eq!(power.get(), 1);
    assert_eq!(brightness.get(), 1);
    assert!(!window.get_search_focused() && window.get_native_popup().open);
    assert!(store.0.lock().unwrap().actions.is_empty());
    window.invoke_native_popup_dismiss(
        window.get_native_popup_generation(),
        crate::CloseReason::Escape,
    );
    window.invoke_take_shell_focus(crate::ProShellFocus::Search);
    assert!(
        window.get_search_focused(),
        "the shell becomes eligible again after modal closure"
    );
    window.hide().unwrap();
}

#[test]
fn popup_native_tabs_keep_helper_editors_and_nested_selector_popups_inside_the_modal() {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use home_wire_contract::home::helpers::v1::HelperKind;
    use slint::{
        ComponentHandle,
        platform::{Key, WindowEvent},
    };
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    for kind in [HelperKind::Text, HelperKind::Select] {
        let window = crate::create_window().unwrap();
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        crate::cache_view::bind(&window);
        let (mut request, original, store) = fixture(Intention::MoreInfo(entity_id()), None);
        let mut entity = super::tests::helper_entity(&super::tests::request(), kind);
        entity.state.as_option_mut().unwrap().quality =
            original.state.as_option().unwrap().quality.clone();
        let mut layout = request.dashboard.layout().unwrap().clone();
        let native = layout.native.as_mut().unwrap();
        native
            .nodes
            .get_mut(&NodeId::parse("popup-tile").unwrap())
            .unwrap()
            .recipes = native.nodes[&NodeId::parse("recipe").unwrap()]
            .recipes
            .clone();
        request.dashboard = LayoutState::Ready(Arc::new(layout.clone()));
        let shared = Arc::new(Mutex::new(request));
        let devices = store.clone();
        let navigation = crate::navigation::bind_devices(
            &window,
            shared.clone(),
            || {},
            move || Some(devices.clone()),
        );
        let drafts = Rc::new(Cell::new(0));
        let observed = drafts.clone();
        let editing = store.clone();
        let requests = shared.clone();
        let cached = entity.clone();
        let weak = window.as_weak();
        window.on_device_edit(move |token, generation, field, intent| {
            if matches!(
                intent.event,
                crate::ProValueEvent::Draft | crate::ProValueEvent::SelectQuery
            ) {
                observed.set(observed.get() + 1);
            }
            let revision = requests.lock().unwrap().revision;
            // Mirror the real host's editor acknowledgment, with no command
            // dispatcher: invalid drafts stay editable; selection only filters.
            let result = editing.edit(token, generation, revision, field, intent);
            if let Ok(Some(_)) = result {
                panic!("typing/tabbing must not prepare a command");
            }
            if let Some(window) = weak.upgrade() {
                publish_focus_view(&window, &requests, &editing, &cached);
            }
        });
        publish_focus_view(&window, &shared, &store, &entity);
        window.show().unwrap();
        settle_focus(&window);
        navigation
            .navigate(
                Destination {
                    dashboard: DashboardId::parse(layout.id.as_str()).unwrap(),
                    node: NodeId::parse("popup").unwrap(),
                    presentation: Presentation::Popup,
                },
                &layout,
                None,
            )
            .unwrap();
        publish_focus_view(&window, &shared, &store, &entity);
        settle_focus(&window);
        let revision = shared.lock().unwrap().revision;
        if kind == HelperKind::Select {
            for key in [Key::Tab, Key::Tab, Key::Return] {
                window
                    .window()
                    .dispatch_event(WindowEvent::KeyPressed { text: key.into() });
                window
                    .window()
                    .dispatch_event(WindowEvent::KeyReleased { text: key.into() });
                settle_focus(&window);
            }
            window
                .window()
                .dispatch_event(WindowEvent::KeyPressed { text: "k".into() });
            window
                .window()
                .dispatch_event(WindowEvent::KeyReleased { text: "k".into() });
            settle_focus(&window);
            assert!(
                drafts.get() > 0,
                "Enter opens the selector's real native search editor"
            );
        }
        for index in 0..20 {
            let tab = if index < 10 { Key::Tab } else { Key::Backtab };
            window
                .window()
                .dispatch_event(WindowEvent::KeyPressed { text: tab.into() });
            window
                .window()
                .dispatch_event(WindowEvent::KeyReleased { text: tab.into() });
            settle_focus(&window);
            window
                .window()
                .dispatch_event(WindowEvent::KeyPressed { text: "k".into() });
            window
                .window()
                .dispatch_event(WindowEvent::KeyReleased { text: "k".into() });
            settle_focus(&window);
            assert!(
                window.get_native_popup().open && !window.get_search_focused(),
                "helper {kind:?} cannot escape the modal"
            );
            assert_eq!(shared.lock().unwrap().revision, revision);
        }
        assert!(
            drafts.get() > 0,
            "the actual native editor must participate in tab traversal for {kind:?}"
        );
        assert!(store.0.lock().unwrap().actions.is_empty());
        window.hide().unwrap();
    }
}

#[test]
fn native_page_without_a_room_path_uses_its_exact_source_identity() {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use slint::ComponentHandle;
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    crate::cache_view::bind(&window);
    let (mut request, entity, store) = fixture(Intention::MoreInfo(entity_id()), None);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let native = layout.native.as_mut().unwrap();
    let mut page = native.nodes[&NodeId::parse("view").unwrap()].clone();
    page.id = NodeId::parse("configured-page-without-path").unwrap();
    page.source.path = page.id.as_str().into();
    page.title = "Configured page".into();
    page.route = None;
    page.children.clear();
    native.views.push(page.id.clone());
    native.nodes.insert(page.id.clone(), page.clone());
    request.dashboard = LayoutState::Ready(Arc::new(layout.clone()));
    let shared = Arc::new(Mutex::new(request));
    let navigation = crate::navigation::bind_devices(&window, shared.clone(), || {}, || None);
    let destination = Destination {
        dashboard: DashboardId::parse(layout.id.as_str()).unwrap(),
        node: page.id.clone(),
        presentation: Presentation::Page,
    };
    navigation
        .navigate(destination.clone(), &layout, None)
        .unwrap();
    assert_eq!(window.get_page(), crate::ViewPage::Room);
    assert!(window.get_native_page());
    assert_eq!(window.get_page_title(), "Configured page");
    {
        let request = shared.lock().unwrap();
        assert!(request.room.is_none());
        assert_eq!(request.native_root(&layout), Some(&page.id));
        let projection = project(&store, &request, &entity);
        assert_eq!(projection.native_nodes.len(), 1);
        assert_eq!(projection.native_nodes[0].id, page.id);
    }
    let revision = shared.lock().unwrap().revision;
    let mut wrong = destination.clone();
    wrong.node = NodeId::parse("missing-source-node").unwrap();
    assert_eq!(
        navigation.navigate(wrong, &layout, None),
        Err(Refusal::Unavailable)
    );
    let mut wrong = destination;
    wrong.presentation = Presentation::Popup;
    assert_eq!(
        navigation.navigate(wrong, &layout, None),
        Err(Refusal::Unsupported)
    );
    assert_eq!(
        shared.lock().unwrap().revision,
        revision,
        "a rejected destination never changes the route"
    );
    window.invoke_go_back();
    assert_eq!(window.get_page(), crate::ViewPage::Home);
    assert!(shared.lock().unwrap().native.is_none());
    assert!(store.0.lock().unwrap().actions.is_empty());
    window.hide().unwrap();
}

#[test]
fn popup_detail_and_back_keep_separate_models_and_never_open_a_device_command() {
    use home_client_cache::{ProjectionMailbox, Viewport};
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use slint::{ComponentHandle, Model};
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    crate::cache_view::bind(&window);
    let (mut request, entity, store) = fixture(Intention::MoreInfo(entity_id()), None);
    let mut layout = request.dashboard.layout().unwrap().clone();
    let popup_id = NodeId::parse("popup").unwrap();
    layout
        .native
        .as_mut()
        .unwrap()
        .nodes
        .get_mut(&popup_id)
        .unwrap()
        .title = "Configured popup".into();
    request.dashboard = LayoutState::Ready(Arc::new(layout.clone()));
    let cache = view(&request, vec![entity]);
    let shared = Arc::new(Mutex::new(request));
    let navigation = crate::navigation::bind_devices(&window, shared.clone(), || {}, || None);
    let generation = ProjectionMailbox::<()>::new().generation();
    let publish = || {
        let projection = crate::cache_view::project_devices(
            CachePhase::Live,
            Some(&cache),
            Viewport::default(),
            &shared.lock().unwrap(),
            &store,
        );
        crate::cache_view::render(&window, generation, projection);
    };
    publish();
    let home = window.get_home_layout_nodes();
    let home_count = home.row_count();
    assert!(home_count > 2 && window.get_popup_layout_nodes().row_count() == 0);
    let destination = Destination {
        dashboard: DashboardId::parse(layout.id.as_str()).unwrap(),
        node: popup_id.clone(),
        presentation: Presentation::Popup,
    };
    navigation.navigate(destination, &layout, None).unwrap();
    assert!(window.get_native_popup().open);
    assert_eq!(window.get_native_popup_title(), "Configured popup");
    assert_eq!(window.get_page(), crate::ViewPage::Home);
    publish();
    assert_eq!(window.get_popup_layout_nodes().row_count(), 2);
    assert_eq!(
        home.row_count(),
        home_count,
        "popup publication must preserve the hidden home content/scroll extent"
    );
    assert_eq!(home, window.get_home_layout_nodes());
    let popup_generation = window.get_native_popup_generation();
    window.invoke_native_popup_dismiss(popup_generation - 1, crate::CloseReason::Escape);
    assert!(
        window.get_native_popup().open,
        "a stale close cannot dismiss the current popup"
    );
    navigation.more_info(entity_id(), None).unwrap();
    assert_eq!(window.get_page(), crate::ViewPage::Detail);
    assert!(!window.get_native_popup().open);
    assert_eq!(
        shared.lock().unwrap().native.as_ref().unwrap().node,
        popup_id
    );
    publish();
    window.invoke_go_back();
    assert!(window.get_native_popup().open);
    publish();
    assert_eq!(window.get_popup_layout_nodes().row_count(), 2);
    window.invoke_native_popup_dismiss(
        window.get_native_popup_generation(),
        crate::CloseReason::Escape,
    );
    assert!(!window.get_native_popup().open);
    assert!(shared.lock().unwrap().native.is_none());
    publish();
    assert_eq!(home.row_count(), home_count);
    assert!(store.0.lock().unwrap().actions.is_empty());
    crate::cache_view::clear(&window);
    assert_eq!(home.row_count(), 0);
    assert_eq!(window.get_popup_layout_nodes().row_count(), 0);
    assert_eq!(window.get_room_layout_nodes().row_count(), 0);
    window.hide().unwrap();
}

#[test]
fn a_pointer_press_from_an_old_prompt_cannot_confirm_its_replacement() {
    use i_slint_backend_testing::ElementQuery;
    use slint::{
        ComponentHandle,
        platform::{PointerEventButton, WindowEvent},
    };
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_view_loaded(true);
    let (request, mut entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::TurnOn,
        },
        Some("Confirm"),
    );
    let first = project(&store, &request, &entity);
    let prepare = store.clone();
    let sent = Rc::new(Cell::new(0));
    let count = sent.clone();
    crate::dashboard_gestures::bind(
        &window,
        move |token, generation, gesture| prepare.prepare_recipe(token, generation, 0, gesture),
        move |_| count.set(count.get() + 1),
        |_| {},
    );
    window.show().unwrap();
    window.invoke_recipe_gesture(
        recipe(&first).token,
        recipe(&first).generation,
        crate::ProRecipeGesture::Tap,
    );
    slint::platform::update_timers_and_animations();
    let confirm = ElementQuery::from_root(&window)
        .match_inherits("KitButton")
        .match_predicate(|item| item.accessible_label().as_deref() == Some("Confirmer"))
        .find_first()
        .unwrap();
    let origin = confirm.absolute_position();
    let size = confirm.size();
    assert!(size.width > 0. && size.height >= 48.);
    let position =
        slint::LogicalPosition::new(origin.x + size.width / 2., origin.y + size.height / 2.);
    window.window().dispatch_event(WindowEvent::PointerPressed {
        position,
        button: PointerEventButton::Left,
    });
    let old = window.get_recipe_confirmation_id();
    entity.action_access[0].access = state::ActionAccess::Denied.into();
    project(&store, &request, &entity);
    window.invoke_recipe_context_changed();
    entity.action_access[0].access = state::ActionAccess::Allowed.into();
    let next = project(&store, &request, &entity);
    window.invoke_recipe_gesture(
        recipe(&next).token,
        recipe(&next).generation,
        crate::ProRecipeGesture::Tap,
    );
    slint::platform::update_timers_and_animations();
    assert!(window.get_recipe_confirmation_id() > old && window.get_recipe_confirmation().open);
    window
        .window()
        .dispatch_event(WindowEvent::PointerReleased {
            position,
            button: PointerEventButton::Left,
        });
    assert_eq!(sent.get(), 0);
    assert!(window.get_recipe_confirmation().open);
    window.window().dispatch_event(WindowEvent::PointerPressed {
        position,
        button: PointerEventButton::Left,
    });
    window
        .window()
        .dispatch_event(WindowEvent::PointerReleased {
            position,
            button: PointerEventButton::Left,
        });
    assert_eq!(
        sent.get(),
        1,
        "a fresh pointer activation may confirm the current prompt"
    );
    window.hide().unwrap();
}

#[test]
fn physical_toggle_is_resolved_once_and_ordinary_observations_preserve_the_binding() {
    let (request, mut entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::Toggle,
        },
        Some(""),
    );
    let before = project(&store, &request, &entity);
    assert_eq!(recipe(&before).access(Gesture::Tap), GestureAccess::Command);
    let captured = prepare(&store, &before);
    assert_eq!(captured.confirmation.as_deref(), Some(""));
    assert!(physical_on(&captured));
    assert!(captured.description.starts_with("Allumer ·"));
    set_power(&mut entity, true);
    entity.state.as_option_mut().unwrap().revision += 1;
    let refreshed = project(&store, &request, &entity);
    assert_eq!(recipe(&before).generation, recipe(&refreshed).generation);
    assert!(captured.is_current());
    assert!(
        physical_on(&captured),
        "confirmation keeps the prepared ON; it cannot become a new toggle"
    );
    assert!(
        !physical_on(&prepare(&store, &refreshed)),
        "a new gesture resolves against the latest observed power"
    );
}

#[test]
fn a_command_target_without_a_visible_device_card_uses_the_same_central_admission() {
    let (mut request, entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::TurnOff,
        },
        None,
    );
    let mut layout = request.dashboard.layout().unwrap().clone();
    let native = layout.native.as_mut().unwrap();
    let tile = NodeId::parse("tile").unwrap();
    native.nodes.remove(&tile);
    for node in native.nodes.values_mut() {
        node.children.retain(|id| id != &tile);
    }
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    let projection = project(&store, &request, &entity);
    assert_eq!(
        projection.rows.len(),
        1,
        "a configured command resolves its opaque target even without a device card"
    );
    assert_eq!(
        recipe(&projection).access(Gesture::Tap),
        GestureAccess::Command
    );
    assert!(!physical_on(&prepare(&store, &projection)));
}

#[test]
fn permission_capability_and_ambiguous_endpoints_cannot_fall_back_to_the_first_target() {
    let (request, mut entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::TurnOn,
        },
        Some("Confirm"),
    );
    let before = project(&store, &request, &entity);
    let captured = prepare(&store, &before);
    entity.action_access[0].access = state::ActionAccess::Denied.into();
    let denied = project(&store, &request, &entity);
    assert_eq!(
        recipe(&denied).access(Gesture::Tap),
        GestureAccess::Inactive
    );
    assert!(!captured.is_current());
    assert!(matches!(
        store.prepare_recipe(
            recipe(&denied).token,
            recipe(&denied).generation,
            0,
            Gesture::Tap
        ),
        Err(Refusal::ReadOnly)
    ));
    entity.action_access[0].access = state::ActionAccess::Allowed.into();
    let mut second = entity.capabilities[0].clone();
    second.endpoint = "second-endpoint".into();
    let mut permission = entity.action_access[0].clone();
    permission.endpoint = second.endpoint.clone();
    entity.capabilities.push(second);
    entity.action_access.push(permission);
    let ambiguous = project(&store, &request, &entity);
    assert!(recipe(&ambiguous).unavailable);
    assert!(matches!(
        store.prepare_recipe(
            recipe(&ambiguous).token,
            recipe(&ambiguous).generation,
            0,
            Gesture::Tap
        ),
        Err(Refusal::Unsupported)
    ));
    entity.capabilities.pop();
    entity.action_access.pop();
    entity.capabilities[0].major = 99;
    let unknown = project(&store, &request, &entity);
    assert_eq!(
        recipe(&unknown).access(Gesture::Tap),
        GestureAccess::Inactive
    );
    assert!(!captured.is_current());
}

#[test]
fn explicit_none_is_consumed_and_missing_state_cannot_create_a_physical_toggle() {
    let (mut request, mut entity, store) = fixture(Intention::None, None);
    let projection = project(&store, &request, &entity);
    assert_eq!(
        recipe(&projection).access(Gesture::Tap),
        GestureAccess::Inactive
    );
    assert_eq!(
        recipe(&projection).access(Gesture::Hold),
        GestureAccess::Absent
    );
    assert!(matches!(
        store.prepare_recipe(
            recipe(&projection).token,
            recipe(&projection).generation,
            0,
            Gesture::Tap
        ),
        Err(Refusal::Unsupported)
    ));
    let mut layout = request.dashboard.layout().unwrap().clone();
    layout
        .native
        .as_mut()
        .unwrap()
        .nodes
        .get_mut(&NodeId::parse("recipe").unwrap())
        .unwrap()
        .recipes
        .as_mut()
        .unwrap()
        .interactions
        .get_mut(&Gesture::Tap)
        .unwrap()
        .intention = Intention::Command {
        entity: entity_id(),
        kind: CommandKind::Toggle,
    };
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    entity
        .state
        .as_option_mut()
        .unwrap()
        .quality
        .as_option_mut()
        .unwrap()
        .freshness = state::Freshness::Stale.into();
    let stale = project(&store, &request, &entity);
    assert_eq!(recipe(&stale).access(Gesture::Tap), GestureAccess::Inactive);
}

#[test]
fn read_intentions_keep_their_entity_but_removal_private_clear_and_route_changes_cancel_them() {
    let (mut request, entity, store) = fixture(Intention::MoreInfo(entity_id()), None);
    let before = project(&store, &request, &entity);
    let captured = prepare(&store, &before);
    assert!(matches!(&captured.intent, NativeIntent::MoreInfo(entity) if entity == &entity_id()));
    let empty = store.project(CachePhase::Live, Some(&view(&request, vec![])), &request);
    assert_eq!(recipe(&empty).access(Gesture::Tap), GestureAccess::Inactive);
    assert!(!captured.is_current());
    let visible = project(&store, &request, &entity);
    let captured = prepare(&store, &visible);
    request.revision += 1;
    project(&store, &request, &entity);
    assert!(!captured.is_current());
    let row = recipe(&visible);
    assert!(matches!(
        store.prepare_recipe(row.token, row.generation, 0, Gesture::Tap),
        Err(Refusal::Stale)
    ));
    store.clear_private();
    assert!(
        store
            .prepare_recipe(row.token, row.generation, 1, Gesture::Tap)
            .is_err()
    );
}

#[test]
fn a_hidden_recipe_cannot_keep_authority_through_its_still_visible_command_target() {
    use home_client_model::dashboard::{Comparison, Predicate};
    let (mut request, mut entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::TurnOff,
        },
        Some("Confirm"),
    );
    let mut layout = request.dashboard.layout().unwrap().clone();
    layout
        .native
        .as_mut()
        .unwrap()
        .nodes
        .get_mut(&NodeId::parse("recipe").unwrap())
        .unwrap()
        .recipes
        .as_mut()
        .unwrap()
        .visibility = Some(Predicate::State {
        entity: entity_id(),
        expected: "on".into(),
        comparison: Comparison::Equal,
    });
    request.dashboard = LayoutState::Ready(Arc::new(layout));
    set_power(&mut entity, true);
    let visible = project(&store, &request, &entity);
    let captured = prepare(&store, &visible);
    set_power(&mut entity, false);
    let hidden = project(&store, &request, &entity);
    assert_eq!(
        hidden.rows.len(),
        1,
        "the independent tile still represents this device"
    );
    assert!(
        !hidden
            .recipes
            .contains_key(&NodeId::parse("recipe").unwrap())
    );
    assert!(!captured.is_current());
    let old = recipe(&visible);
    assert!(matches!(
        store.prepare_recipe(old.token, old.generation, 0, Gesture::Tap),
        Err(Refusal::Stale)
    ));
}

#[test]
fn a_helper_recipe_keeps_the_exact_sql_revision_and_cannot_reuse_it_after_an_update() {
    use home_wire_contract::home::helpers::v1 as helpers;
    let (request, mut entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::Toggle,
        },
        Some("Confirm"),
    );
    entity.capabilities[0].schema_id = "core.helper.v1".into();
    entity.capabilities[0].constraints =
        Some(registry::capability_description::Constraints::Helper(
            helpers::HelperConstraints {
                kind: helpers::HelperKind::Boolean.into(),
                operations: vec![helpers::HelperOperation::Toggle.into()],
                ..Default::default()
            }
            .into(),
        ));
    entity.action_access[0].schema_id = "core.helper.v1".into();
    entity.state.as_option_mut().unwrap().value = Some(state::observed_state::Value::Helper(
        helpers::HelperValue {
            helper_revision: 7,
            value: Some(helpers::helper_value::Value::Boolean(false)),
            ..Default::default()
        }
        .into(),
    ));
    let initial = project(&store, &request, &entity);
    let captured = prepare(&store, &initial);
    let NativeIntent::Command(command) = &captured.intent else {
        panic!()
    };
    let Some(actions::action_args::Arguments::Helper(action)) = command
        .request
        .arguments
        .as_option()
        .unwrap()
        .arguments
        .as_ref()
    else {
        panic!()
    };
    assert_eq!(
        action.operation.as_known(),
        Some(helpers::HelperOperation::Toggle)
    );
    assert_eq!(action.expected_helper_revision, Some(7));
    let Some(state::observed_state::Value::Helper(value)) =
        entity.state.as_option_mut().unwrap().value.as_mut()
    else {
        panic!()
    };
    value.helper_revision = 8;
    let changed = project(&store, &request, &entity);
    assert_ne!(recipe(&initial).generation, recipe(&changed).generation);
    assert!(!captured.is_current());
    entity.state.as_option_mut().unwrap().value = None;
    let absent = project(&store, &request, &entity);
    assert_eq!(
        recipe(&absent).access(Gesture::Tap),
        GestureAccess::Inactive
    );
}

#[test]
fn native_confirmation_consumes_one_exact_intention_and_never_replays_an_old_id() {
    use slint::ComponentHandle;
    use std::{cell::RefCell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_view_loaded(true);
    let (request, mut entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::Toggle,
        },
        Some(""),
    );
    let before = project(&store, &request, &entity);
    let prepare = store.clone();
    let dispatched = Rc::new(RefCell::new(Vec::new()));
    let received = dispatched.clone();
    crate::dashboard_gestures::bind(
        &window,
        move |token, generation, gesture| prepare.prepare_recipe(token, generation, 0, gesture),
        move |prepared| received.borrow_mut().push(prepared),
        |_| {},
    );
    window.invoke_recipe_gesture(
        recipe(&before).token,
        recipe(&before).generation,
        crate::ProRecipeGesture::Tap,
    );
    assert!(window.get_recipe_confirmation().open);
    assert_eq!(
        window.get_recipe_confirmation_message(),
        "Confirmer cette interaction ?"
    );
    assert!(
        window
            .get_recipe_confirmation_description()
            .starts_with("Allumer ·")
    );
    let id = window.get_recipe_confirmation_id();
    window.invoke_recipe_confirm(id + 1);
    assert!(dispatched.borrow().is_empty() && window.get_recipe_confirmation().open);
    set_power(&mut entity, true);
    project(&store, &request, &entity);
    window.invoke_recipe_context_changed();
    assert!(window.get_recipe_confirmation().open);
    window.invoke_recipe_confirm(id);
    assert_eq!(dispatched.borrow().len(), 1);
    assert!(physical_on(&dispatched.borrow()[0]));
    window.invoke_recipe_confirm(id);
    assert_eq!(dispatched.borrow().len(), 1);
    assert!(!window.get_recipe_confirmation().open);
    window.hide().unwrap();
}

#[test]
fn permission_loss_or_private_clear_closes_a_native_confirmation_without_submission() {
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_view_loaded(true);
    let (request, mut entity, store) = fixture(
        Intention::Command {
            entity: entity_id(),
            kind: CommandKind::TurnOn,
        },
        Some("Custom prompt"),
    );
    let before = project(&store, &request, &entity);
    let prepare = store.clone();
    let sent = Rc::new(Cell::new(0));
    let count = sent.clone();
    crate::dashboard_gestures::bind(
        &window,
        move |token, generation, gesture| prepare.prepare_recipe(token, generation, 0, gesture),
        move |_| count.set(count.get() + 1),
        |_| {},
    );
    window.invoke_recipe_gesture(
        recipe(&before).token,
        recipe(&before).generation,
        crate::ProRecipeGesture::Tap,
    );
    assert_eq!(window.get_recipe_confirmation_message(), "Custom prompt");
    let old = window.get_recipe_confirmation_id();
    entity.action_access[0].access = state::ActionAccess::Denied.into();
    project(&store, &request, &entity);
    window.invoke_recipe_context_changed();
    assert!(!window.get_recipe_confirmation().open);
    window.invoke_recipe_confirm(old);
    assert_eq!(sent.get(), 0);
    entity.action_access[0].access = state::ActionAccess::Allowed.into();
    let restored = project(&store, &request, &entity);
    window.invoke_recipe_gesture(
        recipe(&restored).token,
        recipe(&restored).generation,
        crate::ProRecipeGesture::Tap,
    );
    assert!(window.get_recipe_confirmation_id() > old);
    crate::cache_view::clear(&window);
    assert!(!window.get_recipe_confirmation().open);
    assert!(window.get_recipe_confirmation_message().is_empty());
    window.invoke_recipe_confirm(window.get_recipe_confirmation_id());
    assert_eq!(sent.get(), 0);
}

fn cross_dashboard_fixture() -> (
    ProjectionRequest,
    state::ViewEntity,
    DeviceStore,
    crate::dashboard_selection::Choices,
    Arc<crate::dashboard::Layout>,
) {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    let (mut request, entity, store) = fixture(
        Intention::Navigate(Destination {
            dashboard: DashboardId::parse("synthetic-target-dashboard").unwrap(),
            node: NodeId::parse("view").unwrap(),
            presentation: Presentation::Page,
        }),
        None,
    );
    let source = request.dashboard.shared_layout().unwrap();
    let mut target = source.as_ref().clone();
    target.id = crate::dashboard::DashboardId::parse("synthetic-target-dashboard").unwrap();
    target.title = "Synthetic target dashboard".into();
    let native = target.native.as_mut().unwrap();
    native
        .nodes
        .get_mut(&NodeId::parse("view").unwrap())
        .unwrap()
        .title = "Target page".into();
    native
        .nodes
        .get_mut(&NodeId::parse("popup").unwrap())
        .unwrap()
        .title = "Target popup".into();
    let target = Arc::new(target);
    let snapshot = crate::dashboard_catalogue::Snapshot::fixture(
        "synthetic-cohort-a",
        vec![source.clone(), target.clone()],
    );
    let choices = crate::dashboard_selection::Choices::default();
    assert!(
        choices.accept(
            0,
            crate::dashboard_selection::Catalogue {
                choices: [source.as_ref(), target.as_ref()]
                    .into_iter()
                    .map(|layout| crate::dashboard_selection::Choice {
                        id: layout.id.clone(),
                        title: layout.title.clone()
                    })
                    .collect(),
                selected: Some(source.id.clone()),
                snapshot: Some(snapshot.clone()),
            }
        )
    );
    request.catalogue = Some(snapshot);
    (request, entity, store, choices, target)
}

#[test]
fn cross_dashboard_pages_and_popups_return_to_the_exact_source_binding_without_commands() {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use slint::{
        ComponentHandle,
        platform::{Key, WindowEvent},
    };
    use std::{cell::RefCell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    for presentation in [Presentation::Page, Presentation::Popup] {
        let window = crate::create_window().unwrap();
        window.set_access_stage(crate::ProAccessStage::Connected);
        window.set_show_endpoint(false);
        crate::cache_view::bind(&window);
        let (request, entity, store, choices, target) = cross_dashboard_fixture();
        let source = request.dashboard.shared_layout().unwrap();
        let shared = Arc::new(Mutex::new(request));
        let devices = store.clone();
        let navigation = crate::navigation::bind_devices(
            &window,
            shared.clone(),
            || {},
            move || Some(devices.clone()),
        );
        navigation.attach_catalogue(choices.clone());
        publish_focus_view(&window, &shared, &store, &entity);
        window.show().unwrap();
        settle_focus(&window);
        let row = store
            .recipe_focus(&NodeId::parse("recipe").unwrap(), &source, 0)
            .unwrap();
        let prepared = store
            .prepare_recipe(row.token, row.generation, 0, Gesture::Tap)
            .unwrap();
        navigation
            .navigate(
                Destination {
                    dashboard: DashboardId::parse(target.id.as_str()).unwrap(),
                    node: NodeId::parse(match presentation {
                        Presentation::Page => "view",
                        Presentation::Popup => "popup",
                    })
                    .unwrap(),
                    presentation,
                },
                &source,
                Some(crate::dashboard_focus::Origin::prepared(&prepared)),
            )
            .unwrap();
        assert_eq!(choices.snapshot().1.selected, Some(target.id.clone()));
        assert_eq!(
            shared.lock().unwrap().dashboard.layout(),
            Some(target.as_ref())
        );
        match presentation {
            Presentation::Page => assert_eq!(window.get_page_title(), "Target page"),
            Presentation::Popup => {
                assert_eq!(window.get_native_popup_title(), "Target popup");
                assert_eq!(window.get_page(), crate::ViewPage::Home);
            }
        }
        publish_focus_view(&window, &shared, &store, &entity);
        assert!(
            !prepared.is_current(),
            "a source gesture cannot survive switching its dashboard"
        );
        window.invoke_go_back();
        assert_eq!(choices.snapshot().1.selected, Some(source.id.clone()));
        assert_eq!(
            shared.lock().unwrap().dashboard.layout(),
            Some(source.as_ref())
        );
        assert_eq!(window.get_page(), crate::ViewPage::Home);
        assert!(!window.get_native_popup().open);
        publish_focus_view(&window, &shared, &store, &entity);
        settle_focus(&window);
        let revision = shared.lock().unwrap().revision;
        let restored = store
            .recipe_focus(&NodeId::parse("recipe").unwrap(), &source, revision)
            .unwrap();
        assert_ne!(restored.generation, row.generation);
        let calls = Rc::new(RefCell::new(Vec::new()));
        let output = calls.clone();
        window.on_recipe_gesture(move |token, generation, gesture| {
            output.borrow_mut().push((token, generation, gesture))
        });
        window.window().dispatch_event(WindowEvent::KeyPressed {
            text: Key::Return.into(),
        });
        window.window().dispatch_event(WindowEvent::KeyReleased {
            text: Key::Return.into(),
        });
        assert_eq!(
            &*calls.borrow(),
            &[(
                restored.token,
                restored.generation,
                crate::ProRecipeGesture::Tap
            )]
        );
        assert!(store.0.lock().unwrap().actions.is_empty());
        window.hide().unwrap();
    }
}

#[derive(Clone, Copy, Debug)]
enum CatalogueRefusal {
    MissingTarget,
    WrongRole,
    MissingChoice,
    ChangedSource,
    ForeignToken,
}
#[test]
fn cross_dashboard_refusals_never_mutate_the_route_or_selection() {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use slint::ComponentHandle;
    i_slint_backend_testing::init_no_event_loop();
    for case in [
        CatalogueRefusal::MissingTarget,
        CatalogueRefusal::WrongRole,
        CatalogueRefusal::MissingChoice,
        CatalogueRefusal::ChangedSource,
        CatalogueRefusal::ForeignToken,
    ] {
        let window = crate::create_window().unwrap();
        let (mut request, entity, store, choices, target) = cross_dashboard_fixture();
        let source = request.dashboard.shared_layout().unwrap();
        let mut destination = Destination {
            dashboard: DashboardId::parse(target.id.as_str()).unwrap(),
            node: NodeId::parse("view").unwrap(),
            presentation: Presentation::Page,
        };
        match case {
            CatalogueRefusal::MissingTarget => {
                request.catalogue = Some(crate::dashboard_catalogue::Snapshot::fixture(
                    "synthetic-cohort-a",
                    vec![source.clone()],
                ))
            }
            CatalogueRefusal::WrongRole => destination.presentation = Presentation::Popup,
            CatalogueRefusal::MissingChoice => {
                let mut catalogue = choices.snapshot().1;
                catalogue.choices.retain(|choice| choice.id == source.id);
                assert!(choices.accept(0, catalogue));
            }
            CatalogueRefusal::ChangedSource => {
                let mut changed = source.as_ref().clone();
                changed.title = "Changed source".into();
                request.catalogue = Some(crate::dashboard_catalogue::Snapshot::fixture(
                    "synthetic-cohort-a",
                    vec![Arc::new(changed), target.clone()],
                ));
            }
            CatalogueRefusal::ForeignToken => {
                request.catalogue = Some(crate::dashboard_catalogue::Snapshot::fixture(
                    "synthetic-cohort-b",
                    vec![source.clone(), target.clone()],
                ))
            }
        }
        let shared = Arc::new(Mutex::new(request));
        let navigation = crate::navigation::bind_devices(&window, shared.clone(), || {}, || None);
        navigation.attach_catalogue(choices.clone());
        publish_focus_view(&window, &shared, &store, &entity);
        let revision = shared.lock().unwrap().revision;
        assert!(
            navigation.navigate(destination, &source, None).is_err(),
            "{case:?}"
        );
        assert_eq!(shared.lock().unwrap().revision, revision, "{case:?}");
        assert_eq!(
            shared.lock().unwrap().dashboard.layout(),
            Some(source.as_ref()),
            "{case:?}"
        );
        assert_eq!(
            choices.snapshot().1.selected,
            Some(source.id.clone()),
            "{case:?}"
        );
        assert!(store.0.lock().unwrap().actions.is_empty());
        window.hide().unwrap();
    }
}

#[test]
fn a_new_catalogue_token_invalidates_prepared_gestures_and_cross_dashboard_history() {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use slint::ComponentHandle;
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    crate::cache_view::bind(&window);
    let (request, entity, store, choices, target) = cross_dashboard_fixture();
    let source = request.dashboard.shared_layout().unwrap();
    let shared = Arc::new(Mutex::new(request));
    let devices = store.clone();
    let navigation = crate::navigation::bind_devices(
        &window,
        shared.clone(),
        || {},
        move || Some(devices.clone()),
    );
    navigation.attach_catalogue(choices.clone());
    publish_focus_view(&window, &shared, &store, &entity);
    let row = store
        .recipe_focus(&NodeId::parse("recipe").unwrap(), &source, 0)
        .unwrap();
    let prepared = store
        .prepare_recipe(row.token, row.generation, 0, Gesture::Tap)
        .unwrap();
    // Rebuilding an equal cohort preserves an ongoing interaction.
    shared.lock().unwrap().catalogue = Some(crate::dashboard_catalogue::Snapshot::fixture(
        "synthetic-cohort-a",
        vec![source.clone(), target.clone()],
    ));
    publish_focus_view(&window, &shared, &store, &entity);
    assert!(prepared.is_current());
    // An identical document under a new opaque authority/replay cohort does not.
    shared.lock().unwrap().catalogue = Some(crate::dashboard_catalogue::Snapshot::fixture(
        "synthetic-cohort-b",
        vec![source.clone(), target.clone()],
    ));
    publish_focus_view(&window, &shared, &store, &entity);
    assert!(!prepared.is_current());
    let mut catalogue = choices.snapshot().1;
    catalogue.snapshot = shared.lock().unwrap().catalogue.clone();
    assert!(choices.accept(0, catalogue));
    navigation
        .navigate(
            Destination {
                dashboard: DashboardId::parse(target.id.as_str()).unwrap(),
                node: NodeId::parse("view").unwrap(),
                presentation: Presentation::Page,
            },
            &source,
            None,
        )
        .unwrap();
    let replacement = crate::dashboard_catalogue::Snapshot::fixture(
        "synthetic-cohort-c",
        vec![source.clone(), target.clone()],
    );
    shared.lock().unwrap().catalogue = Some(replacement.clone());
    let mut catalogue = choices.snapshot().1;
    catalogue.snapshot = Some(replacement);
    assert!(choices.accept(0, catalogue));
    window.invoke_go_back();
    assert_eq!(
        shared.lock().unwrap().dashboard.layout(),
        Some(target.as_ref()),
        "Back cannot restore an old cohort's document"
    );
    assert_eq!(choices.snapshot().1.selected, Some(target.id.clone()));
    assert_eq!(window.get_page(), crate::ViewPage::Home);
    assert!(window.get_action_feedback().contains("catalogue courant"));
    assert!(store.0.lock().unwrap().actions.is_empty());
    window.hide().unwrap();
}

#[test]
fn cross_dashboard_back_restores_the_sources_search_context() {
    use home_client_model::dashboard::{DashboardId, Destination, Presentation};
    use slint::ComponentHandle;
    i_slint_backend_testing::init_no_event_loop();
    let window = crate::create_window().unwrap();
    window.set_access_stage(crate::ProAccessStage::Connected);
    window.set_show_endpoint(false);
    crate::cache_view::bind(&window);
    let (request, entity, store, choices, target) = cross_dashboard_fixture();
    let source = request.dashboard.shared_layout().unwrap();
    let shared = Arc::new(Mutex::new(request));
    let navigation = crate::navigation::bind_devices(&window, shared.clone(), || {}, || None);
    navigation.attach_catalogue(choices.clone());
    publish_focus_view(&window, &shared, &store, &entity);
    window.set_search_query("source search".into());
    window.invoke_query_edited("source search".into());
    assert_eq!(window.get_page(), crate::ViewPage::Inventory);
    navigation
        .navigate(
            Destination {
                dashboard: DashboardId::parse(target.id.as_str()).unwrap(),
                node: NodeId::parse("view").unwrap(),
                presentation: Presentation::Page,
            },
            &source,
            None,
        )
        .unwrap();
    assert!(shared.lock().unwrap().query.is_empty());
    assert!(window.get_search_query().is_empty());
    window.invoke_go_back();
    assert_eq!(window.get_page(), crate::ViewPage::Inventory);
    assert_eq!(window.get_search_query(), "source search");
    assert_eq!(shared.lock().unwrap().query, "source search");
    assert_eq!(choices.snapshot().1.selected, Some(source.id.clone()));
    assert!(store.0.lock().unwrap().actions.is_empty());
    window.hide().unwrap();
}
