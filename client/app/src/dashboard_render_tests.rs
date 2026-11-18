//! Synthetic product renders: no profile, session, transport or device command.
use crate::*;
use slint::{ComponentHandle, Model, ModelRc};

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
        panel.climate.activity = "Heating".into();
        if kind == ProDeviceKind::Climate {
            panel.has_climate_editor = true;
            panel.climate_field = ProDeviceValue {
                token: 0,
                kind: ProValueKind::Numeric,
                mode: ProValueMode::Single,
                label: "Setpoint".into(),
                lower: "21.5".into(),
                suffix: "°C".into(),
                lower_position: 0.55,
                slider_enabled: true,
                can_step: true,
                ..Default::default()
            };
            panel.device.value = "20.0".into();
            panel.device.unit = "°C".into();
        }
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
                    label: "High".into(),
                    selected: false,
                    disabled: false,
                },
                ChoiceRow {
                    token: 1,
                    label: "Medium".into(),
                    selected: true,
                    disabled: false,
                },
                ChoiceRow {
                    token: 2,
                    label: "Low".into(),
                    selected: false,
                    disabled: false,
                },
            ]
            .as_slice(),
        );
        if kind == ProDeviceKind::Fan {
            panel.fan.state_label = "Medium".into();
        }
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
        if kind == ProDeviceKind::Cover {
            panel.associated_lock = ProAssociatedLock {
                token: 1,
                generation: 7,
                title: "Remote controls".into(),
                state_label: "Unlocked".into(),
                state: ProCoverLock::Unlocked,
                can_lock: true,
                can_unlock: true,
            };
            panel.lock_commands_enabled = true;
        }
        panel.alarm_code_required = kind == ProDeviceKind::Alarm;
        panel.alarm.code_length = 4;
        panel.alarm.code_capacity = 32;
        panel.alarm.entered_count = 3;
        panel.alarm.code_label = "3 digits entered".into();
        panel.alarm.prompt = "Code to disarm".into();
        panel.alarm.confirm_label = "Disarm".into();
        panel.alarm.can_confirm = true;
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
fn current_refusals_are_visible_and_history_stays_below_controls() {
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
        if name == "Synthetic light" {
            return ElementQuery::from_root(&window)
                .match_id("HomewardWindow::detail-scroll")
                .find_first()
                .unwrap();
        }
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
    let scroll_area = |name: &str| {
        if name == "Synthetic light" {
            dialog(name)
        } else {
            dialog(name)
                .query_descendants()
                .match_inherits("KitScrollArea")
                .find_first()
                .unwrap()
        }
    };
    let scroll = |name: &str, to_bottom: bool| {
        use slint::platform::WindowEvent;
        let area = scroll_area(name);
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
    };
    let assert_notice = |name: &str, expected: &str, before: [f32; 8]| {
        let after = geometry(name);
        // Content-sized desktop surfaces may grow and recenter. Controls keep
        // their dimensions and offset within the surface.
        let relative = |g: [f32; 8]| [g[0] - g[4], g[1] - g[5], g[2], g[3], g[6]];
        for (before, after) in relative(before).into_iter().zip(relative(after)) {
            assert!(
                (before - after).abs() < 0.1,
                "feedback moved a control: {before} -> {after}"
            );
        }
        let slider = dialog(name)
            .query_descendants()
            .match_inherits("KitSlider")
            .find_first()
            .unwrap();
        // TestingBackend excludes clipped descendants. Reveal the real footer
        // before querying it, while retaining the control handle for geometry.
        scroll(name, true);
        let notices = dialog(name)
            .query_descendants()
            .match_inherits("KitAlert")
            .find_all();
        assert_eq!(
            notices.len(),
            1,
            "one notice for {name}/{expected}, no global device-progress duplicate"
        );
        assert_eq!(notices[0].accessible_label().as_deref(), Some(expected));
        assert!(
            notices[0].absolute_position().y >= slider.absolute_position().y + slider.size().height,
            "notice follows the slider"
        );
        let area = scroll_area(name);
        let origin = area.absolute_position();
        let size = area.size();
        assert!(
            notices[0].absolute_position().y >= origin.y - 1.,
            "notice starts inside viewport"
        );
        assert!(
            notices[0].absolute_position().y + notices[0].size().height
                <= origin.y + size.height + 1.,
            "complete notice is reachable"
        );
    };
    let assert_current = |expected: &str, width: f32, height: f32| {
        slint::platform::update_timers_and_animations();
        let notices = ElementQuery::from_root(&window)
            .match_inherits("KitAlert")
            .find_all();
        assert_eq!(
            notices.len(),
            1,
            "current refusal is visible without scrolling"
        );
        assert_eq!(notices[0].accessible_label().as_deref(), Some(expected));
        let point = notices[0].absolute_position();
        let size = notices[0].size();
        assert!(
            point.x >= 0.
                && point.y >= 0.
                && point.x + size.width <= width + 1.
                && point.y + size.height <= height + 1.,
            "current refusal is entirely inside the viewport"
        );
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
                // A current refusal is immediately visible and announced;
                // historical command detail remains explicitly consulted.
                window.set_action_feedback(device_view::Refusal::Capacity.message().into());
                render(&format!("{screen}-detail-refusal-before-assert"));
                assert_current(device_view::Refusal::Capacity.message(), width, height);
                render(&format!("{screen}-detail-refusal"));
                window.set_action_feedback("".into());
                // An unverified acknowledgement is silent: no notice, no
                // disclosure control. Only a refusal leaves a notice, and it
                // never moves the control.
                let unverified = device_view::progress::tests::snapshot(Some(
                    home_wire_contract::home::api::v2::CommandOutcome::AcknowledgedUnverified,
                ));
                let (phase, title, detail) = device_view::progress::presentation(Some(&unverified));
                assert_eq!(phase, crate::ProActionPhase::Idle);
                assert!(title.is_empty() && detail.is_empty());
                let mut refused = device_view::progress::tests::snapshot(None);
                refused.operation = None;
                refused.submission =
                    home_client_actions::capabilities::CapabilitySubmission::Rejected;
                refused.tracking =
                    home_client_actions::capabilities::CapabilityTracking::Unavailable;
                let (phase, title, detail) = device_view::progress::presentation(Some(&refused));
                panel.device.phase = phase;
                panel.device.action_label = title.into();
                panel.device.action_detail = detail.into();
                window.set_detail_panels(ModelRc::from([panel]));
                scroll("Synthetic light", true);
                assert!(
                    ElementQuery::from_root(&window)
                        .match_inherits("KitPressable")
                        .match_predicate(|item| {
                            item.accessible_label().as_deref() == Some("Last command")
                                || item.accessible_label().as_deref() == Some("Stop Tracking")
                        })
                        .find_first()
                        .is_none(),
                    "no tracking controls"
                );
                scroll("Synthetic light", false);
                render(&format!("{screen}-detail-refused-before-assert"));
                assert_notice("Synthetic light", "Change refused", before);
                scroll("Synthetic light", true);
                render(&format!("{screen}-detail-refused"));

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
                window.set_action_feedback(device_view::Refusal::Capacity.message().into());
                render(&format!("{screen}-room-refusal-before-assert"));
                assert_current(device_view::Refusal::Capacity.message(), width, height);
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
    for (screen, width, height) in [
        ("desktop", 1280., 900.),
        ("phone", 390., 844.),
        ("phone-small", 360., 740.),
    ] {
        window
            .window()
            .set_size(slint::LogicalSize::new(width, height));
        for dark in [false, true] {
            window.set_dark_mode(dark);
            for large in [false, true] {
                window.set_large_text(large);
                let mut variants = panels();
                for (name, count) in [
                    ("alarm-code-4", 4),
                    ("alarm-code-6", 6),
                    ("alarm-code-32", 32),
                ] {
                    let mut panel = variants
                        .iter()
                        .find(|(name, _)| *name == "alarm")
                        .unwrap()
                        .1
                        .clone();
                    panel.alarm.entered_count = count;
                    panel.alarm.code_length = count.max(4);
                    panel.alarm.code_label = format!("{count} digits entered").into();
                    variants.push((name, panel));
                }
                let mut white = variants
                    .iter()
                    .find(|(name, _)| *name == "light")
                    .unwrap()
                    .1
                    .clone();
                white.has_temperature = true;
                white.temperature_field = ProDeviceValue {
                    token: 1,
                    kind: ProValueKind::Numeric,
                    mode: ProValueMode::Single,
                    label: "Color temperature".into(),
                    lower: "3000".into(),
                    suffix: "K".into(),
                    lower_position: 0.55,
                    slider_enabled: true,
                    can_step: true,
                    ..Default::default()
                };
                variants.push(("light-white", white));
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
                    if name == "light-white" {
                        i_slint_backend_testing::ElementQuery::from_root(&window)
                            .match_predicate(|item| {
                                item.accessible_label().as_deref() == Some("White")
                            })
                            .find_first()
                            .expect("white section")
                            .invoke_accessible_default_action();
                    }
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
                            operation: ProMediaOperation::Previous,
                            label: "Previous".into(),
                        },
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
                window.set_media_values(ModelRc::from([ProDeviceValue {
                    token: 0,
                    kind: ProValueKind::Numeric,
                    mode: ProValueMode::Single,
                    label: "Volume".into(),
                    lower: "35".into(),
                    suffix: "%".into(),
                    lower_position: 0.35,
                    slider_enabled: true,
                    can_step: true,
                    ..Default::default()
                }]));
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
                window.set_page(ViewPage::Home);
                window.set_native_popup_title("Synthetic room thermostat".into());
                window.set_native_popup_generation(3);
                window.set_native_popup_detail(true);
                window.set_native_popup_can_back(true);
                window.set_popup_layout_nodes(ModelRc::from([synthetic_thermostat_popup()]));
                window.set_native_popup(DialogState {
                    open: true,
                    topmost: true,
                    ..Default::default()
                });
                save(
                    &window,
                    &directory.join(format!(
                        "{screen}-room-thermostat-{}-{}.png",
                        if dark { "dark" } else { "light" },
                        if large { 200 } else { 100 }
                    )),
                );
                window.set_native_popup(DialogState::default());
                window.set_page(ViewPage::Detail);
            }
        }
    }
}
fn save(window: &HomewardWindow, path: &std::path::Path) {
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(300));
    let pixels = window.window().take_snapshot().unwrap();
    write_png(path, pixels.width(), pixels.height(), pixels.as_bytes());
}
fn write_png(path: &std::path::Path, width: u32, height: u32, bytes: &[u8]) {
    let output = std::fs::File::create(path).unwrap();
    let mut encoder = png::Encoder::new(output, width, height);
    encoder.set_color(png::ColorType::Rgba);
    encoder.set_depth(png::BitDepth::Eight);
    encoder
        .write_header()
        .unwrap()
        .write_image_data(bytes)
        .unwrap();
}

