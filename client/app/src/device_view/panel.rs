//! Presentation recipes derive only from admitted instance constraints. The
//! renderer receives opaque field/choice tokens; it never chooses a wire mode.
use super::{
    DeviceIntent, Refusal,
    capability::Control,
    climate::{ClimateIntent, Range},
    controls::CoverIntent,
    helper::{HelperIntent, Kind},
    number::{Bounds, NumericInput},
};
use crate::{
    NavigationStep, ProAlarmChoice, ProAlarmOperation, ProCoverChoice, ProCoverOperation,
    ProDeviceChoice, ProDeviceValue, ProLockChoice, ProLockOperation, ProValueEvent,
    ProValueIntent, ProValueKind, ProValueMode, RangeThumb, StepDirection,
};
use home_wire_contract::home::{
    controls::v1 as controls, helpers::v1 as helpers, registry::v1 as registry, state::v1 as state,
};

#[derive(Clone, Copy)]
pub(super) enum Editor {
    Setpoint(Bounds),
    CoverPosition(Bounds),
    Range(Range),
    Helper,
    ColorTemperature,
}
#[derive(Clone, Default)]
pub(super) struct Draft {
    pub lower: String,
    pub upper: String,
    pub dirty: bool,
    pub error: String,
    gesture: bool,
}
pub(super) struct Choice {
    pub label: String,
    pub intent: DeviceIntent,
}
pub(super) fn editors(control: &Control) -> Vec<Editor> {
    let mut result = Vec::new();
    match control {
        Control::Climate(climate) => {
            if let Some(bounds) = climate.setpoint() {
                result.push(Editor::Setpoint(bounds));
            }
            if let Some(range) = climate.range() {
                result.push(Editor::Range(range));
            }
        }
        Control::Helper(helper) => {
            if helper.available()
                && !matches!(helper.kind(), Kind::Boolean | Kind::Button)
                && helper.operations().contains(&helpers::HelperOperation::Set)
            {
                result.push(Editor::Helper);
            }
        }
        Control::Light { color: Some(_), .. } => result.push(Editor::ColorTemperature),
        Control::Cover(cover) => {
            if let Some(bounds) = cover.position() {
                result.push(Editor::CoverPosition(bounds));
            }
        }
        Control::Switch { .. }
        | Control::Light { color: None, .. }
        | Control::Fan(_)
        | Control::Binary
        | Control::Measurement
        | Control::Lock(_)
        | Control::Alarm(_)
        | Control::ReadOnly
        | Control::Unsupported => {}
    }
    result
}
fn mode_label(value: registry::ClimateMode) -> &'static str {
    match value {
        registry::ClimateMode::Unspecified => "Non précisé",
        registry::ClimateMode::Off => "Arrêt",
        registry::ClimateMode::Heat => "Chauffage",
        registry::ClimateMode::Cool => "Climatisation",
        registry::ClimateMode::Auto => "Automatique",
        registry::ClimateMode::Dry => "Déshumidification",
        registry::ClimateMode::FanOnly => "Ventilation seule",
    }
}
fn fan_target_label(value: registry::ClimateFanTarget) -> &'static str {
    match value {
        registry::ClimateFanTarget::Unspecified => "Non précisé",
        registry::ClimateFanTarget::On => "Marche",
        registry::ClimateFanTarget::Auto => "Automatique",
    }
}
fn activity_label(value: registry::ClimateActivity) -> &'static str {
    match value {
        registry::ClimateActivity::Unspecified => "Non précisée",
        registry::ClimateActivity::Idle => "Au repos",
        registry::ClimateActivity::Heating => "Chauffe",
        registry::ClimateActivity::Cooling => "Refroidit",
        registry::ClimateActivity::Drying => "Déshumidifie",
        registry::ClimateActivity::FanOnly => "Ventile",
    }
}
pub(super) fn choices(control: &Control) -> Vec<Choice> {
    let mut choices = Vec::new();
    match control {
        Control::Climate(climate) => {
            for mode in climate.modes() {
                choices.push(Choice {
                    label: format!("Mode demandé · {}", mode_label(*mode)),
                    intent: DeviceIntent::Climate(ClimateIntent::Mode(*mode)),
                });
            }
            for target in climate.fan_targets() {
                choices.push(Choice {
                    label: format!("Ventilation demandée · {}", fan_target_label(*target)),
                    intent: DeviceIntent::Climate(ClimateIntent::FanTarget(*target)),
                });
            }
        }
        Control::Fan(fan) => {
            for speed in fan.speeds() {
                let label = match *speed {
                    state::FanSpeed::Unspecified => "Non précisée",
                    state::FanSpeed::Off => "Arrêt",
                    state::FanSpeed::Low => "Faible",
                    state::FanSpeed::Medium => "Moyenne",
                    state::FanSpeed::MediumHigh => "Moyenne haute",
                    state::FanSpeed::High => "Haute",
                };
                choices.push(Choice {
                    label: format!("Vitesse · {label}"),
                    intent: DeviceIntent::FanSpeed(*speed),
                });
            }
        }
        Control::Helper(helper) => {
            if helper.operations().contains(&helpers::HelperOperation::Set)
                && matches!(helper.kind(), Kind::Boolean)
            {
                for (value, label) in [(true, "Activer"), (false, "Désactiver")] {
                    choices.push(Choice {
                        label: label.into(),
                        intent: DeviceIntent::Helper(HelperIntent::Boolean(value)),
                    });
                }
            }
            for operation in helper.operations() {
                let label = match *operation {
                    helpers::HelperOperation::Set | helpers::HelperOperation::Unspecified => None,
                    helpers::HelperOperation::Toggle => Some("Inverser"),
                    helpers::HelperOperation::Increment => Some("Augmenter"),
                    helpers::HelperOperation::Decrement => Some("Diminuer"),
                    helpers::HelperOperation::Reset => Some("Réinitialiser"),
                    helpers::HelperOperation::Press => Some("Déclencher"),
                    helpers::HelperOperation::Next => Some("Option suivante"),
                    helpers::HelperOperation::Previous => Some("Option précédente"),
                };
                if let Some(label) = label {
                    choices.push(Choice {
                        label: label.into(),
                        intent: DeviceIntent::Helper(HelperIntent::Operation(*operation)),
                    });
                }
            }
        }
        Control::Cover(cover) => {
            for operation in cover.operations() {
                choices.push(Choice {
                    label: crate::control_text::cover_operation(*operation)
                        .unwrap_or_default()
                        .into(),
                    intent: DeviceIntent::Cover(CoverIntent::Operation(*operation)),
                });
            }
        }
        Control::Lock(lock) => {
            for operation in lock.operations() {
                choices.push(Choice {
                    label: crate::control_text::lock_operation(*operation)
                        .unwrap_or_default()
                        .into(),
                    intent: DeviceIntent::Lock(*operation),
                });
            }
        }
        Control::Alarm(alarm) => {
            for operation in alarm.operations() {
                choices.push(Choice {
                    label: crate::control_text::alarm_operation(*operation)
                        .unwrap_or_default()
                        .into(),
                    intent: DeviceIntent::Alarm(*operation),
                });
            }
        }
        Control::Switch { .. }
        | Control::Light { .. }
        | Control::Binary
        | Control::Measurement
        | Control::ReadOnly
        | Control::Unsupported => {}
    }
    choices
}
pub(super) fn choice_rows(control: &Control) -> Vec<ProDeviceChoice> {
    if matches!(
        control,
        Control::Cover(_) | Control::Lock(_) | Control::Alarm(_)
    ) {
        return Vec::new();
    }
    choices(control)
        .into_iter()
        .enumerate()
        .map(|(token, choice)| ProDeviceChoice {
            token: token as i32,
            label: choice.label.into(),
            selected: false,
        })
        .collect()
}

