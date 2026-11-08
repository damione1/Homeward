//! Synthetic UI data only. No profile, session, transport, provider or commands.
use crate::*;
use slint::ModelRc;

fn state(token: i32, label: &str) -> ProModuleState {
    ProModuleState {
        identity: ProIdentity {
            token,
            generation: 1,
        },
        access: ProControlAccess::Allowed,
        available: true,
        phase: ProActionPhase::Idle,
        label: label.into(),
        observation: "Observed · current".into(),
        ..Default::default()
    }
}
fn numeric(label: &str, value: &str, suffix: &str) -> ProDeviceValue {
    ProDeviceValue {
        token: 0,
        kind: ProValueKind::Numeric,
        mode: ProValueMode::Single,
        label: label.into(),
        lower: value.into(),
        suffix: suffix.into(),
        description: "Exact source range and step".into(),
        lower_position: 0.4,
        slider_enabled: true,
        can_step: true,
        ..Default::default()
    }
}
pub fn rows() -> Vec<ProDeviceRow> {
    let power = |token| ProControlModule {
        state: state(token, "Power"),
        role: ProControlRole::Power,
        power: ProPowerReading::On,
        ..Default::default()
    };
    let range = |token| ProControlModule {
        state: state(token, "Level"),
        role: ProControlRole::Range,
        range: numeric("Level", "40", "%"),
        ..Default::default()
    };
    let color = ProControlModule {
        state: state(3, "Color"),
        role: ProControlRole::Color,
        temperature_supported: true,
        rgb_supported: true,
        observed_color_mode: ProColorMode::Rgb,
        editor_color_mode: ProColorMode::Rgb,
        temperature: numeric("White temperature", "300", "mired"),
        rgb_known: true,
        rgb: SrgbColorView {
            red: 32,
            green: 140,
            blue: 225,
            alpha: 255,
        },
        ..Default::default()
    };
    let preset = ProControlModule {
        state: state(4, "Preset"),
        role: ProControlRole::Preset,
        choices: ModelRc::from(
            [
                ProDeviceChoice {
                    token: 11,
                    label: "Reading".into(),
                    selected: true,
                },
                ProDeviceChoice {
                    token: 73,
                    label: "Evening".into(),
                    selected: false,
                },
            ]
            .as_slice(),
        ),
        ..Default::default()
    };
    let steps = ProControlModule {
        state: state(6, "Speed"),
        role: ProControlRole::Steps,
        choices: ModelRc::from(
            [
                ProDeviceChoice {
                    token: 7,
                    label: "Quiet".into(),
                    selected: true,
                },
                ProDeviceChoice {
                    token: 29,
                    label: "Breeze".into(),
                    selected: false,
                },
                ProDeviceChoice {
                    token: 83,
                    label: "Boost".into(),
                    selected: false,
                },
            ]
            .as_slice(),
        ),
        ..Default::default()
    };
    let motion = |token| ProControlModule {
        state: state(token, "Motion"),
        role: ProControlRole::Motion,
        motion_operations: ModelRc::from(
            [
                ProCoverChoice {
                    token: 1,
                    label: "Open".into(),
                    operation: ProCoverOperation::Open,
                },
                ProCoverChoice {
                    token: 2,
                    label: "Stop".into(),
                    operation: ProCoverOperation::Stop,
                },
                ProCoverChoice {
                    token: 3,
                    label: "Close".into(),
                    operation: ProCoverOperation::Close,
                },
            ]
            .as_slice(),
        ),
        ..Default::default()
    };
    [
        (
            "Synthetic reading light",
            ProDeviceKind::Light,
            ProControlFamily::ContinuousLevel,
            vec![power(1), range(2), color, preset],
        ),
        (
            "Synthetic ventilation",
            ProDeviceKind::Fan,
            ProControlFamily::SteppedLevel,
            vec![power(5), steps],
        ),
        (
            "Synthetic blind",
            ProDeviceKind::Cover,
            ProControlFamily::Blind,
            vec![motion(7), range(8)],
        ),
        (
            "Synthetic garage",
            ProDeviceKind::Cover,
            ProControlFamily::Garage,
            vec![motion(9)],
        ),
        (
            "Synthetic outlet",
            ProDeviceKind::Switch,
            ProControlFamily::Binary,
            vec![power(10)],
        ),
    ]
    .into_iter()
    .enumerate()
    .map(|(index, (title, kind, family, modules))| ProDeviceRow {
        token: 100 + index as i32,
        generation: 1,
        title: title.into(),
        value: "Observed · current".into(),
        quality: "Synthetic source".into(),
        kind,
        modular: true,
        control_family: family,
        modules: modules.as_slice().into(),
        access: ProControlAccess::Allowed,
        ..Default::default()
    })
    .collect()
}
pub fn apply(window: &HomewardWindow, detail: bool) {
    window.set_show_endpoint(false);
    window.set_access_stage(ProAccessStage::Connected);
    window.set_view_loaded(true);
    window.set_commands_ready(true);
    window.set_connection_status(ConnectionStatus::Live);
    let rows = rows();
    window.set_device_cards(rows.as_slice().into());
    if detail {
        window.set_page(ViewPage::Detail);
        window.set_detail_title("Synthetic reading light".into());
        window.set_detail_panels(
            [HomewardDetailRow {
                device: rows[0].clone(),
                ..Default::default()
            }]
            .as_slice()
            .into(),
        );
    } else {
        window.set_page(ViewPage::Home);
        window.set_has_configured_layout(true);
        let children = rows
            .iter()
            .enumerate()
            .map(|(index, _)| ProLayoutInput {
                index: index as i32 + 1,
                column_span: ProLayoutSpan::Count,
                columns: 1,
                rows: 1,
                ..Default::default()
            })
            .collect::<Vec<_>>();
        let mut nodes = vec![ProLayoutNode {
            token: 1,
            generation: 1,
            kind: ProLayoutKind::Container,
            flow: ProLayoutFlow::Grid,
            columns: 2,
            column_policy: ProLayoutColumns::Maximum,
            minimum_column_width: 300.,
            gap: 16.,
            children: children.as_slice().into(),
            ..Default::default()
        }];
        nodes.extend(rows.iter().map(|row| ProLayoutNode {
            token: row.token,
            generation: row.generation,
            kind: ProLayoutKind::Device,
            title: row.title.clone(),
            device: row.clone(),
            ..Default::default()
        }));
        window.set_home_layout_nodes(nodes.as_slice().into());
    }
}
