//! Synthetic product renders: no profile, session, transport or device command.
use crate::*;
use slint::{ComponentHandle, ModelRc};

fn panels() -> Vec<(&'static str, HomewardDetailRow)> {
    let mut result = Vec::new();
    for (name, kind) in [
        ("light", ProDeviceKind::Light),
        ("switch", ProDeviceKind::Switch),
        ("climate", ProDeviceKind::Climate),
        ("fan", ProDeviceKind::Fan),
        ("cover", ProDeviceKind::Cover),
        ("alarm", ProDeviceKind::Alarm),
    ] {
        let row = device_view::presentation::CardRow {
            title: format!("Synthetic {name}").into(),
            token: 0,
            generation: 1,
            kind,
            value: "On".into(),
            quality: "Current observation".into(),
            access: ProControlAccess::Allowed,
            power_known: true,
            power_on: true,
            can_power: true,
            can_brightness: kind == ProDeviceKind::Light,
            brightness_known: true,
            brightness_percent: 70.,
            brightness_minimum: 0.,
            brightness_maximum: 100.,
            brightness_step: 1.,
            ..Default::default()
        };
        let mut panel = device_view::detail::project(&row, row.ui(None, &Default::default()), None);
        panel.climate.setpoint = "21.5 °C".into();
        panel.climate.current = "20.0 °C".into();
        panel.climate.can_increment = true;
        panel.climate.can_decrement = true;
        panel.climate.modes.rows = ModelRc::from(
            [
                ChoiceRow {
                    token: 0,
                    label: "Heat".into(),
                    selected: true,
                    disabled: false,
                },
                ChoiceRow {
                    token: 1,
                    label: "Cool".into(),
                    selected: false,
                    disabled: false,
                },
            ]
            .as_slice(),
        );
        panel.fan.speeds = ModelRc::from(
            [
                ChoiceRow {
                    token: 0,
                    label: "Off".into(),
                    selected: false,
                    disabled: false,
                },
                ChoiceRow {
                    token: 1,
                    label: "Low".into(),
                    selected: true,
                    disabled: false,
                },
                ChoiceRow {
                    token: 2,
                    label: "High".into(),
                    selected: false,
                    disabled: false,
                },
            ]
            .as_slice(),
        );
        panel.cover.position = 65.;
        panel.cover.position_known = true;
        panel.positioned = true;
        panel.position_minimum = 0.;
        panel.position_maximum = 100.;
        panel.position_step = 0.1;
        panel.cover.operations = ModelRc::from(
            [
                ProCoverChoice {
                    token: 0,
                    label: "Open".into(),
                    operation: ProCoverOperation::Open,
                },
                ProCoverChoice {
                    token: 1,
                    label: "Stop".into(),
                    operation: ProCoverOperation::Stop,
                },
                ProCoverChoice {
                    token: 2,
                    label: "Close".into(),
                    operation: ProCoverOperation::Close,
                },
            ]
            .as_slice(),
        );
        panel.alarm.condition = ProAlarmCondition::Disarmed;
        panel.alarm.state_label = "Disarmed".into();
        panel.alarm.modes = ModelRc::from(
            [
                ChoiceRow {
                    token: 0,
                    label: "Disarm".into(),
                    selected: true,
                    disabled: false,
                },
                ChoiceRow {
                    token: 1,
                    label: "Arm away".into(),
                    selected: false,
                    disabled: false,
                },
            ]
            .as_slice(),
        );
        result.push((name, panel));
    }
    result
}
fn setup() -> HomewardWindow {
    let window = create_window().unwrap();
    window.set_show_endpoint(false);
    window.set_access_stage(ProAccessStage::Connected);
    window.set_view_loaded(true);
    window.set_commands_ready(true);
    window.set_connection_status(ConnectionStatus::Live);
    window.set_page(ViewPage::Detail);
    window
}

