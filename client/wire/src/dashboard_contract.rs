//! Strict decoding of the single API v2 inert dashboard recipe vocabulary.
use crate::home::api::v2 as wire;
use buffa::EnumValue;
use home_client_model::{
    api::{CapabilityName, EntityName},
    dashboard::*,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RecipeFailure {
    Unsupported,
    Invalid,
    Budget,
}

fn text(value: &str, empty: bool, limit: usize) -> bool {
    (empty || !value.is_empty()) && value.len() <= limit && !value.chars().any(char::is_control)
}
fn entity(value: &str) -> Result<EntityName, RecipeFailure> {
    if !text(value, false, 4096) {
        return Err(RecipeFailure::Invalid);
    }
    EntityName::try_from(value).map_err(|_| RecipeFailure::Invalid)
}
fn capability(value: &str) -> Result<CapabilityName, RecipeFailure> {
    CapabilityName::try_from(value).map_err(|_| RecipeFailure::Invalid)
}
pub fn decode_field(value: &wire::DashboardFieldBinding) -> Result<FieldBinding, RecipeFailure> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(RecipeFailure::Unsupported);
    }
    let field = match value.field.as_known() {
        Some(wire::DashboardFieldKey::State) => FieldKey::State,
        Some(wire::DashboardFieldKey::Value) => FieldKey::Value,
        Some(wire::DashboardFieldKey::CurrentTemperatureCelsius) => FieldKey::CurrentTemperature,
        Some(wire::DashboardFieldKey::SetpointCelsius) => FieldKey::Setpoint,
        Some(wire::DashboardFieldKey::Measurement) => FieldKey::Measurement,
        Some(wire::DashboardFieldKey::Unspecified) | None => {
            return Err(RecipeFailure::Unsupported);
        }
    };
    Ok(FieldBinding {
        capability: capability(&value.capability)?,
        field,
    })
}

