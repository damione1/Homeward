//! Family panel projections. Capabilities and observations are interpreted once,
//! beside the command boundary; UI choice tokens still resolve through that boundary.
use super::{
    capability::Control,
    observation,
    presentation::{CardRow, nested},
};
use crate::{
    ChoiceIcon, ChoiceRow, HomewardDetailRow, IconGlyph, ProAlarm, ProAlarmCondition,
    ProAssociatedLock, ProClimate, ProClimateOptions, ProCover, ProCoverLock, ProCoverMotion,
    ProDetailFact, ProDeviceRow, ProFan, ProLight, ProSwitch, ProSwitchState, ProValueMode, Tone,
};
use home_wire_contract::home::api::v2 as wire;
use slint::{Model, ModelRc, SharedString};

#[derive(Clone, Debug, Default)]
pub(crate) struct Metadata {
    pub associated_only: bool,
    associated_lock: Option<AssociatedLock>,
    lock_state: ProCoverLock,
    modes: Vec<ChoiceRow>,
    mode_icons: Vec<ChoiceIcon>,
    climate_activity: String,
    /// The observed activity admitted by the capability, for its glyph and tone.
    climate_activity_value: Option<wire::TemperatureActivity>,
    /// When the capability's observation was taken; the panels' "since" copy.
    observed: Option<chrono::DateTime<chrono::Utc>>,
    /// The light's observed colour and preset name, shown as facts.
    light_color: Option<(u8, u8, u8)>,
    light_preset: String,
    quality_notice: String,
    fan_modes: Vec<ChoiceRow>,
    speeds: Vec<ChoiceRow>,
    alarm_modes: Vec<ChoiceRow>,
    alarm_icons: Vec<ChoiceIcon>,
    alarm_code_required: bool,
    alarm_operations: Vec<wire::AlarmOperation>,
    alarm_code_count: usize,
    alarm_code_selected: bool,
    alarm_prompt: String,
    alarm_confirm: String,
    condition: ProAlarmCondition,
    motion: ProCoverMotion,
    position: Option<f64>,
    position_step: Option<f64>,
    position_bounds: Option<(f64, f64)>,
}
#[derive(Clone, Debug)]
struct AssociatedLock {
    token: i32,
    generation: i32,
    title: String,
    label: SharedString,
    state: ProCoverLock,
    enabled: bool,
    can_lock: bool,
    can_unlock: bool,
}
/// Synthetic observations for the render tests: what a capability would have
/// reported, stated directly so a board can be drawn without a server.
#[cfg(test)]
impl Metadata {
    /// A lamp observed in `color`.
    pub(crate) fn with_light_color(mut self, color: (u8, u8, u8)) -> Self {
        self.light_color = Some(color);
        self
    }
    /// A thermostat observed in `activity`, offering `modes` with `selected`
    /// chosen.
    pub(crate) fn with_climate(
        mut self,
        activity: wire::TemperatureActivity,
        modes: &[wire::TemperatureMode],
        selected: wire::TemperatureMode,
    ) -> Self {
        self.climate_activity_value = Some(activity);
        self.climate_activity = super::panel::activity_label(activity).into();
        self.modes = modes
            .iter()
            .enumerate()
            .map(|(index, mode)| ChoiceRow {
                token: index as i32,
                label: super::panel::mode_label(*mode).into(),
                selected: *mode == selected,
                disabled: false,
            })
            .collect();
        self.mode_icons = modes
            .iter()
            .enumerate()
            .map(|(index, mode)| ChoiceIcon {
                token: index as i32,
                glyph: mode_glyph(*mode),
            })
            .collect();
        self
    }
    /// A fan offering `speeds`, the selected one flagged.
    pub(crate) fn with_speeds(mut self, speeds: &[(&str, bool)]) -> Self {
        self.speeds = speeds
            .iter()
            .enumerate()
            .map(|(index, (label, selected))| ChoiceRow {
                token: index as i32,
                label: (*label).into(),
                selected: *selected,
                disabled: false,
            })
            .collect();
        self
    }
    /// A covering observed at `position` within `bounds`, `motion` under way.
    pub(crate) fn with_cover(
        mut self,
        position: f64,
        bounds: (f64, f64),
        motion: ProCoverMotion,
    ) -> Self {
        self.position = Some(position);
        self.position_bounds = Some(bounds);
        self.position_step = Some(1.);
        self.motion = motion;
        self
    }
    /// An alarm observed in `condition`.
    pub(crate) fn with_alarm(mut self, condition: ProAlarmCondition) -> Self {
        self.condition = condition;
        self
    }
}
impl Metadata {
    pub(super) fn associate_lock(&mut self, row: &CardRow, title: &str) {
        self.associated_lock = Some(AssociatedLock {
            token: row.token,
            generation: row.generation,
            title: title.into(),
            label: row.value.clone(),
            state: row.detail.lock_state,
            enabled: row.access == crate::ProControlAccess::Allowed && !row.command_unavailable,
            can_lock: row
                .lock_operations
                .iter()
                .any(|operation| operation.operation == crate::ProLockOperation::Lock),
            can_unlock: row
                .lock_operations
                .iter()
                .any(|operation| operation.operation == crate::ProLockOperation::Unlock),
        });
    }