#[test]
fn the_product_light_detail_contains_one_brightness_control() {
    use i_slint_backend_testing::ElementQuery;
    i_slint_backend_testing::init_no_event_loop();
    let window = setup();
    let (_, panel) = panels().remove(0);
    window.set_device_cards(ModelRc::from([panel.device.clone()].as_slice()));
    window.set_detail_panels(ModelRc::from([panel].as_slice()));
    window.show().unwrap();
    let sliders = ElementQuery::from_root(&window)
        .match_inherits("KitSlider")
        .find_all();
    assert_eq!(
        sliders.len(),
        1,
        "the family detail must not repeat power/level/cohesive light controls"
    );
}

#[test]
#[ignore = "writes optional synthetic UI evidence outside the repository"]
fn render_synthetic_dashboard_families() {
    use i_slint_backend_testing::{TestingBackend, TestingBackendOptions};
    let directory = std::path::PathBuf::from(
        std::env::var_os("HOMEWARD_SYNTHETIC_RENDER_DIR").expect("explicit artifact directory"),
    );
    std::fs::create_dir_all(&directory).unwrap();
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        renderer_name: Some("software".into()),
        ..Default::default()
    })))
    .unwrap();
    let window = setup();
    window.show().unwrap();
    for (screen, width, height) in [("desktop", 1280., 900.), ("phone", 390., 844.)] {
        window
            .window()
            .set_size(slint::LogicalSize::new(width, height));
        for dark in [false, true] {
            window.set_dark_mode(dark);
            for large in [false, true] {
                window.set_large_text(large);
                for (name, panel) in panels() {
                    window.set_media_has_data(false);
                    window.set_device_cards(ModelRc::from([panel.device.clone()].as_slice()));
                    window.set_detail_panels(ModelRc::from([panel].as_slice()));
                    save(
                        &window,
                        &directory.join(format!(
                            "{screen}-{name}-{}-{}.png",
                            if dark { "dark" } else { "light" },
                            if large { 200 } else { 100 }
                        )),
                    );
                }
                window.set_detail_title("Synthetic speaker".into());
                window.set_device_cards(Default::default());
                window.set_media_visible(true);
                window.set_media_has_data(true);
                window.set_media_editable(true);
                window.set_media_data(ProMediaPresentation {
                    token: 0,
                    generation: 1,
                    title: "Quiet afternoon".into(),
                    subtitle: "Synthetic Quartet".into(),
                    collection: "Demo album".into(),
                    playback: "Playing".into(),
                    artwork_state: ProArtworkState::Absent,
                    ..Default::default()
                });
                window.set_media_actions(ModelRc::from(
                    [
                        ProMediaAction {
                            operation: ProMediaOperation::Pause,
                            label: "Pause".into(),
                        },
                        ProMediaAction {
                            operation: ProMediaOperation::Next,
                            label: "Next".into(),
                        },
                    ]
                    .as_slice(),
                ));
                window.set_media_progress_known(true);
                window.set_media_progress(0.4);
                window.set_media_position_label("1:20".into());
                window.set_media_duration_label("3:20".into());
                save(
                    &window,
                    &directory.join(format!(
                        "{screen}-media-{}-{}.png",
                        if dark { "dark" } else { "light" },
                        if large { 200 } else { 100 }
                    )),
                );
            }
        }
    }
}
fn save(window: &HomewardWindow, path: &std::path::Path) {
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(300));
    let pixels = window.window().take_snapshot().unwrap();
    let output = std::fs::File::create(path).unwrap();
    let mut encoder = png::Encoder::new(output, pixels.width(), pixels.height());
    encoder.set_color(png::ColorType::Rgba);
    encoder.set_depth(png::BitDepth::Eight);
    encoder
        .write_header()
        .unwrap()
        .write_image_data(pixels.as_bytes())
        .unwrap();
}