// ---------------------------------------------------------------------------
// Mockup parity: one capture per design board, drawn from one synthetic home.
// Every board is the product window at the board's pixel size; the home holds
// only data the server projects (rooms, equipment, weather, agenda, lists).

mod parity {
    use super::*;
    use crate::dashboard_presentation::{Appearance, synthetic_tile};
    use crate::device_view::{detail::Metadata, presentation::CardRow};
    use home_wire_contract::home::api::v2 as wire;

    pub(super) fn glyph(icon: wire::DashboardIcon) -> IconGlyph {
        dashboard_icons::glyph(icon, &IconGlyph::default())
    }
    fn card(token: i32, kind: ProDeviceKind, title: &str, value: &str, on: bool) -> CardRow {
        CardRow {
            token,
            generation: 1,
            kind,
            title: title.into(),
            value: value.into(),
            quality: "Current observation".into(),
            access: ProControlAccess::Allowed,
            power_known: true,
            power_on: on,
            can_power: true,
            brightness_minimum: 0.,
            brightness_maximum: 100.,
            brightness_step: 1.,
            ..Default::default()
        }
    }
    /// A dimmable lamp at `percent` (off when zero), coloured when `color` is
    /// observed.
    pub(super) fn lamp(
        token: i32,
        title: &str,
        percent: f32,
        color: Option<(u8, u8, u8)>,
    ) -> CardRow {
        let on = percent > 0.;
        let mut row = card(
            token,
            ProDeviceKind::Light,
            title,
            if on { "On" } else { "Off" },
            on,
        );
        row.can_brightness = true;
        row.brightness_known = true;
        row.brightness_percent = percent;
        if let Some(color) = color {
            row.detail = Metadata::default().with_light_color(color);
        }
        row
    }
    pub(super) fn switch(token: i32, title: &str, on: bool) -> CardRow {
        card(
            token,
            ProDeviceKind::Switch,
            title,
            if on { "On" } else { "Off" },
            on,
        )
    }
    pub(super) fn fan(token: i32, title: &str, speed: &str) -> CardRow {
        let on = speed != "Off";
        let mut row = card(token, ProDeviceKind::Fan, title, speed, on);
        // Fastest first, as the panel stacks the speed columns top-down.
        row.choices = ["High", "Medium", "Low"]
            .into_iter()
            .enumerate()
            .map(|(index, label)| ProDeviceChoice {
                token: index as i32,
                label: label.into(),
                selected: label == speed,
            })
            .collect();
        row.detail = Metadata::default().with_speeds(&[
            ("High", speed == "High"),
            ("Medium", speed == "Medium"),
            ("Low", speed == "Low"),
        ]);
        row
    }
    pub(super) fn thermostat(token: i32, title: &str, current: &str, setpoint: &str) -> CardRow {
        let mut row = card(token, ProDeviceKind::Climate, title, current, false);
        row.unit = "°C".into();
        row.can_power = false;
        row.values = vec![ProDeviceValue {
            token: 0,
            kind: ProValueKind::Numeric,
            mode: ProValueMode::Single,
            label: "Setpoint".into(),
            lower: setpoint.into(),
            suffix: "°C".into(),
            lower_position: 0.55,
            slider_enabled: true,
            can_step: true,
            ..Default::default()
        }];
        row.field_keys.insert("setpoint_celsius", 0);
        row.detail = Metadata::default().with_climate(
            wire::TemperatureActivity::Heating,
            &[
                wire::TemperatureMode::Off,
                wire::TemperatureMode::Heat,
                wire::TemperatureMode::Cool,
                wire::TemperatureMode::Auto,
            ],
            wire::TemperatureMode::Heat,
        );
        row
    }
    pub(super) fn garage(token: i32, title: &str) -> CardRow {
        let mut row = card(token, ProDeviceKind::Cover, title, "Closed", false);
        row.can_power = false;
        row.cover_operations = vec![
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
        ];
        row.detail = Metadata::default().with_cover(0., (0., 100.), ProCoverMotion::Idle);
        row
    }
    pub(super) fn lock(token: i32, title: &str) -> CardRow {
        let mut row = card(token, ProDeviceKind::Lock, title, "Locked", false);
        row.can_power = false;
        row.lock_operations = vec![
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
        ];
        row
    }
    pub(super) fn alarm(token: i32, condition: ProAlarmCondition) -> CardRow {
        let label = match condition {
            ProAlarmCondition::Disarmed => "Disarmed",
            ProAlarmCondition::Armed => "Armed",
            ProAlarmCondition::Arming => "Arming",
            ProAlarmCondition::Pending => "Pending",
            ProAlarmCondition::Triggered => "Triggered",
            ProAlarmCondition::Unknown => "Unknown",
        };
        let mut row = card(token, ProDeviceKind::Alarm, "Alarm", label, false);
        row.can_power = false;
        row.alarm_operations = [
            ("Arm Home", ProAlarmOperation::ArmHome),
            ("Arm Away", ProAlarmOperation::ArmAway),
            ("Arm Night", ProAlarmOperation::ArmNight),
            ("Disarm", ProAlarmOperation::Disarm),
        ]
        .into_iter()
        .enumerate()
        .map(|(index, (label, operation))| ProAlarmChoice {
            token: index as i32,
            label: label.into(),
            operation,
        })
        .collect();
        row.detail = Metadata::default().with_alarm(condition);
        row
    }
    pub(super) fn player(token: i32, title: &str, place: &str, playing: bool) -> CardRow {
        let mut row = card(
            token,
            ProDeviceKind::Media,
            title,
            if playing { "Playing" } else { "Paused" },
            playing,
        );
        row.quality = place.into();
        row
    }
    pub(super) fn scene(token: i32, title: &str, place: &str, active: bool) -> CardRow {
        // A scene reports no value of its own; the place is its only subtitle.
        let mut row = card(token, ProDeviceKind::Scene, title, "", active);
        row.quality = place.into();
        row
    }
    pub(super) fn sensor(token: i32, title: &str, value: &str, unit: &str) -> CardRow {
        let mut row = card(token, ProDeviceKind::Measurement, title, value, false);
        row.unit = unit.into();
        row.power_known = false;
        row.can_power = false;
        row
    }
    pub(super) fn device(card: &CardRow, icon: wire::DashboardIcon) -> ProDeviceRow {
        card.ui(None, &glyph(icon))
    }
    /// The detail panel of a synthetic card, as the product projects it.
    pub(super) fn detail(card: &CardRow, icon: wire::DashboardIcon) -> HomewardDetailRow {
        let mut panel = device_view::detail::project(card, device(card, icon), None);
        match card.kind {
            ProDeviceKind::Climate => {
                panel.climate.current = format!("{} {}", card.value, card.unit).into();
            }
            ProDeviceKind::Fan => {
                panel.fan.state_label = card.value.clone();
            }
            ProDeviceKind::Alarm => {
                panel.alarm_code_required = true;
                panel.alarm.code_length = 4;
                panel.alarm.code_capacity = 32;
                panel.alarm.entered_count = 0;
                panel.alarm.code_label = "0 digits entered".into();
                panel.alarm.prompt = "Code to arm away".into();
                panel.alarm.confirm_label = "Arm Away".into();
                panel.alarm.can_confirm = false;
                panel.alarm.state_label = card.value.clone();
                panel.alarm.modes = ModelRc::from(
                    [
                        ("Disarm", true),
                        ("Arm Home", false),
                        ("Arm Away", false),
                        ("Arm Night", false),
                    ]
                    .into_iter()
                    .enumerate()
                    .map(|(index, (label, selected))| ChoiceRow {
                        token: index as i32,
                        label: label.into(),
                        selected,
                        disabled: false,
                    })
                    .collect::<Vec<_>>()
                    .as_slice(),
                );
                panel.alarm.mode_icons = ModelRc::from(
                    [
                        wire::DashboardIcon::Power,
                        wire::DashboardIcon::Home,
                        wire::DashboardIcon::Shield,
                        wire::DashboardIcon::Bed,
                    ]
                    .into_iter()
                    .enumerate()
                    .map(|(index, icon)| ChoiceIcon {
                        token: index as i32,
                        glyph: glyph(icon),
                    })
                    .collect::<Vec<_>>()
                    .as_slice(),
                );
            }
            ProDeviceKind::Cover => {
                panel.cover.state_label = "Closed".into();
                panel.associated_lock = ProAssociatedLock {
                    token: card.token + 1,
                    generation: 1,
                    title: "Remote controls".into(),
                    state_label: "Locked".into(),
                    state: ProCoverLock::Locked,
                    can_lock: true,
                    can_unlock: true,
                };
                panel.lock_commands_enabled = true;
            }
            ProDeviceKind::Light
            | ProDeviceKind::Switch
            | ProDeviceKind::Binary
            | ProDeviceKind::Measurement
            | ProDeviceKind::Helper
            | ProDeviceKind::Lock
            | ProDeviceKind::Observation
            | ProDeviceKind::Unsupported
            | ProDeviceKind::Media
            | ProDeviceKind::Scene => {}
        }
        panel
    }

