//! Product labels and exact HA predicate literals for typed control observations.
use home_wire_contract::{api_controls_contract as admit, home::api::v2 as wire};

pub(crate) struct StateText {
    pub label: &'static str,
    #[cfg(test)]
    pub literal: &'static str,
}
pub(crate) fn cover_state(value: wire::CoverState) -> Option<StateText> {
    use wire::CoverState as S;
    let (label, _literal) = match value {
        S::Open => ("Open", "open"),
        S::Closed => ("Closed", "closed"),
        S::Opening => ("Opening", "opening"),
        S::Closing => ("Closing", "closing"),
        S::Unspecified => return None,
    };
    Some(StateText {
        label,
        #[cfg(test)]
        literal: _literal,
    })
}
pub(crate) fn lock_state(value: wire::LockState) -> Option<StateText> {
    use wire::LockState as S;
    let (label, _literal) = match value {
        S::Locked => ("Locked", "locked"),
        S::Unlocked => ("Unlocked", "unlocked"),
        S::Locking => ("Locking", "locking"),
        S::Unlocking => ("Unlocking", "unlocking"),
        S::Jammed => ("Lock jammed", "jammed"),
        S::Opening => ("Opening", "opening"),
        S::Open => ("Open", "open"),
        S::Unspecified => return None,
    };
    Some(StateText {
        label,
        #[cfg(test)]
        literal: _literal,
    })
}
pub(crate) fn alarm_state(value: wire::AlarmState) -> Option<StateText> {
    use wire::AlarmState as S;
    let (label, _literal) = match value {
        S::Disarmed => ("Disarmed", "disarmed"),
        S::ArmedHome => ("Armed · home", "armed_home"),
        S::ArmedAway => ("Armed · away", "armed_away"),
        S::ArmedNight => ("Armed · night", "armed_night"),
        S::ArmedVacation => ("Armed · vacation", "armed_vacation"),
        S::ArmedCustomBypass => ("Armed · custom bypass", "armed_custom_bypass"),
        S::Pending => ("Pending trigger", "pending"),
        S::Arming => ("Arming", "arming"),
        S::Disarming => ("Disarming", "disarming"),
        S::Triggered => ("Alarm triggered", "triggered"),
        S::Unspecified => return None,
    };
    Some(StateText {
        label,
        #[cfg(test)]
        literal: _literal,
    })
}
pub(crate) fn cover(value: &wire::CoverValue) -> String {
    if !admit::cover_value(value) {
        return "Unsupported value".into();
    }
    let mut parts = Vec::new();
    if let Some(text) = value
        .state
        .and_then(admit::cover_state)
        .and_then(cover_state)
    {
        parts.push(text.label.to_owned());
    }
    if let Some(position) = value.position_percent {
        parts.push(format!("{position} %"));
    }
    if let Some(obstructed) = value.obstructed {
        parts.push(
            if obstructed {
                "Obstacle detected"
            } else {
                "No obstacle detected"
            }
            .into(),
        );
    }
    parts.join(" · ")
}
pub(crate) fn lock(value: &wire::LockValue) -> &'static str {
    if !admit::lock_value(value) {
        return "Unsupported value";
    }
    admit::lock_state(value.state)
        .and_then(lock_state)
        .map(|text| text.label)
        .unwrap_or("Unsupported value")
}
pub(crate) fn alarm(value: &wire::AlarmValue) -> &'static str {
    if !admit::alarm_value(value) {
        return "Unsupported value";
    }
    admit::alarm_state(value.state)
        .and_then(alarm_state)
        .map(|text| text.label)
        .unwrap_or("Unsupported value")
}

pub(crate) fn cover_operation(value: wire::CoverOperation) -> Option<&'static str> {
    match value {
        wire::CoverOperation::Open => Some("Open"),
        wire::CoverOperation::Close => Some("Close"),
        wire::CoverOperation::Stop => Some("Stop"),
        wire::CoverOperation::Unspecified => None,
    }
}
pub(crate) fn lock_operation(value: wire::LockOperation) -> Option<&'static str> {
    match value {
        wire::LockOperation::Lock => Some("Lock"),
        wire::LockOperation::Unlock => Some("Unlock"),
        wire::LockOperation::Unspecified => None,
    }
}
pub(crate) fn alarm_operation(value: wire::AlarmOperation) -> Option<&'static str> {
    match value {
        wire::AlarmOperation::ArmHome => Some("Arm Home"),
        wire::AlarmOperation::ArmAway => Some("Arm Away"),
        wire::AlarmOperation::ArmNight => Some("Arm Night"),
        wire::AlarmOperation::Disarm => Some("Disarm"),
        wire::AlarmOperation::Unspecified => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use buffa::EnumValue;

    #[test]
    fn typed_control_labels_keep_presence_and_security_states_distinct() {
        let value = wire::CoverValue {
            position_percent: Some(0.),
            obstructed: Some(false),
            ..Default::default()
        };
        assert_eq!(cover(&value), "0 % · No obstacle detected");
        assert_eq!(
            cover(&wire::CoverValue {
                state: Some(wire::CoverState::Open.into()),
                position_percent: Some(35.),
                ..Default::default()
            }),
            "Open · 35 %"
        );
        assert_eq!(cover(&wire::CoverValue::default()), "Unsupported value");
        assert_eq!(
            lock(&wire::LockValue {
                state: wire::LockState::Jammed.into(),
                ..Default::default()
            }),
            "Lock jammed"
        );
        assert_eq!(
            alarm(&wire::AlarmValue {
                state: wire::AlarmState::Triggered.into(),
                ..Default::default()
            }),
            "Alarm triggered"
        );
        assert_eq!(
            lock(&wire::LockValue {
                state: EnumValue::Unknown(99),
                ..Default::default()
            }),
            "Unsupported value"
        );
        assert_eq!(
            lock_state(wire::LockState::Unlocked).unwrap().literal,
            "unlocked"
        );
        assert_eq!(lock_state(wire::LockState::Open).unwrap().literal, "open");
        assert_eq!(
            alarm_state(wire::AlarmState::Pending).unwrap().literal,
            "pending"
        );
    }
}