    pub(super) fn alarm_input(&mut self, input: Option<&super::alarm_input::Input>) {
        if !self.alarm_code_required {
            return;
        }
        if let Some(operation) = input.and_then(|input| input.operation) {
            for row in &mut self.alarm_modes {
                row.selected = self.alarm_operations.get(row.token as usize) == Some(&operation);
            }
        }
        self.alarm_code_count = input.map_or(0, |input| input.digits.len());
        self.alarm_code_selected = input.is_some_and(|input| input.operation.is_some());
        self.alarm_confirm = input
            .and_then(|input| input.operation)
            .and_then(crate::control_text::alarm_operation)
            .unwrap_or("Confirm")
            .into();
        self.alarm_prompt = if input.is_some_and(|input| input.operation.is_some()) {
            format!("Code to {}", self.alarm_confirm.to_lowercase())
        } else {
            "Choose a mode requiring a code".into()
        };
    }

    pub(super) fn new(control: &Control, capability: &wire::Capability) -> Self {
        let mut result = Self {
            condition: ProAlarmCondition::Unknown,
            quality_notice: observation::quality_notice(capability),
            observed: observation::observed_time(capability),
            ..Default::default()
        };
        let observed = observation::evidence(capability).is_some();
        use wire::capability::Trait as T;
        match control {
            Control::Climate(climate) => {
                let Some(T::TemperatureControl(value)) = capability.r#trait.as_ref() else {
                    return result;
                };
                result.climate_activity_value = observed
                    .then(|| value.observation.activity.and_then(|v| v.as_known()))
                    .flatten()
                    .filter(|activity| climate.observes().activities.contains(activity));
                result.climate_activity = result
                    .climate_activity_value
                    .map(super::panel::activity_label)
                    .unwrap_or_default()
                    .into();
                result.mode_icons = climate
                    .modes()
                    .iter()
                    .enumerate()
                    .map(|(index, mode)| ChoiceIcon {
                        token: index as i32,
                        glyph: mode_glyph(*mode),
                    })
                    .collect();
                let selected_mode = observed
                    .then(|| value.observation.mode.and_then(|v| v.as_known()))
                    .flatten();
                let selected_fan = observed
                    .then(|| value.observation.fan_target.and_then(|v| v.as_known()))
                    .flatten();
                result.modes = climate
                    .modes()
                    .iter()
                    .enumerate()
                    .map(|(index, mode)| ChoiceRow {
                        token: index as i32,
                        label: super::panel::mode_label(*mode).into(),
                        selected: selected_mode == Some(*mode),
                        disabled: false,
                    })
                    .collect();
                result.fan_modes = climate
                    .fan_targets()
                    .iter()
                    .enumerate()
                    .map(|(index, target)| ChoiceRow {
                        token: (climate.modes().len() + index) as i32,
                        label: match *target {
                            wire::TemperatureFanTarget::Unspecified => "Unknown",
                            wire::TemperatureFanTarget::On => "On",
                            wire::TemperatureFanTarget::Auto => "Auto",
                        }
                        .into(),
                        selected: selected_fan == Some(*target),
                        disabled: false,
                    })
                    .collect();
            }
            Control::Fan(fan) => {
                let Some(T::FanControl(value)) = capability.r#trait.as_ref() else {
                    return result;
                };
                let selected = observed
                    .then(|| value.observation.speed.as_deref())
                    .flatten();
                result.speeds = fan
                    .speeds()
                    .iter()
                    .enumerate()
                    .map(|(index, (key, label))| ChoiceRow {
                        token: index as i32,
                        label: label.clone().into(),
                        selected: selected == Some(key.as_str()),
                        disabled: false,
                    })
                    .collect();
            }
            Control::Cover(cover) => {
                let Some(T::Cover(value)) = capability.r#trait.as_ref() else {
                    return result;
                };
                result.position_step = cover.position().and_then(|bounds| bounds.step());
                result.position_bounds = cover
                    .position()
                    .map(|bounds| (bounds.minimum(), bounds.maximum()));
                if observed && let Some(value) = value.observation.value.as_option() {
                    result.position = cover.observed_position(value);
                    result.motion = match value.state.and_then(|v| v.as_known()) {
                        Some(wire::CoverState::Opening) => ProCoverMotion::Opening,
                        Some(wire::CoverState::Closing) => ProCoverMotion::Closing,
                        Some(
                            wire::CoverState::Unspecified
                            | wire::CoverState::Open
                            | wire::CoverState::Closed,
                        )
                        | None => ProCoverMotion::Idle,
                    };
                }
            }
            Control::Alarm(alarm) => {
                result.alarm_code_required = alarm.has_code();
                result.alarm_operations = alarm.operations().to_vec();
                let Some(T::Alarm(value)) = capability.r#trait.as_ref() else {
                    return result;
                };
                let state = observed
                    .then(|| {
                        value
                            .observation
                            .value
                            .as_option()
                            .and_then(|v| v.state.as_known())
                    })
                    .flatten();
                result.condition = match state {
                    Some(wire::AlarmState::Disarmed) => ProAlarmCondition::Disarmed,
                    Some(
                        wire::AlarmState::ArmedHome
                        | wire::AlarmState::ArmedAway
                        | wire::AlarmState::ArmedNight
                        | wire::AlarmState::ArmedVacation
                        | wire::AlarmState::ArmedCustomBypass,
                    ) => ProAlarmCondition::Armed,
                    Some(wire::AlarmState::Arming) => ProAlarmCondition::Arming,
                    Some(wire::AlarmState::Pending | wire::AlarmState::Disarming) => {
                        ProAlarmCondition::Pending
                    }
                    Some(wire::AlarmState::Triggered) => ProAlarmCondition::Triggered,
                    Some(wire::AlarmState::Unspecified) | None => ProAlarmCondition::Unknown,
                };
                result.alarm_icons = alarm
                    .operations()
                    .iter()
                    .enumerate()
                    .map(|(index, operation)| ChoiceIcon {
                        token: index as i32,
                        glyph: match *operation {
                            wire::AlarmOperation::ArmHome => crate::dashboard_icons::glyph(
                                wire::DashboardIcon::Home,
                                &Default::default(),
                            ),
                            wire::AlarmOperation::ArmAway => crate::dashboard_icons::glyph(
                                wire::DashboardIcon::Shield,
                                &Default::default(),
                            ),
                            wire::AlarmOperation::ArmNight => crate::dashboard_icons::glyph(
                                wire::DashboardIcon::Bed,
                                &Default::default(),
                            ),
                            wire::AlarmOperation::Disarm => crate::dashboard_icons::glyph(
                                wire::DashboardIcon::Power,
                                &Default::default(),
                            ),
                            wire::AlarmOperation::Unspecified => {
                                unreachable!("admitted alarm operation")
                            }
                        },
                    })
                    .collect();
                result.alarm_modes = alarm
                    .operations()
                    .iter()
                    .enumerate()
                    .map(|(index, operation)| ChoiceRow {
                        token: index as i32,
                        label: crate::control_text::alarm_operation(*operation)
                            .unwrap_or_default()
                            .into(),
                        selected: matches!(
                            (state, *operation),
                            (
                                Some(wire::AlarmState::Disarmed),
                                wire::AlarmOperation::Disarm
                            ) | (
                                Some(wire::AlarmState::ArmedHome),
                                wire::AlarmOperation::ArmHome
                            ) | (
                                Some(wire::AlarmState::ArmedAway),
                                wire::AlarmOperation::ArmAway
                            ) | (
                                Some(wire::AlarmState::ArmedNight),
                                wire::AlarmOperation::ArmNight
                            )
                        ),
                        disabled: false,
                    })
                    .collect();
            }
            Control::Lock(_) => {
                result.lock_state = ProCoverLock::Unknown;
                if observed && let Some(T::Lock(value)) = capability.r#trait.as_ref() {
                    result.lock_state = match value
                        .observation
                        .value
                        .as_option()
                        .and_then(|value| value.state.as_known())
                    {
                        Some(wire::LockState::Locked) => ProCoverLock::Locked,
                        Some(wire::LockState::Unlocked) => ProCoverLock::Unlocked,
                        Some(
                            wire::LockState::Unspecified
                            | wire::LockState::Locking
                            | wire::LockState::Unlocking
                            | wire::LockState::Jammed
                            | wire::LockState::Opening
                            | wire::LockState::Open,
                        )
                        | None => ProCoverLock::Unknown,
                    };
                }
            }
            Control::Light { .. } => {
                // Observed only: Homeward sends no colour or preset command yet,
                // so the panel shows them as facts and keeps its colour view closed.
                if observed && let Some(T::LightControl(value)) = capability.r#trait.as_ref() {
                    result.light_color = value.observation.rgb.as_option().and_then(rgb_color);
                    result.light_preset = value.observation.preset.clone().unwrap_or_default();
                }
            }
            Control::Switch { .. }
            | Control::Level { .. }
            | Control::Helper(_)
            | Control::Binary
            | Control::Measurement
            | Control::ReadOnly(_)
            | Control::Unsupported => {}
        }
        result
    }
}
// Glyphs the dashboard vocabulary lacks, from Slint Kit's bundled Lucide
// catalog (ISC license), as dashboard_icons.rs carries the others.
const FLAME: &str = "M 12 3 Q 13 7 16 9.5 Q 19 12 19 15 C 19 18.866 15.866 22 12 22 C 8.134 22 5 18.866 5 15 C 5 13.918 5.351 12.865 6 12 C 6 13.381 7.119 14.5 8.5 14.5 C 9.881 14.5 11 13.381 11 12 C 11 10 9.5 9 9.5 7 Q 9.5 5 12 3";
const SNOWFLAKE: &str = "M 10 20 L 8.75 17.5 L 6 18 M 10 4 L 8.75 6.5 L 6 6 M 14 20 L 15.25 17.5 L 18 18 M 14 4 L 15.25 6.5 L 18 6 M 17 21 L 14 15 L 10 15 M 17 3 L 14 9 L 15.5 12 M 2 12 L 8.5 12 L 10 9 M 20 10 L 18.5 12 L 20 14 M 22 12 L 15.5 12 L 14 15 M 4 10 L 5.5 12 L 4 14 M 7 21 L 10 15 L 8.5 12 M 7 3 L 10 9 L 14 9";
fn rgb_color(rgb: &wire::RgbColor) -> Option<(u8, u8, u8)> {
    Some((
        u8::try_from(rgb.red?).ok()?,
        u8::try_from(rgb.green?).ok()?,
        u8::try_from(rgb.blue?).ok()?,
    ))
}
fn lucide(stroke: &str) -> IconGlyph {
    IconGlyph {
        stroke: stroke.into(),
        fill: "".into(),
        viewbox: 24.,
    }
}
fn dashboard_glyph(icon: wire::DashboardIcon) -> IconGlyph {
    crate::dashboard_icons::glyph(icon, &IconGlyph::default())
}
fn mode_glyph(mode: wire::TemperatureMode) -> IconGlyph {
    match mode {
        wire::TemperatureMode::Unspecified => IconGlyph::default(),
        wire::TemperatureMode::Off => dashboard_glyph(wire::DashboardIcon::Power),
        wire::TemperatureMode::Heat => lucide(FLAME),
        wire::TemperatureMode::Cool => lucide(SNOWFLAKE),
        wire::TemperatureMode::Auto => dashboard_glyph(wire::DashboardIcon::Thermostat),
        wire::TemperatureMode::Dry => dashboard_glyph(wire::DashboardIcon::Water),
        wire::TemperatureMode::FanOnly => dashboard_glyph(wire::DashboardIcon::Fan),
    }
}
/// How the panel draws the observed activity: its glyph, its tone, and
/// whether it is heating (the product paints heating in its own tint).
struct Activity {
    glyph: IconGlyph,
    tone: Tone,
    heating: bool,
}
fn activity_paint(value: Option<wire::TemperatureActivity>) -> Activity {
    let (glyph, tone, heating) = match value {
        Some(wire::TemperatureActivity::Heating) => (lucide(FLAME), Tone::Warning, true),
        Some(wire::TemperatureActivity::Cooling) => (lucide(SNOWFLAKE), Tone::Information, false),
        Some(wire::TemperatureActivity::Drying) => (
            dashboard_glyph(wire::DashboardIcon::Water),
            Tone::Information,
            false,
        ),
        Some(wire::TemperatureActivity::FanOnly) => (
            dashboard_glyph(wire::DashboardIcon::Fan),
            Tone::Information,
            false,
        ),
        Some(wire::TemperatureActivity::Idle | wire::TemperatureActivity::Unspecified) | None => {
            (IconGlyph::default(), Tone::Neutral, false)
        }
    };
    Activity {
        glyph,
        tone,
        heating,
    }
}
/// The observation time as a clock reading, with the date when it is not today.
fn observed_label(time: chrono::DateTime<chrono::Utc>) -> String {
    let local = time.with_timezone(&chrono::Local);
    if local.date_naive() == chrono::Local::now().date_naive() {
        local.format("%H:%M").to_string()
    } else {
        local.format("%-d %b %H:%M").to_string()
    }
}
fn hue_degrees((red, green, blue): (u8, u8, u8)) -> i32 {
    let (r, g, b) = (
        f64::from(red) / 255.,
        f64::from(green) / 255.,
        f64::from(blue) / 255.,
    );
    let max = r.max(g).max(b);
    let delta = max - r.min(g).min(b);
    if delta <= f64::EPSILON {
        return 0;
    }
    let sextant = if max == r {
        ((g - b) / delta).rem_euclid(6.)
    } else if max == g {
        (b - r) / delta + 2.
    } else {
        (r - g) / delta + 4.
    };
    (sextant * 60.).round() as i32 % 360
}
fn selected(rows: &[ChoiceRow]) -> i32 {
    rows.iter()
        .find(|row| row.selected)
        .or_else(|| rows.first())
        .map_or(-1, |row| row.token)
}
pub(crate) fn project(
    row: &CardRow,
    device: ProDeviceRow,
    previous: Option<&HomewardDetailRow>,
) -> HomewardDetailRow {
    let previous = previous.filter(|old| {
        old.device.token == device.token && old.device.generation == device.generation
    });
    let metadata = &row.detail;
    let power = if !device.power_known {
        ProSwitchState::Unknown
    } else if device.power_on {
        ProSwitchState::On
    } else {
        ProSwitchState::Off
    };
    let state: SharedString = match power {
        ProSwitchState::Unknown => "Unknown",
        ProSwitchState::On => "On",
        ProSwitchState::Off => "Off",
    }
    .into();
    let primary = device
        .values
        .iter()
        .find(|field| device.has_primary_value && field.token == device.primary_value_token);
    let climate_field = primary
        .as_ref()
        .filter(|field| field.mode == ProValueMode::Single);
    let temperature = row
        .field_keys
        .get("color_temperature_mireds")
        .and_then(|token| device.values.iter().find(|field| field.token == *token));
    let observed = metadata.observed.map(observed_label);
    // Degraded evidence stays visible; otherwise the status names what the
    // light does with what it can do.
    let light_status: SharedString = if !metadata.quality_notice.is_empty() {
        metadata.quality_notice.clone().into()
    } else if !metadata.light_preset.is_empty() {
        format!("Preset · {}", metadata.light_preset).into()
    } else if power == ProSwitchState::On
        && device.can_brightness
        && temperature.is_none()
        && metadata.light_color.is_none()
    {
        "On · dimming only".into()
    } else {
        SharedString::default()
    };
    let mut light_facts = Vec::new();
    if let Some(observed) = &observed {
        light_facts.push(ProDetailFact {
            token: 1,
            label: "Observed".into(),
            value: observed.clone().into(),
            glyph: dashboard_glyph(wire::DashboardIcon::Clock),
            navigable: false,
        });
    }
    if !metadata.light_preset.is_empty() {
        light_facts.push(ProDetailFact {
            token: 2,
            label: "Preset".into(),
            value: metadata.light_preset.clone().into(),
            glyph: dashboard_glyph(wire::DashboardIcon::Scene),
            navigable: false,
        });
    }
    let activity = activity_paint(metadata.climate_activity_value);
    HomewardDetailRow {
        pending: row.pending,
        has_temperature: temperature.is_some(),
        temperature_field: temperature.unwrap_or_default(),
        has_climate_editor: primary.is_some(),
        climate_field: primary.clone().unwrap_or_default(),
        light: ProLight {
            token: device.token,
            generation: device.generation,
            power,
            state_label: state.clone(),
            status: light_status,
            brightness: device.brightness_percent,
            brightness_known: device.brightness_known,
            hue: metadata.light_color.map_or(0, hue_degrees),
            hue_known: metadata.light_color.is_some(),
            color: metadata
                .light_color
                .map_or_else(slint::Color::default, |(red, green, blue)| {
                    slint::Color::from_rgb_u8(red, green, blue)
                }),
            color_label: if !metadata.light_preset.is_empty() {
                metadata.light_preset.clone().into()
            } else if metadata.light_color.is_some() {
                "Custom colour".into()
            } else {
                SharedString::default()
            },
            color_detail: metadata
                .light_color
                .map(|(red, green, blue)| format!("RGB {red} {green} {blue}"))
                .unwrap_or_default()
                .into(),
            ..Default::default()
        },
        light_facts: nested(&light_facts, previous.map(|v| &v.light_facts)),
        switch: ProSwitch {
            token: device.token,
            generation: device.generation,
            state: power,
            state_label: state,
            since: observed
                .as_ref()
                .map(|time| format!("Observed {time}"))
                .unwrap_or_default()
                .into(),
            // The Pro pending glyph would hold input; the switch shows the
            // intended state instead and stays editable.
            pending: false,
        },
        fan: ProFan {
            token: device.token,
            generation: device.generation,
            power,
            state_label: device.value.clone(),
            status: metadata.quality_notice.clone().into(),
            speeds: nested(&metadata.speeds, previous.map(|v| &v.fan.speeds)),
            speed_focus: selected(&metadata.speeds),
            ..Default::default()
        },
        climate: ProClimate {
            token: device.token,
            generation: device.generation,
            setpoint: climate_field
                .map(|field| format!("{} {}", field.lower, field.suffix).into())
                .unwrap_or_else(|| device.value.clone()),
            can_increment: climate_field
                .is_some_and(|field| field.can_step && field.lower_position < 1.),
            can_decrement: climate_field
                .is_some_and(|field| field.can_step && field.lower_position > 0.),
            activity: metadata.climate_activity.clone().into(),
            activity_glyph: activity.glyph,
            activity_tone: activity.tone,
            modes: ProClimateOptions {
                rows: nested(&metadata.modes, previous.map(|v| &v.climate.modes.rows)),
                icons: nested(
                    &metadata.mode_icons,
                    previous.map(|v| &v.climate.modes.icons),
                ),
                focus: selected(&metadata.modes),
            },
            fan_modes: ProClimateOptions {
                rows: nested(
                    &metadata.fan_modes,
                    previous.map(|v| &v.climate.fan_modes.rows),
                ),
                icons: ModelRc::default(),
                focus: selected(&metadata.fan_modes),
            },
            ..Default::default()
        },
        climate_heating: activity.heating,
        cover: ProCover {
            token: device.token,
            generation: device.generation,
            state_label: device.value.clone(),
            status: metadata.quality_notice.clone().into(),
            motion: metadata.motion,
            operations: device.cover_operations.clone(),
            position: metadata.position.unwrap_or_default() as f32,
            position_known: metadata.position.is_some(),
        },
        associated_lock: metadata
            .associated_lock
            .as_ref()
            .map_or_else(Default::default, |lock| ProAssociatedLock {
                token: lock.token,
                generation: lock.generation,
                title: lock.title.clone().into(),
                state: lock.state,
                state_label: lock.label.clone(),
                can_lock: lock.can_lock,
                can_unlock: lock.can_unlock,
            }),
        lock_commands_enabled: metadata
            .associated_lock
            .as_ref()
            .is_some_and(|lock| lock.enabled),
        positioned: metadata
            .position_bounds
            .is_some_and(|(minimum, maximum)| maximum > minimum),
        position_minimum: metadata.position_bounds.map_or(0., |bounds| bounds.0) as f32,
        position_maximum: metadata.position_bounds.map_or(0., |bounds| bounds.1) as f32,
        position_step: metadata.position_step.unwrap_or(0.) as f32,
        alarm_code_required: metadata.alarm_code_required,
        alarm: ProAlarm {
            entered_count: metadata.alarm_code_count as i32,
            code_capacity: 32,
            can_confirm: metadata.alarm_code_count > 0 && metadata.alarm_code_selected,
            code_length: metadata.alarm_code_count.max(4) as i32,
            code_label: format!("{} digits entered", metadata.alarm_code_count).into(),
            prompt: metadata.alarm_prompt.clone().into(),
            confirm_label: metadata.alarm_confirm.clone().into(),
            token: device.token,
            generation: device.generation,
            condition: metadata.condition,
            state_label: device.value.clone(),
            status: metadata.quality_notice.clone().into(),
            modes: nested(&metadata.alarm_modes, previous.map(|v| &v.alarm.modes)),
            mode_icons: nested(&metadata.alarm_icons, previous.map(|v| &v.alarm.mode_icons)),
            mode_focus: selected(&metadata.alarm_modes),
        },
        device,
    }
}