    /// A flat layout model under construction; children refer to indices.
    pub(super) struct Board {
        pub nodes: Vec<ProLayoutNode>,
        pub appearance: Appearance,
    }
    /// One child placement: the node, its span in twelfths, and its grid rows.
    pub(super) struct Place {
        pub index: i32,
        pub span: i32,
        pub rows: i32,
    }
    pub(super) fn place(index: i32, span: i32, rows: i32) -> Place {
        Place { index, span, rows }
    }
    fn input(place: &Place) -> ProLayoutInput {
        ProLayoutInput {
            index: place.index,
            column_span: if place.span > 0 {
                ProLayoutSpan::Count
            } else {
                ProLayoutSpan::Automatic
            },
            columns: place.span.max(0),
            rows: place.rows.max(1),
            ..Default::default()
        }
    }
    impl Board {
        pub fn new(appearance: Appearance) -> Self {
            Self {
                nodes: Vec::new(),
                appearance,
            }
        }
        fn push(&mut self, mut node: ProLayoutNode) -> i32 {
            let index = self.nodes.len() as i32;
            node.token = 100_000 + index;
            node.generation = 1;
            self.nodes.push(node);
            index
        }
        pub fn tile(
            &mut self,
            card: &CardRow,
            icon: wire::DashboardIcon,
            template: ProTileTemplate,
            rows: i32,
        ) -> i32 {
            let device = device(card, icon);
            let tile = synthetic_tile(card, &device, template, self.appearance);
            self.push(ProLayoutNode {
                kind: ProLayoutKind::Tile,
                flow: ProLayoutFlow::Vertical,
                device,
                tile,
                tile_template: template,
                tile_rows: rows,
                ..Default::default()
            })
        }
        pub fn widget(&mut self, title: &str, widget: ProDashboardWidget) -> i32 {
            self.push(ProLayoutNode {
                kind: ProLayoutKind::Widget,
                flow: ProLayoutFlow::Vertical,
                title: title.into(),
                widget,
                ..Default::default()
            })
        }
        /// A twelve-column section: titled with a section heading, or untitled.
        /// A child spanning half the section or more takes the whole width on
        /// a phone (under 600 px); narrower children keep their span.
        pub fn section(
            &mut self,
            title: &str,
            action: &str,
            columns: i32,
            minimum_column_width: f32,
            children: &[Place],
        ) -> i32 {
            let titled = !title.is_empty();
            let children = children
                .iter()
                .flat_map(|child| {
                    [
                        ProLayoutInput {
                            viewport_min_width: 600,
                            ..input(child)
                        },
                        ProLayoutInput {
                            viewport_max_width: 599,
                            ..input(&place(
                                child.index,
                                if child.span >= 6 { 12 } else { child.span },
                                child.rows,
                            ))
                        },
                    ]
                })
                .collect::<Vec<_>>();
            self.push(ProLayoutNode {
                kind: ProLayoutKind::Container,
                flow: ProLayoutFlow::Grid,
                title: title.into(),
                heading: if titled {
                    ProLayoutHeading::Section
                } else {
                    ProLayoutHeading::Normal
                },
                heading_action: action.into(),
                columns,
                column_policy: ProLayoutColumns::Fixed,
                span_base: 12,
                minimum_column_width,
                gap: 8.,
                section_gap: if titled { 24. } else { 0. },
                metrics_policy: ProLayoutMetricsPolicy::Configured,
                children: ModelRc::from(children.as_slice()),
                ..Default::default()
            })
        }
        /// The board root: sections placed with a wide span (600 px and up)
        /// and a phone span below it.
        pub fn root(&mut self, gap: f32, sections: &[(i32, i32, i32, i32)]) {
            let children = sections
                .iter()
                .flat_map(|(index, wide, phone, rows)| {
                    [
                        ProLayoutInput {
                            viewport_min_width: 600,
                            ..input(&place(*index, *wide, *rows))
                        },
                        ProLayoutInput {
                            viewport_max_width: 599,
                            ..input(&place(*index, *phone, *rows))
                        },
                    ]
                })
                .collect::<Vec<_>>();
            let root = ProLayoutNode {
                kind: ProLayoutKind::Container,
                flow: ProLayoutFlow::Grid,
                columns: 12,
                column_policy: ProLayoutColumns::Fixed,
                span_base: 12,
                minimum_column_width: 16.,
                gap,
                metrics_policy: ProLayoutMetricsPolicy::Configured,
                children: ModelRc::from(children.as_slice()),
                ..Default::default()
            };
            // The root must be the first node: the product reads node 0.
            self.nodes.insert(0, root);
            for (index, node) in self.nodes.iter_mut().enumerate() {
                node.token = 100_000 + index as i32;
            }
            // Children indices shift by one once the root leads the model.
            for node in self.nodes.iter_mut().skip(1) {
                let shifted = node
                    .children
                    .iter()
                    .map(|mut child| {
                        child.index += 1;
                        child
                    })
                    .collect::<Vec<_>>();
                node.children = ModelRc::from(shifted.as_slice());
            }
            let shifted = self.nodes[0]
                .children
                .iter()
                .map(|mut child| {
                    child.index += 1;
                    child
                })
                .collect::<Vec<_>>();
            self.nodes[0].children = ModelRc::from(shifted.as_slice());
        }
        pub fn model(&self) -> ModelRc<ProLayoutNode> {
            ModelRc::from(self.nodes.as_slice())
        }
    }

