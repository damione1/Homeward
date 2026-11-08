//! Source-bound reusable modules. A presentation family never grants a command.
use super::{
    DeviceIntent, DeviceStore, Refusal, Slot, active_slot,
    number::{Bounds, NumericInput},
};
use crate::{
    ProColorMode, ProControlFamily, ProControlRole, ProModuleEvent, ProModuleIntent, ProValueEvent,
};
use home_client_model::{
    api as model,
    control_modules::{ControlModule, ModuleDefinition, ModuleRole, NativeRange, VisualFamily},
};
use home_wire_contract::{control_modules_contract, home::api::v2 as wire};

#[derive(Clone, Debug)]
pub(super) enum Value {
    Power(bool),
    Number(f64),
    Choice(model::SelectorKey),
    Rgb(model::Rgb8),
    Motion(model::CoverOperation),
}
#[derive(Clone, Debug)]
pub(crate) struct Command {
    pub(super) role: ModuleRole,
    pub(super) value: Value,
}
#[derive(Clone, Debug, Default)]
pub(super) struct Draft {
    pub text: Option<String>,
    pub gesture: Option<f64>,
    pub active: bool,
    pub mode: Option<ProColorMode>,
}

pub(super) fn current(binding: &ControlModule, capability: &wire::Capability) -> bool {
    use wire::capability::Trait as T;
    if !control_modules_contract::capability_modules(capability)
        .is_ok_and(|modules| modules.contains(binding))
    {
        return false;
    }
    let range_matches = |a: &NativeRange, b: &wire::NumberBounds| {
        a.minimum == b.minimum && a.maximum == b.maximum && a.step == b.step
    };
    let choices_match = |a: &Vec<home_client_model::control_modules::Choice>,
                         b: &wire::ControlChoices| {
        a.len() == b.choices.len()
            && a.iter()
                .zip(&b.choices)
                .all(|(a, b)| a.key.as_str() == b.key && a.display_name == b.display_name)
    };
    match (capability.r#trait.as_ref(), &binding.definition) {
        (Some(T::Power(v)), ModuleDefinition::Power) => v.supports_set,
        (Some(T::LightControl(v)), ModuleDefinition::Power) => v.supports_power,
        (Some(T::FanControl(v)), ModuleDefinition::Power) => v.supports_power,
        (Some(T::Level(v)), ModuleDefinition::Range(r)) => {
            v.supports_set
                && v.bounds.as_option().is_some_and(|b| {
                    r.minimum == b.minimum && r.maximum == b.maximum && r.step == b.step
                })
        }
        (Some(T::LightControl(v)), ModuleDefinition::Range(r)) => v
            .brightness_percent
            .as_option()
            .is_some_and(|b| range_matches(r, b)),
        (Some(T::FanControl(v)), ModuleDefinition::Range(r)) => {
            matches!(v.level.definition.as_ref(),Some(wire::control_level_definition::Definition::Range(b)) if range_matches(r,b))
        }
        (Some(T::FanControl(v)), ModuleDefinition::Steps(c)) => {
            matches!(v.level.definition.as_ref(),Some(wire::control_level_definition::Definition::Steps(b)) if choices_match(c,b))
        }
        (Some(T::LightControl(v)), ModuleDefinition::Preset(c)) => {
            v.presets.as_option().is_some_and(|b| choices_match(c, b))
        }
        (
            Some(T::LightControl(v)),
            ModuleDefinition::Color {
                temperature_mireds,
                rgb,
            },
        ) => {
            *rgb == v.rgb
                && match (temperature_mireds, v.color_temperature_mireds.as_option()) {
                    (Some(a), Some(b)) => range_matches(a, b),
                    (None, None) => true,
                    _ => false,
                }
        }
        (Some(T::Cover(v)), ModuleDefinition::Range(r)) => v
            .definition
            .set_position_percent
            .as_option()
            .is_some_and(|b| r.minimum == b.minimum && r.maximum == b.maximum && r.step == b.step),
        (Some(T::Cover(v)), ModuleDefinition::Motion(operations)) => {
            operations.len() == v.definition.operations.len()
                && operations.iter().all(|operation| {
                    v.definition.operations.iter().any(|o| {
                        o.as_known()
                            == Some(match operation {
                                model::CoverOperation::Open => wire::CoverOperation::Open,
                                model::CoverOperation::Close => wire::CoverOperation::Close,
                                model::CoverOperation::Stop => wire::CoverOperation::Stop,
                            })
                    })
                })
        }
        _ => false,
    }
}
pub(super) fn bounds(range: &NativeRange) -> Bounds {
    Bounds::parse(&wire::NumberBounds {
        minimum: range.minimum,
        maximum: range.maximum,
        step: range.step,
        ..Default::default()
    })
    .expect("admitted finite module range")
}
pub(super) fn family(value: VisualFamily) -> ProControlFamily {
    match value {
        VisualFamily::Binary => ProControlFamily::Binary,
        VisualFamily::ContinuousLevel => ProControlFamily::ContinuousLevel,
        VisualFamily::SteppedLevel => ProControlFamily::SteppedLevel,
        VisualFamily::Blind => ProControlFamily::Blind,
        VisualFamily::Garage => ProControlFamily::Garage,
    }
}
pub(super) fn role(value: ModuleRole) -> ProControlRole {
    match value {
        ModuleRole::Power => ProControlRole::Power,
        ModuleRole::Range => ProControlRole::Range,
        ModuleRole::Steps => ProControlRole::Steps,
        ModuleRole::Color => ProControlRole::Color,
        ModuleRole::Preset => ProControlRole::Preset,
        ModuleRole::Motion => ProControlRole::Motion,
    }
}
fn observed_number(slot: &Slot, role: ModuleRole) -> Option<f64> {
    use wire::capability::Trait as T;
    match (slot.capability.r#trait.as_ref(), role) {
        (Some(T::LightControl(v)), ModuleRole::Range) => v.observation.brightness_percent,
        (Some(T::LightControl(v)), ModuleRole::Color) => {
            v.observation.color_temperature_mireds.map(f64::from)
        }
        (Some(T::Level(v)), ModuleRole::Range) => v.observation.percentage,
        (Some(T::FanControl(v)), ModuleRole::Range) => v.observation.level,
        (Some(T::Cover(v)), ModuleRole::Range) => v
            .observation
            .value
            .as_option()
            .and_then(|v| v.position_percent),
        _ => None,
    }
    .filter(|v| v.is_finite())
}
impl DeviceStore {
    pub fn module_event(
        &self,
        revision: i32,
        event: ProModuleIntent,
    ) -> Result<Option<DeviceIntent>, Refusal> {
        let mut state = self.0.lock().expect("device store mutex");
        let local = matches!(
            event.event,
            ProModuleEvent::ColorMode | ProModuleEvent::Cancel
        ) || (event.event == ProModuleEvent::Numeric
            && event.value.event == ProValueEvent::Cancel);
        let slot = if local {
            state
                .context
                .as_ref()
                .filter(|c| c.source_current() && c.revision == revision)
                .and_then(|c| {
                    usize::try_from(event.identity.token)
                        .ok()
                        .and_then(|i| c.slots.get(i))
                })
                .filter(|slot| slot.generation.0 == event.identity.generation)
                .ok_or(Refusal::Stale)?
                .clone()
        } else {
            active_slot(
                &state,
                event.identity.token,
                event.identity.generation,
                revision,
            )?
            .clone()
        };
        let binding = slot.module.as_ref().ok_or(Refusal::Unsupported)?;
        if !local && !super::observation::fresh(&slot.capability) {
            return Err(Refusal::Unavailable);
        }
        if role(binding.role) != event.role
            || (!local && !binding.allowed)
            || !current(binding, &slot.capability)
        {
            return Err(Refusal::Stale);
        }
        if !local
            && state.actions.iter().any(|(target, handle)| {
                target == &slot.target && super::progress::blocks_input(Some(&handle.snapshot()))
            })
        {
            return Err(Refusal::Capacity);
        }
        let draft = state.module_drafts.entry(slot.generation).or_default();
        if event.event == ProModuleEvent::Numeric && event.value.event == ProValueEvent::Cancel {
            draft.text = None;
            draft.gesture = None;
            draft.active = false;
            return Ok(None);
        }
        let value = match event.event {
            ProModuleEvent::Cancel => {
                *draft = Draft::default();
                return Ok(None);
            }
            ProModuleEvent::ColorMode => {
                let ModuleDefinition::Color {
                    temperature_mireds,
                    rgb,
                } = &binding.definition
                else {
                    return Err(Refusal::Unsupported);
                };
                if !matches!(
                    (event.color_mode, temperature_mireds.is_some(), *rgb),
                    (ProColorMode::Temperature, true, _) | (ProColorMode::Rgb, _, true)
                ) {
                    return Err(Refusal::Unsupported);
                }
                draft.mode = Some(event.color_mode);
                draft.text = None;
                draft.gesture = None;
                draft.active = false;
                return Ok(None);
            }
            ProModuleEvent::Power if binding.role == ModuleRole::Power => {
                Value::Power(event.power == crate::ProPowerIntent::On)
            }
            ProModuleEvent::Choice => {
                let choices = match &binding.definition {
                    ModuleDefinition::Steps(v) | ModuleDefinition::Preset(v) => v,
                    _ => return Err(Refusal::Unsupported),
                };
                let index =
                    usize::try_from(event.choice_token).map_err(|_| Refusal::InvalidValue)?;
                Value::Choice(choices.get(index).ok_or(Refusal::Stale)?.key.clone())
            }
            ProModuleEvent::RgbCommit => {
                if !matches!(
                    binding.definition,
                    ModuleDefinition::Color { rgb: true, .. }
                ) || draft.mode == Some(ProColorMode::Temperature)
                {
                    return Err(Refusal::Unsupported);
                }
                if event.rgb.alpha != 255 {
                    return Err(Refusal::InvalidValue);
                }
                Value::Rgb(model::Rgb8 {
                    red: u8::try_from(event.rgb.red).map_err(|_| Refusal::InvalidValue)?,
                    green: u8::try_from(event.rgb.green).map_err(|_| Refusal::InvalidValue)?,
                    blue: u8::try_from(event.rgb.blue).map_err(|_| Refusal::InvalidValue)?,
                })
            }
            ProModuleEvent::Motion => {
                let operation = match event.motion {
                    crate::ProCoverOperation::Open => model::CoverOperation::Open,
                    crate::ProCoverOperation::Close => model::CoverOperation::Close,
                    crate::ProCoverOperation::Stop => model::CoverOperation::Stop,
                };
                let ModuleDefinition::Motion(operations) = &binding.definition else {
                    return Err(Refusal::Unsupported);
                };
                if !operations.contains(&operation) {
                    return Err(Refusal::Unsupported);
                }
                Value::Motion(operation)
            }
            ProModuleEvent::Numeric => {
                let range = match &binding.definition {
                    ModuleDefinition::Range(v) => v,
                    ModuleDefinition::Color {
                        temperature_mireds: Some(v),
                        ..
                    } if draft.mode != Some(ProColorMode::Rgb) => v,
                    _ => return Err(Refusal::Unsupported),
                };
                let limits = bounds(range);
                let value = match event.value.event {
                    ProValueEvent::Draft => {
                        if event.value.lower.len() > 128 {
                            return Err(Refusal::InvalidValue);
                        }
                        draft.text = Some(event.value.lower.to_string());
                        return Ok(None);
                    }
                    ProValueEvent::Start => {
                        draft.gesture = None;
                        draft.active = true;
                        return Ok(None);
                    }
                    ProValueEvent::Cancel => {
                        draft.text = None;
                        draft.gesture = None;
                        draft.active = false;
                        return Ok(None);
                    }
                    ProValueEvent::Position => {
                        if !draft.active {
                            return Err(Refusal::Stale);
                        }
                        let p = f64::from(event.value.position);
                        if !p.is_finite() || !(0.0..=1.0).contains(&p) {
                            return Err(Refusal::InvalidValue);
                        }
                        draft.gesture = Some(
                            limits
                                .canonical(range.minimum + p * (range.maximum - range.minimum))?,
                        );
                        return Ok(None);
                    }
                    ProValueEvent::Commit => {
                        if !draft.active {
                            return Err(Refusal::Stale);
                        }
                        draft.active = false;
                        draft.gesture.take().ok_or(Refusal::InvalidValue)?
                    }
                    ProValueEvent::Submit => {
                        draft.active = false;
                        draft.gesture = None;
                        NumericInput::Text(event.value.lower.to_string()).admit(limits)?
                    }
                    ProValueEvent::Step | ProValueEvent::Navigate => {
                        if event.value.event == ProValueEvent::Navigate && !draft.active {
                            return Err(Refusal::Stale);
                        }
                        let current = draft
                            .gesture
                            .or(draft
                                .text
                                .clone()
                                .map(|v| NumericInput::Text(v).admit(limits))
                                .transpose()?)
                            .or_else(|| observed_number(&slot, binding.role))
                            .ok_or(Refusal::Unavailable)?;
                        let navigation = if event.value.event == ProValueEvent::Step {
                            if event.value.direction == crate::StepDirection::Increment {
                                crate::NavigationStep::Next
                            } else {
                                crate::NavigationStep::Previous
                            }
                        } else {
                            event.value.navigation
                        };
                        let next = match navigation {
                            crate::NavigationStep::First => range.minimum,
                            crate::NavigationStep::Last => range.maximum,
                            crate::NavigationStep::Next => {
                                current + range.step.ok_or(Refusal::Unsupported)?
                            }
                            crate::NavigationStep::Previous => {
                                current - range.step.ok_or(Refusal::Unsupported)?
                            }
                            crate::NavigationStep::PageNext => {
                                current + 10. * range.step.ok_or(Refusal::Unsupported)?
                            }
                            crate::NavigationStep::PagePrevious => {
                                current - 10. * range.step.ok_or(Refusal::Unsupported)?
                            }
                        };
                        let next = limits.canonical(next.clamp(range.minimum, range.maximum))?;
                        if event.value.event == ProValueEvent::Step {
                            draft.active = false;
                            draft.gesture = None;
                        } else {
                            draft.gesture = Some(next);
                        }
                        draft.text = Some(next.to_string());
                        return Ok(None);
                    }
                    ProValueEvent::SelectOpen
                    | ProValueEvent::SelectNavigate
                    | ProValueEvent::SelectActivate
                    | ProValueEvent::SelectDismiss
                    | ProValueEvent::SelectQuery
                    | ProValueEvent::SelectTypeahead => return Err(Refusal::Unsupported),
                };
                draft.text = None;
                Value::Number(value)
            }
            ProModuleEvent::Power => return Err(Refusal::Unsupported),
        };
        Ok(Some(DeviceIntent::Module(Command {
            role: binding.role,
            value,
        })))
    }
}