#[test]
#[ignore = "writes optional synthetic scrolled UI evidence outside the repository"]
fn render_synthetic_detail_bottoms() {
    use i_slint_backend_testing::{ElementQuery, TestingBackend, TestingBackendOptions};
    use slint::platform::WindowEvent;
    let directory = std::path::PathBuf::from(
        std::env::var_os("HOMEWARD_SYNTHETIC_RENDER_DIR").expect("explicit artifact directory"),
    );
    std::fs::create_dir_all(&directory).unwrap();
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        renderer_name: Some("software".into()),
        ..Default::default()
    })))
    .unwrap();
    let window = setup();
    window
        .window()
        .set_size(slint::LogicalSize::new(1280., 900.));
    window.on_device_power(|_, _, _| panic!("scroll must not issue a power intention"));
    window.on_device_brightness(|_, _, _| panic!("scroll must not issue a level intention"));
    window.on_device_edit(|_, _, _, _| panic!("scroll must not issue an edit intention"));
    window.on_device_choice(|_, _, _| panic!("scroll must not issue a choice intention"));
    window.on_device_step(|_, _, _| panic!("scroll must not issue a step intention"));
    window.show().unwrap();
    for dark in [false, true] {
        window.set_dark_mode(dark);
        for large in [false, true] {
            window.set_large_text(large);
            for (name, panel) in panels().into_iter().filter(|(name, _)| {
                matches!(*name, "light" | "cover") || (large && matches!(*name, "fan" | "switch"))
            }) {
                window.set_device_cards(ModelRc::from([panel.device.clone()].as_slice()));
                window.set_detail_panels(ModelRc::from([panel].as_slice()));
                i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(300));
                let scroll = ElementQuery::from_root(&window)
                    .match_id("HomewardWindow::detail-scroll")
                    .find_first()
                    .expect("actual product detail scroller");
                let origin = scroll.absolute_position();
                let size = scroll.size();
                // Use the scroller's trailing edge, outside every device
                // control. This is synthetic wheel navigation, never a tap.
                let position = slint::LogicalPosition::new(
                    origin.x + size.width - 2.,
                    origin.y + size.height / 2.,
                );
                for _ in 0..4 {
                    window
                        .window()
                        .dispatch_event(WindowEvent::PointerScrolled {
                            position,
                            delta_x: 0.,
                            delta_y: -4096.,
                        });
                    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(
                        100,
                    ));
                }
                let history = ElementQuery::from_root(&window)
                    .match_inherits("KitPressable")
                    .match_predicate(|e| {
                        e.accessible_label().as_deref() == Some("History and Diagnostics")
                    })
                    .find_first()
                    .expect("product detail footer");
                assert!(history.absolute_position().y >= origin.y - 1.);
                assert!(
                    history.absolute_position().y + history.size().height
                        <= origin.y + size.height + 1.,
                    "the complete footer must be inside the scroller"
                );
                save(
                    &window,
                    &directory.join(format!(
                        "desktop-{name}-{}-{}-bottom.png",
                        if dark { "dark" } else { "light" },
                        if large { 200 } else { 100 }
                    )),
                );
            }
        }
    }
}