    fn room(
        token: i32,
        title: &str,
        subtitle: &str,
        icon: wire::DashboardIcon,
        value: &str,
        active: bool,
    ) -> ProDashboardWidget {
        ProDashboardWidget {
            kind: ProDashboardWidgetKind::Room,
            room: ProRoomRow {
                token,
                generation: 1,
                title: title.into(),
                subtitle: subtitle.into(),
                glyph: glyph(icon),
                value: value.into(),
                active,
                tone: Tone::Warning,
            },
            value: value.into(),
            active,
            data_ready: true,
            can_open: true,
            ..Default::default()
        }
    }
    fn observation(
        token: i32,
        title: &str,
        value: &str,
        icon: wire::DashboardIcon,
        active: bool,
        tone: Tone,
    ) -> ProObservationRow {
        ProObservationRow {
            token,
            generation: 1,
            title: title.into(),
            value: value.into(),
            description: "".into(),
            glyph: glyph(icon),
            available: true,
            active,
            tone,
        }
    }
    fn task(token: i32, title: &str, detail: &str, quantity: &str, completed: bool) -> ProTaskRow {
        ProTaskRow {
            token,
            generation: 1,
            title: title.into(),
            detail: detail.into(),
            completed,
            writable: true,
            phase: ProTaskPhase::Ready,
            quantity: quantity.into(),
            ..Default::default()
        }
    }
    fn agenda(
        token: i32,
        day: i32,
        day_label: &str,
        time_label: &str,
        title: &str,
        detail: &str,
        kind: ProAgendaTemporalKind,
    ) -> ProAgendaRow {
        ProAgendaRow {
            token,
            generation: 1,
            date: CivilDateView {
                year: 2026,
                month: if day > 20 { 10 } else { 11 },
                day,
            },
            day_label: day_label.into(),
            time_label: time_label.into(),
            title: title.into(),
            detail: detail.into(),
            status: "".into(),
            kind,
            phase: ProAgendaPhase::Ready,
        }
    }
    fn weather() -> ProDashboardWidget {
        let forecast = [
            ("Sun", "Sunny", "8° / 1°"),
            ("Mon", "Cloudy", "5° / -1°"),
            ("Tue", "Rain", "7° / 2°"),
            ("Wed", "Sunny", "10° / 3°"),
            ("Thu", "Cloudy", "3° / -3°"),
        ]
        .into_iter()
        .map(|(period, condition, temperature)| ProForecastRow {
            period: period.into(),
            condition: condition.into(),
            temperature: temperature.into(),
            precipitation: "".into(),
        })
        .collect::<Vec<_>>();
        let facts = [("Feels like", "3°"), ("Max", "9°"), ("Min", "2°")]
            .into_iter()
            .map(|(label, value)| ProEnvironmentFact {
                label: label.into(),
                value: value.into(),
                detail: "".into(),
            })
            .collect::<Vec<_>>();
        ProDashboardWidget {
            kind: ProDashboardWidgetKind::ClockWeather,
            clock: "19:42".into(),
            date: "Saturday 31 October".into(),
            has_weather: true,
            weather: ProWeatherData {
                temperature: "6°".into(),
                condition: "Cloudy".into(),
                facts: ModelRc::from(facts.as_slice()),
                forecast_heading: "Next days".into(),
                forecast: ModelRc::from(forecast.as_slice()),
                freshness: "Observed 19:40".into(),
                ..Default::default()
            },
            data_ready: true,
            ..Default::default()
        }
    }
    fn thermostat_widget(card: &CardRow, appearance: Appearance) -> ProDashboardWidget {
        let device = device(card, wire::DashboardIcon::Thermostat);
        let paint = appearance.heating();
        ProDashboardWidget {
            kind: ProDashboardWidgetKind::Thermostat,
            field: device.values.row_data(0).unwrap_or_default(),
            thermostat: device,
            detailed: false,
            active: true,
            tint: paint.tint,
            ink: paint.ink,
            status: "Heating".into(),
            data_ready: true,
            can_open: true,
            ..Default::default()
        }
    }
    fn groceries(token_base: i32) -> ProDashboardWidget {
        let sections = [
            (
                "Walmart",
                (0x3b, 0x82, 0xf6),
                "4",
                vec![
                    ("Milk 2 %", "2 L", ""),
                    ("Eggs", "", "×2"),
                    ("Flour", "", "1/1"),
                ],
            ),
            (
                "Costco",
                (0xef, 0x44, 0x44),
                "5",
                vec![("Whole chicken", "", "×2"), ("Ground beef", "2 kg", "")],
            ),
        ]
        .into_iter()
        .enumerate()
        .map(
            |(section, (label, (r, g, b), count, rows))| ProGrocerySection {
                label: label.into(),
                color: slint::Color::from_rgb_u8(r, g, b),
                count_label: count.into(),
                rows: ModelRc::from(
                    rows.into_iter()
                        .enumerate()
                        .map(|(index, (title, detail, quantity))| {
                            task(
                                token_base + section as i32 * 10 + index as i32,
                                title,
                                detail,
                                quantity,
                                false,
                            )
                        })
                        .collect::<Vec<_>>()
                        .as_slice(),
                ),
            },
        )
        .collect::<Vec<_>>();
        ProDashboardWidget {
            kind: ProDashboardWidgetKind::Tasks,
            shopping_only: true,
            sections: ModelRc::from(sections.as_slice()),
            task_access: ProTaskAccess::Editable,
            status: "Walmart + Costco".into(),
            action_label: "Set up".into(),
            action_glyph: glyph(wire::DashboardIcon::Shopping),
            overflow_label: "+4 more".into(),
            data_ready: true,
            can_open: true,
            ..Default::default()
        }
    }