pub(super) fn arguments(
    slot: &Slot,
    command: Command,
) -> Result<wire::execute_capability_request::Command, Refusal> {
    use wire::{capability::Trait as T, execute_capability_request::Command as C};
    if !super::observation::fresh(&slot.capability) {
        return Err(Refusal::Unavailable);
    }
    let binding = slot
        .module
        .as_ref()
        .filter(|b| b.role == command.role && b.allowed && current(b, &slot.capability))
        .ok_or(Refusal::Stale)?;
    match (&binding.definition, &command.value) {
        (ModuleDefinition::Power, Value::Power(_)) => {}
        (ModuleDefinition::Range(range), Value::Number(v)) => {
            bounds(range).admit(*v)?;
        }
        (
            ModuleDefinition::Color {
                temperature_mireds: Some(range),
                ..
            },
            Value::Number(v),
        ) => {
            bounds(range).admit(*v)?;
            if v.fract() != 0.0 {
                return Err(Refusal::InvalidValue);
            }
        }
        (ModuleDefinition::Color { rgb: true, .. }, Value::Rgb(_)) => {}
        (
            ModuleDefinition::Steps(choices) | ModuleDefinition::Preset(choices),
            Value::Choice(v),
        ) if choices.iter().any(|c| &c.key == v) => {}
        (ModuleDefinition::Motion(operations), Value::Motion(v)) if operations.contains(v) => {}
        _ => return Err(Refusal::Unsupported),
    }
    let power = |on| wire::SetPowerCommand {
        value: if on {
            wire::PowerValue::On
        } else {
            wire::PowerValue::Off
        }
        .into(),
        ..Default::default()
    };
    match (
        slot.capability.r#trait.as_ref(),
        command.role,
        command.value,
    ) {
        (Some(T::Power(_)), ModuleRole::Power, Value::Power(v)) => Ok(C::Power(power(v).into())),
        (Some(T::Level(_)), ModuleRole::Range, Value::Number(v)) => Ok(C::Level(
            wire::SetLevelCommand {
                target: Some(wire::set_level_command::Target::Percentage(v)),
                ..Default::default()
            }
            .into(),
        )),
        (Some(T::LightControl(_)), role, value) => {
            let mut light = wire::SetLightCommand::default();
            match (role, value) {
                (ModuleRole::Power, Value::Power(v)) => light.power = power(v).into(),
                (ModuleRole::Range, Value::Number(v)) => {
                    light.level = wire::SetLevelCommand {
                        target: Some(wire::set_level_command::Target::Percentage(v)),
                        ..Default::default()
                    }
                    .into()
                }
                (ModuleRole::Color, Value::Number(v)) => {
                    light.color_temperature_mireds = Some(v as i32)
                }
                (ModuleRole::Color, Value::Rgb(v)) => {
                    light.rgb = wire::RgbColor {
                        red: Some(v.red.into()),
                        green: Some(v.green.into()),
                        blue: Some(v.blue.into()),
                        ..Default::default()
                    }
                    .into()
                }
                (ModuleRole::Preset, Value::Choice(v)) => light.preset = Some(v.as_str().into()),
                _ => return Err(Refusal::Unsupported),
            }
            Ok(C::Light(light.into()))
        }
        (Some(T::FanControl(_)), role, value) => {
            let mut fan = wire::SetFanCommand::default();
            match (role, value) {
                (ModuleRole::Power, Value::Power(v)) => fan.power = power(v).into(),
                (ModuleRole::Steps, Value::Choice(v)) => fan.speed = Some(v.as_str().into()),
                (ModuleRole::Range, Value::Number(v)) => fan.level = Some(v),
                _ => return Err(Refusal::Unsupported),
            }
            Ok(C::Fan(fan.into()))
        }
        (Some(T::Cover(_)), ModuleRole::Range, Value::Number(v)) => Ok(C::Cover(
            wire::SetCoverCommand {
                target: Some(wire::set_cover_command::Target::PositionPercent(v)),
                ..Default::default()
            }
            .into(),
        )),
        (Some(T::Cover(_)), ModuleRole::Motion, Value::Motion(v)) => Ok(C::Cover(
            wire::SetCoverCommand {
                target: Some(wire::set_cover_command::Target::Operation(
                    match v {
                        model::CoverOperation::Open => wire::CoverOperation::Open,
                        model::CoverOperation::Close => wire::CoverOperation::Close,
                        model::CoverOperation::Stop => wire::CoverOperation::Stop,
                    }
                    .into(),
                )),
                ..Default::default()
            }
            .into(),
        )),
        _ => Err(Refusal::Unsupported),
    }
}

