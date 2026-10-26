use buffa::{EnumValue, Enumeration, Message};
use home_wire_contract::{controls_contract as admit, home::controls::v1 as w};

fn cover_limits() -> w::CoverConstraints {
    w::CoverConstraints {
        operations: vec![
            w::CoverOperation::Open.into(),
            w::CoverOperation::Stop.into(),
        ],
        set_position_percent: w::PercentBounds {
            minimum: 10.,
            maximum: 90.,
            step: Some(5.),
            ..Default::default()
        }
        .into(),
        observed_states: vec![w::CoverState::Open.into(), w::CoverState::Closed.into()],
        observes_position_percent: true,
        observes_obstruction: true,
        ..Default::default()
    }
}
fn unknown<M: Message + Default>(value: &M) -> M {
    let mut bytes = value.encode_to_vec();
    bytes.extend_from_slice(&[0x98, 6, 1]);
    M::decode_from_slice(&bytes).unwrap()
}
#[test]
fn all_states_are_closed_and_do_not_collapse() {
    for state in w::CoverState::values() {
        assert_eq!(
            admit::cover_state((*state).into()).is_some(),
            *state != w::CoverState::Unspecified
        );
    }
    for state in w::LockState::values() {
        assert_eq!(
            admit::lock_state((*state).into()).is_some(),
            *state != w::LockState::Unspecified
        );
        let value = w::LockValue {
            state: (*state).into(),
            ..Default::default()
        };
        let decoded = w::LockValue::decode_from_slice(&value.encode_to_vec()).unwrap();
        assert_eq!(decoded.state, value.state);
    }
    for state in w::AlarmState::values() {
        assert_eq!(
            admit::alarm_state((*state).into()).is_some(),
            *state != w::AlarmState::Unspecified
        );
        let value = w::AlarmValue {
            state: (*state).into(),
            ..Default::default()
        };
        let decoded = w::AlarmValue::decode_from_slice(&value.encode_to_vec()).unwrap();
        assert_eq!(decoded.state, value.state);
    }
    assert!(admit::cover_state(EnumValue::Unknown(99)).is_none());
    assert!(admit::lock_state(EnumValue::Unknown(99)).is_none());
    assert!(admit::alarm_state(EnumValue::Unknown(99)).is_none());
    assert!(admit::cover_operation(EnumValue::Unknown(99)).is_none());
    assert!(admit::lock_operation(EnumValue::Unknown(99)).is_none());
    assert!(admit::alarm_operation(EnumValue::Unknown(99)).is_none());
}
#[test]
fn cover_presence_is_exact_and_observations_are_independent_of_command_limits() {
    // Fixed protobuf bytes: double position field2=0 and optional bool field3=false.
    let bytes = [0x11, 0, 0, 0, 0, 0, 0, 0, 0, 0x18, 0];
    let value = w::CoverValue::decode_from_slice(&bytes).unwrap();
    assert_eq!(value.state, None);
    assert_eq!(value.position_percent, Some(0.));
    assert_eq!(value.obstructed, Some(false));
    assert_eq!(value.encode_to_vec(), bytes);
    assert!(admit::cover_observation(&value, &cover_limits()));
    let mut limits = cover_limits();
    limits.observes_obstruction = false;
    assert!(!admit::cover_observation(&value, &limits));
    for position in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY, -0.1, 100.1] {
        assert!(!admit::cover_value(&w::CoverValue {
            position_percent: Some(position),
            ..Default::default()
        }));
    }
    assert!(!admit::cover_value(&w::CoverValue::default()));
    assert!(!admit::cover_value(&unknown(&value)));
    assert_eq!(unknown(&value).position_percent, Some(0.));
}
#[test]
fn commands_require_their_own_advertised_operation_and_position_grid() {
    let limits = cover_limits();
    for (operation, valid) in [
        (w::CoverOperation::Open, true),
        (w::CoverOperation::Close, false),
        (w::CoverOperation::Stop, true),
        (w::CoverOperation::Unspecified, false),
    ] {
        let command = w::CoverAction {
            command: Some(w::cover_action::Command::Operation(operation.into())),
            ..Default::default()
        };
        assert_eq!(admit::cover_action(&command, &limits), valid);
        assert!(!admit::cover_action(&unknown(&command), &limits));
    }
    for (position, valid) in [
        (0., false),
        (10., true),
        (12., false),
        (15., true),
        (90., true),
        (95., false),
        (f64::NAN, false),
        (f64::INFINITY, false),
    ] {
        let command = w::CoverAction {
            command: Some(w::cover_action::Command::PositionPercent(position)),
            ..Default::default()
        };
        assert_eq!(admit::cover_action(&command, &limits), valid);
    }
    assert!(!admit::cover_action(&w::CoverAction::default(), &limits));
    let lock_limits = w::LockConstraints {
        operations: vec![w::LockOperation::Lock.into()],
        observed_states: vec![w::LockState::Jammed.into()],
        ..Default::default()
    };
    let mut lock = w::LockAction {
        operation: w::LockOperation::Lock.into(),
        ..Default::default()
    };
    assert!(admit::lock_action(&lock, &lock_limits));
    lock.operation = w::LockOperation::Unlock.into();
    assert!(!admit::lock_action(&lock, &lock_limits));
    assert!(admit::lock_observation(
        &w::LockValue {
            state: w::LockState::Jammed.into(),
            ..Default::default()
        },
        &lock_limits
    ));
    assert!(!admit::lock_observation(
        &w::LockValue {
            state: w::LockState::Unlocked.into(),
            ..Default::default()
        },
        &lock_limits
    ));
    let alarm_limits = w::AlarmConstraints {
        operations: vec![w::AlarmOperation::ArmHome.into()],
        observed_states: vec![w::AlarmState::Triggered.into()],
        ..Default::default()
    };
    let mut alarm = w::AlarmAction {
        operation: w::AlarmOperation::ArmHome.into(),
        ..Default::default()
    };
    assert!(admit::alarm_action(&alarm, &alarm_limits));
    alarm.operation = w::AlarmOperation::Disarm.into();
    assert!(!admit::alarm_action(&alarm, &alarm_limits));
    assert!(admit::alarm_observation(
        &w::AlarmValue {
            state: w::AlarmState::Triggered.into(),
            ..Default::default()
        },
        &alarm_limits
    ));
}
#[test]
fn constraints_reject_unknown_duplicate_and_unusable_declarations() {
    assert!(!admit::cover_constraints(&w::CoverConstraints::default()));
    assert!(!admit::lock_constraints(&w::LockConstraints::default()));
    assert!(!admit::alarm_constraints(&w::AlarmConstraints::default()));
    let limits = cover_limits();
    assert!(admit::cover_constraints(&limits));
    assert!(!admit::cover_constraints(&unknown(&limits)));
    let mut duplicate = limits.clone();
    duplicate.operations.push(duplicate.operations[0]);
    assert!(!admit::cover_constraints(&duplicate));
    let mut future = limits.clone();
    future.observed_states.push(EnumValue::Unknown(99));
    assert!(!admit::cover_constraints(&future));
    let mut nested = limits;
    nested.set_position_percent = unknown(nested.set_position_percent.as_option().unwrap()).into();
    assert!(!admit::cover_constraints(&nested));
}