pub(super) fn cover_rows(control: &Control) -> Vec<ProCoverChoice> {
    let Control::Cover(cover) = control else {
        return Vec::new();
    };
    cover
        .operations()
        .iter()
        .enumerate()
        .filter_map(|(token, operation)| {
            let kind = match *operation {
                controls::CoverOperation::Open => ProCoverOperation::Open,
                controls::CoverOperation::Close => ProCoverOperation::Close,
                controls::CoverOperation::Stop => ProCoverOperation::Stop,
                controls::CoverOperation::Unspecified => return None,
            };
            Some(ProCoverChoice {
                token: token as i32,
                label: crate::control_text::cover_operation(*operation)?.into(),
                operation: kind,
            })
        })
        .collect()
}
pub(super) fn lock_rows(control: &Control) -> Vec<ProLockChoice> {
    let Control::Lock(lock) = control else {
        return Vec::new();
    };
    lock.operations()
        .iter()
        .enumerate()
        .filter_map(|(token, operation)| {
            let kind = match *operation {
                controls::LockOperation::Lock => ProLockOperation::Lock,
                controls::LockOperation::Unlock => ProLockOperation::Unlock,
                controls::LockOperation::Unspecified => return None,
            };
            Some(ProLockChoice {
                token: token as i32,
                label: crate::control_text::lock_operation(*operation)?.into(),
                operation: kind,
            })
        })
        .collect()
}
pub(super) fn alarm_rows(control: &Control) -> Vec<ProAlarmChoice> {
    let Control::Alarm(alarm) = control else {
        return Vec::new();
    };
    alarm
        .operations()
        .iter()
        .enumerate()
        .filter_map(|(token, operation)| {
            let kind = match *operation {
                controls::AlarmOperation::ArmHome => ProAlarmOperation::ArmHome,
                controls::AlarmOperation::ArmAway => ProAlarmOperation::ArmAway,
                controls::AlarmOperation::Disarm => ProAlarmOperation::Disarm,
                controls::AlarmOperation::Unspecified => return None,
            };
            Some(ProAlarmChoice {
                token: token as i32,
                label: crate::control_text::alarm_operation(*operation)?.into(),
                operation: kind,
            })
        })
        .collect()
}
fn climate_observation(entity: &state::ViewEntity) -> Option<&state::ClimateValue> {
    let observed = entity
        .state
        .as_option()
        .filter(|value| value.__buffa_unknown_fields.is_empty())?;
    match observed.value.as_ref()? {
        state::observed_state::Value::Climate(value) => {
            value.__buffa_unknown_fields.is_empty().then_some(value)
        }
        state::observed_state::Value::SwitchOn(_)
        | state::observed_state::Value::Measurement(_)
        | state::observed_state::Value::Light(_)
        | state::observed_state::Value::Fan(_)
        | state::observed_state::Value::Helper(_)
        | state::observed_state::Value::Solar(_)
        | state::observed_state::Value::SolarEvent(_)
        | state::observed_state::Value::Cover(_)
        | state::observed_state::Value::Lock(_)
        | state::observed_state::Value::Alarm(_)
        | state::observed_state::Value::Weather(_)
        | state::observed_state::Value::Presence(_)
        | state::observed_state::Value::PresenceDetails(_) => None,
    }
}
pub(super) fn readings(control: &Control, entity: &state::ViewEntity) -> String {
    let Control::Climate(climate) = control else {
        return String::new();
    };
    let Some(value) = climate_observation(entity) else {
        return "Aucune observation climatique".into();
    };
    let observes = climate.observes();
    let mut parts = Vec::new();
    if observes.temperature
        && let Some(value) = value.current_temperature_celsius.filter(|v| v.is_finite())
    {
        parts.push(format!("Température · {value} °C"));
    }
    match value.setpoint.as_ref() {
        Some(state::climate_value::Setpoint::SetpointCelsius(value))
            if observes.setpoint && value.is_finite() =>
        {
            parts.push(format!("Consigne observée · {value} °C"));
        }
        Some(state::climate_value::Setpoint::SetpointRangeCelsius(value))
            if observes.range && value.__buffa_unknown_fields.is_empty() =>
        {
            if let Some(low) = value.low_celsius.filter(|v| v.is_finite()) {
                parts.push(format!("Seuil bas observé · {low} °C"));
            }
            if let Some(high) = value.high_celsius.filter(|v| v.is_finite()) {
                parts.push(format!("Seuil haut observé · {high} °C"));
            }
        }
        Some(
            state::climate_value::Setpoint::SetpointCelsius(_)
            | state::climate_value::Setpoint::SetpointRangeCelsius(_),
        )
        | None => {}
    }
    if let Some(mode) = value
        .mode
        .and_then(|v| v.as_known())
        .filter(|v| observes.modes.contains(v))
    {
        parts.push(format!("Mode observé · {}", mode_label(mode)));
    }
    if let Some(activity) = value
        .activity
        .and_then(|v| v.as_known())
        .filter(|v| observes.activities.contains(v))
    {
        parts.push(format!("Activité observée · {}", activity_label(activity)));
    }
    if observes.humidity
        && let Some(value) = value.humidity_percent.filter(|v| v.is_finite())
    {
        parts.push(format!("Humidité · {value} %"));
    }
    if observes.fan_activity
        && let Some(value) = value.fan_active
    {
        parts.push(format!(
            "Ventilation observée · {}",
            if value { "Active" } else { "Inactive" }
        ));
    }
    if let Some(target) = value
        .fan_target
        .and_then(|v| v.as_known())
        .filter(|v| observes.fan_targets.contains(v))
    {
        parts.push(format!(
            "Ventilation demandée observée · {}",
            fan_target_label(target)
        ));
    }
    if parts.is_empty() {
        "Aucune observation climatique annoncée".into()
    } else {
        parts.join("\n")
    }
}
impl Editor {
    fn color(control: &Control) -> Result<&super::light::ColorTemperature, Refusal> {
        match control {
            Control::Light {
                color: Some(color), ..
            } => Ok(color),
            Control::Light { color: None, .. }
            | Control::Switch { .. }
            | Control::Climate(_)
            | Control::Fan(_)
            | Control::Helper(_)
            | Control::Cover(_)
            | Control::Binary
            | Control::Measurement
            | Control::Lock(_)
            | Control::Alarm(_)
            | Control::ReadOnly
            | Control::Unsupported => Err(Refusal::Unsupported),
        }
    }
    fn bounds(self, control: &Control) -> Result<(Bounds, Option<Bounds>), Refusal> {
        match self {
            Self::Setpoint(bounds) | Self::CoverPosition(bounds) => Ok((bounds, None)),
            Self::ColorTemperature => Ok((Self::color(control)?.presentation(), None)),
            Self::Range(range) => {
                let (low, high) = range.bounds();
                Ok((low, Some(high)))
            }
            Self::Helper => match control {
                Control::Helper(helper) => match helper.kind() {
                    Kind::Number(bounds) => Ok((*bounds, None)),
                    Kind::Boolean
                    | Kind::Counter { .. }
                    | Kind::Button
                    | Kind::Date
                    | Kind::Time
                    | Kind::DateTime
                    | Kind::Select(_)
                    | Kind::Text { .. } => Err(Refusal::Unsupported),
                },
                Control::Climate(_)
                | Control::Fan(_)
                | Control::Switch { .. }
                | Control::Light { .. }
                | Control::Binary
                | Control::Measurement
                | Control::Cover(_)
                | Control::Lock(_)
                | Control::Alarm(_)
                | Control::ReadOnly
                | Control::Unsupported => Err(Refusal::Unsupported),
            },
        }
    }
    pub fn initial(self, entity: &state::ViewEntity, control: &Control) -> Draft {
        let mut draft = Draft::default();
        if let Self::CoverPosition(_) = self {
            if let Control::Cover(cover) = control
                && let Some(observation) = entity
                    .state
                    .as_option()
                    .filter(|v| v.__buffa_unknown_fields.is_empty())
                && let Some(state::observed_state::Value::Cover(value)) = &observation.value
                && let Some(position) = cover.observed_position(value)
            {
                draft.lower = position.to_string();
            }
            return draft;
        }
        if matches!(self, Self::ColorTemperature) {
            if let Ok(color) = Self::color(control)
                && let Some(value) = entity
                    .state
                    .as_option()
                    .filter(|value| value.__buffa_unknown_fields.is_empty())
                    .and_then(|state| state.value.as_ref())
                && let state::observed_state::Value::Light(value) = value
                && value.__buffa_unknown_fields.is_empty()
                && let Some(value) = value
                    .color_temp_mireds
                    .and_then(super::light::Mireds::from_wire)
                && color.admit(value).is_ok()
            {
                draft.lower = value.text();
            }
            return draft;
        }
        if matches!(self, Self::Helper) {
            if let Control::Helper(helper) = control
                && let Some(value) = super::helper::observation(entity)
            {
                let text = super::helper::draft_text(value, helper.kind());
                if let Some(text) = text
                    && helper.input(text.clone(), None).is_ok()
                {
                    draft.lower = text;
                }
            }
            return draft;
        }
        let advertised = match control {
            Control::Climate(climate) => match self {
                Self::Setpoint(_) => climate.observes().setpoint,
                Self::Range(_) => climate.observes().range,
                Self::Helper | Self::ColorTemperature | Self::CoverPosition(_) => false,
            },
            Control::Switch { .. }
            | Control::Light { .. }
            | Control::Fan(_)
            | Control::Helper(_)
            | Control::Cover(_)
            | Control::Binary
            | Control::Measurement
            | Control::Lock(_)
            | Control::Alarm(_)
            | Control::ReadOnly
            | Control::Unsupported => false,
        };
        if !advertised {
            return draft;
        }
        if let Some(value) = climate_observation(entity) {
            match (self, value.setpoint.as_ref()) {
                (
                    Self::Setpoint(bounds),
                    Some(state::climate_value::Setpoint::SetpointCelsius(value)),
                ) => {
                    if bounds.admit(*value).is_ok() {
                        draft.lower = value.to_string();
                    }
                }
                (
                    Self::Range(range),
                    Some(state::climate_value::Setpoint::SetpointRangeCelsius(value)),
                ) => {
                    if value.__buffa_unknown_fields.is_empty() {
                        let (low, high) = range.bounds();
                        if let Some(value) =
                            value.low_celsius.filter(|value| low.admit(*value).is_ok())
                        {
                            draft.lower = value.to_string();
                        }
                        if let Some(value) = value
                            .high_celsius
                            .filter(|value| high.admit(*value).is_ok())
                        {
                            draft.upper = value.to_string();
                        }
                    }
                }
                (
                    Self::Setpoint(_),
                    Some(state::climate_value::Setpoint::SetpointRangeCelsius(_)),
                )
                | (Self::Range(_), Some(state::climate_value::Setpoint::SetpointCelsius(_)))
                | (Self::Setpoint(_) | Self::Range(_), None)
                | (
                    Self::Helper | Self::ColorTemperature | Self::CoverPosition(_),
                    Some(_) | None,
                ) => {}
            }
        }
        draft
    }
    fn intent(self, draft: &Draft, control: &Control) -> Result<DeviceIntent, Refusal> {
        if matches!(self, Self::ColorTemperature) {
            return Self::color(control)?
                .convert(NumericInput::Text(draft.lower.clone()))
                .map(DeviceIntent::ColorTemperature);
        }
        if matches!(self, Self::Helper) {
            let Control::Helper(helper) = control else {
                return Err(Refusal::Unsupported);
            };
            return helper
                .input(draft.lower.clone(), None)
                .map(DeviceIntent::Helper);
        }
        if let Self::CoverPosition(bounds) = self {
            return Ok(DeviceIntent::Cover(CoverIntent::Position(
                NumericInput::Renderer(NumericInput::Text(draft.lower.clone()).admit(bounds)?),
            )));
        }
        let low = NumericInput::Text(draft.lower.clone());
        let intent = match self {
            Self::Setpoint(bounds) => {
                ClimateIntent::Setpoint(NumericInput::Renderer(low.admit(bounds)?))
            }
            Self::Range(range) => {
                let (low, high) = range.admit(low, NumericInput::Text(draft.upper.clone()))?;
                ClimateIntent::Range(NumericInput::Renderer(low), NumericInput::Renderer(high))
            }
            Self::Helper | Self::ColorTemperature | Self::CoverPosition(_) => {
                return Err(Refusal::Unsupported);
            }
        };
        Ok(DeviceIntent::Climate(intent))
    }
    pub fn row(self, token: i32, draft: &Draft, control: &Control) -> ProDeviceValue {
        let Ok((low, high)) = self.bounds(control) else {
            return helper_row(token, draft, control);
        };
        let (mode, label) = match self {
            Self::Setpoint(_) => (ProValueMode::Single, "Consigne"),
            Self::CoverPosition(_) => (ProValueMode::Single, "Position"),
            Self::Range(_) => (ProValueMode::Range, "Plage de consigne"),
            Self::Helper => (ProValueMode::Single, "Valeur"),
            Self::ColorTemperature => (ProValueMode::Single, "Température de couleur"),
        };
        let description = |bounds: Bounds| {
            format!(
                "{} à {}{}",
                bounds.minimum(),
                bounds.maximum(),
                bounds
                    .step()
                    .map(|v| format!(", pas {v}"))
                    .unwrap_or_default()
            )
        };
        let position = |text: &str, bounds: Bounds| {
            NumericInput::Text(text.into())
                .admit(bounds)
                .map(|v| {
                    if bounds.maximum() > bounds.minimum() {
                        (v - bounds.minimum()) / (bounds.maximum() - bounds.minimum())
                    } else {
                        0.
                    }
                })
                .unwrap_or(0.) as f32
        };
        let mut row = ProDeviceValue {
            token,
            kind: ProValueKind::Numeric,
            mode,
            label: label.into(),
            lower_label: "Seuil bas".into(),
            upper_label: "Seuil haut".into(),
            suffix: match self {
                Self::Setpoint(_) | Self::Range(_) => "°C",
                Self::Helper => "",
                Self::ColorTemperature => "K",
                Self::CoverPosition(_) => "%",
            }
            .into(),
            description: format!(
                "{}{}",
                description(low),
                high.map(|v| format!(" · {}", description(v)))
                    .unwrap_or_default()
            )
            .into(),
            lower: draft.lower.clone().into(),
            upper: draft.upper.clone().into(),
            error: draft.error.clone().into(),
            lower_position: position(&draft.lower, low),
            upper_position: high.map(|v| position(&draft.upper, v)).unwrap_or(1.),
            slider_enabled: low.maximum() > low.minimum() && high.is_none_or(|v| v == low),
            can_step: low.step().is_some() && high.is_none_or(|v| v.step().is_some()),
        };
        if matches!(self, Self::ColorTemperature)
            && let Ok(color) = Self::color(control)
        {
            row.description = color.description(&draft.lower).into();
            row.can_step = color.can_move();
            row.slider_enabled = color.can_move();
        }
        row
    }
    pub fn edit(
        self,
        draft: &mut Draft,
        intent: ProValueIntent,
        control: &Control,
    ) -> Result<Option<DeviceIntent>, Refusal> {
        let moving = matches!(
            intent.event,
            ProValueEvent::Position | ProValueEvent::Navigate | ProValueEvent::Step
        );
        let result = self.apply(draft, intent, control);
        if moving && result.is_err() {
            draft.gesture = false;
        }
        draft.error = if result.is_err() {
            "Valeur hors limites, hors pas ou plage invalide.".into()
        } else {
            String::new()
        };
        result
    }
    fn move_color(
        draft: &mut Draft,
        intent: ProValueIntent,
        control: &Control,
    ) -> Result<Option<DeviceIntent>, Refusal> {
        if intent.thumb != RangeThumb::Lower {
            return Err(Refusal::Unsupported);
        }
        let color = Self::color(control)?;
        let value = match intent.event {
            ProValueEvent::Position => {
                let position = f64::from(intent.position);
                if !position.is_finite() || !(0. ..=1.).contains(&position) {
                    return Err(Refusal::InvalidValue);
                }
                let bounds = color.presentation();
                color.convert(NumericInput::Renderer(
                    bounds.minimum() + position * (bounds.maximum() - bounds.minimum()),
                ))?
            }
            ProValueEvent::Step => color.navigate(
                NumericInput::Text(draft.lower.clone()),
                match intent.direction {
                    StepDirection::Increment => NavigationStep::Next,
                    StepDirection::Decrement => NavigationStep::Previous,
                },
            )?,
            ProValueEvent::Navigate => {
                color.navigate(NumericInput::Text(draft.lower.clone()), intent.navigation)?
            }
            ProValueEvent::Draft
            | ProValueEvent::Submit
            | ProValueEvent::Start
            | ProValueEvent::Commit
            | ProValueEvent::Cancel
            | ProValueEvent::SelectOpen
            | ProValueEvent::SelectNavigate
            | ProValueEvent::SelectActivate
            | ProValueEvent::SelectDismiss
            | ProValueEvent::SelectQuery
            | ProValueEvent::SelectTypeahead => return Err(Refusal::Unsupported),
        };
        draft.lower = value.text();
        draft.dirty = true;
        Ok(None)
    }
    fn apply(
        self,
        draft: &mut Draft,
        intent: ProValueIntent,
        control: &Control,
    ) -> Result<Option<DeviceIntent>, Refusal> {
        match intent.event {
            ProValueEvent::SelectOpen
            | ProValueEvent::SelectNavigate
            | ProValueEvent::SelectActivate
            | ProValueEvent::SelectDismiss
            | ProValueEvent::SelectQuery
            | ProValueEvent::SelectTypeahead => return Err(Refusal::Unsupported),
            ProValueEvent::Draft => {
                match intent.thumb {
                    RangeThumb::Lower => draft.lower = intent.lower.to_string(),
                    RangeThumb::Upper => draft.upper = intent.lower.to_string(),
                }
                draft.dirty = true;
                draft.gesture = false;
                // Incomplete input remains editable and never sends a command.
                let _ = self.intent(draft, control)?;
            }
            ProValueEvent::Submit => {
                draft.lower = intent.lower.to_string();
                draft.upper = intent.upper.to_string();
                draft.dirty = true;
                draft.gesture = false;
                return self.intent(draft, control).map(Some);
            }
            ProValueEvent::Start => {
                draft.gesture = true;
                draft.dirty = true;
            }
            ProValueEvent::Cancel => {
                draft.gesture = false;
            }
            ProValueEvent::Commit => {
                if !draft.gesture {
                    return Err(Refusal::Stale);
                }
                draft.gesture = false;
                return self.intent(draft, control).map(Some);
            }
            ProValueEvent::Position | ProValueEvent::Navigate | ProValueEvent::Step => {
                if intent.event != ProValueEvent::Step && !draft.gesture {
                    return Err(Refusal::Stale);
                }
                if matches!(self, Self::ColorTemperature) {
                    return Self::move_color(draft, intent, control);
                }
                let (low, high) = self.bounds(control)?;
                let (bounds, text) = match intent.thumb {
                    RangeThumb::Lower => (low, &mut draft.lower),
                    RangeThumb::Upper => (high.ok_or(Refusal::Unsupported)?, &mut draft.upper),
                };
                let value = match intent.event {
                    ProValueEvent::Position => {
                        let position = f64::from(intent.position);
                        if !position.is_finite() || !(0. ..=1.).contains(&position) {
                            return Err(Refusal::InvalidValue);
                        }
                        bounds.minimum() + position * (bounds.maximum() - bounds.minimum())
                    }
                    ProValueEvent::Step | ProValueEvent::Navigate => {
                        let step = bounds.step().ok_or(Refusal::Unsupported)?;
                        let current = NumericInput::Text(text.clone()).admit(bounds)?;
                        if intent.event == ProValueEvent::Step {
                            match intent.direction {
                                StepDirection::Increment => current + step,
                                StepDirection::Decrement => current - step,
                            }
                        } else {
                            match intent.navigation {
                                NavigationStep::First => bounds.minimum(),
                                NavigationStep::Last => bounds.maximum(),
                                NavigationStep::Next => current + step,
                                NavigationStep::Previous => current - step,
                                NavigationStep::PageNext => current + 10. * step,
                                NavigationStep::PagePrevious => current - 10. * step,
                            }
                        }
                    }
                    ProValueEvent::Draft
                    | ProValueEvent::Submit
                    | ProValueEvent::Start
                    | ProValueEvent::Commit
                    | ProValueEvent::Cancel
                    | ProValueEvent::SelectOpen
                    | ProValueEvent::SelectNavigate
                    | ProValueEvent::SelectActivate
                    | ProValueEvent::SelectDismiss
                    | ProValueEvent::SelectQuery
                    | ProValueEvent::SelectTypeahead => return Err(Refusal::Unsupported),
                };
                *text = bounds
                    .canonical(value.clamp(bounds.minimum(), bounds.maximum()))?
                    .to_string();
                draft.dirty = true;
            }
        }
        Ok(None)
    }
}

