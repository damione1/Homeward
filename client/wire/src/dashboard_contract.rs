//! Strict decoding of the negotiated, inert dashboard recipe vocabulary.
use crate::home::dashboard::v1 as wire;
use buffa::EnumValue;
use home_client_model::{EntityId, dashboard::*};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RecipeFailure {
    Unsupported,
    Invalid,
    Budget,
}

fn text(value: &str, empty: bool, limit: usize) -> bool {
    (empty || !value.is_empty()) && value.len() <= limit && !value.chars().any(char::is_control)
}
fn entity(value: &str) -> Result<EntityId, RecipeFailure> {
    if !text(value, false, 4096) {
        return Err(RecipeFailure::Invalid);
    }
    value.parse().map_err(|_| RecipeFailure::Invalid)
}
fn source(value: Option<&wire::LayoutSource>) -> Result<Source, RecipeFailure> {
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
        line: value.line,
        column: value.column,
        path: value.path.clone(),
        variant: value.variant.clone(),
    })
}
fn command(value: EnumValue<wire::NativeCommandKind>) -> Result<CommandKind, RecipeFailure> {
    match value {
        EnumValue::Known(wire::NativeCommandKind::Toggle) => Ok(CommandKind::Toggle),
        EnumValue::Known(wire::NativeCommandKind::TurnOn) => Ok(CommandKind::TurnOn),
        EnumValue::Known(wire::NativeCommandKind::TurnOff) => Ok(CommandKind::TurnOff),
        EnumValue::Known(wire::NativeCommandKind::Activate) => Ok(CommandKind::Activate),
        EnumValue::Known(wire::NativeCommandKind::CoverOpen) => Ok(CommandKind::CoverOpen),
        EnumValue::Known(wire::NativeCommandKind::CoverClose) => Ok(CommandKind::CoverClose),
        EnumValue::Known(wire::NativeCommandKind::CoverStop) => Ok(CommandKind::CoverStop),
        EnumValue::Known(wire::NativeCommandKind::Lock) => Ok(CommandKind::Lock),
        EnumValue::Known(wire::NativeCommandKind::Unlock) => Ok(CommandKind::Unlock),
        EnumValue::Known(wire::NativeCommandKind::AlarmArmHome) => Ok(CommandKind::AlarmArmHome),
        EnumValue::Known(wire::NativeCommandKind::AlarmArmAway) => Ok(CommandKind::AlarmArmAway),
        EnumValue::Known(wire::NativeCommandKind::AlarmDisarm) => Ok(CommandKind::AlarmDisarm),
        EnumValue::Known(wire::NativeCommandKind::Unspecified) | EnumValue::Unknown(_) => {
            Err(RecipeFailure::Unsupported)
        }
    }
}
fn interaction(value: &wire::NativeInteraction) -> Result<(Gesture, Interaction), RecipeFailure> {
    use wire::native_interaction::Intention as I;
    if !value.__buffa_unknown_fields.is_empty() {
        return Err(RecipeFailure::Unsupported);
    }
    let gesture = match value.gesture {
        EnumValue::Known(wire::NativeGesture::Tap) => Gesture::Tap,
        EnumValue::Known(wire::NativeGesture::Hold) => Gesture::Hold,
        EnumValue::Known(wire::NativeGesture::DoubleTap) => Gesture::DoubleTap,
        EnumValue::Known(wire::NativeGesture::Unspecified) | EnumValue::Unknown(_) => {
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
            Intention::MoreInfo(entity(&target.entity_id)?)
        }
        Some(I::Navigate(target)) => {
            if !target.__buffa_unknown_fields.is_empty() {
                return Err(RecipeFailure::Unsupported);
            }
            let dashboard = target
                .dashboard_id
                .as_option()
                .ok_or(RecipeFailure::Invalid)?;
            if !dashboard.__buffa_unknown_fields.is_empty() {
                return Err(RecipeFailure::Unsupported);
            }
            let presentation = match target.presentation {
                EnumValue::Known(wire::NativePresentation::Page) => Presentation::Page,
                EnumValue::Known(wire::NativePresentation::Popup) => Presentation::Popup,
                EnumValue::Known(wire::NativePresentation::Unspecified) | EnumValue::Unknown(_) => {
                    return Err(RecipeFailure::Unsupported);
                }
            };
            Intention::Navigate(Destination {
                dashboard: DashboardId::parse(&dashboard.value)
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
                entity: entity(&target.entity_id)?,
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
    value: &wire::NativePredicate,
    depth: usize,
    remaining: &mut usize,
) -> Result<Predicate, RecipeFailure> {
    use wire::native_predicate::Condition;
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
            if state.state.len() > 256 || state.state.contains('\0') {
                return Err(RecipeFailure::Invalid);
            }
            let comparison = match state.comparison {
                EnumValue::Known(wire::NativeComparison::Equal) => Comparison::Equal,
                EnumValue::Known(wire::NativeComparison::NotEqual) => Comparison::NotEqual,
                EnumValue::Known(wire::NativeComparison::Unspecified) | EnumValue::Unknown(_) => {
                    return Err(RecipeFailure::Unsupported);
                }
            };
            Ok(Predicate::State {
                entity: entity(&state.entity_id)?,
                expected: state.state.clone(),
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

pub fn decode_recipes(value: &wire::NativeRecipes) -> Result<Recipes, RecipeFailure> {
    if value.version != 1 || !value.__buffa_unknown_fields.is_empty() {
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
    let visibility = value
        .visibility
        .as_option()
        .map(|value| predicate(value, 0, &mut 64))
        .transpose()?;
    Ok(Recipes {
        interactions,
        visibility,
    })
}