// Nested Slint models are materialized only on the UI thread.
#[derive(Clone, Debug, Default)]
pub(crate) struct Row {
    state: crate::ProModuleState,
    role: ProControlRole,
    power: crate::ProPowerReading,
    range: crate::ProDeviceValue,
    choices: Vec<crate::ProDeviceChoice>,
    temperature_supported: bool,
    rgb_supported: bool,
    observed_color_mode: ProColorMode,
    editor_color_mode: ProColorMode,
    temperature: crate::ProDeviceValue,
    rgb_known: bool,
    rgb: crate::SrgbColorView,
    motion_operations: Vec<crate::ProCoverChoice>,
}
impl Row {
    pub fn ui(&self, previous: Option<&crate::ProControlModule>) -> crate::ProControlModule {
        let previous = previous
            .filter(|row| row.state.identity == self.state.identity && row.role == self.role);
        crate::ProControlModule {
            state: self.state.clone(),
            role: self.role,
            power: self.power,
            range: self.range.clone(),
            choices: super::presentation::nested(&self.choices, previous.map(|row| &row.choices)),
            temperature_supported: self.temperature_supported,
            rgb_supported: self.rgb_supported,
            observed_color_mode: self.observed_color_mode,
            editor_color_mode: self.editor_color_mode,
            temperature: self.temperature.clone(),
            rgb_known: self.rgb_known,
            rgb: self.rgb.clone(),
            motion_operations: super::presentation::nested(
                &self.motion_operations,
                previous.map(|row| &row.motion_operations),
            ),
        }
    }
}
pub(super) fn project(
    slot: &Slot,
    card: &super::presentation::CardRow,
    draft: Option<&Draft>,
    ready: bool,
) -> Row {
    use wire::capability::Trait as T;
    let binding = slot.module.as_ref().expect("module slot");
    let known = super::observation::evidence(&slot.capability).is_some();
    let fresh = super::observation::fresh(&slot.capability);
    let mut row = Row {
        state: crate::ProModuleState {
            identity: crate::ProIdentity {
                token: card.token,
                generation: card.generation,
            },
            access: card.access,
            available: ready && fresh && !card.command_unavailable,
            phase: card.phase,
            label: match binding.role {
                ModuleRole::Power => "Power",
                ModuleRole::Range => "Level",
                ModuleRole::Steps => "Speed",
                ModuleRole::Color => "Color",
                ModuleRole::Preset => "Preset",
                ModuleRole::Motion => "Motion",
            }
            .into(),
            observation: card.value.clone(),
            status: card.action_label.clone(),
        },
        role: role(binding.role),
        ..Default::default()
    };
    let observed = known.then(|| observed_number(slot, binding.role)).flatten();
    let numeric = |range: &NativeRange, suffix: &str| {
        let text = draft
            .and_then(|d| d.text.clone())
            .or_else(|| {
                draft
                    .and_then(|d| d.gesture)
                    .or(observed)
                    .map(|v| v.to_string())
            })
            .unwrap_or_default();
        let value = text.parse::<f64>().ok().filter(|v| v.is_finite());
        let span = range.maximum - range.minimum;
        crate::ProDeviceValue {
            token: 0,
            kind: crate::ProValueKind::Numeric,
            mode: crate::ProValueMode::Single,
            label: row.state.label.clone(),
            lower: text.into(),
            suffix: suffix.into(),
            description: format!(
                "{} – {}{}",
                range.minimum,
                range.maximum,
                range
                    .step
                    .map(|s| format!(" · step {s}"))
                    .unwrap_or_default()
            )
            .into(),
            lower_position: value
                .filter(|_| span > 0.)
                .map(|v| ((v - range.minimum) / span).clamp(0., 1.) as f32)
                .unwrap_or(0.),
            slider_enabled: span > 0.,
            can_step: range.step.is_some(),
            ..Default::default()
        }
    };
    match &binding.definition {
        ModuleDefinition::Power => {
            let value = if known {
                match slot.capability.r#trait.as_ref() {
                    Some(T::LightControl(v)) => v.observation.on,
                    Some(T::FanControl(v)) => v.observation.on,
                    Some(T::Power(v)) => match v.observation.value.and_then(|v| v.as_known()) {
                        Some(wire::PowerValue::On) => Some(true),
                        Some(wire::PowerValue::Off) => Some(false),
                        _ => None,
                    },
                    _ => None,
                }
            } else {
                None
            };
            row.power = match value {
                Some(true) => crate::ProPowerReading::On,
                Some(false) => crate::ProPowerReading::Off,
                None => crate::ProPowerReading::Unknown,
            };
        }
        ModuleDefinition::Range(v) => {
            row.range = numeric(
                v,
                if matches!(slot.capability.r#trait.as_ref(), Some(T::FanControl(_))) {
                    ""
                } else {
                    "%"
                },
            )
        }
        ModuleDefinition::Steps(choices) | ModuleDefinition::Preset(choices) => {
            let selected = if known {
                match (slot.capability.r#trait.as_ref(), binding.role) {
                    (Some(T::FanControl(v)), ModuleRole::Steps) => v.observation.speed.as_deref(),
                    (Some(T::LightControl(v)), ModuleRole::Preset) => {
                        v.observation.preset.as_deref()
                    }
                    _ => None,
                }
            } else {
                None
            };
            row.choices = choices
                .iter()
                .enumerate()
                .map(|(index, c)| crate::ProDeviceChoice {
                    token: index as i32,
                    label: c.display_name.clone().into(),
                    selected: selected == Some(c.key.as_str()),
                })
                .collect();
        }
        ModuleDefinition::Color {
            temperature_mireds,
            rgb,
        } => {
            row.temperature_supported = temperature_mireds.is_some();
            row.rgb_supported = *rgb;
            if let Some(v) = temperature_mireds {
                row.temperature = numeric(v, "mired");
            }
            if let Some(T::LightControl(v)) = slot.capability.r#trait.as_ref().filter(|_| known) {
                if let Some(rgb) = v.observation.rgb.as_option() {
                    if let (Some(r), Some(g), Some(b)) = (rgb.red, rgb.green, rgb.blue) {
                        if r <= 255
                            && g <= 255
                            && b <= 255
                            && v.observation.color_temperature_mireds.is_none()
                        {
                            row.rgb_known = true;
                            row.rgb = crate::SrgbColorView {
                                red: r as i32,
                                green: g as i32,
                                blue: b as i32,
                                alpha: 255,
                            };
                            row.observed_color_mode = ProColorMode::Rgb;
                        }
                    }
                } else if v.observation.color_temperature_mireds.is_some() {
                    row.observed_color_mode = ProColorMode::Temperature;
                }
            }
            row.editor_color_mode = draft
                .and_then(|d| d.mode)
                .unwrap_or(row.observed_color_mode);
        }
        ModuleDefinition::Motion(operations) => {
            row.motion_operations = operations
                .iter()
                .enumerate()
                .map(|(index, operation)| crate::ProCoverChoice {
                    token: index as i32,
                    label: match operation {
                        model::CoverOperation::Open => "Open",
                        model::CoverOperation::Close => "Close",
                        model::CoverOperation::Stop => "Stop",
                    }
                    .into(),
                    operation: match operation {
                        model::CoverOperation::Open => crate::ProCoverOperation::Open,
                        model::CoverOperation::Close => crate::ProCoverOperation::Close,
                        model::CoverOperation::Stop => crate::ProCoverOperation::Stop,
                    },
                })
                .collect()
        }
    }
    row
}

#[cfg(test)]
#[path = "modules_tests.rs"]
mod tests;
