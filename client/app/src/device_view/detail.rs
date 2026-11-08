//! Family panel projections. Capabilities and observations are interpreted once,
//! beside the command boundary; UI choice tokens still resolve through that boundary.
use super::{
    capability::Control,
    observation,
    presentation::{CardRow, nested},
};
use crate::{
    ChoiceIcon, ChoiceRow, HomewardDetailRow, ProAlarm, ProAlarmCondition, ProAssociatedLock,
    ProClimate, ProClimateOptions, ProCover, ProCoverLock, ProCoverMotion, ProDeviceRow, ProFan,
    ProLight, ProSwitch, ProSwitchState, ProValueMode,
};
use home_wire_contract::home::api::v2 as wire;
use slint::{Model, SharedString};

#[derive(Clone, Debug, Default)]
pub(crate) struct Metadata {
    pub associated_only: bool,
    associated_lock: Option<AssociatedLock>,
    lock_state: ProCoverLock,
    modes: Vec<ChoiceRow>,
    climate_activity: String,
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
impl Metadata {
    pub(super) fn associate_lock(&mut self, row: &CardRow, title: &str) {
        self.associated_lock = Some(AssociatedLock {
            token: row.token,
            generation: row.generation,
            title: title.into(),
            label: row.value.clone(),
            state: row.detail.lock_state,
            enabled: row.access == crate::ProControlAccess::Allowed
                && !row.command_unavailable
                && !matches!(
                    row.phase,
                    crate::ProActionPhase::Preparing
                        | crate::ProActionPhase::Sending
                        | crate::ProActionPhase::Accepted
                        | crate::ProActionPhase::Dispatched
                        | crate::ProActionPhase::Acknowledged
                        | crate::ProActionPhase::Uncertain
                        | crate::ProActionPhase::Interrupted
                ),
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
            ..Default::default()
        };
        let observed = observation::evidence(capability).is_some();
        use wire::capability::Trait as T;
        match control {
            Control::Climate(climate) => {
                let Some(T::TemperatureControl(value)) = capability.r#trait.as_ref() else {
                    return result;
                };
                result.climate_activity = observed
                    .then(|| value.observation.activity.and_then(|v| v.as_known()))
                    .flatten()
                    .filter(|activity| climate.observes().activities.contains(activity))
                    .map(super::panel::activity_label)
                    .unwrap_or_default()
                    .into();
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
            Control::Switch { .. }
            | Control::Light { .. }
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
    HomewardDetailRow {
        has_temperature: temperature.is_some(),
        temperature_field: temperature.unwrap_or_default(),
        has_climate_editor: primary.is_some(),
        climate_field: primary.clone().unwrap_or_default(),
        light: ProLight {
            token: device.token,
            generation: device.generation,
            power,
            state_label: state.clone(),
            status: metadata.quality_notice.clone().into(),
            brightness: device.brightness_percent,
            brightness_known: device.brightness_known,
            ..Default::default()
        },
        switch: ProSwitch {
            token: device.token,
            generation: device.generation,
            state: power,
            state_label: state,
            since: device.freshness.clone(),
            pending: matches!(
                device.phase,
                crate::ProActionPhase::Preparing
                    | crate::ProActionPhase::Sending
                    | crate::ProActionPhase::Accepted
                    | crate::ProActionPhase::Dispatched
                    | crate::ProActionPhase::Acknowledged
            ),
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
            modes: ProClimateOptions {
                rows: nested(&metadata.modes, previous.map(|v| &v.climate.modes.rows)),
                focus: selected(&metadata.modes),
            },
            fan_modes: ProClimateOptions {
                rows: nested(
                    &metadata.fan_modes,
                    previous.map(|v| &v.climate.fan_modes.rows),
                ),
                focus: selected(&metadata.fan_modes),
            },
            ..Default::default()
        },
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