/// Closed wire admission shared by predicates and widget activity bindings.
/// This decodes typed values only; source-format string interpretation is native.
pub fn decode_literal(value: &wire::DashboardLiteral) -> Result<Literal, RecipeFailure> {
    use wire::dashboard_literal::Value as V;
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(RecipeFailure::Unsupported);
    }
    Ok(match value.value.as_ref().ok_or(RecipeFailure::Invalid)? {
        V::BooleanValue(value) => Literal::Boolean(*value),
        V::IntegerValue(value) => Literal::Integer(*value),
        V::NumberValue(value) => Literal::Number(
            home_client_model::api::Finite::try_from(*value).map_err(|_| RecipeFailure::Invalid)?,
        ),
        V::TextValue(value) => Literal::Text(
            TextLiteral::try_from(value.as_str()).map_err(|_| RecipeFailure::Invalid)?,
        ),
        V::DecimalValue(value) => Literal::Decimal(
            DecimalLiteral::try_from(value.as_str()).map_err(|_| RecipeFailure::Invalid)?,
        ),
        V::PowerValue(value) => Literal::Power(match value.as_known() {
            Some(wire::PowerValue::On) => home_client_model::api::Power::On,
            Some(wire::PowerValue::Off) => home_client_model::api::Power::Off,
            Some(wire::PowerValue::Unspecified) | None => return Err(RecipeFailure::Unsupported),
        }),
        V::CoverState(value) => Literal::Cover(match value.as_known() {
            Some(wire::CoverState::Open) => home_client_model::dashboard::CoverState::Open,
            Some(wire::CoverState::Closed) => home_client_model::dashboard::CoverState::Closed,
            Some(wire::CoverState::Opening) => home_client_model::dashboard::CoverState::Opening,
            Some(wire::CoverState::Closing) => home_client_model::dashboard::CoverState::Closing,
            Some(wire::CoverState::Unspecified) | None => return Err(RecipeFailure::Unsupported),
        }),
        V::LockState(value) => Literal::Lock(match value.as_known() {
            Some(wire::LockState::Locked) => home_client_model::dashboard::LockState::Locked,
            Some(wire::LockState::Unlocked) => home_client_model::dashboard::LockState::Unlocked,
            Some(wire::LockState::Locking) => home_client_model::dashboard::LockState::Locking,
            Some(wire::LockState::Unlocking) => home_client_model::dashboard::LockState::Unlocking,
            Some(wire::LockState::Jammed) => home_client_model::dashboard::LockState::Jammed,
            Some(wire::LockState::Opening) => home_client_model::dashboard::LockState::Opening,
            Some(wire::LockState::Open) => home_client_model::dashboard::LockState::Open,
            Some(wire::LockState::Unspecified) | None => return Err(RecipeFailure::Unsupported),
        }),
        V::AlarmState(value) => Literal::Alarm(match value.as_known() {
            Some(wire::AlarmState::Disarmed) => home_client_model::dashboard::AlarmState::Disarmed,
            Some(wire::AlarmState::ArmedHome) => {
                home_client_model::dashboard::AlarmState::ArmedHome
            }
            Some(wire::AlarmState::ArmedAway) => {
                home_client_model::dashboard::AlarmState::ArmedAway
            }
            Some(wire::AlarmState::ArmedNight) => {
                home_client_model::dashboard::AlarmState::ArmedNight
            }
            Some(wire::AlarmState::ArmedVacation) => {
                home_client_model::dashboard::AlarmState::ArmedVacation
            }
            Some(wire::AlarmState::ArmedCustomBypass) => {
                home_client_model::dashboard::AlarmState::ArmedCustomBypass
            }
            Some(wire::AlarmState::Pending) => home_client_model::dashboard::AlarmState::Pending,
            Some(wire::AlarmState::Arming) => home_client_model::dashboard::AlarmState::Arming,
            Some(wire::AlarmState::Disarming) => {
                home_client_model::dashboard::AlarmState::Disarming
            }
            Some(wire::AlarmState::Triggered) => {
                home_client_model::dashboard::AlarmState::Triggered
            }
            Some(wire::AlarmState::Unspecified) | None => return Err(RecipeFailure::Unsupported),
        }),
        V::TemperatureMode(value) => Literal::TemperatureMode(match value.as_known() {
            Some(wire::TemperatureMode::Off) => home_client_model::api::TemperatureMode::Off,
            Some(wire::TemperatureMode::Heat) => home_client_model::api::TemperatureMode::Heat,
            Some(wire::TemperatureMode::Cool) => home_client_model::api::TemperatureMode::Cool,
            Some(wire::TemperatureMode::Auto) => home_client_model::api::TemperatureMode::Auto,
            Some(wire::TemperatureMode::Dry) => home_client_model::api::TemperatureMode::Dry,
            Some(wire::TemperatureMode::FanOnly) => {
                home_client_model::api::TemperatureMode::FanOnly
            }
            Some(wire::TemperatureMode::Unspecified) | None => {
                return Err(RecipeFailure::Unsupported);
            }
        }),
        V::FanSpeed(value) => Literal::FanSpeed(
            home_client_model::api::SelectorKey::try_from(value.as_str())
                .map_err(|_| RecipeFailure::Unsupported)?,
        ),
        V::WeatherCondition(value) => Literal::WeatherCondition(match value.as_known() {
            Some(wire::WeatherCondition::Unknown) => {
                home_client_model::dashboard::WeatherCondition::Unknown
            }
            Some(wire::WeatherCondition::ClearDay) => {
                home_client_model::dashboard::WeatherCondition::ClearDay
            }
            Some(wire::WeatherCondition::ClearNight) => {
                home_client_model::dashboard::WeatherCondition::ClearNight
            }
            Some(wire::WeatherCondition::Cloudy) => {
                home_client_model::dashboard::WeatherCondition::Cloudy
            }
            Some(wire::WeatherCondition::PartlyCloudy) => {
                home_client_model::dashboard::WeatherCondition::PartlyCloudy
            }
            Some(wire::WeatherCondition::Rainy) => {
                home_client_model::dashboard::WeatherCondition::Rainy
            }
            Some(wire::WeatherCondition::Pouring) => {
                home_client_model::dashboard::WeatherCondition::Pouring
            }
            Some(wire::WeatherCondition::Snowy) => {
                home_client_model::dashboard::WeatherCondition::Snowy
            }
            Some(wire::WeatherCondition::SnowyRainy) => {
                home_client_model::dashboard::WeatherCondition::SnowyRainy
            }
            Some(wire::WeatherCondition::Hail) => {
                home_client_model::dashboard::WeatherCondition::Hail
            }
            Some(wire::WeatherCondition::Lightning) => {
                home_client_model::dashboard::WeatherCondition::Lightning
            }
            Some(wire::WeatherCondition::LightningRainy) => {
                home_client_model::dashboard::WeatherCondition::LightningRainy
            }
            Some(wire::WeatherCondition::Windy) => {
                home_client_model::dashboard::WeatherCondition::Windy
            }
            Some(wire::WeatherCondition::WindyVariant) => {
                home_client_model::dashboard::WeatherCondition::WindyVariant
            }
            Some(wire::WeatherCondition::Fog) => {
                home_client_model::dashboard::WeatherCondition::Fog
            }
            Some(wire::WeatherCondition::Exceptional) => {
                home_client_model::dashboard::WeatherCondition::Exceptional
            }
            Some(wire::WeatherCondition::Unspecified) | None => {
                return Err(RecipeFailure::Unsupported);
            }
        }),
        V::Residence(value) => Literal::Residence(match value.as_known() {
            Some(wire::Residence::Home) => home_client_model::dashboard::Residence::Home,
            Some(wire::Residence::Away) => home_client_model::dashboard::Residence::Away,
            Some(wire::Residence::Unknown) => home_client_model::dashboard::Residence::Unknown,
            Some(wire::Residence::Unspecified) | None => return Err(RecipeFailure::Unsupported),
        }),
    })
}