#[test]
fn product_history_raster_survives_cohort_pause_and_resume_with_inputs_closed() {
    use i_slint_backend_testing::ElementQuery;
    use slint::platform::PointerEventButton;
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = setup();
    window.set_detail_title("Synthetic history".into());
    window.show().unwrap();
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(1));
    let button = ElementQuery::from_root(&window)
        .match_predicate(|e| e.accessible_label().as_deref() == Some("History and Diagnostics"))
        .find_first()
        .expect("history navigation");
    button.mock_single_click(PointerEventButton::Left);
    let pixels = slint::SharedPixelBuffer::<slint::Rgb8Pixel>::new(4, 4);
    window.set_history_presentation(HistoryPresentation {
        generation: 7,
        phase: ProChartPhase::Ready,
        image: slint::Image::from_rgb8(pixels),
        source_width: 4,
        source_height: 4,
        series: ModelRc::from(
            [ProChartSeries {
                token: 1,
                label: "Synthetic series".into(),
                ..Default::default()
            }]
            .as_slice(),
        ),
        ..Default::default()
    });
    let calls = Rc::new(Cell::new(0));
    let output = calls.clone();
    window.on_history_inspect(move |_, _| output.set(output.get() + 1));
    let raster_count = || {
        ElementQuery::from_root(&window)
            .match_inherits("ChartRaster")
            .find_all()
            .len()
    };
    assert_eq!(raster_count(), 1);
    window.set_cohort_retain_ms(30000);
    window.set_cohort_pending(true);
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(1));
    assert_eq!(raster_count(), 1);
    ElementQuery::from_root(&window)
        .match_inherits("ChartCanvas")
        .find_first()
        .unwrap()
        .mock_single_click(PointerEventButton::Left);
    assert_eq!(calls.get(), 0);
    window.set_cohort_pending(false);
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(1));
    assert_eq!(raster_count(), 1);
}

#[test]
fn dashboard_capture_pause_does_not_close_the_independent_camera_owner() {
    use std::{cell::RefCell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = setup();
    let calls = Rc::new(RefCell::new(Vec::new()));
    let output = calls.clone();
    window.on_cameras_interactive(move |active| output.borrow_mut().push(active));
    window.set_page(ViewPage::Cameras);
    window.show().unwrap();
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(1));
    assert_eq!(calls.borrow().last(), Some(&true));
    calls.borrow_mut().clear();
    window.set_cohort_retain_ms(30000);
    window.set_cohort_pending(true);
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(1));
    assert!(calls.borrow().is_empty());
    window.set_page(ViewPage::Home);
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(1));
    assert_eq!(calls.borrow().last(), Some(&false));
}

#[test]
fn product_media_retains_native_focus_across_cohort_and_reader_revalidation() {
    use i_slint_backend_testing::ElementQuery;
    use slint::platform::{Key, PointerEventButton, WindowEvent};
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = setup();
    window.set_media_visible(true);
    window.set_media_has_data(true);
    window.set_media_editable(true);
    window.set_media_data(ProMediaPresentation {
        token: 4,
        generation: 7,
        title: "Synthetic audio".into(),
        ..Default::default()
    });
    window.set_media_actions(ModelRc::from(
        [ProMediaAction {
            operation: ProMediaOperation::Play,
            label: "Play".into(),
        }]
        .as_slice(),
    ));
    let calls = Rc::new(Cell::new(0));
    let output = calls.clone();
    window.on_media_operation(move |_, _, _| output.set(output.get() + 1));
    window.show().unwrap();
    let play = ElementQuery::from_root(&window)
        .match_inherits("KitPressable")
        .match_predicate(|e| e.accessible_label().as_deref() == Some("Play"))
        .find_first()
        .expect("actual product transport");
    play.mock_single_click(PointerEventButton::Left);
    assert_eq!(calls.get(), 1);
    let space = || {
        window.window().dispatch_event(WindowEvent::KeyPressed {
            text: Key::Space.into(),
        });
        window.window().dispatch_event(WindowEvent::KeyReleased {
            text: Key::Space.into(),
        });
    };
    window.set_cohort_retain_ms(30000);
    window.set_media_retention_ms(30000);
    window.set_cohort_pending(true);
    window.set_media_editable(false);
    slint::platform::update_timers_and_animations();
    assert!(play.is_valid());
    space();
    assert_eq!(calls.get(), 1);
    window.set_cohort_pending(false);
    slint::platform::update_timers_and_animations();
    space();
    assert_eq!(
        calls.get(),
        1,
        "joined dashboard does not reopen a media reader still revalidating"
    );
    window.set_media_retention_ms(0);
    window.set_media_editable(true);
    slint::platform::update_timers_and_animations();
    space();
    assert_eq!(
        calls.get(),
        2,
        "the original native focus survives without a second pointer click"
    );
}