// A cohesive light trait replaces only the power/level capabilities it actually
// supplies. Other traits and ambiguous multiple light controllers remain visible.
pub(super) fn primary_light(entity: &wire::EntityViewEntry) -> Option<&wire::Capability> {
    let mut lights = entity
        .capabilities
        .iter()
        .filter(|capability| matches!(Control::parse(capability), Control::Light { .. }));
    let primary = lights.next()?;
    lights.next().is_none().then_some(primary)
}
pub(super) fn covered_by_light(
    entity: &wire::EntityViewEntry,
    capability: &wire::Capability,
    primary: Option<&wire::Capability>,
) -> bool {
    use home_client_actions::{WriteAccess, write_access};
    use home_client_model::api::CapabilityName;
    use wire::capability::Trait as T;
    let Some(primary) = primary else {
        return false;
    };
    if !capability.__buffa_unknown_fields.is_empty() {
        return false;
    }
    let Some(T::LightControl(light)) = primary.r#trait.as_ref() else {
        return false;
    };
    let Ok(primary_name) = CapabilityName::try_from(primary.name.as_str()) else {
        return false;
    };
    let Ok(sibling_name) = CapabilityName::try_from(capability.name.as_str()) else {
        return false;
    };
    // A presentation preference must never hide the only admitted command.
    if write_access(entity, &sibling_name) == WriteAccess::Allowed
        && write_access(entity, &primary_name) != WriteAccess::Allowed
    {
        return false;
    }
    if let Some(T::Power(power)) = capability.r#trait.as_ref() {
        // Known sibling observations are not discarded merely because another
        // trait can send the same kind of command. Only a matching source frame
        // can be represented by the cohesive panel without losing information.
        if power.observation.is_set() {
            let on = match power.observation.value {
                Some(value) => match value.as_known() {
                    Some(wire::PowerValue::On) => Some(true),
                    Some(wire::PowerValue::Off) => Some(false),
                    Some(wire::PowerValue::Unspecified) | None => return false,
                },
                None => None,
            };
            if !power.observation.__buffa_unknown_fields.is_empty()
                || !light.observation.is_set()
                || on != light.observation.on
                || power.observation.quality != light.observation.quality
                || power.observation.observed_time != light.observation.observed_time
            {
                return false;
            }
        }
        light.supports_power
            && power.__buffa_unknown_fields.is_empty()
            && entity
                .capabilities
                .iter()
                .filter(|cap| matches!(cap.r#trait, Some(T::Power(_))))
                .count()
                == 1
    } else if let Some(T::Level(level)) = capability.r#trait.as_ref() {
        if level.observation.is_set()
            && (!level.observation.__buffa_unknown_fields.is_empty()
                || !light.observation.is_set()
                || level.observation.percentage != light.observation.brightness_percent
                || level.observation.quality != light.observation.quality
                || level.observation.observed_time != light.observation.observed_time)
        {
            return false;
        }
        level.__buffa_unknown_fields.is_empty()
            && light.brightness_percent.is_set()
            && level.bounds.is_set()
            && level.bounds.minimum == light.brightness_percent.minimum
            && level.bounds.maximum == light.brightness_percent.maximum
            && level.bounds.step == light.brightness_percent.step
            && entity
                .capabilities
                .iter()
                .filter(|cap| matches!(cap.r#trait, Some(T::Level(_))))
                .count()
                == 1
    } else {
        false
    }
}

#[cfg(test)]
mod tests {
    use super::super::fixture;
    use super::*;

    fn composite() -> wire::EntityViewEntry {
        let entity = "homes/synthetic/entities/lamp";
        fixture::entry(
            entity,
            "Reading light",
            vec![
                fixture::capability(
                    &format!("{entity}/capabilities/light"),
                    wire::capability::Trait::LightControl(Box::new(wire::LightControlCapability {
                        supports_power: true,
                        observation: wire::LightControlObservation {
                            on: Some(false),
                            quality: fixture::quality().into(),
                            ..Default::default()
                        }
                        .into(),
                        brightness_percent: wire::NumberBounds {
                            minimum: 0.1,
                            maximum: 0.3,
                            step: Some(0.1),
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    })),
                ),
                fixture::power(
                    &format!("{entity}/capabilities/power"),
                    Some(wire::PowerValue::Off),
                ),
                fixture::capability(
                    &format!("{entity}/capabilities/level"),
                    wire::capability::Trait::Level(Box::new(wire::LevelCapability {
                        bounds: wire::PercentageBounds {
                            minimum: 0.1,
                            maximum: 0.3,
                            step: Some(0.1),
                            ..Default::default()
                        }
                        .into(),
                        ..Default::default()
                    })),
                ),
            ],
        )
    }
    #[test]
    fn cohesive_light_hides_only_equivalent_admitted_siblings() {
        let mut entity = composite();
        assert!(covered_by_light(
            &entity,
            &entity.capabilities[1],
            primary_light(&entity)
        ));
        assert!(covered_by_light(
            &entity,
            &entity.capabilities[2],
            primary_light(&entity)
        ));
        entity.capabilities[0].action_access = wire::ActionAccess::Denied.into();
        assert!(!covered_by_light(
            &entity,
            &entity.capabilities[1],
            primary_light(&entity)
        ));
        assert!(!covered_by_light(
            &entity,
            &entity.capabilities[2],
            primary_light(&entity)
        ));
        entity.capabilities[0].action_access = wire::ActionAccess::Allowed.into();
        fixture::light_mut(&mut entity).supports_power = false;
        assert!(!covered_by_light(
            &entity,
            &entity.capabilities[1],
            primary_light(&entity)
        ));
        fixture::light_mut(&mut entity)
            .brightness_percent
            .as_option_mut()
            .unwrap()
            .step = Some(0.05);
        assert!(!covered_by_light(
            &entity,
            &entity.capabilities[2],
            primary_light(&entity)
        ));
        let mut second = entity.capabilities[0].clone();
        second.name = "homes/synthetic/entities/lamp/capabilities/other-light".into();
        entity.capabilities.push(second);
        assert!(primary_light(&entity).is_none());
    }

    #[test]
    fn a_cohesive_panel_never_hides_a_different_or_more_precise_sibling_observation() {
        let mut entity = composite();
        fixture::light_mut(&mut entity)
            .observation
            .as_option_mut()
            .unwrap()
            .on = None;
        assert!(!covered_by_light(
            &entity,
            &entity.capabilities[1],
            primary_light(&entity)
        ));
        fixture::light_mut(&mut entity)
            .observation
            .as_option_mut()
            .unwrap()
            .on = Some(false);
        let Some(wire::capability::Trait::Level(level)) = entity.capabilities[2].r#trait.as_mut()
        else {
            panic!()
        };
        level.observation = wire::LevelObservation {
            percentage: Some(0.2),
            quality: fixture::quality().into(),
            ..Default::default()
        }
        .into();
        assert!(!covered_by_light(
            &entity,
            &entity.capabilities[2],
            primary_light(&entity)
        ));
        fixture::light_mut(&mut entity)
            .observation
            .as_option_mut()
            .unwrap()
            .brightness_percent = Some(0.2);
        assert!(covered_by_light(
            &entity,
            &entity.capabilities[2],
            primary_light(&entity)
        ));
    }
    #[test]
    fn distinct_power_siblings_are_never_assumed_equivalent() {
        let mut entity = composite();
        entity.capabilities.push(fixture::power(
            "homes/synthetic/entities/lamp/capabilities/secondary-power",
            Some(wire::PowerValue::On),
        ));
        assert!(!covered_by_light(
            &entity,
            &entity.capabilities[1],
            primary_light(&entity)
        ));
    }
    #[test]
    fn fractional_light_and_zero_token_switch_keep_their_observations() {
        let row = CardRow {
            brightness_percent: 0.3,
            brightness_known: true,
            power_known: true,
            power_on: true,
            ..Default::default()
        };
        let panel = project(&row, row.ui(None, &Default::default()), None);
        assert_eq!(panel.light.brightness, 0.3);
        assert!(panel.light.brightness_known);
        assert_eq!(panel.switch.token, 0);
        assert_eq!(panel.switch.state, ProSwitchState::On);
    }
    #[test]
    fn a_composite_light_projects_one_panel_without_merging_capability_authority() {
        let entity = composite();
        let name =
            home_client_model::api::EntityName::try_from(entity.entity.name.as_str()).unwrap();
        let view = fixture::capture(vec![entity]);
        let request = crate::dashboard::ProjectionRequest {
            detail: Some(name),
            dashboard: crate::dashboard::LayoutState::Ready(std::sync::Arc::new(
                crate::dashboard::joined_fixture(&view),
            )),
            ..Default::default()
        };
        let store = super::super::DeviceStore::default();
        let projected = store.project(home_client_cache::CachePhase::Live, Some(&view), &request);
        assert_eq!(projected.rows.len(), 1);
        assert_eq!(projected.rows[0].kind, crate::ProDeviceKind::Light);
        assert!(projected.rows[0].can_power);
        assert!(
            !projected.rows[0].can_brightness,
            "a fractional-only brightness domain has no integer editing values"
        );
    }
}