    /// The home board: overview, rooms, agenda, lists, security and media.
    pub(super) fn home(appearance: Appearance) -> Board {
        use wire::DashboardIcon as I;
        let mut board = Board::new(appearance);
        let chips = board.widget(
            "",
            ProDashboardWidget {
                kind: ProDashboardWidgetKind::Observations,
                observations: ModelRc::from(
                    [
                        observation(1, "Alarm", "Disarmed", I::Shield, true, Tone::Success),
                        observation(
                            2,
                            "Weather",
                            "6° · Cloudy",
                            I::Sun,
                            false,
                            Tone::Information,
                        ),
                        observation(3, "Garage", "Closed", I::Garage, false, Tone::Neutral),
                        observation(4, "Leak", "None", I::Water, false, Tone::Success),
                    ]
                    .as_slice(),
                ),
                item_limit: 6,
                data_ready: true,
                ..Default::default()
            },
        );
        let clock = board.widget("", weather());
        let heating = thermostat(10, "Thermostat", "21.5", "21.0");
        let thermostat = board.widget("Thermostat", thermostat_widget(&heating, appearance));
        let off = board.tile(
            &scene(11, "Turn off the ground floor", "", false),
            I::Power,
            ProTileTemplate::Rectangle,
            1,
        );
        let fan = board.tile(
            &fan(12, "Living room fan", "Medium"),
            I::Fan,
            ProTileTemplate::Rectangle,
            1,
        );
        let overview = board.section(
            "Overview",
            "",
            12,
            16.,
            &[
                place(clock, 12, 3),
                place(thermostat, 6, 3),
                place(off, 6, 1),
                place(fan, 6, 1),
            ],
        );
        let rooms = [
            ("Living room", "3 lights · TV", I::Sofa, "21.0°", true),
            ("Kitchen", "2 lights", I::Kitchen, "22.5°", true),
            ("Dining room", "Off", I::Kitchen, "20.5°", false),
            ("Main bedroom", "1 lamp", I::Bed, "19.0°", true),
            ("Eugenie's bedroom", "Off", I::Bed, "20.0°", false),
            ("Bathroom", "Off", I::Bath, "22.0°", false),
            ("Basement", "Desk lamp on", I::Office, "18.5°", true),
            ("Guest bedroom", "Off", I::Bed, "17.0°", false),
            ("Outside", "Porch light on", I::Tree, "6°", true),
        ]
        .into_iter()
        .enumerate()
        .map(|(index, (title, subtitle, icon, value, active))| {
            let node = board.widget(
                "",
                room(20 + index as i32, title, subtitle, icon, value, active),
            );
            place(node, 4, 2)
        })
        .collect::<Vec<_>>();
        let rooms = board.section("Rooms", "All rooms", 3, 136., &rooms);
        let agenda_rows = [
            agenda(
                1,
                31,
                "Today",
                "17:30",
                "Halloween — neighbourhood round",
                "Family",
                ProAgendaTemporalKind::Timed,
            ),
            agenda(
                2,
                1,
                "Tomorrow · Sun 1 Nov",
                "All day",
                "Back to standard time",
                "Family",
                ProAgendaTemporalKind::AllDay,
            ),
            agenda(
                3,
                2,
                "Mon 2 Nov",
                "Tonight",
                "Blue bin collection",
                "Collections",
                ProAgendaTemporalKind::AllDay,
            ),
        ];
        let agenda = board.widget(
            "",
            ProDashboardWidget {
                kind: ProDashboardWidgetKind::Agenda,
                agenda: ModelRc::from(agenda_rows.as_slice()),
                item_limit: 3,
                data_ready: true,
                can_open: true,
                ..Default::default()
            },
        );
        let agenda = board.section("Agenda", "Open", 12, 16., &[place(agenda, 12, 5)]);
        let tasks = [
            task(1, "Milk 2 %", "", "", false),
            task(2, "Bananas", "", "", false),
            task(3, "Sandwich bread", "", "", false),
            task(4, "Eggs", "", "×12", false),
            task(5, "Coffee beans", "", "", true),
        ];
        let lists = board.widget(
            "Groceries",
            ProDashboardWidget {
                kind: ProDashboardWidgetKind::Tasks,
                tasks: ModelRc::from(tasks.as_slice()),
                task_access: ProTaskAccess::Editable,
                item_limit: 5,
                status: "8 to buy".into(),
                data_ready: true,
                can_open: true,
                ..Default::default()
            },
        );
        let lists = board.section("Lists", "Open", 12, 16., &[place(lists, 12, 5)]);
        let security = board.widget(
            "",
            ProDashboardWidget {
                kind: ProDashboardWidgetKind::Security,
                security: ModelRc::from(
                    [
                        device(&alarm(30, ProAlarmCondition::Disarmed), I::Shield),
                        device(&garage(31, "Garage"), I::Garage),
                        device(&lock(32, "Front door"), I::Lock),
                    ]
                    .as_slice(),
                ),
                action_label: "Details".into(),
                data_ready: true,
                can_open: true,
                ..Default::default()
            },
        );
        let security = board.section("Security", "", 12, 16., &[place(security, 12, 5)]);
        let media = board.tile(
            &player(40, "ICI Musique", "Kitchen · Sonos", true),
            I::Music,
            ProTileTemplate::Rectangle,
            2,
        );
        let power = board.tile(
            &sensor(41, "Current power", "2.4", "kW"),
            I::Power,
            ProTileTemplate::Rectangle,
            2,
        );
        let media = board.section(
            "Media & energy",
            "",
            12,
            16.,
            &[place(media, 12, 2), place(power, 12, 2)],
        );
        board.root(
            24.,
            &[
                (chips, 12, 12, 1),
                (overview, 6, 12, 7),
                (rooms, 6, 12, 7),
                (agenda, 3, 12, 5),
                (lists, 3, 12, 5),
                (security, 3, 12, 5),
                (media, 3, 12, 5),
            ],
        );
        board
    }
    /// The living room board shown in the room popup.
    pub(super) fn living_room(appearance: Appearance) -> Board {
        use wire::DashboardIcon as I;
        let mut board = Board::new(appearance);
        let scenes = [
            ("Day", false),
            ("In between", false),
            ("Cosy", true),
            ("Off", false),
        ]
        .into_iter()
        .enumerate()
        .map(|(index, (title, active))| {
            let node = board.tile(
                &scene(50 + index as i32, title, "Living room", active),
                I::Scene,
                ProTileTemplate::Rectangle,
                1,
            );
            place(node, 6, 1)
        })
        .collect::<Vec<_>>();
        let scenes = board.section("Scenes", "", 12, 16., &scenes);
        let heater = thermostat(60, "Living room baseboard", "21.0", "21.5");
        let heater = board.widget(
            "Living room baseboard",
            thermostat_widget(&heater, appearance),
        );
        let ceiling = board.tile(
            &fan(61, "Ceiling fan", "Medium"),
            I::Fan,
            ProTileTemplate::Rectangle,
            2,
        );
        let climate = board.section(
            "Climate",
            "",
            12,
            16.,
            &[place(heater, 12, 3), place(ceiling, 12, 2)],
        );
        let lamps = [
            ("Living room ceiling light", 70.),
            ("Stairs", 35.),
            ("Bookshelf lamp", 55.),
            ("Table lamp", 0.),
            ("Design lamp", 0.),
        ]
        .into_iter()
        .enumerate()
        .map(|(index, (title, percent))| {
            let node = board.tile(
                &lamp(70 + index as i32, title, percent, None),
                I::Light,
                ProTileTemplate::Rectangle,
                1,
            );
            place(node, 12, 1)
        })
        .collect::<Vec<_>>();
        let lights = board.section("Lights", "", 12, 16., &lamps);
        let temperature = board.tile(
            &sensor(80, "Temperature", "21.0", "°C"),
            I::Thermostat,
            ProTileTemplate::Compact,
            1,
        );
        let sensors = board.section("Sensors", "", 12, 16., &[place(temperature, 6, 1)]);
        let bar = board.tile(
            &player(90, "Audio TV", "Sound bar · Sonos", true),
            I::Music,
            ProTileTemplate::Large,
            3,
        );
        let shield = board.tile(
            &player(91, "Shield TV", "On · Plex", false),
            I::Music,
            ProTileTemplate::Rectangle,
            1,
        );
        let media = board.section(
            "Media",
            "",
            12,
            16.,
            &[place(bar, 12, 3), place(shield, 12, 1)],
        );
        board.root(
            24.,
            &[
                (scenes, 4, 12, 3),
                (lights, 4, 12, 6),
                (media, 4, 12, 5),
                (climate, 4, 12, 6),
                (sensors, 4, 12, 2),
            ],
        );
        board
    }
    /// Which page of the tile gallery a capture shows.
    #[derive(Clone, Copy)]
    pub(super) enum GalleryPage {
        /// Lights, switch, fan and garage door.
        A,
        /// Thermostat, media player and the two alarm states.
        B1,
        /// Sensor and scene.
        B2,
    }
    /// The tile gallery: every kind in its compact, rectangle and large template.
    pub(super) fn gallery(appearance: Appearance, page: GalleryPage) -> Board {
        use wire::DashboardIcon as I;
        let mut board = Board::new(appearance);
        let mut sections = Vec::new();
        let mut row = |board: &mut Board, title: &str, card: CardRow, icon: I| {
            let compact = board.tile(&card, icon, ProTileTemplate::Compact, 2);
            let rectangle = board.tile(&card, icon, ProTileTemplate::Rectangle, 2);
            let large = board.tile(&card, icon, ProTileTemplate::Large, 3);
            let section = board.section(
                title,
                "",
                12,
                16.,
                &[
                    place(compact, 1, 2),
                    place(rectangle, 4, 2),
                    place(large, 2, 3),
                ],
            );
            sections.push((section, 12, 12, 3));
        };
        match page {
            GalleryPage::A => {
                row(
                    &mut board,
                    "Dimmable light",
                    lamp(100, "Living room ceiling light", 70., None),
                    I::Light,
                );
                row(
                    &mut board,
                    "RGB light",
                    lamp(101, "Desk strip", 60., Some((124, 108, 240))),
                    I::Light,
                );
                row(
                    &mut board,
                    "Switch",
                    switch(102, "Design lamp", true),
                    I::Power,
                );
                row(&mut board, "Fan", fan(103, "Ceiling fan", "Medium"), I::Fan);
                row(
                    &mut board,
                    "Garage door",
                    garage(104, "Garage door"),
                    I::Garage,
                );
            }
            GalleryPage::B1 => {
                row(
                    &mut board,
                    "Thermostat",
                    thermostat(100, "Home thermostat", "21.5", "21.0"),
                    I::Thermostat,
                );
                row(
                    &mut board,
                    "Media player",
                    player(101, "ICI Musique", "Kitchen · Sonos", true),
                    I::Music,
                );
                row(
                    &mut board,
                    "Alarm",
                    alarm(102, ProAlarmCondition::Disarmed),
                    I::Shield,
                );
                row(
                    &mut board,
                    "Alarm · triggered",
                    alarm(103, ProAlarmCondition::Triggered),
                    I::Shield,
                );
            }
            GalleryPage::B2 => {
                row(
                    &mut board,
                    "Sensor",
                    sensor(104, "Living room temperature", "21.0", "°C"),
                    I::Thermostat,
                );
                row(
                    &mut board,
                    "Scene",
                    scene(105, "Cosy", "Living room", true),
                    I::Scene,
                );
            }
        }
        board.root(24., &sections);
        board
    }
    /// Two grocery widgets side by side, the tablet and phone tile widths.
    pub(super) fn list_tiles(appearance: Appearance) -> Board {
        let mut board = Board::new(appearance);
        let wide = board.widget("Groceries", groceries(200));
        let narrow = board.widget("Groceries", groceries(300));
        let section = board.section("", "", 12, 16., &[place(wide, 6, 6), place(narrow, 6, 6)]);
        board.root(24., &[(section, 12, 12, 6)]);
        board
    }
}

