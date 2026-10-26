//! Structural and instance admission for covers, locks and alarm panels.
//! Command support never follows from an observed state. Session authorization
//! and dispatch remain server responsibilities.
use crate::home::controls::v1 as wire;
use buffa::{EnumValue, Enumeration};

pub fn cover_state(value: EnumValue<wire::CoverState>) -> Option<wire::CoverState> {
    use wire::CoverState as S;
    match value {
        EnumValue::Known(state @ (S::Open | S::Closed | S::Opening | S::Closing)) => Some(state),
        EnumValue::Known(S::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub fn lock_state(value: EnumValue<wire::LockState>) -> Option<wire::LockState> {
    use wire::LockState as S;
    match value {
        EnumValue::Known(
            state @ (S::Locked
            | S::Unlocked
            | S::Locking
            | S::Unlocking
            | S::Jammed
            | S::Opening
            | S::Open),
        ) => Some(state),
        EnumValue::Known(S::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub fn alarm_state(value: EnumValue<wire::AlarmState>) -> Option<wire::AlarmState> {
    use wire::AlarmState as S;
    match value {
        EnumValue::Known(
            state @ (S::Disarmed
            | S::ArmedHome
            | S::ArmedAway
            | S::ArmedNight
            | S::ArmedVacation
            | S::ArmedCustomBypass
            | S::Pending
            | S::Arming
            | S::Disarming
            | S::Triggered),
        ) => Some(state),
        EnumValue::Known(S::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub fn cover_operation(value: EnumValue<wire::CoverOperation>) -> Option<wire::CoverOperation> {
    use wire::CoverOperation as O;
    match value {
        EnumValue::Known(operation @ (O::Open | O::Close | O::Stop)) => Some(operation),
        EnumValue::Known(O::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub fn lock_operation(value: EnumValue<wire::LockOperation>) -> Option<wire::LockOperation> {
    use wire::LockOperation as O;
    match value {
        EnumValue::Known(operation @ (O::Lock | O::Unlock)) => Some(operation),
        EnumValue::Known(O::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
pub fn alarm_operation(value: EnumValue<wire::AlarmOperation>) -> Option<wire::AlarmOperation> {
    use wire::AlarmOperation as O;
    match value {
        EnumValue::Known(operation @ (O::ArmHome | O::ArmAway | O::Disarm)) => Some(operation),
        EnumValue::Known(O::Unspecified) | EnumValue::Unknown(_) => None,
    }
}
fn options<E: Enumeration>(
    values: &[EnumValue<E>],
    admit: impl Fn(EnumValue<E>) -> Option<E>,
    maximum: usize,
) -> bool {
    values.len() <= maximum
        && values
            .iter()
            .enumerate()
            .all(|(index, value)| admit(*value).is_some() && !values[..index].contains(value))
}
pub fn percent_bounds(value: &wire::PercentBounds) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && value.minimum.is_finite()
        && value.maximum.is_finite()
        && value.minimum >= 0.
        && value.maximum <= 100.
        && value.minimum <= value.maximum
        && value.step.is_none_or(|step| step.is_finite() && step > 0.)
}
pub fn position_admitted(value: f64, bounds: &wire::PercentBounds) -> bool {
    percent_bounds(bounds)
        && value.is_finite()
        && (bounds.minimum..=bounds.maximum).contains(&value)
        && bounds.step.is_none_or(|step| {
            let steps = (value - bounds.minimum) / step;
            (steps - steps.round()).abs() <= 1e-9
        })
}
pub fn cover_constraints(value: &wire::CoverConstraints) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && options(&value.operations, cover_operation, 3)
        && options(&value.observed_states, cover_state, 4)
        && value
            .set_position_percent
            .as_option()
            .is_none_or(percent_bounds)
        && (!value.operations.is_empty()
            || value.set_position_percent.as_option().is_some()
            || !value.observed_states.is_empty()
            || value.observes_position_percent
            || value.observes_obstruction)
}
pub fn lock_constraints(value: &wire::LockConstraints) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && options(&value.operations, lock_operation, 2)
        && options(&value.observed_states, lock_state, 7)
        && (!value.operations.is_empty() || !value.observed_states.is_empty())
}
pub fn alarm_constraints(value: &wire::AlarmConstraints) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && options(&value.operations, alarm_operation, 3)
        && options(&value.observed_states, alarm_state, 10)
        && (!value.operations.is_empty() || !value.observed_states.is_empty())
}
pub fn cover_value(value: &wire::CoverValue) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && (value.state.is_some() || value.position_percent.is_some() || value.obstructed.is_some())
        && value.state.is_none_or(|state| cover_state(state).is_some())
        && value
            .position_percent
            .is_none_or(|position| position.is_finite() && (0.0..=100.0).contains(&position))
}
pub fn lock_value(value: &wire::LockValue) -> bool {
    value.__buffa_unknown_fields.is_empty() && lock_state(value.state).is_some()
}
pub fn alarm_value(value: &wire::AlarmValue) -> bool {
    value.__buffa_unknown_fields.is_empty() && alarm_state(value.state).is_some()
}
pub fn cover_observation(value: &wire::CoverValue, limits: &wire::CoverConstraints) -> bool {
    cover_value(value)
        && cover_constraints(limits)
        && value
            .state
            .is_none_or(|state| limits.observed_states.contains(&state))
        && (value.position_percent.is_none() || limits.observes_position_percent)
        && (value.obstructed.is_none() || limits.observes_obstruction)
}
pub fn lock_observation(value: &wire::LockValue, limits: &wire::LockConstraints) -> bool {
    lock_value(value) && lock_constraints(limits) && limits.observed_states.contains(&value.state)
}
pub fn alarm_observation(value: &wire::AlarmValue, limits: &wire::AlarmConstraints) -> bool {
    alarm_value(value) && alarm_constraints(limits) && limits.observed_states.contains(&value.state)
}
pub fn cover_action(value: &wire::CoverAction, limits: &wire::CoverConstraints) -> bool {
    use wire::cover_action::Command;
    value.__buffa_unknown_fields.is_empty()
        && cover_constraints(limits)
        && match &value.command {
            Some(Command::Operation(operation)) => {
                cover_operation(*operation).is_some() && limits.operations.contains(operation)
            }
            Some(Command::PositionPercent(position)) => limits
                .set_position_percent
                .as_option()
                .is_some_and(|bounds| position_admitted(*position, bounds)),
            None => false,
        }
}
pub fn lock_action(value: &wire::LockAction, limits: &wire::LockConstraints) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && lock_constraints(limits)
        && lock_operation(value.operation).is_some()
        && limits.operations.contains(&value.operation)
}
pub fn alarm_action(value: &wire::AlarmAction, limits: &wire::AlarmConstraints) -> bool {
    value.__buffa_unknown_fields.is_empty()
        && alarm_constraints(limits)
        && alarm_operation(value.operation).is_some()
        && limits.operations.contains(&value.operation)
}