fn source(value: Option<&wire::DashboardLayoutSource>) -> Result<Source, RecipeFailure> {
    let value = value.ok_or(RecipeFailure::Invalid)?;
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(RecipeFailure::Unsupported);
    }
    if !text(&value.file, false, 4096)
        || !text(&value.path, false, 4096)
        || !text(&value.variant, false, 256)
    {
        return Err(RecipeFailure::Invalid);
    }
    Ok(Source {
        file: value.file.clone(),
        line: u32::try_from(value.line).map_err(|_| RecipeFailure::Invalid)?,
        column: u32::try_from(value.column).map_err(|_| RecipeFailure::Invalid)?,
        path: value.path.clone(),
        variant: value.variant.clone(),
    })
}
fn command(value: EnumValue<wire::DashboardCommandKind>) -> Result<CommandKind, RecipeFailure> {
    match value {
        EnumValue::Known(wire::DashboardCommandKind::Toggle) => Ok(CommandKind::Toggle),
        EnumValue::Known(wire::DashboardCommandKind::TurnOn) => Ok(CommandKind::TurnOn),
        EnumValue::Known(wire::DashboardCommandKind::TurnOff) => Ok(CommandKind::TurnOff),
        EnumValue::Known(wire::DashboardCommandKind::Activate) => Ok(CommandKind::Activate),
        EnumValue::Known(wire::DashboardCommandKind::CoverOpen) => Ok(CommandKind::CoverOpen),
        EnumValue::Known(wire::DashboardCommandKind::CoverClose) => Ok(CommandKind::CoverClose),
        EnumValue::Known(wire::DashboardCommandKind::CoverStop) => Ok(CommandKind::CoverStop),
        EnumValue::Known(wire::DashboardCommandKind::Lock) => Ok(CommandKind::Lock),
        EnumValue::Known(wire::DashboardCommandKind::Unlock) => Ok(CommandKind::Unlock),
        EnumValue::Known(wire::DashboardCommandKind::AlarmArmHome) => Ok(CommandKind::AlarmArmHome),
        EnumValue::Known(wire::DashboardCommandKind::AlarmArmAway) => Ok(CommandKind::AlarmArmAway),
        EnumValue::Known(wire::DashboardCommandKind::AlarmArmNight) => {
            Ok(CommandKind::AlarmArmNight)
        }
        EnumValue::Known(wire::DashboardCommandKind::AlarmDisarm) => Ok(CommandKind::AlarmDisarm),
        EnumValue::Known(wire::DashboardCommandKind::Unspecified) | EnumValue::Unknown(_) => {
            Err(RecipeFailure::Unsupported)
        }
    }
}
fn interaction(
    value: &wire::DashboardInteraction,
) -> Result<(Gesture, Interaction), RecipeFailure> {
    use wire::dashboard_interaction::Intention as I;
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(RecipeFailure::Unsupported);
    }
    let gesture = match value.gesture {
        EnumValue::Known(wire::DashboardGesture::Tap) => Gesture::Tap,
        EnumValue::Known(wire::DashboardGesture::Hold) => Gesture::Hold,
        EnumValue::Known(wire::DashboardGesture::DoubleTap) => Gesture::DoubleTap,
        EnumValue::Known(wire::DashboardGesture::Unspecified) | EnumValue::Unknown(_) => {
            return Err(RecipeFailure::Unsupported);
        }
    };
    let intention = match value.intention.as_ref() {
        Some(I::None(none)) => {
            if !none.__buffa_unknown_fields.is_empty() || value.confirmation.as_option().is_some() {
                return Err(RecipeFailure::Unsupported);
            }
            Intention::None
        }
        Some(I::MoreInfo(target)) => {
            if !target.__buffa_unknown_fields.is_empty() {
                return Err(RecipeFailure::Unsupported);
            }
            Intention::MoreInfo(entity(&target.entity)?)
        }
        Some(I::Navigate(target)) => {
            if !target.__buffa_unknown_fields.is_empty() {
                return Err(RecipeFailure::Unsupported);
            }
            let presentation = match target.presentation {
                EnumValue::Known(wire::DashboardPresentation::Page) => Presentation::Page,
                EnumValue::Known(wire::DashboardPresentation::Popup) => Presentation::Popup,
                EnumValue::Known(wire::DashboardPresentation::Unspecified)
                | EnumValue::Unknown(_) => {
                    return Err(RecipeFailure::Unsupported);
                }
            };
            Intention::Navigate(Destination {
                dashboard: DashboardName::try_from(target.dashboard.as_str())
                    .map_err(|_| RecipeFailure::Invalid)?,
                node: NodeId::parse(&target.node_id).map_err(|_| RecipeFailure::Invalid)?,
                presentation,
            })
        }
        Some(I::Command(target)) => {
            if !target.__buffa_unknown_fields.is_empty() {
                return Err(RecipeFailure::Unsupported);
            }
            Intention::Command {
                capability: capability(&target.capability)?,
                kind: command(target.kind)?,
            }
        }
        None => return Err(RecipeFailure::Invalid),
    };
    let confirmation = value
        .confirmation
        .as_option()
        .map(|confirmation| {
            if !confirmation.__buffa_unknown_fields.is_empty() {
                return Err(RecipeFailure::Unsupported);
            }
            // A native prompt may contain line breaks. No code is interpreted.
            if confirmation.text.len() > 512 || confirmation.text.contains('\0') {
                return Err(RecipeFailure::Invalid);
            }
            Ok(confirmation.text.clone())
        })
        .transpose()?;
    Ok((
        gesture,
        Interaction {
            intention,
            confirmation,
            source: source(value.source.as_option())?,
        },
    ))
}
fn predicate(
    value: &wire::DashboardPredicate,
    depth: usize,
    remaining: &mut usize,
) -> Result<Predicate, RecipeFailure> {
    use wire::dashboard_predicate::Condition;
    if depth > 8 || *remaining == 0 {
        return Err(RecipeFailure::Budget);
    }
    *remaining -= 1;
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(RecipeFailure::Unsupported);
    }
    source(value.source.as_option())?;
    match value.condition.as_ref() {
        Some(Condition::Constant(flag)) => Ok(Predicate::Constant(*flag)),
        Some(Condition::State(state)) => {
            if !state.__buffa_unknown_fields.is_empty() {
                return Err(RecipeFailure::Unsupported);
            }
            let comparison = match state.comparison {
                EnumValue::Known(wire::DashboardComparison::Equal) => Comparison::Equal,
                EnumValue::Known(wire::DashboardComparison::NotEqual) => Comparison::NotEqual,
                EnumValue::Known(wire::DashboardComparison::Unspecified)
                | EnumValue::Unknown(_) => {
                    return Err(RecipeFailure::Unsupported);
                }
            };
            Ok(Predicate::State {
                field: decode_field(state.field.as_option().ok_or(RecipeFailure::Invalid)?)?,
                expected: decode_literal(
                    state.expected.as_option().ok_or(RecipeFailure::Invalid)?,
                )?,
                comparison,
            })
        }
        Some(Condition::All(list)) | Some(Condition::Any(list)) => {
            if !list.__buffa_unknown_fields.is_empty() {
                return Err(RecipeFailure::Unsupported);
            }
            if list.conditions.is_empty() {
                return Err(RecipeFailure::Invalid);
            }
            if list.conditions.len() > *remaining {
                return Err(RecipeFailure::Budget);
            }
            let children = list
                .conditions
                .iter()
                .map(|child| predicate(child, depth + 1, remaining))
                .collect::<Result<_, _>>()?;
            match value.condition.as_ref() {
                Some(Condition::All(_)) => Ok(Predicate::All(children)),
                Some(Condition::Any(_)) => Ok(Predicate::Any(children)),
                Some(Condition::Constant(_) | Condition::State(_)) | None => {
                    Err(RecipeFailure::Invalid)
                }
            }
        }
        None => Err(RecipeFailure::Invalid),
    }
}

pub fn decode_interactions(
    value: &wire::DashboardInteractions,
) -> Result<std::collections::BTreeMap<Gesture, Interaction>, RecipeFailure> {
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(RecipeFailure::Unsupported);
    }
    if value.interactions.len() > 3 {
        return Err(RecipeFailure::Budget);
    }
    let mut interactions = std::collections::BTreeMap::new();
    for value in &value.interactions {
        let (gesture, interaction) = interaction(value)?;
        if interactions.insert(gesture, interaction).is_some() {
            return Err(RecipeFailure::Invalid);
        }
    }
    Ok(interactions)
}

pub fn decode_predicate(value: &wire::DashboardPredicate) -> Result<Predicate, RecipeFailure> {
    predicate(value, 0, &mut 64)
}