fn helper_row(token: i32, draft: &Draft, control: &Control) -> ProDeviceValue {
    let Control::Helper(helper) = control else {
        return ProDeviceValue::default();
    };
    let (kind, label, description) = match helper.kind() {
        Kind::Counter {
            minimum,
            maximum,
            step,
        } => (
            ProValueKind::Counter,
            "Compteur",
            format!(
                "Entier 64 bits · minimum {} · maximum {} · valeur multiple de {step} · incrément {step}",
                minimum
                    .map(|v| v.to_string())
                    .unwrap_or_else(|| "non précisé".into()),
                maximum
                    .map(|v| v.to_string())
                    .unwrap_or_else(|| "non précisé".into())
            ),
        ),
        Kind::Text { minimum, maximum } => (
            ProValueKind::Text,
            "Texte",
            format!("{minimum} à {maximum} caractères Unicode"),
        ),
        Kind::Date => (
            ProValueKind::Date,
            "Date",
            "AAAA-MM-JJ · date civile".into(),
        ),
        Kind::Time => (
            ProValueKind::Time,
            "Heure",
            "HH:MM[:SS] · heure civile".into(),
        ),
        Kind::DateTime => (
            ProValueKind::DateTime,
            "Date et heure",
            "AAAA-MM-JJTHH:MM[:SS] · sans fuseau implicite".into(),
        ),
        Kind::Select(_) => (ProValueKind::Select, "Option", String::new()),
        Kind::Number(_) | Kind::Boolean | Kind::Button => return ProDeviceValue::default(),
    };
    ProDeviceValue {
        token,
        kind,
        label: label.into(),
        description: description.into(),
        lower: draft.lower.clone().into(),
        error: draft.error.clone().into(),
        ..Default::default()
    }
}