#[test]
#[ignore = "writes mockup parity captures outside the repository"]
fn mockup_parity_captures() {
    use crate::dashboard_presentation::Appearance;
    use home_wire_contract::home::api::v2::DashboardIcon as I;
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
    window.set_greeting("Good evening, Damien".into());
    window.set_local_date("Saturday 31 October".into());
    window.set_account_initials("D".into());
    window.set_account_label("Damien".into());
    window.set_navigation_rows(ModelRc::from(
        ["Home", "Rooms", "Lists", "Calendar", "Cameras", "Settings"]
            .into_iter()
            .enumerate()
            .map(|(index, label)| OptionRow {
                token: index as i32,
                generation: 1,
                label: label.into(),
                description: "".into(),
                disabled: false,
            })
            .collect::<Vec<_>>()
            .as_slice(),
    ));
    window.set_navigation_state(SelectState {
        selected: SelectionRef {
            present: true,
            token: 0,
        },
        active: SelectionRef {
            present: true,
            token: 0,
        },
        ..Default::default()
    });
    window.show().unwrap();
    let size = |width: f32, height: f32| {
        window
            .window()
            .set_size(slint::LogicalSize::new(width, height));
    };
    let capture = |name: &str| save(&window, &directory.join(format!("{name}.png")));
    let clear = || {
        window.set_native_popup(DialogState::default());
        window.set_media_visible(false);
        window.set_media_has_data(false);
        window.set_detail_panels(Default::default());
        window.set_device_cards(Default::default());
        window.set_home_layout_nodes(Default::default());
        window.set_popup_layout_nodes(Default::default());
    };
    let home = |appearance: Appearance| {
        clear();
        window.set_dark_mode(appearance == Appearance::Dark);
        window.set_page(ViewPage::Home);
        window.set_page_title("Home".into());
        window.set_home_layout_nodes(parity::home(appearance).model());
    };
    let room = || {
        clear();
        window.set_dark_mode(false);
        window.set_page(ViewPage::Home);
        window.set_home_layout_nodes(parity::home(Appearance::Light).model());
        window.set_native_popup_title("Living room".into());
        window.set_native_popup_glyph(parity::glyph(I::Sofa));
        window.set_native_popup_generation(5);
        window.set_native_popup_detail(false);
        window.set_native_popup_can_back(false);
        window.set_popup_layout_nodes(parity::living_room(Appearance::Light).model());
        window.set_native_popup(DialogState {
            open: true,
            topmost: true,
            ..Default::default()
        });
    };
    let detail = |panel: HomewardDetailRow| {
        clear();
        window.set_dark_mode(false);
        window.set_page(ViewPage::Detail);
        window.set_detail_title(panel.device.title.clone());
        window.set_device_cards(ModelRc::from([panel.device.clone()].as_slice()));
        window.set_detail_panels(ModelRc::from([panel].as_slice()));
    };

    // Home boards.
    home(Appearance::Light);
    size(1440., 1000.);
    capture("Main");
    size(1280., 800.);
    capture("Tablet");
    size(390., 844.);
    capture("Mobile");
    // The System board shows the light and dark token sets: the home board
    // under each theme, stacked.
    size(1440., 740.);
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(300));
    let light = window.window().take_snapshot().unwrap();
    home(Appearance::Dark);
    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(300));
    let dark = window.window().take_snapshot().unwrap();
    let mut stacked = light.as_bytes().to_vec();
    stacked.extend_from_slice(dark.as_bytes());
    write_png(
        &directory.join("System.png"),
        light.width(),
        light.height() + dark.height(),
        &stacked,
    );

    // Tile galleries.
    clear();
    window.set_dark_mode(false);
    window.set_page(ViewPage::Home);
    window.set_page_title("Tiles · catalogue A".into());
    window
        .set_home_layout_nodes(parity::gallery(Appearance::Light, parity::GalleryPage::A).model());
    size(1440., 1400.);
    capture("TilesA");
    window.set_page_title("Tiles · catalogue B1".into());
    window
        .set_home_layout_nodes(parity::gallery(Appearance::Light, parity::GalleryPage::B1).model());
    capture("TilesB1");
    window.set_page_title("Tiles · catalogue B2".into());
    window
        .set_home_layout_nodes(parity::gallery(Appearance::Light, parity::GalleryPage::B2).model());
    size(1440., 800.);
    capture("TilesB2");
    window.set_page_title("Home".into());
    window.set_home_layout_nodes(parity::list_tiles(Appearance::Light).model());
    size(880., 520.);
    capture("ListTile");

    // Room popup.
    room();
    size(1280., 800.);
    capture("Room");
    size(390., 844.);
    capture("MobileRoom");

    // Device detail panels.
    let lamp = parity::detail(
        &parity::lamp(1, "Living room ceiling light", 70., None),
        I::Light,
    );
    let strip = parity::detail(
        &parity::lamp(2, "Desk strip", 60., Some((124, 108, 240))),
        I::Light,
    );
    let heating = parity::detail(
        &parity::thermostat(3, "Home thermostat", "21.5", "21.0"),
        I::Thermostat,
    );
    let fan = parity::detail(&parity::fan(4, "Ceiling fan", "Medium"), I::Fan);
    let garage = parity::detail(&parity::garage(5, "Garage door"), I::Garage);
    let alarm = parity::detail(&parity::alarm(6, ProAlarmCondition::Disarmed), I::Shield);
    let switch = parity::detail(&parity::switch(7, "Design lamp", true), I::Power);
    size(480., 760.);
    for (name, panel) in [
        ("ModalLight", lamp),
        ("ModalLightColor", strip.clone()),
        ("ModalThermostat", heating.clone()),
        ("ModalFan", fan),
        ("ModalGarage", garage),
        ("ModalAlarm", alarm.clone()),
        ("ModalSwitch", switch),
    ] {
        detail(panel);
        capture(name);
    }
    size(390., 844.);
    // The phone light sheet shows the colour strip: its wheel is the taller panel.
    for (name, panel) in [
        ("MobileLight", strip),
        ("MobileThermostat", heating),
        ("MobileAlarm", alarm),
    ] {
        detail(panel);
        capture(name);
    }

    // Media detail.
    clear();
    window.set_page(ViewPage::Detail);
    // The sheet is titled by the player; what it plays is the panel's own title.
    window.set_detail_title("Kitchen".into());
    window.set_media_visible(true);
    window.set_media_has_data(true);
    window.set_media_editable(true);
    window.set_media_data(ProMediaPresentation {
        token: 0,
        generation: 1,
        title: "ICI Musique".into(),
        subtitle: "Kitchen · Sonos".into(),
        collection: "Live radio".into(),
        playback: "Playing".into(),
        artwork_state: ProArtworkState::Absent,
        ..Default::default()
    });
    window.set_media_actions(ModelRc::from(
        [
            ProMediaAction {
                operation: ProMediaOperation::Previous,
                label: "Previous".into(),
            },
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
    // The volume is the panel's own slider, not a value field; a live stream
    // has no position field either.
    window.set_media_values(ModelRc::default());
    window.set_media_volume(HomewardMediaVolume {
        field: 0,
        offered: true,
        known: true,
        level: 35,
    });
    window.set_media_members(ModelRc::from(
        [
            ("Kitchen", true, false, 35),
            ("Living room", true, true, 20),
            ("Bedroom", false, true, 10),
        ]
        .into_iter()
        .enumerate()
        .map(
            |(index, (title, grouped, can_group, volume))| ProMediaMember {
                token: index as i32,
                generation: 1,
                title: title.into(),
                glyph: parity::glyph(I::Music),
                grouped,
                can_group,
                volume,
                volume_known: true,
                can_volume: true,
            },
        )
        .collect::<Vec<_>>()
        .as_slice(),
    ));
    window.set_media_progress_known(false);
    size(480., 760.);
    capture("ModalMedia");

    // Lists.
    clear();
    window.set_page(ViewPage::Lists);
    window.set_page_title("Lists".into());
    let mut state = crate::list_state::tests::ready();
    // The host conventions in the summaries: "- " nests a row under the one
    // above, a trailing "xN" is its quantity; the description is the note
    // line. Three checked top-level rows feed the completed group and the
    // suggestions; the checked sub-item shows the strike and the "1/3" progress.
    let items = [
        ("milk", "Milk 2 %", false, "2 L"),
        ("eggs", "Eggs x2", false, "Large"),
        ("flour", "Flour", false, ""),
        ("yeast", "- Instant yeast", false, ""),
        ("tacos", "Taco dinner", false, ""),
        ("tortillas", "- Tortillas", false, ""),
        ("salsa", "- Mild salsa", true, ""),
        ("avocados", "- Avocados x3", false, ""),
        ("coffee", "Coffee beans", true, ""),
        ("bananas", "Bananas", true, ""),
        ("bread", "Sandwich bread", true, ""),
    ]
    .map(
        |(uid, summary, completed, description)| crate::list_state::tests::ItemFixture {
            uid,
            revision: 1,
            summary,
            completed,
            description,
        },
    );
    state
        .publish_items(crate::list_state::tests::detailed_items(
            crate::list_state::tests::LIST,
            11,
            &items,
        ))
        .unwrap();
    crate::list_host::projection::render(
        &window,
        crate::list_host::projection::Projection::new(&state),
    );
    size(1440., 1000.);
    capture("Lists");
    size(1280., 800.);
    capture("TabletLists");
    size(390., 844.);
    capture("MobileLists");
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

    window.on_device_power(|_, _, _| panic!("scroll must not issue a power intention"));
    window.on_device_brightness(|_, _, _| panic!("scroll must not issue a level intention"));
    window.on_device_edit(|_, _, _, _| panic!("scroll must not issue an edit intention"));
    window.on_device_choice(|_, _, _| panic!("scroll must not issue a choice intention"));
    window.on_device_step(|_, _, _| panic!("scroll must not issue a step intention"));
    window.on_alarm_confirm(|_, _| panic!("scroll must not submit a code"));
    window.on_alarm_digit(|_, _, _| panic!("scroll must not enter a digit"));
    window.on_alarm_mode(|_, _, _| panic!("scroll must not change the alarm mode"));
    window.show().unwrap();
    for (screen, width, height) in [("desktop", 1280., 900.), ("phone-small", 360., 740.)] {
        window
            .window()
            .set_size(slint::LogicalSize::new(width, height));
        for dark in [false, true] {
            window.set_dark_mode(dark);
            for large in [false, true] {
                window.set_large_text(large);
                for (name, panel) in panels()
                    .into_iter()
                    .filter(|(name, _)| matches!(*name, "alarm"))
                {
                    window.set_device_cards(ModelRc::from([panel.device.clone()].as_slice()));
                    window.set_detail_panels(ModelRc::from([panel].as_slice()));
                    i_slint_backend_testing::mock_elapsed_time(std::time::Duration::from_millis(
                        300,
                    ));
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
                        i_slint_backend_testing::mock_elapsed_time(
                            std::time::Duration::from_millis(100),
                        );
                    }
                    let keypad = ElementQuery::from_root(&window)
                        .match_inherits("KitKeypad")
                        .find_first()
                        .expect("real product keypad");
                    let confirm = keypad
                        .query_descendants()
                        .match_inherits("KitPressable")
                        .match_predicate(|e| {
                            e.accessible_label()
                                .is_some_and(|label| label.contains("Disarm"))
                        })
                        .find_first()
                        .expect("the actual confirmation action");
                    let point = confirm.absolute_position();
                    let extent = confirm.size();
                    assert!(
                        point.y >= origin.y - 1.,
                        "confirmation starts inside viewport"
                    );
                    assert!(
                        point.y + extent.height <= origin.y + size.height + 1.,
                        "the complete confirmation action must be reachable by scrolling"
                    );
                    assert!(
                        point.x >= 0. && point.x + extent.width <= width + 1.,
                        "confirmation is not horizontally clipped"
                    );
                    save(
                        &window,
                        &directory.join(format!(
                            "{screen}-{name}-{}-{}-bottom.png",
                            if dark { "dark" } else { "light" },
                            if large { 200 } else { 100 }
                        )),
                    );
                }
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
    // Slint Kit 0.9.0 names the field once, on its root; the native editor
    // inside it carries no accessible node of its own.
    let input = ElementQuery::from_root(&window)
        .match_inherits("KitTextField")
        .match_predicate(|e| e.accessible_label().as_deref() == Some("Start UTC (RFC3339)"))
        .find_first()
        .expect("actual product query field")
        .query_descendants()
        .match_type_name("TextInput")
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
    window.on_alarm_mode(move |token, generation, choice| {
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

fn synthetic_thermostat_popup() -> ProLayoutNode {
    let (_, panel) = panels()
        .into_iter()
        .find(|(name, _)| *name == "climate")
        .unwrap();
    ProLayoutNode {
        token: 20,
        generation: 3,
        kind: ProLayoutKind::Widget,
        widget: ProDashboardWidget {
            kind: ProDashboardWidgetKind::Thermostat,
            detailed: true,
            data_ready: true,
            thermostat: panel.device,
            field: panel.climate_field,
            ..Default::default()
        },
        ..Default::default()
    }
}

#[test]
fn room_detail_uses_one_panel_and_keeps_back_distinct_from_close() {
    use i_slint_backend_testing::ElementQuery;
    use std::{cell::RefCell, rc::Rc};
    i_slint_backend_testing::init_no_event_loop();
    let window = setup();
    window.set_page(ViewPage::Home);
    window.set_native_popup_title("Synthetic room thermostat".into());
    window.set_native_popup_generation(3);
    window.set_native_popup_detail(true);
    window.set_native_popup_can_back(true);
    window.set_popup_layout_nodes(ModelRc::from([synthetic_thermostat_popup()]));
    window.set_native_popup(DialogState {
        open: true,
        topmost: true,
        ..Default::default()
    });
    let events = Rc::new(RefCell::new(Vec::new()));
    let output = events.clone();
    window.on_native_popup_back(move |generation| output.borrow_mut().push((true, generation)));
    let output = events.clone();
    window.on_native_popup_dismiss(move |generation, _| {
        output.borrow_mut().push((false, generation))
    });
    window.show().unwrap();
    let popup = ElementQuery::from_root(&window)
        .match_inherits("HomewardPopup")
        .find_first()
        .unwrap();
    assert_eq!(
        popup
            .query_descendants()
            .match_inherits("ProDetailPanel")
            .find_all()
            .len(),
        1
    );
    popup
        .query_descendants()
        .match_predicate(|item| item.accessible_label().as_deref() == Some("Back"))
        .find_first()
        .unwrap()
        .invoke_accessible_default_action();
    assert_eq!(*events.borrow(), vec![(true, 3)]);
    window.set_native_popup_generation(4);
    popup
        .query_descendants()
        .match_predicate(|item| {
            item.accessible_label().as_deref() == Some("Close Synthetic room thermostat")
        })
        .find_first()
        .unwrap()
        .invoke_accessible_default_action();
    assert_eq!(*events.borrow(), vec![(true, 3), (false, 4)]);
    window.hide().unwrap();
}

#[test]
fn modal_content_respects_dynamic_system_insets_without_shrinking_scrims() {
    use i_slint_backend_testing::{ElementQuery, TestingBackend, TestingBackendOptions};
    use i_slint_core::{lengths::LogicalEdges, window::WindowInner};
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        renderer_name: Some("software".into()),
        ..Default::default()
    })))
    .unwrap();
    let directory = std::env::var_os("HOMEWARD_SYNTHETIC_RENDER_DIR").map(std::path::PathBuf::from);
    if let Some(directory) = &directory {
        std::fs::create_dir_all(directory).unwrap();
    }
    let window = setup();
    let (_, panel) = panels()
        .into_iter()
        .find(|(name, _)| *name == "alarm")
        .unwrap();
    window.set_device_cards(ModelRc::from([panel.device.clone()]));
    window.set_detail_panels(ModelRc::from([panel]));
    window.set_native_popup_title("Synthetic room thermostat".into());
    window.set_native_popup_detail(true);
    window.set_native_popup_can_back(true);
    window.set_popup_layout_nodes(ModelRc::from([synthetic_thermostat_popup()]));
    window.show().unwrap();
    for (screen, width, height) in [
        ("phone", 360., 800.),
        ("tablet", 800., 1100.),
        ("desktop", 1100., 900.),
    ] {
        window
            .window()
            .set_size(slint::LogicalSize::new(width, height));
        for (appearance, dark, large) in [("light100", false, false), ("dark200", true, true)] {
            window.set_dark_mode(dark);
            window.set_large_text(large);
            for room in [false, true] {
                window.set_page(if room {
                    ViewPage::Home
                } else {
                    ViewPage::Detail
                });
                window.set_native_popup(DialogState {
                    open: room,
                    topmost: room,
                    ..Default::default()
                });
                // Change the real Window property after opening, then remove the
                // insets again. This exercises runtime rotation/system-bar updates.
                for (step, insets) in [
                    LogicalEdges::default(),
                    LogicalEdges::new(28., 24., 12., 8.),
                    LogicalEdges::default(),
                ]
                .into_iter()
                .enumerate()
                {
                    WindowInner::from_pub(window.window()).set_window_item_safe_area(insets);
                    slint::platform::update_timers_and_animations();
                    let route = if room { "room" } else { "direct" };
                    let context = format!("{screen}-{appearance}-{route}-{step}");
                    let surface = ElementQuery::from_root(&window)
                        .match_id(if room {
                            "HomewardWindow::popup-dialog"
                        } else {
                            "HomewardWindow::detail-dialog"
                        })
                        .find_first()
                        .expect("actual product modal surface");
                    let origin = surface.absolute_position();
                    let size = surface.size();
                    assert!(
                        origin.x.abs() < 1. && origin.y.abs() < 1.,
                        "scrim origin: {context}"
                    );
                    assert!(
                        (size.width - width).abs() < 1. && (size.height - height).abs() < 1.,
                        "scrim extent: {context}"
                    );
                    let area = surface
                        .query_descendants()
                        .match_id(if room {
                            "HomewardPopup::panel"
                        } else {
                            "HomewardWindow::detail-scroll"
                        })
                        .find_first()
                        .expect("bounded interactive content");
                    let origin = area.absolute_position();
                    let size = area.size();
                    assert!(
                        origin.x >= insets.left - 1. && origin.y >= insets.top - 1.,
                        "content origin: {context} {origin:?}"
                    );
                    assert!(
                        origin.x + size.width <= width - insets.right + 1.
                            && origin.y + size.height <= height - insets.bottom + 1.,
                        "content extent: {context} {origin:?} {size:?}"
                    );
                    // A phone room is a sheet whose single Back chevron
                    // dismisses it; every other modal keeps a Close button.
                    let sheet = room && screen == "phone";
                    let close = surface
                        .query_descendants()
                        .match_inherits("KitPressable")
                        .match_predicate(move |item| {
                            item.accessible_label().is_some_and(|label| {
                                if sheet {
                                    label == "Back"
                                } else {
                                    label.starts_with("Close")
                                }
                            })
                        })
                        .find_first()
                        .expect("modal Close");
                    let origin = close.absolute_position();
                    let size = close.size();
                    assert!(
                        origin.x >= insets.left
                            && origin.y >= insets.top
                            && origin.x + size.width <= width - insets.right
                            && origin.y + size.height <= height - insets.bottom,
                        "Close stays outside system UI: {context} {origin:?} {size:?}"
                    );
                    if step == 1
                        && let Some(directory) = &directory
                    {
                        save(
                            &window,
                            &directory.join(format!("safe-area-{screen}-{appearance}-{route}.png")),
                        );
                    }
                }
            }
        }
    }
    window.hide().unwrap();
}

#[path = "../examples/fixtures/modular.rs"]
mod modular_fixture;

#[test]
fn nhf044_modular_product_matrix_and_detail_use_the_same_source_rows() {
    use i_slint_backend_testing::{TestingBackend, TestingBackendOptions};
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        renderer_name: Some("software".into()),
        ..Default::default()
    })))
    .unwrap();
    let window = setup();
    modular_fixture::apply(&window, false);
    assert_eq!(
        window.get_home_layout_nodes().row_count(),
        6,
        "one root and five families"
    );
    assert_eq!(
        window
            .get_home_layout_nodes()
            .row_data(0)
            .unwrap()
            .children
            .row_count(),
        5
    );
    window.show().unwrap();
    let directory = std::env::var_os("HOMEWARD_NHF044_RENDER_DIR").map(std::path::PathBuf::from);
    if let Some(path) = &directory {
        std::fs::create_dir_all(path).unwrap();
    }
    for (size, width, height) in [("compact", 360., 800.), ("desktop", 1100., 900.)] {
        for dark in [false, true] {
            for large in [false, true] {
                window.set_dark_mode(dark);
                window.set_large_text(large);
                window
                    .window()
                    .set_size(slint::LogicalSize::new(width, height));
                slint::platform::update_timers_and_animations();
                if let Some(path) = &directory {
                    save(
                        &window,
                        &path.join(format!(
                            "{size}-{}-{}.png",
                            if dark { "dark" } else { "light" },
                            if large { 200 } else { 100 }
                        )),
                    );
                }
            }
        }
    }
    let before = window.get_device_cards().row_data(0).unwrap();
    modular_fixture::apply(&window, true);
    let detail = window.get_detail_panels().row_data(0).unwrap().device;
    assert_eq!(before.control_family, detail.control_family);
    assert_eq!(before.modules.row_count(), detail.modules.row_count());
    for index in 0..before.modules.row_count() {
        let before = before.modules.row_data(index).unwrap();
        let after = detail.modules.row_data(index).unwrap();
        assert_eq!(before.state, after.state);
        assert_eq!(before.role, after.role);
        assert_eq!(
            before.choices.iter().collect::<Vec<_>>(),
            after.choices.iter().collect::<Vec<_>>()
        );
    }
}
