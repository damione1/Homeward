//! Instance-owned capabilities. Observation never grants a command; every
//! intention is checked again against the same admitted constraint snapshot.
use super::{
    Refusal,
    number::{Bounds, NumericInput},
};
use home_wire_contract::{
    api_controls_contract as admit,
    home::api::v2::{self as wire, NumberBounds},
};

#[derive(Clone, Debug)]
pub(crate) enum CoverIntent {
    Operation(wire::CoverOperation),
    Position(NumericInput),
}
#[derive(Clone, PartialEq)]
pub(super) struct Cover {
    limits: wire::CoverDefinition,
    operations: Vec<wire::CoverOperation>,
    position: Option<Bounds>,
}
impl Cover {
    pub fn parse(limits: &wire::CoverDefinition) -> Option<Self> {
        if !admit::cover_constraints(limits) {
            return None;
        }
        let position = match limits.set_position_percent.as_option() {
            Some(b) => Some(Bounds::parse(&NumberBounds {
                minimum: b.minimum,
                maximum: b.maximum,
                step: b.step,
                ..Default::default()
            })?),
            None => None,
        };
        Some(Self {
            limits: limits.clone(),
            operations: limits
                .operations
                .iter()
                .copied()
                .map(admit::cover_operation)
                .collect::<Option<_>>()?,
            position,
        })
    }
    pub fn operations(&self) -> &[wire::CoverOperation] {
        &self.operations
    }
    pub fn position(&self) -> Option<Bounds> {
        self.position
    }
    pub fn restricted(&self, operations: bool, position: bool) -> Self {
        let mut value = self.clone();
        if !operations {
            value.operations.clear();
            value.limits.operations.clear();
        }
        if !position {
            value.position = None;
            value.limits.set_position_percent = Default::default();
        }
        value
    }
    pub fn observed_position(&self, value: &wire::CoverValue) -> Option<f64> {
        if !admit::cover_observation(value, &self.limits) {
            return None;
        }
        value
            .position_percent
            .filter(|p| self.position.is_some_and(|b| b.admit(*p).is_ok()))
    }
    pub fn arguments(&self, intent: CoverIntent) -> Result<wire::SetCoverCommand, Refusal> {
        let command = match intent {
            CoverIntent::Operation(operation) => {
                wire::set_cover_command::Target::Operation(operation.into())
            }
            CoverIntent::Position(value) => wire::set_cover_command::Target::PositionPercent(
                value.admit(self.position.ok_or(Refusal::Unsupported)?)?,
            ),
        };
        let value = wire::SetCoverCommand {
            target: Some(command),
            ..Default::default()
        };
        if admit::cover_action(&value, &self.limits) {
            Ok(value)
        } else {
            Err(Refusal::Unsupported)
        }
    }
}
#[derive(Clone, PartialEq)]
pub(super) struct Lock {
    limits: wire::LockDefinition,
    operations: Vec<wire::LockOperation>,
}
impl Lock {
    pub fn parse(limits: &wire::LockDefinition) -> Option<Self> {
        if !admit::lock_constraints(limits) {
            return None;
        }
        Some(Self {
            limits: limits.clone(),
            operations: limits
                .operations
                .iter()
                .copied()
                .map(admit::lock_operation)
                .collect::<Option<_>>()?,
        })
    }
    pub fn operations(&self) -> &[wire::LockOperation] {
        &self.operations
    }
    pub fn arguments(
        &self,
        operation: wire::LockOperation,
    ) -> Result<wire::SetLockCommand, Refusal> {
        let value = wire::SetLockCommand {
            operation: operation.into(),
            ..Default::default()
        };
        if admit::lock_action(&value, &self.limits) {
            Ok(value)
        } else {
            Err(Refusal::Unsupported)
        }
    }
}
#[derive(Clone, PartialEq)]
pub(super) struct Alarm {
    limits: wire::AlarmDefinition,
    operations: Vec<wire::AlarmOperation>,
}
impl Alarm {
    pub fn parse(limits: &wire::AlarmDefinition) -> Option<Self> {
        if !admit::alarm_constraints(limits) {
            return None;
        }
        Some(Self {
            limits: limits.clone(),
            operations: limits
                .operations
                .iter()
                .copied()
                .map(admit::alarm_operation)
                .collect::<Option<_>>()?,
        })
    }
    pub fn operations(&self) -> &[wire::AlarmOperation] {
        &self.operations
    }
    pub fn arguments(
        &self,
        operation: wire::AlarmOperation,
    ) -> Result<wire::SetAlarmCommand, Refusal> {
        let value = wire::SetAlarmCommand {
            operation: operation.into(),
            ..Default::default()
        };
        if admit::alarm_action(&value, &self.limits) {
            Ok(value)
        } else {
            Err(Refusal::Unsupported)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::super::{DeviceIntent, Power, capability::Control};
    use super::*;
    use buffa::EnumValue;
    fn limits() -> wire::CoverDefinition {
        wire::CoverDefinition {
            operations: vec![
                wire::CoverOperation::Stop.into(),
                wire::CoverOperation::Open.into(),
            ],
            set_position_percent: wire::PercentageBounds {
                minimum: 10.,
                maximum: 90.,
                step: Some(5.),
                ..Default::default()
            }
            .into(),
            observes_position_percent: true,
            ..Default::default()
        }
    }
    #[test]
    fn exact_capability_family_and_instance_operations_are_required() {
        let capability = wire::Capability {
            r#trait: Some(wire::capability::Trait::Cover(Box::new(
                wire::CoverCapability {
                    definition: limits().into(),
                    ..Default::default()
                },
            ))),
            ..Default::default()
        };
        let control = Control::parse(&capability);
        let args = control
            .arguments(
                DeviceIntent::Cover(CoverIntent::Operation(wire::CoverOperation::Stop)),
                None,
            )
            .unwrap();
        assert!(
            matches!(args,wire::execute_capability_request::Command::Cover(value) if value.target==Some(wire::set_cover_command::Target::Operation(wire::CoverOperation::Stop.into())))
        );
        for intent in [
            DeviceIntent::Cover(CoverIntent::Operation(wire::CoverOperation::Close)),
            DeviceIntent::Power(Power::On),
            DeviceIntent::Lock(wire::LockOperation::Unlock),
            DeviceIntent::Alarm(wire::AlarmOperation::Disarm),
        ] {
            assert!(control.arguments(intent, None).is_err());
        }
        let mut foreign = capability.clone();
        foreign.r#trait = Some(wire::capability::Trait::Opaque(Box::default()));
        assert!(matches!(Control::parse(&foreign), Control::Unsupported));
        foreign = capability;
        foreign.r#trait = None;
        assert!(matches!(Control::parse(&foreign), Control::Unsupported));
    }
    #[test]
    fn cover_position_keeps_absence_zero_and_observation_outside_command_bounds_distinct() {
        let cover = Cover::parse(&limits()).unwrap();
        for value in ["0", "35.1", "95", "NaN", "-"] {
            assert!(
                cover
                    .arguments(CoverIntent::Position(NumericInput::Text(value.into())))
                    .is_err()
            );
        }
        let action = cover
            .arguments(CoverIntent::Position(NumericInput::Text("35".into())))
            .unwrap();
        assert_eq!(
            action.target,
            Some(wire::set_cover_command::Target::PositionPercent(35.))
        );
        assert!(
            cover
                .observed_position(&wire::CoverValue::default())
                .is_none()
        );
        let observed = wire::CoverValue {
            position_percent: Some(0.),
            ..Default::default()
        };
        assert!(cover.observed_position(&observed).is_none());
        let mut zero = limits();
        zero.set_position_percent.as_option_mut().unwrap().minimum = 0.;
        let cover = Cover::parse(&zero).unwrap();
        assert_eq!(cover.observed_position(&observed), Some(0.));
        zero.observes_position_percent = false;
        assert!(
            Cover::parse(&zero)
                .unwrap()
                .observed_position(&observed)
                .is_none()
        );
    }
    #[test]
    fn malformed_future_and_duplicate_constraints_never_become_partial_controls() {
        let mut value = limits();
        value.operations.push(EnumValue::Unknown(99));
        assert!(Cover::parse(&value).is_none());
        value = limits();
        value.operations.push(wire::CoverOperation::Stop.into());
        assert!(Cover::parse(&value).is_none());
        value = limits();
        value.set_position_percent.as_option_mut().unwrap().step = Some(0.);
        assert!(Cover::parse(&value).is_none());
        value = limits();
        value.set_position_percent.as_option_mut().unwrap().maximum = 101.;
        assert!(Cover::parse(&value).is_none());
    }
    #[test]
    fn locks_and_alarms_keep_observation_vocabulary_separate_from_command_admission() {
        let lock = Lock::parse(&wire::LockDefinition {
            operations: vec![wire::LockOperation::Lock.into()],
            observed_states: vec![wire::LockState::Jammed.into()],
            ..Default::default()
        })
        .unwrap();
        assert!(lock.arguments(wire::LockOperation::Lock).is_ok());
        assert!(lock.arguments(wire::LockOperation::Unlock).is_err());
        assert!(lock.arguments(wire::LockOperation::Unspecified).is_err());
        let alarm = Alarm::parse(&wire::AlarmDefinition {
            operations: vec![wire::AlarmOperation::Disarm.into()],
            observed_states: vec![
                wire::AlarmState::Triggered.into(),
                wire::AlarmState::ArmedNight.into(),
            ],
            ..Default::default()
        })
        .unwrap();
        assert!(alarm.arguments(wire::AlarmOperation::Disarm).is_ok());
        assert!(alarm.arguments(wire::AlarmOperation::ArmHome).is_err());
        assert!(alarm.arguments(wire::AlarmOperation::ArmAway).is_err());
    }
    #[test]
    fn native_feature_restriction_changes_argument_admission_and_preserves_observations() {
        let cover = Cover::parse(&limits()).unwrap();
        let positions = cover.restricted(false, true);
        assert!(positions.operations().is_empty());
        assert!(
            positions
                .arguments(CoverIntent::Operation(wire::CoverOperation::Open))
                .is_err()
        );
        assert!(
            positions
                .arguments(CoverIntent::Position(NumericInput::Text("40".into())))
                .is_ok()
        );
        let observation = wire::CoverValue {
            position_percent: Some(35.),
            ..Default::default()
        };
        assert_eq!(positions.observed_position(&observation), Some(35.));
        let operations = cover.restricted(true, false);
        assert!(operations.position().is_none());
        assert!(
            operations
                .arguments(CoverIntent::Position(NumericInput::Text("40".into())))
                .is_err()
        );
        assert!(
            operations
                .arguments(CoverIntent::Operation(wire::CoverOperation::Open))
                .is_ok()
        );
    }
}
