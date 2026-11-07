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
                    token: 2,
                    label: "Arm Home".into(),
                    selected: false,
                    disabled: false,
                },
                ChoiceRow {
                    token: 1,
                    label: "Arm away".into(),
                    selected: false,
                    disabled: false,
                },
                ChoiceRow {
                    token: 3,
                    label: "Arm Night".into(),
                    selected: false,
                    disabled: false,
                },
            ]
            .as_slice(),
        );
        panel.alarm.mode_icons = ModelRc::from(
            [
                (0, home_wire_contract::home::api::v2::DashboardIcon::Power),
                (1, home_wire_contract::home::api::v2::DashboardIcon::Shield),
                (2, home_wire_contract::home::api::v2::DashboardIcon::Home),
                (3, home_wire_contract::home::api::v2::DashboardIcon::Bed),
            ]
            .map(|(token, icon)| ChoiceIcon {
                token,
                glyph: dashboard_icons::glyph(icon, &Default::default()),
            })
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
fn modal_feedback_stays_below_controls_without_moving_them() {
    use i_slint_backend_testing::{ElementQuery, TestingBackend, TestingBackendOptions};
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        renderer_name: Some("software".into()),
        ..Default::default()
    })))
    .unwrap();
    let window = setup();
    window.show().unwrap();
    let render = |name: &str| {
        if let Some(directory) = std::env::var_os("HOMEWARD_FEEDBACK_RENDER_DIR") {
            let directory = std::path::PathBuf::from(directory);
            std::fs::create_dir_all(&directory).unwrap();
            save(&window, &directory.join(format!("{name}.png")));
        }
    };
    let dialog = |name: &str| {
        let name = name.to_owned();
        ElementQuery::from_root(&window)
            .match_inherits("KitDialogPanel")
            .match_predicate(move |element| {
                element.accessible_label().as_deref() == Some(name.as_str())
            })
            .find_first()
            .expect("named product modal")
    };
    let geometry = |name: &str| {
        slint::platform::update_timers_and_animations();
        let panel = dialog(name);
        let slider = panel
            .query_descendants()
            .match_inherits("KitSlider")
            .find_first()
            .unwrap();
        let origin = slider.absolute_position();
        let size = slider.size();
        let panel_origin = panel.absolute_position();
        let panel_size = panel.size();
        [
            origin.x,
            origin.y,
            size.width,
            size.height,
            panel_origin.x,
            panel_origin.y,
            panel_size.width,
            panel_size.height,
        ]
    };
    let assert_notice = |name: &str, expected: &str, before: [f32; 8]| {
        let after = geometry(name);
        for (before, after) in before.into_iter().zip(after) {
            assert!(
                (before - after).abs() < 0.1,
                "feedback moved a control: {before} -> {after}"
            );
        }
        let notices = dialog(name)
            .query_descendants()
            .match_inherits("KitAlert")
            .find_all();
        assert_eq!(
            notices.len(),
            1,
            "one notice, no global device-progress duplicate"
        );
        assert_eq!(notices[0].accessible_label().as_deref(), Some(expected));
        assert!(
            notices[0].absolute_position().y >= after[1] + after[3],
            "notice follows the slider"
        );
    };
    let scroll = |name: &str, to_bottom: bool| {
        use slint::platform::WindowEvent;
        let area = dialog(name)
            .query_descendants()
            .match_inherits("KitScrollArea")
            .find_first()
            .unwrap();
        let origin = area.absolute_position();
        let size = area.size();
        let position =
            slint::LogicalPosition::new(origin.x + size.width - 2., origin.y + size.height / 2.);
        for _ in 0..3 {
            window
                .window()
                .dispatch_event(WindowEvent::PointerScrolled {
                    position,
                    delta_x: 0.,
                    delta_y: if to_bottom { -4096. } else { 4096. },
                });
            i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(100));
        }
        if to_bottom {
            let notice = dialog(name)
                .query_descendants()
                .match_inherits("KitAlert")
                .find_first()
                .unwrap();
            assert!(
                notice.absolute_position().y >= origin.y - 1.,
                "notice starts inside the viewport"
            );
            assert!(
                notice.absolute_position().y + notice.size().height <= origin.y + size.height + 1.,
                "the complete notice is reachable"
            );
        }
    };
    for (screen, width, height) in [("phone", 390., 844.), ("tablet", 900., 1400.)] {
        for dark in [false, true] {
            for large in [false, true] {
                let screen = format!(
                    "{screen}-{}-{}",
                    if dark { "dark" } else { "light" },
                    if large { 200 } else { 100 }
                );
                window.set_dark_mode(dark);
                window.set_large_text(large);
                window
                    .window()
                    .set_size(slint::LogicalSize::new(width, height));
                window.set_native_popup(DialogState::default());
                window.set_page(ViewPage::Detail);
                window.set_action_feedback("".into());
                let (_, mut panel) = panels().remove(0);
                window.set_device_cards(ModelRc::from([panel.device.clone()]));
                window.set_detail_panels(ModelRc::from([panel.clone()]));
                scroll("Synthetic light", false);
                let before = geometry("Synthetic light");
                // Refusals before submission have no action row and retain their
                // global fallback, now following the device/media module.
                window.set_action_feedback(device_view::Refusal::Capacity.message().into());
                assert_notice("Synthetic light", "Update", before);
                scroll("Synthetic light", true);
                render(&format!("{screen}-detail-refusal"));
                window.set_action_feedback("".into());
                let snapshot = device_view::progress::tests::snapshot(Some(
                    home_wire_contract::home::api::v2::CommandOutcome::AcknowledgedUnverified,
                ));
                let (phase, title, detail) = device_view::progress::presentation(Some(&snapshot));
                panel.device.phase = phase;
                panel.device.action_label = title.into();
                panel.device.action_detail = detail.into();
                window.set_detail_panels(ModelRc::from([panel]));
                scroll("Synthetic light", false);
                assert_notice("Synthetic light", "Change not confirmed", before);
                scroll("Synthetic light", true);
                render(&format!("{screen}-detail-unconfirmed"));

                window.set_page(ViewPage::Home);
                window.set_native_popup_title("Synthetic room".into());
                window.set_native_popup_generation(1);
                window.set_popup_layout_nodes(ModelRc::from([ProLayoutNode {
                    token: 10,
                    generation: 1,
                    kind: ProLayoutKind::Tile,
                    tile_template: ProTileTemplate::Rectangle,
                    tile_rows: 2,
                    tile: ProTileRow {
                        token: 0,
                        generation: 1,
                        title: "Synthetic dimmer".into(),
                        control: ProTileControl::Level,
                        level: 70.,
                        level_known: true,
                        level_minimum: 0.,
                        level_maximum: 100.,
                        level_increment: 1.,
                        ..Default::default()
                    },
                    ..Default::default()
                }]));
                window.set_native_popup(DialogState {
                    open: true,
                    topmost: true,
                    ..Default::default()
                });
                scroll("Synthetic room", false);
                let before = geometry("Synthetic room");
                window.set_action_feedback(device_view::Refusal::Capacity.message().into());
                assert_notice("Synthetic room", "Update", before);
                scroll("Synthetic room", true);
                render(&format!("{screen}-room-refusal"));
            }
        }
    }
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
                let mut variants = panels();
                let mut close_only = variants
                    .iter()
                    .find(|(name, _)| *name == "cover")
                    .unwrap()
                    .1
                    .clone();
                close_only.device.title = "Synthetic closed cover".into();
                close_only.cover.state_label = "Closed · 0 %".into();
                close_only.cover.position = 0.;
                close_only.positioned = false;
                close_only.cover.operations = ModelRc::from([ProCoverChoice {
                    token: 2,
                    label: "Close".into(),
                    operation: ProCoverOperation::Close,
                }]);
                variants.push(("cover-close-only", close_only));
                let lock = device_view::presentation::CardRow {
                    title: "Synthetic remote controls".into(),
                    token: 0,
                    generation: 1,
                    kind: ProDeviceKind::Lock,
                    value: "Unlocked".into(),
                    quality: "Current observation".into(),
                    access: ProControlAccess::Allowed,
                    lock_operations: vec![
                        ProLockChoice {
                            token: 0,
                            label: "Lock".into(),
                            operation: ProLockOperation::Lock,
                        },
                        ProLockChoice {
                            token: 1,
                            label: "Unlock".into(),
                            operation: ProLockOperation::Unlock,
                        },
                    ],
                    ..Default::default()
                };
                variants.push((
                    "lock",
                    device_view::detail::project(&lock, lock.ui(None, &Default::default()), None),
                ));
                for (name, panel) in variants {
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

#[test]
fn alarm_detail_has_one_close_and_preserves_mode_identity() {
    use i_slint_backend_testing::ElementQuery;
    use slint::platform::PointerEventButton;
    use std::{cell::RefCell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = setup();
    let (_, panel) = panels()
        .into_iter()
        .find(|(name, _)| *name == "alarm")
        .unwrap();
    window.set_device_cards(ModelRc::from([panel.device.clone()].as_slice()));
    window.set_detail_panels(ModelRc::from([panel].as_slice()));
    let calls = Rc::new(RefCell::new(Vec::new()));
    let output = calls.clone();
    window.on_device_choice(move |token, generation, choice| {
        output.borrow_mut().push((token, generation, choice))
    });
    window.show().unwrap();
    let back = ElementQuery::from_root(&window)
        .match_id("HomewardWindow::detail-back")
        .find_all();
    assert!(
        back.is_empty(),
        "the specialized panel owns its close button"
    );
    let modes = ElementQuery::from_root(&window)
        .match_inherits("KitPressable")
        .match_predicate(|item| item.accessible_label().as_deref() == Some("Arm away"))
        .find_all();
    assert_eq!(modes.len(), 1);
    modes[0].mock_single_click(PointerEventButton::Left);
    assert_eq!(&*calls.borrow(), &[(0, 1, 1)]);
    window.set_commands_ready(false);
    slint::platform::update_timers_and_animations();
    modes[0].mock_single_click(PointerEventButton::Left);
    assert_eq!(calls.borrow().len(), 1, "command suspension stays closed");
    let dismissals = Rc::new(std::cell::Cell::new(0));
    let output = dismissals.clone();
    window.on_go_back(move || output.set(output.get() + 1));
    window.invoke_take_body_focus(BodyFocus::Back);
    slint::platform::update_timers_and_animations();
    window
        .window()
        .dispatch_event(slint::platform::WindowEvent::KeyPressed {
            text: slint::platform::Key::Return.into(),
        });
    window
        .window()
        .dispatch_event(slint::platform::WindowEvent::KeyReleased {
            text: slint::platform::Key::Return.into(),
        });
    assert_eq!(
        dismissals.get(),
        1,
        "initial detail focus reaches the remaining Close control"
    );
}