#[test]
fn product_history_query_keeps_its_draft_and_focus_during_capture_pause() {
    use i_slint_backend_testing::ElementQuery;
    use slint::platform::{PointerEventButton, WindowEvent};
    i_slint_backend_testing::init_no_event_loop();
    let window = setup();
    window.show().unwrap();
    ElementQuery::from_root(&window)
        .match_predicate(|e| e.accessible_label().as_deref() == Some("History and Diagnostics"))
        .find_first()
        .unwrap()
        .mock_single_click(PointerEventButton::Left);
    window.set_history_start("draft".into());
    let input = ElementQuery::from_root(&window)
        .match_type_name("TextInput")
        .match_predicate(|e| e.accessible_label().as_deref() == Some("Start UTC (RFC3339)"))
        .find_first()
        .expect("actual product query editor");
    input.mock_single_click(PointerEventButton::Left);
    let type_text = |text: &str| {
        window
            .window()
            .dispatch_event(WindowEvent::KeyPressed { text: text.into() });
        window
            .window()
            .dispatch_event(WindowEvent::KeyReleased { text: text.into() });
    };
    window.set_cohort_retain_ms(30000);
    window.set_cohort_pending(true);
    slint::platform::update_timers_and_animations();
    type_text("x");
    assert_eq!(window.get_history_start(), "draft");
    assert!(input.is_valid());
    window.set_cohort_pending(false);
    slint::platform::update_timers_and_animations();
    type_text("x");
    assert!(
        window.get_history_start().contains('x'),
        "the native query editor keeps focus and caret"
    );
}

#[test]
fn product_dashboard_tile_keeps_focus_when_only_capture_admission_is_closed() {
    use i_slint_backend_testing::ElementQuery;
    use slint::platform::{Key, PointerEventButton, WindowEvent};
    use std::{cell::Cell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = setup();
    window.set_page(ViewPage::Home);
    window.set_has_configured_layout(true);
    window.set_home_layout_nodes(ModelRc::from(
        [ProLayoutNode {
            token: 10,
            generation: 1,
            kind: ProLayoutKind::Tile,
            tile_template: ProTileTemplate::Rectangle,
            tile_rows: 2,
            tile: ProTileRow {
                token: 1,
                generation: 3,
                title: "Synthetic toggle".into(),
                control: ProTileControl::Toggle,
                power: ProSwitchState::Off,
                tap: ProTileTap::Open,
                ..Default::default()
            },
            ..Default::default()
        }]
        .as_slice(),
    ));
    let calls = Rc::new(Cell::new(0));
    let output = calls.clone();
    window.on_device_power(move |_, _, _| output.set(output.get() + 1));
    window.show().unwrap();
    let button = ElementQuery::from_root(&window)
        .match_inherits("KitPressable")
        .match_predicate(|e| e.accessible_label().as_deref() == Some("Synthetic toggle · Turn On"))
        .find_first()
        .expect("product ProTile toggle");
    button.mock_single_click(PointerEventButton::Left);
    assert_eq!(calls.get(), 1);
    let space = || {
        window.window().dispatch_event(WindowEvent::KeyPressed {
            text: Key::Space.into(),
        });
        window.window().dispatch_event(WindowEvent::KeyReleased {
            text: Key::Space.into(),
        });
    };
    window.set_cohort_retain_ms(30000);
    window.set_cohort_pending(true);
    window.set_commands_ready(false);
    slint::platform::update_timers_and_animations();
    space();
    assert_eq!(calls.get(), 1);
    assert!(button.is_valid());
    window.set_cohort_pending(false);
    window.set_commands_ready(true);
    slint::platform::update_timers_and_animations();
    space();
    assert_eq!(
        calls.get(),
        2,
        "the same focused control resumes only for a fresh action"
    );
}
