//! Product labels and exact HA predicate literals for typed control observations.
use home_wire_contract::{controls_contract as admit, home::controls::v1 as wire};

pub(crate) struct StateText {
    pub label: &'static str,
    pub literal: &'static str,
}
pub(crate) fn cover_state(value: wire::CoverState) -> Option<StateText> {
    use wire::CoverState as S;
    let (label, literal) = match value {
        S::Open => ("Ouvert", "open"),
        S::Closed => ("Fermé", "closed"),
        S::Opening => ("Ouverture en cours", "opening"),
        S::Closing => ("Fermeture en cours", "closing"),
        S::Unspecified => return None,
    };
    Some(StateText { label, literal })
}
pub(crate) fn lock_state(value: wire::LockState) -> Option<StateText> {
    use wire::LockState as S;
    let (label, literal) = match value {
        S::Locked => ("Verrouillé", "locked"),
        S::Unlocked => ("Déverrouillé", "unlocked"),
        S::Locking => ("Verrouillage en cours", "locking"),
        S::Unlocking => ("Déverrouillage en cours", "unlocking"),
        S::Jammed => ("Serrure bloquée", "jammed"),
        S::Opening => ("Ouverture en cours", "opening"),
        S::Open => ("Ouvert", "open"),
        S::Unspecified => return None,
    };
    Some(StateText { label, literal })
}
pub(crate) fn alarm_state(value: wire::AlarmState) -> Option<StateText> {
    use wire::AlarmState as S;
    let (label, literal) = match value {
        S::Disarmed => ("Désarmée", "disarmed"),
        S::ArmedHome => ("Armée · présence", "armed_home"),
        S::ArmedAway => ("Armée · absence", "armed_away"),
        S::ArmedNight => ("Armée · nuit", "armed_night"),
        S::ArmedVacation => ("Armée · vacances", "armed_vacation"),
        S::ArmedCustomBypass => ("Armée · contournement personnalisé", "armed_custom_bypass"),
        S::Pending => ("Déclenchement en attente", "pending"),
        S::Arming => ("Armement en cours", "arming"),
        S::Disarming => ("Désarmement en cours", "disarming"),
        S::Triggered => ("Alarme déclenchée", "triggered"),
        S::Unspecified => return None,
    };
    Some(StateText { label, literal })
}
pub(crate) fn cover(value: &wire::CoverValue) -> String {
    if !admit::cover_value(value) {
        return "Valeur non prise en charge".into();
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
                "Obstacle détecté"
            } else {
                "Aucun obstacle détecté"
            }
            .into(),
        );
    }
    parts.join(" · ")
}
pub(crate) fn lock(value: &wire::LockValue) -> &'static str {
    if !admit::lock_value(value) {
        return "Valeur non prise en charge";
    }
    admit::lock_state(value.state)
        .and_then(lock_state)
        .map(|text| text.label)
        .unwrap_or("Valeur non prise en charge")
}
pub(crate) fn alarm(value: &wire::AlarmValue) -> &'static str {
    if !admit::alarm_value(value) {
        return "Valeur non prise en charge";
    }
    admit::alarm_state(value.state)
        .and_then(alarm_state)
        .map(|text| text.label)
        .unwrap_or("Valeur non prise en charge")
}

pub(crate) fn cover_operation(value: wire::CoverOperation) -> Option<&'static str> {
    match value {
        wire::CoverOperation::Open => Some("Ouvrir"),
        wire::CoverOperation::Close => Some("Fermer"),
        wire::CoverOperation::Stop => Some("Arrêter"),
        wire::CoverOperation::Unspecified => None,
    }
}
pub(crate) fn lock_operation(value: wire::LockOperation) -> Option<&'static str> {
    match value {
        wire::LockOperation::Lock => Some("Verrouiller"),
        wire::LockOperation::Unlock => Some("Déverrouiller"),
        wire::LockOperation::Unspecified => None,
    }
}
pub(crate) fn alarm_operation(value: wire::AlarmOperation) -> Option<&'static str> {
    match value {
        wire::AlarmOperation::ArmHome => Some("Armer en présence"),
        wire::AlarmOperation::ArmAway => Some("Armer en absence"),
        wire::AlarmOperation::Disarm => Some("Désarmer"),
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
        assert_eq!(cover(&value), "0 % · Aucun obstacle détecté");
        assert_eq!(
            cover(&wire::CoverValue {
                state: Some(wire::CoverState::Open.into()),
                position_percent: Some(35.),
                ..Default::default()
            }),
            "Ouvert · 35 %"
        );
        assert_eq!(
            cover(&wire::CoverValue::default()),
            "Valeur non prise en charge"
        );
        assert_eq!(
            lock(&wire::LockValue {
                state: wire::LockState::Jammed.into(),
                ..Default::default()
            }),
            "Serrure bloquée"
        );
        assert_eq!(
            alarm(&wire::AlarmValue {
                state: wire::AlarmState::Triggered.into(),
                ..Default::default()
            }),
            "Alarme déclenchée"
        );
        assert_eq!(
            lock(&wire::LockValue {
                state: EnumValue::Unknown(99),
                ..Default::default()
            }),
            "Valeur non prise en charge"
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
