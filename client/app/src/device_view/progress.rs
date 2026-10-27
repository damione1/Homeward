use crate::ProActionPhase as Phase;
use buffa::EnumValue;
use home_client_actions::{ActionSnapshot, LiveState, SubmissionState};
use home_wire_contract::home::actions::v1 as wire;
type Presentation = (Phase, &'static str, &'static str);
fn contract_error(error: &wire::ContractError) -> &'static str {
    match error.code {
        EnumValue::Known(wire::ErrorCode::InvalidArgument) => {
            "La valeur demandée a été refusée avant admission."
        }
        EnumValue::Known(wire::ErrorCode::UnsupportedCapability) => {
            "Cette fonction n’est plus commandable."
        }
        EnumValue::Known(wire::ErrorCode::StaleEpoch) => {
            "Le serveur a redémarré. Reprenez le geste sur la nouvelle vue."
        }
        EnumValue::Known(wire::ErrorCode::StaleConfiguration) => {
            "La configuration a changé. Reprenez le geste sur la nouvelle vue."
        }
        EnumValue::Known(wire::ErrorCode::PermissionDenied) => "Votre autorisation a été retirée.",
        EnumValue::Known(wire::ErrorCode::Unspecified) | EnumValue::Unknown(_) => {
            "Le refus n’est pas reconnu par cette version."
        }
    }
}
pub(crate) fn presentation(snapshot: Option<&ActionSnapshot>) -> Presentation {
    let Some(action) = snapshot else {
        return (Phase::Idle, "", "");
    };
    // Only live server progress can assert confirmation. A durable dispatch
    // result, a matching cached value or a Submit response cannot do so.
    if let Some(point) = &action.progress {
        match point.status {
            EnumValue::Known(wire::ActionStatus::Confirmed) => {
                return (
                    Phase::Confirmed,
                    "Effet confirmé",
                    "Confirmation reçue du serveur ; l’observation reste affichée séparément.",
                );
            }
            EnumValue::Known(wire::ActionStatus::Failed) => {
                return (
                    Phase::Failed,
                    "Échec de la commande",
                    "Le suivi serveur signale un échec. L’observation reste inchangée.",
                );
            }
            EnumValue::Known(wire::ActionStatus::Cancelled) => {
                return (
                    Phase::Cancelled,
                    "Commande annulée",
                    "Le serveur a terminé cette action comme annulée.",
                );
            }
            EnumValue::Known(wire::ActionStatus::Indeterminate) => {
                return (
                    Phase::Indeterminate,
                    "Résultat indéterminé",
                    "La commande a pu être exécutée. Aucune nouvelle soumission automatique.",
                );
            }
            EnumValue::Known(
                wire::ActionStatus::Accepted
                | wire::ActionStatus::Dispatched
                | wire::ActionStatus::Acknowledged,
            ) => {}
            EnumValue::Known(wire::ActionStatus::Unspecified) | EnumValue::Unknown(_) => {
                return (
                    Phase::Unavailable,
                    "Progression non reconnue",
                    "Cette version ne peut pas interpréter le suivi reçu.",
                );
            }
        }
    }
    match action.live {
        LiveState::Stopped => {
            return (
                Phase::Stopped,
                "Suivi arrêté sur cet appareil",
                "Cela n’annule pas une commande déjà admise par le serveur.",
            );
        }
        LiveState::Unavailable => {
            return (
                Phase::Unavailable,
                "Suivi en direct indisponible",
                "Le résultat durable est conservé. Aucun effet physique supplémentaire n’est déduit.",
            );
        }
        LiveState::Interrupted => {
            return (
                Phase::Interrupted,
                "Suivi interrompu",
                "Relecture de la même commande ; aucune nouvelle soumission.",
            );
        }
        LiveState::NotRequested | LiveState::Available => {}
    }
    if let Some(response) = &action.response
        && response.status == wire::ActionStatus::Failed
        && let Some(error) = response.error.as_option()
    {
        return (Phase::Refused, "Non admise", contract_error(error));
    }
    if let Some(point) = &action.progress {
        let detail = if action.submission == SubmissionState::Uncertain {
            "Réponse initiale perdue ; la même commande a été retrouvée."
        } else {
            "L’état observé reste indépendant de cette commande."
        };
        return match point.status {
            EnumValue::Known(wire::ActionStatus::Accepted) => {
                (Phase::Accepted, "Commande admise", detail)
            }
            EnumValue::Known(wire::ActionStatus::Dispatched) => {
                (Phase::Dispatched, "Commande transmise", detail)
            }
            EnumValue::Known(wire::ActionStatus::Acknowledged) => (
                Phase::Acknowledged,
                "Acquittée · confirmation en attente",
                detail,
            ),
            EnumValue::Known(
                wire::ActionStatus::Confirmed
                | wire::ActionStatus::Failed
                | wire::ActionStatus::Cancelled
                | wire::ActionStatus::Indeterminate
                | wire::ActionStatus::Unspecified,
            )
            | EnumValue::Unknown(_) => (
                Phase::Unavailable,
                "Suivi indisponible",
                "Aucune confirmation supplémentaire n’est déduite.",
            ),
        };
    }
    match action.submission {
        SubmissionState::Prepared => (
            Phase::Preparing,
            "Vérification de la commande",
            "L’intention sera abandonnée si la vue ou vos droits changent.",
        ),
        SubmissionState::InFlight => (
            Phase::Sending,
            "Envoi en cours",
            "Une seule soumission de cette intention.",
        ),
        SubmissionState::Replied => (
            Phase::Accepted,
            "Réponse reçue · suivi en cours",
            "La réponse de soumission ne confirme pas l’effet physique.",
        ),
        SubmissionState::Uncertain => (
            Phase::Uncertain,
            "Soumission incertaine",
            "Consultation de la même commande ; aucun renvoi automatique.",
        ),
        SubmissionState::NotSent => (
            Phase::Refused,
            "Non envoyée",
            "Le contexte du geste n’était plus valide avant l’envoi.",
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn snapshot() -> ActionSnapshot {
        ActionSnapshot {
            id: "synthetic-command".parse().unwrap(),
            submission: SubmissionState::Replied,
            live: LiveState::NotRequested,
            response: Some(wire::CommandResponse {
                command_id: "synthetic-command".into(),
                status: wire::ActionStatus::Confirmed.into(),
                ..Default::default()
            }),
            failure: None,
            durable: None,
            progress: None,
            issue: None,
        }
    }
    #[test]
    fn dispatch_results_never_become_confirmation_and_ack_expiry_and_refusal_stay_distinct() {
        let mut action = snapshot();
        assert_eq!(presentation(Some(&action)).0, Phase::Accepted);
        action.live = LiveState::Unavailable;
        action.durable = Some(wire::DurableActionResult {
            response: action.response.clone().into(),
            state: wire::DurableActionState::Result.into(),
            error: wire::DurableActionError::None.into(),
            ..Default::default()
        });
        assert_eq!(presentation(Some(&action)).0, Phase::Unavailable);
        action.live = LiveState::Available;
        action.progress = Some(wire::ActionProgress {
            admission_epoch: "aabbccddeeff00112233445566778899".into(),
            config_revision: 1,
            sequence: 3,
            status: wire::ActionStatus::Acknowledged.into(),
            ..Default::default()
        });
        assert_eq!(presentation(Some(&action)).0, Phase::Acknowledged);
        action.progress.as_mut().unwrap().status = wire::ActionStatus::Indeterminate.into();
        assert_eq!(presentation(Some(&action)).0, Phase::Indeterminate);
        action.progress = None;
        action.response.as_mut().unwrap().status = wire::ActionStatus::Failed.into();
        action.response.as_mut().unwrap().error = wire::ContractError {
            code: wire::ErrorCode::StaleConfiguration.into(),
            ..Default::default()
        }
        .into();
        let refused = presentation(Some(&action));
        assert_eq!(refused.0, Phase::Refused);
        assert!(refused.2.contains("configuration"));
        action.progress = Some(wire::ActionProgress {
            status: wire::ActionStatus::Confirmed.into(),
            ..Default::default()
        });
        assert_eq!(presentation(Some(&action)).0, Phase::Confirmed);
    }
    #[test]
    fn stopping_local_tracking_does_not_render_a_physical_cancellation() {
        let mut action = snapshot();
        action.live = LiveState::Stopped;
        assert_eq!(presentation(Some(&action)).0, Phase::Stopped);
        action.progress = Some(wire::ActionProgress {
            status: wire::ActionStatus::Cancelled.into(),
            ..Default::default()
        });
        assert_eq!(presentation(Some(&action)).0, Phase::Cancelled);
    }
}
